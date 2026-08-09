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
	"sort"
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

	inline, gaps := s.roomGrounding(prompt)
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
	partial []string // a large read where only the sections relevant to the question were included
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
func (s *Server) roomGrounding(prompt string) (inline []aigentic.InlineFile, gaps groundingGaps) {
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
				remaining := int(maxGroundingBytes - total)
				if len(t) <= remaining {
					add(d.Title, t, "text/markdown")
				} else if kept := selectRelevantText(t, prompt, remaining); kept != "" {
					total += int64(len(kept))
					out = append(out, aigentic.InlineFile{Path: groundingPath(d.Title), Content: kept, MediaType: "text/markdown"})
					gaps.partial = append(gaps.partial, groundingPath(d.Title))
				} else {
					gaps.omitted = append(gaps.omitted, groundingPath(d.Title))
				}
			case "pending", "reading":
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
	if len(g.omitted) == 0 && len(g.notRead) == 0 && len(g.unread) == 0 && len(g.partial) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("The room knowledge below is INCOMPLETE for this answer. ")
	if len(g.omitted) > 0 {
		b.WriteString("These documents were too large to include in full and were NOT provided to you: " +
			strings.Join(g.omitted, ", ") + ". ")
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

// askFormat clamps the requested answer shape to what aigentic accepts, defaulting to markdown.
func askFormat(f string) string {
	switch strings.TrimSpace(f) {
	case "text", "json", "markdown":
		return strings.TrimSpace(f)
	default:
		return "markdown"
	}
}
