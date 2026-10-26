package resolve

import (
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/zalgonoise/codegraph/internal/symbols"
)

// Root is a module function that runs when a registered handler is invoked:
// the start of a journey. Method is the interface method it implements when
// the handler was registered as an interface value (a gRPC server, an
// http.Handler, a sarama.ConsumerGroupHandler), "" for functions and closures.
//
// Bind is set on closures returned by a module handler factory (middleware(
// next), mw.Wrap(h)): it maps the closure's free variables to the handlers
// passed for them at this registration, so a journey follows the wrapped
// handler instead of every handler the factory is ever applied to.
type Root struct {
	Method string
	Func   *ssa.Function
	Bind   map[*ssa.FreeVar][]Root
}

// Innermost follows an unambiguous chain of wrapped handlers and returns the
// function at its end (Func itself when nothing is wrapped).
func (root Root) Innermost() *ssa.Function {
	for i := 0; i < 8 && len(root.Bind) == 1; i++ {
		var next []Root
		for _, rs := range root.Bind {
			next = rs
		}
		if len(next) != 1 {
			break
		}
		root = next[0]
	}
	return root.Func
}

// BoundCallees returns the handlers a call inside a bound closure dispatches
// to when its callee (or invoke receiver) is one of the bound free variables.
func (root Root) BoundCallees(c *ssa.CallCommon) ([]Root, bool) {
	fv, ok := unwrap(c.Value, true).(*ssa.FreeVar) // captured by reference: *fv
	if !ok {
		return nil, false
	}
	var out []Root
	for _, b := range root.Bind[fv] {
		if !c.IsInvoke() || b.Method == "" || b.Method == c.Method.Name() {
			out = append(out, b)
		}
	}
	return out, len(out) > 0
}

// factoryCall is the call of a module handler factory being followed into.
type factoryCall struct {
	callee *ssa.Function
	common *ssa.CallCommon
}

// HandlerFuncs resolves a registered handler value to the module functions
// that run when it is invoked. It follows functions, closures, bound method
// values, conversions (http.HandlerFunc(f)), interface values of concrete
// types, handler factories (itemHandler(store)), library middleware that
// wraps a handler of the same type, variadic chains (the last element),
// parameters (through callers) and fields or globals (through stores).
//
// For interface-typed handlers, methods limits which interface methods count;
// when empty, every exported method does (unexported ones are markers such as
// mustEmbedUnimplementedXServer).
func (r *Resolver) HandlerFuncs(v ssa.Value, methods []string) []Root {
	var out []Root
	r.handlerFuncs(v, methods, 0, map[ssa.Value]bool{}, &out, nil)
	return DedupeRoots(out)
}

// DedupeRoots merges roots of the same function and method, keeping the union
// of their bindings.
func DedupeRoots(roots []Root) []Root {
	type key struct {
		method string
		fn     *ssa.Function
	}
	idx := map[key]int{}
	var out []Root
	for _, root := range roots {
		k := key{root.Method, root.Func}
		i, ok := idx[k]
		if !ok {
			idx[k] = len(out)
			out = append(out, root)
			continue
		}
		for fv, bound := range root.Bind {
			if out[i].Bind == nil {
				out[i].Bind = map[*ssa.FreeVar][]Root{}
			}
			out[i].Bind[fv] = DedupeRoots(append(out[i].Bind[fv], bound...))
		}
	}
	return out
}

