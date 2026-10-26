package render

import (
	"fmt"
	"strings"

	"github.com/zalgonoise/codegraph/internal/model"
	"github.com/zalgonoise/codegraph/internal/symbols"
)

// Journey builds the view of one journey: its entrypoint, the components it
// runs through (dashed edges are calls through an interface) and the outbound
// endpoints it reaches (dashed when async or started in a goroutine).
func Journey(m *model.Manifest, j model.Journey) *Graph {
	g := NewGraph(j.Key, "LR")
	eps := endpointsByID(m)
	names := map[string]string{}
	for _, c := range m.Components {
		names[c.Package] = c.Name
	}
	name := func(pkg string) string {
		if n := names[pkg]; n != "" {
			return n
		}
		return symbols.Short(pkg, m.Module)
	}

	role := eps[j.Entrypoint].Role
	g.AddNode("entry", strings.TrimSpace(strings.ToUpper(j.Transport)+" "+role)+"\n"+j.Key, "", "inbound", ShapeStadium)
	for _, pkg := range j.Components {
		class := "component"
		if pkg == m.Entry {
			class = "entry"
		}
		g.AddNode("c:"+pkg, name(pkg), "", class, ShapeRounded)
	}
	g.AddEdge("entry", "c:"+j.Component, shortLabel(j.Handler), StyleSolid)
	for _, call := range j.Calls {
		style := StyleSolid
		if call.ViaInterface {
			style = StyleDashed
		}
		g.AddEdge("c:"+call.From, "c:"+call.To, callLabel(call), style)
	}
	for _, o := range j.Outbound {
		ep, ok := eps[o.Endpoint]
		if !ok {
			continue
		}
		class, shape := "outbound", ShapeStadium
		if ep.Transport == "sql" || ep.Role == "connect" {
			class, shape = "store", ShapeCylinder
		}
		ep.Count = 1 // the merged site count is service-wide, not per journey
		g.AddNode("e:"+ep.ID, endpointLabel(ep), "", class, shape)
		label, style := "", StyleSolid
		switch {
		case o.Background:
			label, style = "go", StyleDashed
		case o.Kind == model.Async:
			label, style = "async", StyleDashed
		}
		g.AddEdge("c:"+ep.Component, "e:"+ep.ID, label, style)
	}
	return g
}

func endpointsByID(m *model.Manifest) map[string]model.Endpoint {
	out := make(map[string]model.Endpoint, len(m.Endpoints))
	for _, ep := range m.Endpoints {
		out[ep.ID] = ep
	}
	return out
}

func journeysSection(b *strings.Builder, m *model.Manifest) {
	b.WriteString("\n## Journeys\n\n")
	if len(m.Journeys) == 0 {
		b.WriteString("_none_\n")
		return
	}
	b.WriteString("Every inbound endpoint is the start of a journey: what its handler reaches in module code and " +
		"which outbound endpoints (other services, brokers, stores) it ends up using. Dashed component edges are " +
		"calls through an interface; dashed outbound edges are async or run in a goroutine.\n\n")

	eps := endpointsByID(m)
	var shown []model.Journey
	for _, j := range m.Journeys {
		if !InternalAPI(eps[j.Entrypoint]) { // the internal API has its own table
			shown = append(shown, j)
		}
	}
	var rows [][]string
	for _, j := range shown {
		var comps, outs []string
		for _, c := range j.Components {
			comps = append(comps, symbols.Short(c, m.Module))
		}
		for _, o := range j.Outbound {
			outs = append(outs, outboundLine(eps[o.Endpoint], o))
		}
		rows = append(rows, []string{code(j.Transport + " " + j.Key), j.Handler, fmt.Sprint(j.Funcs),
			strings.Join(comps, "\n"), strings.Join(outs, "\n")})
	}
	table(b, []string{"Entrypoint", "Handler", "Funcs", "Components", "Outbound"}, rows)

	for _, j := range shown {
		fmt.Fprintf(b, "\n### `%s %s`\n\n", j.Transport, j.Key)
		roots := make([]string, 0, len(j.Roots))
		for _, r := range j.Roots {
			roots = append(roots, fmt.Sprintf("`%s` (%s)", r.Function, r.Position))
		}
		fmt.Fprintf(b, "Handler %s", strings.Join(roots, ", "))
		if len(j.Wrapped) > 0 {
			wrapped := make([]string, 0, len(j.Wrapped))
			for _, w := range j.Wrapped {
				wrapped = append(wrapped, fmt.Sprintf("`%s` (%s)", w.Function, w.Position))
			}
			fmt.Fprintf(b, " wrapping %s", strings.Join(wrapped, ", "))
		}
		fmt.Fprintf(b, " · %d functions", j.Funcs)
		if len(j.Async) > 0 {
			fmt.Fprintf(b, " · goroutines: %s", code(strings.Join(j.Async, "\n")))
		}
		if j.Truncated {
			b.WriteString(" · _truncated at -journey-depth_")
		}
		b.WriteString("\n\n```mermaid\n")
		b.WriteString(Mermaid(Journey(m, j)))
		b.WriteString("```\n")
		if len(j.Functions) > 0 {
			fmt.Fprintf(b, "\n<details><summary>Functions reached (%d)</summary>\n\n", len(j.Functions))
			for _, f := range j.Functions {
				fmt.Fprintf(b, "- `%s`\n", f)
			}
			b.WriteString("\n</details>\n")
		}
		if len(j.Uses) > 0 {
			fmt.Fprintf(b, "\n<details><summary>Library functions and methods it calls (%d)</summary>\n\n", len(j.Uses))
			var rows [][]string
			for _, u := range j.Uses {
				rows = append(rows, []string{code(u.Symbol), code(u.Module), fmt.Sprint(u.Calls), u.Position})
			}
			table(b, []string{"Symbol", "Module", "Call sites", "First call"}, rows)
			b.WriteString("\n</details>\n")
		}
	}

	var missing []string
	covered := map[string]bool{}
	for _, j := range m.Journeys {
		covered[j.Entrypoint] = true
	}
	for _, ep := range m.Endpoints {
		if ep.Direction == model.Inbound && (ep.Role == "server" || ep.Role == "consumer") && !covered[ep.ID] {
			handler := ep.Handler
			if handler == "" {
				handler = "unresolved handler"
			}
			missing = append(missing, fmt.Sprintf("- `%s %s` (%s)", ep.Transport, strings.Join(ep.Keys, ", "), handler))
		}
	}
	if len(missing) > 0 {
		b.WriteString("\nInbound endpoints without a journey (handler outside the module, or not resolved):\n\n")
		b.WriteString(strings.Join(missing, "\n") + "\n")
	}
}

