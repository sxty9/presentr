package extract

// The lexical toolkit the page splitter is built on: locating indirect objects in a PDF, breaking a
// dictionary into tokens, reading a dictionary value by key, collecting the indirect references inside a
// value, and rewriting those references to new numbers. It is deliberately small and forgiving — a full
// PDF parser is not needed to lift out whole pages, only enough structure to find them, follow what they
// reference, and copy the bytes across unchanged. Anything malformed makes the caller fall back to the
// named "too large" failure rather than emit a torn section.

import (
	"bytes"
	"strconv"
)

// scanObjects locates every indirect object (`N G obj … endobj`) by a linear scan, resolving each
// stream object's exact byte length from its /Length (direct or indirect) so binary streams — images,
// fonts — are carried across byte-for-byte. Objects living inside compressed object streams are added
// later by parsePDF (decodeObjStm); this pass sees only the top-level objects.
func scanObjects(data []byte) map[int]*pdfObj {
	type raw struct {
		num                    int
		dict                   []byte
		hasStream              bool
		streamStart, streamEnd int // streamEnd is the position of the "endstream" keyword
	}
	var raws []raw
	dictByNum := map[int][]byte{}

	i := 0
	for {
		rel := bytes.Index(data[i:], []byte("obj"))
		if rel < 0 {
			break
		}
		pos := i + rel
		// "obj" must be a real object header (preceded by "num gen "), never the tail of "endobj".
		if pos == 0 || !isSpace(data[pos-1]) {
			i = pos + 3
			continue
		}
		num, ok := readBackwardObjHeader(data, pos)
		if !ok {
			i = pos + 3
			continue
		}
		bodyStart := pos + 3
		endobjIdx := bytes.Index(data[bodyStart:], []byte("endobj"))
		if endobjIdx < 0 {
			break
		}
		endobjIdx += bodyStart
		streamIdx := indexStreamKeyword(data, bodyStart, endobjIdx)
		if streamIdx >= 0 {
			dict := data[bodyStart:streamIdx]
			b := streamIdx + len("stream")
			if b < len(data) && data[b] == '\r' {
				b++
			}
			if b < len(data) && data[b] == '\n' {
				b++
			}
			esRel := bytes.Index(data[b:], []byte("endstream"))
			if esRel < 0 {
				break
			}
			es := b + esRel
			raws = append(raws, raw{num: num, dict: dict, hasStream: true, streamStart: b, streamEnd: es})
			dictByNum[num] = dict
			// Advance past the whole object, so "obj"/"endobj" bytes INSIDE the stream are never mistaken
			// for a new object.
			realEnd := bytes.Index(data[es:], []byte("endobj"))
			if realEnd < 0 {
				break
			}
			i = es + realEnd + len("endobj")
		} else {
			dict := data[bodyStart:endobjIdx]
			raws = append(raws, raw{num: num, dict: dict})
			dictByNum[num] = dict
			i = endobjIdx + len("endobj")
		}
	}

	objs := make(map[int]*pdfObj, len(raws))
	for _, r := range raws {
		o := &pdfObj{num: r.num, dict: r.dict}
		if r.hasStream {
			o.hasStream = true
			o.stream = resolveStreamBytes(data, r.streamStart, r.streamEnd, r.dict, dictByNum)
		}
		objs[r.num] = o // a later duplicate object number wins, matching how a PDF reader honours the newest
	}
	return objs
}

// resolveStreamBytes returns a stream's exact bytes. It trusts /Length when it names a plausible length
// (direct integer, or an indirect reference to an integer object) and otherwise falls back to the region
// up to "endstream" minus the single trailing EOL the PDF spec inserts — so a stream is copied whole
// even when its length is unreadable.
func resolveStreamBytes(data []byte, start, end int, dict []byte, dictByNum map[int][]byte) []byte {
	if n, ok := resolveLength(dict, dictByNum); ok && start+n <= end {
		return data[start : start+n]
	}
	body := data[start:end]
	// Strip the one EOL that precedes "endstream" (\n or \r\n), never more, so real trailing data bytes survive.
	if len(body) > 0 && body[len(body)-1] == '\n' {
		body = body[:len(body)-1]
		if len(body) > 0 && body[len(body)-1] == '\r' {
			body = body[:len(body)-1]
		}
	}
	return body
}

