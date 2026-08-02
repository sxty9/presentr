package extract

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"image/jpeg"
	"strings"
	"testing"
)

// stubAI is a fake AIExtractor: it records the call and returns a canned transcription (or an error),
// so the routing and provenance can be tested without a live AI.
type stubAI struct {
	called  bool
	gotMime string
	text    string
	engine  string
	model   string
	err     error
}

func (s *stubAI) Extract(_ context.Context, _ /*subject*/, _ /*filename*/, mime string, _ []byte) (string, string, string, error) {
	s.called = true
	s.gotMime = mime
	return s.text, s.engine, s.model, s.err
}

// textPDF builds a minimal, valid-enough PDF with an UNCOMPRESSED content stream showing text via BT/Tj
// — a "PDF with a text layer" — so the deterministic reader has real input and NO AI is needed.
func textPDF(t *testing.T, show string) []byte {
	t.Helper()
	content := "BT /F1 12 Tf 72 720 Td (" + show + ") Tj ET"
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	b.WriteString("1 0 obj<< /Type /Catalog >>endobj\n")
	b.WriteString("2 0 obj<< /Length ")
	b.WriteString(itoa(len(content)))
	b.WriteString(" >>\nstream\n")
	b.WriteString(content)
	b.WriteString("\nendstream endobj\n")
	b.WriteString("%%EOF")
	return b.Bytes()
}

// flatePDF builds a PDF whose content stream is FlateDecode-compressed, to exercise the inflate path.
func flatePDF(t *testing.T, show string) []byte {
	t.Helper()
	content := "BT (" + show + ") Tj ET"
	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	_, _ = zw.Write([]byte(content))
	_ = zw.Close()
	var b bytes.Buffer
	b.WriteString("%PDF-1.5\n")
	b.WriteString("2 0 obj<< /Filter /FlateDecode /Length ")
	b.WriteString(itoa(zbuf.Len()))
	b.WriteString(" >>\nstream\n")
	b.Write(zbuf.Bytes())
	b.WriteString("\nendstream endobj\n%%EOF")
	return b.Bytes()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}

// CASE 1 — a document WITH a text layer is read deterministically, exactly, with NO AI.
func TestRunTextLayerPDFNoAI(t *testing.T) {
	ai := &stubAI{}
	res, err := Run(context.Background(), "ada", "manual.pdf", "application/pdf", textPDF(t, "Projector Epson EB-2250U"), ai)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ai.called {
		t.Fatal("a PDF with a readable text layer must not call the AI (it is exact and free)")
	}
	if res.Source != "text-layer" {
		t.Fatalf("source = %q, want text-layer", res.Source)
	}
	if !strings.Contains(res.Text, "Projector Epson EB-2250U") {
		t.Fatalf("text layer not read: %q", res.Text)
	}
}

// The FlateDecode path reads a compressed text layer just the same.
func TestRunFlatePDFTextLayer(t *testing.T) {
	ai := &stubAI{}
	res, err := Run(context.Background(), "ada", "c.pdf", "application/pdf", flatePDF(t, "HDMI 1 wired to the wall plate"), ai)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ai.called || res.Source != "text-layer" || !strings.Contains(res.Text, "wall plate") {
		t.Fatalf("compressed text layer not read without AI: called=%v src=%q text=%q", ai.called, res.Source, res.Text)
	}
}

// A full-resolution embedded image is DOWNSCALED before it is sent to the AI: its long edge is capped
// and it is re-encoded as a compact JPEG. This is the change that lets a photo-heavy PDF be read at all —
// a 15-megabyte sensor frame no longer ships whole.
func TestEmbeddedImageIsDownscaledForVision(t *testing.T) {
	pdf := rgbImagePDF(t, "spec sheet", 2400, 60) // a wide image, long edge 2400 > the 1568 cap
	secs, skipped, ok := extractImageSections(pdf, 20<<20)
	if !ok || len(secs) != 1 || skipped != 0 {
		t.Fatalf("expected one decodable image section, got ok=%v n=%d skipped=%d", ok, len(secs), skipped)
	}
	if secs[0].Mime != "image/jpeg" {
		t.Fatalf("a downscaled image must be sent as JPEG, got %q", secs[0].Mime)
	}
	img, err := jpeg.Decode(bytes.NewReader(secs[0].Data))
	if err != nil {
		t.Fatalf("the section must be a valid JPEG: %v", err)
	}
	if b := img.Bounds(); b.Dx() > visionLongEdge || b.Dy() > visionLongEdge {
		t.Fatalf("the image must be downscaled to a %d px long edge, got %dx%d", visionLongEdge, b.Dx(), b.Dy())
	}
	if b := img.Bounds(); b.Dx() != visionLongEdge {
		t.Fatalf("a 2400px-wide image should scale its long edge exactly to %d, got %d", visionLongEdge, b.Dx())
	}
}