func outboundLine(ep model.Endpoint, o model.JourneyOutbound) string {
	s := fmt.Sprintf("%s %s %s (%s)", ep.Transport, ep.Role, strings.Join(ep.Keys, ", "), o.Kind)
	if o.Background {
		s += " in goroutine"
	}
	return s
}

// ---- system journeys ------------------------------------------------------------

// Sequence renders a system journey as a Mermaid sequence diagram: sync calls
// are "->>" with activations, async ones "-)", and stores or endpoints
// outside the analysed services are their own participants.
func Sequence(sj model.SystemJourney) string {
	s := &sequence{ids: map[string]string{}}
	caller := s.participant("caller", "caller", true)
	svc := s.participant("s:"+sj.Service, sj.Service, false)
	var body strings.Builder
	arrow := "->>+"
	if sj.Kind == model.Async {
		arrow = "-)"
	}
	fmt.Fprintf(&body, "  %s%s%s: %s\n", caller, arrow, svc, seqText(sj.Transport+" "+sj.Key))
	fmt.Fprintf(&body, "  Note over %s: %s\n", svc, seqText(sj.Handler))
	s.hops(&body, sj.Hops)
	if sj.Kind != model.Async {
		fmt.Fprintf(&body, "  %s-->>-%s: response\n", svc, caller)
	}
	return "sequenceDiagram\n" + s.decl.String() + body.String()
}

type sequence struct {
	ids  map[string]string
	decl strings.Builder
}

func (s *sequence) participant(key, label string, actor bool) string {
	if id, ok := s.ids[key]; ok {
		return id
	}
	id := fmt.Sprintf("p%d", len(s.ids))
	s.ids[key] = id
	kind := "participant"
	if actor {
		kind = "actor"
	}
	fmt.Fprintf(&s.decl, "  %s %s as %s\n", kind, id, seqAlias(label))
	return id
}

func (s *sequence) hops(b *strings.Builder, hops []model.Hop) {
	seen := map[string]bool{} // identical external calls from one journey
	for _, h := range hops {
		from := s.participant("s:"+h.From, h.From, false)
		if h.External {
			target, msg := externalTarget(h)
			to := s.participant("x:"+h.Transport+":"+target, strings.ToUpper(h.Transport)+" "+target, false)
			arrow := "->>"
			if h.Kind == model.Async || h.Background {
				arrow = "-)"
			}
			if h.Background {
				msg += " (goroutine)"
			}
			line := fmt.Sprintf("  %s%s%s: %s\n", from, arrow, to, seqText(msg))
			if !seen[line] {
				seen[line] = true
				b.WriteString(line)
			}
			continue
		}
		msg := h.Transport + " " + h.Key
		if h.Background {
			msg += " (goroutine)"
		}
		to := s.participant("s:"+h.To, h.To, false)
		sync := h.Kind != model.Async && !h.Background
		arrow := "-)"
		if sync {
			arrow = "->>+"
		}
		fmt.Fprintf(b, "  %s%s%s: %s\n", from, arrow, to, seqText(msg))
		switch {
		case h.Cycle:
			fmt.Fprintf(b, "  Note over %s: %s (cycle, already on this path)\n", to, seqText(h.Handler))
		case h.Repeated:
			fmt.Fprintf(b, "  Note over %s: %s (continues as shown above)\n", to, seqText(h.Handler))
		case h.Truncated:
			fmt.Fprintf(b, "  Note over %s: %s (not followed, too deep)\n", to, seqText(h.Handler))
		case h.Handler != "":
			fmt.Fprintf(b, "  Note over %s: %s\n", to, seqText(h.Handler))
		}
		s.hops(b, h.Next)
		if sync {
			fmt.Fprintf(b, "  %s-->>-%s: response\n", to, from)
		}
	}
}

