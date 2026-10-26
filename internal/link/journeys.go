package link

import (
	"math"
	"sort"
	"strings"

	"github.com/zalgonoise/codegraph/internal/model"
)

// maxHops caps how many services deep a journey is followed.
const maxHops = 6

// chain follows every service journey across services: each outbound
// endpoint of a journey becomes a hop, either to another analysed service
// (continuing with the journey of the inbound endpoint it links to) or to
// something outside the system (a store, broker or external API). Only
// journeys that reach at least one other service are returned, with the
// continuations of every linked outbound endpoint ("service|endpoint ID").
func chain(ms []*model.Manifest, byOut map[string][]model.Link, depth int) ([]model.SystemJourney, map[string][]model.Continuation) {
	c := &chainer{
		byOut:     byOut,
		endpoints: map[string]model.Endpoint{},
		journeys:  map[string][]model.Journey{},
		next:      map[string][]model.Continuation{},
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

	var out []model.SystemJourney
	for _, m := range ms {
		for _, j := range m.Journeys {
			entry := c.endpoints[m.Service+"|"+j.Entrypoint]
			sj := model.SystemJourney{
				Service: m.Service, Entrypoint: j.Entrypoint, Transport: j.Transport, Key: j.Key,
				Kind: entry.InteractionKind(), Handler: j.Handler,
			}
			root := m.Service + "|" + j.Key
			sj.Hops = c.hops(m.Service, j, 1, map[string]bool{root: true}, map[string]int{root: 0})
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
// exponentially with depth.
func (c *chainer) hops(service string, j model.Journey, depth int, path map[string]bool, expanded map[string]int) []model.Hop {
	var out []model.Hop
	for _, o := range j.Outbound {
		ep, ok := c.endpoints[service+"|"+o.Endpoint]
		if !ok {
			continue
		}
		base := model.Hop{
			From: service, Transport: ep.Transport, Role: ep.Role, Key: strings.Join(ep.Keys, ", "),
			Message: ep.Message, Kind: o.Kind, Background: o.Background, FromEndpoint: ep.ID,
		}
		next := c.next[service+"|"+ep.ID]
		if len(next) == 0 {
			base.External = true
			out = append(out, base)
			continue
		}
		for _, s := range next {
			l := s.Link
			h := base
			h.To, h.Key, h.Kind, h.Confidence, h.ToEndpoint = l.To, l.Key, l.Kind, l.Confidence, l.ToEndpoint
			targets := c.targets(l, s.Journeys)
			if len(targets) == 0 {
				out = append(out, h)
				continue
			}
			for _, tj := range targets {
				t := h
				t.Key, t.Journey, t.Handler = tj.Key, tj.Key, tj.Handler
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
					t.Next = c.hops(l.To, tj, depth+1, path, expanded)
					delete(path, id)
				}
				out = append(out, t)
			}
		}
	}
	return out
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
