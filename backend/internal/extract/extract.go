// Package extract reads the text OUT of an uploaded file, ONCE, when the file arrives — so every
// later question works with a few hundred kilobytes of text instead of re-sending a 70 MB file to the
// AI. It is the read step that lives OUTSIDE the passive document pool: the pool stores the result
// (store.SetExtract), this package computes it.
//
// Two sources, one result:
//
//   - A text file's bytes ARE its text, and a PDF's machine-readable text layer can be read locally —
//     both are EXACT and cost no AI (pdfTextLayer, this package, stdlib only).
//   - Text that lives INSIDE an image — an equipment nameplate, a connector label, a model number on a
//     photograph, a scanned page — is not machine-readable text; RECOGNIZING it is an AI-shaped job.
//     That recognition is a capability EVERY document service needs, so it lives once in the shared
//     aigentic service (its `extract` capability), reached here through the injected AIExtractor —
//     never re-implemented locally (Reuse before Build; Keine ähnlichen Geschwister).
//
// A file that needs AI recognition but is TOO LARGE for one aigentic request is no longer turned away:
// it is SPLIT into page-sized sections (Prepare/splitPDFByPages), each a self-contained sub-PDF safely
// under the request limit, and each read on its own — the text is then reassembled into ONE extract. The
// split is byte-preserving (a page's streams are copied verbatim, never re-encoded), the page is the
// natural cut, and no page is dropped or read twice. Anything the reader cannot split cleanly is named
// per section rather than swallowed (Kein stummes Ausbleiben).
//
// Text and images are TWO INDEPENDENT tracks, not one all-or-nothing switch (pdfPlan). If a PDF has a
// machine-readable text layer it is ALWAYS read exactly and locally, whether or not the PDF also embeds
// images — the presence of a photo never drags the text into a slow, costly AI read. The embedded images
// are their own track: enumerated per page, each DOWNSCALED and sent to the vision AI on its own
// (pdfimages.go), so a photographed nameplate is still read without a 15-megabyte payload stalling the
// whole file. Only a PDF with NEITHER a readable text layer NOR a decodable image falls back to handing
// whole pages to the AI (pdfSections), so a scan in a codec this reader cannot open is never lost.
package extract

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ErrNoAI means a file needs AI recognition (an image, a scanned/photo PDF) but no AI extractor is
// available (the room assistant is not configured, or the caller lacks the aigentic right). The read
// is marked failed with this named reason and can be retried later without re-uploading the file.
var ErrNoAI = errors.New("extract: no AI is available to read text from images")

// ErrTooLargeForAI means a section that cannot be split any smaller (a single image, or a single PDF
// page) is still larger than one aigentic request can carry. presentr does not silently drop it: the
// section is named as unreadable and, for a whole-file single section, this error is returned so the
// api layer records a named, retriable failed state. A large PDF is split into pages before this can
// apply, so it only ever concerns a lone oversized image or a single enormous page.
var ErrTooLargeForAI = errors.New("extract: section is too large to read with the assistant in one request")

// MaxAIBytes is the FALLBACK per-request raw-byte budget for one section, used by Run and when a caller
// supplies no explicit budget. It is no longer the authority on aigentic's capacity: the api layer sizes
// sections from the limit aigentic REPORTS (Server.sectionBudget), so a change in aigentic's request
// ceiling needs no change here. This constant is only the conservative floor used when that query is
// unavailable.
const MaxAIBytes = 20 << 20

// Result is what a successful read produced: the text, whether it came from a machine-readable layer
// or AI recognition, and (for AI) which engine/model read it, so the document can label the source
// (Kennzeichnungspflicht für KI-Modellantworten).
type Result struct {
	Text   string
	Source string // "text-layer" | "ai"
	Model  string
	Engine string
}

// Section is one unit of an AI read: a whole file that fits one request, one page range of a large PDF
// split to fit, or one embedded image extracted and downscaled (Track "image"). Data is fed to aigentic
// as Mime (image → vision, application/pdf → document). TooBig marks a section that could not be made to
// fit even on its own (a lone oversized image or a single huge page) — named, never sent. Note marks a
// section that is NOT sent to the AI at all but records a named gap (an image in a codec this reader
// cannot decode), so the gap is disclosed rather than dropped (Kein stummes Ausbleiben).
type Section struct {
	Label  string
	Mime   string
	Data   []byte
	TooBig bool
	Track  string // "image" for an extracted embedded image; "" for a whole-document / page-split / image-file read
	Page   int    // 1-based source page of an image section (0 when not applicable)
	Note   string // when set, the section is not sent to the AI; this is the gap it records
}

