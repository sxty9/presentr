package extract

// The IMAGE track of a PDF read — separate from, and independent of, the text-layer track (pdf.go).
//
// A PDF can carry two very different kinds of content at once: machine-readable text (read exactly and
// for free from the text layer) and raster images that may have text baked INTO them — a photographed
// nameplate, a connector label, a scanned diagram — which only vision reads. The old routing treated
// these as one all-or-nothing switch: the moment a PDF embedded any image, the WHOLE file (text pages
// included) was handed to the vision AI in byte-budget sections. A LaTeX manual with a few full-
// resolution phone photos would then fail entirely, because every section dragged one 15-plus-megabyte
// photo along and overran even the stretched read deadline — while its perfectly readable text layer
// sat right there, ignored.
//
// This file makes the image track its own lane. It enumerates the embedded raster images per page (like
// `pdfimages`), decodes each one, and — crucially — DOWNSCALES and re-encodes it as a modest JPEG before
// it is sent to the AI: a vision model gains nothing from a 4032×3024 sensor frame, but the payload and
// the recognition time drop by an order of magnitude. The ORIGINAL file is never touched (the axiom that
// stored bytes are kept verbatim); the downscaled copy exists only for the request. Each image becomes
// its own small Section labelled by the page it sits on, so a read reports "image on page 6: read" and
// never blocks the text on the images.

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"strings"
)

// visionLongEdge caps the long edge (in pixels) of an image sent to the vision AI. Anthropic's vision
// models see no benefit past roughly this size, so a full-resolution phone photo is scaled down to it —
// the single change that lets a photo-heavy PDF be read at all. An image already at or under this size is
// sent as-is (re-encoded to JPEG for a predictable, compact payload).
const visionLongEdge = 1568

// visionJPEGQuality is the quality of the re-encoded JPEG sent to the AI. High enough that small printed
// text on a nameplate stays legible, low enough that the payload is a fraction of the original.
const visionJPEGQuality = 82

// maxImagePixels bounds the pixel count of a single embedded image this reader will decode, so a
// pathological width/height in a PDF cannot make one image allocate unbounded memory. A genuine sensor
// frame (about 12 megapixels) sits well under it.
const maxImagePixels = 64 << 20 // 64 megapixels

// extractImageSections enumerates the embedded raster images of a PDF, one Section per image, each a
// downscaled JPEG ready for the vision AI and labelled by the page it appears on. It also returns how
// many embedded images were found but could NOT be decoded (an unsupported codec such as JBIG2 or
// JPEG2000), so the caller can disclose that gap rather than silently drop those images' text. ok is
// false only when the PDF cannot be parsed into pages at all — the caller then falls back to reading the
// whole document by AI, so nothing is lost.
func extractImageSections(data []byte, budget int) (sections []Section, skipped int, ok bool) {
	doc, ok := parsePDF(data)
	if !ok || len(doc.pages) == 0 {
		return nil, 0, false
	}
	imgs := locateImages(doc)
	if len(imgs) == 0 {
		return nil, 0, true
	}
	// Count images per page so a page with several images can label them "image 2 of 6".
	perPage := map[int]int{}
	for _, im := range imgs {
		perPage[im.page]++
	}
	pageSeen := map[int]int{}
	for _, im := range imgs {
		pageSeen[im.page]++
		label := imageLabel(im.page, pageSeen[im.page], perPage[im.page])
		jpg, dok := renderImageForVision(doc, im)
		if !dok {
			skipped++
			continue
		}
		sections = append(sections, Section{
			Label:  label,
			Mime:   "image/jpeg",
			Data:   jpg,
			Track:  "image",
			Page:   im.page,
			TooBig: len(jpg) > budget, // defensive; a downscaled JPEG is far under any real budget
		})
	}
	return sections, skipped, true
}

// imageLabel names an image section by the page it sits on, adding its position when the page holds more
// than one image ("image 2 of 6 on page 9"), so per-image progress and provenance point at exactly which
// picture on which page.
func imageLabel(page, idxOnPage, onPage int) string {
	if onPage <= 1 {
		return fmt.Sprintf("image on page %d", page)
	}
	return fmt.Sprintf("image %d of %d on page %d", idxOnPage, onPage, page)
}

