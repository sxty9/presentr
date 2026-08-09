package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"presentr/internal/aigentic"
	"presentr/internal/extract"
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

// A PDF read from BOTH tracks persists its compressed images beside the extract (so the room assistant
// can later be shown the pictures themselves), AND runs the per-image COMPRESSION as its own visible
// sub-step, separate from the AI read of the same image. Here the full read is driven end to end and the
// stored images and the section phases the UI would show are asserted.
func TestRunExtractionPersistsCompressedImages(t *testing.T) {
	// A stub aigentic that reads any image section as the same text; it also lets us observe that the read
	// ever entered the "compressing" phase before an image reached us.
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":{"output":"SN 9931 nameplate","engine":"claude","model":"claude-sonnet-4-6"}}`)
	}))
	defer stub.Close()

	s := newServer(t)
	s.ai = aigentic.New(stub.URL, "sekret")
	pdf := embeddedImagePDF(t, "Projector Epson EB-2250U", 2000, 40) // text layer + one embedded image
	if _, err := s.docs.AddFile(store.Document{ID: "mix", Title: "mixed.pdf", Kind: "file", Mime: "application/pdf", Author: "ada", ExtractState: "pending"}, bytes.NewReader(pdf), 100<<20); err != nil {
		t.Fatal(err)
	}
	s.runExtraction("mix")

	d, _ := s.docs.Get("mix")
	if d.ExtractState != "ready" {
		t.Fatalf("a mixed read must end ready, got %q (%s)", d.ExtractState, d.ExtractError)
	}
	// A completed read clears its in-progress phase.
	if d.ExtractSectionPhase != "" {
		t.Fatalf("a finished read must clear its section phase, got %q", d.ExtractSectionPhase)
	}
	// The compressed image is persisted beside the extract, as a real JPEG downscaled to the vision cap.
	imgs, ok := s.docs.ExtractImages("mix")
	if !ok || len(imgs) != 1 || imgs[0].Page != 1 {
		t.Fatalf("the read image must be persisted with its page, got ok=%v %+v", ok, imgs)
	}
	if len(imgs[0].Data) < 2 || imgs[0].Data[0] != 0xff || imgs[0].Data[1] != 0xd8 {
		t.Fatalf("the persisted image must be JPEG bytes")
	}
	// And that image is then part of the grounding as its own image/jpeg part (not only its text).
	parts, _ := s.roomGrounding("")
	var haveImage bool
	for _, p := range parts {
		if p.MediaType == "image/jpeg" {
			haveImage = true
		}
	}
	if !haveImage {
		t.Fatalf("the persisted image must be included in the room grounding")
	}
}

// The image-compression step is DEFERRED out of planning so the read loop can show it as its own
// per-image sub-step: an embedded-image PDF plans one image section whose bytes do not yet exist
// (NeedsCompression), produced only when Compress runs — the step the read loop publishes as "compressing".
func TestEmbeddedImagePlanDefersCompression(t *testing.T) {
	s := newServer(t)
	pdf := embeddedImagePDF(t, "wall plate HDMI 1", 64, 64)
	plan, err := extract.Prepare("application/pdf", pdf, s.sectionBudget())
	if err != nil {
		t.Fatal(err)
	}
	if plan.LocalText == "" {
		t.Fatalf("the text layer must be read exactly and locally")
	}
	if len(plan.Sections) != 1 || !plan.Sections[0].NeedsCompression() {
		t.Fatalf("the PDF must plan one deferred image-compression section, got %+v", plan.Sections)
	}
	if _, ok := plan.Sections[0].Compress(s.sectionBudget()); !ok {
		t.Fatalf("the deferred image must compress on demand")
	}
}

// An embedded image whose codec this reader cannot decode is not silently dropped: the text layer still
// reads, the picture is named as a gap in the assembled text (so its missing content is disclosed), and
// no image is persisted for grounding — the read still ends ready on its text.
func TestRunExtractionNamesUndecodableImage(t *testing.T) {
	s := newServer(t) // no AI needed: the text track reads locally, the image cannot be decoded at all
	pdf := embeddedImagePDFCS(t, "HDMI 1 to wall plate", 8, 8, "/DeviceCMYK", 4)
	if _, err := s.docs.AddFile(store.Document{ID: "u", Title: "u.pdf", Kind: "file", Mime: "application/pdf", Author: "ada", ExtractState: "pending"}, bytes.NewReader(pdf), 100<<20); err != nil {
		t.Fatal(err)
	}
	s.runExtraction("u")

	d, _ := s.docs.Get("u")
	if d.ExtractState != "ready" {
		t.Fatalf("the text layer must survive an undecodable image, got %q (%s)", d.ExtractState, d.ExtractError)
	}
	text, _ := s.docs.ExtractText("u")
	if !strings.Contains(text, "wall plate") {
		t.Fatalf("the exact text layer must be read: %q", text)
	}
	if !strings.Contains(text, "could not be read") {
		t.Fatalf("the undecodable image must be named as a gap: %q", text)
	}
	if _, ok := s.docs.ExtractImages("u"); ok {
		t.Fatalf("an undecodable image must not be persisted for grounding")
	}
}

// embeddedImagePDF builds a one-page PDF carrying a machine-readable text layer AND a real decodable
// embedded image (an uncompressed DeviceRGB raster), the shape of the feature's target file. Mirrors the
// extract package's own test fixture.
func embeddedImagePDF(t *testing.T, show string, w, h int) []byte {
	return embeddedImagePDFCS(t, show, w, h, "/DeviceRGB", 3)
}

// embeddedImagePDFCS is embeddedImagePDF with a chosen colour space and per-pixel component count, so a
// test can build either a decodable (DeviceRGB, 3) or an undecodable-here (DeviceCMYK, 4) embedded image.
func embeddedImagePDFCS(t *testing.T, show string, w, h int, colorSpace string, comps int) []byte {
	t.Helper()
	px := bytes.Repeat(bytes.Repeat([]byte{0x80}, comps), w*h)
	content := "BT /F1 12 Tf 72 720 Td (" + show + ") Tj ET"
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [ 3 0 R ] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /Resources << /Font << /F1 6 0 R >> /XObject << /Im0 5 0 R >> >> /MediaBox [0 0 612 792] /Contents 4 0 R >>",
		"",
		"",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offs := make([]int, len(objs)+1)
	writeObj := func(num int, dict, stream string) {
		offs[num] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s", num, dict)
		if stream != "" {
			buf.WriteString("\nstream\n")
			buf.WriteString(stream)
			buf.WriteString("\nendstream")
		}
		buf.WriteString("\nendobj\n")
	}
	for i, body := range objs {
		num := i + 1
		switch num {
		case 4:
			writeObj(4, fmt.Sprintf("<< /Length %d >>", len(content)), content)
		case 5:
			dict := fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace %s /BitsPerComponent 8 /Length %d >>", w, h, colorSpace, len(px))
			writeObj(5, dict, string(px))
		default:
			writeObj(num, body, "")
		}
	}
	x := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for num := 1; num <= len(objs); num++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offs[num])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF", len(objs)+1, x)
	return buf.Bytes()
}
