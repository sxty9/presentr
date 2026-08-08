package store

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func openDocs(t *testing.T) *DocPool {
	t.Helper()
	p, err := OpenDocs(filepath.Join(t.TempDir(), "data", "docs.json"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// addDoc builds a complete record outside the pool — exactly as the api layer does — and hands
// it to the passive Add. The pool authors nothing itself.
func addDoc(t *testing.T, p *DocPool, title, content string) Document {
	t.Helper()
	d := Document{
		ID: NewID(), Title: title, Kind: "text", Mime: "text/markdown",
		Content: content, Size: int64(len(content)), Author: "ada", Created: time.Now().Unix(),
	}
	if err := p.Add(d); err != nil {
		t.Fatalf("Add: %v", err)
	}
	return d
}

// A save must round-trip through disk with no loss: the pool is reopened from the same file and
// the reloaded state must equal what was written, in order. End-to-end proof of the atomic write.
func TestDocsPersistRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "docs.json")
	p, err := OpenDocs(path)
	if err != nil {
		t.Fatal(err)
	}
	a := addDoc(t, p, "Projector", "HDMI in, VGA in")
	addDoc(t, p, "Room layout", "screen on the north wall")

	p2, err := OpenDocs(path)
	if err != nil {
		t.Fatal(err)
	}
	got := p2.List()
	if len(got) != 2 || got[0].ID != a.ID || got[0].Title != "Projector" || got[1].Title != "Room layout" {
		t.Fatalf("documents did not survive reload in order: %+v", got)
	}
}

// The pool must hand back exactly what was stored, in storage order, and List must return a copy:
// mutating the result must not disturb the next read (Passive Speicher).
func TestDocsListIsPassiveAndCopied(t *testing.T) {
	p := openDocs(t)
	addDoc(t, p, "one", "a")
	addDoc(t, p, "two", "b")

	got := p.List()
	if len(got) != 2 || got[0].Title != "one" || got[1].Title != "two" {
		t.Fatalf("pool did not return documents verbatim in storage order: %+v", got)
	}
	got[0].Title = "tampered"
	if again := p.List(); again[0].Title != "one" {
		t.Fatalf("List handed out a reference into the live snapshot: %+v", again)
	}
}

// Delete removes exactly the named document and is idempotent for a missing id.
func TestDocsDelete(t *testing.T) {
	p := openDocs(t)
	a := addDoc(t, p, "keep", "x")
	b := addDoc(t, p, "drop", "y")

	if err := p.Delete(b.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got := p.List()
	if len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("delete removed the wrong document: %+v", got)
	}
	if err := p.Delete("no-such-id"); err != nil {
		t.Fatalf("Delete of a missing id must be a no-op, got: %v", err)
	}
	if len(p.List()) != 1 {
		t.Fatalf("idempotent delete changed the pool")
	}
}

// Every write is one critical section, so a failed persist must leave NO observable change: the
// pool rolls the in-memory snapshot back to exactly what it was. save() is forced to fail by
// replacing the data directory with a regular file, so MkdirAll fails for any user.
func TestDocsFailedSaveRollsBack(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "data")
	path := filepath.Join(dir, "docs.json")
	p, err := OpenDocs(path)
	if err != nil {
		t.Fatal(err)
	}
	keep := addDoc(t, p, "keep", "x")

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	bad := Document{ID: NewID(), Title: "lost", Kind: "text", Created: time.Now().Unix()}
	if err := p.Add(bad); err == nil {
		t.Fatal("Add succeeded despite a broken write path")
	}
	got := p.List()
	if len(got) != 1 || got[0].ID != keep.ID {
		t.Fatalf("failed save left an observable intermediate state: %+v", got)
	}
}

// Many writers concurrently must all land: the per-pool mutex makes each read-modify-write atomic.
// Run under -race this also pins the absence of a data race on the shared snapshot.
func TestDocsConcurrentAddsAllLand(t *testing.T) {
	p := openDocs(t)
	const n = 40
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d := Document{ID: NewID(), Title: "n" + strconv.Itoa(i), Kind: "text", Created: time.Now().Unix()}
			if err := p.Add(d); err != nil {
				t.Errorf("Add: %v", err)
			}
		}(i)
	}
	wg.Wait()

	got := p.List()
	if len(got) != n {
		t.Fatalf("concurrent adds lost updates: got %d, want %d", len(got), n)
	}
	seen := map[string]bool{}
	for _, d := range got {
		if seen[d.ID] {
			t.Fatalf("duplicate id %q from concurrent adds", d.ID)
		}
		seen[d.ID] = true
	}
}

// The atomic write must not leave its temp file behind: after a successful save the data directory
// holds exactly the state file, so no partial artifact is ever observable to a reader.
func TestDocsSaveLeavesNoTempFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	p, err := OpenDocs(filepath.Join(dir, "docs.json"))
	if err != nil {
		t.Fatal(err)
	}
	addDoc(t, p, "a", "x")
	addDoc(t, p, "b", "y")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "docs.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("data dir holds %v, want exactly [docs.json]", names)
	}
}

