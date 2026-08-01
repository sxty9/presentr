// The chat pool: presentr's third workflow stage persisted. It holds each user's conversations
// with the room assistant so a browser reload lands the user back in the same session
// (Zustandserhalt beim Reload). The assistant itself runs in aigentic (the "Ask AI" standard);
// presentr owns only the transcript, keyed by the holistic account so it follows the user across
// devices.
//
// The room assistant is now the ONE shared chat building block (@holisdk/ui <Chat>), the same one
// aigentic drives — so presentr stores conversations exactly as aigentic does: one OPAQUE JSON blob
// per owner (a list of conversations), whose shape is owned by the UI's ChatAdapter, the single
// source of truth. The pool never parses that blob; it keeps the bytes and hands them back. The api
// layer validates the blob is well-formed before storing it (policy lives outside the pool). Reads
// are copies and writes are atomic (temp→fsync→rename, rollback on a failed save).
package store

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
)

// chatState is the whole on-disk document: each owner's conversations as one opaque JSON blob,
// keyed by username.
type chatState struct {
	Chats map[string]json.RawMessage `json:"chats"`
}

// ChatPool is the atomic, in-memory-cached persistence for the conversations. The daemon is the
// only writer, so one mutex is the whole concurrency story.
type ChatPool struct {
	path string
	pool[chatState]
}

// OpenChats loads the chat pool from path. A missing file means "no conversations yet".
func OpenChats(path string) (*ChatPool, error) {
	if path == "" {
		path = "/var/lib/presentr/chats.json"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	p := &ChatPool{path: path}
	p.st = chatState{Chats: map[string]json.RawMessage{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return p, nil
		}
		return nil, err
	}
	var st chatState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	if st.Chats == nil {
		st.Chats = map[string]json.RawMessage{}
	}
	p.st = st
	return p, nil
}

// emptyBlob is the valid-JSON stand-in returned for an owner with no conversations, so the UI always
// receives parseable JSON, never a null.
var emptyBlob = json.RawMessage("[]")

// Blob returns a copy of the owner's stored conversations verbatim, or "[]" when there are none.
// Passive: exactly the bytes last stored, no shaping.
func (p *ChatPool) Blob(owner string) json.RawMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	src := p.st.Chats[owner]
	if len(src) == 0 {
		return append(json.RawMessage(nil), emptyBlob...)
	}
	out := make(json.RawMessage, len(src))
	copy(out, src)
	return out
}

// Save stores the owner's whole conversation blob verbatim, replacing any prior one. The caller
// validates it is well-formed JSON within a size cap; the pool only keeps it. An empty blob (or a
// JSON empty array / null) clears the owner and drops the key, so a cleared chat leaves no ghost.
// Atomic: a persist that fails before the rename rolls the snapshot back to exactly its prior value.
func (p *ChatPool) Save(owner string, data json.RawMessage) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	prev, existed := p.st.Chats[owner]
	if isEmptyBlob(data) {
		delete(p.st.Chats, owner)
	} else {
		p.st.Chats[owner] = append(json.RawMessage(nil), data...)
	}
	committed, err := p.persist(p.path)
	if err != nil && !committed {
		if existed {
			p.st.Chats[owner] = prev
		} else {
			delete(p.st.Chats, owner)
		}
	}
	return err
}

// isEmptyBlob reports whether data carries no conversations — empty bytes, or the JSON empty
// array / null — so clearing every conversation drops the owner's key rather than storing a husk.
func isEmptyBlob(data json.RawMessage) bool {
	t := bytes.TrimSpace(data)
	return len(t) == 0 || bytes.Equal(t, []byte("[]")) || bytes.Equal(t, []byte("null"))
}