// handlerFuncs collects roots into out. fc is the handler factory whose
// results are being followed, if any (nil after leaving it).
func (r *Resolver) handlerFuncs(v ssa.Value, methods []string, depth int, seen map[ssa.Value]bool, out *[]Root, fc *factoryCall) {
	if v == nil || depth > r.maxDepth || seen[v] {
		return
	}
	seen[v] = true

	switch x := v.(type) {
	case *ssa.Function:
		r.addRoot(out, Root{Func: x})
	case *ssa.MakeClosure:
		fn, _ := x.Fn.(*ssa.Function)
		if fn == nil {
			return
		}
		if isBound(fn) && len(x.Bindings) == 1 {
			r.boundMethod(fn, x.Bindings[0], depth, seen, out)
			return
		}
		root := Root{Func: fn}
		if fc != nil && fn.Parent() == fc.callee {
			root.Bind = r.bindings(x, fc, depth)
		}
		r.addRoot(out, root)
	case *ssa.ChangeType:
		r.handlerFuncs(x.X, methods, depth, seen, out, fc)
	case *ssa.ChangeInterface: // a handler typed as one interface, passed as another
		r.handlerFuncs(x.X, methods, depth, seen, out, fc)
	case *ssa.Convert:
		r.handlerFuncs(x.X, methods, depth, seen, out, fc)
	case *ssa.MakeInterface:
		if _, ok := x.X.Type().Underlying().(*types.Signature); ok {
			r.handlerFuncs(x.X, methods, depth, seen, out, fc) // a func type boxed as an interface
			return
		}
		r.addMethods(out, x.X.Type(), x.Type(), methods)
	case *ssa.Phi:
		for _, e := range x.Edges {
			r.handlerFuncs(e, methods, depth, seen, out, fc)
		}
	case *ssa.Slice:
		// Variadic handler chains (gin/echo): the last one is the endpoint.
		if alloc, ok := x.X.(*ssa.Alloc); ok {
			if vals := elemValues(alloc); len(vals) > 0 {
				r.handlerFuncs(vals[len(vals)-1], methods, depth, seen, out, fc)
			}
		}
	case *ssa.Parameter:
		fn := x.Parent()
		for i, p := range fn.Params {
			if p != x || r.reach == nil {
				continue
			}
			for _, e := range r.reach.Callers(fn) {
				if e.Site != nil {
					r.handlerFuncs(argFor(e.Site.Common(), fn, i), methods, depth+1, seen, out, nil)
				}
			}
		}
	case *ssa.UnOp:
		if x.Op != token.MUL {
			return
		}
		switch addr := x.X.(type) {
		case *ssa.FieldAddr:
			r.handlersFromField(addr.X.Type(), addr.Field, methods, depth, seen, out)
		case *ssa.Global:
			for _, val := range r.globals[addr] {
				r.handlerFuncs(val, methods, depth+1, seen, out, nil)
			}
		}
	case *ssa.Field:
		r.handlersFromField(x.X.Type(), x.Field, methods, depth, seen, out)
	case *ssa.Call:
		r.handlersFromCall(&x.Call, 0, x.Type(), methods, depth, seen, out)
	case *ssa.Extract:
		if call, ok := x.Tuple.(*ssa.Call); ok {
			r.handlersFromCall(&call.Call, x.Index, x.Type(), methods, depth, seen, out)
		}
	}
}

// bindings maps the handler-typed free variables of a closure returned by a
// factory to the handlers passed for the factory parameters they capture.
// Captured variables are spilled to the heap in SSA, so a binding is usually
// an Alloc holding the parameter (and whatever else the factory stored).
func (r *Resolver) bindings(mc *ssa.MakeClosure, fc *factoryCall, depth int) map[*ssa.FreeVar][]Root {
	fn := mc.Fn.(*ssa.Function)
	var bind map[*ssa.FreeVar][]Root
	for k, b := range mc.Bindings {
		if k >= len(fn.FreeVars) || !handlerLike(deref(fn.FreeVars[k].Type())) {
			continue
		}
		vals := []ssa.Value{b}
		if alloc, ok := b.(*ssa.Alloc); ok {
			vals = storedValues(alloc)
		}
		var roots []Root
		for _, v := range vals {
			if p, ok := unwrap(v, false).(*ssa.Parameter); ok && p.Parent() == fc.callee {
				for i, q := range fc.callee.Params {
					if q == p {
						r.handlerFuncs(argFor(fc.common, fc.callee, i), nil, depth+1, map[ssa.Value]bool{}, &roots, nil)
					}
				}
				continue
			}
			r.handlerFuncs(v, nil, depth+1, map[ssa.Value]bool{}, &roots, fc)
		}
		if roots = DedupeRoots(roots); len(roots) > 0 {
			if bind == nil {
				bind = map[*ssa.FreeVar][]Root{}
			}
			bind[fn.FreeVars[k]] = roots
		}
	}
	return bind
}

// handlerLike reports whether values of t can be handlers: funcs and
// interfaces with methods.
func handlerLike(t types.Type) bool {
	switch u := t.Underlying().(type) {
	case *types.Signature:
		return true
	case *types.Interface:
		return u.NumMethods() > 0
	}
	return false
}

func (r *Resolver) handlersFromField(owner types.Type, idx int, methods []string, depth int, seen map[ssa.Value]bool, out *[]Root) {
	if k, ok := keyOf(owner, idx); ok {
		for _, val := range r.fields[k] {
			r.handlerFuncs(val, methods, depth+1, seen, out, nil)
		}
	}
}

