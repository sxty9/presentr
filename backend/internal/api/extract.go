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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"presentr/internal/aigentic"
	"presentr/internal/auth"
	"presentr/internal/extract"
	"presentr/internal/store"
)

// extractTimeout bounds the FIRST read of ONE section (a whole small file, or one page range of a split
// PDF). AI recognition of a scanned page is slow, so this is generous; it exists only so a stuck upstream
// call cannot pin a worker forever. A large PDF read is many sections, each bounded by this on its first
// try and by sectionTimeout on any retry.
const extractTimeout = 5 * time.Minute

// maxSectionAttempts bounds how many times ONE section is read before presentr stops waiting on it. The
// first attempt uses extractTimeout; each retry after a TIMEOUT stretches the deadline (sectionTimeout).
// A section that keeps exceeding even the stretched deadline is structurally too slow — a page so dense
// the engine cannot finish it in the window — not merely unlucky, so retrying it forever would reproduce
// the same timeout endlessly. After this many timed-out attempts the section is named as unread and the
// read moves on, so the rest of the file still reads. Only a TIMEOUT counts against this budget; a genuine
// engine outage is a plain interruption that a later resume retries unchanged.
const maxSectionAttempts = 2

// sectionTimeout is the deadline for the (attempts+1)-th read of one section: extractTimeout on the first
// try and stretched on each retry after a timeout, capped so a single slow section cannot pin a worker
// without bound. A slow-but-finite section gets a real, longer second chance before it is abandoned; a
// structurally too-slow one is still bounded by maxSectionAttempts.
func sectionTimeout(attempts int) time.Duration {
	d := extractTimeout * time.Duration(attempts+1)
	if limit := extractTimeout * time.Duration(maxSectionAttempts); d > limit {
		d = limit
	}
	return d
}

// fallbackRequestLimit is the conservative per-request byte ceiling presentr assumes when it could not
// learn aigentic's own (RefreshRequestLimit) — aigentic's documented request cap. It is a FLOOR used
// only until the real limit is known, never the authority: the boundary belongs to aigentic and is
// asked for, not guessed (the Schnittstellen-Axiom).
const fallbackRequestLimit = 32 << 20

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
// the pool (never the browser), plans the read outside the pool (a local text-layer read, a single AI
// request, or — for a scanned PDF too big for one request — a split into page-sized sections), and hands
// the result back via SetExtract. Every ending is a stored, named state: ready (text + source), or
// failed (with a reason a retry can clear). A large read reports PROGRESS as "reading" with a section
// count, persists a resume job after each section, and — if the engine drops out mid-way — leaves a
// named, RESUMABLE failed state rather than starting over. Runs synchronously; startExtraction wraps it.
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
	plan, err := extract.Prepare(d.Mime, b, s.sectionBudget())
	if err != nil {
		_ = s.docs.SetExtract(id, store.Extract{State: "failed", Error: extractFailureReason(err), At: time.Now().Unix()})
		return
	}
	// A text file or a PDF with a readable text layer is read locally — exact, no AI, no sections.
	if plan.Local {
		_ = s.docs.SetExtract(id, store.Extract{State: "ready", Text: plan.LocalText, Source: plan.Source, At: time.Now().Unix()})
		return
	}
	ai := s.extractor()
	if ai == nil {
		_ = s.docs.SetExtract(id, store.Extract{State: "failed", Error: extractFailureReason(extract.ErrNoAI), At: time.Now().Unix()})
		return
	}
	s.readSections(id, d.Author, d.Title, plan.Sections, ai)
}

