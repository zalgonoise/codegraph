package link

import (
	"math"
	"sort"
	"strings"

	"github.com/zalgonoise/codegraph/internal/model"
)

// maxHops caps how many services deep a journey is followed.
const maxHops = 6

// flows is what chaining knows of the data between journeys: the databases
// each service is configured with, and the journeys run on a schedule
// ("service|journey key").
type flows struct {
	databases map[string][]model.Database
	scheduled map[string]string
}

// chain follows every service journey across services: each outbound
// endpoint of a journey becomes a hop, either to another analysed service
// (continuing with the journey of the inbound endpoint it links to, or, for a
// table it writes, with the journeys reading that table on their own) or to
// something outside the system (a store, broker or external API). Only
// journeys that reach at least one other service are returned, with the
// continuations of every linked outbound endpoint ("service|endpoint ID").
func chain(ms []*model.Manifest, byOut map[string][]model.Link, depth int, f flows) ([]model.SystemJourney, map[string][]model.Continuation) {
	c := &chainer{
		byOut:     byOut,
		endpoints: map[string]model.Endpoint{},
		journeys:  map[string][]model.Journey{},
		next:      map[string][]model.Continuation{},
		scheduled: f.scheduled,
		maxDepth:  depth,
	}
	for _, m := range ms {
		for _, ep := range m.Endpoints {
			c.endpoints[m.Service+"|"+ep.ID] = ep
		}
		for _, j := range m.Journeys {
			c.journeys[m.Service+"|"+j.Entrypoint] = append(c.journeys[m.Service+"|"+j.Entrypoint], j)
		}
	}
	for key := range byOut {
		if ep, ok := c.endpoints[key]; ok {
			service, _, _ := strings.Cut(key, "|")
			c.next[key] = c.continuations(service, ep)
		}
	}
	tableContinuations(ms, f, c.next)

	var out []model.SystemJourney
	for _, m := range ms {
		for _, j := range m.Journeys {
			entry := c.endpoints[m.Service+"|"+j.Entrypoint]
			sj := model.SystemJourney{
				Service: m.Service, Entrypoint: j.Entrypoint, Transport: j.Transport, Key: j.Key,
				Kind: entry.InteractionKind(), Handler: j.Handler, Schedule: c.scheduled[m.Service+"|"+j.Key],
			}
			root := m.Service + "|" + j.Key
			sj.Hops = c.hops(m.Service, j, 1, map[string]bool{root: true}, map[string]int{root: 0}, false)
			if crossesServices(sj.Hops) {
				out = append(out, sj)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].Key < out[j].Key
	})
	return out, c.next
}

type chainer struct {
	byOut     map[string][]model.Link
	endpoints map[string]model.Endpoint       // service|endpoint ID
	journeys  map[string][]model.Journey      // service|inbound endpoint ID
	next      map[string][]model.Continuation // service|outbound endpoint ID
	scheduled map[string]string               // service|journey key: its schedule
	maxDepth  int
}

// continuations lists where an outbound endpoint leads: each link, with the
// journeys of the target it continues into (for a gRPC call, only the
// journey of the method called).
func (c *chainer) continuations(service string, ep model.Endpoint) []model.Continuation {
	var out []model.Continuation
	for _, l := range c.byOut[service+"|"+ep.ID] {
		targets := c.journeys[l.To+"|"+l.ToEndpoint]
		if ep.Transport == "grpc" {
			targets = sameMethod(targets, ep.Keys)
		}
		s := model.Continuation{Link: l}
		for _, tj := range targets {
			s.Journeys = append(s.Journeys, tj.Key)
		}
		out = append(out, s)
	}
	return out
}

// hops follows j's outbound endpoints. path holds the journeys on the way
// here (a call back into one is a cycle); expanded holds the depth at which
// each journey of this chain was followed, so a second route to it (A→B→D
// and A→C→D) is marked repeated instead of listed again, unless it arrives
// shallower and can be followed further. Without it, chains grow
// exponentially with depth. viaTable is set past a table hand-off: a chain
// takes one (a request into the job reading what it wrote), as jobs writing
// and reading the same tables would otherwise lead into each other at every
// step; each job's own journey shows the hand-offs it makes.
func (c *chainer) hops(service string, j model.Journey, depth int, path map[string]bool, expanded map[string]int, viaTable bool) []model.Hop {
	var out []model.Hop
	for _, o := range j.Outbound {
		ep, ok := c.endpoints[service+"|"+o.Endpoint]
		if !ok {
			continue
		}
		base := model.Hop{
			From: service, Transport: ep.Transport, Role: ep.Role, Key: strings.Join(ep.Keys, ", "),
			Access: ep.Access, Message: ep.Message, Kind: o.Kind, Background: o.Background, FromEndpoint: ep.ID,
		}
		hopped := false
		for _, s := range c.next[service+"|"+ep.ID] {
			l := s.Link
			table := model.Store(l.Transport)
			if table && viaTable {
				continue
			}
			h := base
			h.To, h.Key, h.Kind, h.Confidence, h.ToEndpoint = l.To, l.Key, l.Kind, l.Confidence, l.ToEndpoint
			targets := c.targets(l, s.Journeys)
			if len(targets) == 0 {
				hopped = true
				out = append(out, h)
				continue
			}
			for _, tj := range targets {
				if l.To == service && tj.Key == j.Key {
					continue // a job writing what it reads itself
				}
				hopped = true
				t := h
				t.Journey, t.Handler, t.Schedule = tj.Key, tj.Handler, c.scheduled[l.To+"|"+tj.Key]
				if !model.Store(l.Transport) { // a table keeps its name: the journey reading it is any
					t.Key = tj.Key
				}
				id := l.To + "|" + tj.Key
				switch {
				case path[id]:
					t.Cycle = true
				case seenAt(expanded, id) <= depth:
					t.Repeated = true
				case depth >= c.maxDepth:
					t.Truncated = true
				default:
					path[id], expanded[id] = true, depth
					t.Next = c.hops(l.To, tj, depth+1, path, expanded, viaTable || table)
					delete(path, id)
				}
				out = append(out, t)
			}
		}
		if !hopped {
			base.External = true
			out = append(out, base)
		}
	}
	return out
}

