package store

import (
	"path/filepath"
	"testing"
)

func openDiagram(t *testing.T) (*DiagramPool, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data", "diagram.json")
	p, err := OpenDiagram(path)
	if err != nil {
		t.Fatal(err)
	}
	return p, path
}

func sampleGraph(sym string) Graph {
	return Graph{
		Nodes: []Node{{ID: "n1", Name: "Projector", Symbol: sym, X: 10, Y: 20, Ports: []Port{{ID: "p1", Name: "HDMI"}}}},
		Edges: []Edge{},
	}
}

// A fresh pool reports the document state with empty, non-nil graphs (so they serialise as []).
func TestDiagramEmptyDefaults(t *testing.T) {
	p, _ := openDiagram(t)
	st := p.Get()
	if st.Modified {
		t.Fatal("a fresh diagram must be in the document state")
	}
	if st.Current.Nodes == nil || st.Current.Edges == nil || st.Doc.Nodes == nil {
		t.Fatalf("graphs must be non-nil (serialise as []), got: %+v", st)
	}
}

// The modify → restore cycle: a manual edit diverges Current from Doc and sets Modified; restore
// brings Doc back over Current and clears the flag.
func TestDiagramModifyThenRestore(t *testing.T) {
	p, path := openDiagram(t)

	// Install a document-derived baseline (as generate does): Doc + Current both set, unmodified.
	base := sampleGraph("P")
	if err := p.Update(func(st *DiagramSnapshot) {
		st.Doc = base
		st.Current = CloneGraph(base)
		st.SourceKey = "k1"
	}); err != nil {
		t.Fatal(err)
	}

	// Manually modify Current only.
	if err := p.Update(func(st *DiagramSnapshot) {
		st.Current = Graph{Nodes: []Node{{ID: "n2", Name: "Laptop", Symbol: "L", Ports: []Port{}}}, Edges: []Edge{}}
		st.Modified = true
	}); err != nil {
		t.Fatal(err)
	}
	st := p.Get()
	if !st.Modified || len(st.Current.Nodes) != 1 || st.Current.Nodes[0].Name != "Laptop" {
		t.Fatalf("manual edit did not take: %+v", st.Current)
	}
	if len(st.Doc.Nodes) != 1 || st.Doc.Nodes[0].Name != "Projector" {
		t.Fatalf("manual edit leaked into the document baseline: %+v", st.Doc)
	}

	// Restore: Current becomes Doc again, flag clears.
	if err := p.Update(func(st *DiagramSnapshot) {
		st.Current = CloneGraph(st.Doc)
		st.Modified = false
	}); err != nil {
		t.Fatal(err)
	}
	st = p.Get()
	if st.Modified || len(st.Current.Nodes) != 1 || st.Current.Nodes[0].Name != "Projector" {
		t.Fatalf("restore did not return to the document state: %+v", st)
	}

	// It survives a reload.
	p2, err := OpenDiagram(path)
	if err != nil {
		t.Fatal(err)
	}
	if st := p2.Get(); st.Modified || st.Current.Nodes[0].Name != "Projector" || st.SourceKey != "k1" {
		t.Fatalf("diagram state did not survive reload: %+v", st)
	}
}

// Get hands back a deep copy: mutating it must not disturb the pool (the node's Ports slice too).
func TestDiagramGetIsDeepCopied(t *testing.T) {
	p, _ := openDiagram(t)
	if err := p.Update(func(st *DiagramSnapshot) { st.Current = sampleGraph("P") }); err != nil {
		t.Fatal(err)
	}
	got := p.Get()
	got.Current.Nodes[0].Name = "tampered"
	got.Current.Nodes[0].Ports[0].Name = "tampered"
	again := p.Get()
	if again.Current.Nodes[0].Name != "Projector" || again.Current.Nodes[0].Ports[0].Name != "HDMI" {
		t.Fatalf("Get handed out a reference into the live snapshot: %+v", again.Current.Nodes[0])
	}
}
