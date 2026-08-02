package store

import (
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

// A conversation must round-trip through disk unchanged and stay owner-scoped.
func TestChatsPersistAndIsolate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "chats.json")
	p, err := OpenChats(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Replace("ada", []Message{{Role: "user", Text: "hi", Created: 1}, {Role: "assistant", Text: "hello", Model: "m", Engine: "e", Created: 2}}); err != nil {
		t.Fatal(err)
	}
	if err := p.Replace("grace", []Message{{Role: "user", Text: "hers", Created: 1}}); err != nil {
		t.Fatal(err)
	}

	p2, err := OpenChats(path)
	if err != nil {
		t.Fatal(err)
	}
	ada := p2.History("ada")
	if len(ada) != 2 || ada[0].Text != "hi" || ada[1].Model != "m" {
		t.Fatalf("ada's conversation did not survive reload: %+v", ada)
	}
	if g := p2.History("grace"); len(g) != 1 || g[0].Text != "hers" {
		t.Fatalf("owner isolation broke on reload: %+v", g)
	}
}

// History returns [] not nil for an unknown owner (so it serialises as [], never null), and hands
// back a copy that cannot mutate the pool.
func TestChatsHistoryEmptyAndCopied(t *testing.T) {
	p := openChats(t)
	got := p.History("nobody")
	if got == nil || len(got) != 0 {
		t.Fatalf("History for an unknown owner must be an empty, non-nil slice, got: %#v", got)
	}
	if err := p.Replace("ada", []Message{{Role: "user", Text: "one", Created: 1}}); err != nil {
		t.Fatal(err)
	}
	h := p.History("ada")
	h[0].Text = "tampered"
	if again := p.History("ada"); again[0].Text != "one" {
		t.Fatalf("History handed out a reference into the live snapshot: %+v", again)
	}
}

// Replacing with an empty slice clears the owner and drops the key — no ghost survives.
func TestChatsReplaceEmptyClears(t *testing.T) {
	p := openChats(t)
	if err := p.Replace("ada", []Message{{Role: "user", Text: "one", Created: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := p.Replace("ada", nil); err != nil {
		t.Fatal(err)
	}
	if got := p.History("ada"); len(got) != 0 {
		t.Fatalf("cleared conversation still returns messages: %+v", got)
	}
	if _, ok := p.st.Chats["ada"]; ok {
		t.Fatalf("cleared conversation left a ghost key: %+v", p.st.Chats)
	}
}

// A failed persist must leave the prior conversation exactly as it was (Atomare Zugriffe).
func TestChatsFailedSaveRollsBack(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "data")
	path := filepath.Join(dir, "chats.json")
	p, err := OpenChats(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Replace("ada", []Message{{Role: "user", Text: "keep", Created: 1}}); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := p.Replace("ada", []Message{{Role: "user", Text: "lost", Created: 2}}); err == nil {
		t.Fatal("Replace succeeded despite a broken write path")
	}
	got := p.History("ada")
	if len(got) != 1 || got[0].Text != "keep" {
		t.Fatalf("failed save left an observable intermediate state: %+v", got)
	}
}
