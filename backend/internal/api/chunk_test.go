package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"presentr/internal/aigentic"
	"presentr/internal/extract"
	"presentr/internal/store"
)

// fakeAI is an in-process extract.AIExtractor: it records every section read and returns a scripted
// transcription (or a scripted error on a chosen call), so the api layer's section loop — assembly,
// progress, resume — is tested without a live AI or the PDF splitter.
type fakeAI struct {
	mu    sync.Mutex
	calls int
	seen  []string // the filename passed for each call (carries the section label)
	reply func(call int) (string, error)
}

func (f *fakeAI) Extract(_ context.Context, _, filename, _ string, _ []byte) (string, string, string, error) {
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.seen = append(f.seen, filename)
	f.mu.Unlock()
	text, err := f.reply(call)
	if err != nil {
		return "", "", "", err
	}
	return text, "claude", "claude-sonnet-4-6", nil
}

func fileDoc(t *testing.T, s *Server, id string) {
	t.Helper()
	if _, err := s.docs.AddFile(store.Document{ID: id, Title: "manual.pdf", Kind: "file", Mime: "application/pdf", Author: "ada", ExtractState: "pending"}, bytes.NewReader([]byte("%PDF-1.7")), 100<<20); err != nil {
		t.Fatal(err)
	}
}

func sections(n int) []extract.Section {
	out := make([]extract.Section, n)
	for i := range out {
		out[i] = extract.Section{Label: fmt.Sprintf("page %d", i+1), Mime: "application/pdf", Data: []byte{0x25, 0x50}}
	}
	return out
}

// A large file read in several sections is reassembled into ONE document with ONE text containing every
// section, labelled as an AI read — the split is invisible at the surface (the pool still holds one item).
func TestChunkedReadAssemblesEverySection(t *testing.T) {
	s := newServer(t)
	fileDoc(t, s, "big")
	ai := &fakeAI{reply: func(call int) (string, error) { return fmt.Sprintf("TEXT_OF_SECTION_%d", call), nil }}

	s.readSections("big", "ada", "manual.pdf", sections(3), ai)

	// The surface shows exactly one document, not three fragments.
	if got := len(s.docs.List()); got != 1 {
		t.Fatalf("the split must stay invisible: %d documents, want 1", got)
	}
	d, _ := s.docs.Get("big")
	if d.ExtractState != "ready" || d.ExtractSource != "ai" {
		t.Fatalf("read = state %q source %q, want ready/ai", d.ExtractState, d.ExtractSource)
	}
	if d.ExtractSectionsTotal != 0 || d.ExtractSectionsDone != 0 {
		t.Fatalf("a finished read clears its section counters, got %d/%d", d.ExtractSectionsDone, d.ExtractSectionsTotal)
	}
	text, _ := s.docs.ExtractText("big")
	for i := 1; i <= 3; i++ {
		if !strings.Contains(text, fmt.Sprintf("TEXT_OF_SECTION_%d", i)) {
			t.Fatalf("assembled text is missing section %d: %q", i, text)
		}
	}
	if ai.calls != 3 {
		t.Fatalf("each of the 3 sections must be read once, got %d calls", ai.calls)
	}
	// The resume job is cleared once the read completes.
	if _, ok := s.docs.ExtractJob("big"); ok {
		t.Fatal("a completed read must leave no resume job behind")
	}
}

// A read interrupted mid-way (the engine drops out) leaves a NAMED, resumable failed state with its
// progress; a later run CONTINUES from where it stopped rather than re-reading the sections already read.
func TestChunkedReadResumesAfterInterruption(t *testing.T) {
	s := newServer(t)
	fileDoc(t, s, "big")

	// First run: section 1 succeeds, section 2 fails with an engine-unavailable error → interruption.
	first := &fakeAI{reply: func(call int) (string, error) {
		if call == 2 {
			return "", aigentic.ErrUnavailable
		}
		return fmt.Sprintf("SECTION_%d", call), nil
	}}
	s.readSections("big", "ada", "manual.pdf", sections(4), first)

	d, _ := s.docs.Get("big")
	if d.ExtractState != "failed" || d.ExtractError == "" {
		t.Fatalf("an interrupted read must be a named failed state, got %+v", d)
	}
	if d.ExtractSectionsDone != 1 || d.ExtractSectionsTotal != 4 {
		t.Fatalf("progress after interruption = %d/%d, want 1/4", d.ExtractSectionsDone, d.ExtractSectionsTotal)
	}
	if _, ok := s.docs.ExtractJob("big"); !ok {
		t.Fatal("an interrupted read must persist a resume job")
	}
	if first.calls != 2 {
		t.Fatalf("the first run should stop at the failing section, got %d calls", first.calls)
	}

	// Second run (a retry): the engine is back. It must resume — only sections 2..4 are read.
	second := &fakeAI{reply: func(call int) (string, error) { return fmt.Sprintf("RESUMED_%d", call), nil }}
	s.readSections("big", "ada", "manual.pdf", sections(4), second)

	d, _ = s.docs.Get("big")
	if d.ExtractState != "ready" {
		t.Fatalf("after resume the read must complete, got %q", d.ExtractState)
	}
	if second.calls != 3 {
		t.Fatalf("resume must read only the 3 remaining sections, got %d calls (not from the start)", second.calls)
	}
	text, _ := s.docs.ExtractText("big")
	if !strings.Contains(text, "SECTION_1") {
		t.Fatalf("the section read before the interruption must survive the resume: %q", text)
	}
	for _, want := range []string{"RESUMED_1", "RESUMED_2", "RESUMED_3"} {
		if !strings.Contains(text, want) {
			t.Fatalf("resumed sections missing from assembled text: %q", text)
		}
	}
}

