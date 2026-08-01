package store

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func openChats(t *testing.T) *ChatPool {
	t.Helper()
	p, err := OpenChats(filepath.Join(t.TempDir(), "data", "chats.json"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A conversation blob must round-trip through disk byte-for-byte and stay owner-scoped.
func TestChatsPersistAndIsolate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "chats.json")
	p, err := OpenChats(path)
	if err != nil {
		t.Fatal(err)
	}
	ada := json.RawMessage(`[{"id":"c1","title":"hi","messages":[{"role":"user","content":"hi"}]}]`)
	grace := json.RawMessage(`[{"id":"g1","title":"hers","messages":[{"role":"user","content":"hers"}]}]`)
	if err := p.Save("ada", ada); err != nil {
		t.Fatal(err)
	}
	if err := p.Save("grace", grace); err != nil {
		t.Fatal(err)
	}

	p2, err := OpenChats(path)
	if err != nil {
		t.Fatal(err)
	}
	// The blob round-trips semantically (the on-disk file is pretty-printed, so bytes may be
	// re-indented; the UI parses JSON either way).
	if got := compact(t, p2.Blob("ada")); got != string(ada) {
		t.Fatalf("ada's conversations did not survive reload: %s", got)
	}
	if got := compact(t, p2.Blob("grace")); got != string(grace) {
		t.Fatalf("owner isolation broke on reload: %s", got)
	}
}

// compact strips insignificant whitespace so a re-indented blob can be compared to its source.
func compact(t *testing.T, b json.RawMessage) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		t.Fatalf("blob is not valid JSON: %v (%s)", err, b)
	}
	return buf.String()
}

// Blob returns "[]" (never null) for an unknown owner and hands back a copy that cannot mutate the
// pool.
func TestChatsBlobEmptyAndCopied(t *testing.T) {
	p := openChats(t)
	got := p.Blob("nobody")
	if string(got) != "[]" {
		t.Fatalf("Blob for an unknown owner must be [], got: %s", got)
	}
	if err := p.Save("ada", json.RawMessage(`[{"id":"c1","title":"one"}]`)); err != nil {
		t.Fatal(err)
	}
	h := p.Blob("ada")
	h[0] = 'X' // mutate the returned copy
	if again := p.Blob("ada"); again[0] != '[' {
		t.Fatalf("Blob handed out a reference into the live snapshot: %s", again)
	}
}

// Clearing with an empty array (or nil) drops the owner and leaves no ghost key.
func TestChatsSaveEmptyClears(t *testing.T) {
	p := openChats(t)
	if err := p.Save("ada", json.RawMessage(`[{"id":"c1"}]`)); err != nil {
		t.Fatal(err)
	}
	if err := p.Save("ada", json.RawMessage(`[]`)); err != nil {
		t.Fatal(err)
	}
	if got := p.Blob("ada"); string(got) != "[]" {
		t.Fatalf("cleared conversations still return data: %s", got)
	}
	if _, ok := p.st.Chats["ada"]; ok {
		t.Fatalf("cleared conversations left a ghost key: %+v", p.st.Chats)
	}
}

// A failed persist must leave the prior blob exactly as it was (Atomare Zugriffe).
func TestChatsFailedSaveRollsBack(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "data")
	path := filepath.Join(dir, "chats.json")
	p, err := OpenChats(path)
	if err != nil {
		t.Fatal(err)
	}
	keep := json.RawMessage(`[{"id":"c1","title":"keep"}]`)
	if err := p.Save("ada", keep); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := p.Save("ada", json.RawMessage(`[{"id":"c1","title":"lost"}]`)); err == nil {
		t.Fatal("Save succeeded despite a broken write path")
	}
	if got := p.Blob("ada"); string(got) != string(keep) {
		t.Fatalf("failed save left an observable intermediate state: %s", got)
	}
}
