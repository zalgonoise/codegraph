package query

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zalgonoise/codegraph/internal/render"
)

// Commands describes the questions Answer takes, for usage texts and guides.
const Commands = `  services                     every service and the journeys it offers
  service <name>               a service: its journeys, upstream and downstream links, unmatched endpoints
  journey <service> [transport] <key>
                               what a journey does, across services (-depth; part of the key is enough)
  who <key>                    who sends, receives or uses a topic, queue, table, route or gRPC service
  path <from> <to>             how one service reaches another (shortest chains of links)
  impact <service|key>         what depends on a service or a key: direct users, journeys that reach it
  uses <module|package|symbol> the journeys and services calling a library's exported functions and methods
  image <name|reference>       the repositories building or using a container image, and where it runs
  search <words>               services, journeys and keys whose names contain every word`

// Options shape an answer.
type Options struct {
	Depth   int  // journey: how many services beyond its own to follow
	JSON    bool // JSON instead of Markdown
	Diagram bool // add a Mermaid diagram (service, journey, who, path, impact)
}

// Answer runs one command (args[0], then its arguments) and renders the
// answer as Markdown, or as JSON. Markdown answers end with links to the
// explorer's pages when the run has one; with opts.Diagram, they also carry
// a Mermaid diagram, and JSON answers become an Illustrated.
func Answer(r *Run, args []string, opts Options) (string, error) {
	if len(args) == 0 {
		return "", errors.New("no command; commands:\n" + Commands)
	}
	cmd, rest := args[0], args[1:]
	need := func(n int, usage string) error {
		if len(rest) < n {
			return fmt.Errorf("usage: %s", usage)
		}
		return nil
	}
	var (
		v       any
		md      string
		err     error
		pages   []page
		diagram func() (string, string) // the diagram and a note on it; nil: the command draws none
	)
	switch cmd {
	case "services":
		ss := r.Services()
		v, md, pages = ss, ServicesMarkdown(ss), []page{homePage()}
	case "service":
		if err = need(1, "service <name>"); err == nil {
			var d *ServiceDetail
			if d, err = r.Service(rest[0]); err == nil {
				v, md, pages = d, ServiceMarkdown(d), []page{servicePage(d.Name)}
				diagram = func() (string, string) { return serviceDiagram(d), "" }
			}
		}
	case "journey":
		if err = need(2, "journey <service> <key>"); err == nil {
			var d *JourneyDetail
			depth := max(opts.Depth, 0)
			if d, err = r.Journey(rest[0], strings.Join(rest[1:], " "), depth); err == nil {
				v, md, pages = d, JourneyMarkdown(d, depth), []page{journeyPage(d.Service, d.Transport, d.Key)}
				diagram = func() (string, string) {
					seq, drawn := r.journeyDiagram(d, depth)
					switch {
					case seq == "":
						return "", fmt.Sprintf("_No diagram: even the journey's own calls are too many to draw (over %d steps, "+
							"or too long a diagram). The explorer shows it step by step._", render.MaxSequenceSteps)
					case drawn < depth:
						return seq, fmt.Sprintf("_The diagram follows %d service(s) beyond this one, not %d: deeper, it is too "+
							"large to draw (over %d steps, or too long a diagram)._", drawn, depth, render.MaxSequenceSteps)
					}
					return seq, ""
				}
			}
		}
	case "who":
		if err = need(1, "who <key>"); err == nil {
			key := strings.Join(rest, " ")
			us := r.Who(key)
			v, md = us, WhoMarkdown(key, us)
			for _, u := range us {
				pages = append(pages, keyPages(u.Transport, u.Key)...)
			}
			diagram = func() (string, string) { return whoDiagram(us), "" }
		}
	case "path":
		if err = need(2, "path <from> <to>"); err == nil {
			var ps []Path
			if ps, err = r.Paths(rest[0], rest[1]); err == nil {
				v, md = ps, PathsMarkdown(rest[0], rest[1], ps)
				pages = []page{servicePage(r.services[rest[0]].Service), servicePage(r.services[rest[1]].Service)}
				diagram = func() (string, string) { return pathsDiagram(rest[0], rest[1], ps), "" }
			}
		}
	case "impact":
		if err = need(1, "impact <service|key>"); err == nil {
			var im *Impact
			if im, err = r.Impact(strings.Join(rest, " ")); err == nil {
				v, md = im, ImpactMarkdown(im)
				if im.Kind == "service" {
					pages = []page{servicePage(im.Target)}
				}
				for _, u := range im.Users {
					pages = append(pages, keyPages(u.Transport, u.Key)...)
				}
				diagram = func() (string, string) { return r.impactDiagram(im), "" }
			}
		}
	case "uses":
		if err = need(1, "uses <module|package|symbol>"); err == nil {
			text := strings.Join(rest, " ")
			us := r.Uses(text)
			v, md, pages = us, UsesMarkdown(text, us), []page{homePage()}
		}
	case "image":
		if err = need(1, "image <name|reference>"); err == nil {
			a := r.Image(strings.Join(rest, " "))
			v, md, pages = a, ImageMarkdown(a), []page{homePage()}
		}
	case "search":
		if err = need(1, "search <words>"); err == nil {
			text := strings.Join(rest, " ")
			f := r.Search(text)
			v, md, pages = f, FoundMarkdown(text, f), []page{homePage()}
		}
	default:
		err = fmt.Errorf("unknown command %q; commands:\n%s", cmd, Commands)
	}
	if err != nil {
		return "", err
	}
	drawn, note := "", ""
	if opts.Diagram && diagram != nil {
		drawn, note = diagram()
	}
	if opts.JSON {
		if opts.Diagram {
			v = Illustrated{Answer: v, Diagram: drawn, Note: strings.Trim(note, "_"), Explorer: r.links(pages)}
		}
		data, err := json.MarshalIndent(v, "", "  ")
		return string(data) + "\n", err
	}
	if opts.Diagram && diagram == nil {
		note = fmt.Sprintf("_`%s` answers draw no diagram (service, journey, who, path and impact do)._", cmd)
	}
	if drawn != "" || note != "" {
		md += "\n## Diagram\n"
		if note != "" {
			md += "\n" + note + "\n"
		}
		if drawn != "" {
			md += "\n```mermaid\n" + drawn + "```\n"
		}
	}
	return md + r.explorerMarkdown(pages), nil
}