// locatedImage is one embedded image XObject and the first page that references it. An image reused on
// several pages (a repeated logo) is read once, labelled with the first page it appears on, so the AI
// budget is not spent re-reading identical pictures.
type locatedImage struct {
	num  int
	page int // 1-based
	obj  *pdfObj
}

// locateImages walks the pages in reading order and collects every image XObject referenced by a page's
// resources, in first-seen order. It dedupes by object number so a shared image is read once.
func locateImages(doc *pdfDoc) []locatedImage {
	var out []locatedImage
	seen := map[int]bool{}
	for i, p := range doc.pages {
		for _, ref := range imageRefsInResources(doc, p.resources) {
			if seen[ref] {
				continue
			}
			o := doc.objs[ref]
			if o == nil || !o.hasStream {
				continue
			}
			if !dictHasNameValue(o.dict, "/Subtype", "/Image") {
				continue
			}
			seen[ref] = true
			out = append(out, locatedImage{num: ref, page: i + 1, obj: o})
		}
	}
	return out
}

// imageRefsInResources returns the object numbers of every XObject named in a page's /Resources /XObject
// dictionary. The resources value may itself be an indirect reference, as may the /XObject sub-dictionary,
// so both are resolved. Non-image XObjects (Forms) are filtered out by the caller via /Subtype.
func imageRefsInResources(doc *pdfDoc, resources []byte) []int {
	res := resolveIndirect(doc, resources)
	if res == nil {
		return nil
	}
	xo := dictGet(res, "/XObject")
	if xo == nil {
		return nil
	}
	xo = resolveIndirect(doc, xo)
	if xo == nil {
		return nil
	}
	return collectRefs(tokenizeDict(xo))
}

// resolveIndirect returns the dictionary bytes a value denotes: if the value IS a single "N G R"
// reference, the referenced object's dictionary is returned; otherwise the value is returned unchanged
// (it is already an inline dictionary/array). A dangling reference yields nil.
func resolveIndirect(doc *pdfDoc, val []byte) []byte {
	if num, ok := firstRef(val); ok {
		if o := doc.objs[num]; o != nil {
			return o.dict
		}
		return nil
	}
	return val
}

// renderImageForVision decodes one embedded image and returns it downscaled and re-encoded as a JPEG
// small enough for the vision AI. ok is false for a codec this reader cannot decode (JBIG2, JPEG2000,
// CCITT fax) — the caller then names that image as an undecoded gap rather than sending a payload it
// could not shrink.
func renderImageForVision(doc *pdfDoc, im locatedImage) ([]byte, bool) {
	img, ok := decodeImage(doc, im.obj)
	if !ok {
		return nil, false
	}
	img = downscaleLongEdge(img, visionLongEdge)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: visionJPEGQuality}); err != nil {
		return nil, false
	}
	return buf.Bytes(), true
}

// decodeImage turns an image XObject into a Go image. A DCTDecode stream IS a JPEG and is decoded
// directly (the common case for photographs). An unfiltered or FlateDecode stream carries raw samples
// that are reassembled from the image's /Width, /Height, /BitsPerComponent and colour space. Any other
// codec reports ok=false.
func decodeImage(doc *pdfDoc, o *pdfObj) (image.Image, bool) {
	w, _ := intValue(o.dict, "/Width")
	h, _ := intValue(o.dict, "/Height")
	if w <= 0 || h <= 0 || w*h > maxImagePixels {
		return nil, false
	}
	filter := imageCodec(o.dict)
	switch {
	case strings.Contains(filter, "DCTDecode"):
		img, err := jpeg.Decode(bytes.NewReader(o.stream))
		if err != nil {
			return nil, false
		}
		return img, true
	case filter == "" || strings.Contains(filter, "FlateDecode"):
		return decodeRawImage(doc, o, w, h)
	default:
		// LZWDecode, CCITTFaxDecode, JBIG2Decode, JPXDecode — not decodable here.
		return nil, false
	}
}

// imageCodec returns the actual image codec of an XObject — the LAST name in /Filter, which is the one
// that produced the image bytes (a /Filter may be an array like [/FlateDecode /DCTDecode], where the
// trailing codec is what matters). An absent /Filter yields "" (raw samples).
func imageCodec(dict []byte) string {
	v := dictGet(dict, "/Filter")
	if v == nil {
		return ""
	}
	toks := tokenizeDict(v)
	last := ""
	for _, tk := range toks {
		if len(tk) > 0 && tk[0] == '/' {
			last = string(tk)
		}
	}
	return last
}