// readSections reads a file's AI sections one at a time, assembling the text and keeping the document's
// state honest throughout. For a single section it behaves like the old single-pass read. For many
// (a split PDF) it shows progress ("section 7 of 40"), persists a resume job after each section so an
// interruption continues rather than restarts, and names any section that could not be read.
//
// A section that TIMES OUT is retried with a stretched deadline (sectionTimeout) up to maxSectionAttempts;
// a structurally too-slow section that still overruns is then named as unread and SKIPPED, so a single
// poison section can no longer reproduce the same timeout forever nor block the rest of the file. A
// genuine engine outage (not a timeout) stays a plain, resumable interruption — it is not the section's
// fault, so it does not spend the section's attempts.
func (s *Server) readSections(id, subject, title string, sections []extract.Section, ai extract.AIExtractor) {
	total := len(sections)
	multi := total > 1

	// Resume: if a prior run of THIS same plan (same section count) left completed sections behind, reuse
	// them and pick up the attempt count of the section it was on — never re-read a section already read
	// (the task's "nicht von vorn"), and never reset a stretched deadline back to the short one.
	var results []extract.SectionResult
	attempts := 0
	if multi {
		if raw, ok := s.docs.ExtractJob(id); ok {
			if done, at, ok := decodeJob(raw, total); ok {
				results = done
				attempts = at
			}
		}
	}
	resumeFrom := len(results)
	if resumeFrom > total {
		resumeFrom = total
		results = results[:total]
	}
	if multi {
		_ = s.docs.SetExtract(id, store.Extract{State: "reading", SectionsDone: resumeFrom, SectionsTotal: total})
	}

	for i := resumeFrom; i < total; i++ {
		// Read section i, stretching the deadline on each timeout up to maxSectionAttempts. attempts carries
		// in from a resume so an already-stretched section is not restarted at the short deadline.
		var r extract.SectionResult
		for {
			ctx, cancel := context.WithTimeout(context.Background(), sectionTimeout(attempts))
			r = extract.ReadSection(ctx, ai, subject, title, sections[i])
			cancel()
			if r.Err == nil || r.TooBig || !isRetryableAbort(r.Err) {
				break // read, or a per-section verdict (too big) — not a retry case
			}
			if !errors.Is(r.Err, context.DeadlineExceeded) {
				// A dropped engine (or a cancelled call) is an INTERRUPTION, not this section being slow: stop,
				// keep what is read, and leave a named, resumable failed state. A retry (reExtract) or a restart
				// (ResumePending, for the "reading" state) continues from exactly here. attempts is preserved so
				// a stretched deadline survives the interruption.
				if multi {
					_ = s.docs.SetExtractJob(id, encodeJob(total, attempts, results))
					_ = s.docs.SetExtract(id, store.Extract{State: "failed", Error: interruptedReason(i, total, r.Err), SectionsDone: i, SectionsTotal: total, At: time.Now().Unix()})
				} else {
					_ = s.docs.SetExtract(id, store.Extract{State: "failed", Error: extractFailureReason(r.Err), At: time.Now().Unix()})
				}
				return
			}
			// A timeout. Give the section another, longer try — unless its attempts are used up.
			attempts++
			if attempts >= maxSectionAttempts {
				break
			}
			if multi {
				// Persist the raised attempt count so a crash-restart resumes the stretched read, not restarts it.
				_ = s.docs.SetExtractJob(id, encodeJob(total, attempts, results))
			}
		}

		if r.Err != nil && errors.Is(r.Err, context.DeadlineExceeded) && attempts >= maxSectionAttempts {
			// Structurally too slow even stretched: abandon THIS section. A multi-section read names it as
			// unread (so the gap is disclosed, Kein stummes Ausbleiben) and carries on; a single-section file
			// has nothing else to read, so this is the whole, named failure.
			if !multi {
				_ = s.docs.SetExtract(id, store.Extract{State: "failed", Error: tooSlowReason(), At: time.Now().Unix()})
				return
			}
			r = extract.SectionResult{Label: sections[i].Label, Err: errSectionTooSlow}
		}

		results = append(results, r)
		attempts = 0
		if multi {
			_ = s.docs.SetExtractJob(id, encodeJob(total, attempts, results))
			_ = s.docs.SetExtract(id, store.Extract{State: "reading", SectionsDone: len(results), SectionsTotal: total})
		}
	}

	res, anyText := extract.AssembleText(results)
	if !anyText {
		// Nothing legible came out of any section (e.g. a lone oversized image, or every page unreadable).
		// Fail with a named reason rather than store an empty "ready", and clear the resume job so a retry
		// re-reads from the start rather than replaying the same empty result.
		if multi {
			_ = s.docs.SetExtractJob(id, nil)
		}
		_ = s.docs.SetExtract(id, store.Extract{State: "failed", Error: noTextReason(results), SectionsDone: total, SectionsTotal: total, At: time.Now().Unix()})
		return
	}
	_ = s.docs.SetExtract(id, store.Extract{State: "ready", Text: res.Text, Source: res.Source, Model: res.Model, Engine: res.Engine, At: time.Now().Unix()})
}