// schedules resolves the journey each job runs (by "service|journey key"):
// the CLI command a word of its command names, else its main. A journey run
// by several jobs lists their schedules.
func schedules(ms []*model.Manifest, jobs []model.Job) map[string]string {
	if len(jobs) == 0 {
		return nil
	}
	byService := map[string]*model.Manifest{}
	for _, m := range ms {
		byService[m.Service] = m
	}
	out := map[string]string{}
	for _, job := range jobs {
		m := byService[job.Service]
		if m == nil {
			continue
		}
		key := model.MainKey
		for _, w := range job.Command {
			if hasJourney(m, "cli", w) {
				key = w
				break
			}
		}
		id := job.Service + "|" + key
		if prev := out[id]; prev == "" {
			out[id] = job.Schedule
		} else if !strings.Contains(prev, job.Schedule) {
			out[id] = prev + ", " + job.Schedule
		}
	}
	return out
}

func hasJourney(m *model.Manifest, transport, key string) bool {
	for _, j := range m.Journeys {
		if j.Transport == transport && j.Key == key {
			return true
		}
	}
	return false
}

// tableReader is a journey reading a table on its own: one a deployment runs
// on a schedule, the main journey of a program nothing else starts (a worker's
// loop), or another main journey from its goroutines.
type tableReader struct {
	service, entry, journey string
	db                      model.Database
}

// tableContinuations adds, for every outbound endpoint writing a table of its
// service's database, a continuation into each journey reading that table on
// its own in the same database, as an async link "<database>.<table>"
// (confidence same-database). A journey reading it because a request asks
// (a route, a method, a consumer) is not continued into: the write does not
// start it.
func tableContinuations(ms []*model.Manifest, f flows, next map[string][]model.Continuation) {
	only := func(service string) *model.Database {
		if dbs := f.databases[service]; len(dbs) == 1 {
			return &dbs[0]
		}
		return nil
	}
	readers := map[string][]tableReader{} // table
	for _, m := range ms {
		db := only(m.Service)
		if db == nil {
			continue
		}
		eps := map[string]model.Endpoint{}
		started := false // an inbound endpoint other than main starts work in it
		for _, ep := range m.Endpoints {
			eps[ep.ID] = ep
			started = started || ep.Direction == model.Inbound && ep.Transport != model.MainTransport
		}
		for _, j := range m.Journeys {
			_, scheduled := f.scheduled[m.Service+"|"+j.Key]
			main := j.Transport == model.MainTransport
			if !scheduled && !main {
				continue
			}
			seen := map[string]bool{}
			for _, o := range j.Outbound {
				ep := eps[o.Endpoint]
				if ep.Transport != "sql" || ep.Role != "query" || ep.Owner != "" || !model.Reads(ep.Access) ||
					!scheduled && started && !o.Background {
					continue
				}
				for _, k := range ep.Keys {
					if named(k) && !seen[k] {
						seen[k] = true
						readers[k] = append(readers[k], tableReader{m.Service, j.Entrypoint, j.Key, *db})
					}
				}
			}
		}
	}
	for _, m := range ms {
		db := only(m.Service)
		if db == nil {
			continue
		}
		for _, ep := range m.Endpoints {
			if ep.Transport != "sql" || ep.Role != "query" || ep.Owner != "" || !model.Writes(ep.Access) {
				continue
			}
			for _, k := range ep.Keys {
				for _, r := range readers[k] {
					if !r.db.Same(*db) {
						continue
					}
					l := model.Link{From: m.Service, To: r.service, Transport: ep.Transport, Key: db.Name + "." + k,
						Kind: model.Async, Confidence: "same-database", ToEndpoint: r.entry}
					next[m.Service+"|"+ep.ID] = append(next[m.Service+"|"+ep.ID], model.Continuation{Link: l, Journeys: []string{r.journey}})
				}
			}
		}
	}
}

// seenAt is the depth a journey was followed at in this chain, or more than
// any depth when it was not.
func seenAt(expanded map[string]int, id string) int {
	if d, ok := expanded[id]; ok {
		return d
	}
	return math.MaxInt
}

// targets returns the journeys of a link's target endpoint with the given keys.
func (c *chainer) targets(l model.Link, keys []string) []model.Journey {
	var out []model.Journey
	for _, tj := range c.journeys[l.To+"|"+l.ToEndpoint] {
		for _, k := range keys {
			if tj.Key == k {
				out = append(out, tj)
				break
			}
		}
	}
	return out
}

func crossesServices(hops []model.Hop) bool {
	for _, h := range hops {
		if !h.External {
			return true
		}
	}
	return false
}

// sameMethod keeps the per-method journeys of a gRPC server that a call keyed
// "Service/Method" invokes; a server with a single service-level journey is
// kept whole.
func sameMethod(targets []model.Journey, keys []string) []model.Journey {
	perMethod := false
	var out []model.Journey
	for _, tj := range targets {
		if strings.Contains(tj.Key, "/") {
			perMethod = true
		}
		for _, k := range keys {
			if tj.Key == k {
				out = append(out, tj)
			}
		}
	}
	if !perMethod {
		return targets
	}
	return out
}
