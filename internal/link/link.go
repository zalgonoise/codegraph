// Package link connects service manifests into a system view by matching the
// tags of outbound endpoints (clients, producers) against inbound endpoints
// (servers, consumers) of other services, per transport:
//
//   - http: the client URL host must name the target service (name, alias or
//     tags["host"]) and the path must match a route ({id}, {rest...}, :id, *);
//     an unresolved host still links on the path, marked "path-only".
//   - grpc: a call keyed "Service/Method" (or a client keyed "Service")
//     matches a server registered for Service, of the same proto service
//     (contract) when both sides know it.
//   - amqp: "exchange routingKey" matches a queue named like the routing key.
//   - everything else: exact key match, or glob match where "*" marks the
//     parts the resolver could not determine.
//
// SQL tables and Redis key patterns used by services of different modules
// become "shared-store" links with confidence "name-only": the name is the
// only evidence that it is the same database or Redis. Mains of one module
// sharing their stores is the normal case and not reported.
//
// Unmatched endpoints are dangling, with a reason when they are not orphans:
// a counterpart in the same service, a broker-fed retry or dead-letter queue,
// or the same name on another messaging transport (dangling.go).
//
// Journeys are then chained: when an outbound endpoint of a journey links to
// another service's inbound endpoint, the chain continues with that
// endpoint's journey (see journeys.go).
package link

import (
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/zalgonoise/codegraph/internal/model"
)

