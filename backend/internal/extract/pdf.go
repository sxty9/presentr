package extract

// A small, dependency-free PDF text-layer reader. It exists for ONE narrow job: read the
// machine-readable text of a digitally-generated PDF locally, exactly and with no AI. It deliberately
// does NOT try to be a full PDF engine — anything it cannot read cleanly (a scanned page, a PDF whose
// fonts use a custom/subset encoding) is left to aigentic's extract instead (see pdfPlan), which reads
// text INSIDE images too. The text layer is ALWAYS read when it is coherent, whether or not the PDF also
// embeds images — the images are their own, separate track (pdfimages.go), so a photo never blocks the
// text. The coherence gate (coherent) rejects a garbled decode so a bad read falls back to the AI path.
// It uses only the standard library (compress/zlib, compress/flate).

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"encoding/hex"
	"io"
	"sort"
	"strconv"
	"strings"
)

// maxPDFScan bounds the total decompressed bytes this reader will process, so a pathological PDF can
// never make an upload's background read run unbounded. A PDF whose text does not fit is a large
// document that the sibling chunking order handles; here it simply reads what fits.
const maxPDFScan = 32 << 20

// pdfTextLayer reads the text of a PDF from its content streams and reports whether the result is
// trustworthy (coherent) — a caller uses the text only when ok is true, else it falls back to AI. It
// walks the PDF's indirect objects (scanObjects, which delimits each object's dictionary PRECISELY and
// resolves stream lengths), skips the image XObjects, decompresses each remaining content stream and
// pulls the operands of the text-show operators (Tj, TJ, ', "). Using the exact object dictionary — not a
// fixed window of bytes before the stream — is what lets the text layer be read reliably even in a photo-
// heavy PDF where content streams sit right next to image objects (the two tracks are independent).
func pdfTextLayer(data []byte) (string, bool) {
	objs := scanObjects(data)
	nums := make([]int, 0, len(objs))
	for n := range objs {
		nums = append(nums, n)
	}
	sort.Ints(nums) // object-number order is a stable, reasonable reading order for the assembled text
	var out strings.Builder
	scanned := 0
	for _, n := range nums {
		o := objs[n]
		if !o.hasStream {
			continue
		}
		if dictHasNameValue(o.dict, "/Subtype", "/Image") {
			continue // an image XObject, not a content stream — the image track reads it, not this
		}
		dec, ok := pdfDecodeStream(pdfStream{dict: o.dict, body: o.stream})
		if !ok {
			continue
		}
		if !bytes.Contains(dec, []byte("BT")) && !bytes.Contains(dec, []byte("Tj")) && !bytes.Contains(dec, []byte("TJ")) {
			continue // no text-showing operators — not a content stream worth parsing
		}
		out.WriteString(extractTextOps(dec))
		scanned += len(dec)
		if scanned >= maxPDFScan {
			break
		}
	}
	text := squeezeBlank(out.String())
	return text, coherent(text)
}

// pdfStream is one raw PDF stream: the dictionary bytes (so /Filter and /Subtype can be read) and the
// stream body bytes. It is the small value pdfDecodeStream operates on.
type pdfStream struct {
	dict []byte
	body []byte
}

// pdfDecodeStream returns a stream's bytes ready to read: FlateDecode streams are inflated (zlib, with
// a raw-deflate fallback), an unfiltered stream is returned as-is. A stream using a filter this reader
// does not implement (LZW, DCT/JPEG, …) reports ok=false and is skipped — its text, if any, reaches
// the reader via the AI path instead.
func pdfDecodeStream(s pdfStream) ([]byte, bool) {
	if bytes.Contains(s.dict, []byte("FlateDecode")) {
		if r, err := zlib.NewReader(bytes.NewReader(s.body)); err == nil {
			if dec, err := io.ReadAll(io.LimitReader(r, maxPDFScan)); err == nil && len(dec) > 0 {
				return dec, true
			}
		}
		// Some producers omit the zlib header; try raw deflate.
		fr := flate.NewReader(bytes.NewReader(s.body))
		if dec, err := io.ReadAll(io.LimitReader(fr, maxPDFScan)); err == nil && len(dec) > 0 {
			return dec, true
		}
		return nil, false
	}
	if bytes.Contains(s.dict, []byte("/Filter")) {
		return nil, false // a filter we do not decode
	}
	return s.body, true
}

// extractTextOps pulls the shown text out of a decompressed content stream. It scans operands and
// operators: a literal ( ) or hex < > string is remembered, and a text-show operator (Tj, ', ")
// commits the last string while TJ commits every string in the preceding [ ] array (with a space
// inserted where a large negative kerning number implies a word gap). Td/TD/T* (line moves) emit a
// newline so the text keeps a readable shape.
func extractTextOps(b []byte) string {
	var out strings.Builder
	var arr []string
	inArray := false
	lastStr := ""
	hasLast := false
	i, n := 0, len(b)
	for i < n {
		c := b[i]
		switch {
		case c == '(':
			s, ni := readLiteral(b, i)
			i = ni
			if inArray {
				arr = append(arr, s)
			} else {
				lastStr, hasLast = s, true
			}
		case c == '<':
			if i+1 < n && b[i+1] == '<' {
				i = skipDict(b, i)
				continue
			}
			s, ni := readHexString(b, i)
			i = ni
			if inArray {
				arr = append(arr, s)
			} else {
				lastStr, hasLast = s, true
			}
		case c == '[':
			inArray = true
			arr = arr[:0]
			i++
		case c == ']':
			inArray = false
			i++
		case c == ' ' || c == '\r' || c == '\n' || c == '\t' || c == '\f' || c == 0:
			i++
		default:
			tok, ni := readToken(b, i)
			i = ni
			switch tok {
			case "Tj", "'", "\"":
				if hasLast {
					out.WriteString(lastStr)
					out.WriteByte(' ')
					hasLast = false
				}
			case "TJ":
				for _, s := range arr {
					out.WriteString(s)
				}
				out.WriteByte(' ')
				arr = arr[:0]
			case "Td", "TD", "T*":
				out.WriteByte('\n')
			default:
				// A large negative number inside a TJ array is a word-spacing gap; make it a space.
				if inArray {
					if v, err := strconv.ParseFloat(tok, 64); err == nil && v <= -100 {
						arr = append(arr, " ")
					}
				}
			}
		}
	}
	return out.String()
}

