package render

import (
	"fmt"
	"strings"

	"github.com/zalgonoise/codegraph/internal/model"
)

// Diagrams drawn with the answers of codegraph query and the MCP server: small
// enough to read in a chat, where the client draws them.

// maxAnswerEdges caps a Flow's edges. Those touching the services in focus
// are drawn first, and a note counts the others.
const maxAnswerEdges = 80

// Flow collects services, the links between them and the keys they use, for
// a small flowchart. Links between two services are one edge per transport
// and kind (sync solid, async dashed, shared stores dotted without an arrow),
// labelled with the key, or with the number of keys when there are several.
type Flow struct {
	focus map[string]bool
	edges map[string]*flowEdge
	order []string
	seen  map[string]bool // links and key uses already added
}

type flowEdge struct {
	from, to  string // services, or "k:<transport> <key>" for a key's node
	transport string
	kind      string // sync | async | shared-store
	keys      []string
}

// NewFlow starts a flowchart about the services in focus, which are
// highlighted and whose edges are drawn first.
func NewFlow(focus ...string) *Flow {
	f := &Flow{focus: map[string]bool{}, edges: map[string]*flowEdge{}, seen: map[string]bool{}}
	for _, s := range focus {
		f.focus[s] = true
	}
	return f
}

// Link adds a link between two services; adding it again changes nothing.
func (f *Flow) Link(l model.Link) {
	id := l.From + "|" + l.To + "|" + l.Transport + "|" + l.Kind + "|" + l.Key
	if l.From == l.To || f.seen[id] {
		return
	}
	f.seen[id] = true
	f.edge(l.From, l.To, l.Transport, l.Kind, l.Key)
}

// Key adds a key (a topic, queue, table, route or gRPC service) as a node of
// its own, between the services that send it, receive it and use it (stores).
func (f *Flow) Key(transport, key string, senders, receivers, users []string) {
	node := "k:" + transport + " " + key
	kind := model.DefaultKind(transport, "") // http, grpc: sync; messaging: async
	add := func(from, to, kind string) {
		if id := from + "|" + to + "|" + kind; !f.seen[id] {
			f.seen[id] = true
			f.edge(from, to, transport, kind, "")
		}
	}
	for _, s := range senders {
		add(s, node, kind)
	}
	for _, s := range receivers {
		add(node, s, kind)
	}
	for _, s := range users {
		add(s, node, "shared-store")
	}
}

func (f *Flow) edge(from, to, transport, kind, key string) {
	k := from + "|" + to + "|" + transport + "|" + kind
	e := f.edges[k]
	if e == nil {
		e = &flowEdge{from: from, to: to, transport: transport, kind: kind}
		f.edges[k] = e
		f.order = append(f.order, k)
	}
	if key != "" {
		e.keys = append(e.keys, key)
	}
}

// Mermaid renders the flowchart, or "" when there is nothing to draw.
func (f *Flow) Mermaid() string {
	if len(f.order) == 0 {
		return ""
	}
	var first, rest []*flowEdge
	for _, k := range f.order {
		e := f.edges[k]
		if f.focus[e.from] || f.focus[e.to] {
			first = append(first, e)
		} else {
			rest = append(rest, e)
		}
	}
	edges := append(first, rest...)
	omitted := 0
	if len(edges) > maxAnswerEdges {
		edges, omitted = edges[:maxAnswerEdges], len(edges)-maxAnswerEdges
	}

	g := NewGraph("answer", "LR")
	node := func(name string) string {
		if key, isKey := strings.CutPrefix(name, "k:"); isKey {
			transport, label, _ := strings.Cut(key, " ")
			class, shape := "outbound", ShapeStadium
			if transport == "sql" || transport == "redis" {
				class, shape = "store", ShapeCylinder
			}
			g.AddNode(name, strings.ToUpper(transport)+"\n"+clip(label), "", class, shape)
			return name
		}
		class := "service"
		if f.focus[name] {
			class = "focus"
		}
		g.AddNode("s:"+name, name, "", class, ShapeRounded)
		return "s:" + name
	}
	for _, e := range edges {
		from, to := node(e.from), node(e.to)
		style := StyleSolid
		switch e.kind {
		case model.Async:
			style = StyleDashed
		case "shared-store":
			style = StyleShared
		}
		g.AddEdge(from, to, e.label(), style)
	}
	if omitted > 0 {
		g.AddNode("note", fmt.Sprintf("%d more connections not drawn", omitted), "", "note", ShapeBox)
	}
	return Mermaid(g)
}

func (e *flowEdge) label() string {
	if strings.HasPrefix(e.from, "k:") || strings.HasPrefix(e.to, "k:") {
		return "" // the key's node names it
	}
	label := e.transport
	if e.kind == "shared-store" {
		label = "shared " + label
	}
	switch n := len(e.keys); {
	case n == 1:
		label += " " + clip(e.keys[0])
	case n > 1:
		label += fmt.Sprintf(" ×%d", n)
	}
	return label
}

// clip shortens a label to keep diagrams readable.
func clip(s string) string {
	if r := []rune(s); len(r) > 60 {
		return string(r[:59]) + "…"
	}
	return s
}

// MaxSequenceSteps is the most steps SequenceDiagram draws.
const MaxSequenceSteps = maxSequenceHops

// SequenceDiagram is Sequence(sj), or "" when the journey is too large to
// draw: over maxSequenceHops steps or maxMermaidText characters, as in
// system.md.
func SequenceDiagram(sj model.SystemJourney) string {
	if steps, _ := hopStats(sj.Hops, map[string]bool{}); steps > maxSequenceHops {
		return ""
	}
	if seq := Sequence(sj); len(seq) <= maxMermaidText {
		return seq
	}
	return ""
}
