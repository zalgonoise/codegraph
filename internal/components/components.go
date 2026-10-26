// Package components breaks the reachable part of the module down into
// components (one per package) with their contracts (declared interfaces and
// the concrete types that implement them at runtime), the concrete types in
// use, the calls between components, dependency-injection bindings and the
// third-party libraries each component relies on.
package components

import (
	"go/token"
	"go/types"
	"sort"
	"strings"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"

	"github.com/zalgonoise/codegraph/internal/loader"
	"github.com/zalgonoise/codegraph/internal/model"
	"github.com/zalgonoise/codegraph/internal/reach"
	"github.com/zalgonoise/codegraph/internal/symbols"
)

// Analyzer computes the component view of one service.
type Analyzer struct {
	Fset       *token.FileSet
	Root       string
	ModulePath string
	EntryPkg   string
	ModulePkgs map[string]bool
	PkgModule  map[string]string
	Packages   map[string]*packages.Package
	Reach      *reach.Result
	IncludeStd bool

	candidates []types.Type
}

// Result is the component view.
type Result struct {
	Components []model.Component
	Calls      []model.Call
	Bindings   []model.Binding
	Libraries  []model.Library
}

// Analyze builds the view from the reachable module functions.
func (a *Analyzer) Analyze(funcs []*ssa.Function) *Result {
	comps := map[string]*model.Component{}
	methods := map[string]map[string]map[string]bool{} // pkg -> type -> method set

	for _, fn := range funcs {
		if fn.Synthetic != "" {
			continue
		}
		pkg := symbols.PkgPath(fn)
		c := a.component(comps, pkg)
		c.Funcs++
		o := symbols.Origin(fn)
		if recv := o.Signature.Recv(); recv != nil && o.Parent() == nil {
			tn := symbols.TypeName(recv.Type())
			if methods[pkg] == nil {
				methods[pkg] = map[string]map[string]bool{}
			}
			if methods[pkg][tn] == nil {
				methods[pkg][tn] = map[string]bool{}
			}
			methods[pkg][tn][o.Name()] = true
		}
	}

	for pkg, c := range comps {
		c.Interfaces = a.interfaces(pkg)
		byType := methods[pkg]
		for _, tn := range sortedKeys(byType) {
			c.Types = append(c.Types, model.ConcreteType{Name: tn, Methods: sortedSet(byType[tn])})
		}
	}

	res := &Result{}
	for _, pkg := range sortedKeys(comps) {
		res.Components = append(res.Components, *comps[pkg])
	}
	res.Calls, res.Libraries = a.edges()
	a.librarySymbols(funcs, res.Libraries)
	res.Bindings = a.bindings(funcs)
	return res
}

// librarySymbols fills each library's exported functions and methods that
// the module's reachable code calls (symbols.Called).
func (a *Analyzer) librarySymbols(funcs []*ssa.Function, libs []model.Library) {
	byModule := map[string]map[string]bool{}
	for _, fn := range funcs {
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
				if byModule[mod] == nil {
					byModule[mod] = map[string]bool{}
				}
				byModule[mod][label] = true
			}
		}
	}
	for i := range libs {
		libs[i].Symbols = sortedSet(byModule[libs[i].Module])
	}
}

func (a *Analyzer) component(comps map[string]*model.Component, pkg string) *model.Component {
	c := comps[pkg]
	if c == nil {
		name := symbols.Short(pkg, a.ModulePath)
		layer := Layer(name)
		if a.ModulePath != "" && pkg != a.ModulePath && !strings.HasPrefix(pkg, a.ModulePath+"/") {
			layer = "adapters" // an included module (-include-modules): a client library
		}
		if pkg == a.EntryPkg {
			layer = "entry"
		}
		c = &model.Component{Package: pkg, Name: name, Layer: layer}
		comps[pkg] = c
	}
	return c
}