// readLiteral decodes a ( ) string starting at b[i]=='(', handling balanced parens, backslash escapes
// and octal codes. Bytes are mapped to runes as Latin-1 so the result is always valid UTF-8; a PDF
// whose real encoding differs yields text the coherence gate then rejects.
func readLiteral(b []byte, i int) (string, int) {
	i++ // skip '('
	depth := 1
	var sb strings.Builder
	for i < len(b) {
		c := b[i]
		if c == '\\' {
			i++
			if i >= len(b) {
				break
			}
			e := b[i]
			switch e {
			case 'n':
				sb.WriteByte('\n')
			case 'r':
				sb.WriteByte('\r')
			case 't':
				sb.WriteByte('\t')
			case 'b':
				sb.WriteByte('\b')
			case 'f':
				sb.WriteByte('\f')
			case '(', ')', '\\':
				sb.WriteByte(e)
			case '\r':
				if i+1 < len(b) && b[i+1] == '\n' {
					i++
				}
			case '\n':
				// line continuation: emit nothing
			default:
				if e >= '0' && e <= '7' {
					oct := int(e - '0')
					for k := 0; k < 2 && i+1 < len(b) && b[i+1] >= '0' && b[i+1] <= '7'; k++ {
						i++
						oct = oct*8 + int(b[i]-'0')
					}
					emitByte(&sb, byte(oct))
				} else {
					emitByte(&sb, e)
				}
			}
			i++
			continue
		}
		if c == '(' {
			depth++
			emitByte(&sb, c)
			i++
			continue
		}
		if c == ')' {
			depth--
			if depth == 0 {
				i++
				break
			}
			emitByte(&sb, c)
			i++
			continue
		}
		emitByte(&sb, c)
		i++
	}
	return sb.String(), i
}

// readHexString decodes a < > hex string starting at b[i]=='<' into bytes, mapped to runes as Latin-1.
func readHexString(b []byte, i int) (string, int) {
	i++ // skip '<'
	start := i
	for i < len(b) && b[i] != '>' {
		i++
	}
	raw := b[start:i]
	if i < len(b) {
		i++ // skip '>'
	}
	// Strip whitespace, pad an odd length with a trailing 0 (per the PDF spec).
	var hexb strings.Builder
	for _, c := range raw {
		if c == ' ' || c == '\r' || c == '\n' || c == '\t' || c == '\f' {
			continue
		}
		hexb.WriteByte(c)
	}
	hs := hexb.String()
	if len(hs)%2 == 1 {
		hs += "0"
	}
	decoded, err := hex.DecodeString(hs)
	if err != nil {
		return "", i
	}
	var sb strings.Builder
	for _, by := range decoded {
		emitByte(&sb, by)
	}
	return sb.String(), i
}

// readToken reads a run of regular (non-delimiter, non-whitespace) bytes — an operator or a number.
func readToken(b []byte, i int) (string, int) {
	start := i
	for i < len(b) {
		c := b[i]
		if c == ' ' || c == '\r' || c == '\n' || c == '\t' || c == '\f' || c == 0 ||
			c == '(' || c == ')' || c == '<' || c == '>' || c == '[' || c == ']' || c == '{' || c == '}' || c == '/' || c == '%' {
			break
		}
		i++
	}
	if i == start {
		return "", i + 1 // never stall on a stray delimiter
	}
	return string(b[start:i]), i
}

// skipDict advances past a << >> dictionary starting at b[i]=='<' (b[i+1]=='<'), balancing nesting.
func skipDict(b []byte, i int) int {
	depth := 0
	for i < len(b) {
		if i+1 < len(b) && b[i] == '<' && b[i+1] == '<' {
			depth++
			i += 2
			continue
		}
		if i+1 < len(b) && b[i] == '>' && b[i+1] == '>' {
			depth--
			i += 2
			if depth == 0 {
				return i
			}
			continue
		}
		i++
	}
	return i
}

// emitByte appends one PDF text byte to sb as a rune, mapping 0x80–0xFF as Latin-1 so the builder
// always holds valid UTF-8. Control bytes are written verbatim (they make the coherence gate reject a
// stream that decoded to non-text, which is exactly when the file should fall back to AI).
func emitByte(sb *strings.Builder, c byte) {
	if c < 0x80 {
		sb.WriteByte(c)
		return
	}
	sb.WriteRune(rune(c))
}

// squeezeBlank collapses runs of blank lines and trailing spaces so the stored extract is tidy.
func squeezeBlank(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, ln := range lines {
		ln = strings.TrimRight(ln, " \t\r")
		if strings.TrimSpace(ln) == "" {
			blank++
			if blank > 1 {
				continue
			}
			out = append(out, "")
			continue
		}
		blank = 0
		out = append(out, ln)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