// resolveLength reads /Length: a direct integer, or an indirect reference to an integer object.
func resolveLength(dict []byte, dictByNum map[int][]byte) (int, bool) {
	v := dictGet(dict, "/Length")
	if v == nil {
		return 0, false
	}
	toks := tokenizeDict(v)
	if len(toks) == 1 && isInt(toks[0]) {
		return atoi(toks[0]), true
	}
	if num, ok := firstRef(v); ok {
		if ld, ok := dictByNum[num]; ok {
			lt := tokenizeDict(ld)
			if len(lt) >= 1 && isInt(lt[0]) {
				return atoi(lt[0]), true
			}
		}
	}
	return 0, false
}

// readBackwardObjHeader reads the "num gen" that must precede an "obj" keyword at objPos, returning the
// object number. ok is false when the two integers are not there (so the "obj" was a false match).
func readBackwardObjHeader(data []byte, objPos int) (int, bool) {
	j := objPos - 1
	for j >= 0 && isSpace(data[j]) {
		j--
	}
	genEnd := j + 1
	for j >= 0 && isDigit(data[j]) {
		j--
	}
	if j+1 == genEnd {
		return 0, false
	}
	for j >= 0 && isSpace(data[j]) {
		j--
	}
	numEnd := j + 1
	for j >= 0 && isDigit(data[j]) {
		j--
	}
	numStart := j + 1
	if numStart == numEnd {
		return 0, false
	}
	return atoi(data[numStart:numEnd]), true
}

// indexStreamKeyword finds the first real "stream" keyword in [from,limit), i.e. the one that opens the
// object's stream, never the "stream" tail of "endstream".
func indexStreamKeyword(data []byte, from, limit int) int {
	i := from
	for i < limit {
		rel := bytes.Index(data[i:limit], []byte("stream"))
		if rel < 0 {
			return -1
		}
		p := i + rel
		if p >= 3 && bytes.Equal(data[p-3:p], []byte("end")) {
			i = p + 6
			continue
		}
		return p
	}
	return -1
}

// decodeObjStm pulls the objects out of a compressed object stream (/Type /ObjStm), returning each by
// number. The stream begins with N pairs of integers — object number and its byte offset — followed by
// the object bodies at /First + offset. Only the object dictionaries live here (an ObjStm never holds
// another stream), which is exactly what the page walk needs.
func decodeObjStm(o *pdfObj) map[int][]byte {
	out := map[int][]byte{}
	dec, ok := pdfDecodeStream(pdfStream{dict: o.dict, body: o.stream})
	if !ok {
		return out
	}
	n, ok1 := intValue(o.dict, "/N")
	first, ok2 := intValue(o.dict, "/First")
	if !ok1 || !ok2 || n <= 0 || first < 0 || first > len(dec) {
		return out
	}
	header := readInts(dec[:first], 2*n)
	if len(header) < 2*n {
		return out
	}
	for k := 0; k < n; k++ {
		objNum := header[2*k]
		off := header[2*k+1]
		start := first + off
		if start < 0 || start > len(dec) {
			continue
		}
		end := len(dec)
		if k+1 < n {
			e := first + header[2*(k+1)+1]
			if e >= start && e <= len(dec) {
				end = e
			}
		}
		out[objNum] = bytes.TrimSpace(dec[start:end])
	}
	return out
}

// ── dictionary tokenizing & value access ─────────────────────────────────────────────────────────

