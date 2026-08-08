// Package store is presentr's persistence layer — the place the daemon OWNS state. Each
// pool here is a pure, PASSIVE store: it holds records and hands them back, and never
// interprets, filters, evaluates or applies policy to what it keeps. Every such decision
// lives in the caller (the api layer), never in here (Passive Speicher).
//
// This file holds the ONE atomic-write primitive every pool reuses (Keine Redundanz):
// temp file → fsync file → rename → fsync dir. The rename is the atomic commit point.
// Two fsyncs do two jobs: syncing the file orders its bytes ahead of the rename (a crash
// can never expose a half-written file); syncing the directory makes the rename entry
// itself durable (a crash can never resurrect the old name and drop an acknowledged write).
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// ErrFileTooLarge is returned by a streaming blob write when the source carries more than the
// caller's byte limit. It is a distinct sentinel so the api layer can turn the file away with a
// NAMED per-file reason (never a torn stream) rather than a generic failure.
var ErrFileTooLarge = errors.New("store: file exceeds the maximum size")

// pool is the shared base every passive pool embeds: one in-memory snapshot guarded by one
// mutex, plus the marshal-and-atomically-write step. It holds no policy — persist just serializes
// the current snapshot and hands the bytes to atomicWrite. The embedding pool owns its own typed
// state (st) and its path, and does the read-modify-write + rollback under mu.
type pool[T any] struct {
	mu sync.Mutex
	st T
}

// persist serializes the current snapshot and writes it atomically to path. The caller must hold
// p.mu. committed follows atomicWrite: false ⇒ the caller rolls its snapshot back; true ⇒ the
// write already landed and memory must be kept in agreement with disk.
func (p *pool[T]) persist(path string) (committed bool, err error) {
	b, err := json.MarshalIndent(p.st, "", "  ")
	if err != nil {
		return false, err
	}
	return atomicWrite(path, b)
}

// atomicWrite persists data to path atomically. It reports committed=true once the rename
// has landed the new bytes: a failure AFTER that point (only the directory fsync is left)
// is surfaced WITHOUT the caller rolling back — memory and the freshly renamed file already
// agree. A failure BEFORE the rename returns committed=false, so the caller restores its
// prior in-memory snapshot and no partial state is ever observable (Atomare Zugriffe).
func atomicWrite(path string, data []byte) (committed bool, err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return false, err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once the rename succeeds
	if _, err := f.Write(data); err != nil {
		f.Close()
		return false, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return false, err
	}
	// The rename has committed the new state to disk; sealing the directory only hardens it.
	return true, fsyncDir(dir)
}

// streamBlob writes src to path atomically, holding NONE of it whole in memory: the bytes stream
// straight from the reader through the temp file to the final name (temp → fsync → rename → dir
// fsync), the same durability as atomicWrite but for a body the daemon must not buffer (a 100 MB
// upload flows to disk as it arrives). It copies at most max bytes; a source carrying more writes
// nothing durable and returns ErrFileTooLarge (the temp file is discarded), so an over-limit file
// leaves no partial blob behind. Returns the number of bytes written.
func streamBlob(path string, src io.Reader, max int64) (int64, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return 0, err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once the rename succeeds
	// Copy one byte past the limit so an exactly-at-limit file is accepted and the first
	// over-limit byte is detected without reading the rest of an oversized body.
	n, err := io.Copy(f, io.LimitReader(src, max+1))
	if err != nil {
		f.Close()
		return 0, err
	}
	if n > max {
		f.Close()
		return 0, ErrFileTooLarge
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return 0, err
	}
	return n, fsyncDir(dir)
}

// fsyncDir flushes a directory's own metadata so a rename into it survives a crash. Fsync'ing
// the renamed file's data is not enough: until the directory entry is flushed, a crash can
// bring the pool back pointing at the pre-rename name, silently dropping an acknowledged write.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

// NewID returns a short, unguessable id for a record. It lives here so every pool mints ids
// one way (no redundancy), but it is a plain helper, not a pool method: the caller — never a
// passive pool — decides a new record's identity, then hands the finished record to the pool.
func NewID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