// CASE 2 — an image with text on it is READ by the AI (its transcription is stored, labelled with the
// model that produced it).
func TestRunImageUsesAI(t *testing.T) {
	ai := &stubAI{text: "SN: 12345\nInput: HDMI", engine: "claude", model: "claude-sonnet-4-6"}
	res, err := Run(context.Background(), "ada", "plate.png", "image/png", []byte{0x89, 'P', 'N', 'G'}, ai)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !ai.called || ai.gotMime != "image/png" {
		t.Fatalf("an image must be read by the AI as image/png: called=%v mime=%q", ai.called, ai.gotMime)
	}
	if res.Source != "ai" || res.Model != "claude-sonnet-4-6" || res.Text != "SN: 12345\nInput: HDMI" {
		t.Fatalf("AI result not carried through with its model label: %+v", res)
	}
}

// rgbImagePDF builds a one-page PDF that carries BOTH a machine-readable text layer (the shown string)
// AND a real, decodable embedded image (an uncompressed DeviceRGB raster referenced by the page's
// resources). It is the shape of the feature's target file — a LaTeX-style document with photos — so the
// two independent tracks (exact text layer, AI-read image) can be exercised together.
func rgbImagePDF(t *testing.T, show string, w, h int) []byte {
	t.Helper()
	px := bytes.Repeat([]byte{0x80, 0x80, 0x80}, w*h) // a flat gray raster; decodes cleanly to an image
	content := "BT /F1 12 Tf 72 720 Td (" + show + ") Tj ET"
	// The content stream (4) is placed BEFORE the image (5): the text-layer reader takes a window of the
	// bytes preceding each `stream` to spot an /Image marker, so a content stream must not sit right after
	// an image object — exactly how a real producer lays a page out (content, then its image resources).
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [ 3 0 R ] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /Resources << /Font << /F1 6 0 R >> /XObject << /Im0 5 0 R >> >> /MediaBox [0 0 612 792] /Contents 4 0 R >>",
		"", // 4: content stream, built below
		"", // 5: image XObject, built below (has a stream)
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offs := make([]int, len(objs)+1)
	writeObj := func(num int, dict, stream string) {
		offs[num] = buf.Len()
		buf.WriteString(itoa(num))
		buf.WriteString(" 0 obj\n")
		buf.WriteString(dict)
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
			writeObj(4, "<< /Length "+itoa(len(content))+" >>", content)
		case 5:
			dict := "<< /Type /XObject /Subtype /Image /Width " + itoa(w) + " /Height " + itoa(h) +
				" /ColorSpace /DeviceRGB /BitsPerComponent 8 /Length " + itoa(len(px)) + " >>"
			writeObj(5, dict, string(px))
		default:
			writeObj(num, body, "")
		}
	}
	x := buf.Len()
	buf.WriteString("xref\n0 ")
	buf.WriteString(itoa(len(objs) + 1))
	buf.WriteString("\n0000000000 65535 f \n")
	for num := 1; num <= len(objs); num++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offs[num])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF", len(objs)+1, x)
	return buf.Bytes()
}