// handlersFromCall follows a handler factory: into the results of a module
// function, or, for library middleware (http.StripPrefix(p, h),
// otelhttp.NewHandler(h, ...)), into the arguments of the handler's own type.
func (r *Resolver) handlersFromCall(c *ssa.CallCommon, index int, result types.Type, methods []string, depth int, seen map[ssa.Value]bool, out *[]Root) {
	if callee := c.StaticCallee(); callee != nil && r.modulePkgs[symbols.PkgPath(callee)] {
		fc := &factoryCall{callee: callee, common: c}
		for _, b := range callee.Blocks {
			if len(b.Instrs) == 0 {
				continue
			}
			if ret, ok := b.Instrs[len(b.Instrs)-1].(*ssa.Return); ok && index < len(ret.Results) {
				r.handlerFuncs(ret.Results[index], methods, depth+1, seen, out, fc)
			}
		}
		return
	}
	for _, a := range Args(c) {
		if types.Identical(a.Type(), result) {
			r.handlerFuncs(a, methods, depth+1, seen, out, nil)
		}
	}
}

// boundMethod resolves a method value (h.List). DeleteSyntheticNodes removes
// the $bound wrapper from the call graph, so the real method is looked up.
func (r *Resolver) boundMethod(fn *ssa.Function, recv ssa.Value, depth int, seen map[ssa.Value]bool, out *[]Root) {
	obj, _ := fn.Object().(*types.Func)
	if obj == nil {
		return
	}
	if types.IsInterface(recv.Type()) { // a method value of an interface: find the dynamic types
		r.handlerFuncs(recv, []string{obj.Name()}, depth+1, seen, out, nil)
		return
	}
	r.addRoot(out, Root{Func: r.method(recv.Type(), obj.Pkg(), obj.Name())})
}

// addMethods adds the methods of iface (filtered by names) implemented by the
// concrete type t.
func (r *Resolver) addMethods(out *[]Root, t, iface types.Type, names []string) {
	it, ok := iface.Underlying().(*types.Interface)
	if !ok || types.IsInterface(t) {
		return
	}
	for i := 0; i < it.NumMethods(); i++ {
		m := it.Method(i)
		if !m.Exported() || (len(names) > 0 && !containsName(names, m.Name())) {
			continue
		}
		r.addRoot(out, Root{Method: m.Name(), Func: r.method(t, m.Pkg(), m.Name())})
	}
}

// method returns the function implementing method (pkg, name) of t, skipping
// the wrappers that have no syntax (pointer receivers of value methods,
// promoted methods), which are not nodes of the call graph.
func (r *Resolver) method(t types.Type, pkg *types.Package, name string) *ssa.Function {
	if r.prog == nil || types.IsInterface(t) {
		return nil
	}
	sel := r.prog.MethodSets.MethodSet(t).Lookup(pkg, name)
	if sel == nil {
		return nil
	}
	fn := r.prog.MethodValue(sel)
	if fn == nil || fn.Syntax() == nil {
		if obj, ok := sel.Obj().(*types.Func); ok {
			if declared := r.prog.FuncValue(obj); declared != nil {
				fn = declared
			}
		}
	}
	return fn
}

func (r *Resolver) addRoot(out *[]Root, root Root) {
	fn := unwrapWrapper(root.Func)
	if fn == nil || len(fn.Blocks) == 0 || !r.modulePkgs[symbols.PkgPath(fn)] {
		return
	}
	if strings.HasPrefix(symbols.RecvName(fn), "Unimplemented") { // generated gRPC stubs
		return
	}
	root.Func = fn
	*out = append(*out, root)
}

// unwrapWrapper follows synthetic wrappers without syntax to the function
// they call statically.
func unwrapWrapper(fn *ssa.Function) *ssa.Function {
	for i := 0; i < 4 && fn != nil && fn.Synthetic != "" && fn.Syntax() == nil && fn.Synthetic != symbols.PackageInit; i++ {
		var next *ssa.Function
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				if site, ok := instr.(ssa.CallInstruction); ok {
					if callee := site.Common().StaticCallee(); callee != nil {
						next = callee
					}
				}
			}
		}
		if next == nil {
			break
		}
		fn = next
	}
	return fn
}

func isBound(fn *ssa.Function) bool {
	return fn.Synthetic != "" && strings.HasSuffix(fn.Name(), "$bound")
}

func containsName(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
