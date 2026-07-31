// Package api serves presentr's HTTP surface under /api/services/presentr/, behind the shared
// holistic session. presentr is the presentation-room tutor: a shared document pool describing
// the room, an auto-derived connection diagram, and an assistant that explains both. This file
// is the api layer — the ONLY place policy lives. It authenticates, enforces the one right the
// service declares, stamps identity/time onto records, and reaches the passive pools through
// their single access points. The pools evaluate nothing; every decision is made here.
// Error bodies match holistic's contract: {"detail": "..."}.
package api

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"presentr/internal/auth"
	"presentr/internal/rights"
	"presentr/internal/store"
)

const (
	base    = "/api/services/presentr/"
	service = "presentr"
	version = "0.1.0"

	maxDocBody     = 1 << 20 // 1 MiB cap for a text document submission
	maxChatBody    = 4 << 20 // 4 MiB cap for a full-conversation replace
	maxDiagramBody = 4 << 20 // 4 MiB cap for a whole-diagram replace
	maxMessages    = 200     // keep at most the newest N turns of a conversation
	maxMsgLen      = 100_000 // clamp a single message's text
	maxLabelLen    = 100     // clamp a model/engine label

	maxNodes   = 200   // clamp the number of devices in a diagram
	maxEdges   = 600   // clamp the number of connections
	maxPorts   = 24    // clamp the ports on one device
	maxNameLen = 120   // clamp a node/port/label name
	maxSymbol  = 16    // clamp a node symbol
	maxIDLen   = 64    // clamp a client-supplied id
	maxKeyLen  = 128   // clamp the source fingerprint
	maxCoord   = 20000 // clamp a canvas coordinate
)

// Server wires the session verifier and presentr's passive pools into HTTP handlers. The document
// pool is held as an interface so the backend (pure-Go JSON or scheme) is a build-time choice the
// api layer is blind to.
type Server struct {
	v       *auth.Verifier
	docs    store.DocStore
	chats   *store.ChatPool
	diagram *store.DiagramPool
}

// New builds a server over the session verifier and the pools (the single access points to the
// room's knowledge, its connection diagram and the users' conversations with the assistant).
func New(v *auth.Verifier, docs store.DocStore, chats *store.ChatPool, diagram *store.DiagramPool) *Server {
	return &Server{v: v, docs: docs, chats: chats, diagram: diagram}
}

type handler func(w http.ResponseWriter, r *http.Request, u *auth.User)

// Handler returns the routed http.Handler (Go 1.22 method+path patterns).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Public to any signed-in holistic user: service identity.
	mux.HandleFunc("GET "+base+"info", s.guard("", false, s.info))

	// The document pool (workflow stage 1). Reading and writing both require the presentr
	// right; writes additionally carry the CSRF double-submit guard.
	mux.HandleFunc("GET "+base+"docs", s.guard(rights.GroupUse, false, s.listDocs))
	// POST docs is the ONE way into the pool for BOTH kinds of knowledge: a JSON body writes a typed
	// text document; a multipart body uploads one or more files. addDoc routes on the content type,
	// so the two are one access point, not similar siblings.
	mux.HandleFunc("POST "+base+"docs", s.guard(rights.GroupUse, true, s.addDoc))
	mux.HandleFunc("GET "+base+"docs/{id}", s.guard(rights.GroupUse, false, s.getDoc))
	// The raw bytes of a file document, for the SDK viewers (image/PDF preview). Read-only.
	mux.HandleFunc("GET "+base+"docs/{id}/raw", s.guard(rights.GroupUse, false, s.getRaw))
	mux.HandleFunc("DELETE "+base+"docs/{id}", s.guard(rights.GroupUse, true, s.deleteDoc))

	// The room assistant's conversation (workflow stage 3), persisted per user so a reload lands
	// back in the same session. The assistant itself runs in aigentic; presentr keeps only the
	// transcript. GET reads the caller's conversation; PUT replaces it (CSRF on the write).
	mux.HandleFunc("GET "+base+"chats", s.guard(rights.GroupUse, false, s.getChats))
	mux.HandleFunc("PUT "+base+"chats", s.guard(rights.GroupUse, true, s.putChats))

	// The connection diagram (workflow stage 2). GET reads the current graph + whether it is the
	// document-derived state or manually modified. PUT replaces the current graph (a manual edit).
	// generate installs a freshly AI-derived baseline (the UI runs the aigentic extraction and
	// posts the result). restore brings the document-derived state back. All writes carry CSRF.
	mux.HandleFunc("GET "+base+"diagram", s.guard(rights.GroupUse, false, s.getDiagram))
	mux.HandleFunc("PUT "+base+"diagram", s.guard(rights.GroupUse, true, s.putDiagram))
	mux.HandleFunc("POST "+base+"diagram/generate", s.guard(rights.GroupUse, true, s.generateDiagram))
	mux.HandleFunc("POST "+base+"diagram/restore", s.guard(rights.GroupUse, true, s.restoreDiagram))

	mux.HandleFunc("GET "+base+"health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	return mux
}

