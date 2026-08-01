package api

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
	// The upload/raw handlers reach only the document pool; the verifier, aigentic client and the
	// other pools are exercised through the guard, which these tests call the handlers beneath.
	return New(nil, docs, nil, nil, nil)
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

// zeros is an infinite reader of NUL bytes, used to stream a large upload body WITHOUT allocating it
// — the file flows through the daemon as it would over the network, exercising the streaming path.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// streamUpload builds a POST /docs whose multipart body is generated on the fly (via a pipe), so a
// 100 MB file never sits whole in the test's memory. The part carries head followed by NUL padding
// up to total bytes.
func streamUpload(t *testing.T, field, name string, head []byte, total int64) *http.Request {
	t.Helper()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	ct := mw.FormDataContentType()
	go func() {
		w, err := mw.CreateFormFile(field, name)
		if err == nil {
			_, err = w.Write(head)
		}
		if err == nil && total > int64(len(head)) {
			_, err = io.CopyN(w, zeros{}, total-int64(len(head)))
		}
		mw.Close()
		pw.CloseWithError(err)
	}()
	req := httptest.NewRequest(http.MethodPost, "/api/services/presentr/docs", pr)
	req.Header.Set("Content-Type", ct)
	return req
}

// A single file one byte over the per-file limit is turned away with the NAMED reason (415 + detail),
// never a torn stream — even though its bytes are read as a stream, not buffered. This is the honest
// property the previous task established (PR #8), preserved above the new 100 MB limit.
func TestUploadOversizedFileNamedRejection(t *testing.T) {
	s := newServer(t)

	// A PDF header followed by NUL padding, one byte past the 100 MB per-file limit.
	req := streamUpload(t, "files", "huge-manual.pdf", []byte("%PDF-1.7\n"), maxFileBytes+1)
	rec := httptest.NewRecorder()
	s.uploadFiles(rec, req, user())

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("oversized-file status %d, want 415: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Detail   string `json:"detail"`
		Rejected []struct {
			Name, Reason string
		} `json:"rejected"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Rejected) != 1 || out.Rejected[0].Name != "huge-manual.pdf" {
		t.Fatalf("rejected = %+v, want exactly huge-manual.pdf", out.Rejected)
	}
	// The reason and the detail must name the 100 MB limit — an honest rejection, never a bare failure.
	if !strings.Contains(out.Rejected[0].Reason, "100 MB limit") {
		t.Fatalf("rejection reason %q must name the 100 MB per-file limit", out.Rejected[0].Reason)
	}
	if !strings.Contains(out.Detail, "100 MB limit") {
		t.Fatalf("detail %q must name the 100 MB per-file limit", out.Detail)
	}
}

// A file exactly AT the 100 MB limit (the boundary just under "too large") is accepted and streamed
// into the pool in full — the limit is inclusive, and the streaming ingest handles a full-size file.
func TestUploadAtLimitAccepted(t *testing.T) {
	s := newServer(t)

	req := streamUpload(t, "files", "manual.pdf", []byte("%PDF-1.7\n"), maxFileBytes)
	rec := httptest.NewRecorder()
	s.uploadFiles(rec, req, user())

	if rec.Code != http.StatusOK {
		t.Fatalf("at-limit upload status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Documents []store.Document                `json:"documents"`
		Rejected  []struct{ Name, Reason string } `json:"rejected"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Rejected) != 0 {
		t.Fatalf("at-limit file was rejected: %+v", out.Rejected)
	}
	if len(out.Documents) != 1 || out.Documents[0].Mime != "application/pdf" {
		t.Fatalf("accepted = %+v, want one application/pdf document", out.Documents)
	}
	if out.Documents[0].Size != maxFileBytes {
		t.Fatalf("stored size %d, want %d (the full file)", out.Documents[0].Size, int64(maxFileBytes))
	}
}

// A batch with nothing usable answers 415 and still names why, and getRaw serves a typed text
// document's inline markdown (the viewer navigates one pool across all kinds, so a text note is
// fetched the same way as a file). An unknown id still 404s.
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

	// A text document's raw is its inline markdown, served under its stored type.
	textID := store.NewID()
	if err := s.docs.Add(store.Document{ID: textID, Title: "n", Kind: "text", Mime: "text/markdown", Content: "hi"}); err != nil {
		t.Fatal(err)
	}
	rreq := httptest.NewRequest(http.MethodGet, "/x", nil)
	rreq.SetPathValue("id", textID)
	rrec := httptest.NewRecorder()
	s.getRaw(rrec, rreq, user())
	if rrec.Code != http.StatusOK {
		t.Fatalf("raw of a text document status %d, want 200", rrec.Code)
	}
	if rrec.Body.String() != "hi" {
		t.Fatalf("raw of a text document body %q, want %q", rrec.Body.String(), "hi")
	}
	if ct := rrec.Header().Get("Content-Type"); ct != "text/markdown" {
		t.Fatalf("raw of a text document content-type %q, want %q", ct, "text/markdown")
	}

	// An unknown id still 404s.
	ureq := httptest.NewRequest(http.MethodGet, "/x", nil)
	ureq.SetPathValue("id", "does-not-exist")
	urec := httptest.NewRecorder()
	s.getRaw(urec, ureq, user())
	if urec.Code != http.StatusNotFound {
		t.Fatalf("raw of an unknown id status %d, want 404", urec.Code)
	}
}
