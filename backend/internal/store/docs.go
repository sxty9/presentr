// The document pool: presentr's first workflow stage. Users pour everything known about the
// presentation room into it — device manuals, wiring notes, the room layout, free text. The
// connection diagram and the chat assistant are both derived FROM this pool downstream, so it
// is the single source of truth for "what the room is".
//
// The pool is shared, not per-user: one room, one shared body of knowledge that everyone with
// the right sees and grows together (matching the sketches — one Docs tab, one diagram, one
// chat over them). It is a pure passive store: it keeps whole Documents and hands them back in
// storage order, authoring nothing. Identity, author and time are stamped by the api layer and
// handed in complete; the pool never fills a field of its own. Every read is a copy and every
// write is atomic (temp→fsync→rename), so no partial state is ever observable.
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Document is one item in the room's knowledge pool. The caller authors every field before
// handing it to Add. Description is a short human summary of the item — it is mandatory in
// scheme (the intended production backend for this pool), so presentr carries it from day one.
type Document struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Kind        string `json:"kind"`        // "text" | "file"
	Mime        string `json:"mime"`        // e.g. "text/markdown", "application/pdf"
	Description string `json:"description"` // short human summary of what this item is
	Content     string `json:"content"`     // inline text for kind=="text" (files store bytes out of band, reached via Bytes)
	Size        int64  `json:"size"`        // byte length of the item's content (text runes or file bytes)
	Author      string `json:"author"`      // who added it (stamped by the api layer)
	Created     int64  `json:"created"`     // epoch seconds (stamped by the api layer)
}

// docState is the whole on-disk document: the room's flat, ordered list of items.
type docState struct {
	Docs []Document `json:"docs"`
}

// DocPool is the atomic, in-memory-cached persistence for the room's documents. The daemon is
// the only writer, so one mutex is the whole concurrency story. Typed-text documents live entirely
// in docs.json (their markdown inline); an uploaded file's raw bytes live out of band as one file
// per document under blobDir, so the metadata list a poll walks stays small (Portionierte Daten).
type DocPool struct {
	path    string
	blobDir string
	pool[docState]
}

// OpenDocs loads the document pool from path. A missing file means "no documents yet". Uploaded
// file bytes are kept in a "blobs" directory beside the metadata file.
func OpenDocs(path string) (*DocPool, error) {
	if path == "" {
		path = "/var/lib/presentr/docs.json"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	p := &DocPool{path: path, blobDir: filepath.Join(filepath.Dir(path), "blobs")}
	p.st = docState{Docs: []Document{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return p, nil
		}
		return nil, err
	}
	var st docState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	if st.Docs == nil {
		st.Docs = []Document{}
	}
	p.st = st
	return p, nil
}

// List returns a copy of every document in storage order. Passive: it hands back exactly
// what is stored — ordering and shaping for presentation are the caller's job, outside the pool.
func (p *DocPool) List() []Document {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Document, len(p.st.Docs))
	copy(out, p.st.Docs)
	return out
}

// Get returns a copy of the document with the given id, and whether it was found.
func (p *DocPool) Get(id string) (Document, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, d := range p.st.Docs {
		if d.ID == id {
			return d, true
		}
	}
	return Document{}, false
}

// Add keeps a complete document and persists it. The pool is passive: the caller authors the
// whole record, and Add only stores it. The read-modify-write runs under the lock as one unit;
// a persist that fails before the atomic rename rolls the snapshot back to exactly its prior value.
func (p *DocPool) Add(d Document) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	prev := p.st.Docs
	p.st.Docs = append(append([]Document{}, prev...), d)
	committed, err := p.persist(p.path)
	if err != nil && !committed {
		p.st.Docs = prev
	}
	return err
}

// AddFile stores a file document: its raw bytes are written out of band FIRST (atomically), then
// the metadata record is appended (atomically). The document only becomes observable once the
// metadata append lands, so a reader never sees a file entry whose bytes are missing; a crash
// between the two leaves only an orphan blob (invisible, harmless). A failed metadata append
// removes the just-written blob so nothing is left behind.
func (p *DocPool) AddFile(d Document, data []byte) error {
	blob := filepath.Join(p.blobDir, d.ID)
	if _, err := atomicWrite(blob, data); err != nil {
		return err
	}
	if err := p.Add(d); err != nil {
		_ = os.Remove(blob) // roll back the orphan blob; best-effort
		return err
	}
	return nil
}

// Bytes returns the raw bytes of a file document. A missing blob (a text document, or an unknown
// id) reports found=false.
func (p *DocPool) Bytes(id string) ([]byte, bool) {
	if id == "" {
		return nil, false
	}
	b, err := os.ReadFile(filepath.Join(p.blobDir, id))
	if err != nil {
		return nil, false
	}
	return b, true
}

// Delete removes the document with the given id. Missing id is a no-op (idempotent). Atomic:
// a failed persist restores the prior slice, so a rejected delete leaves no observable trace. The
// metadata is removed first (making the document invisible); its blob is then dropped best-effort.
func (p *DocPool) Delete(id string) error {
	p.mu.Lock()
	prev := p.st.Docs
	next := make([]Document, 0, len(prev))
	for _, d := range prev {
		if d.ID != id {
			next = append(next, d)
		}
	}
	if len(next) == len(prev) {
		p.mu.Unlock()
		return nil
	}
	p.st.Docs = next
	committed, err := p.persist(p.path)
	if err != nil && !committed {
		p.st.Docs = prev
	}
	p.mu.Unlock()
	if err == nil {
		_ = os.Remove(filepath.Join(p.blobDir, id)) // drop the blob once the metadata is gone
	}
	return err
}
