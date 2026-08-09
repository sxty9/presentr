package api

// The room's AI, server-side. Every AI turn in presentr — the Chat assistant and the Connection
// diagram's extraction — is grounded in the SAME source, the room's document pool, and routed
// through the shared aigentic service (the "Ask AI" standard). That grounding lives HERE rather than
// in the browser for two reasons: uploaded PDFs and images are read by aigentic natively, so their
// bytes must reach it — and shipping those bytes out to the browser only to post them back would
// double the traffic and break Portionierte Daten. So the api layer assembles the grounding from the
// pool (text inline; files as base64 with their media type) and hands it to aigentic on the caller's
// behalf. The pool stays passive: it returns bytes; this layer decides what becomes context.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"presentr/internal/aigentic"
	"presentr/internal/auth"
	"presentr/internal/extract"
	"presentr/internal/store"
)

// groundingImageMime is the media type every read-out image is grounded as (ExtractImages compresses
// each picture to JPEG). Named once so the part's MediaType and its provenance-path extension stay in
// step.
const groundingImageMime = "image/jpeg"

const (
	maxAskBody = 1 << 20 // the prompt/format submission (the grounding is assembled server-side)
	// The total raw bytes assembled into one grounding, kept under aigentic's 32 MiB request ceiling
	// so the prompt and envelope still fit. Documents past the budget are skipped (Portionierte Daten).
	maxGroundingBytes = 24 << 20
)

// ask job states and phases. State is the coarse lifecycle; phase names the fine, page-related step the
// UI shows instead of a bare spinner (grounding assembled → sent to the assistant → answer received).
const (
	askStateRunning = "running"
	askStateDone    = "done"
	askStateFailed  = "failed"

	askPhaseGrounding = "grounding" // assembling the room context
	askPhaseSending   = "sending"   // context ready, the turn is at aigentic awaiting an answer
	askPhaseDone      = "done"      // the answer is back
)

// askJobTTL is how long a finished ask job stays pollable before it is reaped, so a client that polls a
// little after completion still reads the answer while memory stays bounded.
const askJobTTL = 10 * time.Minute

// askJob is one background AI turn. It lives in memory (the answer is transient — a browser reload just
// asks again), keyed by an unguessable id and scoped to the user who started it. Phase/Docs/Images drive
// the UI's granular progress; Output/Model/Engine hold the answer; Error holds a clean failure reason.
type askJob struct {
	ID     string
	Owner  string
	State  string
	Phase  string
	Docs   int
	Images int
	Output string
	Model  string
	Engine string
	Error  string
	Done   int64 // epoch seconds the job reached a terminal state (0 while running), for reaping
}

