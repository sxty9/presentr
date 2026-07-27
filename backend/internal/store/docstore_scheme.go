//go:build scheme

// This file is compiled only under `-tags scheme`. It backs the document pool with scheme — the
// mutable, path-addressed document store that is presentr's intended production backend — consumed
// in-process through its C ABI (scheme_ffi.h). Each document is stored as one file at
// documents/<id>, its bytes the JSON-encoded Document and its mandatory scheme description the
// document's own description (§4 of scheme's law requires a non-empty description on every node).
//
// Building this requires CGO_ENABLED=1, a C toolchain, and the prebuilt scheme static library — the
// scheme repo present as a sibling of presentr, with `libscheme_ffi.a` built (cargo build -p
// scheme-ffi --release). The default `go build ./...` selects the pure-Go pool instead and needs
// none of this. The sibling-relative paths below mirror how aigentic embeds scheme.
package store

/*
#cgo CFLAGS: -I${SRCDIR}/../../../../scheme/crates/scheme-ffi/include
#cgo LDFLAGS: ${SRCDIR}/../../../../scheme/target/release/libscheme_ffi.a -lpthread -ldl -lm
#include <stdlib.h>
#include "scheme_ffi.h"
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"
)

// docPrefix is the folder under which every document lives in the scheme tree.
const docPrefix = "documents"

// NewDocStore returns the scheme-backed document backend (selected because this file's `scheme`
// build tag is active). The store lives in a "scheme" subdirectory of the service's data dir.
func NewDocStore(dataDir string) (DocStore, error) {
	return OpenSchemeDocs(filepath.Join(dataDir, "scheme"))
}

// SchemeDocs is a DocStore over an open scheme Bestand. One mutex serialises all FFI calls, so a
// List (which reads many files) and any write are each an indivisible critical section — the same
// atomic-access guarantee the JSON pool gives, and the whole concurrency story since the daemon is
// the only writer.
type SchemeDocs struct {
	mu sync.Mutex
	h  *C.SchemeHandle
}

// OpenSchemeDocs opens (creating if needed) a scheme store at dir.
func OpenSchemeDocs(dir string) (*SchemeDocs, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	cdir := C.CBytes([]byte(dir))
	defer C.free(cdir)
	var h *C.SchemeHandle
	if st := C.scheme_open((*C.char)(cdir), C.size_t(len(dir)), &h); st != C.SCHEME_OK {
		return nil, schemeErr("open", st)
	}
	return &SchemeDocs{h: h}, nil
}

// Close releases the scheme handle.
func (s *SchemeDocs) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.h == nil {
		return nil
	}
	st := C.scheme_close(s.h)
	s.h = nil
	if st != C.SCHEME_OK {
		return schemeErr("close", st)
	}
	return nil
}

// Add stores the complete document as one described file. The caller authors the whole record; this
// backend only persists it (Passive Speicher). scheme requires a non-empty description, so we fall
// back to the title, then a placeholder, when the document carries none.
func (s *SchemeDocs) Add(d Document) error {
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	desc := strings.TrimSpace(d.Description)
	if desc == "" {
		desc = strings.TrimSpace(d.Title)
	}
	if desc == "" {
		desc = "(no description)"
	}
	path := docPrefix + "/" + d.ID
	s.mu.Lock()
	defer s.mu.Unlock()

	cpath := C.CBytes([]byte(path))
	defer C.free(cpath)
	cdesc := C.CBytes([]byte(desc))
	defer C.free(cdesc)
	var dptr unsafe.Pointer
	if len(data) > 0 {
		cdata := C.CBytes(data)
		defer C.free(cdata)
		dptr = cdata
	}
	st := C.scheme_put_structured(s.h,
		(*C.char)(cpath), C.size_t(len(path)),
		(*C.char)(cdesc), C.size_t(len(desc)),
		(*C.uint8_t)(dptr), C.size_t(len(data)))
	if st != C.SCHEME_OK {
		return schemeErr("put "+path, st)
	}
	return nil
}

// Get returns the document with the given id, and whether it was found.
func (s *SchemeDocs) Get(id string) (Document, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok, err := s.getBytes(docPrefix + "/" + id)
	if err != nil || !ok {
		return Document{}, false
	}
	var d Document
	if json.Unmarshal(b, &d) != nil {
		return Document{}, false
	}
	return d, true
}

// List returns every document. Passive: it hands back what is stored (scheme lists by path; the
// api layer decides presentation order). A single unreadable entry is skipped, not fatal.
func (s *SchemeDocs) List() []Document {
	s.mu.Lock()
	defer s.mu.Unlock()
	paths, err := s.listPaths(docPrefix)
	if err != nil {
		return []Document{}
	}
	out := make([]Document, 0, len(paths))
	for _, p := range paths {
		b, ok, err := s.getBytes(p)
		if err != nil || !ok {
			continue
		}
		var d Document
		if json.Unmarshal(b, &d) == nil {
			out = append(out, d)
		}
	}
	return out
}

// Delete removes a document. scheme delete is idempotent — a missing id is not an error.
func (s *SchemeDocs) Delete(id string) error {
	path := docPrefix + "/" + id
	s.mu.Lock()
	defer s.mu.Unlock()
	cpath := C.CBytes([]byte(path))
	defer C.free(cpath)
	if st := C.scheme_delete(s.h, (*C.char)(cpath), C.size_t(len(path)), nil); st != C.SCHEME_OK {
		return schemeErr("delete "+path, st)
	}
	return nil
}

// ── FFI buffer-protocol helpers (caller holds s.mu) ─────────────────────────────────────────

// getBytes reads a file's bytes via the two-call buffer protocol: a length query (NULL buffer,
// capacity 0) then a sized read. Absence (a missing node or a folder) reports found=false.
func (s *SchemeDocs) getBytes(path string) ([]byte, bool, error) {
	cpath := C.CBytes([]byte(path))
	defer C.free(cpath)
	var n C.size_t
	st := C.scheme_get(s.h, (*C.char)(cpath), C.size_t(len(path)), (*C.uint8_t)(nil), &n)
	if st == C.SCHEME_NOT_FOUND {
		return nil, false, nil
	}
	if st != C.SCHEME_OK && st != C.SCHEME_BUFFER_TOO_SMALL {
		return nil, false, schemeErr("get "+path, st)
	}
	if n == 0 {
		return []byte{}, true, nil
	}
	buf := C.malloc(n)
	if buf == nil {
		return nil, false, fmt.Errorf("scheme get %s: out of memory", path)
	}
	defer C.free(buf)
	outLen := n
	st = C.scheme_get(s.h, (*C.char)(cpath), C.size_t(len(path)), (*C.uint8_t)(buf), &outLen)
	if st == C.SCHEME_NOT_FOUND {
		return nil, false, nil
	}
	if st != C.SCHEME_OK {
		return nil, false, schemeErr("get "+path, st)
	}
	return C.GoBytes(buf, C.int(outLen)), true, nil
}

// listPaths returns the file paths under prefix (one per line from scheme_list), via the same
// two-call buffer protocol.
func (s *SchemeDocs) listPaths(prefix string) ([]string, error) {
	cpre := C.CBytes([]byte(prefix))
	defer C.free(cpre)
	var n C.size_t
	st := C.scheme_list(s.h, (*C.char)(cpre), C.size_t(len(prefix)), (*C.uint8_t)(nil), &n)
	if st != C.SCHEME_OK && st != C.SCHEME_BUFFER_TOO_SMALL {
		return nil, schemeErr("list", st)
	}
	if n == 0 {
		return nil, nil
	}
	buf := C.malloc(n)
	if buf == nil {
		return nil, fmt.Errorf("scheme list: out of memory")
	}
	defer C.free(buf)
	outLen := n
	if st = C.scheme_list(s.h, (*C.char)(cpre), C.size_t(len(prefix)), (*C.uint8_t)(buf), &outLen); st != C.SCHEME_OK {
		return nil, schemeErr("list", st)
	}
	raw := string(C.GoBytes(buf, C.int(outLen)))
	var paths []string
	for _, line := range strings.Split(raw, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			paths = append(paths, line)
		}
	}
	return paths, nil
}

func schemeErr(op string, st C.SchemeStatus) error {
	return fmt.Errorf("scheme %s: status %d", op, int(st))
}
