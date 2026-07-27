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
type DocStore interface {
	List() []Document
	Get(id string) (Document, bool)
	Add(d Document) error
	Delete(id string) error
}
