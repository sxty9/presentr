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
	"io"
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

	// Extraction: the derived plain text of a FILE document, read ONCE when the file is uploaded and
	// kept beside the document so every later question works with a few hundred KB of text instead of
	// re-reading a 70 MB file (the point of the feature). These are the SMALL metadata fields a
	// List/Get carries (Portionierte Daten); the extract TEXT itself lives out of band, exactly like
	// the file bytes, and is reached via ExtractText — so List stays cheap even as extracts grow. The
	// extract is EITHER a PDF's machine-readable text layer read locally (exact, no AI) OR text
	// recognized by aigentic's shared extract capability (an image, a scan — the text on a nameplate).
	// A text document (kind "text") carries its text inline in Content, so it needs no separate
	// extract and leaves these empty.
	ExtractState  string `json:"extractState,omitempty"`  // "" (n/a) | "pending" | "reading" | "ready" | "failed"
	ExtractSource string `json:"extractSource,omitempty"` // "text-layer" (deterministic) | "ai" (recognized)
	ExtractModel  string `json:"extractModel,omitempty"`  // the AI model that read it (Kennzeichnungspflicht)
	ExtractEngine string `json:"extractEngine,omitempty"` // the AI engine that read it
	ExtractError  string `json:"extractError,omitempty"`  // why the read failed (state=="failed"); a retry can clear it
	ExtractSize   int64  `json:"extractSize,omitempty"`   // byte length of the extract text (storage accounting)
	ExtractedAt   int64  `json:"extractedAt,omitempty"`   // epoch seconds the read completed

	// Section progress for a LARGE file read in pieces (state "reading"): a scanned PDF too big for one
	// AI request is split into page-sized sections and read one at a time, so the state can say "section
	// 7 of 40" instead of only "pending" (Portionierte Daten — the UI shows how far, not the pieces
	// themselves). Zero for a small file read in one pass.
	ExtractSectionsDone  int `json:"extractSectionsDone,omitempty"`
	ExtractSectionsTotal int `json:"extractSectionsTotal,omitempty"`
}

