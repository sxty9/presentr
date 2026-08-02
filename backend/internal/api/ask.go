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
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"presentr/internal/aigentic"
	"presentr/internal/auth"
)

const (
	maxAskBody = 1 << 20 // the prompt/format submission (the grounding is assembled server-side)
	// The total raw bytes assembled into one grounding, kept under aigentic's 32 MiB request ceiling
	// so the prompt and envelope still fit. Documents past the budget are skipped (Portionierte Daten).
	maxGroundingBytes = 24 << 20
)

// ask runs one AI turn for the caller, grounded in the whole document pool, and returns the model's
// answer labelled with the engine/model that produced it (Kennzeichnungspflicht). The prompt and the
// requested answer shape come from the UI; the grounding is assembled here.
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

	inline, gaps := s.roomGrounding()
	if note := groundingNote(gaps); note != "" {
		// The grounding is not the whole pool: some documents were too large to include, and some
		// files have not been read yet or could not be read. Rather than let the model answer as if it
		// had seen everything (a silent, dishonest gap), tell it plainly what it did NOT receive, so
		// its answer can name what it is — and is not — based on (EHRLICH BLEIBEN).
		inline = append([]aigentic.InlineFile{{
			Path:      "grounding-note",
			MediaType: "text/markdown",
			Content:   note,
		}}, inline...)
	}

	res, err := s.ai.Run(r.Context(), u.Username, aigentic.Req{
		Prompt:       prompt,
		OutputFormat: askFormat(body.OutputFormat),
		Inline:       inline,
	})
	if err != nil {
		switch {
		case errors.Is(err, aigentic.ErrDisabled):
			writeErr(w, http.StatusServiceUnavailable, "The room assistant is not configured on this server")
		case errors.Is(err, aigentic.ErrUnavailable):
			writeErr(w, http.StatusServiceUnavailable, "No AI engine is available — an admin can link a Claude credential or start a local model in aigentic")
		default:
			writeErr(w, http.StatusBadGateway, "The assistant could not answer")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"output": res.Output, "model": res.Model, "engine": res.Engine})
}

// groundingGaps is what the assembled grounding left out, each list kept separate so the note to the
// model can name the DIFFERENT reasons a document is missing (too large / not read yet / could not be
// read) rather than blur them into one (Kein Befund ohne Bedeutung).
type groundingGaps struct {
	omitted []string // too large to fit aigentic's per-request budget
	notRead []string // a file whose text has not been read yet (read still pending)
	unread  []string // a file whose text read failed (a retry may fix it)
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
// THE MISSING BUILDING BLOCK, still: splitting one large read into question-relevant sections (RAG)
// belongs in aigentic, not here (Reuse before Build; Keine ähnlichen Geschwister). The extract is the
// FOUNDATION that sibling order builds on — it chunks the read TEXT, not the raw bytes. Until it
// lands, an over-budget extract is named as not-fully-consulted rather than faked.
func (s *Server) roomGrounding() (inline []aigentic.InlineFile, gaps groundingGaps) {
	docs := s.docs.List()
	out := make([]aigentic.InlineFile, 0, len(docs))
	var total int64
	fits := func(n int) bool { return total+int64(n) <= maxGroundingBytes }
	add := func(title, content, media string) {
		if content == "" {
			return
		}
		if !fits(len(content)) {
			gaps.omitted = append(gaps.omitted, groundingPath(title))
			return
		}
		total += int64(len(content))
		out = append(out, aigentic.InlineFile{Path: groundingPath(title), Content: content, MediaType: media})
	}
	for _, d := range docs {
		switch d.Kind {
		case "text":
			add(d.Title, strings.TrimSpace(d.Content), "text/markdown")
		case "file":
			switch d.ExtractState {
			case "ready":
				// Ground by the read text — small, exact enough, and already vision-read for images.
				text, ok := s.docs.ExtractText(d.ID)
				if ok {
					add(d.Title, strings.TrimSpace(text), "text/markdown")
				}
			case "pending":
				gaps.notRead = append(gaps.notRead, groundingPath(d.Title))
			case "failed":
				gaps.unread = append(gaps.unread, groundingPath(d.Title))
			default:
				// Legacy file with no read: ground by its bytes, bounded (the pre-extract behaviour).
				if !fits(int(d.Size)) {
					gaps.omitted = append(gaps.omitted, groundingPath(d.Title))
					continue
				}
				b, ok := s.docs.Bytes(d.ID)
				if !ok || len(b) == 0 {
					continue
				}
				if !fits(len(b)) {
					gaps.omitted = append(gaps.omitted, groundingPath(d.Title))
					continue
				}
				total += int64(len(b))
				part := aigentic.InlineFile{Path: groundingPath(d.Title), MediaType: d.Mime}
				if strings.HasPrefix(d.Mime, "text/") {
					part.Content = string(b)
				} else {
					part.Content = base64.StdEncoding.EncodeToString(b)
				}
				out = append(out, part)
			}
		}
	}
	return out, gaps
}

// groundingNote composes the honest disclosure prepended to a turn when the grounding is incomplete,
// naming each kind of gap with its own reason. Returns "" when the grounding is complete.
func groundingNote(g groundingGaps) string {
	if len(g.omitted) == 0 && len(g.notRead) == 0 && len(g.unread) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("The room knowledge below is INCOMPLETE for this answer. ")
	if len(g.omitted) > 0 {
		b.WriteString("These documents were too large to include in full and were NOT provided to you: " +
			strings.Join(g.omitted, ", ") + ". ")
	}
	if len(g.notRead) > 0 {
		b.WriteString("These uploaded files have not been read yet, so their text is NOT available to you: " +
			strings.Join(g.notRead, ", ") + ". ")
	}
	if len(g.unread) > 0 {
		b.WriteString("These uploaded files could not be read, so their text is NOT available to you: " +
			strings.Join(g.unread, ", ") + ". ")
	}
	b.WriteString("If a complete answer would depend on any of them, say clearly that you could not read them.")
	return b.String()
}

// groundingPath is the display/provenance path aigentic shows for a grounding part; never used for
// any fs access. Falls back to a generic label so an untitled item still names itself.
func groundingPath(title string) string {
	if t := strings.TrimSpace(title); t != "" {
		return t
	}
	return "document"
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
