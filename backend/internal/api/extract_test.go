package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"presentr/internal/aigentic"
	"presentr/internal/store"
)

// A text file is read deterministically at upload: runExtraction stores its bytes as the extract, from
// the local text-layer path, with no AI involved.
func TestRunExtractionTextFile(t *testing.T) {
	s := newServer(t)
	if _, err := s.docs.AddFile(store.Document{ID: "t1", Title: "wiring.txt", Kind: "file", Mime: "text/plain", Author: "ada", ExtractState: "pending"}, bytes.NewReader([]byte("HDMI 1 -> projector")), 100<<20); err != nil {
		t.Fatal(err)
	}
	s.runExtraction("t1")

	d, _ := s.docs.Get("t1")
	if d.ExtractState != "ready" || d.ExtractSource != "text-layer" {
		t.Fatalf("text file read = state %q source %q, want ready/text-layer", d.ExtractState, d.ExtractSource)
	}
	if text, _ := s.docs.ExtractText("t1"); text != "HDMI 1 -> projector" {
		t.Fatalf("stored extract = %q", text)
	}
}

// An image is read by the AI: runExtraction forwards it to aigentic's extract capability and stores the
// transcription labelled with the model that produced it (Kennzeichnungspflicht).
func TestRunExtractionImageUsesAI(t *testing.T) {
	var gotKind string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Header struct {
				Kind string `json:"kind"`
			} `json:"header"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotKind = body.Header.Kind
		io.WriteString(w, `{"data":{"output":"Model EB-2250U\nInput: HDMI 1","engine":"claude","model":"claude-sonnet-4-6"}}`)
	}))
	defer stub.Close()

	s := newServer(t)
	s.ai = aigentic.New(stub.URL, "sekret")
	if _, err := s.docs.AddFile(store.Document{ID: "img", Title: "plate.png", Kind: "file", Mime: "image/png", Author: "ada", ExtractState: "pending"}, bytes.NewReader([]byte{0x89, 'P', 'N', 'G'}), 100<<20); err != nil {
		t.Fatal(err)
	}
	s.runExtraction("img")

	if gotKind != "extract" {
		t.Fatalf("the AI read must go through aigentic's extract capability, kind=%q", gotKind)
	}
	d, _ := s.docs.Get("img")
	if d.ExtractState != "ready" || d.ExtractSource != "ai" || d.ExtractModel != "claude-sonnet-4-6" {
		t.Fatalf("image read = %+v, want ready/ai labelled with the model", d)
	}
	if text, _ := s.docs.ExtractText("img"); text != "Model EB-2250U\nInput: HDMI 1" {
		t.Fatalf("stored transcription = %q", text)
	}
}

// A read that fails is a NAMED, retriable state, never a silent empty extract: with no AI configured an
// image read fails with a clear reason; a re-run (reExtract) flips it back to pending to try again.
func TestExtractionFailureIsNamedAndRetriable(t *testing.T) {
	s := newServer(t) // no ai client
	if _, err := s.docs.AddFile(store.Document{ID: "img", Title: "plate.png", Kind: "file", Mime: "image/png", Author: "ada", ExtractState: "pending"}, bytes.NewReader([]byte{0x89, 'P', 'N', 'G'}), 100<<20); err != nil {
		t.Fatal(err)
	}
	s.runExtraction("img")

	d, _ := s.docs.Get("img")
	if d.ExtractState != "failed" || d.ExtractError == "" {
		t.Fatalf("a failed read must record state=failed with a reason, got %+v", d)
	}

	// reExtract flips it back to pending (a retry, no re-upload) and answers ok.
	rreq := httptest.NewRequest(http.MethodPost, "/x", nil)
	rreq.SetPathValue("id", "img")
	rrec := httptest.NewRecorder()
	s.reExtract(rrec, rreq, user())
	if rrec.Code != http.StatusOK {
		t.Fatalf("reExtract status %d, want 200", rrec.Code)
	}
	if d, _ := s.docs.Get("img"); d.ExtractState != "pending" {
		t.Fatalf("after retry the state must be pending, got %q", d.ExtractState)
	}
}

// reExtract and getExtract act only on file documents: a text document or an unknown id is a 404, not a
// server error (the same shape as getRaw). getExtract returns the read state and text for a file.
func TestExtractEndpointsScope(t *testing.T) {
	s := newServer(t)
	// A text document carries no file read.
	textID := store.NewID()
	if err := s.docs.Add(store.Document{ID: textID, Title: "note", Kind: "text", Mime: "text/markdown", Content: "hi"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{textID, "nope"} {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		s.getExtract(rec, req, user())
		if rec.Code != http.StatusNotFound {
			t.Fatalf("getExtract(%q) status %d, want 404", id, rec.Code)
		}
		preq := httptest.NewRequest(http.MethodPost, "/x", nil)
		preq.SetPathValue("id", id)
		prec := httptest.NewRecorder()
		s.reExtract(prec, preq, user())
		if prec.Code != http.StatusNotFound {
			t.Fatalf("reExtract(%q) status %d, want 404", id, prec.Code)
		}
	}

	// A ready file returns its state and text.
	if _, err := s.docs.AddFile(store.Document{ID: "f", Title: "wiring.txt", Kind: "file", Mime: "text/plain", ExtractState: "pending"}, bytes.NewReader([]byte("x")), 100<<20); err != nil {
		t.Fatal(err)
	}
	if err := s.docs.SetExtract("f", store.Extract{State: "ready", Text: "HDMI", Source: "text-layer"}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.SetPathValue("id", "f")
	rec := httptest.NewRecorder()
	s.getExtract(rec, req, user())
	if rec.Code != http.StatusOK {
		t.Fatalf("getExtract status %d", rec.Code)
	}
	var out struct {
		State, Source, Text string
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.State != "ready" || out.Source != "text-layer" || out.Text != "HDMI" {
		t.Fatalf("getExtract body = %+v", out)
	}
}