// Link builds the system graph. Libraries (Manifest.Library) are listed as
// services but never linked: their endpoints are what their API can do, not
// what runs.
func Link(all []*model.Manifest) *model.System {
	sys := &model.System{}
	var ms []*model.Manifest
	for _, m := range all {
		if !m.Library {
			ms = append(ms, m)
		}
	}
	for _, m := range all {
		svc := model.SystemService{Name: m.Service, Module: m.Module, Aliases: m.Aliases, Tags: m.Tags, Library: m.Library}
		for _, ep := range m.Endpoints {
			if ep.Direction == model.Inbound {
				svc.Inbound++
			} else {
				svc.Outbound++
			}
		}
		sys.Services = append(sys.Services, svc)
	}

	seen := map[string]bool{}
	matchedIn, matchedOut := map[string]bool{}, map[string]bool{}
	byOut := map[string][]model.Link{} // service|outbound endpoint ID -> every match
	for _, a := range ms {
		for _, out := range a.Endpoints {
			if out.Direction != model.Outbound || !initiates(out.Role) {
				continue
			}
			matched := false
			for _, b := range ms {
				self := b.Service == a.Service
				if self && !gatewayCall(out) {
					continue
				}
				for _, in := range b.Endpoints {
					if in.Direction != model.Inbound || !accepts(in.Role) || in.Transport != out.Transport {
						continue
					}
					key, conf, ok := match(out, in, b)
					if !ok {
						continue
					}
					matched = true
					matchedIn[b.Service+"|"+in.ID] = true
					l := model.Link{
						From: a.Service, To: b.Service, Transport: out.Transport, Key: key,
						Kind: out.InteractionKind(), Confidence: conf, FromEndpoint: out.ID, ToEndpoint: in.ID,
					}
					if self {
						// A grpc-gateway route forwarding to its own service's server:
						// its journey continues into the method's, but no link joins a
						// service to itself.
						l.Confidence = "gateway"
						byOut[a.Service+"|"+out.ID] = append(byOut[a.Service+"|"+out.ID], l)
						continue
					}
					byOut[a.Service+"|"+out.ID] = append(byOut[a.Service+"|"+out.ID], l)
					if id := l.From + "|" + l.To + "|" + l.Transport + "|" + l.Key; !seen[id] {
						seen[id] = true
						sys.Links = append(sys.Links, l)
					}
				}
			}
			if !matched {
				sys.Dangling = append(sys.Dangling, dangling(a.Service, out))
			} else {
				matchedOut[a.Service+"|"+out.ID] = true
			}
		}
	}
	// Consumers nobody (among the analysed services) produces for.
	for _, m := range ms {
		for _, in := range m.Endpoints {
			if in.Direction == model.Inbound && in.Role == "consumer" && !matchedIn[m.Service+"|"+in.ID] {
				sys.Dangling = append(sys.Dangling, dangling(m.Service, in))
			}
		}
	}
	classify(sys, ms)
	sys.Dangling = append(sys.Dangling, unlinkedKeys(ms, matchedIn, matchedOut)...)
	sys.Links = append(sys.Links, sharedStores(ms)...)

	sort.SliceStable(sys.Links, func(i, j int) bool {
		a, b := sys.Links[i], sys.Links[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.To != b.To {
			return a.To < b.To
		}
		return a.Key < b.Key
	})
	sys.Journeys, sys.Continuations = chain(ms, byOut, maxHops)
	return sys
}

func dangling(service string, ep model.Endpoint) model.Dangling {
	return model.Dangling{
		Service: service, Transport: ep.Transport, Role: ep.Role, Direction: ep.Direction,
		Key: strings.Join(ep.Keys, ", "), Endpoint: ep.ID,
	}
}

// gatewayCall reports whether an outbound gRPC call is made by generated
// grpc-gateway code (a *.pb.gw.go file): the call a gateway route forwards,
// which may reach the service's own server.
func gatewayCall(ep model.Endpoint) bool {
	if ep.Transport != "grpc" {
		return false
	}
	for _, s := range ep.Sites {
		if strings.Contains(s.Position, ".pb.gw.go:") {
			return true
		}
	}
	return strings.Contains(ep.Position, ".pb.gw.go:")
}

func initiates(role string) bool { return role == "client" || role == "producer" }
func accepts(role string) bool   { return role == "server" || role == "consumer" }

// match reports whether out reaches in, with the inbound key(s) it matches
// ("a, b" when several do) and the weakest confidence among them.
func match(out, in model.Endpoint, target *model.Manifest) (key, confidence string, ok bool) {
	switch out.Transport {
	case "http":
		return matchHTTP(out.Keys, in.Keys, hostNames(target))
	case "grpc":
		// Calls are keyed "Service/Method", servers by service; when both
		// sides know the proto service (notify.v1 vs notify.v2), it must agree.
		if out.Contract != "" && in.Contract != "" && out.Contract != in.Contract {
			return "", "", false
		}
		return matchAll(out.Keys, in.Keys, func(o string) string {
			svc, _, _ := strings.Cut(o, "/")
			return svc
		})
	case "amqp":
		// "exchange routingKey" reaches a queue named like the routing key.
		return matchAll(out.Keys, in.Keys, func(o string) string {
			if f := strings.Fields(o); len(f) > 0 {
				return f[len(f)-1]
			}
			return ""
		})
	}
	return matchAll(out.Keys, in.Keys, func(o string) string { return o })
}

// matchAll matches every outbound key (as seen by the inbound side) against
// every inbound key.
func matchAll(outs, ins []string, view func(string) string) (key, confidence string, ok bool) {
	var keys []string
	confidence = "exact"
	for _, i := range ins {
		for _, o := range outs {
			if conf, matched := matchKey(view(o), i); matched {
				keys = append(keys, i)
				if conf != "exact" {
					confidence = conf
				}
				break
			}
		}
	}
	if len(keys) == 0 {
		return "", "", false
	}
	return strings.Join(keys, ", "), confidence, true
}

func matchKey(a, b string) (string, bool) {
	if a == "" || b == "" || a == model.Unknown || b == model.Unknown {
		return "", false
	}
	if a == b {
		return "exact", true
	}
	if glob(a, b) || glob(b, a) {
		return "pattern", true
	}
	return "", false
}

func glob(pattern, s string) bool {
	if !strings.Contains(pattern, model.Unknown) {
		return false
	}
	parts := strings.Split(pattern, model.Unknown)
	for i := range parts {
		parts[i] = regexp.QuoteMeta(parts[i])
	}
	re, err := regexp.Compile("^" + strings.Join(parts, ".*") + "$")
	return err == nil && re.MatchString(s)
}

// --- HTTP --------------------------------------------------------------------

func hostNames(m *model.Manifest) map[string]bool {
	names := map[string]bool{strings.ToLower(m.Service): true}
	for _, a := range m.Aliases {
		names[strings.ToLower(a)] = true
	}
	if h := m.Tags["host"]; h != "" {
		names[strings.ToLower(h)] = true
	}
	return names
}

func matchHTTP(outs, ins []string, hosts map[string]bool) (string, string, bool) {
	for _, o := range outs {
		om, host, opath := parseClientKey(o)
		hostKnown := host != "" && !strings.Contains(host, model.Unknown)
		if hostKnown && !hosts[host] {
			continue
		}
		if !hostKnown && !hasLiteralSegment(opath) {
			continue // "*/*" from a generic client would match every route of every service
		}
		for _, i := range ins {
			im, ipath := parseRoute(i)
			if om != "" && im != "" && om != im {
				continue
			}
			if !hostKnown && (ipath == "/" || !hasLiteralSegment(ipath)) {
				continue // nor would a catch-all or unresolved route
			}
			if !matchPath(ipath, opath) {
				continue
			}
			conf := "exact"
			switch {
			case !hostKnown:
				conf = "path-only"
			case strings.Contains(opath, model.Unknown):
				conf = "pattern"
			}
			return i, conf, true
		}
	}
	return "", "", false
}

// parseClientKey splits "GET http://inventory:8081/items/*" into its parts.
func parseClientKey(k string) (method, host, path string) {
	if f := strings.Fields(k); len(f) == 2 && isMethod(f[0]) {
		method, k = f[0], f[1]
	}
	if i := strings.Index(k, "://"); i >= 0 {
		k = k[i+3:]
	}
	if strings.HasPrefix(k, "/") {
		return method, "", stripQuery(k)
	}
	host, rest, found := strings.Cut(k, "/")
	path = "/"
	if found {
		path = "/" + rest
	}
	host, _, _ = strings.Cut(host, ":")
	return method, strings.ToLower(host), stripQuery(path)
}

// parseRoute splits "GET /items/{id}" (Go 1.22 ServeMux style) into its parts.
func parseRoute(k string) (method, path string) {
	if f := strings.Fields(k); len(f) == 2 && isMethod(f[0]) {
		method, k = f[0], f[1]
	}
	if !strings.HasPrefix(k, "/") && !strings.HasPrefix(k, model.Unknown) {
		if _, rest, ok := strings.Cut(k, "/"); ok { // "host/path" patterns
			k = "/" + rest
		}
	}
	return method, k
}

func matchPath(route, p string) bool {
	if route == "/" || route == model.Unknown {
		return true
	}
	rs := strings.Split(strings.Trim(route, "/"), "/")
	ps := strings.Split(strings.Trim(p, "/"), "/")
	for i, r := range rs {
		if strings.HasPrefix(r, "{") && strings.HasSuffix(r, "...}") {
			return true
		}
		if i >= len(ps) {
			return false
		}
		seg := ps[i]
		switch {
		case seg == model.Unknown, r == seg, r == model.Unknown, strings.HasPrefix(r, ":"),
			strings.HasPrefix(r, "{") && strings.HasSuffix(r, "}"):
			continue
		case strings.Contains(seg, model.Unknown) && glob(seg, r):
			continue
		}
		return false
	}
	return len(ps) == len(rs) || strings.HasSuffix(route, "/")
}

// hasLiteralSegment reports whether a path has a segment that is neither
// empty nor partly unresolved.
func hasLiteralSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg != "" && !strings.Contains(seg, model.Unknown) {
			return true
		}
	}
	return false
}

