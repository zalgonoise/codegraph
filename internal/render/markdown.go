package render

import (
	"fmt"
	"strings"

	"github.com/zalgonoise/codegraph/internal/model"
)

// entryLine describes where the analysis starts: a main package, or a
// library's exported API.
func entryLine(m *model.Manifest) string {
	if !m.Library {
		return code(m.Entry)
	}
	n := 0
	for _, ep := range m.Endpoints {
		if ep.Transport == model.APITransport {
			n++
		}
	}
	return fmt.Sprintf("its exported API: %d functions and methods of its public packages (transport `%s`)", n, model.APITransport)
}

// ServiceReport renders a Markdown report of one manifest (diagram embedded).
func ServiceReport(m *model.Manifest, diagram string) string {
	var b strings.Builder
	kind := "Service"
	if m.Library {
		kind = "Library"
	}
	fmt.Fprintf(&b, "# %s `%s`\n\n", kind, m.Service)
	if d := m.Declared; d != nil {
		table(&b, []string{"Property", "Value"}, [][]string{
			{"Declared by hand", fmt.Sprintf("not analysed; endpoints and journeys as written in %s", code(d.File))},
			{"Repository", fmt.Sprintf("%s at %s (%s)", d.Repository, code(d.Commit), d.Language)},
			{"Description", d.Description},
			{"Workloads", strings.Join(d.Workloads, ", ")},
			{"Aliases", strings.Join(m.Aliases, ", ")},
		})
	} else {
		table(&b, []string{"Property", "Value"}, [][]string{
			{"Module", code(m.Module)},
			{"Entrypoint", entryLine(m)},
			{"Call graph", strings.ToUpper(m.Algorithm)},
			{"Reachable functions", fmt.Sprintf("%d total, %d in module, %d components", m.Stats.ReachableFuncs, m.Stats.ModuleFuncs, m.Stats.Components)},
			{"Aliases", strings.Join(m.Aliases, ", ")},
			{"Tags", tagLine(m.Tags)},
		})
	}

	b.WriteString("\n## Architecture\n\n```mermaid\n")
	b.WriteString(diagram)
	b.WriteString("```\n")

	b.WriteString("\n## Endpoints\n\n")
	var rows, internal [][]string
	for _, ep := range m.Endpoints {
		if InternalAPI(ep) {
			internal = append(internal, []string{code(strings.Join(ep.Keys, "\n")), ep.Handler, ep.Position})
			continue
		}
		site := ep.Handler
		if site == "" {
			site = ep.Function
		}
		rows = append(rows, []string{
			string(ep.Direction), ep.Transport, ep.Role, code(strings.Join(ep.Keys, "\n")), ep.Message,
			site, ep.Component, fmt.Sprintf("%s (%d)", ep.Source, ep.Count), ep.Position,
		})
	}
	table(&b, []string{"Direction", "Transport", "Role", "Keys", "Message", "Handler / function", "Component", "Source", "Position"}, rows)
	if len(internal) > 0 {
		fmt.Fprintf(&b, "\n## Internal API (%d)\n\nThe exported functions and methods of its internal packages, which only the module itself can "+
			"call. Each has a journey, like the API's, left out of the diagram and the journeys below (the explorer shows "+
			"them on request; `codegraph query journey %s go <key>` follows one).\n\n", len(internal), m.Service)
		table(&b, []string{"Key", "Handler", "Position"}, internal)
	}

	journeysSection(&b, m)
	if m.Library {
		return b.String()
	}

	b.WriteString("\n## Bootstrap (main)\n\n")
	rows = nil
	for _, s := range m.Bootstrap {
		if s.Kind == "command" { // a CLI command's own wiring follows main's
			table(&b, []string{"#", "Kind", "Call", "", "Caller", "Position"}, rows)
			fmt.Fprintf(&b, "\n### Command `%s` (`%s`, %s)\n\n", s.Command, s.Call, s.Position)
			rows = nil
			continue
		}
		flags := ""
		if s.Async {
			flags = "go"
		}
		if s.Deferred {
			flags = "defer"
		}
		rows = append(rows, []string{fmt.Sprint(s.Order), s.Kind, strings.Repeat("· ", s.Depth) + code(s.Call), flags, s.Caller, s.Position})
	}
	table(&b, []string{"#", "Kind", "Call", "", "Caller", "Position"}, rows)

	b.WriteString("\n## Components and contracts\n")
	for _, c := range m.Components {
		fmt.Fprintf(&b, "\n### `%s` (%s, %d reachable funcs)\n\n", c.Name, c.Layer, c.Funcs)
		if len(c.Interfaces) > 0 {
			rows = nil
			for _, i := range c.Interfaces {
				impls := strings.Join(i.Implementations, "\n")
				if impls == "" {
					impls = "_none at runtime_"
				}
				rows = append(rows, []string{code(i.Name), code(strings.Join(i.Methods, "\n")), impls})
			}
			table(&b, []string{"Interface", "Contract", "Runtime implementations"}, rows)
			b.WriteString("\n")
		}
		if len(c.Types) > 0 {
			rows = nil
			for _, t := range c.Types {
				rows = append(rows, []string{code(t.Name), strings.Join(t.Methods, ", ")})
			}
			table(&b, []string{"Type in use", "Reachable methods"}, rows)
		}
	}

	b.WriteString("\n## Calls between components\n\n")
	rows = nil
	for _, c := range m.Calls {
		via := ""
		if c.ViaInterface {
			via = "yes"
		}
		rows = append(rows, []string{c.From, c.To, fmt.Sprint(c.Count), via, strings.Join(c.Symbols, "\n")})
	}
	table(&b, []string{"From", "To", "Edges", "Via interface", "Symbols"}, rows)

	b.WriteString("\n## Interface bindings (runtime wiring)\n\n")
	rows = nil
	for _, bd := range m.Bindings {
		rows = append(rows, []string{code(bd.Interface), code(bd.Concrete), fmt.Sprint(bd.Sites), strings.Join(bd.Positions, "\n")})
	}
	table(&b, []string{"Interface", "Concrete", "Sites", "Where"}, rows)

	if len(m.Libraries) > 0 {
		b.WriteString("\n## Libraries\n\n")
		rows = nil
		for _, l := range m.Libraries {
			syms := code(strings.Join(l.Symbols, "\n"))
			if len(l.Symbols) == 0 {
				syms = "_none named: reached through interfaces_"
			}
			rows = append(rows, []string{code(l.Module), strings.Join(l.UsedBy, "\n"), fmt.Sprint(l.Calls), syms})
		}
		table(&b, []string{"Module", "Used by", "Call edges", "Exported functions and methods called"}, rows)
	}
	return b.String()
}

