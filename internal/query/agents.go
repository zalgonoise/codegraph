package query

import (
	_ "embed"
	"fmt"
	"sort"
	"strings"
	"text/template"

	"github.com/zalgonoise/codegraph/internal/model"
)

//go:embed guide.md
var guideText string

var guideTemplate = template.Must(template.New("guide").Parse(guideText))

// Guide renders AGENTS.md: how an AI agent (or a person) answers questions
// about the system from this run, with examples taken from the run itself.
// tool is the codegraph repository to run codegraph from, when known.
func Guide(r *Run, tool string) string {
	repos := map[string]bool{}
	for _, m := range r.Manifests {
		repos[repo(m.Module)] = true
	}
	var b strings.Builder
	err := guideTemplate.Execute(&b, map[string]any{
		"Services": len(r.Manifests), "Repos": len(repos), "Run": r.Dir, "Tool": tool,
		"Commands": Commands, "MaxRows": maxRows, "Examples": r.examples(),
	})
	if err != nil { // the template is fixed: only a bug can fail it
		panic(err)
	}
	return b.String()
}

// examples picks real questions from the run for the guide.
func (r *Run) examples() [][2]string {
	var out [][2]string
	ss := r.Services()
	busiest := ""
	most := 0
	for _, s := range ss {
		n := 0
		for _, c := range s.Journeys {
			n += c
		}
		if n > most {
			busiest, most = s.Name, n
		}
	}
	out = append(out, [2]string{"Every service", "services"})
	if busiest != "" {
		out = append(out, [2]string{"What " + busiest + " offers and talks to", "service " + busiest})
	}
	// A journey that reaches another service, a topic with both sides, a path.
	var link *model.Link
	for i, l := range r.System.Links {
		if l.Kind == model.Async && !strings.Contains(l.Key, "*") && !strings.Contains(l.Key, ",") {
			link = &r.System.Links[i]
			break
		}
	}
	if link != nil {
		key := strings.TrimSpace(strings.Split(link.Key, ",")[0])
		out = append(out, [2]string{"Who produces and consumes a topic or queue", "who " + key})
		out = append(out, [2]string{"How one service reaches another", "path " + link.From + " " + link.To})
		for i, jr := range r.journeys {
			if dist, _, _ := r.reachable(i, 1); jr.service == link.From && !strings.Contains(jr.j.Key, ",") && len(dist) > 1 {
				out = append(out, [2]string{"What a journey does across services", fmt.Sprintf("journey %s %q", jr.service, jr.j.Key)})
				break
			}
		}
		out = append(out, [2]string{"What depends on a service", "impact " + link.To})
	}
	return out
}

// Context renders context.md: the whole system in one compact file, for an
// agent without a shell (attach it to a chat).
func Context(r *Run) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# System context\n\n%d services, %d links; a static analysis of their Go code by codegraph. "+
		"A journey is what an inbound endpoint (route, gRPC method, consumed topic or queue) does; `->` sync, `-)` async; "+
		"`*` is a part the analysis could not resolve.\n", len(r.Manifests), len(r.System.Links))
	for _, s := range r.Services() {
		m := r.services[s.Name]
		switch {
		case s.Declared != "":
			fmt.Fprintf(&b, "\n## %s (%s; declared by hand, %s)\n", s.Name, repo(s.Module), s.Declared)
		case s.Library:
			fmt.Fprintf(&b, "\n## %s (%s; a library: its exported API, never linked)\n", s.Name, repo(s.Module))
		default:
			fmt.Fprintf(&b, "\n## %s (%s)\n", s.Name, repo(s.Module))
		}
		var js []string
		internal := 0
		for _, j := range m.Journeys {
			if r.endpoints[m.Service+"|"+j.Entrypoint].Role == model.APIInternalRole {
				internal++
				continue
			}
			js = append(js, fmt.Sprintf("%s `%s`", j.Transport, j.Key))
		}
		if internal > 0 {
			js = append(js, fmt.Sprintf("and %d symbols of its internal packages (`service %s` lists them)", internal, m.Service))
		}
		if len(js) > 0 {
			fmt.Fprintf(&b, "\nJourneys: %s.\n", strings.Join(js, "; "))
		}
		out := map[string][]string{}
		for _, l := range r.System.Links {
			if l.From == s.Name && l.Kind != "shared-store" {
				arrow := "->"
				if l.Kind == model.Async {
					arrow = "-)"
				}
				out[l.To] = append(out[l.To], fmt.Sprintf("%s %s `%s`", arrow, l.Transport, l.Key))
			}
		}
		if len(out) > 0 {
			var parts []string
			for _, to := range sortedKeys(out) {
				parts = append(parts, fmt.Sprintf("%s (%s)", to, strings.Join(out[to], ", ")))
			}
			fmt.Fprintf(&b, "\nCalls: %s.\n", strings.Join(parts, "; "))
		}
		var stores []string
		seen := map[string]bool{}
		for _, ep := range m.Endpoints {
			if (ep.Transport == "sql" || ep.Transport == "redis") && ep.Role == "query" {
				for _, k := range ep.Keys {
					if k != model.Unknown && !seen[ep.Transport+k] {
						seen[ep.Transport+k] = true
						stores = append(stores, ep.Transport+" `"+k+"`")
					}
				}
			}
		}
		if len(stores) > 0 {
			sort.Strings(stores)
			fmt.Fprintf(&b, "\nStores: %s.\n", strings.Join(stores, ", "))
		}
	}
	var orphans []string
	seen := map[string]bool{}
	for _, d := range r.System.Dangling {
		if d.Reason == "" && (d.Transport == "kafka" || d.Transport == "amqp" || d.Transport == "nats") && d.Key != model.Unknown {
			line := fmt.Sprintf("- %s %s `%s` (%s)", d.Transport, d.Role, d.Key, d.Service)
			if !seen[line] {
				seen[line] = true
				orphans = append(orphans, line)
			}
		}
	}
	if len(orphans) > 0 {
		sort.Strings(orphans)
		b.WriteString("\n## Topics and queues nothing analysed answers\n\n")
		b.WriteString(strings.Join(orphans, "\n") + "\n")
	}
	return b.String()
}
