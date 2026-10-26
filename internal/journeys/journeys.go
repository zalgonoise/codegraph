// Package journeys treats every inbound endpoint (an HTTP route, a gRPC
// method, a consumed topic) as the start of a user journey and computes what
// it reaches in module code: the functions and components it runs through,
// the calls between them, the goroutines it starts and the outbound endpoints
// (other services, brokers, stores) whose call sites it reaches, in program
// order.
//
// The walk follows the call graph but only enters module functions: going
// through the standard library or other libraries would re-enter the module
// through interfaces (net/http leads back to every handler). Function values
// created along the way usually run through such library code, so anonymous
// functions defined in a reached function, and module functions or method
// values handed to library calls (retry.Do(f), errgroup.Go, sort.Slice), are
// treated as reached. Inside a closure returned by a handler factory
// (middleware(next)), calls of the captured handler go to the handler wrapped
// at that registration only (resolve.Root.Bind). Reachability is otherwise
// context-insensitive, so shared dispatch code can over-approximate a journey.
package journeys

import (
	"go/token"
	"sort"
	"strings"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/ssa"

	"github.com/zalgonoise/codegraph/internal/loader"
	"github.com/zalgonoise/codegraph/internal/model"
	"github.com/zalgonoise/codegraph/internal/reach"
	"github.com/zalgonoise/codegraph/internal/resolve"
	"github.com/zalgonoise/codegraph/internal/symbols"
	"github.com/zalgonoise/codegraph/internal/transport"
)

// Analyzer computes the journeys of one service.
type Analyzer struct {
	Reach      *reach.Result
	Resolver   *resolve.Resolver
	ModulePkgs map[string]bool
	Fset       *token.FileSet
	Root       string
	MaxDepth   int // call depth followed from the handler; 0 = unlimited

	// The module of each package (loader.StdModule for the standard library),
	// for the symbols of other modules a journey uses; the standard library's
	// only with IncludeStd.
	PkgModule  map[string]string
	IncludeStd bool
}

// outbound indexes the outbound detections by call site (and by function for
// directives, which have no site).
type outbound struct {
	bySite map[ssa.CallInstruction][]model.Endpoint
	byFunc map[*ssa.Function][]model.Endpoint
	// Per-caller variants (transport.Detection.Callers): the call sites into
	// the helper each variant endpoint was resolved for.
	callers  map[string]map[ssa.CallInstruction]bool
	variants map[ssa.CallInstruction]bool
}

// Analyze returns one journey per inbound endpoint with resolved handlers, or
// one per interface method when the handler is an interface with several
// (gRPC servers), keyed "Service/Method".
func (a *Analyzer) Analyze(dets []transport.Detection) []model.Journey {
	type entry struct {
		ep    model.Endpoint
		roots []resolve.Root
	}
	out := outbound{
		bySite:   map[ssa.CallInstruction][]model.Endpoint{},
		byFunc:   map[*ssa.Function][]model.Endpoint{},
		callers:  map[string]map[ssa.CallInstruction]bool{},
		variants: map[ssa.CallInstruction]bool{},
	}
	entries := map[string]*entry{}
	for _, d := range dets {
		ep := d.Endpoint
		ep.ID = model.EndpointID(ep)
		switch {
		case ep.Direction == model.Outbound && d.Site != nil:
			out.bySite[d.Site] = append(out.bySite[d.Site], ep)
			if len(d.Callers) > 0 {
				out.variants[d.Site] = true
				if out.callers[ep.ID] == nil {
					out.callers[ep.ID] = map[ssa.CallInstruction]bool{}
				}
				for _, c := range d.Callers {
					out.callers[ep.ID][c] = true
				}
			}
		case ep.Direction == model.Outbound && d.Func != nil:
			out.byFunc[d.Func] = append(out.byFunc[d.Func], ep)
		case ep.Direction == model.Inbound && len(d.Roots) > 0:
			e := entries[ep.ID]
			if e == nil {
				e = &entry{ep: ep}
				entries[ep.ID] = e
			}
			e.roots = append(e.roots, d.Roots...)
		}
	}

	// A main journey stops where another journey starts: the handlers main
	// registers have journeys of their own.
	others := map[*ssa.Function]bool{}
	for _, e := range entries {
		if e.ep.Transport != model.MainTransport {
			for _, r := range e.roots {
				others[r.Func] = true
			}
		}
	}
	var journeys []model.Journey
	for _, e := range entries {
		var stop map[*ssa.Function]bool
		if e.ep.Transport == model.MainTransport {
			stop = others
		}
		for _, g := range groupRoots(e.roots) {
			journeys = append(journeys, a.journey(e.ep, g, out, stop))
		}
	}
	sort.Slice(journeys, func(i, j int) bool {
		if journeys[i].Transport != journeys[j].Transport {
			return journeys[i].Transport < journeys[j].Transport
		}
		if journeys[i].Key != journeys[j].Key {
			return journeys[i].Key < journeys[j].Key
		}
		return journeys[i].Entrypoint < journeys[j].Entrypoint
	})
	return journeys
}

