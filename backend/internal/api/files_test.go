package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"presentr/internal/auth"
	"presentr/internal/store"
)

// pngSig is the 8-byte PNG signature http.DetectContentType keys on, enough to be classified as an
// image without a full encoder.
var pngSig = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 13}

func newServer(t *testing.T) *Server {
	t.Helper()
	docs, err := store.OpenDocs(filepath.Join(t.TempDir(), "data", "docs.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The upload/raw handlers reach only the document pool; the verifier and the other pools are
	// exercised through the guard, which these tests call the handlers beneath.
	return New(nil, docs, nil, nil)
}

func user() *auth.User { return &auth.User{Username: "ada", Groups: []string{"hp_presentr_use"}} }

// classifyUpload accepts exactly what aigentic can read (images, PDF, text) and rejects the rest
// with a reason.
func TestClassifyUpload(t *testing.T) {
	cases := []struct {
		name    string
		data    []byte
		wantOK  bool
		wantMim string
	}{
		{"layout.png", pngSig, true, "image/png"},
		{"manual.pdf", []byte("%PDF-1.7\n1 0 obj\n"), true, "application/pdf"},
		{"notes.md", []byte("# Room\n\nThe projector is an Epson."), true, "text/markdown"},
		{"wiring.txt", []byte("HDMI 1 -> projector"), true, "text/plain; charset=utf-8"},
		{"data.csv", []byte("device,port\nprojector,hdmi1\n"), true, "text/csv"},
		{"config.json", []byte(`{"device":"projector"}`), true, "text/plain; charset=utf-8"},
		{"blob.bin", []byte{0x00, 0x01, 0x02, 0x00, 0xff, 0xfe, 0x00}, false, ""},
	}
	for _, c := range cases {
		mime, ok, reason := classifyUpload(c.name, c.data)
		if ok != c.wantOK {
			t.Errorf("%s: ok=%v reason=%q, want ok=%v", c.name, ok, reason, c.wantOK)
			continue
		}
		if ok && mime != c.wantMim {
			t.Errorf("%s: mime=%q, want %q", c.name, mime, c.wantMim)
		}
		if !ok && reason == "" {
			t.Errorf("%s: a rejection must carry a reason", c.name)
		}
	}
}

// A multipart batch stores the usable files, reports the unusable one with a reason, and the stored
// image's bytes come back verbatim from the raw endpoint under its stored type.
func TestUploadAndRaw(t *testing.T) {
	s := newServer(t)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	writePart := func(field, name string, data []byte) {
		w, _ := mw.CreateFormFile(field, name)
		w.Write(data)
	}
	writePart("files", "layout.png", pngSig)
	writePart("files", "notes.md", []byte("# Room notes"))
	writePart("files", "junk.bin", []byte{0x00, 0xff, 0x00, 0x13, 0x37, 0x00})
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/services/presentr/docs", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	s.uploadFiles(rec, req, user())

	if rec.Code != http.StatusOK {
		t.Fatalf("upload status %d, body %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Documents []store.Document `json:"documents"`
		Rejected  []struct {
			Name, Reason string
		} `json:"rejected"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Documents) != 2 {
		t.Fatalf("accepted %d documents, want 2: %+v", len(out.Documents), out.Documents)
	}
	if len(out.Rejected) != 1 || out.Rejected[0].Name != "junk.bin" || out.Rejected[0].Reason == "" {
		t.Fatalf("rejected = %+v, want exactly junk.bin with a reason", out.Rejected)
	}

	// Find the stored image and fetch its raw bytes.
	var imgID, imgMime string
	for _, d := range out.Documents {
		if d.Kind != "file" {
			t.Fatalf("stored document is kind %q, want file", d.Kind)
		}
		if d.Mime == "image/png" {
			imgID, imgMime = d.ID, d.Mime
		}
	}
	if imgID == "" {
		t.Fatal("the PNG was not stored as image/png")
	}

	rreq := httptest.NewRequest(http.MethodGet, "/api/services/presentr/docs/"+imgID+"/raw", nil)
	rreq.SetPathValue("id", imgID)
	rrec := httptest.NewRecorder()
	s.getRaw(rrec, rreq, user())

	if rrec.Code != http.StatusOK {
		t.Fatalf("raw status %d", rrec.Code)
	}
	if ct := rrec.Header().Get("Content-Type"); ct != imgMime {
		t.Fatalf("raw content-type %q, want %q", ct, imgMime)
	}
	if !bytes.Equal(rrec.Body.Bytes(), pngSig) {
		t.Fatalf("raw bytes mismatch: got %v", rrec.Body.Bytes())
	}
}

// A batch with nothing usable answers 415 and still names why, and getRaw 404s a text document
// (which carries no bytes).
func TestUploadAllRejectedAndRawText(t *testing.T) {
	s := newServer(t)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	w, _ := mw.CreateFormFile("files", "junk.bin")
	w.Write([]byte{0x00, 0x01, 0x00, 0xfe})
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/x", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	s.uploadFiles(rec, req, user())
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("all-rejected status %d, want 415", rec.Code)
	}

	// A text document has no blob → raw is a 404, not a server error.
	textID := store.NewID()
	if err := s.docs.Add(store.Document{ID: textID, Title: "n", Kind: "text", Mime: "text/markdown", Content: "hi"}); err != nil {
		t.Fatal(err)
	}
	rreq := httptest.NewRequest(http.MethodGet, "/x", nil)
	rreq.SetPathValue("id", textID)
	rrec := httptest.NewRecorder()
	s.getRaw(rrec, rreq, user())
	if rrec.Code != http.StatusNotFound {
		t.Fatalf("raw of a text document status %d, want 404", rrec.Code)
	}
}