// ask starts one AI turn for the caller as a BACKGROUND job and returns its id at once. The turn no
// longer runs inside this HTTP request: a grounded turn over a large pool can take longer than
// Cloudflare's ~100s edge limit, which would return a 524 HTML page to the browser instead of an answer.
// Running it in the background (on context.Background(), so a browser reload cannot abort it — the same
// pattern as a file read in extract.go) lets the UI poll askStatus for granular progress and, when it is
// ready, the answer. The prompt and requested shape come from the UI; the grounding is assembled here.
func (s *Server) ask(w http.ResponseWriter, r *http.Request, u *auth.User) {
	if s.ai == nil || !s.ai.Enabled() {
		writeErr(w, http.StatusServiceUnavailable, "The room assistant is not configured on this server")
		return
	}
	var body struct {
		Prompt       string `json:"prompt"`
		OutputFormat string `json:"outputFormat"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAskBody)).Decode(&body); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	prompt := strings.TrimSpace(body.Prompt)
	if prompt == "" {
		writeErr(w, http.StatusBadRequest, "A prompt is required")
		return
	}
	format := askFormat(body.OutputFormat)

	job := &askJob{ID: store.NewID(), Owner: u.Username, State: askStateRunning, Phase: askPhaseGrounding}
	s.askMu.Lock()
	s.reapAskJobsLocked()
	s.askJobs[job.ID] = job
	s.askMu.Unlock()

	s.askWG.Add(1)
	go func() {
		defer s.askWG.Done()
		s.runAsk(job.ID, u.Username, prompt, format)
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{"jobId": job.ID, "state": askStateRunning, "phase": askPhaseGrounding})
}

// askStatus reports one ask job's progress and, once it is done, its answer (Kennzeichnungspflicht: the
// model/engine that produced it). It is scoped to the user who started the job, so one caller never reads
// another's answer. A job the client polls after it was reaped (or an unknown id) reports 404, which the
// UI treats as "ask again".
func (s *Server) askStatus(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id := r.PathValue("id")
	s.askMu.Lock()
	j, ok := s.askJobs[id]
	var snap askJob
	if ok {
		snap = *j
	}
	s.askMu.Unlock()
	if !ok || snap.Owner != u.Username {
		writeErr(w, http.StatusNotFound, "That request is no longer available — please ask again")
		return
	}
	resp := map[string]any{"id": snap.ID, "state": snap.State, "phase": snap.Phase, "docs": snap.Docs, "images": snap.Images}
	switch snap.State {
	case askStateDone:
		resp["output"] = snap.Output
		resp["model"] = snap.Model
		resp["engine"] = snap.Engine
	case askStateFailed:
		resp["error"] = snap.Error
	}
	writeJSON(w, http.StatusOK, resp)
}

// runAsk carries out one background turn: assemble the grounding, send it to aigentic, record the answer.
// It advances the job's phase as it goes so the UI shows page-related progress, and it drives the two
// resilience paths: a text-only retry when no engine can see images, and a full-bytes retry when a
// put-once reference turned out stale (the bytes aigentic held were evicted). Runs on context.Background()
// so a browser reload cannot cut it short.
func (s *Server) runAsk(jobID, subject, prompt, format string) {
	inline, gaps, origins := s.roomGrounding(prompt)
	docs, images := countGrounding(inline)
	s.updateAsk(jobID, func(j *askJob) {
		j.Docs, j.Images = docs, images
		j.Phase = askPhaseSending
	})
	grounded := withGroundingNote(inline, groundingNote(gaps))

	res, err := s.runGrounded(subject, prompt, format, grounded)

	// A turn that referenced already-held bytes (put-once) can fail if aigentic evicted them. Drop the
	// remembered references and retry ONCE with full content, so the optimization can never make an answer
	// fail where full bytes would have succeeded (Self-Healing).
	if err != nil && usesRefs(grounded) {
		for _, o := range origins {
			s.dropRef(o.Key)
		}
		inline, gaps, origins = s.roomGrounding(prompt)
		grounded = withGroundingNote(inline, groundingNote(gaps))
		res, err = s.runGrounded(subject, prompt, format, grounded)
	}

	if err != nil {
		s.updateAsk(jobID, func(j *askJob) {
			j.State = askStateFailed
			j.Error = askErrorMessage(err)
		})
		return
	}
	// Remember the references aigentic returned so the NEXT turn sends only them, not the bytes again.
	s.rememberRefs(res.Context, origins)
	s.updateAsk(jobID, func(j *askJob) {
		j.State = askStateDone
		j.Phase = askPhaseDone
		j.Output, j.Model, j.Engine = res.Output, res.Model, res.Engine
	})
}

// runGrounded sends one assembled grounding to aigentic, transparently retrying WITHOUT images when the
// only reachable engine cannot see them (aigentic 422): the text grounding still stands and a note tells
// the model it could not see the room's pictures, so a room with a text-only engine keeps its assistant
// instead of losing it the moment it holds one image (kein stummes Ausbleiben).
func (s *Server) runGrounded(subject, prompt, format string, inline []aigentic.InlineFile) (aigentic.Res, error) {
	res, err := s.ai.Run(context.Background(), subject, aigentic.Req{Prompt: prompt, OutputFormat: format, Inline: inline})
	if errors.Is(err, aigentic.ErrNoVisionEngine) {
		textOnly := withGroundingNote(dropImageParts(inline), visionUnavailableNote)
		res, err = s.ai.Run(context.Background(), subject, aigentic.Req{Prompt: prompt, OutputFormat: format, Inline: textOnly})
	}
	return res, err
}

// updateAsk applies fn to a live job under the lock, stamping the terminal time when the job finishes so
// the reaper can retire it after askJobTTL. A no-op if the job was already reaped.
func (s *Server) updateAsk(id string, fn func(*askJob)) {
	s.askMu.Lock()
	defer s.askMu.Unlock()
	j, ok := s.askJobs[id]
	if !ok {
		return
	}
	fn(j)
	if (j.State == askStateDone || j.State == askStateFailed) && j.Done == 0 {
		j.Done = time.Now().Unix()
	}
}

// reapAskJobsLocked drops finished jobs older than askJobTTL, bounding the in-memory registry. The caller
// holds askMu. A still-running job is never reaped.
func (s *Server) reapAskJobsLocked() {
	cutoff := time.Now().Add(-askJobTTL).Unix()
	for id, j := range s.askJobs {
		if j.Done != 0 && j.Done < cutoff {
			delete(s.askJobs, id)
		}
	}
}

// WaitAsk blocks until every in-flight background ask turn has finished. Used to drain jobs
// deterministically (a test that must not race a temp-dir cleanup, a graceful stop).
func (s *Server) WaitAsk() { s.askWG.Wait() }

// askErrorMessage maps an aigentic failure to the same clean, user-facing reason the old synchronous
// handler returned, now stored on the job for the UI to show.
func askErrorMessage(err error) string {
	switch {
	case errors.Is(err, aigentic.ErrDisabled):
		return "The room assistant is not configured on this server"
	case errors.Is(err, aigentic.ErrUnavailable):
		return "No AI engine is available — an admin can link a Claude credential or start a local model in aigentic"
	default:
		return "The assistant could not answer"
	}
}

// countGrounding counts the document parts and image parts in an assembled grounding, for the progress
// the UI shows ("room context ready — N documents, M images").
func countGrounding(inline []aigentic.InlineFile) (docs, images int) {
	for _, f := range inline {
		if strings.HasPrefix(f.MediaType, "image/") {
			images++
		} else {
			docs++
		}
	}
	return docs, images
}

// usesRefs reports whether any part of a grounding was sent as a put-once reference (empty content, a
// reference to bytes aigentic already holds) rather than full bytes.
func usesRefs(inline []aigentic.InlineFile) bool {
	for _, f := range inline {
		if f.Ref != "" {
			return true
		}
	}
	return false
}

// ── put-once / reference-many cache ───────────────────────────────────────────────────────────────
//
// After aigentic reads a document/image once it returns a reference to the bytes it now holds. presentr
// remembers each reference against the piece it came from, so the next turn sends only the reference and
// the full bytes are put ONCE, not re-shipped on every question — the fix for a large PDF timing out at
// the edge. Keys are stable per document (and per image within a document); the cache lives in memory and
// is invalidated when a document is deleted or re-read.

// textRefKey / imageRefKey are the stable cache keys for a document's text part and for one image read
// out of it (keyed by the image's section index, which survives a resume without re-keying).
func textRefKey(docID string) string           { return "t:" + docID }
func imageRefKey(docID string, idx int) string { return "i:" + docID + ":" + strconv.Itoa(idx) }

// refFor returns the remembered reference for a cache key, if aigentic previously kept that piece's bytes.
func (s *Server) refFor(key string) (string, bool) {
	if key == "" {
		return "", false
	}
	s.refMu.Lock()
	defer s.refMu.Unlock()
	ref, ok := s.refs[key]
	return ref, ok
}

// rememberRefs records the references aigentic returned for this turn's grounding parts, matching each
// returned reference (by the path presentr sent) to the document/image it came from (origins). Only parts
// sent in FULL are in origins, so a partial send is never remembered as if it were the whole document.
func (s *Server) rememberRefs(ctx []aigentic.ContextRef, origins []groundingOrigin) {
	if len(ctx) == 0 || len(origins) == 0 {
		return
	}
	keyByPath := make(map[string]string, len(origins))
	for _, o := range origins {
		if o.Key != "" {
			keyByPath[o.Path] = o.Key
		}
	}
	s.refMu.Lock()
	defer s.refMu.Unlock()
	for _, c := range ctx {
		if c.Ref == "" {
			continue
		}
		if key, ok := keyByPath[c.Path]; ok {
			s.refs[key] = c.Ref
		}
	}
}

// dropRef forgets one remembered reference (a stale-reference recovery).
func (s *Server) dropRef(key string) {
	if key == "" {
		return
	}
	s.refMu.Lock()
	delete(s.refs, key)
	s.refMu.Unlock()
}

// invalidateRefs forgets every reference for a document — its text part and all its images — so a later
// turn re-sends fresh content. Called when the document is deleted or re-read (its bytes changed).
func (s *Server) invalidateRefs(docID string) {
	if docID == "" {
		return
	}
	prefix := "i:" + docID + ":"
	s.refMu.Lock()
	defer s.refMu.Unlock()
	delete(s.refs, textRefKey(docID))
	for k := range s.refs {
		if strings.HasPrefix(k, prefix) {
			delete(s.refs, k)
		}
	}
}

// groundingOrigin ties one grounding part (by the path aigentic echoes back) to the ref-cache key of the
// document/image it came from, so a returned reference can be filed against the right piece. Only parts
// sent in full (or already sent as a reference) get an origin — never a partial text send.
type groundingOrigin struct {
	Path string
	Key  string
}

// visionUnavailableNote is prepended when an image-bearing turn is retried without its images because no
// reachable engine can see them, so the assistant answers from text and is honest that it did not see the
// room's pictures rather than inventing what is in them.
const visionUnavailableNote = "The room includes pictures (photographs, scans, nameplates) that could NOT be shown to the available AI engine, which cannot see images. Answer from the text below and say plainly that you could not see the room's images."

// withGroundingNote prepends a text/markdown note part to the grounding when note is non-empty, so a
// disclosure (an incomplete grounding, or images that could not be shown) rides ahead of the content it
// qualifies. An empty note leaves the grounding unchanged.
func withGroundingNote(inline []aigentic.InlineFile, note string) []aigentic.InlineFile {
	if note == "" {
		return inline
	}
	return append([]aigentic.InlineFile{{Path: "grounding-note", MediaType: "text/markdown", Content: note}}, inline...)
}

// dropImageParts returns the grounding without its image parts, for the text-only retry when no engine
// can see images. The text (each file's read text, and any disclosure note) is kept intact.
func dropImageParts(inline []aigentic.InlineFile) []aigentic.InlineFile {
	out := inline[:0:0]
	for _, f := range inline {
		if strings.HasPrefix(f.MediaType, "image/") {
			continue
		}
		out = append(out, f)
	}
	return out
}

// groundingGaps is what the assembled grounding left out, each list kept separate so the note to the
// model can name the DIFFERENT reasons a document is missing (too large / not read yet / could not be
// read) rather than blur them into one (Kein Befund ohne Bedeutung).
type groundingGaps struct {
	omitted       []string // too large to fit aigentic's per-request budget
	notRead       []string // a file whose text has not been read yet (read still pending)
	unread        []string // a file whose text read failed (a retry may fix it)
	partial       []string // a large read where only the sections relevant to the question were included
	omittedImages []string // a read-out image the budget could not fit alongside the text
}

// roomGrounding turns the pool into aigentic inline parts. The key move of this feature: an uploaded
// file is grounded by the TEXT read out of it once at upload (a few hundred KB), NOT by its raw bytes
// (up to 100 MB) — so a 70 MB scanned manual costs the answer a few hundred KB, and its bytes never
// round-trip to the AI on every question. Text documents ground as their inline markdown.
//
// Every gap is named, never silently dropped (EHRLICH BLEIBEN): a document too large for the
// per-request budget, a file whose read is still pending, and a file whose read failed are each
// returned so ask can disclose them to the model (and through it, the user). A legacy file with no
// read yet (state "") falls back to grounding by its bytes, bounded, so nothing uploaded before this
// feature stops being usable.
//
// USING THE RELEVANT SECTIONS, NOT EVERYTHING: a large file is now read in full (its scanned pages are
// split, read and reassembled into one text), so a single extract can itself be big. When a ready
// extract does not fit the remaining budget, presentr does NOT drop it and does NOT ship all of it —
// it selects the parts of the read text most relevant to the question and includes those, naming the
// document as only-partially-consulted (EHRLICH BLEIBEN). The selection uses the means at hand — lexical
// overlap between the question and the read text's own paragraphs (the sections a chunked read already
// separated with blank lines) — NOT a new retrieval engine or a vector store (Keine ähnlichen
// Geschwister; the task's "loese es mit den Mitteln, die da sind"). prompt is the question the selection
// is relevant to; "" (no question) keeps the leading text.
func (s *Server) roomGrounding(prompt string) (inline []aigentic.InlineFile, gaps groundingGaps, origins []groundingOrigin) {
	docs := s.docs.List()
	out := make([]aigentic.InlineFile, 0, len(docs))
	var total int64
	fits := func(n int) bool { return total+int64(n) <= maxGroundingBytes }

	// emit adds one cacheable grounding part (a text document, a file's read text, or one read-out image).
	// If aigentic already holds this piece's bytes (a remembered reference) it sends ONLY the reference —
	// no bytes, no budget cost: the put-once/reference-many win. Otherwise it sends the full content within
	// the budget, naming an over-budget part in gaps rather than dropping it silently. Either way it records
	// an origin, so the reference aigentic returns can be filed against this exact piece. isImage routes an
	// over-budget miss to the right gap list.
	emit := func(key, path, content, media string, isImage bool) {
		if ref, ok := s.refFor(key); ok {
			out = append(out, aigentic.InlineFile{Path: path, MediaType: media, Ref: ref})
			origins = append(origins, groundingOrigin{Path: path, Key: key})
			return
		}
		if content == "" {
			return
		}
		if !fits(len(content)) {
			if isImage {
				gaps.omittedImages = append(gaps.omittedImages, path)
			} else {
				gaps.omitted = append(gaps.omitted, path)
			}
			return
		}
		total += int64(len(content))
		out = append(out, aigentic.InlineFile{Path: path, Content: content, MediaType: media})
		origins = append(origins, groundingOrigin{Path: path, Key: key})
	}

	for _, d := range docs {
		switch d.Kind {
		case "text":
			emit(textRefKey(d.ID), groundingPath(d.Title), strings.TrimSpace(d.Content), "text/markdown", false)
		case "file":
			switch d.ExtractState {
			case "ready":
				// Ground by the read text — small, exact enough, and already vision-read for images. A big
				// read that does not fit is trimmed to the question-relevant sections rather than dropped.
				text, ok := s.docs.ExtractText(d.ID)
				if !ok {
					continue
				}
				t := strings.TrimSpace(text)
				if t == "" {
					continue
				}
				key := textRefKey(d.ID)
				path := groundingPath(d.Title)
				if ref, ok := s.refFor(key); ok {
					out = append(out, aigentic.InlineFile{Path: path, MediaType: "text/markdown", Ref: ref})
					origins = append(origins, groundingOrigin{Path: path, Key: key})
				} else {
					remaining := int(maxGroundingBytes - total)
					if len(t) <= remaining {
						total += int64(len(t))
						out = append(out, aigentic.InlineFile{Path: path, Content: t, MediaType: "text/markdown"})
						origins = append(origins, groundingOrigin{Path: path, Key: key})
					} else if kept := selectRelevantText(t, prompt, remaining); kept != "" {
						// A PARTIAL send is not the whole document, so it is NOT given an origin — caching a
						// reference for it would later re-send only the partial as if it were the full file.
						total += int64(len(kept))
						out = append(out, aigentic.InlineFile{Path: path, Content: kept, MediaType: "text/markdown"})
						gaps.partial = append(gaps.partial, path)
					} else {
						gaps.omitted = append(gaps.omitted, path)
					}
				}
				// The images READ OUT of this file at upload — kept compressed beside its text — are grounded
				// as their OWN parts (image/jpeg), so the assistant SEES the pictures (a nameplate, a wiring
				// photo), not only the text transcribed from them. They count against the SAME budget as text;
				// an image that no longer fits is NAMED, never silently dropped (Kein stummes Ausbleiben). Any
				// image part makes the whole turn vision-only on aigentic's side, which the ask handler honours.
				if pics, ok := s.docs.ExtractImages(d.ID); ok {
					for _, im := range pics {
						label := imageGroundingLabel(d.Title, im.Page)
						emit(imageRefKey(d.ID, im.Index), label, base64.StdEncoding.EncodeToString(im.Data), groundingImageMime, true)
					}
				}
			case "pending", "reading":
				gaps.notRead = append(gaps.notRead, groundingPath(d.Title))
			case "failed":
				gaps.unread = append(gaps.unread, groundingPath(d.Title))
			default:
				// Legacy file with no read: ground by its bytes, bounded (the pre-extract behaviour). A
				// remembered reference short-circuits the byte read entirely.
				key := textRefKey(d.ID)
				path := groundingPath(d.Title)
				if ref, ok := s.refFor(key); ok {
					out = append(out, aigentic.InlineFile{Path: path, MediaType: d.Mime, Ref: ref})
					origins = append(origins, groundingOrigin{Path: path, Key: key})
					continue
				}
				if !fits(int(d.Size)) {
					gaps.omitted = append(gaps.omitted, path)
					continue
				}
				b, ok := s.docs.Bytes(d.ID)
				if !ok || len(b) == 0 {
					continue
				}
				if !fits(len(b)) {
					gaps.omitted = append(gaps.omitted, path)
					continue
				}
				total += int64(len(b))
				part := aigentic.InlineFile{Path: path, MediaType: d.Mime}
				if strings.HasPrefix(d.Mime, "text/") {
					part.Content = string(b)
				} else {
					part.Content = base64.StdEncoding.EncodeToString(b)
				}
				out = append(out, part)
				origins = append(origins, groundingOrigin{Path: path, Key: key})
			}
		}
	}
	return out, gaps, origins
}

// groundingNote composes the honest disclosure prepended to a turn when the grounding is incomplete,
// naming each kind of gap with its own reason. Returns "" when the grounding is complete.
func groundingNote(g groundingGaps) string {
	if len(g.omitted) == 0 && len(g.notRead) == 0 && len(g.unread) == 0 && len(g.partial) == 0 && len(g.omittedImages) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("The room knowledge below is INCOMPLETE for this answer. ")
	if len(g.omitted) > 0 {
		b.WriteString("These documents were too large to include in full and were NOT provided to you: " +
			strings.Join(g.omitted, ", ") + ". ")
	}
	if len(g.omittedImages) > 0 {
		b.WriteString("These document images were too large to include alongside the text and were NOT provided to you: " +
			strings.Join(g.omittedImages, ", ") + ". ")
	}
	if len(g.partial) > 0 {
		b.WriteString("These documents are large, so only the parts most relevant to the question were included — other parts were left out: " +
			strings.Join(g.partial, ", ") + ". ")
	}
	if len(g.notRead) > 0 {
		b.WriteString("These uploaded files have not been read yet, so their text is NOT available to you: " +
			strings.Join(g.notRead, ", ") + ". ")
	}
	if len(g.unread) > 0 {
		b.WriteString("These uploaded files could not be read, so their text is NOT available to you: " +
			strings.Join(g.unread, ", ") + ". ")
	}
	b.WriteString("If a complete answer would depend on any of them, say clearly what you could not read.")
	return b.String()
}

// selectRelevantText picks the paragraphs of a large read most relevant to the question, up to budget
// bytes, keeping them in their original order. It is deliberately simple — the means at hand, not a
// retrieval engine: paragraphs (the blank-line-separated sections a chunked read already produced) are
// scored by how many distinct question words they contain, the best are taken until the budget is spent,
// and they are re-joined in reading order. With no question, the leading paragraphs are kept. Returns ""
// when not even one paragraph fits.
func selectRelevantText(text, prompt string, budget int) string {
	if budget <= 0 {
		return ""
	}
	paras := splitParagraphs(text)
	if len(paras) == 0 {
		return ""
	}
	terms := queryTerms(prompt)

	type scored struct {
		idx   int
		score int
	}
	ranked := make([]scored, len(paras))
	for i, p := range paras {
		ranked[i] = scored{idx: i, score: paragraphScore(p, terms)}
	}
	// Highest score first; ties keep reading order so a no-question request keeps the leading text.
	sort.SliceStable(ranked, func(a, b int) bool {
		if ranked[a].score != ranked[b].score {
			return ranked[a].score > ranked[b].score
		}
		return ranked[a].idx < ranked[b].idx
	})

	chosen := map[int]bool{}
	used := 0
	const sep = "\n\n"
	for _, r := range ranked {
		cost := len(paras[r.idx])
		if used > 0 {
			cost += len(sep)
		}
		if used+cost > budget {
			continue // skip this one, a later shorter paragraph may still fit
		}
		chosen[r.idx] = true
		used += cost
	}
	var b strings.Builder
	for i, p := range paras {
		if !chosen[i] {
			continue
		}
		if b.Len() > 0 {
			b.WriteString(sep)
		}
		b.WriteString(p)
	}
	return b.String()
}

// splitParagraphs breaks the read text into paragraphs on blank lines (the boundary a chunked read uses
// between sections), dropping empty runs.
func splitParagraphs(text string) []string {
	var out []string
	for _, p := range strings.Split(text, "\n\n") {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// queryTerms reduces a question to the distinct lowercase words worth matching (three characters or
// more), so scoring ignores punctuation and filler-length tokens.
func queryTerms(prompt string) map[string]bool {
	terms := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(prompt), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if len(w) >= 3 {
			terms[w] = true
		}
	}
	return terms
}

// paragraphScore counts how many distinct question terms appear in a paragraph.
func paragraphScore(p string, terms map[string]bool) int {
	if len(terms) == 0 {
		return 0
	}
	lower := strings.ToLower(p)
	score := 0
	for t := range terms {
		if strings.Contains(lower, t) {
			score++
		}
	}
	return score
}

// groundingPath is the display/provenance path aigentic shows for a grounding part; never used for
// any fs access. Falls back to a generic label so an untitled item still names itself.
func groundingPath(title string) string {
	if t := strings.TrimSpace(title); t != "" {
		return t
	}
	return "document"
}

// imageGroundingLabel is the provenance path for one read-out image part, naming the document and the
// page the picture came from ("Room manual (image on page 6).jpg"), so the model — and the honesty note
// that lists an omitted one — can point at exactly which picture. It ends in the image's real extension
// because aigentic's claude-cli leaf writes this part to disk under this path and its Read tool only
// treats the bytes as a picture when the name ends in an image extension; a label that ends in ")"
// reads as opaque binary and burns a whole (often timing-out) attempt (see extract.NameWithExt).
func imageGroundingLabel(title string, page int) string {
	base := groundingPath(title)
	if page > 0 {
		return base + " (image on page " + strconv.Itoa(page) + ")" + extract.ExtForMime(groundingImageMime)
	}
	return base + " (image)" + extract.ExtForMime(groundingImageMime)
}

// askFormat clamps the requested answer shape to what aigentic accepts, defaulting to markdown.
func askFormat(f string) string {
	switch strings.TrimSpace(f) {
	case "text", "json", "markdown":
		return strings.TrimSpace(f)
	default:
		return "markdown"
	}
}