type group struct {
	method string // set when the handler is split per interface method
	roots  []resolve.Root
}

// groupRoots splits roots per interface method when they implement more than
// one (a gRPC server); otherwise they form a single journey.
func groupRoots(roots []resolve.Root) []group {
	byMethod := map[string][]resolve.Root{}
	for _, r := range resolve.DedupeRoots(roots) {
		byMethod[r.Method] = append(byMethod[r.Method], r)
	}
	if len(byMethod) == 1 {
		for _, rs := range byMethod {
			return []group{{roots: rs}}
		}
	}
	methods := make([]string, 0, len(byMethod))
	for m := range byMethod {
		methods = append(methods, m)
	}
	sort.Strings(methods)
	out := make([]group, 0, len(methods))
	for _, m := range methods {
		out = append(out, group{method: m, roots: byMethod[m]})
	}
	return out
}

func (a *Analyzer) journey(ep model.Endpoint, g group, out outbound, stop map[*ssa.Function]bool) model.Journey {
	keys := append([]string(nil), ep.Keys...)
	sort.Strings(keys)
	j := model.Journey{
		Entrypoint: ep.ID,
		Transport:  ep.Transport,
		Key:        strings.Join(keys, ", "),
		Handler:    ep.Handler,
		Component:  symbols.PkgPath(g.roots[0].Func),
	}
	if g.method != "" {
		j.Key += "/" + g.method
		j.Handler = symbols.FuncLabel(g.roots[0].Func)
	}
	if j.Handler == "" {
		j.Handler = symbols.FuncLabel(g.roots[0].Func)
	}
	wrapped := map[*ssa.Function]bool{}
	for _, r := range g.roots {
		j.Roots = append(j.Roots, model.Site{Function: symbols.FuncLabel(r.Func), Position: symbols.Pos(a.Fset, r.Func.Pos(), a.Root)})
		// A root that is middleware (mw.Wrap(h.X)) has its own position; the handler it wraps is what answers cite.
		if inner := r.Innermost(); inner != nil && inner != r.Func && !wrapped[inner] {
			wrapped[inner] = true
			j.Wrapped = append(j.Wrapped, model.Site{Function: symbols.FuncLabel(inner), Position: symbols.Pos(a.Fset, inner.Pos(), a.Root)})
		}
	}

	w := a.walk(g.roots, stop)
	j.Funcs = len(w.order)
	j.Truncated = w.truncated
	comps, labels := map[string]bool{}, map[string]bool{}
	for _, fn := range w.order {
		comps[symbols.PkgPath(fn)] = true
		labels[symbols.FuncLabel(fn)] = true
	}
	j.Components = sortedSet(comps)
	j.Functions = sortedSet(labels)
	j.Async = sortedSet(w.async)
	j.Calls = w.componentCalls()
	j.Uses = a.uses(w.order)

	// Outbound endpoints in program order; background when every site using
	// them is only reached from a goroutine.
	background := map[string]bool{}
	byID := map[string]model.Endpoint{}
	note := func(eps []model.Endpoint, bg bool) {
		for _, ep := range eps {
			if prev, ok := background[ep.ID]; ok {
				background[ep.ID] = prev && bg
			} else {
				background[ep.ID], byID[ep.ID] = bg, ep
			}
		}
	}
	for _, fn := range w.order {
		bg := !w.reached[fn]
		note(out.byFunc[fn], bg)
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				if site, ok := instr.(ssa.CallInstruction); ok {
					note(out.forJourney(site, w.via[fn]), bg)
				}
			}
		}
	}
	for _, id := range a.sequence(g.roots, out, byID, stop) {
		ep := byID[id]
		j.Outbound = append(j.Outbound, model.JourneyOutbound{Endpoint: id, Kind: ep.InteractionKind(), Background: background[id]})
	}
	return j
}

