package store

import (
	"os"
	"path/filepath"
	"strconv"
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

// addFile builds a file record outside the pool (as the api layer does) and stores it with its raw
// bytes via the passive AddFile.
func addFile(t *testing.T, p *DocPool, id, name, mime string, data []byte) Document {
	t.Helper()
	d := Document{
		ID: id, Title: name, Kind: "file", Mime: mime,
		Description: name, Size: int64(len(data)), Author: "ada", Created: time.Now().Unix(),
	}
	if err := p.AddFile(d, data); err != nil {
		t.Fatalf("AddFile: %v", err)
	}
	return d
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
