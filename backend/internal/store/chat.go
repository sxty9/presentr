// The chat pool: presentr's third workflow stage persisted. It holds each user's conversation
// with the room assistant so a browser reload lands the user back in the same session
// (Zustandserhalt beim Reload). The assistant itself runs in aigentic (the "Ask AI" standard);
// presentr owns only the transcript, keyed by the holistic account so it follows the user across
// devices.
//
// Like every pool here it is a pure passive store: it keeps whole Messages per owner and hands
// them back, authoring nothing. The api layer sanitises and stamps each record and hands over the
// finished slice; the pool only stores it. Reads are copies and writes are atomic.
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Message is one turn in a room-assistant conversation. Model and Engine label an assistant turn
// with the aigentic model that produced it (Kennzeichnungspflicht für KI-Modellantworten); they
// are empty on user turns.
type Message struct {
	Role    string `json:"role"` // "user" | "assistant"
	Text    string `json:"text"`
	Model   string `json:"model,omitempty"`
	Engine  string `json:"engine,omitempty"`
	Created int64  `json:"created"`
}

// chatState is the whole on-disk document: each owner's conversation, keyed by username.
type chatState struct {
	Chats map[string][]Message `json:"chats"`
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
	p.st = chatState{Chats: map[string][]Message{}}
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
		st.Chats = map[string][]Message{}
	}
	p.st = st
	return p, nil
}

// History returns a copy of the owner's conversation in order (empty, never nil, when there is
// none — so it serialises as [] not null). Passive: exactly what is stored, no shaping.
func (p *ChatPool) History(owner string) []Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	src := p.st.Chats[owner]
	out := make([]Message, len(src))
	copy(out, src)
	return out
}

// Replace stores the owner's whole conversation, replacing any prior one. The caller authors and
// sanitises the messages; the pool only keeps them. An empty slice clears the owner's history and
// drops the key, so a cleared conversation leaves no ghost. Atomic: a persist that fails before the
// rename rolls the snapshot back to exactly its prior value.
func (p *ChatPool) Replace(owner string, msgs []Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	prev, existed := p.st.Chats[owner]
	if len(msgs) == 0 {
		delete(p.st.Chats, owner)
	} else {
		p.st.Chats[owner] = append([]Message{}, msgs...)
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
