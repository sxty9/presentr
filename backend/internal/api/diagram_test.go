package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"presentr/internal/store"
)

// newDiagramServer builds a server wired with a real diagram pool (newServer leaves it nil), so the
// connection-diagram handlers can be exercised end to end.
func newDiagramServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	docs, err := store.OpenDocs(filepath.Join(dir, "docs.json"))
	if err != nil {
		t.Fatal(err)
	}
	diagram, err := store.OpenDiagram(filepath.Join(dir, "diagram.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil, docs, nil, diagram, nil)
	t.Cleanup(s.WaitAsk)
	t.Cleanup(s.WaitExtractions)
	return s
}

// postGenerate posts one generation outcome and returns the decoded diagram view.
func postGenerate(t *testing.T, s *Server, body string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	s.generateDiagram(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body)), user())
	if rec.Code != http.StatusOK {
		t.Fatalf("generate = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("generate body = %s (err %v)", rec.Body.String(), err)
	}
	return out
}

// getDiagram reads the current diagram view (used to prove an outcome PERSISTED across a fresh read,
// which is what a browser reload does).
func getDiagramView(t *testing.T, s *Server) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	s.getDiagram(rec, httptest.NewRequest(http.MethodGet, "/x", nil), user())
	if rec.Code != http.StatusOK {
		t.Fatalf("get diagram = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("get diagram body = %s (err %v)", rec.Body.String(), err)
	}
	return out
}

func generationOf(t *testing.T, view map[string]any) map[string]any {
	t.Helper()
	gen, ok := view["generation"].(map[string]any)
	if !ok {
		t.Fatalf("view carries no generation object: %+v", view)
	}
	return gen
}

func nodeCount(view map[string]any) int {
	nodes, _ := view["nodes"].([]any)
	return len(nodes)
}

// A successful generation installs the graph AND records the model that produced it (Kennzeichnungspflicht),
// and the outcome survives a fresh read (Zustandserhalt).
func TestGenerateDiagramOKRecordsModel(t *testing.T) {
	s := newDiagramServer(t)
	view := postGenerate(t, s, `{"state":"ok","nodes":[{"id":"n1","name":"Display","symbol":"TV","ports":[{"id":"n1p1","name":"HDMI"}]}],"edges":[],"sourceKey":"k1","note":"one display found","model":"claude-x","engine":"anthropic"}`)
	if nodeCount(view) != 1 {
		t.Fatalf("the derived graph must be installed, got %d nodes", nodeCount(view))
	}
	gen := generationOf(t, view)
	if gen["state"] != "ok" || gen["model"] != "claude-x" || gen["engine"] != "anthropic" {
		t.Fatalf("ok generation must record its model/engine: %+v", gen)
	}
	// A fresh read (a browser reload) sees the same outcome — it is persisted, not a transient toast.
	if g := generationOf(t, getDiagramView(t, s)); g["state"] != "ok" {
		t.Fatalf("the ok outcome must persist across a read: %+v", g)
	}
}

// An "empty" outcome — the assistant concluded no connections — persists the assistant's OWN reason so
// the UI can show a standing message instead of an endless spinner (Kein stummes Ausbleiben).
func TestGenerateDiagramEmptyPersistsReason(t *testing.T) {
	s := newDiagramServer(t)
	view := postGenerate(t, s, `{"state":"empty","note":"no wiring diagram or device list was found","model":"claude-x","engine":"anthropic"}`)
	gen := generationOf(t, view)
	if gen["state"] != "empty" {
		t.Fatalf("state must be empty, got %+v", gen)
	}
	if gen["note"] != "no wiring diagram or device list was found" {
		t.Fatalf("the assistant's reason must be persisted, got %+v", gen["note"])
	}
	// Reload continuity: the standing message is read back verbatim.
	if g := generationOf(t, getDiagramView(t, s)); g["note"] != "no wiring diagram or device list was found" {
		t.Fatalf("the empty reason must persist across a read: %+v", g)
	}
}

// A fruitless attempt (empty) after a good diagram must NOT wipe the good diagram — only flip the standing
// outcome. A prior hand-usable graph is never destroyed by a later attempt that concludes nothing.
func TestGenerateEmptyDoesNotWipeExistingGraph(t *testing.T) {
	s := newDiagramServer(t)
	postGenerate(t, s, `{"state":"ok","nodes":[{"id":"n1","name":"Display","symbol":"TV","ports":[{"id":"n1p1","name":"HDMI"}]}],"edges":[],"sourceKey":"k1","model":"m1"}`)
	view := postGenerate(t, s, `{"state":"empty","note":"nothing this time"}`)
	if nodeCount(view) != 1 {
		t.Fatalf("an empty attempt must leave the existing graph intact, got %d nodes", nodeCount(view))
	}
	if g := generationOf(t, view); g["state"] != "empty" || g["note"] != "nothing this time" {
		t.Fatalf("the empty outcome must be recorded even when the graph is kept: %+v", g)
	}
}

// A "failed" outcome persists a readable reason and leaves any existing graph untouched.
func TestGenerateFailedPersistsReason(t *testing.T) {
	s := newDiagramServer(t)
	postGenerate(t, s, `{"state":"ok","nodes":[{"id":"n1","name":"Display","symbol":"TV","ports":[{"id":"n1p1","name":"HDMI"}]}],"edges":[],"sourceKey":"k1"}`)
	view := postGenerate(t, s, `{"state":"failed","note":"the server did not respond in time"}`)
	if nodeCount(view) != 1 {
		t.Fatalf("a failed attempt must leave the existing graph intact, got %d nodes", nodeCount(view))
	}
	if g := generationOf(t, view); g["state"] != "failed" || g["note"] != "the server did not respond in time" {
		t.Fatalf("the failed reason must be persisted: %+v", g)
	}
}

// The api layer owns the verdict: a caller that labels an outcome "ok" but sends no nodes is recorded as
// "empty", never as a successful generation with an empty graph (the pool/api decides, not the client).
func TestGenerateOKWithNoNodesRecordedEmpty(t *testing.T) {
	s := newDiagramServer(t)
	view := postGenerate(t, s, `{"state":"ok","nodes":[],"edges":[],"note":"actually nothing"}`)
	if g := generationOf(t, view); g["state"] != "empty" {
		t.Fatalf("an ok outcome with no nodes must be recorded empty, got %+v", g)
	}
}