// SectionResult is the outcome of reading one Section: its transcribed text (empty when TooBig, a Note,
// or on error), the engine/model that read it, and any error. Track/Page/Note/TooBig carry through so
// assembly can label each piece by its source and name any gap.
type SectionResult struct {
	Label  string
	Text   string
	Engine string
	Model  string
	TooBig bool
	Track  string
	Page   int
	Note   string
	Err    error
}

// Plan is how a file will be read, as two independent tracks. LocalText/Source hold the exact, no-AI
// text (a text file, or a PDF's text layer) — present whenever a readable text layer exists, EVEN when
// the file also has images. Sections are the AI reads (each embedded image on its own, or — for a scan
// with no text layer this reader cannot open — whole page ranges). A plan with LocalText and no Sections
// is a pure local read; a plan may carry both.
type Plan struct {
	LocalText string
	Source    string // "text-layer" when LocalText is set
	Sections  []Section
}

// AIExtractor recognizes the text contained in one file via aigentic's shared extract capability. It
// is injected (the api layer supplies an adapter over its aigentic client) so this package stays
// testable without a live AI and holds no transport of its own.
type AIExtractor interface {
	// Extract returns the transcribed text of one file plus the engine/model that read it. filename is
	// provenance only; mime selects how aigentic reads the bytes (image → vision, PDF → document).
	Extract(ctx context.Context, subject, filename, mime string, data []byte) (text, engine, model string, err error)
}

// Prepare decides HOW a file will be read, sizing any AI sections to budget (the raw bytes one request
// may carry). It never contacts the AI and never touches the pool — the caller reads the Sections and
// stores the result. budget<=0 falls back to MaxAIBytes.
func Prepare(mime string, data []byte, budget int) (Plan, error) {
	if budget <= 0 {
		budget = MaxAIBytes
	}
	switch {
	case strings.HasPrefix(mime, "text/"):
		// A text file's bytes are its text — exact, no AI. Trimmed of a leading byte-order mark (U+FEFF).
		return Plan{LocalText: strings.TrimPrefix(string(data), "\uFEFF"), Source: "text-layer"}, nil

	case mime == "application/pdf":
		return pdfPlan(data, budget), nil

	case strings.HasPrefix(mime, "image/"):
		return Plan{Sections: imageSections(mime, data, budget)}, nil

	default:
		// The upload classifier only ever admits images, PDFs and text, so this is defensive.
		return Plan{}, fmt.Errorf("extract: cannot read text from %q", mime)
	}
}

// pdfPlan splits a PDF read into its two independent tracks. The text layer, if coherent, is ALWAYS the
// local track (exact, no AI) — regardless of any embedded images. The embedded images are their own AI
// track: each is extracted, downscaled and made its own page-labelled section (extractImageSections). A
// PDF with a text layer but no decodable images is a pure local read; one with both carries both tracks;
// one with images but no text layer is read from its images alone. Only a PDF with NEITHER a readable
// text layer NOR any decodable image is handed to the AI whole (pdfSections) — a scan in a codec this
// reader cannot open, which must still be read rather than lost. A count of images found but not
// decodable is disclosed as a named gap so those images' text is never silently missing.
func pdfPlan(data []byte, budget int) Plan {
	text, hasText := pdfTextLayer(data)
	imgs, skipped, ok := extractImageSections(data, budget)

	if !hasText {
		if ok && len(imgs) > 0 {
			return Plan{Sections: withUndecodedNote(imgs, skipped)}
		}
		// No readable text layer and no decodable image: read the whole document by AI (page-split when
		// large), so a scan this reader cannot open still reaches vision.
		return Plan{Sections: pdfSections(data, budget)}
	}

	// A readable text layer is the exact, free local track — kept whether or not images are present.
	return Plan{LocalText: text, Source: "text-layer", Sections: withUndecodedNote(imgs, skipped)}
}

