package extract

// Splitting one large PDF into page-sized sections, each a SELF-CONTAINED, valid sub-PDF small enough
// for a single AI request. This is the missing building block the package note has always pointed at:
// a 63 MiB scanned manual can be read by the assistant, one page range at a time, and reassembled into
// ONE text. The page is the natural cut of a PDF (a page is what a human reads and what a scan captures
// per sheet), so a section is a whole number of pages — never a torn page and never a page read twice.
//
// The split is byte-preserving: a page's content and image streams are copied VERBATIM into the
// sub-PDF (never re-encoded, downscaled or recompressed — the axiom that the original bytes are kept),
// only the surrounding object graph is rebuilt so the sub-PDF stands on its own. It is a lexical
// rebuild, NOT a full PDF engine: objects are located by scanning for `N G obj`, compressed object
// streams (PDF 1.5+) are decoded so their objects are reachable too, and everything a selected page
// transitively references (its resources, fonts, images, content) is carried along and renumbered.
// Anything this reader cannot parse into a page tree returns ok=false, and the caller falls back to the
// existing named "too large" failure — a section is never emitted torn.

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
)

// maxPagesPerSection caps how many pages one section may hold, independent of the byte budget. It bounds
// the cost of the greedy rebuild on a pathological document of very many tiny pages (where hundreds could
// otherwise fit one request), at no cost to a real scanned manual whose pages are large enough that a
// section is only a page or a few anyway.
const maxPagesPerSection = 100

// splitPDFByPages groups a PDF's pages into consecutive sections, each rebuilt as a standalone sub-PDF
// whose bytes stay at or under budget where possible. Pages are grouped greedily: a section grows until
// adding the next page would push it past the budget. A single page whose own sub-PDF still exceeds the
// budget becomes its own section with TooBig set, so the caller names that one page as unreadable rather
// than dropping it (Kein stummes Ausbleiben). ok is false when the PDF cannot be parsed into pages at
// all, so the caller can fall back cleanly.
func splitPDFByPages(data []byte, budget int) (sections []Section, ok bool) {
	if budget <= 0 {
		return nil, false
	}
	doc, ok := parsePDF(data)
	if !ok || len(doc.pages) == 0 {
		return nil, false
	}

	// Greedily grow a section (a run of consecutive pages) until the rebuilt sub-PDF would exceed the
	// budget. Each candidate is actually BUILT and measured — the true byte size, not an estimate — so a
	// section is never emitted over budget by a bad guess. A single page that alone overruns the budget
	// is emitted as its own TooBig section.
	n := len(doc.pages)
	start := 0
	for start < n {
		end := start // exclusive upper bound of the current run, grown below
		var lastBuilt []byte
		for end < n && (end-start) < maxPagesPerSection {
			built, bok := doc.buildSubPDF(start, end+1)
			if !bok {
				break
			}
			if len(built) > budget {
				break
			}
			lastBuilt = built
			end++
		}
		if end == start {
			// Even a single page (start..start+1) does not fit, or failed to build. Emit that one page as
			// its own section, marked too big if it built but overran, so the caller names it per section.
			built, bok := doc.buildSubPDF(start, start+1)
			if !bok {
				return nil, false // a page we cannot rebuild at all — fall back rather than lose it
			}
			sections = append(sections, Section{
				Label:  pageLabel(start, start+1),
				Mime:   "application/pdf",
				Data:   built,
				TooBig: len(built) > budget,
			})
			start++
			continue
		}
		sections = append(sections, Section{
			Label: pageLabel(start, end),
			Mime:  "application/pdf",
			Data:  lastBuilt,
		})
		start = end
	}
	return sections, true
}

// pageLabel names a section by its 1-based page range, so a per-section gap or the assembled text can
// point at exactly which pages it concerns ("page 7" / "pages 7–9").
func pageLabel(startIdx, endExcl int) string {
	from := startIdx + 1
	to := endExcl
	if from == to {
		return fmt.Sprintf("page %d", from)
	}
	return fmt.Sprintf("pages %d–%d", from, to)
}

// ── the lexical PDF model ────────────────────────────────────────────────────────────────────────

// pdfObj is one indirect object located in the file: its dictionary tokens and (optionally) its raw
// stream bytes, copied verbatim. isPages/isPage/isCatalog cache the /Type so the page walk and the
// closure skip the page tree without re-parsing.
type pdfObj struct {
	num       int
	dict      []byte // the bytes between "obj" and "stream"/"endobj" (the dictionary region)
	stream    []byte // raw stream bytes (unchanged), when hasStream
	hasStream bool
}

// pdfDoc is the parsed file: every object by number, plus the ordered page object numbers with their
// resolved (possibly inherited) /Resources and /MediaBox, so each page can be rebuilt standalone.
type pdfDoc struct {
	objs  map[int]*pdfObj
	pages []pageRef
}