func isMethod(s string) bool {
	switch s {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		return true
	}
	return false
}

func stripQuery(p string) string {
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		return p[:i]
	}
	return p
}

// --- shared stores -------------------------------------------------------------

// named reports whether a store key names something: a key of wildcards and
// separators only ("*", "*:*:*") would make unrelated stores look shared.
func named(key string) bool {
	return strings.ContainsFunc(key, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) })
}

func sharedStores(ms []*model.Manifest) []model.Link {
	type user struct{ service, module, owner string }
	type store struct{ transport, key string }
	users := map[store][]user{}
	for _, m := range ms {
		seen := map[user]map[store]bool{}
		for _, ep := range m.Endpoints {
			if (ep.Transport != "sql" && ep.Transport != "redis") || ep.Role != "query" {
				continue
			}
			u := user{m.Service, m.Module, ep.Owner}
			if seen[u] == nil {
				seen[u] = map[store]bool{}
			}
			for _, k := range ep.Keys {
				st := store{ep.Transport, k}
				if named(k) && !seen[u][st] {
					seen[u][st] = true
					users[st] = append(users[st], u)
				}
			}
		}
	}
	// A store with an owner (a declared service's table in another repository's
	// database) is shared with that repository's services, and with services
	// declaring the same owner, only.
	owns := func(a, b user) bool { return a.owner == "" || a.owner == repoName(b.module) || a.owner == b.owner }
	var out []model.Link
	for st, us := range users {
		sort.Slice(us, func(i, j int) bool { return us[i].service < us[j].service })
		linked := map[[2]string]bool{}
		for i := 0; i < len(us); i++ {
			for j := i + 1; j < len(us); j++ {
				pair := [2]string{us[i].service, us[j].service}
				if us[i].module == us[j].module || linked[pair] || !owns(us[i], us[j]) || !owns(us[j], us[i]) {
					continue
				}
				linked[pair] = true
				out = append(out, model.Link{
					From: us[i].service, To: us[j].service, Transport: st.transport, Key: st.key,
					Kind: "shared-store", Confidence: "name-only",
				})
			}
		}
	}
	return out
}

// repoName is a module's repository: its last element, without a /vN suffix
// (a declared service's module is its repository's name).
func repoName(module string) string {
	parts := strings.Split(module, "/")
	if n := len(parts); n > 1 && strings.HasPrefix(parts[n-1], "v") && strings.Trim(parts[n-1][1:], "0123456789") == "" {
		return parts[n-2]
	}
	return parts[len(parts)-1]
}
