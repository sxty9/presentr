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

	res, err := s.ai.Run(r.Context(), u.Username, aigentic.Req{
		Prompt:       prompt,
		OutputFormat: askFormat(body.OutputFormat),
		Inline:       s.roomGrounding(),
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

// roomGrounding turns the pool into aigentic inline parts: every text document as inline markdown,
// every uploaded file as its bytes (text inline; images/PDFs base64 with their media type — the
// forms aigentic reads). Assembly is bounded by maxGroundingBytes; documents past the budget are
// dropped so one oversized pool cannot blow the request cap.
func (s *Server) roomGrounding() []aigentic.InlineFile {
	docs := s.docs.List()
	out := make([]aigentic.InlineFile, 0, len(docs))
	var total int64
	for _, d := range docs {
		switch d.Kind {
		case "text":
			c := strings.TrimSpace(d.Content)
			if c == "" {
				continue
			}
			if total+int64(len(c)) > maxGroundingBytes {
				continue
			}
			total += int64(len(c))
			out = append(out, aigentic.InlineFile{Path: groundingPath(d.Title), Content: c, MediaType: "text/markdown"})
		case "file":
			b, ok := s.docs.Bytes(d.ID)
			if !ok || len(b) == 0 {
				continue
			}
			if total+int64(len(b)) > maxGroundingBytes {
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
	return out
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