// withUndecodedNote appends a single named-gap section when some embedded images could not be decoded, so
// the assembled text discloses that those images' text is missing (Kein stummes Ausbleiben). With none
// skipped it returns the sections unchanged.
func withUndecodedNote(sections []Section, skipped int) []Section {
	if skipped <= 0 {
		return sections
	}
	noun := "image"
	if skipped > 1 {
		noun = "images"
	}
	return append(sections, Section{
		Track: "image",
		Note:  fmt.Sprintf("%d embedded %s could not be read (an unsupported image format)", skipped, noun),
	})
}

// pdfSections turns a PDF that needs AI into one or more sections. A PDF within budget is one section;
// an over-budget PDF is split by page (splitPDFByPages). If the split fails (an unparseable structure),
// the whole file becomes a single TooBig section, so the caller records the same named "too large"
// failure it would have before — never a torn or lost read.
func pdfSections(data []byte, budget int) []Section {
	if len(data) <= budget {
		return []Section{{Label: "whole document", Mime: "application/pdf", Data: data}}
	}
	if secs, ok := splitPDFByPages(data, budget); ok {
		return secs
	}
	return []Section{{Label: "whole document", Mime: "application/pdf", Data: data, TooBig: true}}
}

// imageSections handles an image. A single raster image cannot be split without re-encoding it (which
// would violate keeping the original bytes intact), so an over-budget image is one TooBig section.
func imageSections(mime string, data []byte, budget int) []Section {
	return []Section{{Label: "image", Mime: mime, Data: data, TooBig: len(data) > budget}}
}

// ReadSection reads one section's text via the AI extractor, or reports it as too big to send. It is the
// per-section unit shared by Run and the api layer's progress-tracked, resumable loop.
func ReadSection(ctx context.Context, ai AIExtractor, subject, filename string, sec Section) SectionResult {
	base := SectionResult{Label: sec.Label, Track: sec.Track, Page: sec.Page}
	if sec.Note != "" {
		// A named gap (an undecodable image): recorded, never sent to the AI.
		base.Note = sec.Note
		return base
	}
	if sec.TooBig {
		base.TooBig = true
		base.Err = ErrTooLargeForAI
		return base
	}
	if ai == nil {
		base.Err = ErrNoAI
		return base
	}
	name := filename
	if sec.Label != "" && sec.Label != "whole document" && sec.Label != "image" {
		name = filename + " (" + sec.Label + ")"
	}
	text, engine, model, err := ai.Extract(ctx, subject, name, sec.Mime, sec.Data)
	if err != nil {
		base.Err = err
		return base
	}
	base.Text, base.Engine, base.Model = text, engine, model
	return base
}

// AssembleText joins the section results into one extract text, naming any section that could not be
// read INLINE and per section (Kein stummes Ausbleiben) so the stored text discloses its own gaps. The
// engine/model are taken from the first section that was actually read. anyText reports whether any real
// text was produced (the api layer marks a read with no text at all as failed, not an empty "ready").
func AssembleText(results []SectionResult) (res Result, anyText bool) {
	res.Source = "ai"
	var parts []string
	for _, r := range results {
		switch {
		case r.Note != "":
			parts = append(parts, "["+r.Note+"]")
		case r.TooBig:
			parts = append(parts, "["+sectionName(r.Label)+": too large to read in one request — this part was not read]")
		case r.Err != nil:
			parts = append(parts, "["+sectionName(r.Label)+": could not be read: "+r.Err.Error()+"]")
		default:
			if strings.TrimSpace(r.Text) != "" {
				anyText = true
			}
			if res.Model == "" && res.Engine == "" && (r.Model != "" || r.Engine != "") {
				res.Model, res.Engine = r.Model, r.Engine
			}
			parts = append(parts, withSourceHeader(r))
		}
	}
	res.Text = strings.TrimSpace(strings.Join(parts, "\n\n"))
	return res, anyText
}

// withSourceHeader prefixes a read image section's text with the page it came from ("Image on page 4:"),
// so the assembled extract keeps the provenance a photographed nameplate needs to be useful downstream —
// which page a recognized label belongs to. Non-image sections (a whole document, a page range) keep
// their text as-is, matching the existing assembled shape.
func withSourceHeader(r SectionResult) string {
	body := strings.TrimRight(r.Text, "\n")
	if r.Track == "image" && r.Page > 0 {
		return "Image on page " + itoaPos(r.Page) + ":\n" + body
	}
	return body
}

// itoaPos renders a positive page number without pulling in strconv for one call site.
func itoaPos(n int) string {
	if n <= 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}