// externalTarget names the participant of an external hop and the message
// sent to it, so calls to the same dependency share one lifeline: a gRPC
// service (method as message), an HTTP host (route as message), or a
// table / topic / subject (role as message).
func externalTarget(h model.Hop) (target, msg string) {
	switch h.Transport {
	case "grpc":
		if svc, method, ok := strings.Cut(h.Key, "/"); ok {
			return svc, method
		}
	case "http":
		if m, rest, ok := strings.Cut(h.Key, " "); ok && strings.Contains(rest, "://") {
			u := rest[strings.Index(rest, "://")+3:]
			host, path, _ := strings.Cut(u, "/")
			return host, m + " /" + path
		}
	}
	if h.Key == model.Unknown && h.Message != "" {
		return h.Message, h.Role
	}
	return h.Key, h.Role
}

var (
	seqTextEscaper  = strings.NewReplacer(";", "#59;", "#", "#35;", "\n", " ")
	seqAliasEscaper = strings.NewReplacer(";", ",", "#", "", "\n", " ")
)

func seqText(s string) string { return seqTextEscaper.Replace(s) }

func seqAlias(s string) string {
	if len(s) > 48 {
		s = s[:47] + "…"
	}
	return seqAliasEscaper.Replace(s)
}

func systemJourneysSection(b *strings.Builder, s *model.System) {
	b.WriteString("\n## Journeys across services\n\n")
	if len(s.Journeys) == 0 {
		b.WriteString("_none_\n")
		return
	}
	b.WriteString("Each journey starts at an inbound endpoint and follows its outbound calls into the services they " +
		"link to (`->>` sync, `-)` async), with the stores and external endpoints it uses on the way.\n")
	for _, sj := range s.Journeys {
		fmt.Fprintf(b, "\n### `%s` · `%s %s`\n\n", sj.Service, sj.Transport, sj.Key)
		seq := SequenceDiagram(sj)
		if seq == "" {
			steps, services := hopStats(sj.Hops, map[string]bool{})
			fmt.Fprintf(b, "%d steps across %d other services (%s): too many to list and draw here. The explorer "+
				"(`system.html`) expands it step by step. The first steps:\n\n", steps, len(services), strings.Join(services, ", "))
			writeHops(b, sj.Hops, 0, 1)
			continue
		}
		writeHops(b, sj.Hops, 0, -1)
		b.WriteString("\n```mermaid\n")
		b.WriteString(seq)
		b.WriteString("```\n")
	}
}

// A system journey is listed and drawn in system.md up to maxSequenceHops
// steps and maxMermaidText characters of diagram (Mermaid's default
// maxTextSize is 50,000; larger diagrams do not render).
const (
	maxSequenceHops = 200
	maxMermaidText  = 45000
)

// hopStats counts the steps of a chain and the other services it reaches.
func hopStats(hops []model.Hop, seen map[string]bool) (int, []string) {
	n := 0
	var services []string
	for _, h := range hops {
		n++
		if !h.External && h.To != "" && !seen[h.To] {
			seen[h.To] = true
			services = append(services, h.To)
		}
		m, more := hopStats(h.Next, seen)
		n += m
		services = append(services, more...)
	}
	return n, services
}

// writeHops lists a chain as nested bullets, down to maxDepth levels (all
// when negative).
func writeHops(b *strings.Builder, hops []model.Hop, depth, maxDepth int) {
	if maxDepth >= 0 && depth >= maxDepth {
		return
	}
	for _, h := range hops {
		target := h.To
		if h.External {
			target = "external"
		}
		line := fmt.Sprintf("%s- %s → %s: `%s %s %s` (%s", strings.Repeat("  ", depth), h.From, target, h.Transport, h.Role, h.Key, h.Kind)
		if h.Background {
			line += ", goroutine"
		}
		if h.Confidence != "" && h.Confidence != "exact" {
			line += ", " + h.Confidence
		}
		line += ")"
		if h.Handler != "" {
			line += " → `" + h.Handler + "`"
		}
		if h.Cycle {
			line += " _(cycle)_"
		}
		if h.Repeated {
			line += " _(as above)_"
		}
		if h.Truncated {
			line += " _(not followed)_"
		}
		b.WriteString(line + "\n")
		writeHops(b, h.Next, depth+1, maxDepth)
	}
}