// guard authenticates, optionally requires the fine-grained right (perm != "" ⇒ admin or
// membership in the backing group), and optionally enforces CSRF.
func (s *Server) guard(perm string, csrf bool, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := s.v.User(r)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "Not authenticated")
			return
		}
		if perm != "" && !u.Can(perm) {
			writeErr(w, http.StatusForbidden, "You do not have permission to use presentr")
			return
		}
		if csrf && !s.v.CheckCSRF(r) {
			writeErr(w, http.StatusForbidden, "CSRF check failed")
			return
		}
		h(w, r, u)
	}
}

// info is public to any authenticated user. It reports the service identity and whether the
// caller may use it, so the UI can gate its own surface the same way the backend does.
func (s *Server) info(w http.ResponseWriter, _ *http.Request, u *auth.User) {
	writeJSON(w, http.StatusOK, map[string]any{
		"service": service,
		"version": version,
		"user":    u.Username,
		"isAdmin": u.IsAdmin,
		"canUse":  u.Can(rights.GroupUse),
	})
}

// listDocs returns the room's documents, newest first. Ordering is a presentation choice, made here
// in the api layer over the snapshot the passive pool hands back — never inside the pool, and never
// assuming the backend's storage order (scheme lists by path, the JSON pool by insertion).
func (s *Server) listDocs(w http.ResponseWriter, _ *http.Request, _ *auth.User) {
	docs := s.docs.List()
	sort.SliceStable(docs, func(i, j int) bool {
		if docs[i].Created != docs[j].Created {
			return docs[i].Created > docs[j].Created
		}
		return docs[i].ID > docs[j].ID
	})
	writeJSON(w, http.StatusOK, map[string]any{"docs": docs})
}

// getDoc returns a single document by id (used by the UI to preview its content).
func (s *Server) getDoc(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	d, ok := s.docs.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "Document not found")
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// addDoc grows the shared room pool. A multipart body is a file upload (handled in files.go); a
// JSON body is a typed text document, appended here. Either way identity, kind, mime, size and
// creation time are stamped HERE, outside the passive pool — every such evaluation lives in this
// layer — and the write is atomic: it lands whole or leaves the pool untouched.
func (s *Server) addDoc(w http.ResponseWriter, r *http.Request, u *auth.User) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		s.uploadFiles(w, r, u)
		return
	}
	var body struct {
		Title       string `json:"title"`
		Content     string `json:"content"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxDocBody)).Decode(&body); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	title := strings.TrimSpace(body.Title)
	content := strings.TrimSpace(body.Content)
	if title == "" {
		writeErr(w, http.StatusBadRequest, "A title is required")
		return
	}
	if content == "" {
		writeErr(w, http.StatusBadRequest, "The document must not be empty")
		return
	}
	// Author the complete record outside the pool, then hand it over for passive storage.
	d := store.Document{
		ID:          store.NewID(),
		Title:       title,
		Kind:        "text",
		Mime:        "text/markdown",
		Description: strings.TrimSpace(body.Description),
		Content:     content,
		Size:        int64(len(content)),
		Author:      u.Username,
		Created:     time.Now().Unix(),
	}
	if err := s.docs.Add(d); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not save the document")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "document": d})
}

// deleteDoc removes a document from the room pool. Idempotent: deleting a missing id still
// reports success, so the UI converges on the same state either way.
func (s *Server) deleteDoc(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	if err := s.docs.Delete(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not delete the document")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// getChats returns the caller's conversation with the room assistant. Owner-scoped: a user only
// ever reads their own transcript.
func (s *Server) getChats(w http.ResponseWriter, _ *http.Request, u *auth.User) {
	writeJSON(w, http.StatusOK, map[string]any{"messages": s.chats.History(u.Username)})
}

// putChats replaces the caller's conversation. The full transcript is submitted by the UI after
// each turn (the assistant is stateless per call). Every record is authored and clamped HERE —
// outside the passive pool — before being handed over: roles are whitelisted, text and labels are
// bounded, creation time is stamped when missing, and the history is capped to the newest turns.
func (s *Server) putChats(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Text    string `json:"text"`
			Model   string `json:"model"`
			Engine  string `json:"engine"`
			Created int64  `json:"created"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxChatBody)).Decode(&body); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	msgs := body.Messages
	if len(msgs) > maxMessages {
		msgs = msgs[len(msgs)-maxMessages:] // keep the newest turns
	}
	out := make([]store.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		text := strings.TrimSpace(m.Text)
		if text == "" {
			continue
		}
		if len(text) > maxMsgLen {
			text = text[:maxMsgLen]
		}
		created := m.Created
		if created == 0 {
			created = time.Now().Unix()
		}
		out = append(out, store.Message{
			Role: m.Role, Text: text, Model: clip(m.Model, maxLabelLen), Engine: clip(m.Engine, maxLabelLen), Created: created,
		})
	}
	if err := s.chats.Replace(u.Username, out); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not save the conversation")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "messages": out})
}

