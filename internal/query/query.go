// Package query answers questions about a run of codegraph (a directory of service
// manifests, as analyze and fleet write them) for people and AI agents: the
// services and the journeys they offer, what a journey reaches, who produces
// or consumes a topic, how one service reaches another and what a change
// would affect. Answers are small and carry source positions to cite.
//
// The same answers back `codegraph query` and, written once per run, the AGENTS.md
// guide and the context.md summary for agents without a shell.
package query

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/zalgonoise/codegraph/internal/deploy"
	"github.com/zalgonoise/codegraph/internal/inventory"
	"github.com/zalgonoise/codegraph/internal/link"
	"github.com/zalgonoise/codegraph/internal/manifest"
	"github.com/zalgonoise/codegraph/internal/model"
)

// Run is a loaded run: its manifests, linked, and what an environment
// deploys when `codegraph deploy` wrote <dir>/deploy/deploy.json.
type Run struct {
	Dir       string
	Manifests []*model.Manifest
	System    *model.System
	Deploy    *deploy.Deployment
	Inventory *inventory.Inventory // the organisation's inventory, for image questions (UseInventory)
	Explorer  string               // the explorer page answers link to ("" for none; see UseExplorer)

	services  map[string]*model.Manifest
	endpoints map[string]model.Endpoint // service|endpoint ID
	journeys  []journey
	byEntry   map[string][]int  // service|entrypoint -> journeys starting there
	usedBy    map[string][]int  // service|outbound endpoint -> journeys using it
	dangling  map[string]string // service|endpoint ID -> reason ("" for none)

	// Analysed libraries: their symbols' journeys, and the journeys calling
	// each.
	api      *model.APIIndex
	apiFirst []int // the index of each manifest's first journey
	apiUsers map[int][]JourneyUse
}

type journey struct {
	service string
	j       model.Journey
}