// sectionName gives a section a readable name for a gap note.
func sectionName(label string) string {
	label = strings.TrimSpace(label)
	if label == "" || label == "whole document" || label == "image" {
		return "this file"
	}
	return label
}

// Run reads the text of one uploaded file in a single call, splitting and reassembling internally when a
// large PDF needs it. It never touches the pool; the caller stores the Result. A text file or a pure-text
// PDF is read locally (exact, no AI); an image or an image-bearing/scanned PDF is read by the AI
// extractor. A lone section too large to send yields ErrTooLargeForAI. Run does not report progress or
// resume — the api layer drives the section loop itself for that (see Server.runExtraction).
func Run(ctx context.Context, subject, filename, mime string, data []byte, ai AIExtractor) (Result, error) {
	plan, err := Prepare(mime, data, MaxAIBytes)
	if err != nil {
		return Result{}, err
	}
	hasLocal := plan.LocalText != ""
	// A pure local read (a text file, or a PDF text layer with no image track).
	if len(plan.Sections) == 0 {
		return Result{Text: plan.LocalText, Source: plan.Source}, nil
	}
	// The image/scan track needs the AI. With no AI AND no local text there is nothing to return but a
	// named, retriable reason. With local text, the read still succeeds on the text and names the image gap.
	if ai == nil && !hasLocal {
		return Result{}, ErrNoAI
	}
	// A lone oversized section (a single image or one huge page) with no local text is the only place
	// ErrTooLargeForAI still surfaces from Run — a large PDF was split into pages above.
	if !hasLocal && len(plan.Sections) == 1 && plan.Sections[0].TooBig {
		return Result{}, ErrTooLargeForAI
	}
	results := make([]SectionResult, 0, len(plan.Sections))
	for _, sec := range plan.Sections {
		r := ReadSection(ctx, ai, subject, filename, sec)
		if r.Err != nil && !r.TooBig && r.Note == "" && !hasLocal {
			return Result{}, r.Err // a pure-AI read aborts on a transport/engine error
		}
		results = append(results, r)
	}
	img, anyImg := AssembleText(results)
	return combineTracks(plan.LocalText, plan.Source, img, anyImg), nil
}

// Combine merges the local text track with the read image sections into one extract Result, and reports
// whether ANY legible text was produced (local text, or a recognized image) — the api layer stores
// "ready" only when text exists, else a named "failed". It is AssembleText plus the two-track merge, so
// the api layer can build the same result after each image while reading AND once at the end.
func Combine(localText, localSource string, results []SectionResult) (res Result, anyText bool) {
	img, anyImg := AssembleText(results)
	return combineTracks(localText, localSource, img, anyImg), localText != "" || anyImg
}

// combineTracks merges the exact local text (a PDF's text layer) with the AI-read image track into one
// extract. With no local text the image result stands alone (Source "ai"). With local text, the image
// text — and any named image gap — is appended after it; the provenance becomes "mixed" when the images
// contributed real text, and stays "text-layer" when they added only a gap note. The engine/model of the
// image read is carried so a mixed extract is still labelled with the model that read its images.
func combineTracks(localText, localSource string, img Result, anyImg bool) Result {
	if localText == "" {
		return img
	}
	combined := strings.TrimRight(localText, "\n")
	if strings.TrimSpace(img.Text) != "" {
		combined += "\n\n" + img.Text
	}
	res := Result{Text: strings.TrimSpace(combined), Source: localSource}
	if anyImg {
		res.Source = "mixed"
		res.Model, res.Engine = img.Model, img.Engine
	}
	return res
}

// coherent reports whether extracted text is trustworthy enough to keep as an EXACT text-layer read.
// A PDF whose fonts use a custom/subset encoding can yield bytes that decode to gibberish; rather than
// store a wrong "exact" extract, such a read is rejected here so the file falls back to AI recognition.
// The test: some real text is present, and the printable share of it is high.
func coherent(text string) bool {
	t := strings.TrimSpace(text)
	if utf8.RuneCountInString(t) < 16 {
		return false
	}
	var printable, total int
	for _, r := range t {
		total++
		if r == '\n' || r == '\t' || r == ' ' || (r >= 0x20 && r != utf8.RuneError) {
			printable++
		}
	}
	return total > 0 && float64(printable)/float64(total) >= 0.85
}
