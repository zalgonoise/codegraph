package link

import (
	"regexp"
	"sort"
	"strings"

	"github.com/zalgonoise/codegraph/internal/model"
)

// messaging transports carry named topics or queues, so the same name on two
// of them is the same channel in transition (Kafka topics moving to RabbitMQ).
var messaging = map[string]bool{"kafka": true, "amqp": true, "nats": true}

// brokerQueue matches the queues a broker library feeds itself:
// <queue>.retry and <queue>.dlq, and delayed retries (<queue>.delay.<ttl>).
var brokerQueue = regexp.MustCompile(`\.(retry|dlq|delay(\.[^.]+)?)$`)

// classify sets the Reason of each unmatched endpoint that is not an orphan.
func classify(sys *model.System, ms []*model.Manifest) {
	byService := map[string]*model.Manifest{}
	for _, m := range ms {
		byService[m.Service] = m
	}
	others := messagingTransports()
	for i, d := range sys.Dangling {
		m := byService[d.Service]
		if m == nil {
			continue
		}
		for _, ep := range m.Endpoints {
			if ep.ID == d.Endpoint {
				sys.Dangling[i].Reason = reason(ms, m, ep, others)
				break
			}
		}
	}
}

func messagingTransports() []string {
	var out []string
	for t := range messaging {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// unlinkedKeys reports the keys nothing answers of the messaging endpoints that
// link through their other keys (a consumer registering several queues, one of
// them never published to): one partial entry per reason, keys in order.
// Without it, a registration linked by one queue hid every other one.
func unlinkedKeys(ms []*model.Manifest, matchedIn, matchedOut map[string]bool) []model.Dangling {
	others := messagingTransports()
	var out []model.Dangling
	for _, m := range ms {
		for _, ep := range m.Endpoints {
			id := m.Service + "|" + ep.ID
			if !messaging[ep.Transport] || len(ep.Keys) < 2 || !matchedIn[id] && !matchedOut[id] {
				continue
			}
			var order []string
			byReason := map[string][]string{}
			for _, k := range ep.Keys {
				probe := ep
				probe.Keys = []string{k}
				if k == model.Unknown || keyLinked(ms, m, probe) {
					continue
				}
				r := reason(ms, m, probe, others)
				if _, ok := byReason[r]; !ok {
					order = append(order, r)
				}
				byReason[r] = append(byReason[r], k)
			}
			for _, r := range order {
				d := dangling(m.Service, ep)
				d.Key, d.Reason, d.Partial = strings.Join(byReason[r], ", "), r, true
				out = append(out, d)
			}
		}
	}
	return out
}

// keyLinked reports whether another service answers ep (one key of an
// endpoint owned by owner), as a link would.
func keyLinked(ms []*model.Manifest, owner *model.Manifest, ep model.Endpoint) bool {
	for _, m := range ms {
		if m != owner && counterpart(m, owner, ep, ep.Transport) {
			return true
		}
	}
	return false
}

func reason(ms []*model.Manifest, owner *model.Manifest, ep model.Endpoint, others []string) string {
	if counterpart(owner, owner, ep, ep.Transport) {
		return model.DanglingSameService
	}
	if !messaging[ep.Transport] {
		return ""
	}
	broker := len(ep.Keys) > 0
	for _, k := range ep.Keys {
		broker = broker && brokerQueue.MatchString(k)
	}
	if broker {
		return model.DanglingBroker
	}
	for _, t := range others {
		if t == ep.Transport {
			continue
		}
		for _, m := range ms {
			if counterpart(m, owner, ep, t) {
				return model.DanglingOtherTransport
			}
		}
	}
	return ""
}

// counterpart reports whether m has an endpoint on transport t that answers
// ep (owned by owner): a server or consumer for an outbound ep, a client or
// producer for an inbound one.
func counterpart(m, owner *model.Manifest, ep model.Endpoint, t string) bool {
	probe := ep
	probe.Transport = t
	for _, other := range m.Endpoints {
		if other.Transport != t || (m == owner && other.ID == ep.ID) {
			continue
		}
		switch {
		case ep.Direction == model.Outbound && other.Direction == model.Inbound && accepts(other.Role):
			if _, _, ok := match(probe, other, m); ok {
				return true
			}
		case ep.Direction == model.Inbound && other.Direction == model.Outbound && initiates(other.Role):
			if _, _, ok := match(other, probe, owner); ok {
				return true
			}
		}
	}
	return false
}