// tokenizeDict breaks a dictionary/value region into PDF tokens: the delimiters << >> [ ], literal ( )
// and hex < > strings (kept raw), /names, and runs of regular characters (numbers, keywords like R).
// Whitespace and comments are dropped. The tokens re-serialize (space-joined) to an equivalent value —
// dictionaries are whitespace-insensitive — which is what lets the splitter renumber references safely.
func tokenizeDict(b []byte) [][]byte {
	var toks [][]byte
	i, n := 0, len(b)
	for i < n {
		c := b[i]
		switch {
		case isSpace(c):
			i++
		case c == '%':
			for i < n && b[i] != '\n' && b[i] != '\r' {
				i++
			}
		case c == '(':
			s, ni := readRawLiteral(b, i)
			toks = append(toks, s)
			i = ni
		case c == '<':
			if i+1 < n && b[i+1] == '<' {
				toks = append(toks, []byte("<<"))
				i += 2
			} else {
				s, ni := readRawHex(b, i)
				toks = append(toks, s)
				i = ni
			}
		case c == '>':
			if i+1 < n && b[i+1] == '>' {
				toks = append(toks, []byte(">>"))
				i += 2
			} else {
				i++
			}
		case c == '[':
			toks = append(toks, []byte("["))
			i++
		case c == ']':
			toks = append(toks, []byte("]"))
			i++
		case c == '{' || c == '}':
			toks = append(toks, []byte{c})
			i++
		case c == '/':
			s, ni := readName(b, i)
			toks = append(toks, s)
			i = ni
		default:
			s, ni := readRegular(b, i)
			if len(s) == 0 {
				i++
			} else {
				toks = append(toks, s)
				i = ni
			}
		}
	}
	return toks
}

// dictGet returns the raw value bytes for a top-level key in a dictionary region (which includes its
// surrounding << >>), or nil when the key is absent. The value is re-serialized from its tokens, so a
// reference comes back as "N G R", an array as "[ … ]" and a nested dict as "<< … >>".
func dictGet(dict []byte, key string) []byte {
	toks := tokenizeDict(dict)
	depth := 0
	for i := 0; i < len(toks); i++ {
		switch string(toks[i]) {
		case "<<", "[":
			depth++
			continue
		case ">>", "]":
			depth--
			continue
		}
		if depth == 1 && string(toks[i]) == key {
			val, _ := readValueTokens(toks, i+1)
			return bytes.Join(val, []byte(" "))
		}
	}
	return nil
}

// readValueTokens reads exactly one value starting at index i: a balanced << >> dict, a balanced [ ]
// array, a "N G R" reference, or a single token (name, string, number, keyword). It returns the value's
// tokens and the index just after them.
func readValueTokens(toks [][]byte, i int) ([][]byte, int) {
	if i >= len(toks) {
		return nil, i
	}
	switch string(toks[i]) {
	case "<<", "[":
		open := string(toks[i])
		close := ">>"
		if open == "[" {
			close = "]"
		}
		depth := 0
		start := i
		for i < len(toks) {
			s := string(toks[i])
			if s == open {
				depth++
			} else if s == close {
				depth--
				if depth == 0 {
					return toks[start : i+1], i + 1
				}
			} else if s == "<<" || s == "[" {
				depth++ // a differently-typed opener nested inside
			} else if s == ">>" || s == "]" {
				depth--
			}
			i++
		}
		return toks[start:], len(toks)
	}
	// A reference "N G R" is three tokens; otherwise a single token.
	if i+2 < len(toks) && isInt(toks[i]) && isInt(toks[i+1]) && string(toks[i+2]) == "R" {
		return toks[i : i+3], i + 3
	}
	return toks[i : i+1], i + 1
}

// collectRefs returns every indirect object number referenced (as "N G R") anywhere in the tokens.
func collectRefs(toks [][]byte) []int {
	var refs []int
	for i := 0; i+2 < len(toks); i++ {
		if isInt(toks[i]) && isInt(toks[i+1]) && string(toks[i+2]) == "R" {
			refs = append(refs, atoi(toks[i]))
		}
	}
	return refs
}

// renumberTokens re-serializes tokens, replacing each "N G R" reference with "map(N) 0 R" so the value
// is valid in the rebuilt object graph. Non-reference tokens are emitted unchanged.
func renumberTokens(toks [][]byte, mapRef func(int) int) []byte {
	var buf bytes.Buffer
	write := func(b []byte) {
		if buf.Len() > 0 {
			buf.WriteByte(' ')
		}
		buf.Write(b)
	}
	for i := 0; i < len(toks); {
		if i+2 < len(toks) && isInt(toks[i]) && isInt(toks[i+1]) && string(toks[i+2]) == "R" {
			write([]byte(strconv.Itoa(mapRef(atoi(toks[i]))) + " 0 R"))
			i += 3
			continue
		}
		write(toks[i])
		i++
	}
	return buf.Bytes()
}