// forJourney returns the outbound endpoints of a site for a journey that
// entered the site's function through the call sites in via: for a helper with
// per-caller variants, the variants of those callers (all of them when the
// journey came another way, e.g. the helper is a root).
func (out outbound) forJourney(site ssa.CallInstruction, via map[ssa.CallInstruction]bool) []model.Endpoint {
	eps := out.bySite[site]
	if !out.variants[site] {
		return eps
	}
	var chosen []model.Endpoint
	for _, ep := range eps {
		for c := range out.callers[ep.ID] {
			if via[c] {
				chosen = append(chosen, ep)
				break
			}
		}
	}
	if len(chosen) == 0 {
		return eps
	}
	return chosen
}

// MainDetection is the inbound endpoint of a program's own work, rooted at its
// main function: its journey is what main does that no other journey covers
// (a pipeline it runs, a job, the connections it opens), walking up to the
// handlers it registers, whose journeys are their own.
func MainDetection(main *ssa.Function, fset *token.FileSet, root string) transport.Detection {
	return transport.Detection{
		Endpoint: model.Endpoint{
			Transport: model.MainTransport, Role: model.MainRole, Direction: model.Inbound,
			Keys: []string{model.MainKey}, Handler: symbols.FuncLabel(main), Component: symbols.PkgPath(main),
			Function: symbols.FuncLabel(main), Position: symbols.Pos(fset, main.Pos(), root), Source: "main",
		},
		Roots: []resolve.Root{{Func: main}},
	}
}

// ---- successors -----------------------------------------------------------------

// step is one way control can move from a reached function into module code.
type step struct {
	at    ssa.Instruction     // where it happens (nil: an anonymous function never referenced)
	site  ssa.CallInstruction // nil for closures created here
	to    resolve.Root
	via   bool // through an interface
	spawn bool // started with a go statement
}

type item struct {
	fn         *ssa.Function
	depth      int
	background bool
	root       resolve.Root // carries the bindings of a factory closure
}

// successors lists the steps out of it.fn in instruction order.
func (a *Analyzer) successors(it item) []step {
	edges := map[ssa.CallInstruction][]*callgraph.Edge{}
	if n := a.Reach.Graph.Nodes[it.fn]; n != nil {
		for _, e := range n.Out {
			edges[e.Site] = append(edges[e.Site], e)
		}
	}
	started := goStarted(it.fn)
	anon := map[*ssa.Function]bool{}
	var out []step
	for _, b := range it.fn.Blocks {
		for _, instr := range b.Instrs {
			// Anonymous functions defined here, where they are created.
			var ops [8]*ssa.Value
			for _, op := range instr.Operands(ops[:0]) {
				if f, ok := (*op).(*ssa.Function); ok && f.Parent() == it.fn && !anon[f] {
					anon[f] = true
					out = append(out, step{at: instr, to: resolve.Root{Func: f}, spawn: started[f]})
				}
			}
			if mc, ok := instr.(*ssa.MakeClosure); ok {
				if f, ok := mc.Fn.(*ssa.Function); ok && f.Parent() == it.fn && !anon[f] {
					anon[f] = true
					out = append(out, step{at: instr, to: resolve.Root{Func: f}, spawn: started[f]})
				}
			}

			site, ok := instr.(ssa.CallInstruction)
			if !ok {
				continue
			}
			invoke := site.Common().IsInvoke()
			if targets, ok := it.root.BoundCallees(site.Common()); ok {
				for _, t := range targets {
					out = append(out, step{at: instr, site: site, to: t, via: invoke})
				}
				continue
			}
			module := false
			for _, e := range edges[site] {
				if a.inModule(e.Callee.Func) {
					module = true
					out = append(out, step{at: instr, site: site, to: resolve.Root{Func: e.Callee.Func}, via: invoke})
				}
			}
			if module || (site.Common().StaticCallee() != nil && a.inModule(site.Common().StaticCallee())) {
				continue
			}
			// Module callbacks handed to library code.
			for _, arg := range site.Common().Args {
				switch arg.(type) {
				case *ssa.MakeClosure, *ssa.Function:
					for _, root := range a.Resolver.HandlerFuncs(arg, nil) {
						out = append(out, step{at: instr, site: site, to: root})
					}
				}
			}
		}
	}
	for _, f := range it.fn.AnonFuncs {
		if !anon[f] {
			out = append(out, step{to: resolve.Root{Func: f}, spawn: started[f]})
		}
	}
	return out
}