// decodeRawImage reassembles a Go image from an XObject's raw (unfiltered or FlateDecode) samples. It
// handles the two colour spaces a design/manual PDF commonly stores uncompressed at 8 bits per component:
// DeviceRGB / CalRGB / ICCBased(N=3) as three bytes per pixel, and DeviceGray / CalGray / ICCBased(N=1)
// as one. Anything else (indexed palettes, CMYK, sub-byte depths) reports ok=false and is left to the
// whole-document AI fallback rather than reconstructed wrongly.
func decodeRawImage(doc *pdfDoc, o *pdfObj, w, h int) (image.Image, bool) {
	if bpc, ok := intValue(o.dict, "/BitsPerComponent"); ok && bpc != 8 {
		return nil, false
	}
	comps := colorComponents(doc, o.dict)
	if comps != 1 && comps != 3 {
		return nil, false
	}
	raw, ok := pdfDecodeStream(pdfStream{dict: o.dict, body: o.stream})
	if !ok {
		return nil, false
	}
	need := w * h * comps
	if len(raw) < need {
		return nil, false
	}
	if comps == 1 {
		g := image.NewGray(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			copy(g.Pix[y*g.Stride:y*g.Stride+w], raw[y*w:y*w+w])
		}
		return g, true
	}
	rgba := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		src := raw[y*w*3:]
		dst := rgba.Pix[y*rgba.Stride:]
		for x := 0; x < w; x++ {
			dst[x*4] = src[x*3]
			dst[x*4+1] = src[x*3+1]
			dst[x*4+2] = src[x*3+2]
			dst[x*4+3] = 0xff
		}
	}
	return rgba, true
}

// colorComponents reports how many samples per pixel an image's colour space carries: 3 for RGB, 1 for
// gray. An ICCBased space resolves to the /N of its profile stream. An unrecognized space returns 0 so
// the caller declines to reconstruct it.
func colorComponents(doc *pdfDoc, dict []byte) int {
	cs := dictGet(dict, "/ColorSpace")
	if cs == nil {
		return 0
	}
	toks := tokenizeDict(cs)
	for _, tk := range toks {
		switch string(tk) {
		case "/DeviceRGB", "/CalRGB", "/RGB":
			return 3
		case "/DeviceGray", "/CalGray", "/G":
			return 1
		}
	}
	// [/ICCBased N 0 R] — the profile stream's /N gives the component count.
	if bytes.Contains(cs, []byte("/ICCBased")) {
		for _, num := range collectRefs(toks) {
			if o := doc.objs[num]; o != nil {
				if n, ok := intValue(o.dict, "/N"); ok {
					return n
				}
			}
		}
	}
	return 0
}

// downscaleLongEdge shrinks an image so its long edge is at most max pixels, averaging the source pixels
// that fall in each destination cell (a box filter) so the smaller image stays clean rather than aliased.
// An image already within the limit is returned unchanged.
func downscaleLongEdge(src image.Image, max int) image.Image {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 {
		return src
	}
	long := sw
	if sh > long {
		long = sh
	}
	if long <= max {
		return src
	}
	nw := sw * max / long
	nh := sh * max / long
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for dy := 0; dy < nh; dy++ {
		sy0 := dy * sh / nh
		sy1 := (dy + 1) * sh / nh
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for dx := 0; dx < nw; dx++ {
			sx0 := dx * sw / nw
			sx1 := (dx + 1) * sw / nw
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var rr, gg, bb, cnt uint64
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					r, g, bl, _ := src.At(b.Min.X+sx, b.Min.Y+sy).RGBA()
					rr += uint64(r >> 8)
					gg += uint64(g >> 8)
					bb += uint64(bl >> 8)
					cnt++
				}
			}
			if cnt == 0 {
				cnt = 1
			}
			dst.SetRGBA(dx, dy, color.RGBA{uint8(rr / cnt), uint8(gg / cnt), uint8(bb / cnt), 0xff})
		}
	}
	return dst
}
