// Package library analyses a Go module without a main, a library, through
// its exported API: every exported function, method of an exported type and
// method of an exported interface of its public packages (not internal, not
// examples) is an entry point, like a route or an RPC is for a service. Each
// becomes an inbound endpoint (transport "go", role "api", keyed like
// "fts.Index.Search") whose journey shows what the symbol reaches in the
// library, the endpoints it touches (queries, HTTP calls, topics) and the
// exported symbols of other modules it calls.
//
// Reachability starts from the API and the packages' initializers. An
// interface method of the API runs whatever implements it: the library's own
// types converted to an interface in the code the API reaches (a constructor
// returning an unexported implementation), found by a first reachability pass
// and added as roots of a second.
package library

import (
	"go/ast"
	"go/token"
	"go/types"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/zalgonoise/codegraph/internal/model"
	"github.com/zalgonoise/codegraph/internal/resolve"
	"github.com/zalgonoise/codegraph/internal/symbols"
	"github.com/zalgonoise/codegraph/internal/transport"
)

// Transport and Role of the API's endpoints.
const (
	Transport = model.APITransport
	Role      = model.APIRole
)

// Kinds of API symbols.
const (
	Func      = "func"
	Method    = "method"
	Interface = "interface" // a method of an exported interface
)

// Symbol is one exported function or method of the API.
type Symbol struct {
	Key      string // qualifier.Member: "fts.New", "fts.Index.Search", "fts/log.Logger.Info"
	Package  string // import path
	Kind     string
	Internal bool            // declared in an internal package: only the module itself can call it
	Label    string          // "(*fts.Index[K, V]).Search"
	Pos      token.Pos       // the declaration
	Funcs    []*ssa.Function // what runs: the function, or an interface method's implementations

	iface *types.Interface // Interface symbols: the interface, and the method's name in Member
	name  string
}

// skipDirs matches package paths (relative to the module) that are no part
// of a library's API, and internalDir those only the module can import.
var (
	skipDirs    = regexp.MustCompile(`(^|/)(testdata|examples?|demos?|cmd)(/|$)`)
	internalDir = regexp.MustCompile(`(^|/)internal(/|$)`)
)

// Public reports whether a package of the module (its path relative to the
// module) belongs to the public API.
func Public(rel string) bool { return !skipDirs.MatchString(rel) && !internalDir.MatchString(rel) }

// Internal reports whether a package of the module is an internal one that
// API analyses with internal set.
func Internal(rel string) bool { return !skipDirs.MatchString(rel) && internalDir.MatchString(rel) }