// goStarted lists the anonymous functions fn starts with a go statement.
func goStarted(fn *ssa.Function) map[*ssa.Function]bool {
	out := map[*ssa.Function]bool{}
	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			if g, ok := instr.(*ssa.Go); ok {
				switch v := g.Call.Value.(type) {
				case *ssa.MakeClosure:
					if f, ok := v.Fn.(*ssa.Function); ok {
						out[f] = true
					}
				case *ssa.Function:
					out[v] = true
				}
			}
		}
	}
	return out
}

// ---- walk (reachability) -------------------------------------------------------

type edgeKey struct {
	from *ssa.Function
	site ssa.CallInstruction
	to   *ssa.Function
}

type walk struct {
	reached   map[*ssa.Function]bool                         // true when reached synchronously
	via       map[*ssa.Function]map[ssa.CallInstruction]bool // call sites each function was entered through
	order     []*ssa.Function
	edges     map[edgeKey]bool
	calls     map[[2]string]*model.Call
	syms      map[[2]string]map[string]bool
	async     map[string]bool
	truncated bool
}

// walk is a BFS from the roots: what the journey reaches, how (directly or
// only from goroutines) and the component edges it uses. It does not enter
// the functions in stop (other journeys' roots, for a main journey).
func (a *Analyzer) walk(roots []resolve.Root, stop map[*ssa.Function]bool) *walk {
	w := &walk{
		reached: map[*ssa.Function]bool{},
		via:     map[*ssa.Function]map[ssa.CallInstruction]bool{},
		edges:   map[edgeKey]bool{},
		calls:   map[[2]string]*model.Call{},
		syms:    map[[2]string]map[string]bool{},
		async:   map[string]bool{},
	}
	var queue []item
	visit := func(root resolve.Root, depth int, background bool) {
		fn := root.Func
		fg, seen := w.reached[fn]
		if seen && (fg || background) {
			return // already reached at least as directly
		}
		if !seen {
			w.order = append(w.order, fn)
		}
		w.reached[fn] = !background
		queue = append(queue, item{fn, depth, background, root})
	}
	for _, r := range roots {
		visit(r, 0, false)
	}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		steps := a.successors(it)
		if a.MaxDepth > 0 && it.depth >= a.MaxDepth {
			w.truncated = w.truncated || len(steps) > 0
			continue
		}
		for _, s := range steps {
			if stop[s.to.Func] {
				continue
			}
			spawn := s.spawn
			if _, isGo := s.site.(*ssa.Go); isGo {
				spawn = true
			}
			if spawn {
				w.async[symbols.FuncLabel(s.to.Func)] = true
			}
			w.edge(edgeKey{it.fn, s.site, s.to.Func}, s.via)
			visit(s.to, it.depth+1, it.background || spawn)
		}
	}
	return w
}