// SystemReport renders the linked system as Markdown.
func SystemReport(s *model.System, diagram string) string {
	var b strings.Builder
	b.WriteString("# System overview\n\n```mermaid\n")
	b.WriteString(diagram)
	b.WriteString("```\n\n## Services\n\n")
	var rows [][]string
	for _, svc := range s.Services {
		rows = append(rows, []string{code(svc.Name), strings.Join(svc.Aliases, ", "), tagLine(svc.Tags), fmt.Sprint(svc.Inbound), fmt.Sprint(svc.Outbound)})
	}
	table(&b, []string{"Service", "Aliases", "Tags", "Inbound", "Outbound"}, rows)

	b.WriteString("\n## Links\n\n")
	rows = nil
	for _, l := range s.Links {
		rows = append(rows, []string{l.From, l.To, l.Transport, code(l.Key), l.Kind, l.Confidence})
	}
	table(&b, []string{"From", "To", "Transport", "Key", "Kind", "Confidence"}, rows)

	systemJourneysSection(&b, s)

	b.WriteString("\n## Unmatched endpoints\n\n")
	b.WriteString("Endpoints no other analysed service answers. A reason says why one is not an orphan; a topic or queue " +
		"without one may belong to a repository that was not analysed, or be unused.\n\n")
	rows = nil
	seen := map[string]bool{}
	for _, d := range s.Dangling {
		reason := DanglingReason(d.Reason)
		if d.Partial {
			reason = strings.TrimPrefix(reason+"; the endpoint's other keys link", "; ")
		}
		row := []string{d.Service, string(d.Direction), d.Transport, d.Role, code(d.Key), reason}
		if id := strings.Join(row, "|"); !seen[id] { // per-caller variants repeat a key
			seen[id] = true
			rows = append(rows, row)
		}
	}
	table(&b, []string{"Service", "Direction", "Transport", "Role", "Key", "Reason"}, rows)
	return b.String()
}

// DanglingReason describes why an unmatched endpoint is not an orphan.
func DanglingReason(reason string) string {
	switch reason {
	case model.DanglingSameService:
		return "same service"
	case model.DanglingBroker:
		return "broker queue (retry, delay, dead letter)"
	case model.DanglingOtherTransport:
		return "answered on another messaging transport"
	}
	return ""
}

func table(b *strings.Builder, header []string, rows [][]string) {
	if len(rows) == 0 {
		b.WriteString("_none_\n")
		return
	}
	cells := make([]string, len(header))
	for i, h := range header {
		cells[i] = cell(h)
	}
	fmt.Fprintf(b, "| %s |\n|%s\n", strings.Join(cells, " | "), strings.Repeat("---|", len(header)))
	for _, r := range rows {
		for i := range r {
			r[i] = cell(r[i])
		}
		fmt.Fprintf(b, "| %s |\n", strings.Join(r, " | "))
	}
}

var cellEscaper = strings.NewReplacer("|", `\|`, "\n", "<br>")

func cell(s string) string { return cellEscaper.Replace(s) }

// code wraps each line in backticks (so multi-line cells stay readable).
func code(s string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "`" + l + "`"
	}
	return strings.Join(lines, "\n")
}