// pageRef is one page in reading order: its object number and the inherited attributes it needs to
// render on its own (a scanner often sets /MediaBox and /Resources on an ancestor, not the page).
type pageRef struct {
	num         int
	mediaBox    []byte // the resolved /MediaBox value tokens, or nil
	resources   []byte // the resolved /Resources value tokens, or nil
	hasMedia    bool
	hasResource bool
}

// parsePDF builds the object map (including objects inside compressed object streams) and walks the
// page tree into an ordered page list. ok is false if no catalog/page tree can be found.
func parsePDF(data []byte) (*pdfDoc, bool) {
	objs := scanObjects(data)
	if len(objs) == 0 {
		return nil, false
	}
	// Pull objects out of any /ObjStm (compressed object streams), so a PDF 1.5+ file whose page/pages
	// dictionaries live compressed is still fully reachable.
	for _, o := range objs {
		if o.hasStream && dictHasNameValue(o.dict, "/Type", "/ObjStm") {
			for num, body := range decodeObjStm(o) {
				if _, exists := objs[num]; !exists {
					objs[num] = &pdfObj{num: num, dict: body}
				}
			}
		}
	}
	// Cache each object's role.
	classify := func(o *pdfObj) (isPages, isPage, isCatalog bool) {
		switch {
		case dictHasNameValue(o.dict, "/Type", "/Catalog"):
			return false, false, true
		case dictHasNameValue(o.dict, "/Type", "/Pages"):
			return true, false, false
		case dictHasNameValue(o.dict, "/Type", "/Page"):
			return false, true, false
		}
		return false, false, false
	}

	doc := &pdfDoc{objs: objs}

	// Find the page tree root: the catalog's /Pages. Fall back to any /Pages node with no /Parent.
	root := -1
	for _, o := range objs {
		if _, _, isCat := classify(o); isCat {
			if r, ok := firstRef(dictGet(o.dict, "/Pages")); ok {
				root = r
			}
		}
	}
	if root < 0 {
		for _, o := range objs {
			if isPages, _, _ := classify(o); isPages {
				if _, ok := firstRef(dictGet(o.dict, "/Parent")); !ok {
					root = o.num
					break
				}
			}
		}
	}
	if root < 0 {
		return nil, false
	}

	// Walk the page tree depth-first, carrying inherited /MediaBox and /Resources down to each page.
	seen := map[int]bool{}
	var walk func(num int, mediaBox, resources []byte) bool
	walk = func(num int, mediaBox, resources []byte) bool {
		if seen[num] {
			return true // a cycle — stop, but do not fail the whole parse
		}
		seen[num] = true
		o := objs[num]
		if o == nil {
			return true
		}
		if mb := dictGet(o.dict, "/MediaBox"); mb != nil {
			mediaBox = mb
		}
		if rs := dictGet(o.dict, "/Resources"); rs != nil {
			resources = rs
		}
		_, isPage, _ := classify(o)
		kids := dictGet(o.dict, "/Kids")
		if isPage || kids == nil {
			doc.pages = append(doc.pages, pageRef{
				num: num, mediaBox: mediaBox, resources: resources,
				hasMedia: mediaBox != nil, hasResource: resources != nil,
			})
			return true
		}
		for _, kid := range arrayRefs(kids) {
			if !walk(kid, mediaBox, resources) {
				return false
			}
		}
		return true
	}
	if !walk(root, nil, nil) {
		return nil, false
	}
	if len(doc.pages) == 0 {
		return nil, false
	}
	return doc, true
}

// ── rebuilding a page-range into a standalone sub-PDF ────────────────────────────────────────────

