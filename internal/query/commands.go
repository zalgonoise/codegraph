package query

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/zalgonoise/codegraph/internal/deploy"
	"github.com/zalgonoise/codegraph/internal/model"
	"github.com/zalgonoise/codegraph/internal/symbols"
)

// Limits keep answers small enough to read (and to fit an agent's context).
const (
	maxRows  = 60  // rows per list
	maxSteps = 250 // steps in one journey answer
	maxPaths = 5
	maxHops  = 6 // as link chains journeys
)

// ---- services -----------------------------------------------------------------

// ServiceSummary is one row of Services.
type ServiceSummary struct {
	Name       string         `json:"name"`
	Module     string         `json:"module"`
	Declared   string         `json:"declared,omitempty"` // the language of a service described by hand
	Library    bool           `json:"library,omitempty"`  // a library, analysed through its exported API
	Journeys   map[string]int `json:"journeys"`           // per transport
	Upstream   int            `json:"upstream"`           // services linking into it
	Downstream int            `json:"downstream"`
}

// Services lists every service with the journeys it offers.
func (r *Run) Services() []ServiceSummary {
	up, down := map[string]map[string]bool{}, map[string]map[string]bool{}
	for _, l := range r.System.Links {
		if l.Kind == "shared-store" {
			continue
		}
		add(up, l.To, l.From)
		add(down, l.From, l.To)
	}
	var out []ServiceSummary
	for _, m := range r.Manifests {
		s := ServiceSummary{Name: m.Service, Module: m.Module, Declared: declaredLanguage(m), Library: m.Library, Journeys: map[string]int{},
			Upstream: len(up[m.Service]), Downstream: len(down[m.Service])}
		for _, j := range m.Journeys {
			if r.endpoints[m.Service+"|"+j.Entrypoint].Role == model.APIInternalRole {
				s.Journeys["go internal"]++
			} else {
				s.Journeys[j.Transport]++
			}
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func add(m map[string]map[string]bool, k, v string) {
	if m[k] == nil {
		m[k] = map[string]bool{}
	}
	m[k][v] = true
}

// ---- service ------------------------------------------------------------------

// ServiceDetail is the answer of Service.
type ServiceDetail struct {
	Name       string          `json:"name"`
	Module     string          `json:"module"`
	Declared   *model.Declared `json:"declared,omitempty"` // described by hand, not analysed
	Library    bool            `json:"library,omitempty"`  // a library: its journeys are its exported API's
	Journeys   []JourneyRef    `json:"journeys"`
	Upstream   []model.Link    `json:"upstream"`           // links into it
	Downstream []model.Link    `json:"downstream"`         // links out of it
	Shares     []model.Link    `json:"shares"`             // tables and Redis keys it shares with other repositories (name-only)
	Unmatched  []EndpointRef   `json:"unmatched"`          // endpoints no analysed service answers
	Deployed   *Deployed       `json:"deployed,omitempty"` // when the run has a deploy.json
}

// Deployed is what an environment runs of a service (see deploy.ServiceView).
type (
	Deployed         = deploy.ServiceView
	DeployedWorkload = deploy.WorkloadView
	Addresses        = deploy.Addresses
)

// JourneyRef names a journey: an inbound endpoint and its handler.
type JourneyRef struct {
	Service   string   `json:"service"`
	Transport string   `json:"transport"`
	Key       string   `json:"key"`
	Handler   string   `json:"handler,omitempty"`
	Position  string   `json:"position,omitempty"` // the handler's root, in its repository
	Funcs     int      `json:"funcs"`
	Distance  int      `json:"distance,omitempty"` // hops, in impact answers
	Via       string   `json:"via,omitempty"`      // impact: the last link, into the target
	Chain     []string `json:"chain,omitempty"`    // impact: the services it crosses, from its own to the target
	Internal  bool     `json:"internal,omitempty"` // a library's internal API: a symbol of an internal package
}

// WholeService reports a gRPC journey that covers every method of a service
// (the handler's methods could not be told apart).
func (j JourneyRef) WholeService() bool {
	return j.Transport == "grpc" && !strings.Contains(j.Key, "/")
}

// Label names a journey with its transport, since one key can start a journey
// on two transports (a topic and a queue of the same name).
func (j JourneyRef) Label() string { return j.Transport + " " + j.Key }

// EndpointRef is an endpoint of a service.
type EndpointRef struct {
	Service   string   `json:"service"`
	Direction string   `json:"direction"`
	Transport string   `json:"transport"`
	Role      string   `json:"role"`
	Key       string   `json:"key"`
	Handler   string   `json:"handler,omitempty"`
	Message   string   `json:"message,omitempty"`
	Position  string   `json:"position,omitempty"` // the call site; for inbound endpoints, where the handler is registered
	Sites     []string `json:"sites,omitempty"`    // every call site merged into the endpoint, when more than one
	Unmatched bool     `json:"unmatched,omitempty"`
	Reason    string   `json:"reason,omitempty"`  // for unmatched endpoints: why it is not an orphan
	Partial   bool     `json:"partial,omitempty"` // only Key is unmatched: the endpoint's other keys link
}

// Service describes one service: its journeys, neighbours and unmatched endpoints.
func (r *Run) Service(name string) (*ServiceDetail, error) {
	m, err := r.service(name)
	if err != nil {
		return nil, err
	}
	d := &ServiceDetail{Name: m.Service, Module: m.Module, Declared: m.Declared, Library: m.Library}
	for i, jr := range r.journeys {
		if jr.service == m.Service {
			d.Journeys = append(d.Journeys, r.ref(i))
		}
	}
	for _, l := range r.System.Links {
		switch {
		case l.Kind == "shared-store" && (l.From == m.Service || l.To == m.Service):
			d.Shares = append(d.Shares, l)
		case l.To == m.Service:
			d.Upstream = append(d.Upstream, l)
		case l.From == m.Service:
			d.Downstream = append(d.Downstream, l)
		}
	}
	for _, dg := range r.System.Dangling {
		if dg.Service == m.Service {
			ref := r.endpointRef(m.Service, dg.Endpoint)
			ref.Key, ref.Reason, ref.Unmatched, ref.Partial = dg.Key, dg.Reason, true, dg.Partial
			d.Unmatched = append(d.Unmatched, ref)
		}
	}
	if r.Deploy != nil {
		d.Deployed = r.deployed(m.Service)
	}
	return d, nil
}

func (r *Run) deployed(service string) *Deployed { return r.Deploy.View(service) }

func (r *Run) ref(i int) JourneyRef {
	jr := r.journeys[i]
	ref := JourneyRef{Service: jr.service, Transport: jr.j.Transport, Key: jr.j.Key, Handler: jr.j.Handler, Funcs: jr.j.Funcs,
		Internal: r.endpoints[jr.service+"|"+jr.j.Entrypoint].Role == model.APIInternalRole}
	switch { // the handler a middleware root wraps, rather than the middleware
	case len(jr.j.Wrapped) > 0:
		ref.Position = jr.j.Wrapped[0].Position
	case len(jr.j.Roots) > 0:
		ref.Position = jr.j.Roots[0].Position
	}
	return ref
}

func (r *Run) endpointRef(service, id string) EndpointRef {
	ep := r.endpoints[service+"|"+id]
	ref := EndpointRef{Service: service, Direction: string(ep.Direction), Transport: ep.Transport, Role: ep.Role,
		Key: keys(ep), Handler: ep.Handler, Message: ep.Message, Position: ep.Position}
	if len(ep.Sites) > 1 {
		for _, s := range ep.Sites {
			ref.Sites = append(ref.Sites, s.Position)
		}
	}
	ref.Reason, ref.Unmatched = r.dangling[service+"|"+id]
	return ref
}

// ---- journey ------------------------------------------------------------------

// JourneyDetail is the answer of Journey.
type JourneyDetail struct {
	JourneyRef
	Kind       string            `json:"kind"` // sync for routes and methods, async for consumers
	Components []string          `json:"components"`
	Uses       []model.SymbolUse `json:"uses,omitempty"`   // the libraries' exported API its own code calls
	UsedBy     []JourneyUse      `json:"usedBy,omitempty"` // a library's exported symbol: the journeys calling it
	Steps      []Step            `json:"steps"`
	Truncated  bool              `json:"truncated,omitempty"` // more steps than an answer holds
}

// Step is an outbound call of a journey, in program order, and the steps of
// the journey it continues into in another service.
type Step struct {
	Transport  string `json:"transport"`
	Role       string `json:"role"`
	Key        string `json:"key"`
	Kind       string `json:"kind"`
	Position   string `json:"position,omitempty"` // the call site, in the calling service's repository
	Background bool   `json:"background,omitempty"`
	To         string `json:"to,omitempty"` // the service it reaches
	Confidence string `json:"confidence,omitempty"`
	Journey    string `json:"journey,omitempty"` // the journey it continues into
	Handler    string `json:"handler,omitempty"`
	HandlerAt  string `json:"handlerAt,omitempty"` // the target handler's position, in the target's repository
	Unmatched  string `json:"unmatched,omitempty"` // when nothing analysed answers it: why
	Cycle      bool   `json:"cycle,omitempty"`     // the journey is already on the path
	Repeated   bool   `json:"repeated,omitempty"`  // followed earlier in this answer
	Deeper     bool   `json:"deeper,omitempty"`    // not followed: past the depth asked
	Steps      []Step `json:"steps,omitempty"`
}

// Journey describes a journey of a service (its key, or part of it when
// unambiguous, optionally after its transport: "amqp orders.created") and
// follows it depth services beyond its own (0: its own calls only).
func (r *Run) Journey(service, key string, depth int) (*JourneyDetail, error) {
	m, err := r.service(service)
	if err != nil {
		return nil, err
	}
	transport := ""
	if t, rest, ok := strings.Cut(key, " "); ok && transports[t] {
		transport, key = t, rest
	}
	var exact, near []int
	for i, jr := range r.journeys {
		if jr.service != m.Service || (transport != "" && jr.j.Transport != transport) {
			continue
		}
		switch {
		case jr.j.Key == key:
			exact = append(exact, i)
		case strings.Contains(strings.ToLower(jr.j.Key), strings.ToLower(key)):
			near = append(near, i)
		}
	}
	if len(exact) == 0 {
		exact = near
	}
	switch {
	case len(exact) == 0:
		return nil, fmt.Errorf("%s has no journey %q (`codegraph query service %s` lists them)", m.Service, key, m.Service)
	case len(exact) > 1:
		var names []string
		for _, i := range exact {
			names = append(names, r.ref(i).Label())
		}
		return nil, fmt.Errorf("%q matches several journeys of %s: %s (name one, with its transport first if needed)",
			key, m.Service, strings.Join(names, "; "))
	}
	i := exact[0]
	d := &JourneyDetail{JourneyRef: r.ref(i), Kind: r.endpoints[m.Service+"|"+r.journeys[i].j.Entrypoint].InteractionKind()}
	for _, c := range r.journeys[i].j.Components {
		d.Components = append(d.Components, symbols.Short(c, m.Module))
	}
	d.Uses = r.journeys[i].j.Uses
	d.UsedBy = r.apiUsers[i]
	budget := maxSteps
	d.Steps = r.steps(i, 1, max(depth, 0)+1, map[int]bool{i: true}, map[int]int{i: 0}, &budget)
	d.Truncated = budget < 0
	return d, nil
}

func (r *Run) steps(i, depth, maxDepth int, path map[int]bool, done map[int]int, budget *int) []Step {
	jr := r.journeys[i]
	var out []Step
	for _, o := range jr.j.Outbound {
		ep, ok := r.endpoints[jr.service+"|"+o.Endpoint]
		if !ok {
			continue
		}
		if *budget--; *budget < 0 {
			return out
		}
		base := Step{Transport: ep.Transport, Role: ep.Role, Key: keys(ep), Kind: o.Kind, Position: ep.Position,
			Background: o.Background}
		conts := r.next(jr.service, o.Endpoint)
		if len(conts) == 0 {
			base.Unmatched = r.unmatched(jr.service, ep)
			out = append(out, base)
			continue
		}
		for _, c := range conts {
			s := base
			s.To, s.Key, s.Kind, s.Confidence = c.To, c.Key, c.Kind, c.Confidence
			if len(c.journeys) == 0 {
				out = append(out, s)
				continue
			}
			for _, t := range c.journeys {
				ts := s
				ts.Journey, ts.Handler, ts.HandlerAt = r.journeys[t].j.Key, r.journeys[t].j.Handler, r.ref(t).Position
				d, seen := done[t]
				switch {
				case path[t]:
					ts.Cycle = true
				case seen && d <= depth:
					ts.Repeated = true
				case depth >= maxDepth:
					ts.Deeper = true
				default:
					path[t], done[t] = true, depth
					ts.Steps = r.steps(t, depth+1, maxDepth, path, done, budget)
					delete(path, t)
				}
				out = append(out, ts)
			}
		}
	}
	return out
}

// unmatched says why an outbound endpoint with no continuation goes nowhere.
func (r *Run) unmatched(service string, ep model.Endpoint) string {
	switch reason := r.dangling[service+"|"+ep.ID]; {
	case reason != "":
		return reason
	case ep.Transport == "sql" || ep.Transport == "redis":
		return "store"
	case ep.Role == "producer":
		return "no consumer among the analysed services"
	case ep.Role == "client":
		return "outside the analysed services"
	}
	return ""
}

// ---- who ----------------------------------------------------------------------

// KeyUsers is the answer of Who for one transport and key.
type KeyUsers struct {
	Transport string `json:"transport"`
	Key       string `json:"key"`
	Senders   []Use  `json:"senders"`   // producers and clients
	Receivers []Use  `json:"receivers"` // consumers and servers
	Users     []Use  `json:"users"`     // stores: queries
}

// Use is an endpoint of a key and the journeys using it, or starting there.
type Use struct {
	EndpointRef
	HandlerPosition string   `json:"handlerPosition,omitempty"` // for inbound endpoints: the handler itself
	Journeys        []string `json:"journeys,omitempty"`        // "transport key"
}

// Who finds who sends, receives or uses a key (a topic, queue, table, route or
// gRPC service): exactly, or containing it when nothing matches exactly.
func (r *Run) Who(key string) []KeyUsers {
	byKey := map[string]*KeyUsers{}
	var order []string
	for _, id := range r.keyEndpoints(key) {
		svc, epID := split(id)
		ep := r.endpoints[id]
		k := ep.Transport + " " + keys(ep)
		ku := byKey[k]
		if ku == nil {
			ku = &KeyUsers{Transport: ep.Transport, Key: keys(ep)}
			byKey[k] = ku
			order = append(order, k)
		}
		use := Use{EndpointRef: r.endpointRef(svc, epID)}
		js := r.usedBy[id]
		if ep.Direction == model.Inbound {
			js = r.byEntry[id]
		}
		for _, i := range js {
			ref := r.ref(i)
			use.Journeys = append(use.Journeys, ref.Label())
			if ep.Direction == model.Inbound && use.HandlerPosition == "" {
				use.HandlerPosition = ref.Position
			}
		}
		switch {
		case ep.Transport == "sql" || ep.Transport == "redis":
			ku.Users = append(ku.Users, use)
		case ep.Direction == model.Inbound:
			ku.Receivers = append(ku.Receivers, use)
		default:
			ku.Senders = append(ku.Senders, use)
		}
	}
	sort.Strings(order)
	var out []KeyUsers
	for _, k := range order {
		out = append(out, *byKey[k])
		if len(out) == maxRows {
			break
		}
	}
	return out
}

// ---- path ---------------------------------------------------------------------

// Path is a chain of links from one service to another; each hop lists every
// link between the two services.
type Path [][]model.Link

// Paths finds the shortest chains of links from one service to another (and
// those one hop longer), up to maxPaths.
func (r *Run) Paths(from, to string) ([]Path, error) {
	a, err := r.service(from)
	if err != nil {
		return nil, err
	}
	b, err := r.service(to)
	if err != nil {
		return nil, err
	}
	edges := map[string]map[string][]model.Link{}
	for _, l := range r.System.Links {
		if l.Kind == "shared-store" {
			continue
		}
		if edges[l.From] == nil {
			edges[l.From] = map[string][]model.Link{}
		}
		edges[l.From][l.To] = append(edges[l.From][l.To], l)
	}
	shortest := r.distance(edges, a.Service, b.Service)
	if shortest < 0 {
		return nil, nil
	}
	var out []Path
	var walk func(at string, seen map[string]bool, path Path)
	walk = func(at string, seen map[string]bool, path Path) {
		if len(out) >= maxPaths || len(path) > shortest+1 {
			return
		}
		if at == b.Service {
			out = append(out, append(Path(nil), path...))
			return
		}
		for _, next := range sortedKeys(edges[at]) {
			if !seen[next] {
				seen[next] = true
				walk(next, seen, append(path, edges[at][next]))
				delete(seen, next)
			}
		}
	}
	walk(a.Service, map[string]bool{a.Service: true}, nil)
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) < len(out[j]) })
	return out, nil
}