// The two tracks run INDEPENDENTLY: a PDF that has both a text layer and an embedded image reads the
// text EXACTLY and locally (no AI, no waiting), and reads the image SEPARATELY with the vision AI — the
// presence of the image never drags the text into the AI path. The assembled extract carries both, and
// the image's text is labelled by the page it came from.
func TestRunTextAndImagesAreIndependentTracks(t *testing.T) {
	ai := &stubAI{text: "SN 9931 on the nameplate", engine: "claude", model: "claude-sonnet-4-6"}
	pdf := rgbImagePDF(t, "Projector Epson EB-2250U", 8, 8)
	res, err := Run(context.Background(), "ada", "mixed.pdf", "application/pdf", pdf, ai)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ai.gotMime != "image/jpeg" {
		t.Fatalf("the embedded image must be downscaled and sent as a JPEG, got mime %q", ai.gotMime)
	}
	if !strings.Contains(res.Text, "Projector Epson EB-2250U") {
		t.Fatalf("the text layer must be read exactly and locally: %q", res.Text)
	}
	if !strings.Contains(res.Text, "SN 9931 on the nameplate") {
		t.Fatalf("the image text must be read by the AI and included: %q", res.Text)
	}
	if !strings.Contains(res.Text, "Image on page 1") {
		t.Fatalf("the image text must be labelled by its page: %q", res.Text)
	}
	if res.Source != "mixed" {
		t.Fatalf("a PDF read from both tracks is source %q, want mixed", res.Source)
	}
	if res.Model != "claude-sonnet-4-6" {
		t.Fatalf("a mixed read must carry the model that read its images, got %q", res.Model)
	}
}

// The text track NEVER depends on the AI: a text-layer PDF with an embedded image whose text the AI
// cannot read (no AI configured) still yields the exact text layer, with the image named as an
// undisclosed gap rather than failing the whole read.
func TestRunTextLayerSurvivesWhenImageUnreadable(t *testing.T) {
	pdf := rgbImagePDF(t, "HDMI 1 wired to the wall plate", 8, 8)
	res, err := Run(context.Background(), "ada", "mixed.pdf", "application/pdf", pdf, nil) // no AI
	if err != nil {
		t.Fatalf("Run must not fail when only the image track cannot be read: %v", err)
	}
	if !strings.Contains(res.Text, "wall plate") {
		t.Fatalf("the exact text layer must survive with no AI: %q", res.Text)
	}
	if res.Source != "text-layer" {
		t.Fatalf("with only the text track read, source = %q, want text-layer", res.Source)
	}
}

// A text file's bytes ARE its text — read deterministically, no AI.
func TestRunTextFileNoAI(t *testing.T) {
	ai := &stubAI{}
	res, err := Run(context.Background(), "ada", "wiring.txt", "text/plain", []byte("HDMI 1 -> projector"), ai)
	if err != nil || ai.called || res.Source != "text-layer" || res.Text != "HDMI 1 -> projector" {
		t.Fatalf("text file must be read verbatim without AI: err=%v called=%v %+v", err, ai.called, res)
	}
}

// CASE 3 — a read that fails is a NAMED error, not a silent empty extract: no AI configured, and the
// AI returning an error, are both surfaced (the api layer turns these into a retriable failed state).
func TestRunFailures(t *testing.T) {
	// No AI available for an image.
	if _, err := Run(context.Background(), "ada", "p.png", "image/png", []byte{0x89, 'P'}, nil); !errors.Is(err, ErrNoAI) {
		t.Fatalf("image with no AI: err=%v, want ErrNoAI", err)
	}
	// A file too large for one AI request.
	big := bytes.Repeat([]byte{0x89}, MaxAIBytes+1)
	if _, err := Run(context.Background(), "ada", "big.png", "image/png", big, &stubAI{}); !errors.Is(err, ErrTooLargeForAI) {
		t.Fatalf("oversized image: err=%v, want ErrTooLargeForAI", err)
	}
	// The AI itself errors (engine unreachable, broken file).
	boom := errors.New("boom")
	if _, err := Run(context.Background(), "ada", "p.png", "image/png", []byte{0x89, 'P'}, &stubAI{err: boom}); !errors.Is(err, boom) {
		t.Fatalf("AI error must propagate: %v", err)
	}
}

// A scanned PDF with no readable text layer and no image marker still falls back to AI rather than
// storing an empty or garbage extract (the coherence gate rejects a non-text decode).
func TestRunScannedPDFFallsBackToAI(t *testing.T) {
	ai := &stubAI{text: "scanned text", engine: "claude", model: "claude"}
	// A PDF whose only stream is binary noise — no coherent text layer.
	pdf := []byte("%PDF-1.4\n2 0 obj<< /Length 8 >>\nstream\n\x01\x02\x03\x04\x05\x06\x07\x08\nendstream endobj\n%%EOF")
	res, err := Run(context.Background(), "ada", "scan.pdf", "application/pdf", pdf, ai)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !ai.called || res.Source != "ai" {
		t.Fatalf("a scan with no text layer must fall back to AI: called=%v src=%q", ai.called, res.Source)
	}
}
