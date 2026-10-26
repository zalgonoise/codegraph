// Package diff compares two runs of codegraph, a base and a head (each a set of
// service manifests), and reports the architectural changes between them:
// services, cross-service links, the endpoints of each service, and topics or
// queues nothing analysed answers. It is meant for reviews: run codegraph on the
// base and the head of a change and post the report, or fail a CI job when
// cross-service links change.
package diff

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zalgonoise/codegraph/internal/link"
	"github.com/zalgonoise/codegraph/internal/model"
)

// Result holds what the head adds to and removes from the base.
type Result struct {
	BaseServices, HeadServices int

	Services  Delta[string]
	Links     Delta[model.Link]
	Endpoints map[string]Delta[string] // service -> endpoint signatures, for services in both runs
	Orphans   Delta[string]            // "service transport role key"

	endpointCounts map[string]int // of the services added or removed
}

// Delta is what was added and removed.
type Delta[T any] struct {
	Added, Removed []T
}

func (d Delta[T]) empty() bool { return len(d.Added) == 0 && len(d.Removed) == 0 }

// Empty reports whether nothing changed.
func (r *Result) Empty() bool {
	return r.Services.empty() && r.Links.empty() && len(r.Endpoints) == 0 && r.Orphans.empty()
}

// LinksChanged reports whether a cross-service link was added or removed.
func (r *Result) LinksChanged() bool { return !r.Links.empty() }

// Compare links both runs and compares them.
func Compare(base, head []*model.Manifest) *Result {
	bs, hs := link.Link(base), link.Link(head)
	r := &Result{BaseServices: len(base), HeadServices: len(head), Endpoints: map[string]Delta[string]{},
		endpointCounts: map[string]int{}}
	bn, hn := names(base), names(head)
	r.Services = compare(bn, hn)

	bl, hl := links(bs), links(hs)
	for _, k := range sortedKeys(hl) {
		if _, ok := bl[k]; !ok {
			r.Links.Added = append(r.Links.Added, hl[k])
		}
	}
	for _, k := range sortedKeys(bl) {
		if _, ok := hl[k]; !ok {
			r.Links.Removed = append(r.Links.Removed, bl[k])
		}
	}

	be, he := endpoints(base), endpoints(head)
	for svc := range union(be, he) {
		if !bn[svc] || !hn[svc] { // a whole service added or removed
			r.endpointCounts[svc] = len(be[svc]) + len(he[svc])
			continue
		}
		if d := compare(be[svc], he[svc]); !d.empty() {
			r.Endpoints[svc] = d
		}
	}
	r.Orphans = compare(orphans(bs), orphans(hs))
	return r
}

func names(ms []*model.Manifest) map[string]bool {
	out := map[string]bool{}
	for _, m := range ms {
		out[m.Service] = true
	}
	return out
}

func links(s *model.System) map[string]model.Link {
	out := map[string]model.Link{}
	for _, l := range s.Links {
		out[l.From+"|"+l.To+"|"+l.Transport+"|"+l.Key] = l
	}
	return out
}

// endpoints maps each service to the signatures of its endpoints; IDs are not
// compared, since they change with any key.
func endpoints(ms []*model.Manifest) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, m := range ms {
		set := map[string]bool{}
		for _, ep := range m.Endpoints {
			set[Signature(ep)] = true
		}
		out[m.Service] = set
	}
	return out
}

// Signature names an endpoint by what it is: "inbound kafka consumer topic".
func Signature(ep model.Endpoint) string {
	return fmt.Sprintf("%s %s %s %s", ep.Direction, ep.Transport, ep.Role, strings.Join(ep.Keys, ", "))
}

// orphans lists the messaging endpoints nothing analysed answers, with no
// reason not to be orphans and a resolved key.
func orphans(s *model.System) map[string]bool {
	messaging := map[string]bool{"kafka": true, "amqp": true, "nats": true}
	out := map[string]bool{}
	for _, d := range s.Dangling {
		if d.Reason == "" && messaging[d.Transport] && d.Key != model.Unknown {
			out[fmt.Sprintf("%s %s %s %s", d.Service, d.Transport, d.Role, d.Key)] = true
		}
	}
	return out
}

func compare(base, head map[string]bool) Delta[string] {
	var d Delta[string]
	for _, k := range sortedKeys(head) {
		if !base[k] {
			d.Added = append(d.Added, k)
		}
	}
	for _, k := range sortedKeys(base) {
		if !head[k] {
			d.Removed = append(d.Removed, k)
		}
	}
	return d
}

func union[V any](a, b map[string]V) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Markdown renders the result for a review comment.
func Markdown(r *Result, base, head string) string {
	var b strings.Builder
	b.WriteString("# Architecture changes\n\n")
	fmt.Fprintf(&b, "Base `%s` (%d services), head `%s` (%d services).\n", base, r.BaseServices, head, r.HeadServices)
	if r.Empty() {
		b.WriteString("\nNo architectural change: the same services, links, endpoints and unanswered topics.\n")
		return b.String()
	}
	if !r.Links.empty() {
		fmt.Fprintf(&b, "\n## Cross-service links (%d added, %d removed)\n\n", len(r.Links.Added), len(r.Links.Removed))
		b.WriteString("| | From | To | Transport | Key | Kind | Confidence |\n|---|---|---|---|---|---|---|\n")
		row := func(sign string, l model.Link) {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | `%s` | %s | %s |\n", sign, l.From, l.To, l.Transport, cell(l.Key), l.Kind, l.Confidence)
		}
		for _, l := range r.Links.Added {
			row("+", l)
		}
		for _, l := range r.Links.Removed {
			row("−", l)
		}
	}
	list := func(title string, d Delta[string], note func(string) string) {
		if d.empty() {
			return
		}
		fmt.Fprintf(&b, "\n## %s\n\n", title)
		for _, s := range d.Added {
			fmt.Fprintf(&b, "- + `%s`%s\n", s, note(s))
		}
		for _, s := range d.Removed {
			fmt.Fprintf(&b, "- − `%s`%s\n", s, note(s))
		}
	}
	list("Services", r.Services, func(svc string) string {
		if n := r.endpointCounts[svc]; n != 1 {
			return fmt.Sprintf(" (%d endpoints)", n)
		}
		return " (1 endpoint)"
	})
	list("Topics and queues nothing analysed answers", r.Orphans, func(string) string { return "" })
	if len(r.Endpoints) > 0 {
		b.WriteString("\n## Endpoints\n")
		for _, svc := range sortedKeys(r.Endpoints) {
			d := r.Endpoints[svc]
			fmt.Fprintf(&b, "\n### `%s`\n\n", svc)
			for _, s := range d.Added {
				fmt.Fprintf(&b, "- + `%s`\n", s)
			}
			for _, s := range d.Removed {
				fmt.Fprintf(&b, "- − `%s`\n", s)
			}
		}
	}
	return b.String()
}

func cell(s string) string { return strings.ReplaceAll(s, "|", "\\|") }