func (r *Run) distance(edges map[string]map[string][]model.Link, from, to string) int {
	dist := map[string]int{from: 0}
	queue := []string{from}
	for len(queue) > 0 {
		at := queue[0]
		queue = queue[1:]
		if at == to {
			return dist[at]
		}
		for next := range edges[at] {
			if _, ok := dist[next]; !ok {
				dist[next] = dist[at] + 1
				queue = append(queue, next)
			}
		}
	}
	return -1
}

// ---- impact -------------------------------------------------------------------

// Impact is what a change to a service, or to a key, would affect.
type Impact struct {
	Target   string       `json:"target"`
	Kind     string       `json:"kind"`               // service | key
	Direct   []model.Link `json:"direct,omitempty"`   // links into the service
	Shares   []model.Link `json:"shares,omitempty"`   // tables and Redis keys it shares (name-only)
	Users    []KeyUsers   `json:"users,omitempty"`    // endpoints of the key
	Journeys []JourneyRef `json:"journeys,omitempty"` // journeys of other services that reach it, nearest first
}

// Impact lists what depends on a service (target names one) or on a key.
func (r *Run) Impact(target string) (*Impact, error) {
	if m, ok := r.services[target]; ok {
		im := &Impact{Target: m.Service, Kind: "service"}
		for _, l := range r.System.Links {
			switch {
			case l.Kind == "shared-store" && (l.From == m.Service || l.To == m.Service):
				im.Shares = append(im.Shares, l)
			case l.To == m.Service:
				im.Direct = append(im.Direct, l)
			}
		}
		targets := map[int]bool{}
		for i, jr := range r.journeys {
			if jr.service == m.Service {
				targets[i] = true
			}
		}
		im.Journeys = r.reaching(targets, m.Service)
		return im, nil
	}
	users := r.Who(target)
	if len(users) == 0 {
		_, err := r.service(target)
		return nil, fmt.Errorf("%q is neither a service nor a key: %v", target, err)
	}
	im := &Impact{Target: target, Kind: "key", Users: users}
	targets := map[int]bool{}
	for _, id := range r.keyEndpoints(target) {
		for _, i := range append(r.usedBy[id], r.byEntry[id]...) {
			targets[i] = true
		}
	}
	im.Journeys = r.reaching(targets, "")
	return im, nil
}