// diagramView is the shape the UI reads: the current graph plus which state it is in and the
// provenance of the document-derived baseline.
func diagramView(st store.DiagramSnapshot) map[string]any {
	state := "document"
	if st.Modified {
		state = "manual"
	}
	return map[string]any{
		"nodes":     st.Current.Nodes,
		"edges":     st.Current.Edges,
		"state":     state,
		"modified":  st.Modified,
		"sourceKey": st.SourceKey,
		"generated": st.Generated,
	}
}

// getDiagram returns the current graph and its state.
func (s *Server) getDiagram(w http.ResponseWriter, _ *http.Request, _ *auth.User) {
	writeJSON(w, http.StatusOK, diagramView(s.diagram.Get()))
}

// putDiagram replaces the current graph with a manual edit. The submitted graph is sanitised HERE
// (outside the passive pool) into consistent records, then handed to the pool's atomic Update. The
// edit flips the diagram into the manually-modified state; the document-derived baseline is left
// intact, so it stays restorable.
func (s *Server) putDiagram(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	g, ok := decodeGraph(w, r)
	if !ok {
		return
	}
	err := s.diagram.Update(func(st *store.DiagramSnapshot) {
		st.Current = g
		st.Modified = true
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not save the diagram")
		return
	}
	writeJSON(w, http.StatusOK, diagramView(s.diagram.Get()))
}

// generateDiagram installs a freshly derived baseline. The UI runs the aigentic investigation of the
// documents (the "Ask AI" standard, session-forwarded) and posts the resulting graph plus a
// fingerprint of the documents it came from. The baseline (Doc) is always replaced; the visible
// graph (Current) is replaced too, but only while the user has NOT manually modified it — so a
// hand-drawn layout is never overwritten behind the user's back (it stays reachable via restore).
func (s *Server) generateDiagram(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	var body struct {
		Nodes     []jsonNode `json:"nodes"`
		Edges     []jsonEdge `json:"edges"`
		SourceKey string     `json:"sourceKey"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxDiagramBody)).Decode(&body); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	g := sanitizeGraph(body.Nodes, body.Edges)
	key := clip(body.SourceKey, maxKeyLen)
	err := s.diagram.Update(func(st *store.DiagramSnapshot) {
		st.Doc = g
		st.SourceKey = key
		st.Generated = time.Now().Unix()
		if !st.Modified {
			st.Current = store.CloneGraph(g)
		}
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not save the diagram")
		return
	}
	writeJSON(w, http.StatusOK, diagramView(s.diagram.Get()))
}

// restoreDiagram brings the document-derived state back: the current graph is replaced by the
// baseline and the manually-modified flag is cleared.
func (s *Server) restoreDiagram(w http.ResponseWriter, _ *http.Request, _ *auth.User) {
	err := s.diagram.Update(func(st *store.DiagramSnapshot) {
		st.Current = store.CloneGraph(st.Doc)
		st.Modified = false
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not restore the diagram")
		return
	}
	writeJSON(w, http.StatusOK, diagramView(s.diagram.Get()))
}

// jsonNode / jsonEdge are the raw wire shapes a client submits; sanitizeGraph turns them into clean,
// consistent store records.
type jsonNode struct {
	ID     string     `json:"id"`
	Name   string     `json:"name"`
	Symbol string     `json:"symbol"`
	X      float64    `json:"x"`
	Y      float64    `json:"y"`
	Ports  []jsonPort `json:"ports"`
}
type jsonPort struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type jsonEdge struct {
	ID       string `json:"id"`
	From     string `json:"from"`
	FromPort string `json:"fromPort"`
	To       string `json:"to"`
	ToPort   string `json:"toPort"`
	Label    string `json:"label"`
}

// decodeGraph reads a {nodes, edges} body and sanitises it, writing the error response itself on a
// bad body. ok=false means a response was already written.
func decodeGraph(w http.ResponseWriter, r *http.Request) (store.Graph, bool) {
	var body struct {
		Nodes []jsonNode `json:"nodes"`
		Edges []jsonEdge `json:"edges"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxDiagramBody)).Decode(&body); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, "Invalid request body")
		return store.Graph{}, false
	}
	return sanitizeGraph(body.Nodes, body.Edges), true
}

