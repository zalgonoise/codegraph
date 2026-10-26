// Package render turns manifests and linked systems into diagrams (Mermaid,
// Graphviz DOT) and Markdown reports. Views are first built as a small
// format-neutral Graph so every output format shows the same thing.
package render

import "fmt"

// Shape of a node.
type Shape int

const (
	ShapeBox Shape = iota
	ShapeRounded
	ShapeStadium
	ShapeCylinder
	ShapeHexagon
	ShapeSubroutine
)

// Style of an edge.
type Style int

const (
	StyleSolid Style = iota
	StyleDashed
	StyleThick
	StyleShared // dotted, without an arrow: two services sharing a store
)

// Node is a diagram node; Label may contain "\n".
type Node struct {
	ID, Label, Group, Class string
	Shape                   Shape
}

// Group is a cluster/subgraph.
type Group struct{ ID, Label string }

// Edge connects two node IDs.
type Edge struct {
	From, To, Label string
	Style           Style
}

// Graph is addressed by caller-chosen keys; IDs are generated.
type Graph struct {
	Title     string
	Direction string
	Groups    []*Group
	Nodes     []*Node
	Edges     []*Edge

	nodes  map[string]*Node
	groups map[string]*Group
	edges  map[string]bool
}

// NewGraph creates an empty graph ("LR" or "TB" direction).
func NewGraph(title, direction string) *Graph {
	return &Graph{
		Title: title, Direction: direction,
		nodes: map[string]*Node{}, groups: map[string]*Group{}, edges: map[string]bool{},
	}
}

// AddGroup declares a group once; declaration order is rendering order.
func (g *Graph) AddGroup(key, label string) {
	if _, ok := g.groups[key]; ok {
		return
	}
	gr := &Group{ID: fmt.Sprintf("g%d", len(g.Groups)), Label: label}
	g.groups[key] = gr
	g.Groups = append(g.Groups, gr)
}

// AddNode adds a node once (by key) to an optional, previously declared group.
func (g *Graph) AddNode(key, label, groupKey, class string, shape Shape) {
	if _, ok := g.nodes[key]; ok {
		return
	}
	n := &Node{ID: fmt.Sprintf("n%d", len(g.Nodes)), Label: label, Class: class, Shape: shape}
	if gr, ok := g.groups[groupKey]; ok {
		n.Group = gr.ID
	}
	g.nodes[key] = n
	g.Nodes = append(g.Nodes, n)
}

// HasNode reports whether key was added.
func (g *Graph) HasNode(key string) bool {
	_, ok := g.nodes[key]
	return ok
}

// AddEdge connects two existing nodes; duplicates and self-loops are dropped.
func (g *Graph) AddEdge(fromKey, toKey, label string, style Style) {
	from, ok1 := g.nodes[fromKey]
	to, ok2 := g.nodes[toKey]
	if !ok1 || !ok2 || from == to {
		return
	}
	k := from.ID + ">" + to.ID + ">" + label
	if g.edges[k] {
		return
	}
	g.edges[k] = true
	g.Edges = append(g.Edges, &Edge{From: from.ID, To: to.ID, Label: label, Style: style})
}

func (g *Graph) nodesIn(groupID string) []*Node {
	var out []*Node
	for _, n := range g.Nodes {
		if n.Group == groupID {
			out = append(out, n)
		}
	}
	return out
}

// classColors maps node classes to fill/stroke colours for all renderers.
var classColors = map[string][2]string{
	"entry":     {"#e8eaff", "#3f51b5"},
	"component": {"#ffffff", "#607d8b"},
	"inbound":   {"#e6f4ea", "#2e7d32"},
	"outbound":  {"#fff3e0", "#ef6c00"},
	"store":     {"#eceff1", "#455a64"},
	"library":   {"#f3e5f5", "#8e24aa"},
	"service":   {"#e3f2fd", "#1565c0"},
	"external":  {"#fafafa", "#9e9e9e"},
	"note":      {"#fffde7", "#f9a825"},
	"focus":     {"#e8eaff", "#3f51b5"}, // the services an answer is about
}