// reaching lists the journeys that are, or reach, one of targets within
// maxHops (except those of the service skip), nearest first.
func (r *Run) reaching(targets map[int]bool, skip string) []JourneyRef {
	var out []JourneyRef
	for i, jr := range r.journeys {
		if jr.service == skip {
			continue
		}
		best, reached := -1, -1
		dist, via, from := r.reachable(i, maxHops)
		for t, d := range dist {
			if targets[t] && (best < 0 || d < best || (d == best && linkLess(via[t], via[reached]))) {
				best, reached = d, t
			}
		}
		if best >= 0 {
			ref := r.ref(i)
			ref.Distance = best
			if best > 0 {
				ref.Via = via[reached].Transport + " " + via[reached].Key
				for t := reached; t != i; t = from[t] {
					ref.Chain = append([]string{r.journeys[t].service}, ref.Chain...)
				}
				ref.Chain = append([]string{jr.service}, ref.Chain...)
			}
			out = append(out, ref)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Distance != out[j].Distance {
			return out[i].Distance < out[j].Distance
		}
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// ---- uses -------------------------------------------------------------------

// SymbolUsers is the answer of Uses for one exported symbol of a library.
type SymbolUsers struct {
	Module   string       `json:"module"`
	Package  string       `json:"package"`
	Symbol   string       `json:"symbol"`
	Services []string     `json:"services"`      // services whose code calls it
	Journeys []JourneyUse `json:"journeys"`      // journeys whose code calls it
	API      *JourneyRef  `json:"api,omitempty"` // its own journey, when the run analysed its library
}

// JourneyUse is a journey calling a symbol, at its first call site.
type JourneyUse struct {
	JourneyRef
	Site  string `json:"site"`  // the first call site, in the journey's service's repository
	Calls int    `json:"calls"` // call sites in the journey's code
}

// Uses finds who calls a library's exported functions and methods: every
// symbol of a module or package given by its path, or the symbols whose name
// contains text (case-insensitive), with the services calling them and the
// journeys that do, nearest the entry first.
func (r *Run) Uses(text string) []SymbolUsers {
	low := strings.ToLower(text)
	matches := func(module, pkg, symbol string) bool {
		return module == text || pkg == text || strings.Contains(strings.ToLower(symbol), low)
	}
	bySym := map[string]*SymbolUsers{}
	get := func(module, pkg, symbol string) *SymbolUsers {
		k := module + "|" + symbol
		if bySym[k] == nil {
			bySym[k] = &SymbolUsers{Module: module, Package: pkg, Symbol: symbol}
		}
		return bySym[k]
	}
	for _, m := range r.Manifests {
		for _, l := range m.Libraries {
			for _, s := range l.Symbols {
				if matches(l.Module, "", s) || l.Module == text {
					u := get(l.Module, "", s)
					u.Services = append(u.Services, m.Service)
				}
			}
		}
	}
	for i, jr := range r.journeys {
		for _, u := range jr.j.Uses {
			if matches(u.Module, u.Package, u.Symbol) {
				su := get(u.Module, u.Package, u.Symbol)
				su.Package = u.Package
				su.Journeys = append(su.Journeys, JourneyUse{JourneyRef: r.ref(i), Site: u.Position, Calls: u.Calls})
			}
		}
	}
	keys := sortedKeys(bySym)
	out := make([]SymbolUsers, 0, len(keys))
	for _, k := range keys {
		su := bySym[k]
		sort.Strings(su.Services)
		su.Services = slices.Compact(su.Services)
		if i, ok := r.apiJourney(su.Module, su.Package, su.Symbol); ok {
			ref := r.ref(i)
			su.API = &ref
		}
		out = append(out, *su)
		if len(out) == maxRows {
			break
		}
	}
	return out
}

// ---- search -------------------------------------------------------------------

// Found is the answer of Search.
type Found struct {
	Services []string     `json:"services"`
	Journeys []JourneyRef `json:"journeys"`
	Keys     []string     `json:"keys"` // "transport key"
}

// Search finds services, journeys and keys whose names contain every word.
func (r *Run) Search(text string) Found {
	words := strings.Fields(strings.ToLower(text))
	hit := func(s string) bool {
		s = strings.ToLower(s)
		for _, w := range words {
			if !strings.Contains(s, w) {
				return false
			}
		}
		return len(words) > 0
	}
	var f Found
	for _, m := range r.Manifests {
		if hit(m.Service + " " + m.Module) {
			f.Services = append(f.Services, m.Service)
		}
	}
	for i, jr := range r.journeys {
		if hit(jr.service+" "+jr.j.Key+" "+jr.j.Handler) && len(f.Journeys) < maxRows {
			f.Journeys = append(f.Journeys, r.ref(i))
		}
	}
	seen := map[string]bool{}
	for _, ep := range r.endpoints {
		if ep.Transport == model.MainTransport {
			continue
		}
		if k := ep.Transport + " " + keys(ep); hit(k) && !seen[k] {
			seen[k] = true
			f.Keys = append(f.Keys, k)
		}
	}
	sort.Strings(f.Services)
	sort.Strings(f.Keys)
	if len(f.Keys) > maxRows {
		f.Keys = f.Keys[:maxRows]
	}
	return f
}

// linkLess orders links so that a choice among equally near ones is stable.
func linkLess(a, b model.Link) bool {
	return a.From+a.Transport+a.Key < b.From+b.Transport+b.Key
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// declaredLanguage is the language of a service described by hand ("declared"
// when none is given), or "" for an analysed one.
func declaredLanguage(m *model.Manifest) string {
	switch {
	case m.Declared == nil:
		return ""
	case m.Declared.Language != "":
		return m.Declared.Language
	}
	return "declared"
}
