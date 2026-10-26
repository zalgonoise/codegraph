package render

import (
	"fmt"
	"sort"
	"strings"
)

var mmdEscaper = strings.NewReplacer(`"`, "#quot;", "<", "#lt;", ">", "#gt;", "\n", "<br/>")

// Mermaid renders g as a Mermaid flowchart.
func Mermaid(g *Graph) string {
	var b strings.Builder
	dir := g.Direction
	if dir == "" {
		dir = "LR"
	}
	fmt.Fprintf(&b, "flowchart %s\n", dir)
	for _, gr := range g.Groups {
		nodes := g.nodesIn(gr.ID)
		if len(nodes) == 0 {
			continue
		}
		fmt.Fprintf(&b, "  subgraph %s[\"%s\"]\n", gr.ID, mmdEscaper.Replace(gr.Label))
		for _, n := range nodes {
			fmt.Fprintf(&b, "    %s\n", mmdNode(n))
		}
		b.WriteString("  end\n")
	}
	for _, n := range g.nodesIn("") {
		fmt.Fprintf(&b, "  %s\n", mmdNode(n))
	}
	for _, e := range g.Edges {
		arrow := "-->"
		switch e.Style {
		case StyleDashed:
			arrow = "-.->"
		case StyleThick:
			arrow = "==>"
		case StyleShared:
			arrow = "-.-"
		}
		if e.Label == "" {
			fmt.Fprintf(&b, "  %s %s %s\n", e.From, arrow, e.To)
		} else {
			fmt.Fprintf(&b, "  %s %s|\"%s\"| %s\n", e.From, arrow, mmdEscaper.Replace(e.Label), e.To)
		}
	}
	byClass := map[string][]string{}
	for _, n := range g.Nodes {
		if n.Class != "" {
			byClass[n.Class] = append(byClass[n.Class], n.ID)
		}
	}
	classes := make([]string, 0, len(byClass))
	for c := range byClass {
		classes = append(classes, c)
	}
	sort.Strings(classes)
	for _, c := range classes {
		col, ok := classColors[c]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "  classDef %s fill:%s,stroke:%s,color:#212121\n", c, col[0], col[1])
		fmt.Fprintf(&b, "  class %s %s\n", strings.Join(byClass[c], ","), c)
	}
	return b.String()
}

func mmdNode(n *Node) string {
	l := mmdEscaper.Replace(n.Label)
	switch n.Shape {
	case ShapeRounded:
		return fmt.Sprintf(`%s("%s")`, n.ID, l)
	case ShapeStadium:
		return fmt.Sprintf(`%s(["%s"])`, n.ID, l)
	case ShapeCylinder:
		return fmt.Sprintf(`%s[("%s")]`, n.ID, l)
	case ShapeHexagon:
		return fmt.Sprintf(`%s{{"%s"}}`, n.ID, l)
	case ShapeSubroutine:
		return fmt.Sprintf(`%s[["%s"]]`, n.ID, l)
	}
	return fmt.Sprintf(`%s["%s"]`, n.ID, l)
}

var dotEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// DOT renders g for Graphviz (`dot -Tsvg service.dot -o service.svg`).
func DOT(g *Graph) string {
	var b strings.Builder
	dir := g.Direction
	if dir == "" {
		dir = "LR"
	}
	b.WriteString("digraph codegraph {\n")
	fmt.Fprintf(&b, "  rankdir=%s;\n  compound=true;\n  newrank=true;\n", dir)
	b.WriteString("  node [fontname=\"Helvetica\", fontsize=10, style=filled];\n")
	b.WriteString("  edge [fontname=\"Helvetica\", fontsize=9, color=\"#607d8b\"];\n")
	for _, gr := range g.Groups {
		nodes := g.nodesIn(gr.ID)
		if len(nodes) == 0 {
			continue
		}
		fmt.Fprintf(&b, "  subgraph cluster_%s {\n    label=\"%s\";\n    style=\"rounded,dashed\";\n    color=\"#b0bec5\";\n", gr.ID, dotEscaper.Replace(gr.Label))
		for _, n := range nodes {
			fmt.Fprintf(&b, "    %s\n", dotNode(n))
		}
		b.WriteString("  }\n")
	}
	for _, n := range g.nodesIn("") {
		fmt.Fprintf(&b, "  %s\n", dotNode(n))
	}
	for _, e := range g.Edges {
		attrs := []string{}
		if e.Label != "" {
			attrs = append(attrs, fmt.Sprintf("label=\"%s\"", dotEscaper.Replace(e.Label)))
		}
		switch e.Style {
		case StyleDashed:
			attrs = append(attrs, "style=dashed")
		case StyleThick:
			attrs = append(attrs, "penwidth=2")
		case StyleShared:
			attrs = append(attrs, "style=dotted", "dir=none")
		}
		fmt.Fprintf(&b, "  %s -> %s [%s];\n", e.From, e.To, strings.Join(attrs, ", "))
	}
	b.WriteString("}\n")
	return b.String()
}

func dotNode(n *Node) string {
	shape, style := "box", "filled"
	switch n.Shape {
	case ShapeRounded:
		style = "rounded,filled"
	case ShapeStadium:
		style = "rounded,filled"
	case ShapeCylinder:
		shape = "cylinder"
	case ShapeHexagon:
		shape = "hexagon"
	case ShapeSubroutine:
		shape = "component"
	}
	fill, stroke := "#ffffff", "#607d8b"
	if col, ok := classColors[n.Class]; ok {
		fill, stroke = col[0], col[1]
	}
	return fmt.Sprintf("%s [label=\"%s\", shape=%s, style=\"%s\", fillcolor=\"%s\", color=\"%s\"];",
		n.ID, dotEscaper.Replace(n.Label), shape, style, fill, stroke)
}