// interfaces lists the non-empty method-set interfaces declared in pkg with
// their contract and their runtime implementations.
func (a *Analyzer) interfaces(pkg string) []model.Interface {
	p := a.Packages[pkg]
	if p == nil || p.Types == nil {
		return nil
	}
	qual := types.RelativeTo(p.Types)
	scope := p.Types.Scope()
	var out []model.Interface
	for _, name := range scope.Names() {
		tn, ok := scope.Lookup(name).(*types.TypeName)
		if !ok || tn.IsAlias() {
			continue
		}
		named, ok := tn.Type().(*types.Named)
		if !ok || named.TypeParams().Len() > 0 {
			continue
		}
		iface, ok := named.Underlying().(*types.Interface)
		if !ok || !iface.IsMethodSet() || iface.NumMethods() == 0 {
			continue
		}
		mi := model.Interface{Name: name}
		for i := 0; i < iface.NumMethods(); i++ {
			m := iface.Method(i)
			mi.Methods = append(mi.Methods, m.Name()+strings.TrimPrefix(types.TypeString(m.Type(), qual), "func"))
		}
		mi.Implementations = a.implementations(iface)
		out = append(out, mi)
	}
	return out
}

func (a *Analyzer) implementations(iface *types.Interface) []string {
	set := map[string]bool{}
	for _, t := range a.candidateTypes() {
		n := symbols.Named(t)
		if n == nil || symbols.IsInterface(t) {
			continue
		}
		if n.TypeParams().Len() > 0 && n.TypeArgs().Len() == 0 {
			continue // uninstantiated generic
		}
		if types.Implements(t, iface) {
			set[symbols.TypeLabel(t)] = true
		}
	}
	return sortedSet(set)
}

// candidateTypes are the types that may implement interfaces at runtime:
// RTA's runtime types, or (for CHA) every named module type and its pointer.
func (a *Analyzer) candidateTypes() []types.Type {
	if a.candidates != nil {
		return a.candidates
	}
	if len(a.Reach.RuntimeTypes) > 0 {
		a.candidates = a.Reach.RuntimeTypes
		return a.candidates
	}
	for _, pkg := range sortedKeys(a.ModulePkgs) {
		p := a.Packages[pkg]
		if p == nil || p.Types == nil {
			continue
		}
		scope := p.Types.Scope()
		for _, name := range scope.Names() {
			if tn, ok := scope.Lookup(name).(*types.TypeName); ok && !tn.IsAlias() {
				a.candidates = append(a.candidates, tn.Type(), types.NewPointer(tn.Type()))
			}
		}
	}
	if a.candidates == nil {
		a.candidates = []types.Type{}
	}
	return a.candidates
}

// edges aggregates call-graph edges into component calls and library usage.
func (a *Analyzer) edges() ([]model.Call, []model.Library) {
	type pair struct{ from, to string }
	calls := map[pair]*model.Call{}
	syms := map[pair]map[string]bool{}
	libs := map[string]*model.Library{}
	libPkgs := map[string]map[string]bool{}
	libUsers := map[string]map[string]bool{}

	_ = callgraph.GraphVisitEdges(a.Reach.Graph, func(e *callgraph.Edge) error {
		caller, callee := e.Caller.Func, e.Callee.Func
		if !a.Reach.Reachable[caller] {
			return nil
		}
		from, to := symbols.PkgPath(caller), symbols.PkgPath(callee)
		if !a.ModulePkgs[from] || from == to || to == "" {
			return nil
		}
		if callee.Synthetic == symbols.PackageInit { // import order, not a dependency call
			return nil
		}
		if a.ModulePkgs[to] {
			k := pair{from, to}
			c := calls[k]
			if c == nil {
				c = &model.Call{From: from, To: to}
				calls[k] = c
				syms[k] = map[string]bool{}
			}
			c.Count++
			if e.Site != nil && e.Site.Common().IsInvoke() {
				c.ViaInterface = true
			}
			syms[k][symbols.FuncLabel(callee)] = true
			return nil
		}
		mod := a.PkgModule[to]
		if mod == "" || (mod == loader.StdModule && !a.IncludeStd) {
			return nil
		}
		l := libs[mod]
		if l == nil {
			l = &model.Library{Module: mod}
			libs[mod] = l
			libPkgs[mod] = map[string]bool{}
			libUsers[mod] = map[string]bool{}
		}
		l.Calls++
		libPkgs[mod][to] = true
		libUsers[mod][from] = true
		return nil
	})

	var outCalls []model.Call
	for k, c := range calls {
		c.Symbols = sortedSet(syms[k])
		outCalls = append(outCalls, *c)
	}
	sort.Slice(outCalls, func(i, j int) bool {
		if outCalls[i].From != outCalls[j].From {
			return outCalls[i].From < outCalls[j].From
		}
		return outCalls[i].To < outCalls[j].To
	})

	var outLibs []model.Library
	for _, mod := range sortedKeys(libs) {
		l := libs[mod]
		l.Packages = sortedSet(libPkgs[mod])
		l.UsedBy = sortedSet(libUsers[mod])
		outLibs = append(outLibs, *l)
	}
	return outCalls, outLibs
}

