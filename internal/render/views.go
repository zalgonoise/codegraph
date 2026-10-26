package render

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zalgonoise/codegraph/internal/model"
)

var layerOrder = []string{"entry", "transport", "messaging", "domain", "adapters", "persistence", "config", "other"}

var layerTitles = map[string]string{
	"entry": "Entrypoint", "transport": "Transport", "messaging": "Messaging", "domain": "Domain",
	"adapters": "Adapters / clients", "persistence": "Persistence", "config": "Configuration", "other": "Other",
}

// maxServiceEdges keeps service diagrams under Mermaid's default limit of 500
// edges (large services with their SDKs analysed exceed it).
const maxServiceEdges = 450

// ServiceOptions tune the service view.
type ServiceOptions struct {
	Libraries bool // draw third-party modules
	Internal  bool // draw a library's internal API (endpoints of role internal), left out by default
}

// InternalAPI reports whether ep is a symbol of a library's internal
// packages: hidden unless asked for.
func InternalAPI(ep model.Endpoint) bool {
	return ep.Transport == model.APITransport && ep.Role == model.APIInternalRole
}

// HasInternalAPI reports whether a manifest has internal API endpoints.
func HasInternalAPI(m *model.Manifest) bool {
	for _, ep := range m.Endpoints {
		if InternalAPI(ep) {
			return true
		}
	}
	return false
}

// Service builds the architecture view of one service: inbound endpoints on
// the left, components grouped by layer, outbound endpoints and stores on the
// right. Dashed component edges are calls through an interface.
func Service(m *model.Manifest, opts ServiceOptions) *Graph {
	g := NewGraph(m.Service, "LR")

	g.AddGroup("inbound", "Inbound")
	used := map[string]bool{}
	for _, c := range m.Components {
		used[c.Layer] = true
	}
	for _, l := range layerOrder {
		if used[l] {
			g.AddGroup("layer:"+l, layerTitles[l])
		}
	}
	for _, c := range m.Components { // custom layers, if any
		g.AddGroup("layer:"+c.Layer, c.Layer)
	}
	g.AddGroup("outbound", "Outbound")
	g.AddGroup("infra", "Data stores")
	if opts.Libraries {
		g.AddGroup("libs", "Libraries")
	}

	for _, c := range m.Components {
		detail := fmt.Sprintf("%d funcs", c.Funcs)
		if n := len(c.Interfaces); n > 0 {
			detail += fmt.Sprintf(" · %d ifaces", n)
		}
		label, shape, class := c.Name+"\n"+detail, ShapeRounded, "component"
		if c.Package == m.Entry {
			label, shape, class = m.Service+"\n"+c.Name, ShapeHexagon, "entry"
		}
		g.AddNode("c:"+c.Package, label, "layer:"+c.Layer, class, shape)
	}
	calls, omitted := diagramCalls(m, opts)
	for _, call := range calls {
		style := StyleSolid
		if call.ViaInterface {
			style = StyleDashed
		}
		g.AddEdge("c:"+call.From, "c:"+call.To, callLabel(call), style)
	}
	if omitted > 0 {
		g.AddNode("note:omitted", fmt.Sprintf("%d of %d component calls not drawn\n(see the tables in report.md)", omitted, len(m.Calls)),
			"", "note", ShapeBox)
	}

	for _, ep := range m.Endpoints {
		if InternalAPI(ep) && !opts.Internal {
			continue
		}
		group, class, shape := "outbound", "outbound", ShapeStadium
		switch {
		case ep.Direction == model.Inbound:
			group, class = "inbound", "inbound"
		case ep.Transport == "sql" || ep.Role == "connect":
			group, class, shape = "infra", "store", ShapeCylinder
		}
		key := "e:" + ep.ID
		g.AddNode(key, endpointLabel(ep), group, class, shape)
		comp := "c:" + ep.Component
		if !g.HasNode(comp) {
			comp = "c:" + m.Entry
		}
		if ep.Direction == model.Inbound {
			g.AddEdge(key, comp, shortLabel(ep.Handler), StyleSolid)
		} else {
			g.AddEdge(comp, key, "", StyleSolid)
		}
	}

	if opts.Libraries {
		for _, lib := range m.Libraries {
			key := "l:" + lib.Module
			g.AddNode(key, lib.Module, "libs", "library", ShapeSubroutine)
			for _, u := range lib.UsedBy {
				g.AddEdge("c:"+u, key, "", StyleDashed)
			}
		}
	}
	return g
}

