//go:build !scheme

package store

import "path/filepath"

// NewDocStore returns the default document backend: the pure-Go passive JSON pool. This is the file
// selected by an ordinary `go build ./...`; build with `-tags scheme` to select the scheme backend
// instead (docstore_scheme.go). dataDir is the service's data directory (e.g. /var/lib/presentr).
func NewDocStore(dataDir string) (DocStore, error) {
	return OpenDocs(filepath.Join(dataDir, "docs.json"))
}