// isRetryableAbort reports whether a section-read error means the WHOLE read was interrupted (the engine
// is unavailable, or the call was cancelled/timed out) rather than a verdict on that one section — so the
// read is left resumable instead of marking every remaining section as failed.
func isRetryableAbort(err error) bool {
	return errors.Is(err, aigentic.ErrUnavailable) ||
		errors.Is(err, aigentic.ErrDisabled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled)
}

// jobSection is one completed section, persisted so an interrupted read resumes. jobState is the whole
// resume job (the plan's section count plus the sections read so far, in order).
type jobSection struct {
	Label  string `json:"label"`
	Text   string `json:"text"`
	Engine string `json:"engine"`
	Model  string `json:"model"`
	TooBig bool   `json:"tooBig"`
	Err    string `json:"err,omitempty"`
}
type jobState struct {
	Total    int          `json:"total"`
	Attempts int          `json:"attempts,omitempty"` // reads already spent on the next (in-progress) section, so a stretched deadline survives a resume
	Sections []jobSection `json:"sections"`
}

// encodeJob serializes the completed sections of a read, plus the attempt count of the section it is on,
// for resume.
func encodeJob(total, attempts int, results []extract.SectionResult) []byte {
	js := jobState{Total: total, Attempts: attempts, Sections: make([]jobSection, 0, len(results))}
	for _, r := range results {
		s := jobSection{Label: r.Label, Text: r.Text, Engine: r.Engine, Model: r.Model, TooBig: r.TooBig}
		if r.Err != nil {
			s.Err = r.Err.Error()
		}
		js.Sections = append(js.Sections, s)
	}
	b, _ := json.Marshal(js)
	return b
}

// decodeJob reads a resume job back into section results plus the in-progress section's attempt count, but
// only when it belongs to the SAME plan (same total section count) — a mismatch (aigentic's limit changed,
// so the split differs) starts fresh.
func decodeJob(raw []byte, total int) ([]extract.SectionResult, int, bool) {
	var js jobState
	if err := json.Unmarshal(raw, &js); err != nil || js.Total != total {
		return nil, 0, false
	}
	out := make([]extract.SectionResult, 0, len(js.Sections))
	for _, s := range js.Sections {
		r := extract.SectionResult{Label: s.Label, Text: s.Text, Engine: s.Engine, Model: s.Model, TooBig: s.TooBig}
		if s.Err != "" {
			r.Err = errors.New(s.Err)
		}
		out = append(out, r)
	}
	return out, js.Attempts, true
}

// errSectionTooSlow marks a section abandoned because it kept exceeding the read deadline even after the
// deadline was stretched — a structurally too-slow section, distinct from a transient engine outage. It is
// named in the assembled text so the gap is disclosed (Kein stummes Ausbleiben) rather than silently lost.
var errSectionTooSlow = errors.New("it kept exceeding the read time limit even after the deadline was stretched")

// tooSlowReason names a whole-file (single-section) read abandoned because it kept exceeding the deadline
// even stretched, so the user knows a plain retry would only repeat it.
func tooSlowReason() string {
	return "This file kept exceeding the read time limit even after the deadline was stretched, so its text could not be read."
}