// bindings finds concrete→interface conversions involving module types; in
// practice this is the dependency wiring done in main and constructors.
func (a *Analyzer) bindings(funcs []*ssa.Function) []model.Binding {
	type key struct{ iface, concrete string }
	binds := map[key]*model.Binding{}
	errType := types.Universe.Lookup("error").Type()

	for _, fn := range funcs {
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				mi, ok := instr.(*ssa.MakeInterface)
				if !ok {
					continue
				}
				it, ct := mi.Type(), mi.X.Type()
				iface, ok := it.Underlying().(*types.Interface)
				if !ok || iface.NumMethods() == 0 || types.Identical(it, errType) {
					continue
				}
				if !a.ModulePkgs[symbols.TypePkg(ct)] && !a.ModulePkgs[symbols.TypePkg(it)] {
					continue
				}
				k := key{symbols.TypeLabel(it), symbols.TypeLabel(ct)}
				bd := binds[k]
				if bd == nil {
					bd = &model.Binding{Interface: k.iface, Concrete: k.concrete}
					binds[k] = bd
				}
				bd.Sites++
				if len(bd.Positions) < 3 {
					if p := symbols.Pos(a.Fset, mi.Pos(), a.Root); p != "" {
						bd.Positions = append(bd.Positions, p)
					}
				}
			}
		}
	}
	var out []model.Binding
	for _, bd := range binds {
		out = append(out, *bd)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Interface != out[j].Interface {
			return out[i].Interface < out[j].Interface
		}
		return out[i].Concrete < out[j].Concrete
	})
	return out
}

// Layer guesses an architectural layer from package path segments.
func Layer(rel string) string {
	segs := strings.Split(rel, "/")
	for i := len(segs) - 1; i >= 0; i-- {
		if strings.HasSuffix(strings.ToLower(segs[i]), "-sdk-go") { // example.com/ledger-sdk-go
			return "adapters"
		}
		switch strings.ToLower(segs[i]) {
		case "cmd", "main":
			return "entry"
		case "http", "httpapi", "api", "handler", "handlers", "transport", "grpc", "rpc", "rest",
			"server", "router", "web", "controller", "controllers", "gateway":
			return "transport"
		case "consumer", "consumers", "subscriber", "worker", "workers", "listener", "events", "event",
			"messaging", "queue", "pubsub", "kafka", "nats", "publisher":
			return "messaging"
		case "service", "services", "usecase", "usecases", "app", "application", "domain", "core", "business":
			return "domain"
		case "repo", "repository", "repositories", "store", "storage", "db", "database", "persistence",
			"postgres", "mysql", "sql", "cache", "redis", "dao":
			return "persistence"
		case "client", "clients", "adapter", "adapters", "integration", "integrations", "external", "sdk":
			return "adapters"
		case "config", "configs", "conf", "settings", "env":
			return "config"
		}
	}
	return "other"
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedSet(m map[string]bool) []string {
	return sortedKeys(m)
}