// System builds the view of linked services. Thick edges are synchronous
// calls, dashed edges asynchronous messaging; shared stores are cylinders.
func System(s *model.System) *Graph {
	g := NewGraph("system", "LR")
	// Mains of one module (orders-grpc, orders-subscribers, ...) are grouped as their repository.
	perModule := map[string]int{}
	for _, svc := range s.Services {
		perModule[svc.Module]++
	}
	for _, svc := range s.Services {
		if svc.Module != "" && perModule[svc.Module] > 1 {
			g.AddGroup("mod:"+svc.Module, svc.Module)
		}
	}
	g.AddGroup("stores", "Shared stores (same table name)")
	g.AddGroup("external", "External / unmatched")

	for _, svc := range s.Services {
		label := svc.Name
		if t := tagLine(svc.Tags); t != "" {
			label += "\n" + t
		}
		g.AddNode("s:"+svc.Name, label, "mod:"+svc.Module, "service", ShapeRounded)
	}
	for _, l := range s.Links {
		label := l.Transport + ": " + l.Key
		if l.Confidence != "" && l.Confidence != "exact" {
			label += " (" + l.Confidence + ")"
		}
		switch l.Kind {
		case "shared-store":
			key := "st:" + l.Transport + ":" + l.Key
			g.AddNode(key, strings.ToUpper(l.Transport)+"\n"+l.Key, "stores", "store", ShapeCylinder)
			g.AddEdge("s:"+l.From, key, "", StyleDashed)
			g.AddEdge("s:"+l.To, key, "", StyleDashed)
		case "async":
			g.AddEdge("s:"+l.From, "s:"+l.To, label, StyleDashed)
		default:
			g.AddEdge("s:"+l.From, "s:"+l.To, label, StyleThick)
		}
	}
	for _, d := range s.Dangling {
		if d.Reason == model.DanglingSameService || d.Reason == model.DanglingBroker {
			continue // not a connection to anything outside the service
		}
		key := "x:" + d.Transport + ":" + d.Key
		g.AddNode(key, strings.ToUpper(d.Transport)+"\n"+d.Key, "external", "external", ShapeStadium)
		if d.Direction == model.Inbound {
			g.AddEdge(key, "s:"+d.Service, d.Role, StyleDashed)
		} else {
			g.AddEdge("s:"+d.Service, key, d.Role, StyleDashed)
		}
	}
	return g
}

// diagramCalls returns the component calls to draw within maxServiceEdges:
// endpoint (and library) edges always fit; then calls by weight, with the
// entry component's wiring calls (main constructing everything) last.
func diagramCalls(m *model.Manifest, opts ServiceOptions) ([]model.Call, int) {
	budget := maxServiceEdges - len(m.Endpoints)
	if opts.Libraries {
		for _, l := range m.Libraries {
			budget -= len(l.UsedBy)
		}
	}
	if budget < 0 {
		budget = 0
	}
	if len(m.Calls) <= budget {
		return m.Calls, 0
	}
	calls := append([]model.Call(nil), m.Calls...)
	sort.SliceStable(calls, func(i, j int) bool {
		ei, ej := calls[i].From == m.Entry, calls[j].From == m.Entry
		if ei != ej {
			return !ei
		}
		return calls[i].Count > calls[j].Count
	})
	kept := calls[:budget]
	sort.SliceStable(kept, func(i, j int) bool { // back to the manifest's order
		if kept[i].From != kept[j].From {
			return kept[i].From < kept[j].From
		}
		return kept[i].To < kept[j].To
	})
	return kept, len(m.Calls) - budget
}

func endpointLabel(ep model.Endpoint) string {
	keys, more := ep.Keys, ""
	if len(keys) > 3 {
		keys, more = keys[:3], fmt.Sprintf("\n(+%d more)", len(ep.Keys)-3)
	}
	label := strings.ToUpper(ep.Transport) + " " + ep.Role + "\n" + strings.Join(keys, "\n") + more
	if ep.Message != "" {
		label += "\nmsg: " + ep.Message
	}
	if ep.Count > 1 {
		label += fmt.Sprintf("\n×%d sites", ep.Count)
	}
	if ep.Source == "directive" {
		label += "\n(directive)"
	}
	return label
}

func callLabel(c model.Call) string {
	syms := make([]string, 0, len(c.Symbols))
	for _, s := range c.Symbols {
		syms = append(syms, shortLabel(s))
	}
	if len(syms) <= 2 {
		return strings.Join(syms, "\n")
	}
	return fmt.Sprintf("%s\n%s\n+%d more", syms[0], syms[1], len(syms)-2)
}

// shortLabel keeps the last "Type.Method" part of a symbol label.
func shortLabel(s string) string {
	s = strings.NewReplacer("(", "", ")", "", "*", "").Replace(s)
	parts := strings.Split(s, ".")
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	return strings.Join(parts, ".")
}

func tagLine(tags map[string]string) string {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+tags[k])
	}
	return strings.Join(parts, ", ")
}
