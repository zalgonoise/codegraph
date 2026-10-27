package query

import (
	"net/url"
	"path/filepath"
	"strings"

	"github.com/zalgonoise/codegraph/internal/model"
	"github.com/zalgonoise/codegraph/internal/render"
)

// Diagrams and explorer links: what an answer can show besides its text.

// maxExplorerLinks caps the explorer links of one answer (who lists a page per key).
const maxExplorerLinks = 12

// Illustrated is a JSON answer asked with a diagram.
type Illustrated struct {
	Answer   any      `json:"answer"`
	Diagram  string   `json:"diagram,omitempty"`  // Mermaid source
	Note     string   `json:"note,omitempty"`     // why there is none, or what it leaves out
	Explorer []string `json:"explorer,omitempty"` // the explorer's pages for the answer
}

// UseExplorer sets the explorer page answers link to: "" keeps the run's own
// <run>/_system/system.html (a file:// URL, found by Load), "none" drops the
// links, anything else is the page's URL (when it is served).
func (r *Run) UseExplorer(page string) {
	switch page {
	case "":
	case "none":
		r.Explorer = ""
	default:
		r.Explorer = page
	}
}

// explorerPage is the file:// URL of a run's explorer, or "" when the run has none.
func explorerPage(dir string) string {
	file := filepath.Join(dir, "_system", "system.html")
	if !fileExists(file) {
		return ""
	}
	return fileURL(file)
}

// fileURL is the file:// URL of a file ("" when its path cannot be made absolute).
func fileURL(file string) string {
	abs, err := filepath.Abs(file)
	if err != nil {
		return ""
	}
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String()
}

// page is a page of the explorer: its label, and its route (the URL's fragment).
type page struct{ label, route string }

func homePage() page { return page{"the system", "#/"} }

func servicePage(service string) page { return page{service, "#/s/" + url.PathEscape(service)} }

func journeyPage(service, transport, key string) page {
	return page{service + " · " + transport + " " + key,
		"#/j/" + url.PathEscape(service) + "/" + url.PathEscape(transport) + "/" + url.PathEscape(key)}
}

func keyPages(transport, keys string) []page {
	var out []page
	for _, k := range strings.Split(keys, ", ") {
		out = append(out, page{transport + " " + k, "#/k/" + url.PathEscape(transport) + "/" + url.PathEscape(k)})
	}
	return out
}

// links turns pages into URLs of the run's explorer (none without one).
func (r *Run) links(pages []page) []string {
	if r.Explorer == "" {
		return nil
	}
	var out []string
	for _, p := range pages {
		if len(out) == maxExplorerLinks {
			break
		}
		out = append(out, r.Explorer+p.route)
	}
	return out
}

// explorerMarkdown is the answer's last line: where to open it in the explorer.
func (r *Run) explorerMarkdown(pages []page) string {
	urls := r.links(pages)
	if len(urls) == 0 {
		return ""
	}
	var parts []string
	for i, u := range urls {
		parts = append(parts, "["+strings.ReplaceAll(pages[i].label, "]", `\]`)+"]("+u+")")
	}
	more := ""
	if len(pages) > len(urls) {
		more = ", …"
	}
	return "\nIn the explorer: " + strings.Join(parts, ", ") + more + ".\n"
}

// ---- diagrams -------------------------------------------------------------------

// serviceDiagram draws a service's connections: the links into and out of
// it, and the stores it shares.
func serviceDiagram(d *ServiceDetail) string {
	f := render.NewFlow(d.Name)
	for _, ls := range [][]model.Link{d.Upstream, d.Downstream, d.Shares} {
		for _, l := range ls {
			f.Link(l)
		}
	}
	return f.Mermaid()
}

// journeyDiagram draws a journey's steps as a sequence diagram. When they
// are too many to draw, it follows fewer services: drawn is the depth it
// drew. It gives "" when even the journey's own calls are too many.
func (r *Run) journeyDiagram(d *JourneyDetail, depth int) (diagram string, drawn int) {
	for {
		if seq := render.SequenceDiagram(model.SystemJourney{Service: d.Service, Transport: d.Transport, Key: d.Key,
			Kind: d.Kind, Handler: d.Handler, Hops: hops(d.Service, d.Steps)}); seq != "" {
			return seq, depth
		}
		if depth == 0 {
			return "", 0
		}
		depth--
		var err error
		if d, err = r.Journey(d.Service, d.Transport+" "+d.Key, depth); err != nil {
			return "", 0
		}
	}
}

// hops turns a journey answer's steps back into the hops of a system journey.
func hops(from string, steps []Step) []model.Hop {
	out := make([]model.Hop, 0, len(steps))
	for _, s := range steps {
		out = append(out, model.Hop{From: from, To: s.To, External: s.To == "", Transport: s.Transport, Role: s.Role,
			Key: s.Key, Kind: s.Kind, Background: s.Background, Confidence: s.Confidence, Journey: s.Journey,
			Handler: s.Handler, Cycle: s.Cycle, Repeated: s.Repeated, Truncated: s.Deeper, Next: hops(s.To, s.Steps)})
	}
	return out
}

// whoDiagram draws each key between the services that send, receive and use it.
func whoDiagram(us []KeyUsers) string {
	f := render.NewFlow()
	addUsers(f, us)
	return f.Mermaid()
}

func addUsers(f *render.Flow, us []KeyUsers) {
	names := func(list []Use) []string {
		var out []string
		for _, u := range list {
			out = append(out, u.Service)
		}
		return out
	}
	for _, u := range us {
		f.Key(u.Transport, u.Key, names(u.Senders), names(u.Receivers), names(u.Users))
	}
}

// pathsDiagram draws every link of the chains found.
func pathsDiagram(from, to string, ps []Path) string {
	f := render.NewFlow(from, to)
	for _, p := range ps {
		for _, hop := range p {
			for _, l := range hop {
				f.Link(l)
			}
		}
	}
	return f.Mermaid()
}

// impactDiagram draws what depends on a service or a key: its direct links
// or endpoints, its shared stores, and the links between the services on
// the chains of the journeys that reach it.
func (r *Run) impactDiagram(im *Impact) string {
	var f *render.Flow
	if im.Kind == "service" {
		f = render.NewFlow(im.Target)
	} else {
		f = render.NewFlow()
	}
	addUsers(f, im.Users)
	for _, ls := range [][]model.Link{im.Direct, im.Shares} {
		for _, l := range ls {
			f.Link(l)
		}
	}
	pairs := map[string]bool{}
	for _, j := range im.Journeys {
		for i := 1; i < len(j.Chain); i++ {
			pairs[j.Chain[i-1]+"|"+j.Chain[i]] = true
		}
	}
	for _, l := range r.System.Links {
		if l.Kind != "shared-store" && pairs[l.From+"|"+l.To] {
			f.Link(l)
		}
	}
	return f.Mermaid()
}
