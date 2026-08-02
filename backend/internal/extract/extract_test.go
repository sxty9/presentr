package extract

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
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

// A PDF that embeds a raster image is routed to the AI even though it also has a text layer, because
// text may live inside the image (a photographed nameplate) which only vision reads.
func TestRunImageBearingPDFUsesAI(t *testing.T) {
	ai := &stubAI{text: "read by vision", engine: "claude", model: "claude"}
	// A text-layer PDF, with an image XObject marker added so pdfHasImages routes it to AI.
	pdf := append(textPDF(t, "some text"), []byte("\n9 0 obj<< /Subtype /Image /Width 10 >>stream\n\x00\x01endstream endobj")...)
	res, err := Run(context.Background(), "ada", "mixed.pdf", "application/pdf", pdf, ai)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !ai.called {
		t.Fatal("an image-bearing PDF must be routed to the AI (vision reads text inside images)")
	}
	if res.Source != "ai" {
		t.Fatalf("source = %q, want ai", res.Source)
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