// Extract is the outcome of reading a file document's text, produced OUTSIDE the pool (the pool is
// passive — it stores this, it never computes it) and handed to SetExtract. State is "ready" (Text
// holds the read text), "failed" (Error names why; Text empty) or "pending" (a read is running/queued;
// only State is touched, any prior Text is left intact so a retry never destroys the last good read).
type Extract struct {
	State  string // "pending" | "reading" | "ready" | "failed"
	Text   string // the read text (ready only)
	Source string // "text-layer" | "ai" (ready only)
	Model  string // AI model, when Source=="ai"
	Engine string // AI engine, when Source=="ai"
	Error  string // reason, when State=="failed"
	At     int64  // epoch seconds the read completed (stamped by the caller)

	// Progress for a chunked read (State "reading"/"failed" mid-way): how many page-sized sections have
	// been read, out of how many. Left zero for a single-pass read.
	SectionsDone  int
	SectionsTotal int
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
	path       string
	blobDir    string
	extractDir string // one file per document holding its derived extract text, kept out of the metadata a List walks
	jobDir     string // one file per document holding an in-progress chunked-read job, so a read resumes after a crash
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
	p := &DocPool{
		path:       path,
		blobDir:    filepath.Join(filepath.Dir(path), "blobs"),
		extractDir: filepath.Join(filepath.Dir(path), "extracts"),
		jobDir:     filepath.Join(filepath.Dir(path), "extract-jobs"),
	}
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

// AddFile stores a file document: its raw bytes are STREAMED out of band FIRST (atomically, never
// held whole in memory), then the metadata record is appended (atomically). The bytes flow straight
// from src to the blob file; a source over max stores nothing and returns ErrFileTooLarge. d.Size is
// stamped from the bytes actually written — the one field a caller cannot know before the stream is
// drained. The document only becomes observable once the metadata append lands, so a reader never
// sees a file entry whose bytes are missing; a crash between the two leaves only an orphan blob
// (invisible, harmless). A failed metadata append removes the just-written blob so nothing is left
// behind.
func (p *DocPool) AddFile(d Document, src io.Reader, max int64) (int64, error) {
	blob := filepath.Join(p.blobDir, d.ID)
	n, err := streamBlob(blob, src, max)
	if err != nil {
		return 0, err
	}
	d.Size = n
	if err := p.Add(d); err != nil {
		_ = os.Remove(blob) // roll back the orphan blob; best-effort
		return 0, err
	}
	return n, nil
}

// Bytes returns the raw bytes of a file document. A missing blob (a text document, or an unknown
// id) reports found=false. It buffers the whole blob, so it serves only bounded reads (the AI
// grounding); streaming a large file to a client goes through OpenBlob.
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

// OpenBlob opens a file document's blob for streaming, returning the reader and its size. The blob
// is one plain file, so os.Open gives a seekable reader (http.ServeContent can answer range
// requests from it) that the caller closes. A missing blob reports found=false.
func (p *DocPool) OpenBlob(id string) (io.ReadSeekCloser, int64, bool) {
	if id == "" {
		return nil, 0, false
	}
	f, err := os.Open(filepath.Join(p.blobDir, id))
	if err != nil {
		return nil, 0, false
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, false
	}
	return f, fi.Size(), true
}

// SetExtract records the outcome of reading a file document's text (computed OUTSIDE the pool — the
// pool only stores it, Passive Speicher). It is reached through the SAME access point as the document
// (no parallel store): the extract text is kept out of band as one file per document, and the small
// state/provenance fields are stamped onto the document metadata. The metadata write is the single
// commit point (Atomare Zugriffe): for a "ready" extract the text file is written FIRST, then the
// metadata is flipped, so a reader never sees state "ready" without the text behind it. A "pending"
// extract touches only the state (leaving any prior text intact, so a re-read never destroys the last
// good one); a "failed" extract records the reason and leaves any prior text intact. An id that no
// longer exists (the document was deleted mid-read) is a no-op — the extract for a gone document is
// simply discarded.
func (p *DocPool) SetExtract(id string, ex Extract) error {
	if id == "" {
		return nil
	}
	if ex.State == "ready" {
		// Write the text out of band first; it becomes reachable only once the metadata flip below
		// lands, so the two never disagree. atomicWrite gives the same temp→fsync→rename durability the
		// metadata file gets, so a crash never leaves a half-written extract.
		if _, err := atomicWrite(filepath.Join(p.extractDir, id), []byte(ex.Text)); err != nil {
			return err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	idx := -1
	for i := range p.st.Docs {
		if p.st.Docs[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		// The document is gone; drop the orphan extract we may have just written.
		if ex.State == "ready" {
			_ = os.Remove(filepath.Join(p.extractDir, id))
		}
		return nil
	}
	prev := append([]Document{}, p.st.Docs...)
	d := p.st.Docs[idx]
	switch ex.State {
	case "pending":
		d.ExtractState = "pending"
		d.ExtractSectionsDone, d.ExtractSectionsTotal = 0, 0
	case "reading":
		// A chunked read is in progress: record how far it has got, leaving any prior text intact.
		d.ExtractState = "reading"
		d.ExtractSectionsDone = ex.SectionsDone
		d.ExtractSectionsTotal = ex.SectionsTotal
	case "ready":
		d.ExtractState = "ready"
		d.ExtractSource = ex.Source
		d.ExtractModel = ex.Model
		d.ExtractEngine = ex.Engine
		d.ExtractError = ""
		d.ExtractSize = int64(len(ex.Text))
		d.ExtractedAt = ex.At
		d.ExtractSectionsDone, d.ExtractSectionsTotal = 0, 0
	case "failed":
		d.ExtractState = "failed"
		d.ExtractError = ex.Error
		d.ExtractedAt = ex.At
		// Keep the section counters so a resumable failure still shows how far it got.
		if ex.SectionsTotal > 0 {
			d.ExtractSectionsDone, d.ExtractSectionsTotal = ex.SectionsDone, ex.SectionsTotal
		}
	default:
		return nil
	}
	p.st.Docs[idx] = d
	committed, err := p.persist(p.path)
	if err != nil && !committed {
		p.st.Docs = prev
		return err
	}
	// A completed read no longer needs its resume job; drop it best-effort once the metadata is committed.
	if ex.State == "ready" {
		_ = os.Remove(filepath.Join(p.jobDir, id))
	}
	return err
}

// SetExtractJob persists the in-progress chunked-read job for a document (the sections read so far),
// out of band beside the extract text, so a read interrupted by a crash or an engine outage RESUMES from
// where it stopped rather than starting over (the task's "aus dem heraus fortgesetzt werden kann"). It is
// the SAME entity reached through this one access point (no second store), computed OUTSIDE the pool and
// handed in. Written atomically (temp→fsync→rename).
func (p *DocPool) SetExtractJob(id string, job []byte) error {
	if id == "" {
		return nil
	}
	_, err := atomicWrite(filepath.Join(p.jobDir, id), job)
	return err
}

// ExtractJob returns a document's persisted chunked-read job, and whether one is present.
func (p *DocPool) ExtractJob(id string) ([]byte, bool) {
	if id == "" {
		return nil, false
	}
	b, err := os.ReadFile(filepath.Join(p.jobDir, id))
	if err != nil {
		return nil, false
	}
	return b, true
}

// ExtractText returns the derived text of a file document, and whether it was found. It buffers the
// whole extract (bounded — a stored extract is text, far smaller than the file it came from), so it
// serves the AI grounding without ever loading the original file bytes.
func (p *DocPool) ExtractText(id string) (string, bool) {
	if id == "" {
		return "", false
	}
	b, err := os.ReadFile(filepath.Join(p.extractDir, id))
	if err != nil {
		return "", false
	}
	return string(b), true
}

// Delete removes the document with the given id. Missing id is a no-op (idempotent). Atomic:
// a failed persist restores the prior slice, so a rejected delete leaves no observable trace. The
// metadata is removed first (making the document invisible); its blob and extract are then dropped
// best-effort.
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
		_ = os.Remove(filepath.Join(p.blobDir, id))    // drop the blob once the metadata is gone
		_ = os.Remove(filepath.Join(p.extractDir, id)) // and its derived extract text
		_ = os.Remove(filepath.Join(p.jobDir, id))     // and any in-progress read job
	}
	return err
}