// firstRef returns the object number of a value that IS a single "N G R" reference.
func firstRef(val []byte) (int, bool) {
	toks := tokenizeDict(val)
	if len(toks) >= 3 && isInt(toks[0]) && isInt(toks[1]) && string(toks[2]) == "R" {
		return atoi(toks[0]), true
	}
	return 0, false
}

// arrayRefs returns every "N G R" reference inside an array value (used for /Kids).
func arrayRefs(val []byte) []int {
	return collectRefs(tokenizeDict(val))
}

// dictHasNameValue reports whether a dictionary maps key to the given name value (e.g. /Type → /Page).
func dictHasNameValue(dict []byte, key, name string) bool {
	v := dictGet(dict, key)
	return v != nil && string(bytes.TrimSpace(v)) == name
}

// intValue reads an integer-valued key from a dictionary.
func intValue(dict []byte, key string) (int, bool) {
	v := dictGet(dict, key)
	if v == nil {
		return 0, false
	}
	toks := tokenizeDict(v)
	if len(toks) >= 1 && isInt(toks[0]) {
		return atoi(toks[0]), true
	}
	return 0, false
}

// ── token readers ────────────────────────────────────────────────────────────────────────────────

// readRawLiteral returns a ( ) string as raw bytes (parens included), honouring backslash escapes and
// balanced parentheses so a ')' inside the string does not end it early.
func readRawLiteral(b []byte, i int) ([]byte, int) {
	start := i
	i++ // '('
	depth := 1
	for i < len(b) {
		switch b[i] {
		case '\\':
			i += 2
			continue
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				i++
				return b[start:i], i
			}
		}
		i++
	}
	return b[start:i], i
}

// readRawHex returns a < > hex string as raw bytes (angle brackets included).
func readRawHex(b []byte, i int) ([]byte, int) {
	start := i
	i++ // '<'
	for i < len(b) && b[i] != '>' {
		i++
	}
	if i < len(b) {
		i++ // '>'
	}
	return b[start:i], i
}

// readName returns a /Name token (slash plus the following regular characters).
func readName(b []byte, i int) ([]byte, int) {
	start := i
	i++ // '/'
	for i < len(b) && isRegular(b[i]) {
		i++
	}
	return b[start:i], i
}

// readRegular returns a run of regular characters (a number, or a keyword like R/true/null).
func readRegular(b []byte, i int) ([]byte, int) {
	start := i
	for i < len(b) && isRegular(b[i]) {
		i++
	}
	return b[start:i], i
}

// ── byte classes & small parsers ─────────────────────────────────────────────────────────────────

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' || c == 0
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// isRegular reports whether c is a regular character — neither whitespace nor a PDF delimiter.
func isRegular(c byte) bool {
	if isSpace(c) {
		return false
	}
	switch c {
	case '(', ')', '<', '>', '[', ']', '{', '}', '/', '%':
		return false
	}
	return true
}

// isInt reports whether a token is a plain non-negative integer (an object number or generation).
func isInt(tok []byte) bool {
	if len(tok) == 0 {
		return false
	}
	for _, c := range tok {
		if !isDigit(c) {
			return false
		}
	}
	return true
}

func atoi(b []byte) int {
	n := 0
	for _, c := range b {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// readInts parses up to max whitespace-separated non-negative integers from b (the ObjStm header).
func readInts(b []byte, max int) []int {
	var out []int
	i := 0
	for i < len(b) && len(out) < max {
		for i < len(b) && !isDigit(b[i]) {
			i++
		}
		if i >= len(b) {
			break
		}
		j := i
		for j < len(b) && isDigit(b[j]) {
			j++
		}
		out = append(out, atoi(b[i:j]))
		i = j
	}
	return out
}
