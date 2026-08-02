package api

// Reading the text out of an uploaded file — the api layer's orchestration of it. The read itself
// lives in internal/extract (a text-layer parse, or aigentic's shared recognition for images/scans);
// the pool only STORES the result (store.SetExtract). This file is the policy/timing glue in between:
//
//   - It runs ASYNCHRONOUSLY. A large PDF is not read in a second, so the upload returns immediately
//     with the document in the "pending" state, and the read completes in the background and flips the
//     state to "ready" or "failed". That named state is what the UI shows and what ENDS — never an
//     endless spinner (a crash mid-read is recovered by ResumePending on the next start).
//   - It is RETRIABLE. A read that failed (no AI configured, a broken file) can be re-run from the
//     stored bytes without re-uploading — reExtract, below.
//   - It is HONEST about the AI it uses: the read runs on the UPLOADER's behalf (aigentic resolves the
//     subject to live rights), and a missing/insufficient AI becomes a named failure, not a silent gap.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"presentr/internal/aigentic"
	"presentr/internal/auth"
	"presentr/internal/extract"
	"presentr/internal/store"
)

// extractTimeout bounds one background read. AI recognition of a big scanned PDF is slow, so this is
// generous; it exists only so a stuck upstream call cannot pin a worker forever.
const extractTimeout = 5 * time.Minute

// aiExtractor adapts the aigentic client to extract.AIExtractor. A nil client (assistant not
// configured) yields a nil extractor, so extract.Run reports ErrNoAI — a named, retriable reason.
type aiExtractor struct{ ai *aigentic.Client }

func (a aiExtractor) Extract(ctx context.Context, subject, filename, mime string, data []byte) (text, engine, model string, err error) {
	res, err := a.ai.Extract(ctx, subject, filename, mime, data)
	if err != nil {
		return "", "", "", err
	}
	return res.Output, res.Engine, res.Model, nil
}

// extractor returns the AI recognizer for background reads, or nil when no AI is configured (so
// image/scan reads fail with a named reason instead of blocking).
func (s *Server) extractor() extract.AIExtractor {
	if s.ai == nil {
		return nil
	}
	return aiExtractor{ai: s.ai}
}

// startExtraction launches the background read of a file document, bounded by extractSem so a burst of
// uploads cannot spawn unbounded work, and tracked by extractWG so the reads can be drained. It returns
// at once; runExtraction records the outcome.
func (s *Server) startExtraction(id string) {
	s.extractWG.Add(1)
	go func() {
		defer s.extractWG.Done()
		s.extractSem <- struct{}{}
		defer func() { <-s.extractSem }()
		s.runExtraction(id)
	}()
}

// WaitExtractions blocks until every in-flight background read has finished. Used to drain reads
// deterministically (a test, a graceful stop) so nothing writes to the pool after the caller moves on.
func (s *Server) WaitExtractions() { s.extractWG.Wait() }

// runExtraction reads one file document's text and stores the outcome. It reads the file's bytes from
// the pool (never the browser), runs the read outside the pool, and hands the result back via
// SetExtract. Every ending is a stored, named state: ready (with the text and its source), or failed
// (with a reason a retry can clear). Runs synchronously; startExtraction is the async wrapper.
func (s *Server) runExtraction(id string) {
	d, ok := s.docs.Get(id)
	if !ok || d.Kind != "file" {
		return // deleted, or not a file — nothing to read
	}
	b, ok := s.docs.Bytes(id)
	if !ok {
		_ = s.docs.SetExtract(id, store.Extract{State: "failed", Error: "the file's stored bytes could not be read", At: time.Now().Unix()})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), extractTimeout)
	defer cancel()
	res, err := extract.Run(ctx, d.Author, d.Title, d.Mime, b, s.extractor())
	if err != nil {
		_ = s.docs.SetExtract(id, store.Extract{State: "failed", Error: extractFailureReason(err), At: time.Now().Unix()})
		return
	}
	_ = s.docs.SetExtract(id, store.Extract{
		State: "ready", Text: res.Text, Source: res.Source, Model: res.Model, Engine: res.Engine, At: time.Now().Unix(),
	})
}

// extractFailureReason turns a read error into a plain, user-facing sentence naming why the text could
// not be read — so the document discloses the cause and the user knows whether a retry can help.
func extractFailureReason(err error) string {
	switch {
	case errors.Is(err, extract.ErrNoAI):
		return "The room assistant is not configured, so text in images and scans cannot be read yet. Retry once it is set up."
	case errors.Is(err, extract.ErrTooLargeForAI):
		return "This file is too large for the assistant to read in one pass. Splitting it into sections is a separate, planned step."
	case errors.Is(err, aigentic.ErrUnavailable):
		return "No AI engine was available to read the file. Retry once an engine is connected."
	default:
		return "The file's text could not be read. You can retry."
	}
}

// ResumePending re-launches the read of any file left in the "pending" state — a document whose upload
// landed but whose read did not finish before the daemon stopped. Called once at start, so a pending
// state always ENDS rather than lingering forever after a restart (Kein stummes Ausbleiben).
func (s *Server) ResumePending() {
	for _, d := range s.docs.List() {
		if d.Kind == "file" && d.ExtractState == "pending" {
			s.startExtraction(d.ID)
		}
	}
}

// getExtract returns a file document's derived text plus its read state, so the UI can show what the
// assistant will actually read (and disclose "not read yet" / "could not read"). Read-only, right-gated.
func (s *Server) getExtract(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	id := r.PathValue("id")
	d, ok := s.docs.Get(id)
	if !ok || d.Kind != "file" {
		writeErr(w, http.StatusNotFound, "File not found")
		return
	}
	text, _ := s.docs.ExtractText(id)
	writeJSON(w, http.StatusOK, map[string]any{
		"state":  d.ExtractState,
		"source": d.ExtractSource,
		"model":  d.ExtractModel,
		"engine": d.ExtractEngine,
		"error":  d.ExtractError,
		"size":   d.ExtractSize,
		"text":   text,
	})
}

// reExtract re-runs the text read of a file document from its stored bytes — no re-upload needed. It
// flips the document back to "pending" and launches the read; the UI polls the list and sees the state
// resolve. Right-gated + CSRF (the read may spend AI budget).
func (s *Server) reExtract(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	id := r.PathValue("id")
	d, ok := s.docs.Get(id)
	if !ok || d.Kind != "file" {
		writeErr(w, http.StatusNotFound, "File not found")
		return
	}
	if err := s.docs.SetExtract(id, store.Extract{State: "pending"}); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not start reading the file")
		return
	}
	s.startExtraction(id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": "pending"})
}