func (w *walk) edge(k edgeKey, viaInterface bool) {
	if w.edges[k] {
		return
	}
	w.edges[k] = true
	if k.site != nil {
		if w.via[k.to] == nil {
			w.via[k.to] = map[ssa.CallInstruction]bool{}
		}
		w.via[k.to][k.site] = true
	}
	from, to := symbols.PkgPath(k.from), symbols.PkgPath(k.to)
	if from == to {
		return
	}
	pk := [2]string{from, to}
	c := w.calls[pk]
	if c == nil {
		c = &model.Call{From: from, To: to}
		w.calls[pk] = c
		w.syms[pk] = map[string]bool{}
	}
	c.Count++
	c.ViaInterface = c.ViaInterface || viaInterface
	w.syms[pk][symbols.FuncLabel(k.to)] = true
}

func (w *walk) componentCalls() []model.Call {
	out := make([]model.Call, 0, len(w.calls))
	for k, c := range w.calls {
		c.Symbols = sortedSet(w.syms[k])
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].To < out[j].To
	})
	return out
}

// ---- sequence (program order) ---------------------------------------------------

// sequence orders the outbound endpoint IDs in known by a depth-first walk in
// instruction order, i.e. roughly the order a request uses them. Endpoints the
// walk does not meet (beyond -journey-depth) follow, sorted.
func (a *Analyzer) sequence(roots []resolve.Root, out outbound, known map[string]model.Endpoint, stop map[*ssa.Function]bool) []string {
	var ids []string
	seen := map[string]bool{}
	emit := func(eps []model.Endpoint) {
		for _, ep := range eps {
			if _, ok := known[ep.ID]; ok && !seen[ep.ID] {
				seen[ep.ID] = true
				ids = append(ids, ep.ID)
			}
		}
	}
	visited := map[*ssa.Function]bool{}
	var dfs func(it item)
	dfs = func(it item) {
		if visited[it.fn] || (a.MaxDepth > 0 && it.depth > a.MaxDepth) {
			return
		}
		visited[it.fn] = true
		emit(out.byFunc[it.fn])
		steps := a.successors(it)
		k := 0
		for _, b := range it.fn.Blocks {
			for _, instr := range b.Instrs {
				if site, ok := instr.(ssa.CallInstruction); ok {
					emit(out.bySite[site])
				}
				for ; k < len(steps) && steps[k].at == instr; k++ {
					if !stop[steps[k].to.Func] {
						dfs(item{fn: steps[k].to.Func, depth: it.depth + 1, root: steps[k].to})
					}
				}
			}
		}
		for ; k < len(steps); k++ {
			if !stop[steps[k].to.Func] {
				dfs(item{fn: steps[k].to.Func, depth: it.depth + 1, root: steps[k].to})
			}
		}
	}
	for _, r := range roots {
		dfs(item{fn: r.Func, root: r})
	}

	var rest []string
	for id := range known {
		if !seen[id] {
			rest = append(rest, id)
		}
	}
	sort.Strings(rest)
	return append(ids, rest...)
}

// uses lists the exported functions and methods of other modules that fns
// call (symbols.Called), with the first call site and the number of sites,
// sorted by module and symbol.
func (a *Analyzer) uses(fns []*ssa.Function) []model.SymbolUse {
	byKey := map[string]*model.SymbolUse{}
	var keys []string
	for _, fn := range fns {
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				site, ok := instr.(ssa.CallInstruction)
				if !ok {
					continue
				}
				pkg, label, ok := symbols.Called(site)
				if !ok || a.ModulePkgs[pkg] {
					continue
				}
				mod := a.PkgModule[pkg]
				if mod == "" || (mod == loader.StdModule && !a.IncludeStd) {
					continue
				}
				k := mod + "|" + label
				u := byKey[k]
				if u == nil {
					u = &model.SymbolUse{Module: mod, Package: pkg, Symbol: label, Position: symbols.Pos(a.Fset, site.Pos(), a.Root)}
					byKey[k] = u
					keys = append(keys, k)
				}
				u.Calls++
			}
		}
	}
	sort.Strings(keys)
	out := make([]model.SymbolUse, 0, len(keys))
	for _, k := range keys {
		out = append(out, *byKey[k])
	}
	return out
}

func (a *Analyzer) inModule(fn *ssa.Function) bool {
	return fn != nil && len(fn.Blocks) > 0 && fn.Synthetic != symbols.PackageInit && a.ModulePkgs[symbols.PkgPath(fn)]
}

func sortedSet(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