// sanitizeGraph authors clean, internally-consistent records from an untrusted graph: counts and
// string lengths are bounded, coordinates are finite and clamped, node/port ids are de-duplicated,
// and every edge is dropped unless both of its endpoints resolve to a real node+port. The result is
// a graph the passive pool can store verbatim and the UI can render without further checks.
func sanitizeGraph(nodes []jsonNode, edges []jsonEdge) store.Graph {
	if len(nodes) > maxNodes {
		nodes = nodes[:maxNodes]
	}
	out := store.Graph{Nodes: make([]store.Node, 0, len(nodes)), Edges: []store.Edge{}}
	seenNode := map[string]bool{}
	ports := map[string]map[string]bool{} // nodeID -> set of portIDs
	for _, n := range nodes {
		id := clip(n.ID, maxIDLen)
		if id == "" || seenNode[id] {
			continue
		}
		seenNode[id] = true
		ps := n.Ports
		if len(ps) > maxPorts {
			ps = ps[:maxPorts]
		}
		portSet := map[string]bool{}
		outPorts := make([]store.Port, 0, len(ps))
		for _, p := range ps {
			pid := clip(p.ID, maxIDLen)
			if pid == "" || portSet[pid] {
				continue
			}
			portSet[pid] = true
			outPorts = append(outPorts, store.Port{ID: pid, Name: clip(p.Name, maxNameLen)})
		}
		ports[id] = portSet
		out.Nodes = append(out.Nodes, store.Node{
			ID:     id,
			Name:   clip(n.Name, maxNameLen),
			Symbol: clip(n.Symbol, maxSymbol),
			X:      clampCoord(n.X),
			Y:      clampCoord(n.Y),
			Ports:  outPorts,
		})
	}
	if len(edges) > maxEdges {
		edges = edges[:maxEdges]
	}
	seenEdge := map[string]bool{}
	for _, e := range edges {
		from, to := clip(e.From, maxIDLen), clip(e.To, maxIDLen)
		fp, tp := clip(e.FromPort, maxIDLen), clip(e.ToPort, maxIDLen)
		if !ports[from][fp] || !ports[to][tp] {
			continue // dangling endpoint — drop it
		}
		id := clip(e.ID, maxIDLen)
		if id == "" || seenEdge[id] {
			id = store.NewID()
		}
		seenEdge[id] = true
		out.Edges = append(out.Edges, store.Edge{
			ID: id, From: from, FromPort: fp, To: to, ToPort: tp, Label: clip(e.Label, maxNameLen),
		})
	}
	return out
}

// clampCoord bounds a canvas coordinate to a finite, sane range (guarding NaN/Inf from the wire).
func clampCoord(v float64) float64 {
	if v != v { // NaN
		return 0
	}
	if v < 0 {
		return 0
	}
	if v > maxCoord {
		return maxCoord
	}
	return v
}

// clip trims a label to at most n bytes (assistant model/engine tags come from an upstream service,
// so bound them before storing).
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]string{"detail": detail})
}