// fsyncDir is the second half of the atomic-durable replace: it must succeed on a real directory
// and surface an error — never silently pass — when handed a path it cannot open.
func TestFsyncDir(t *testing.T) {
	dir := t.TempDir()
	if err := fsyncDir(dir); err != nil {
		t.Fatalf("fsyncDir on a real directory: %v", err)
	}
	if err := fsyncDir(filepath.Join(dir, "no-such-dir")); err == nil {
		t.Fatal("fsyncDir returned nil for a missing directory; a failed seal must not look sealed")
	}
}

// addFile builds a file record outside the pool (as the api layer does) and streams its raw bytes
// in via the passive AddFile; Size is stamped by the pool from the bytes written.
func addFile(t *testing.T, p *DocPool, id, name, mime string, data []byte) Document {
	t.Helper()
	d := Document{
		ID: id, Title: name, Kind: "file", Mime: mime,
		Description: name, Author: "ada", Created: time.Now().Unix(),
	}
	n, err := p.AddFile(d, bytes.NewReader(data), 100<<20)
	if err != nil {
		t.Fatalf("AddFile: %v", err)
	}
	if n != int64(len(data)) {
		t.Fatalf("AddFile stored %d bytes, want %d", n, len(data))
	}
	d.Size = n
	return d
}

// A file streamed in over the byte limit stores nothing and reports ErrFileTooLarge — no partial
// blob, no metadata, no leftover temp file — so an over-limit upload can be turned away cleanly.
func TestAddFileOverLimitStoresNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	p, err := OpenDocs(filepath.Join(dir, "docs.json"))
	if err != nil {
		t.Fatal(err)
	}
	d := Document{ID: NewID(), Title: "huge", Kind: "file", Mime: "application/pdf", Created: time.Now().Unix()}
	// The limit is 1 MiB; the source is 1 MiB + 1 byte, one byte past the ceiling.
	src := io.LimitReader(zeros{}, (1<<20)+1)
	if _, err := p.AddFile(d, src, 1<<20); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("AddFile of an over-limit stream: err=%v, want ErrFileTooLarge", err)
	}
	if _, ok := p.Get(d.ID); ok {
		t.Fatal("over-limit file left metadata behind")
	}
	if _, ok := p.Bytes(d.ID); ok {
		t.Fatal("over-limit file left a blob behind")
	}
	// The blobs directory must hold no leftover temp file from the aborted stream.
	if entries, err := os.ReadDir(filepath.Join(dir, "blobs")); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".tmp-") {
				t.Fatalf("aborted over-limit stream left a temp file: %s", e.Name())
			}
		}
	}
}

// A file exactly at the limit is accepted (the ceiling is inclusive) and streams to a client
// verbatim via OpenBlob.
func TestAddFileAtLimitAndOpenBlob(t *testing.T) {
	p := openDocs(t)
	raw := bytes.Repeat([]byte{0xab}, 1<<20)
	d := addFile(t, p, "f1", "at-limit.bin", "application/octet-stream", raw)
	// OpenBlob streams the same bytes, and reports the size without buffering.
	rc, size, ok := p.OpenBlob(d.ID)
	if !ok {
		t.Fatal("OpenBlob did not find the just-stored blob")
	}
	defer rc.Close()
	if size != int64(len(raw)) {
		t.Fatalf("OpenBlob size %d, want %d", size, len(raw))
	}
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, raw) {
		t.Fatal("OpenBlob streamed different bytes than were stored")
	}
}

// zeros is an infinite reader of NUL bytes, for driving an over-limit stream without allocating it.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// A file document's bytes round-trip out of band: the metadata lists it (with no inline content),
// Bytes hands back exactly the stored bytes, and a reopened pool still finds both.
func TestAddFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "docs.json")
	p, err := OpenDocs(path)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x00, 0xff}
	d := addFile(t, p, "img1", "layout.png", "image/png", raw)

	got, ok := p.Bytes(d.ID)
	if !ok || string(got) != string(raw) {
		t.Fatalf("Bytes(%s) = %v, %v; want the stored bytes", d.ID, got, ok)
	}
	// Metadata carries no inline bytes — the list stays small.
	list := p.List()
	if len(list) != 1 || list[0].Content != "" || list[0].Kind != "file" {
		t.Fatalf("List after AddFile = %+v; want one file with empty inline content", list)
	}

	p2, err := OpenDocs(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := p2.Bytes(d.ID); !ok || string(got) != string(raw) {
		t.Fatalf("after reopen Bytes(%s) = %v, %v; want the stored bytes", d.ID, got, ok)
	}
}

// Deleting a file document drops both its metadata and its bytes; Bytes on a text document or an
// unknown id reports not-found rather than erroring.
func TestDeleteFileRemovesBytes(t *testing.T) {
	p := openDocs(t)
	d := addFile(t, p, "f1", "manual.pdf", "application/pdf", []byte("%PDF-1.7\n..."))
	text := addDoc(t, p, "note", "hello")

	if _, ok := p.Bytes(text.ID); ok {
		t.Fatal("a text document must carry no bytes")
	}
	if _, ok := p.Bytes("nope"); ok {
		t.Fatal("an unknown id must report not-found")
	}
	if err := p.Delete(d.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := p.Bytes(d.ID); ok {
		t.Fatal("Bytes still found after Delete; the blob must be gone")
	}
	if _, ok := p.Get(d.ID); ok {
		t.Fatal("metadata still present after Delete")
	}
}
