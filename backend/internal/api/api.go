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

	maxDocBody  = 1 << 20 // 1 MiB cap for a text document submission
	maxChatBody = 4 << 20 // 4 MiB cap for a full-conversation replace
	maxMessages = 200     // keep at most the newest N turns of a conversation
	maxMsgLen   = 100_000 // clamp a single message's text
	maxLabelLen = 100     // clamp a model/engine label
)

// Server wires the session verifier and presentr's passive pools into HTTP handlers. The
// connection-diagram pool is added here as that stage is built out.
type Server struct {
	v     *auth.Verifier
	docs  *store.DocPool
	chats *store.ChatPool
}

// New builds a server over the session verifier and the pools (the single access points to the
// room's knowledge and the users' conversations with the assistant).
func New(v *auth.Verifier, docs *store.DocPool, chats *store.ChatPool) *Server {
	return &Server{v: v, docs: docs, chats: chats}
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
	mux.HandleFunc("POST "+base+"docs", s.guard(rights.GroupUse, true, s.addDoc))
	mux.HandleFunc("GET "+base+"docs/{id}", s.guard(rights.GroupUse, false, s.getDoc))
	mux.HandleFunc("DELETE "+base+"docs/{id}", s.guard(rights.GroupUse, true, s.deleteDoc))

	// The room assistant's conversation (workflow stage 3), persisted per user so a reload lands
	// back in the same session. The assistant itself runs in aigentic; presentr keeps only the
	// transcript. GET reads the caller's conversation; PUT replaces it (CSRF on the write).
	mux.HandleFunc("GET "+base+"chats", s.guard(rights.GroupUse, false, s.getChats))
	mux.HandleFunc("PUT "+base+"chats", s.guard(rights.GroupUse, true, s.putChats))

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

// listDocs returns the room's documents. The pool hands back a consistent snapshot in storage
// order; newest-first is a presentation choice, made here, outside the passive pool.
func (s *Server) listDocs(w http.ResponseWriter, _ *http.Request, _ *auth.User) {
	docs := s.docs.List()
	for i, j := 0, len(docs)-1; i < j; i, j = i+1, j-1 {
		docs[i], docs[j] = docs[j], docs[i]
	}
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

// addDoc appends a text document to the shared room pool. Identity, kind, size and creation time
// are stamped HERE, outside the passive pool — every such evaluation lives in this layer. The
// append is atomic: it lands whole or leaves the pool untouched.
func (s *Server) addDoc(w http.ResponseWriter, r *http.Request, u *auth.User) {
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