// buildSubPDF writes a valid PDF containing exactly pages[startIdx:endExcl], carrying every object they
// transitively reference and renumbering the graph so the result stands alone. Stream bytes are copied
// unchanged. ok is false only if a selected page object has vanished from the map.
func (d *pdfDoc) buildSubPDF(startIdx, endExcl int) ([]byte, bool) {
	// The page objects in this range, each with its inherited attributes injected so it renders alone.
	type buildObj struct {
		num    int
		tokens [][]byte // dictionary tokens (already augmented for pages)
		obj    *pdfObj  // for stream bytes; nil for the synthesized catalog/pages root
	}

	pagesNodes := map[int]bool{} // old numbers of /Pages tree nodes → collapse to the new root
	for _, o := range d.objs {
		if dictHasNameValue(o.dict, "/Type", "/Pages") || dictHasNameValue(o.dict, "/Type", "/Catalog") {
			pagesNodes[o.num] = true
		}
	}

	// Augmented page dictionaries (inherited /MediaBox and /Resources injected when the page omits them).
	pageTokens := map[int][][]byte{}
	var pageNums []int
	for _, p := range d.pages[startIdx:endExcl] {
		o := d.objs[p.num]
		if o == nil {
			return nil, false
		}
		toks := tokenizeDict(o.dict)
		if !p.hasMedia && p.mediaBox != nil {
			toks = append(toks, []byte("/MediaBox"))
			toks = append(toks, tokenizeDict(p.mediaBox)...)
		}
		if !p.hasResource && p.resources != nil {
			toks = append(toks, []byte("/Resources"))
			toks = append(toks, tokenizeDict(p.resources)...)
		}
		pageTokens[p.num] = toks
		pageNums = append(pageNums, p.num)
	}

	// Transitive closure: everything the selected pages reach, minus the page-tree nodes (a page's
	// /Parent collapses to the one new root). Content streams, images, fonts and length objects ride along.
	copySet := map[int]bool{}
	var stack []int
	pushRefs := func(toks [][]byte) {
		for _, r := range collectRefs(toks) {
			if pagesNodes[r] || copySet[r] {
				continue
			}
			if _, ok := d.objs[r]; !ok {
				continue
			}
			stack = append(stack, r)
		}
	}
	for _, pn := range pageNums {
		copySet[pn] = true
		pushRefs(pageTokens[pn])
	}
	for len(stack) > 0 {
		num := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if copySet[num] || pagesNodes[num] {
			continue
		}
		o := d.objs[num]
		if o == nil {
			continue
		}
		copySet[num] = true
		pushRefs(tokenizeDict(o.dict))
	}

	// Assign new, contiguous numbers: 1 = catalog, 2 = pages root, then every copied object. A reserved
	// empty object catches any dangling reference so the output never points at a missing number.
	copied := make([]int, 0, len(copySet))
	for num := range copySet {
		copied = append(copied, num)
	}
	sort.Ints(copied)
	newNum := map[int]int{}
	next := 3
	for _, old := range copied {
		newNum[old] = next
		next++
	}
	reservedNull := next // an empty object for dangling refs
	next++
	for old := range pagesNodes {
		newNum[old] = 2 // any surviving /Parent ref lands on the new pages root
	}
	mapRef := func(old int) int {
		if nn, ok := newNum[old]; ok {
			return nn
		}
		return reservedNull
	}

	// Serialize.
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.7\n")
	offsets := make([]int, next) // index by new number; [0] unused
	writeObj := func(nn int, body func()) {
		offsets[nn] = buf.Len()
		buf.WriteString(strconv.Itoa(nn))
		buf.WriteString(" 0 obj\n")
		body()
		buf.WriteString("\nendobj\n")
	}

	// 1: catalog, 2: pages root.
	kids := make([]string, 0, len(pageNums))
	for _, pn := range pageNums {
		kids = append(kids, strconv.Itoa(mapRef(pn))+" 0 R")
	}
	writeObj(1, func() { buf.WriteString("<< /Type /Catalog /Pages 2 0 R >>") })
	writeObj(2, func() {
		buf.WriteString("<< /Type /Pages /Kids [ ")
		buf.WriteString(joinStrings(kids, " "))
		buf.WriteString(" ] /Count ")
		buf.WriteString(strconv.Itoa(len(pageNums)))
		buf.WriteString(" >>")
	})

	// Copied objects, in new-number order, refs renumbered, streams verbatim.
	writeCopied := func(old int) {
		nn := newNum[old]
		o := d.objs[old]
		var toks [][]byte
		if t, ok := pageTokens[old]; ok {
			toks = t
		} else {
			toks = tokenizeDict(o.dict)
		}
		dictBytes := renumberTokens(toks, mapRef)
		writeObj(nn, func() {
			buf.Write(dictBytes)
			if o.hasStream {
				buf.WriteString("\nstream\n")
				buf.Write(o.stream)
				buf.WriteString("\nendstream")
			}
		})
	}
	for _, old := range copied {
		writeCopied(old)
	}
	writeObj(reservedNull, func() { buf.WriteString("<< >>") })

	// Cross-reference table + trailer.
	xrefOff := buf.Len()
	buf.WriteString("xref\n")
	buf.WriteString("0 ")
	buf.WriteString(strconv.Itoa(next))
	buf.WriteString("\n0000000000 65535 f \n")
	for nn := 1; nn < next; nn++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[nn])
	}
	buf.WriteString("trailer\n<< /Size ")
	buf.WriteString(strconv.Itoa(next))
	buf.WriteString(" /Root 1 0 R >>\nstartxref\n")
	buf.WriteString(strconv.Itoa(xrefOff))
	buf.WriteString("\n%%EOF")
	return buf.Bytes(), true
}

func joinStrings(s []string, sep string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += sep
		}
		out += v
	}
	return out
}