// End to end: an over-budget scanned-style PDF uploaded to the pool is planned, split into page sections,
// read one at a time through aigentic, and assembled — leaving ONE ready document. This exercises
// Prepare + splitPDFByPages + the section loop together.
func TestUploadOfLargePDFReadsInSectionsEndToEnd(t *testing.T) {
	var calls int
	var mu sync.Mutex
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
		fmt.Fprintf(w, `{"data":{"output":"page-text-%d","engine":"claude","model":"claude"}}`, n)
	}))
	defer stub.Close()

	s := newServer(t)
	s.ai = aigentic.New(stub.URL, "sekret")
	s.aiLimit.Store(4000) // a tiny per-request ceiling forces the PDF to split into several sections

	pdf := imageBearingMultiPagePDF(t, 6, 900)
	if _, err := s.docs.AddFile(store.Document{ID: "scan", Title: "big-scan.pdf", Kind: "file", Mime: "application/pdf", Author: "ada", ExtractState: "pending"}, bytes.NewReader(pdf), 100<<20); err != nil {
		t.Fatal(err)
	}
	s.runExtraction("scan")

	if got := len(s.docs.List()); got != 1 {
		t.Fatalf("the pool must hold one document, got %d", got)
	}
	d, _ := s.docs.Get("scan")
	if d.ExtractState != "ready" || d.ExtractSource != "ai" {
		t.Fatalf("end-to-end read = %+v, want ready/ai", d)
	}
	if calls < 2 {
		t.Fatalf("a large PDF over a tiny budget must be read in several requests, got %d", calls)
	}
	text, _ := s.docs.ExtractText("scan")
	if !strings.Contains(text, "page-text-1") {
		t.Fatalf("assembled text missing the first section: %q", text)
	}
}

// imageBearingMultiPagePDF builds a classic multi-page PDF with an image XObject (so it routes to the AI
// path) and per-page padding (so it can be pushed over a small budget and forced to split).
func imageBearingMultiPagePDF(t *testing.T, n, pad int) []byte {
	t.Helper()
	var objs [][]byte
	add := func(s string) int { objs = append(objs, []byte(s)); return len(objs) }
	addStream := func(dict, stream string) int {
		var b bytes.Buffer
		b.WriteString(dict[:len(dict)-2])
		fmt.Fprintf(&b, " /Length %d >>\nstream\n%s\nendstream", len(stream), stream)
		objs = append(objs, b.Bytes())
		return len(objs)
	}
	_ = add("<< /Type /Catalog /Pages 2 0 R >>")
	pagesIdx := add("")
	fontIdx := add("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	imgIdx := addStream("<< /Type /XObject /Subtype /Image /Width 1 /Height 1 /ColorSpace /DeviceGray /BitsPerComponent 8 >>", "\x00")
	var pages []int
	for i := 0; i < n; i++ {
		content := fmt.Sprintf("BT /F1 12 Tf 72 720 Td (PAGE_%d) Tj ET\n%% %s", i, strings.Repeat("x", pad))
		cIdx := addStream("<< >>", content)
		pIdx := add(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /Resources << /Font << /F1 %d 0 R >> /XObject << /Im0 %d 0 R >> >> /MediaBox [0 0 612 792] /Contents %d 0 R >>", fontIdx, imgIdx, cIdx))
		pages = append(pages, pIdx)
	}
	var kids strings.Builder
	for i, p := range pages {
		if i > 0 {
			kids.WriteByte(' ')
		}
		fmt.Fprintf(&kids, "%d 0 R", p)
	}
	objs[pagesIdx-1] = []byte(fmt.Sprintf("<< /Type /Pages /Kids [ %s ] /Count %d >>", kids.String(), n))

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offs := make([]int, len(objs)+1)
	for i, body := range objs {
		offs[i+1] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}
	x := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for i := 1; i <= len(objs); i++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offs[i])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF", len(objs)+1, x)
	return buf.Bytes()
}
