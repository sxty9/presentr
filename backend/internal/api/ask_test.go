package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"presentr/internal/aigentic"
	"presentr/internal/store"
)

// roomGrounding turns the whole pool into inline parts: text documents as inline markdown, text
// files inline, and images/PDFs base64-encoded with their media type.
func TestRoomGrounding(t *testing.T) {
	s := newServer(t)
	_ = s.docs.Add(store.Document{ID: store.NewID(), Title: "Notes", Kind: "text", Mime: "text/markdown", Content: "# Room"})
	png := []byte{0x89, 'P', 'N', 'G', 1, 2, 3}
	if _, err := s.docs.AddFile(store.Document{ID: "img", Title: "layout.png", Kind: "file", Mime: "image/png"}, bytes.NewReader(png), 100<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := s.docs.AddFile(store.Document{ID: "txt", Title: "wiring.txt", Kind: "file", Mime: "text/plain"}, bytes.NewReader([]byte("HDMI->proj")), 100<<20); err != nil {
		t.Fatal(err)
	}

	parts, gaps := s.roomGrounding("")
	if len(parts) != 3 {
		t.Fatalf("grounding has %d parts, want 3: %+v", len(parts), parts)
	}
	if len(gaps.omitted) != 0 || len(gaps.notRead) != 0 || len(gaps.unread) != 0 {
		t.Fatalf("small legacy documents must not be flagged as gaps: %+v", gaps)
	}
	byPath := map[string]aigentic.InlineFile{}
	for _, p := range parts {
		byPath[p.Path] = p
	}
	if byPath["Notes"].MediaType != "text/markdown" || byPath["Notes"].Content != "# Room" {
		t.Errorf("text doc part = %+v", byPath["Notes"])
	}
	if byPath["wiring.txt"].MediaType != "text/plain" || byPath["wiring.txt"].Content != "HDMI->proj" {
		t.Errorf("text file part = %+v", byPath["wiring.txt"])
	}
	img := byPath["layout.png"]
	if img.MediaType != "image/png" || img.Content != base64.StdEncoding.EncodeToString(png) {
		t.Errorf("image part not base64-encoded: %+v", img)
	}
}

// A document too large to fit aigentic's per-request budget is NOT silently dropped from the
// grounding: it is named in `omitted`, so ask can disclose to the model (and through it the user)
// that the answer could not draw on it (EHRLICH BLEIBEN).
func TestRoomGroundingNamesOmittedDocuments(t *testing.T) {
	s := newServer(t)
	// A small document that fits, and a large one that overruns the budget on its own.
	_ = s.docs.Add(store.Document{ID: store.NewID(), Title: "Fits", Kind: "text", Mime: "text/markdown", Content: "# small"})
	big := strings.Repeat("x", maxGroundingBytes+1)
	_ = s.docs.Add(store.Document{ID: store.NewID(), Title: "Huge manual", Kind: "text", Mime: "text/markdown", Content: big})

	parts, gaps := s.roomGrounding("")
	if len(parts) != 1 || parts[0].Path != "Fits" {
		t.Fatalf("grounding should carry only the fitting document, got %+v", parts)
	}
	if len(gaps.omitted) != 1 || gaps.omitted[0] != "Huge manual" {
		t.Fatalf("the oversized document must be named as omitted, got %+v", gaps.omitted)
	}
}

// A file grounds by the TEXT read out of it (the extract), not its raw bytes — so a big scanned file
// costs a question a few hundred bytes of text, not megabytes of base64. A file whose read is still
// pending or has failed is not sent at all; it is named as a gap so the answer can disclose it.
func TestRoomGroundingUsesExtractAndNamesUnreadFiles(t *testing.T) {
	s := newServer(t)
	// A ready file: its bytes are a large image, but its read produced a short text — the grounding
	// must carry the TEXT, never the bytes.
	bigImage := bytes.Repeat([]byte{0x89, 'P', 'N', 'G'}, 300000) // ~1.2 MB of "image" bytes
	if _, err := s.docs.AddFile(store.Document{ID: "ready", Title: "nameplate.png", Kind: "file", Mime: "image/png", ExtractState: "pending"}, bytes.NewReader(bigImage), 100<<20); err != nil {
		t.Fatal(err)
	}
	if err := s.docs.SetExtract("ready", store.Extract{State: "ready", Text: "Model EB-2250U, HDMI 1", Source: "ai", Model: "claude"}); err != nil {
		t.Fatal(err)
	}
	// A pending file and a failed file: both named, neither sent.
	if _, err := s.docs.AddFile(store.Document{ID: "pend", Title: "scan.pdf", Kind: "file", Mime: "application/pdf", ExtractState: "pending"}, bytes.NewReader([]byte("%PDF-1.7")), 100<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := s.docs.AddFile(store.Document{ID: "fail", Title: "broken.pdf", Kind: "file", Mime: "application/pdf", ExtractState: "pending"}, bytes.NewReader([]byte("%PDF-1.7")), 100<<20); err != nil {
		t.Fatal(err)
	}
	if err := s.docs.SetExtract("fail", store.Extract{State: "failed", Error: "no AI"}); err != nil {
		t.Fatal(err)
	}

	parts, gaps := s.roomGrounding("")
	if len(parts) != 1 {
		t.Fatalf("only the ready file should be grounded, got %d parts: %+v", len(parts), parts)
	}
	if parts[0].Path != "nameplate.png" || parts[0].MediaType != "text/markdown" || parts[0].Content != "Model EB-2250U, HDMI 1" {
		t.Fatalf("ready file must ground by its extract TEXT (not bytes): %+v", parts[0])
	}
	if len(gaps.notRead) != 1 || gaps.notRead[0] != "scan.pdf" {
		t.Fatalf("a pending file must be named as not-read: %+v", gaps.notRead)
	}
	if len(gaps.unread) != 1 || gaps.unread[0] != "broken.pdf" {
		t.Fatalf("a failed file must be named as unread: %+v", gaps.unread)
	}
	note := groundingNote(gaps)
	if !strings.Contains(note, "scan.pdf") || !strings.Contains(note, "broken.pdf") {
		t.Fatalf("the grounding note must disclose both unread files: %q", note)
	}
}

// ask with no configured client reports the assistant as unavailable rather than erroring out.
func TestAskDisabled(t *testing.T) {
	s := newServer(t) // New(..., nil) => no ai client
	rec := httptest.NewRecorder()
	s.ask(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"prompt":"hi"}`)), user())
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("ask without a client = %d, want 503", rec.Code)
	}
}

// ask grounds the turn in the pool and forwards it to aigentic on the caller's behalf, presenting
// the shared secret and the subject, and returns the engine's labelled answer.
func TestAskForwardsToAigentic(t *testing.T) {
	var gotSecret, gotSubject, gotKind string
	var gotInline int
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSecret = r.Header.Get("X-Aigentic-Internal-Secret")
		var body struct {
			Subject string `json:"subject"`
			Header  struct {
				Kind string `json:"kind"`
			} `json:"header"`
			Data json.RawMessage `json:"data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotSubject, gotKind = body.Subject, body.Header.Kind
		var data struct {
			Prompt string                `json:"prompt"`
			Inline []aigentic.InlineFile `json:"inline"`
		}
		_ = json.Unmarshal(body.Data, &data)
		gotInline = len(data.Inline)
		io.WriteString(w, `{"header":{"kind":"choose"},"data":{"output":"the answer","engine":"ollama","model":"llama3"}}`)
	}))
	defer stub.Close()

	s := newServer(t)
	s.ai = aigentic.New(stub.URL, "sekret")
	_ = s.docs.Add(store.Document{ID: store.NewID(), Title: "Notes", Kind: "text", Mime: "text/markdown", Content: "# Room"})

	rec := httptest.NewRecorder()
	s.ask(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"prompt":"how do I connect?","outputFormat":"markdown"}`)), user())

	if rec.Code != http.StatusOK {
		t.Fatalf("ask status %d, body %s", rec.Code, rec.Body.String())
	}
	var out struct{ Output, Engine, Model string }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Output != "the answer" || out.Engine != "ollama" || out.Model != "llama3" {
		t.Fatalf("ask result = %+v", out)
	}
	if gotSecret != "sekret" || gotSubject != "ada" || gotKind != "choose" {
		t.Fatalf("forwarded secret=%q subject=%q kind=%q", gotSecret, gotSubject, gotKind)
	}
	if gotInline != 1 {
		t.Fatalf("forwarded %d grounding parts, want 1", gotInline)
	}
}
