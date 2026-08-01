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
// The routing decision for a PDF (which can be BOTH text pages and scanned/photo pages) is made here
// and justified at pdfWantsAI: a PDF read purely from its text layer is used only when the PDF has NO
// embedded raster image, because any embedded image may carry text that only vision reads — the very
// content (a photographed rating plate) this feature exists to capture. Any image-bearing or
// text-layer-less PDF is handed WHOLE to aigentic's extract, which reads its text layer AND its images
// in one pass. This is the honest, complete choice; the deterministic no-AI path is reserved for the
// case where it is both exact and complete.
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

// ErrTooLargeForAI means a file that needs AI recognition is larger than one request to aigentic can
// carry. presentr does not silently drop it: the read fails with a named reason, and the honest way to
// read such a file — split it into page-sized sections and read each — is the sibling chunking order's
// job (see the package note and roomGrounding). Named, not swallowed.
var ErrTooLargeForAI = errors.New("extract: file is too large to read with the assistant in one request")

// MaxAIBytes bounds the bytes handed to aigentic's extract in one request, kept under aigentic's own
// 32 MiB request ceiling with room for the base64 envelope. A file past it fails with ErrTooLargeForAI
// rather than being sent and rejected upstream (or silently skipped). This is the same boundary the AI
// grounding respects, and the seam where the chunking order takes over for large scanned documents.
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

// AIExtractor recognizes the text contained in one file via aigentic's shared extract capability. It
// is injected (the api layer supplies an adapter over its aigentic client) so this package stays
// testable without a live AI and holds no transport of its own.
type AIExtractor interface {
	// Extract returns the transcribed text of one file plus the engine/model that read it. filename is
	// provenance only; mime selects how aigentic reads the bytes (image → vision, PDF → document).
	Extract(ctx context.Context, subject, filename, mime string, data []byte) (text, engine, model string, err error)
}

// Run reads the text of one uploaded file. It never touches the pool; the caller stores the Result. A
// text file or a pure-text PDF is read locally (exact, no AI); an image or an image-bearing/scanned
// PDF is read by the AI extractor. mime is the document's stored media type (already classified at
// upload); data is the file's bytes.
func Run(ctx context.Context, subject, filename, mime string, data []byte, ai AIExtractor) (Result, error) {
	switch {
	case strings.HasPrefix(mime, "text/"):
		// A text file's bytes are its text — exact, no AI. Trimmed of a leading byte-order mark (U+FEFF)
		// so the stored extract is clean.
		return Result{Text: strings.TrimPrefix(string(data), "\uFEFF"), Source: "text-layer"}, nil

	case mime == "application/pdf":
		if !pdfWantsAI(data) {
			if text, ok := pdfTextLayer(data); ok {
				return Result{Text: text, Source: "text-layer"}, nil
			}
		}
		return aiExtract(ctx, subject, filename, mime, data, ai)

	case strings.HasPrefix(mime, "image/"):
		return aiExtract(ctx, subject, filename, mime, data, ai)

	default:
		// The upload classifier only ever admits images, PDFs and text, so this is defensive: a kind we
		// cannot read is named rather than stored as an empty extract.
		return Result{}, fmt.Errorf("extract: cannot read text from %q", mime)
	}
}

// aiExtract hands a file to the AI recognizer, guarding the two ways it can be unavailable up front so
// each is a distinct, named, retriable reason rather than a generic failure.
func aiExtract(ctx context.Context, subject, filename, mime string, data []byte, ai AIExtractor) (Result, error) {
	if ai == nil {
		return Result{}, ErrNoAI
	}
	if len(data) > MaxAIBytes {
		return Result{}, ErrTooLargeForAI
	}
	text, engine, model, err := ai.Extract(ctx, subject, filename, mime, data)
	if err != nil {
		return Result{}, err
	}
	return Result{Text: text, Source: "ai", Engine: engine, Model: model}, nil
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
