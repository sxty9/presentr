package store

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
	// AddFile stores a file document: its metadata record d plus its raw bytes, kept out of band.
	// The two land so that the document only becomes observable (in List/Get) once its bytes are
	// safely stored — the metadata write is the single commit point (Atomare Zugriffe).
	AddFile(d Document, data []byte) error
	// Bytes returns the raw bytes of a file document, and whether they were found. Text documents
	// carry no bytes (their content is inline in Document.Content), so Bytes reports found=false.
	Bytes(id string) ([]byte, bool)
	Delete(id string) error
}
