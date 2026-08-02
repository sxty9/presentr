package extract

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// multiPagePDF builds a classic (uncompressed, xref-table) PDF with n pages, each showing a unique
// marker string via a text-show operator, plus a shared /Resources and per-page padding so a page can be
// made to weigh a chosen number of bytes. It mirrors what a simple PDF producer emits, so the splitter
// is exercised on realistic structure (a page tree, inherited resources, indirect content streams).
func multiPagePDF(t *testing.T, n int, padPerPage int) []byte {
	t.Helper()
	type obj struct{ body []byte }
	var objs [][]byte
	add := func(format string, a ...any) int {
		objs = append(objs, []byte(fmt.Sprintf(format, a...)))
		return len(objs) // 1-based object number
	}
	addStream := func(dict, stream string) int {
		var b bytes.Buffer
		b.WriteString(dict[:len(dict)-2]) // drop trailing ">>"
		b.WriteString(" /Length ")
		b.WriteString(fmt.Sprintf("%d", len(stream)))
		b.WriteString(" >>\nstream\n")
		b.WriteString(stream)
		b.WriteString("\nendstream")
		objs = append(objs, b.Bytes())
		return len(objs)
	}

	// Reserve numbers: 1 catalog, 2 pages root, 3 shared font resource. Pages and contents follow.
	_ = add("<< /Type /Catalog /Pages 2 0 R >>")                             // 1
	pagesRootIdx := add("")                                                  // 2 (filled below)
	fontIdx := add("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>") // 3

	var pageNums []int
	for i := 0; i < n; i++ {
		marker := fmt.Sprintf("PAGEMARK_%03d", i)
		content := "BT /F1 12 Tf 72 720 Td (" + marker + ") Tj ET"
		if padPerPage > 0 {
			// Padding as a PDF comment inside the content stream keeps the page valid while inflating it.
			content += "\n% " + strings.Repeat("x", padPerPage)
		}
		cIdx := addStream("<< >>", content)
		pIdx := add("<< /Type /Page /Parent 2 0 R /Resources << /Font << /F1 %d 0 R >> >> /MediaBox [0 0 612 792] /Contents %d 0 R >>", fontIdx, cIdx)
		pageNums = append(pageNums, pIdx)
	}

	// Fill the pages root now that page numbers are known.
	var kids strings.Builder
	for i, pn := range pageNums {
		if i > 0 {
			kids.WriteByte(' ')
		}
		fmt.Fprintf(&kids, "%d 0 R", pn)
	}
	objs[pagesRootIdx-1] = []byte(fmt.Sprintf("<< /Type /Pages /Kids [ %s ] /Count %d >>", kids.String(), n))

	// Serialize with a classic xref table.
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs)+1)
	for i, body := range objs {
		num := i + 1
		offsets[num] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n", num)
		buf.Write(body)
		buf.WriteString("\nendobj\n")
	}
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for num := 1; num <= len(objs); num++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[num])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF", len(objs)+1, xref)
	return buf.Bytes()
}

// The parser must find every page in reading order.
func TestParsePDFEnumeratesPages(t *testing.T) {
	doc, ok := parsePDF(multiPagePDF(t, 5, 0))
	if !ok {
		t.Fatal("parsePDF failed on a well-formed 5-page PDF")
	}
	if len(doc.pages) != 5 {
		t.Fatalf("found %d pages, want 5", len(doc.pages))
	}
}

// A large PDF is split into consecutive page sections, each a valid sub-PDF under the budget, with every
// page present exactly once — no page lost, none read twice.
func TestSplitPDFByPagesCoversEveryPageOnce(t *testing.T) {
	// 10 pages, each padded to ~2 KB; a 3 KB budget forces roughly one page per section.
	pdf := multiPagePDF(t, 10, 2000)
	budget := 3000
	secs, ok := splitPDFByPages(pdf, budget)
	if !ok {
		t.Fatal("split failed")
	}
	if len(secs) < 2 {
		t.Fatalf("a large PDF should split into several sections, got %d", len(secs))
	}
	// Each section is a parseable sub-PDF within budget, and its pages carry the right markers.
	seen := map[string]int{}
	for _, s := range secs {
		if s.TooBig {
			t.Fatalf("no section should be too big at this budget: %s", s.Label)
		}
		if len(s.Data) > budget {
			t.Fatalf("section %s is %d bytes, over budget %d", s.Label, len(s.Data), budget)
		}
		if _, ok := parsePDF(s.Data); !ok {
			t.Fatalf("section %s is not a parseable PDF", s.Label)
		}
	}
	// Re-read the whole marker set from every section's raw bytes to prove coverage.
	for i := 0; i < 10; i++ {
		marker := fmt.Sprintf("PAGEMARK_%03d", i)
		for _, s := range secs {
			if bytes.Contains(s.Data, []byte(marker)) {
				seen[marker]++
			}
		}
	}
	for i := 0; i < 10; i++ {
		marker := fmt.Sprintf("PAGEMARK_%03d", i)
		if seen[marker] != 1 {
			t.Fatalf("marker %s appears in %d sections, want exactly 1", marker, seen[marker])
		}
	}
}

// A text-layer PDF is read from its text layer regardless of whether it also embeds an image — the text
// track never depends on the image track. A multi-page text PDF with a bare (unreferenced) image marker
// has no decodable image, so it is a pure local read; its text is present and it needs no AI sections.
func TestPreparePureTextLayerNeedsNoAI(t *testing.T) {
	plan, err := Prepare("application/pdf", multiPagePDF(t, 2, 0), 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	if plan.LocalText == "" || plan.Source != "text-layer" {
		t.Fatalf("a text-layer PDF must be read locally: source=%q text=%q", plan.Source, plan.LocalText)
	}
	if len(plan.Sections) != 0 {
		t.Fatalf("a text PDF with no decodable image needs no AI sections, got %d", len(plan.Sections))
	}
}

// A large PDF with NO readable text layer (a scan this reader cannot open) still falls back to the AI
// whole-document / page-split path, so nothing is lost. splitPDFByPages itself is covered directly by
// TestSplitPDFByPagesCoversEveryPageOnce; here the fallback ROUTING is checked: an incoherent-text,
// image-free PDF over budget produces AI sections rather than a local read.
func TestPrepareScanFallsBackToPageSplit(t *testing.T) {
	// A multi-page PDF whose content streams are binary noise (no coherent text layer) and whose pages
	// reference no image, padded past a tiny budget so the fallback must split it.
	scan := noTextMultiPagePDF(t, 6, 2000)
	plan, err := Prepare("application/pdf", scan, 3000)
	if err != nil {
		t.Fatal(err)
	}
	if plan.LocalText != "" {
		t.Fatalf("a scan with no coherent text layer must not be read locally: %q", plan.LocalText)
	}
	if len(plan.Sections) < 2 {
		t.Fatalf("an over-budget scan must fall back to several page sections, got %d", len(plan.Sections))
	}
}

// noTextMultiPagePDF builds an n-page PDF whose content streams are binary noise (no text-show operators),
// so the text-layer reader rejects it as incoherent and the file routes to the AI fallback. Per-page
// padding lets it be pushed over a small budget so the fallback must split it.
func noTextMultiPagePDF(t *testing.T, n, pad int) []byte {
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
	var pages []int
	for i := 0; i < n; i++ {
		// Binary noise plus padding — no BT/Tj, so pdfTextLayer finds nothing coherent.
		noise := strings.Repeat("\x01\x02\x03\x04", 8+pad/4)
		cIdx := addStream("<< >>", noise)
		pIdx := add(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R >>", cIdx))
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
