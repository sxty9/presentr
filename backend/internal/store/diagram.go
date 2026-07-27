// The connection diagram: presentr's second workflow stage. An AI investigation of the document
// pool yields the devices in the room and how they are wired — objects with a name, a symbol and a
// number of ports, joined by connections. The user can then modify the diagram by hand; the moment
// they do, it flips from the document-derived state to a manually-modified one, and the
// document-derived state stays restorable.
//
// The pool holds two graphs: Doc (the last diagram derived from the documents) and Current (what
// the user sees and edits). While Modified is false the two are identical — the diagram simply IS
// the document state. A manual edit sets Modified and diverges Current from Doc; Restore copies Doc
// back over Current and clears Modified. Regenerating from the documents replaces Doc (and Current
// too, but only while unmodified, so a manual layout is never clobbered behind the user's back).
//
// Like every pool here it is a passive store with atomic access. It exposes a snapshot read (a deep
// copy, so callers never hold a reference into the live state) and an Update that runs a caller-
// supplied transition inside one locked, atomically-persisted critical section. The transition —
// the policy — lives in the api layer; the pool only provides the atomic section and the storage.
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Port is one connector on a device (e.g. "HDMI 1", "VGA", "Power").
type Port struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Node is one device in the room: a named object with a symbol, a canvas position and its ports.
type Node struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Symbol string  `json:"symbol"` // a short glyph or 1–2 letters shown on the object
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Ports  []Port  `json:"ports"`
}

// Edge is a connection between two ports (a cable between two devices).
type Edge struct {
	ID       string `json:"id"`
	From     string `json:"from"`     // source node id
	FromPort string `json:"fromPort"` // source port id
	To       string `json:"to"`       // target node id
	ToPort   string `json:"toPort"`   // target port id
	Label    string `json:"label,omitempty"`
}

// Graph is a whole diagram: the devices and the cables between them.
type Graph struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// DiagramSnapshot is the whole on-disk state of the room's diagram.
type DiagramSnapshot struct {
	Doc       Graph  `json:"doc"`       // the document-derived baseline
	Current   Graph  `json:"current"`   // what the user sees and edits
	Modified  bool   `json:"modified"`  // true => "Manuell modifiziert"; false => the document state
	SourceKey string `json:"sourceKey"` // fingerprint of the documents the baseline was derived from
	Generated int64  `json:"generated"` // when the baseline was last derived (epoch seconds)
}

// DiagramPool is the atomic, in-memory-cached persistence for the single shared room diagram.
type DiagramPool struct {
	path string
	pool[DiagramSnapshot]
}

// OpenDiagram loads the diagram pool from path. A missing file means "no diagram yet" (empty graphs).
func OpenDiagram(path string) (*DiagramPool, error) {
	if path == "" {
		path = "/var/lib/presentr/diagram.json"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	p := &DiagramPool{path: path}
	p.st = DiagramSnapshot{Doc: emptyGraph(), Current: emptyGraph()}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return p, nil
		}
		return nil, err
	}
	var st DiagramSnapshot
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	st.Doc = normalizeGraph(st.Doc)
	st.Current = normalizeGraph(st.Current)
	p.st = st
	return p, nil
}

// Get returns a deep copy of the whole diagram state — no reader ever holds a reference into the
// live snapshot, which is what keeps the read atomic and the pool passive.
func (p *DiagramPool) Get() DiagramSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return cloneState(p.st)
}

// Update runs a caller-supplied transition over the state inside one locked, atomically-persisted
// critical section. The transition (the policy) comes from the api layer; the pool only guarantees
// the mutation and the persist happen as one indivisible unit. A persist that fails before the
// atomic rename rolls the whole state back to exactly its prior value, so a rejected write leaves no
// observable trace.
func (p *DiagramPool) Update(mutate func(*DiagramSnapshot)) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	prev := cloneState(p.st)
	mutate(&p.st)
	committed, err := p.persist(p.path)
	if err != nil && !committed {
		p.st = prev
	}
	return err
}

// ── deep-copy + normalization helpers (kept here so every slice is copied one way) ──────────

func emptyGraph() Graph { return Graph{Nodes: []Node{}, Edges: []Edge{}} }

// normalizeGraph guarantees non-nil slices so a graph always serialises as [] rather than null
// (the UI iterates nodes/edges directly).
func normalizeGraph(g Graph) Graph {
	if g.Nodes == nil {
		g.Nodes = []Node{}
	}
	if g.Edges == nil {
		g.Edges = []Edge{}
	}
	for i := range g.Nodes {
		if g.Nodes[i].Ports == nil {
			g.Nodes[i].Ports = []Port{}
		}
	}
	return g
}

// CloneGraph returns a deep copy of g (every slice fresh), so a stored graph and a copy handed out
// never share backing arrays. Exported for the api layer, which clones a freshly built graph when it
// must land in two places at once (e.g. seeding both Doc and Current on a regeneration).
func CloneGraph(g Graph) Graph {
	out := Graph{Nodes: make([]Node, len(g.Nodes)), Edges: make([]Edge, len(g.Edges))}
	for i, n := range g.Nodes {
		n.Ports = append([]Port{}, n.Ports...)
		out.Nodes[i] = n
	}
	copy(out.Edges, g.Edges)
	return out
}

func cloneState(s DiagramSnapshot) DiagramSnapshot {
	s.Doc = CloneGraph(s.Doc)
	s.Current = CloneGraph(s.Current)
	return s
}
