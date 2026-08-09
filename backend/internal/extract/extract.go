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
// The routing decision for a PDF (which can be BOTH text pages and scanned/photo pages) is made here
// and justified at pdfWantsAI: a PDF read purely from its text layer is used only when the PDF has NO
// embedded image, because any embedded image may carry text that only vision reads — the very
// content (a photographed rating plate) this feature exists to capture. Any image-bearing or
// text-layer-less PDF is handed to aigentic's extract, which reads its text layer AND its images.
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

// Section is one unit of an AI read: a whole file that fits one request, or one page range of a large
// PDF split to fit. Data is fed to aigentic as Mime (image → vision, application/pdf → document). TooBig
// marks a section that could not be made to fit even on its own (a lone oversized image or a single huge
// page) — it is named, never sent.
type Section struct {
	Label  string
	Mime   string
	Data   []byte
	TooBig bool
}

// SectionResult is the outcome of reading one Section: its transcribed text (empty when TooBig or on
// error), the engine/model that read it, and any error. TooBig carries through so assembly can name it.
type SectionResult struct {
	Label  string
	Text   string
	Engine string
	Model  string
	TooBig bool
	Err    error
}

// Plan is how a file will be read. Local means no AI (a text file, or a PDF read exactly from its text
// layer): LocalText/Source hold the answer and Sections is empty. Otherwise the read is by AI over one
// or more Sections (many only when a large PDF was split by page).
type Plan struct {
	Local     bool
	LocalText string
	Source    string // "text-layer" when Local
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
		return Plan{Local: true, LocalText: strings.TrimPrefix(string(data), "\uFEFF"), Source: "text-layer"}, nil

	case mime == "application/pdf":
		if !pdfWantsAI(data) {
			if text, ok := pdfTextLayer(data); ok {
				return Plan{Local: true, LocalText: text, Source: "text-layer"}, nil
			}
		}
		return Plan{Sections: pdfSections(data, budget)}, nil

	case strings.HasPrefix(mime, "image/"):
		return Plan{Sections: imageSections(mime, data, budget)}, nil

	default:
		// The upload classifier only ever admits images, PDFs and text, so this is defensive.
		return Plan{}, fmt.Errorf("extract: cannot read text from %q", mime)
	}
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
	if sec.TooBig {
		return SectionResult{Label: sec.Label, TooBig: true, Err: ErrTooLargeForAI}
	}
	name := filename
	if sec.Label != "" && sec.Label != "whole document" && sec.Label != "image" {
		name = filename + " (" + sec.Label + ")"
	}
	text, engine, model, err := ai.Extract(ctx, subject, name, sec.Mime, sec.Data)
	if err != nil {
		return SectionResult{Label: sec.Label, Err: err}
	}
	return SectionResult{Label: sec.Label, Text: text, Engine: engine, Model: model}
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
			parts = append(parts, strings.TrimRight(r.Text, "\n"))
		}
	}
	res.Text = strings.TrimSpace(strings.Join(parts, "\n\n"))
	return res, anyText
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
	if plan.Local {
		return Result{Text: plan.LocalText, Source: plan.Source}, nil
	}
	if ai == nil {
		return Result{}, ErrNoAI
	}
	// A single oversized section (a lone image or one huge page) is the only place ErrTooLargeForAI still
	// surfaces from Run — a large PDF was split into pages above.
	if len(plan.Sections) == 1 && plan.Sections[0].TooBig {
		return Result{}, ErrTooLargeForAI
	}
	results := make([]SectionResult, 0, len(plan.Sections))
	for _, sec := range plan.Sections {
		r := ReadSection(ctx, ai, subject, filename, sec)
		if r.Err != nil && !r.TooBig {
			return Result{}, r.Err // a transport/engine error aborts the single-call Run
		}
		results = append(results, r)
	}
	res, _ := AssembleText(results)
	if len(plan.Sections) == 1 {
		res.Source = "ai" // a single AI section keeps the simple provenance
	}
	return res, nil
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