// sectionBudget is the raw bytes ONE section may carry, derived from aigentic's published per-request
// limit (asked for via RefreshRequestLimit, not guessed). It reserves room for the base64 envelope a
// binary section expands into plus the prompt, so a section that fits the budget still fits one request.
func (s *Server) sectionBudget() int {
	limit := s.aiLimit.Load()
	if limit <= 0 {
		limit = fallbackRequestLimit
	}
	reserve := limit / 16
	if reserve > 1<<20 {
		reserve = 1 << 20
	}
	// base64 expands bytes by 4/3, so keep the raw section to ~7/10 of the request budget for headroom.
	budget := (limit - reserve) * 7 / 10
	if budget < 1 {
		budget = limit / 2
	}
	return int(budget)
}

// RefreshRequestLimit asks aigentic for its per-request byte ceiling and caches it, so the section split
// follows aigentic's own limit rather than a baked-in guess. Best-effort and bounded: a disabled client
// or an unreachable/older aigentic leaves the conservative fallback in place. Called once at start.
func (s *Server) RefreshRequestLimit() {
	if s.ai == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if n, ok := s.ai.RequestLimit(ctx); ok {
		s.aiLimit.Store(n)
	}
}

// extractFailureReason turns a read error into a plain, user-facing sentence naming why the text could
// not be read — so the document discloses the cause and the user knows whether a retry can help.
func extractFailureReason(err error) string {
	switch {
	case errors.Is(err, extract.ErrNoAI):
		return "The room assistant is not configured, so text in images and scans cannot be read yet. Retry once it is set up."
	case errors.Is(err, extract.ErrTooLargeForAI):
		return "This part of the file is too large for the assistant to read even on its own, so it could not be read."
	case errors.Is(err, aigentic.ErrUnavailable):
		return "No AI engine was available to read the file. Retry once an engine is connected."
	default:
		return "The file's text could not be read. You can retry."
	}
}

// interruptedReason names a chunked read that stopped mid-way because the engine dropped out, telling the
// user exactly how far it got and that a retry continues from there rather than starting over.
func interruptedReason(done, total int, err error) string {
	return fmt.Sprintf("The read was interrupted after section %d of %d (%s). Retry to continue from where it stopped.",
		done, total, strings.TrimSpace(shortReason(err)))
}

// shortReason gives the interruption a short human cause.
func shortReason(err error) string {
	switch {
	case errors.Is(err, aigentic.ErrUnavailable), errors.Is(err, aigentic.ErrDisabled):
		return "no AI engine was available"
	case errors.Is(err, context.DeadlineExceeded):
		return "a section took too long"
	default:
		return "the read was cancelled"
	}
}

// noTextReason names why a read produced no text at all, from the section results.
func noTextReason(results []extract.SectionResult) string {
	for _, r := range results {
		if r.TooBig {
			return "This file is too large for the assistant to read in one request and could not be split into smaller parts, so its text could not be read."
		}
	}
	return "No legible text could be read from this file. You can retry."
}

// ResumePending re-launches the read of any file left mid-read — "pending" (its upload landed but the
// read had not begun) or "reading" (a chunked read that a crash interrupted between sections). Called
// once at start, so such a state always ENDS rather than lingering after a restart (Kein stummes
// Ausbleiben); a chunked read resumes from its persisted job, not from the first page.
func (s *Server) ResumePending() {
	for _, d := range s.docs.List() {
		if d.Kind == "file" && (d.ExtractState == "pending" || d.ExtractState == "reading") {
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
		"state":         d.ExtractState,
		"source":        d.ExtractSource,
		"model":         d.ExtractModel,
		"engine":        d.ExtractEngine,
		"error":         d.ExtractError,
		"size":          d.ExtractSize,
		"sectionsDone":  d.ExtractSectionsDone,
		"sectionsTotal": d.ExtractSectionsTotal,
		"text":          text,
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
