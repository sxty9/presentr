package store

import "io"

// DocStore is the single access point to the room's document pool, abstracted over its backend so
// the daemon can be built against either storage engine without the api layer knowing which:
//
//   - the default pure-Go pool (docs.go) — a passive, atomic JSON file; always available, needs no
//     toolchain, and is what `go build ./...` produces;
//   - scheme (docstore_scheme.go, behind `//go:build scheme`) — the mutable, path-addressed
//     document store that is presentr's intended production backend; consumed in-process via a cgo
//     binding to the scheme FFI.
//
// Both back the same contract: a passive store that keeps whole Documents and hands them back. The
// factory NewDocStore is provided by exactly one of the two build-tagged files (docstore_json.go for
// the default build, docstore_scheme.go under `-tags scheme`), so selecting scheme is a build-time
// choice and the pure-Go build never depends on cgo.
//
// A document is EITHER typed text (kind "text", its markdown inline in Document.Content) or an
// uploaded file (kind "file", its raw bytes kept out of band and reached via Bytes). Keeping the
// bytes out of the Document keeps List/Get cheap (metadata only — Portionierte Daten) while the
// bytes still belong to the same entity, reached through this one access point (no second store).
type DocStore interface {
	List() []Document
	Get(id string) (Document, bool)
	Add(d Document) error
	// AddFile stores a file document: its metadata record d plus its raw bytes, STREAMED from src so
	// the daemon never holds the whole file in memory (a 100 MB upload flows to disk as it arrives).
	// It writes at most max bytes; a source over the limit stores nothing and returns ErrFileTooLarge.
	// The bytes land FIRST, then the metadata, so the document only becomes observable (in List/Get)
	// once its bytes are safely stored — the metadata write is the single commit point (Atomare
	// Zugriffe). d.Size is stamped from the bytes actually written (the one field a caller cannot
	// know before the stream is drained); every other field is authored by the caller. Returns the
	// number of bytes stored.
	AddFile(d Document, src io.Reader, max int64) (int64, error)
	// Bytes returns the raw bytes of a file document, and whether they were found. Text documents
	// carry no bytes (their content is inline in Document.Content), so Bytes reports found=false.
	// It reads the whole blob into memory, so callers use it only for bounded reads (the AI grounding,
	// capped well under aigentic's request ceiling); streaming a large file to a client goes through
	// OpenBlob instead.
	Bytes(id string) ([]byte, bool)
	// OpenBlob returns a seekable reader over a file document's raw bytes plus its size, for streaming
	// the bytes to a client without buffering the whole file (a 100 MB download). The caller must
	// Close the reader. A missing blob (a text document, or an unknown id) reports found=false.
	OpenBlob(id string) (io.ReadSeekCloser, int64, bool)
	// SetExtract records the outcome of reading a file document's text — computed OUTSIDE the pool (the
	// pool is passive; the AI/text-layer read happens in the api/extract layer) and handed here to be
	// stored. The extract TEXT lives out of band beside the file bytes (kept out of List/Get, which
	// stay metadata-only — Portionierte Daten) while the small state/provenance fields land on the
	// document. It is the SAME entity reached through this ONE access point — no second store, no
	// parallel data path (Zugangspunkt wiederverwenden). The metadata write is the single commit point:
	// a "ready" extract's text is written before its state flips, so state "ready" always has text
	// behind it. An unknown id is a no-op (the document was deleted mid-read). Carried by BOTH backends.
	SetExtract(id string, ex Extract) error
	// ExtractText returns a file document's derived text (a bounded read — an extract is text, far
	// smaller than its source file), for the AI grounding, and whether it was found.
	ExtractText(id string) (string, bool)
	Delete(id string) error
}