// API lists the exported symbols of the module's public packages, and with
// internal those of its internal packages (Symbol.Internal), sorted by key,
// leaving out those declared in the skipped files (generated ones: see
// Generated). Interface methods have no Funcs until Implement finds them.
func API(pkgs []*ssa.Package, module string, skip map[string]bool, internal bool) (out []Symbol) {
	defer func() {
		if len(skip) == 0 || len(out) == 0 {
			return
		}
		fset := pkgs[0].Prog.Fset
		kept := out[:0]
		for _, s := range out {
			if f := fset.File(s.Pos); f == nil || !skip[f.Name()] {
				kept = append(kept, s)
			}
		}
		out = kept
	}()
	for _, p := range pkgs {
		pkgPath := p.Pkg.Path()
		rel, ok := relPath(pkgPath, module)
		if !ok || p.Pkg.Name() == "main" || strings.HasSuffix(p.Pkg.Name(), "_test") || !Public(rel) && !(internal && Internal(rel)) {
			continue
		}
		start := len(out)
		qual := model.APIQualifier(pkgPath, module)
		for name, mem := range p.Members {
			if !token.IsExported(name) {
				continue
			}
			switch m := mem.(type) {
			case *ssa.Function:
				out = append(out, Symbol{
					Key: qual + "." + name, Package: pkgPath, Kind: Func, Label: symbols.FuncLabel(m),
					Pos: m.Pos(), Funcs: []*ssa.Function{m},
				})
			case *ssa.Type:
				if tn, ok := m.Object().(*types.TypeName); ok && tn.IsAlias() {
					continue // an alias declares nothing: its type's API is its own package's
				}
				out = append(out, typeSymbols(p.Prog, m.Type(), pkgPath, qual)...)
			}
		}
		for i := start; i < len(out); i++ {
			out[i].Internal = Internal(rel)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// typeSymbols lists the exported methods declared on an exported named type,
// or the methods of an exported interface.
func typeSymbols(prog *ssa.Program, t types.Type, pkgPath, qual string) []Symbol {
	named, ok := types.Unalias(t).(*types.Named)
	if !ok {
		return nil
	}
	tname := named.Obj().Name()
	var out []Symbol
	if iface, ok := named.Underlying().(*types.Interface); ok {
		for i := 0; i < iface.NumMethods(); i++ {
			m := iface.Method(i)
			if !m.Exported() {
				continue
			}
			out = append(out, Symbol{
				Key: qual + "." + tname + "." + m.Name(), Package: pkgPath, Kind: Interface,
				Label: named.Obj().Pkg().Name() + "." + tname + "." + m.Name(), Pos: m.Pos(), iface: iface, name: m.Name(),
			})
		}
		return out
	}
	for i := 0; i < named.NumMethods(); i++ {
		m := named.Method(i)
		if !m.Exported() {
			continue
		}
		fn := prog.FuncValue(m)
		if fn == nil {
			continue
		}
		out = append(out, Symbol{
			Key: qual + "." + tname + "." + m.Name(), Package: pkgPath, Kind: Method, Label: symbols.FuncLabel(fn),
			Pos: m.Pos(), Funcs: []*ssa.Function{fn},
		})
	}
	return out
}

// Roots returns the functions reachability starts from: the API's functions
// and methods (with the implementations Implement found) and the module's
// package initializers.
func Roots(api []Symbol, pkgs []*ssa.Package, modulePkgs map[string]bool) []*ssa.Function {
	seen := map[*ssa.Function]bool{}
	var out []*ssa.Function
	add := func(fn *ssa.Function) {
		if fn != nil && !seen[fn] {
			seen[fn] = true
			out = append(out, fn)
		}
	}
	for _, p := range pkgs {
		if modulePkgs[p.Pkg.Path()] && p.Pkg.Name() != "main" {
			add(p.Func("init"))
		}
	}
	for _, s := range api {
		for _, fn := range s.Funcs {
			add(fn)
		}
	}
	return out
}

// Generic counts the generic functions among roots: bodies with type
// parameters, which only CHA can start from.
func Generic(roots []*ssa.Function) int {
	n := 0
	for _, fn := range roots {
		if fn.TypeParams().Len() > 0 && len(fn.TypeArgs()) == 0 {
			n++
		}
	}
	return n
}

// Implement sets the Funcs of the API's interface methods: the methods of the
// module's types (in modulePkgs) among typs that implement the interface.
// typs are the exported types of the public packages (PublicTypes: a caller
// can pass any of them), plus the runtime types of a reachability pass (RTA's:
// the unexported types the API converts to interfaces), or every named type of
// the module's packages when the algorithm has none (CHA, ModuleTypes).
func Implement(api []Symbol, prog *ssa.Program, typs []types.Type, modulePkgs map[string]bool) {
	var concrete []types.Type
	seen := map[string]bool{}
	for _, t := range typs {
		n := symbols.Named(t)
		if n == nil || n.Obj().Pkg() == nil || !modulePkgs[n.Obj().Pkg().Path()] || types.IsInterface(n) {
			continue
		}
		// One entry per declared type: instances share their origin's methods.
		key := n.Origin().Obj().Pkg().Path() + "." + n.Origin().Obj().Name()
		if !seen[key] {
			seen[key] = true
			concrete = append(concrete, n.Origin())
		}
	}
	sort.Slice(concrete, func(i, j int) bool { return types.TypeString(concrete[i], nil) < types.TypeString(concrete[j], nil) })

	for i := range api {
		s := &api[i]
		if s.Kind != Interface {
			continue
		}
		s.Funcs = nil
		for _, t := range concrete {
			if !implements(t, s.iface) {
				continue
			}
			if fn := method(prog, t, s.name); fn != nil {
				s.Funcs = append(s.Funcs, fn)
			}
		}
	}
}

// PublicTypes returns the exported named types of the module's public
// packages.
func PublicTypes(pkgs []*ssa.Package, module string) []types.Type {
	var out []types.Type
	for _, p := range pkgs {
		rel, ok := relPath(p.Pkg.Path(), module)
		if !ok || p.Pkg.Name() == "main" || !Public(rel) {
			continue
		}
		for name, mem := range p.Members {
			if t, ok := mem.(*ssa.Type); ok && token.IsExported(name) {
				out = append(out, t.Type())
			}
		}
	}
	return out
}

// ModuleTypes returns the named types declared in the module's packages.
func ModuleTypes(pkgs []*ssa.Package, modulePkgs map[string]bool) []types.Type {
	var out []types.Type
	for _, p := range pkgs {
		if !modulePkgs[p.Pkg.Path()] {
			continue
		}
		for _, mem := range p.Members {
			if t, ok := mem.(*ssa.Type); ok {
				out = append(out, t.Type())
			}
		}
	}
	return out
}

// implements reports whether *T has every method of iface. Generic types
// and interfaces are compared by method names only: their type parameters
// differ.
func implements(t types.Type, iface *types.Interface) bool {
	ptr := types.NewPointer(t)
	if !isGeneric(t) && !hasTypeParams(iface) {
		return types.Implements(ptr, iface)
	}
	ms := types.NewMethodSet(ptr)
	for i := 0; i < iface.NumMethods(); i++ {
		m := iface.Method(i)
		if ms.Lookup(m.Pkg(), m.Name()) == nil {
			return false
		}
	}
	return iface.NumMethods() > 0
}

func isGeneric(t types.Type) bool {
	n, ok := t.(*types.Named)
	return ok && n.TypeParams().Len() > 0
}

// hasTypeParams reports whether an interface's methods mention type
// parameters (the underlying interface of a generic interface type).
func hasTypeParams(iface *types.Interface) bool {
	for i := 0; i < iface.NumMethods(); i++ {
		if sig, ok := iface.Method(i).Type().(*types.Signature); ok && containsTypeParam(sig) {
			return true
		}
	}
	return false
}

func containsTypeParam(sig *types.Signature) bool {
	for _, tup := range []*types.Tuple{sig.Params(), sig.Results()} {
		for i := 0; i < tup.Len(); i++ {
			if typeParamIn(tup.At(i).Type(), 0) {
				return true
			}
		}
	}
	return false
}

func typeParamIn(t types.Type, depth int) bool {
	if depth > 8 {
		return false
	}
	switch x := types.Unalias(t).(type) {
	case *types.TypeParam:
		return true
	case *types.Pointer:
		return typeParamIn(x.Elem(), depth+1)
	case *types.Slice:
		return typeParamIn(x.Elem(), depth+1)
	case *types.Array:
		return typeParamIn(x.Elem(), depth+1)
	case *types.Map:
		return typeParamIn(x.Key(), depth+1) || typeParamIn(x.Elem(), depth+1)
	case *types.Chan:
		return typeParamIn(x.Elem(), depth+1)
	case *types.Named:
		args := x.TypeArgs()
		for i := 0; args != nil && i < args.Len(); i++ {
			if typeParamIn(args.At(i), depth+1) {
				return true
			}
		}
	}
	return false
}

// method returns the function that runs for *T's method name: the declared
// method (the generic origin for a generic type), or the promoted one of an
// embedded type.
func method(prog *ssa.Program, t types.Type, name string) *ssa.Function {
	ms := types.NewMethodSet(types.NewPointer(t))
	for i := 0; i < ms.Len(); i++ {
		sel := ms.At(i)
		if sel.Obj().Name() != name {
			continue
		}
		obj, ok := sel.Obj().(*types.Func)
		if !ok {
			return nil
		}
		return prog.FuncValue(obj.Origin())
	}
	return nil
}

// Detections turns the API into inbound endpoints whose roots are what each
// symbol runs; a symbol with nothing to run (an interface nothing in the
// library implements) is an endpoint without a journey.
func Detections(api []Symbol, fset *token.FileSet, root, module string) []transport.Detection {
	out := make([]transport.Detection, 0, len(api))
	for _, s := range api {
		role := Role
		if s.Internal {
			role = model.APIInternalRole
		}
		ep := model.Endpoint{
			Transport: Transport, Role: role, Direction: model.Inbound,
			Keys: []string{s.Key}, Handler: s.Label, Component: s.Package,
			Position: symbols.Pos(fset, s.Pos, root), Source: "library", Rule: s.Kind,
		}
		var roots []resolve.Root
		var labels []string
		for _, fn := range s.Funcs {
			roots = append(roots, resolve.Root{Func: fn})
			labels = append(labels, symbols.FuncLabel(fn))
		}
		if s.Kind == Interface && len(labels) > 0 {
			ep.Handler = s.Label + " → " + strings.Join(labels, ", ")
		}
		if len(s.Funcs) == 1 {
			ep.Function = symbols.FuncLabel(s.Funcs[0])
		}
		out = append(out, transport.Detection{Endpoint: ep, Roots: roots})
	}
	return out
}

// Generated lists the files of the module's packages that are generated
// (a "// Code generated ... DO NOT EDIT." comment, ast.IsGenerated): their
// symbols, such as protobuf messages' getters, are left out of the API unless
// asked for.
func Generated(fset *token.FileSet, files map[string][]*ast.File) map[string]bool {
	out := map[string]bool{}
	for _, fs := range files {
		for _, f := range fs {
			if ast.IsGenerated(f) {
				if tf := fset.File(f.Pos()); tf != nil {
					out[tf.Name()] = true
				}
			}
		}
	}
	return out
}

func relPath(pkgPath, module string) (string, bool) {
	if pkgPath == module {
		return "", true
	}
	rel, ok := strings.CutPrefix(pkgPath, module+"/")
	return rel, ok
}
