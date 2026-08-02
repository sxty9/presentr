package extract

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"testing"
)

// objStmPDF builds a PDF whose page tree (the pages root and the page objects) lives inside a COMPRESSED
// object stream (/Type /ObjStm) — the shape modern producers and many scanners emit (PDF 1.5+). It
// proves the splitter reaches objects that a raw `N G obj` scan alone would miss, which the real
// operational 63 MiB scan may well need.
func objStmPDF(t *testing.T, n int) []byte {
	t.Helper()
	// Bodies that will live INSIDE the object stream: the pages root (obj 2) and n page objects (obj 4..).
	var bodies []struct {
		num  int
		body string
	}
	var kids bytes.Buffer
	for i := 0; i < n; i++ {
		num := 4 + i
		if i > 0 {
			kids.WriteByte(' ')
		}
		fmt.Fprintf(&kids, "%d 0 R", num)
	}
	bodies = append(bodies, struct {
		num  int
		body string
	}{2, fmt.Sprintf("<< /Type /Pages /Kids [ %s ] /Count %d >>", kids.String(), n)})
	for i := 0; i < n; i++ {
		num := 4 + i
		content := 100 + i // normal content-stream object numbers
		bodies = append(bodies, struct {
			num  int
			body string
		}{num, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /Resources << /Font << /F1 3 0 R >> >> /MediaBox [0 0 612 792] /Contents %d 0 R >>", content)})
	}

	// The ObjStm header: pairs of (object-number, offset-within-the-bodies-section).
	var header bytes.Buffer
	var bodyBuf bytes.Buffer
	for _, b := range bodies {
		fmt.Fprintf(&header, "%d %d ", b.num, bodyBuf.Len())
		bodyBuf.WriteString(b.body)
	}
	first := header.Len()
	var decoded bytes.Buffer
	decoded.Write(header.Bytes())
	decoded.Write(bodyBuf.Bytes())

	var comp bytes.Buffer
	zw := zlib.NewWriter(&comp)
	_, _ = zw.Write(decoded.Bytes())
	_ = zw.Close()

	// Now the top-level (uncompressed) objects: catalog, font, the ObjStm holder, and the content streams.
	var objs []struct {
		num  int
		body string
	}
	addNormal := func(num int, body string) {
		objs = append(objs, struct {
			num  int
			body string
		}{num, body})
	}
	addNormal(1, "<< /Type /Catalog /Pages 2 0 R >>")
	addNormal(3, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	// obj 8: the object stream carrying objs 2 and 4..(3+n).
	objStm := fmt.Sprintf("<< /Type /ObjStm /N %d /First %d /Filter /FlateDecode /Length %d >>\nstream\n%s\nendstream",
		len(bodies), first, comp.Len(), comp.String())
	objs = append(objs, struct {
		num  int
		body string
	}{8, objStm})
	for i := 0; i < n; i++ {
		content := "BT /F1 12 Tf 72 720 Td (OBJSTM_" + fmt.Sprintf("%d", i) + ") Tj ET"
		body := fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content)
		addNormal(100+i, body)
	}

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.5\n")
	for _, o := range objs {
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", o.num, o.body)
	}
	buf.WriteString("startxref\n0\n%%EOF")
	return buf.Bytes()
}

// The splitter must find and split pages that live inside a compressed object stream.
func TestSplitObjStmPDF(t *testing.T) {
	pdf := objStmPDF(t, 4)
	doc, ok := parsePDF(pdf)
	if !ok {
		t.Fatal("parsePDF failed on an object-stream PDF")
	}
	if len(doc.pages) != 4 {
		t.Fatalf("found %d pages in an ObjStm PDF, want 4", len(doc.pages))
	}
	secs, ok := splitPDFByPages(pdf, 1200)
	if !ok {
		t.Fatal("split failed on an ObjStm PDF")
	}
	// Every page's content marker survives, exactly once across all sections.
	for i := 0; i < 4; i++ {
		marker := fmt.Sprintf("OBJSTM_%d", i)
		count := 0
		for _, s := range secs {
			if bytes.Contains(s.Data, []byte(marker)) {
				count++
			}
			if len(s.Data) > 1200 && !s.TooBig {
				t.Fatalf("section %s over budget", s.Label)
			}
		}
		if count != 1 {
			t.Fatalf("page marker %s appears in %d sections, want 1", marker, count)
		}
	}
}