// Load reads <dir>/*/manifest.json and links the services.
func Load(dir string) (*Run, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*", "manifest.json"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no manifests in %s: run `make graph` first, or point -run at a run directory", dir)
	}
	ms, err := manifest.ReadAll(paths)
	if err != nil {
		return nil, err
	}
	r := New(dir, ms, link.Link(ms))
	r.Explorer = explorerPage(dir)
	if file := filepath.Join(dir, "deploy", "deploy.json"); fileExists(file) {
		if r.Deploy, err = deploy.ReadFile(file); err != nil {
			return nil, err
		}
	}
	if file := filepath.Join(filepath.Dir(filepath.Clean(dir)), "inventory", "inventory.json"); fileExists(file) {
		if err := r.UseInventory(file); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// New indexes manifests already linked into sys.
func New(dir string, ms []*model.Manifest, sys *model.System) *Run {
	r := &Run{
		Dir: dir, Manifests: ms, System: sys,
		services:  map[string]*model.Manifest{},
		endpoints: map[string]model.Endpoint{},
		byEntry:   map[string][]int{},
		usedBy:    map[string][]int{},
		dangling:  map[string]string{},
	}
	for _, m := range ms {
		r.services[m.Service] = m
		for _, ep := range m.Endpoints {
			r.endpoints[m.Service+"|"+ep.ID] = ep
		}
		for _, j := range m.Journeys {
			i := len(r.journeys)
			r.journeys = append(r.journeys, journey{m.Service, j})
			r.byEntry[m.Service+"|"+j.Entrypoint] = append(r.byEntry[m.Service+"|"+j.Entrypoint], i)
			for _, o := range j.Outbound {
				r.usedBy[m.Service+"|"+o.Endpoint] = append(r.usedBy[m.Service+"|"+o.Endpoint], i)
			}
		}
	}
	for _, d := range sys.Dangling {
		if !d.Partial { // a partly linked endpoint is not unmatched as a whole
			r.dangling[d.Service+"|"+d.Endpoint] = d.Reason
		}
	}
	r.indexAPI()
	return r
}

// indexAPI indexes the calls of journeys into the exported symbols of the
// run's analysed libraries (model.APICalls).
func (r *Run) indexAPI() {
	r.apiUsers = map[int][]JourneyUse{}
	if r.api = model.NewAPIIndex(r.Manifests); r.api == nil {
		return
	}
	r.apiFirst = make([]int, len(r.Manifests))
	for mi, n := 0, 0; mi < len(r.Manifests); mi++ {
		r.apiFirst[mi] = n
		n += len(r.Manifests[mi].Journeys)
	}
	for _, c := range model.APICalls(r.Manifests) {
		caller, api := r.apiFirst[c.Caller.Manifest]+c.Caller.Journey, r.apiFirst[c.API.Manifest]+c.API.Journey
		r.apiUsers[api] = append(r.apiUsers[api], JourneyUse{JourneyRef: r.ref(caller), Site: c.Use.Position, Calls: c.Use.Calls})
	}
}

// apiJourney returns the journey of an analysed library's symbol: of package
// pkg, or of the module's root package when only the module is known (the
// symbols a service calls outside its journeys).
func (r *Run) apiJourney(module, pkg, symbol string) (int, bool) {
	at, ok := r.api.Find(cmp.Or(pkg, module), symbol)
	if !ok {
		return 0, false
	}
	return r.apiFirst[at.Manifest] + at.Journey, true
}

// service finds a service by name, suggesting near names when there is none.
func (r *Run) service(name string) (*model.Manifest, error) {
	if m := r.services[name]; m != nil {
		return m, nil
	}
	var near []string
	for n := range r.services {
		if strings.Contains(n, name) || strings.Contains(name, n) || distance(n, name) <= 2 {
			near = append(near, n)
		}
	}
	sort.Strings(near)
	if len(near) > 0 {
		return nil, fmt.Errorf("no service %q; did you mean %s?", name, strings.Join(near, ", "))
	}
	return nil, fmt.Errorf("no service %q (`codegraph query services` lists them)", name)
}

// next lists the journeys an outbound endpoint of a service continues into.
func (r *Run) next(service, endpoint string) []continuation {
	var out []continuation
	for _, c := range r.System.Continuations[service+"|"+endpoint] {
		cont := continuation{Link: c.Link}
		for _, key := range c.Journeys {
			for _, i := range r.byEntry[c.Link.To+"|"+c.Link.ToEndpoint] {
				if r.journeys[i].j.Key == key {
					cont.journeys = append(cont.journeys, i)
				}
			}
		}
		out = append(out, cont)
	}
	return out
}

type continuation struct {
	model.Link
	journeys []int
}

// reachable maps every journey reachable from start (following continuations
// across services, up to depth hops) to its distance, to the link that
// entered it, and to the journey it was entered from.
func (r *Run) reachable(start, depth int) (dist map[int]int, via map[int]model.Link, from map[int]int) {
	dist, via, from = map[int]int{start: 0}, map[int]model.Link{}, map[int]int{}
	queue := []int{start}
	for len(queue) > 0 {
		i := queue[0]
		queue = queue[1:]
		if dist[i] >= depth {
			continue
		}
		jr := r.journeys[i]
		for _, o := range jr.j.Outbound {
			for _, c := range r.next(jr.service, o.Endpoint) {
				for _, t := range c.journeys {
					if _, ok := dist[t]; !ok {
						dist[t], via[t], from[t] = dist[i]+1, c.Link, i
						queue = append(queue, t)
					}
				}
			}
		}
	}
	return dist, via, from
}

// transports are the first word a journey key may be qualified with.
var transports = map[string]bool{"http": true, "grpc": true, "kafka": true, "amqp": true, "nats": true, "redis": true, "sql": true, "cli": true, model.APITransport: true, model.MainTransport: true}

// keyEndpoints lists the endpoints with key exactly, or, when none has it,
// with keys containing it (case-insensitive), as service|endpoint IDs.
func (r *Run) keyEndpoints(key string) []string {
	var exact, near []string
	low := strings.ToLower(key)
	for id, ep := range r.endpoints {
		if ep.Transport == model.MainTransport {
			continue // every service's main: not a key anything shares
		}
		if slices.Contains(ep.Keys, key) {
			exact = append(exact, id)
			continue
		}
		for _, k := range ep.Keys {
			if strings.Contains(strings.ToLower(k), low) {
				near = append(near, id)
				break
			}
		}
	}
	if len(exact) > 0 {
		sort.Strings(exact)
		return exact
	}
	sort.Strings(near)
	return near
}

func split(id string) (service, endpoint string) {
	service, endpoint, _ = strings.Cut(id, "|")
	return service, endpoint
}

func keys(ep model.Endpoint) string { return strings.Join(ep.Keys, ", ") }

// distance is the edit distance between two names, for suggestions.
func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
