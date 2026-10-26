// Package resolve recovers statically-known values from SSA: the strings that
// become routes, topics and URLs, the handlers registered for them, and the
// message types that are sent.
//
// It is a deliberately small, context-insensitive propagation:
//
//   - constants, conversions, phi nodes and string concatenation;
//   - parameters, by following the call graph to every caller;
//   - struct fields and globals, via a flow-insensitive index of every store
//     performed by reachable module code (so `cfg.Topic` resolves to whatever
//     constant was assigned to Config.Topic anywhere);
//   - return values of module functions and fmt.Sprintf formats;
//   - elements of slices built from literals (range loops over them);
//   - strings returned by a method of a value's dynamic types (MethodStrings).
//
// Anything it cannot see becomes "*" so partially known keys such as
// "http://inventory:8081/items/*" still carry useful information.
package resolve

import (
	"go/constant"
	"go/token"
	"go/types"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/zalgonoise/codegraph/internal/model"
	"github.com/zalgonoise/codegraph/internal/reach"
	"github.com/zalgonoise/codegraph/internal/symbols"
)

type fieldKey struct {
	owner string // type string of the struct (after one dereference)
	field string
}

// Resolver resolves SSA values for one analysed service.
type Resolver struct {
	prog       *ssa.Program
	reach      *reach.Result
	modulePkgs map[string]bool
	fields     map[fieldKey][]ssa.Value
	globals    map[*ssa.Global][]ssa.Value
	libInits   map[*ssa.Package]map[*ssa.Global][]ssa.Value
	base       *frame // set by InCall: one caller's arguments for a function's parameters
	maxDepth   int
	maxValues  int
}

// New indexes every field and global store in funcs (reachable module code).
func New(prog *ssa.Program, res *reach.Result, funcs []*ssa.Function, modulePkgs map[string]bool, maxDepth int) *Resolver {
	if maxDepth <= 0 {
		maxDepth = 4
	}
	r := &Resolver{
		prog:       prog,
		reach:      res,
		modulePkgs: modulePkgs,
		fields:     map[fieldKey][]ssa.Value{},
		globals:    map[*ssa.Global][]ssa.Value{},
		libInits:   map[*ssa.Package]map[*ssa.Global][]ssa.Value{},
		maxDepth:   maxDepth,
		maxValues:  16,
	}
	for _, fn := range funcs {
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				st, ok := instr.(*ssa.Store)
				if !ok {
					continue
				}
				switch addr := st.Addr.(type) {
				case *ssa.FieldAddr:
					if k, ok := keyOf(addr.X.Type(), addr.Field); ok {
						r.fields[k] = append(r.fields[k], st.Val)
					}
				case *ssa.Global:
					r.globals[addr] = append(r.globals[addr], st.Val)
				}
			}
		}
	}
	return r
}

// Args returns the arguments of a call with the receiver (if any) at index 0,
// for both static method calls and interface ("invoke") calls.
func Args(c *ssa.CallCommon) []ssa.Value {
	if c.IsInvoke() {
		return append([]ssa.Value{c.Value}, c.Args...)
	}
	return c.Args
}

// InCall returns a view of the resolver in which the parameters of fn are the
// given arguments of one call site (aligned with fn.Params), so values inside a
// shared helper resolve to what that caller passes.
func (r *Resolver) InCall(fn *ssa.Function, args []ssa.Value) *Resolver {
	v := *r
	v.base = &frame{fn: fn, args: args}
	return &v
}

// Strings returns the possible string values of v (possibly empty).
func (r *Resolver) Strings(v ssa.Value) []string {
	return normalize(r.strs(v, 0, map[ssa.Value]bool{}, r.base), r.maxValues)
}

// StringsOrUnknown is Strings, or ["*"] when nothing could be resolved.
func (r *Resolver) StringsOrUnknown(v ssa.Value) []string {
	if s := r.Strings(v); len(s) > 0 {
		return s
	}
	return []string{model.Unknown}
}

// FieldStrings resolves the string value(s) of a named field of v, where v is
// a struct, a pointer to one, or an interface wrapping either. A composite
// literal in the same function wins; otherwise every store to that field in
// module code is used. A dotted path ("Queues.PriorityQueue", through the
// elements of a slice) resolves only from the particular value.
func (r *Resolver) FieldStrings(v ssa.Value, field string) []string {
	if strings.Contains(field, ".") {
		var out []string
		for _, f := range r.fieldPath(v, field) {
			out = append(out, r.strs(f.v, f.depth, map[ssa.Value]bool{}, f.fr)...)
		}
		return normalize(out, r.maxValues)
	}
	if out := r.fieldStrs(unwrap(v, false), field, 0, map[ssa.Value]bool{}, r.base); len(out) > 0 {
		return normalize(out, r.maxValues)
	}
	if alloc, ok := unwrap(v, true).(*ssa.Alloc); ok {
		var out []string
		seen := map[ssa.Value]bool{}
		for _, fa := range fieldAddrs(alloc, field) {
			for _, val := range storedValues(fa) {
				out = append(out, r.strs(val, 0, seen, r.base)...)
			}
		}
		if len(out) > 0 {
			return normalize(out, r.maxValues)
		}
	}
	typed := unwrap(v, false)
	if s := structOf(typed.Type()); s != nil {
		for i := 0; i < s.NumFields(); i++ {
			if s.Field(i).Name() == field {
				return normalize(r.fromField(typed.Type(), i, 0, map[ssa.Value]bool{}), r.maxValues)
			}
		}
	}
	return nil
}

// Handler describes the function or type registered as a handler, returning
// a label and the package that implements it.
func (r *Resolver) Handler(v ssa.Value) (label, pkg string) {
	v = unwrap(v, false)
	switch x := v.(type) {
	case *ssa.Function:
		return symbols.FuncLabel(x), symbols.PkgPath(x)
	case *ssa.MakeClosure:
		fn, _ := x.Fn.(*ssa.Function)
		if fn == nil {
			return "", ""
		}
		// Method values (h.List) are bound-method closures "List$bound".
		if fn.Synthetic != "" && len(x.Bindings) == 1 && strings.HasSuffix(fn.Name(), "$bound") {
			recv := x.Bindings[0].Type()
			return symbols.MethodLabel(recv, strings.TrimSuffix(fn.Name(), "$bound")), symbols.TypePkg(recv)
		}
		return symbols.FuncLabel(fn), symbols.PkgPath(fn)
	case *ssa.Call:
		// Handler factories: mux.Handle("/x", newHandler(deps)).
		if fn := x.Call.StaticCallee(); fn != nil {
			return symbols.FuncLabel(fn) + "(…)", symbols.PkgPath(fn)
		}
	case *ssa.Slice:
		// Variadic handler chains (gin/echo): the last one is the endpoint.
		if alloc, ok := x.X.(*ssa.Alloc); ok {
			if vals := elemValues(alloc); len(vals) > 0 {
				return r.Handler(vals[len(vals)-1])
			}
		}
	}
	if n := symbols.Named(v.Type()); n != nil && !symbols.IsInterface(v.Type()) {
		return symbols.TypeLabel(v.Type()), symbols.TypePkg(v.Type())
	}
	return "", ""
}

// MessageType returns the concrete type of a message argument, or "" when it
// is untyped payload (interface, []byte, string, ...).
func MessageType(v ssa.Value) string {
	v = unwrap(v, false)
	t := v.Type()
	if symbols.IsInterface(t) {
		return ""
	}
	switch u := t.Underlying().(type) {
	case *types.Basic:
		return ""
	case *types.Slice:
		if b, ok := u.Elem().Underlying().(*types.Basic); ok && b.Kind() == types.Uint8 {
			return ""
		}
	}
	return symbols.TypeLabel(t)
}

// frame binds the parameters of a function followed from one call to that
// call's arguments: one level of call-site sensitivity, so a helper like
// topic(name) or Topic.String() resolves to what this call passes rather than
// to the union over every caller.
type frame struct {
	fn     *ssa.Function
	args   []ssa.Value // aligned with fn.Params
	parent *frame
}

func (r *Resolver) strs(v ssa.Value, depth int, seen map[ssa.Value]bool, fr *frame) []string {
	if v == nil || depth > r.maxDepth || seen[v] {
		return nil
	}
	seen[v] = true
	defer delete(seen, v)

	switch x := v.(type) {
	case *ssa.Const:
		if x.Value != nil && x.Value.Kind() == constant.String {
			return []string{constant.StringVal(x.Value)}
		}
	case *ssa.MakeInterface:
		return r.strs(x.X, depth, seen, fr)
	case *ssa.ChangeType:
		return r.strs(x.X, depth, seen, fr)
	case *ssa.ChangeInterface:
		return r.strs(x.X, depth, seen, fr)
	case *ssa.Convert:
		return r.strs(x.X, depth, seen, fr)
	case *ssa.Phi:
		var out []string
		for _, e := range x.Edges {
			out = append(out, r.strs(e, depth, seen, fr)...)
		}
		return out
	case *ssa.BinOp:
		if x.Op == token.ADD {
			return concat(orUnknown(r.strs(x.X, depth, seen, fr)), orUnknown(r.strs(x.Y, depth, seen, fr)), r.maxValues)
		}
	case *ssa.Parameter:
		if arg, parent, ok := fr.bound(x); ok {
			return r.strs(arg, depth, seen, parent)
		}
		if !r.modulePkgs[symbols.PkgPath(x.Parent())] {
			return nil // never trace a library function's callers
		}
		return r.fromParam(x, depth, seen)
	case *ssa.UnOp:
		if x.Op == token.MUL {
			switch addr := x.X.(type) {
			case *ssa.FieldAddr:
				if out := r.fieldStrs(addr.X, fieldName(addr.X.Type(), addr.Field), depth, seen, fr); len(out) > 0 {
					return out
				}
				if out := r.fromField(addr.X.Type(), addr.Field, depth, seen); len(out) > 0 {
					return out
				}
				return r.envDefault(addr.X.Type(), addr.Field)
			case *ssa.Global:
				var out []string
				for _, val := range r.globalStores(addr) {
					out = append(out, r.strs(val, depth+1, seen, nil)...)
				}
				return out
			case *ssa.IndexAddr: // for _, q := range queues() / xs[i]
				return r.strs(addr.X, depth, seen, fr)
			case *ssa.FreeVar: // a variable captured by reference: what is stored into it
				var out []string
				for _, b := range freeVarBindings(addr) {
					for _, val := range storedValues(b) {
						out = append(out, r.strs(val, depth+1, seen, nil)...)
					}
				}
				return out
			}
		}
	case *ssa.FreeVar: // captured by value
		var out []string
		for _, b := range freeVarBindings(x) {
			out = append(out, r.strs(b, depth+1, seen, nil)...)
		}
		return out
	case *ssa.Field:
		if out := r.fieldStrs(x.X, fieldName(x.X.Type(), x.Field), depth, seen, fr); len(out) > 0 {
			return out
		}
		if out := r.fromField(x.X.Type(), x.Field, depth, seen); len(out) > 0 {
			return out
		}
		return r.envDefault(x.X.Type(), x.Field)
	case *ssa.Slice:
		if alloc, ok := x.X.(*ssa.Alloc); ok {
			var out []string
			for _, el := range elemValues(alloc) {
				out = append(out, r.strs(el, depth, seen, fr)...)
			}
			return out
		}
		return r.strs(x.X, depth, seen, fr)
	case *ssa.Call:
		if b, ok := x.Call.Value.(*ssa.Builtin); ok && b.Name() == "append" && len(x.Call.Args) == 2 {
			return append(r.strs(x.Call.Args[0], depth, seen, fr), r.strs(x.Call.Args[1], depth, seen, fr)...)
		}
		return r.fromCall(&x.Call, 0, depth, seen, fr)
	case *ssa.Extract:
		if call, ok := x.Tuple.(*ssa.Call); ok {
			return r.fromCall(&call.Call, x.Index, depth, seen, fr)
		}
	}
	return nil
}

// valueIn is a value together with the frame its parameters resolve in.
type valueIn struct {
	v  ssa.Value
	fr *frame
}

// fieldStrs resolves field name of the particular struct v denotes (see
// fieldVals). It returns nothing when v cannot be followed, and callers fall
// back on every store to the field.
func (r *Resolver) fieldStrs(v ssa.Value, name string, depth int, seen map[ssa.Value]bool, fr *frame) []string {
	var out []string
	for _, f := range r.fieldVals(v, name, depth, seen, fr) {
		out = append(out, r.strs(f.v, f.depth, seen, f.fr)...)
	}
	return out
}

// fieldVal is a value a field holds, with the frame and depth it was found
// at, which its own resolution continues from.
type fieldVal struct {
	v     ssa.Value
	fr    *frame
	depth int
}

// emptyString stands for a string field that keeps its zero value.
var emptyString = ssa.NewConst(constant.MakeString(""), types.Typ[types.String])

// fieldVals lists the values field name holds in the particular struct v
// denotes: a struct value or the address of one, built by a composite
// literal, returned by a followed call, or taken from a slice element.
func (r *Resolver) fieldVals(v ssa.Value, name string, depth int, seen map[ssa.Value]bool, fr *frame) []fieldVal {
	if v == nil || name == "" || depth > r.maxDepth || seen[v] {
		return nil
	}
	seen[v] = true
	defer delete(seen, v)

	var out []fieldVal
	switch x := v.(type) {
	case *ssa.Alloc, *ssa.IndexAddr: // fields stored in place, or whole values stored
		for _, fa := range fieldAddrs(v, name) {
			for _, val := range storedValues(fa) {
				out = append(out, fieldVal{val, fr, depth})
			}
		}
		for _, val := range storedValues(v) {
			out = append(out, r.fieldVals(val, name, depth, seen, fr)...)
		}
		if ia, ok := x.(*ssa.IndexAddr); ok && len(out) == 0 { // an element of a slice built elsewhere
			for _, el := range r.elements(ia.X, depth, seen, fr) {
				out = append(out, r.fieldVals(el.v, name, depth, seen, el.fr)...)
			}
		}
		if alloc, ok := x.(*ssa.Alloc); ok && len(out) == 0 && zeroField(alloc, name) {
			out = []fieldVal{{emptyString, fr, depth}} // ChiServerOptions{BaseRouter: r}: BaseURL is ""
		}
	case *ssa.UnOp:
		if x.Op == token.MUL {
			out = r.fieldVals(x.X, name, depth, seen, fr)
		}
	case *ssa.MakeInterface:
		out = r.fieldVals(x.X, name, depth, seen, fr)
	case *ssa.ChangeType:
		out = r.fieldVals(x.X, name, depth, seen, fr)
	case *ssa.ChangeInterface:
		out = r.fieldVals(x.X, name, depth, seen, fr)
	case *ssa.Phi:
		for _, e := range x.Edges {
			out = append(out, r.fieldVals(e, name, depth, seen, fr)...)
		}
	case *ssa.Parameter:
		if arg, parent, ok := fr.bound(x); ok {
			out = r.fieldVals(arg, name, depth, seen, parent)
		} else if fn := x.Parent(); r.reach != nil && r.modulePkgs[symbols.PkgPath(fn)] {
			for i, p := range fn.Params { // a struct passed by value: what each caller passes
				if p != x {
					continue
				}
				for _, e := range r.reach.Callers(fn) {
					if e.Site != nil {
						out = append(out, r.fieldVals(argFor(e.Site.Common(), fn, i), name, depth+1, seen, nil)...)
					}
				}
			}
		}
	case *ssa.Call, *ssa.Extract:
		for _, res := range r.results(x, depth, fr) {
			out = append(out, r.fieldVals(res.v, name, depth+1, seen, res.fr)...)
		}
	case *ssa.FreeVar: // r.Group(func(r chi.Router) { r.Get(options.BaseURL+...) })
		for _, b := range freeVarBindings(x) {
			out = append(out, r.fieldVals(b, name, depth+1, seen, nil)...)
		}
	case *ssa.Global: // var cmd = cobra.Command{...}: a literal built in a local, copied into the global
		for _, val := range r.globalStores(x) {
			out = append(out, r.fieldVals(val, name, depth+1, seen, nil)...)
		}
	}
	return out
}

// fieldPath follows a dotted field path ("Queues.PriorityQueue") from v; a
// slice-typed field on the way stands for each of its elements.
func (r *Resolver) fieldPath(v ssa.Value, path string) []fieldVal {
	cur := []fieldVal{{unwrap(v, false), r.base, 0}}
	names := strings.Split(path, ".")
	for i, name := range names {
		seen := map[ssa.Value]bool{}
		var next []fieldVal
		for _, c := range cur {
			for _, f := range r.fieldVals(c.v, name, c.depth, seen, c.fr) {
				if _, ok := f.v.Type().Underlying().(*types.Slice); ok && i < len(names)-1 {
					for _, el := range r.elements(f.v, f.depth, seen, f.fr) {
						next = append(next, fieldVal{el.v, el.fr, f.depth})
					}
					continue
				}
				next = append(next, f)
			}
		}
		cur = next
	}
	return cur
}

// FieldValues returns the values a field (or dotted field path) of the
// particular struct v denotes holds, or nothing when v cannot be followed.
func (r *Resolver) FieldValues(v ssa.Value, path string) []ssa.Value {
	var out []ssa.Value
	for _, f := range r.fieldPath(v, path) {
		out = append(out, f.v)
	}
	return out
}

// FreeVarBindings returns the values bound to a closure's free variable.
func FreeVarBindings(fv *ssa.FreeVar) []ssa.Value { return freeVarBindings(fv) }

// freeVarBindings returns the values bound to a closure's free variable by
// the MakeClosure instructions that create it.
func freeVarBindings(fv *ssa.FreeVar) []ssa.Value {
	fn := fv.Parent()
	idx := -1
	for i, f := range fn.FreeVars {
		if f == fv {
			idx = i
		}
	}
	parent := fn.Parent()
	if idx < 0 || parent == nil {
		return nil
	}
	var out []ssa.Value
	for _, b := range parent.Blocks {
		for _, instr := range b.Instrs {
			if mc, ok := instr.(*ssa.MakeClosure); ok && mc.Fn == fn && idx < len(mc.Bindings) {
				out = append(out, mc.Bindings[idx])
			}
		}
	}
	return out
}

// Elements lists the values a slice holds: its literal's elements (or the
// addresses of elements initialised in place), through append, calls returning
// it and bound parameters (a variadic argument is such a slice).
func (r *Resolver) Elements(v ssa.Value) []ssa.Value {
	var out []ssa.Value
	for _, e := range r.elements(v, 0, map[ssa.Value]bool{}, nil) {
		out = append(out, e.v)
	}
	return out
}

// elements lists the elements of a slice value (or the addresses of elements
// initialised in place), following literals, append, followed calls and bound
// parameters.
func (r *Resolver) elements(v ssa.Value, depth int, seen map[ssa.Value]bool, fr *frame) []valueIn {
	if v == nil || depth > r.maxDepth || seen[v] {
		return nil
	}
	seen[v] = true
	defer delete(seen, v)

	var out []valueIn
	switch x := v.(type) {
	case *ssa.Slice:
		if alloc, ok := x.X.(*ssa.Alloc); ok {
			if refs := alloc.Referrers(); refs != nil {
				for _, ref := range *refs {
					ia, ok := ref.(*ssa.IndexAddr)
					if !ok || ia.X != alloc {
						continue
					}
					vals := storedValues(ia)
					for _, val := range vals {
						out = append(out, valueIn{val, fr})
					}
					if len(vals) == 0 {
						out = append(out, valueIn{ia, fr})
					}
				}
			}
			return out
		}
		return r.elements(x.X, depth, seen, fr)
	case *ssa.Phi:
		for _, e := range x.Edges {
			out = append(out, r.elements(e, depth, seen, fr)...)
		}
	case *ssa.Parameter:
		if arg, parent, ok := fr.bound(x); ok {
			out = r.elements(arg, depth, seen, parent)
		}
	case *ssa.Call:
		if b, ok := x.Call.Value.(*ssa.Builtin); ok && b.Name() == "append" && len(x.Call.Args) == 2 {
			return append(r.elements(x.Call.Args[0], depth, seen, fr), r.elements(x.Call.Args[1], depth, seen, fr)...)
		}
		for _, res := range r.results(x, depth, fr) {
			out = append(out, r.elements(res.v, depth+1, seen, res.fr)...)
		}
	case *ssa.Extract:
		for _, res := range r.results(x, depth, fr) {
			out = append(out, r.elements(res.v, depth+1, seen, res.fr)...)
		}
	}
	return out
}

// results lists the values a call (or an extract of one) may return, each in
// the frame binding the callee's parameters to this call's arguments.
func (r *Resolver) results(v ssa.Value, depth int, fr *frame) []valueIn {
	index := 0
	if ex, ok := v.(*ssa.Extract); ok {
		index, v = ex.Index, ex.Tuple
	}
	call, ok := v.(*ssa.Call)
	if !ok || depth >= r.maxDepth {
		return nil
	}
	callee := call.Call.StaticCallee()
	if callee == nil || !r.followable(callee) {
		return nil
	}
	inner := &frame{fn: callee, args: call.Call.Args, parent: fr}
	var out []valueIn
	for _, b := range callee.Blocks {
		if len(b.Instrs) == 0 {
			continue
		}
		if ret, ok := b.Instrs[len(b.Instrs)-1].(*ssa.Return); ok && index < len(ret.Results) {
			out = append(out, valueIn{ret.Results[index], inner})
		}
	}
	return out
}

// bound returns the argument a frame binds parameter p to.
func (fr *frame) bound(p *ssa.Parameter) (ssa.Value, *frame, bool) {
	if fr == nil || p.Parent() != fr.fn {
		return nil, nil, false
	}
	for i, q := range fr.fn.Params {
		if q == p && i < len(fr.args) {
			return fr.args[i], fr.parent, true
		}
	}
	return nil, nil, false
}

func fieldName(owner types.Type, idx int) string {
	if s := structOf(owner); s != nil && idx >= 0 && idx < s.NumFields() {
		return s.Field(idx).Name()
	}
	return ""
}

func (r *Resolver) fromParam(p *ssa.Parameter, depth int, seen map[ssa.Value]bool) []string {
	fn := p.Parent()
	idx := -1
	for i, q := range fn.Params {
		if q == p {
			idx = i
			break
		}
	}
	if idx < 0 || r.reach == nil {
		return nil
	}
	var out []string
	for _, e := range r.reach.Callers(fn) {
		if e.Site == nil {
			continue
		}
		if a := argFor(e.Site.Common(), fn, idx); a != nil {
			out = append(out, r.strs(a, depth+1, seen, nil)...)
		}
	}
	return out
}

// argFor maps callee parameter idx to the argument at a call site. Method
// values (bound closures) are called without the receiver, hence the shift.
func argFor(c *ssa.CallCommon, callee *ssa.Function, idx int) ssa.Value {
	args := Args(c)
	if callee.Signature.Recv() != nil && c.Signature().Recv() == nil {
		idx--
	}
	if idx < 0 || idx >= len(args) {
		return nil
	}
	return args[idx]
}

// envDefault is the default of a configuration field that nothing in module
// code sets, because an env library fills it by reflection: the envDefault
// tag next to an env tag (caarlos0/env), `env:"QUEUE" envDefault:"orders"`,
// or the default tag next to an envconfig tag (kelseyhightower/envconfig),
// `envconfig:"QUEUE" default:"orders"`.
// Like any value with a default, it stands for what the code chooses when the
// setting is absent. Only types of the module (and included modules) count:
// libraries' defaults are placeholders (a broker client's "priority_queue", "localhost")
// that would make every service using them look connected. Defaults naming
// the local machine (":8082", "127.0.0.1:50051", "http://localhost:8080") are
// development placeholders too, and stay unknown.
func (r *Resolver) envDefault(owner types.Type, idx int) []string {
	named, ok := types.Unalias(deref(owner)).(*types.Named)
	s := structOf(owner)
	if !ok || s == nil || idx < 0 || idx >= s.NumFields() || named.Obj().Pkg() == nil || !r.modulePkgs[named.Obj().Pkg().Path()] {
		return nil
	}
	tag := reflect.StructTag(s.Tag(idx))
	env, _, _ := strings.Cut(tag.Get("env"), ",")
	if def, ok := tag.Lookup("envDefault"); ok && env != "" && env != "-" && !localAddress(def) {
		return []string{def} // caarlos0/env
	}
	if def, ok := tag.Lookup("default"); ok && tag.Get("envconfig") != "" && !localAddress(def) {
		return []string{def} // kelseyhightower/envconfig
	}
	return nil
}

var localAddressRe = regexp.MustCompile(`^(https?://)?(localhost|127\.0\.0\.1|0\.0\.0\.0|\[::1\])?(:\d+)?(/.*)?$`)

// localAddress reports whether v addresses the local machine: a loopback or
// unspecified host, or a port alone, with or without a scheme and path.
func localAddress(v string) bool {
	m := localAddressRe.FindStringSubmatch(v)
	return m != nil && (m[2] != "" || m[3] != "")
}

func (r *Resolver) fromField(owner types.Type, idx, depth int, seen map[ssa.Value]bool) []string {
	k, ok := keyOf(owner, idx)
	if !ok {
		return nil
	}
	var out []string
	for _, val := range r.fields[k] {
		out = append(out, r.strs(val, depth+1, seen, nil)...)
	}
	return out
}

var verbRe = regexp.MustCompile(`%[-+# 0-9.*\[\]]*[a-zA-Z%]`)

// fromCall resolves the result index of a call: fmt.Sprintf formats, the
// returns of module functions, and of straight-line library functions such
// as Topic() or Topic.String() (only single-block bodies: anything with
// branches, like os.Getenv, stays unknown). Interface calls follow the
// methods of the receiver's dynamic types. Callee parameters are bound to
// this call's arguments.
func (r *Resolver) fromCall(c *ssa.CallCommon, index, depth int, seen map[ssa.Value]bool, fr *frame) []string {
	if c.IsInvoke() {
		var out []string
		for _, t := range r.dynamicTypes(c.Value, depth, map[ssa.Value]bool{}, fr) {
			if fn := r.method(t, c.Method.Pkg(), c.Method.Name()); fn != nil && r.followable(fn) {
				out = append(out, r.returns(fn, index, Args(c), depth, seen, fr)...)
			}
		}
		return out
	}
	callee := c.StaticCallee()
	if callee == nil {
		// A call through a func-typed parameter bound in this frame: the callback
		// the caller passed.
		if p, ok := unwrap(c.Value, false).(*ssa.Parameter); ok {
			if arg, _, ok := fr.bound(p); ok {
				var out []string
				for _, fn := range funcsOf(arg) {
					if r.followable(fn) {
						out = append(out, r.returns(fn, index, c.Args, depth, seen, fr)...)
					}
				}
				return out
			}
		}
		return nil
	}
	if callee.String() == "fmt.Sprintf" && index == 0 && len(c.Args) > 0 {
		var out []string
		for _, format := range r.strs(c.Args[0], depth, seen, fr) {
			out = append(out, verbRe.ReplaceAllStringFunc(format, func(m string) string {
				if m == "%%" {
					return "%"
				}
				return model.Unknown
			}))
		}
		return out
	}
	if !r.followable(callee) {
		return nil
	}
	return r.returns(callee, index, c.Args, depth, seen, fr)
}

// followable reports whether fromCall may look into fn's returns.
func (r *Resolver) followable(fn *ssa.Function) bool {
	return r.modulePkgs[symbols.PkgPath(fn)] || len(fn.Blocks) == 1
}

// returns resolves result index of fn with its parameters bound to args.
func (r *Resolver) returns(fn *ssa.Function, index int, args []ssa.Value, depth int, seen map[ssa.Value]bool, fr *frame) []string {
	inner := &frame{fn: fn, args: args, parent: fr}
	var out []string
	for _, b := range fn.Blocks {
		if len(b.Instrs) == 0 {
			continue
		}
		if ret, ok := b.Instrs[len(b.Instrs)-1].(*ssa.Return); ok && index < len(ret.Results) {
			out = append(out, r.strs(ret.Results[index], depth+1, seen, inner)...)
		}
	}
	return out
}

// --- helpers -----------------------------------------------------------------

func unwrap(v ssa.Value, derefs bool) ssa.Value {
	for {
		switch x := v.(type) {
		case *ssa.MakeInterface:
			v = x.X
		case *ssa.ChangeType:
			v = x.X
		case *ssa.ChangeInterface:
			v = x.X
		case *ssa.Convert:
			v = x.X
		case *ssa.UnOp:
			if !derefs || x.Op != token.MUL {
				return v
			}
			v = x.X
		default:
			return v
		}
	}
}

func deref(t types.Type) types.Type {
	if p, ok := types.Unalias(t).(*types.Pointer); ok {
		return p.Elem()
	}
	return t
}

func structOf(t types.Type) *types.Struct {
	s, _ := deref(t).Underlying().(*types.Struct)
	return s
}

func keyOf(owner types.Type, idx int) (fieldKey, bool) {
	s := structOf(owner)
	if s == nil || idx < 0 || idx >= s.NumFields() {
		return fieldKey{}, false
	}
	return fieldKey{owner: types.TypeString(deref(owner), nil), field: s.Field(idx).Name()}, true
}

func fieldAddrs(v ssa.Value, field string) []*ssa.FieldAddr {
	refs := v.Referrers()
	if refs == nil {
		return nil
	}
	var out []*ssa.FieldAddr
	for _, ref := range *refs {
		fa, ok := ref.(*ssa.FieldAddr)
		if !ok || fa.X != v {
			continue
		}
		if s := structOf(fa.X.Type()); s != nil && fa.Field < s.NumFields() && s.Field(fa.Field).Name() == field {
			out = append(out, fa)
		}
	}
	return out
}

func storedValues(addr ssa.Value) []ssa.Value {
	refs := addr.Referrers()
	if refs == nil {
		return nil
	}
	var out []ssa.Value
	for _, ref := range *refs {
		if st, ok := ref.(*ssa.Store); ok && st.Addr == addr {
			out = append(out, st.Val)
		}
	}
	return out
}

// elemValues returns the values stored into the elements of an array
// allocation (how slice literals and variadic arguments look in SSA).
func elemValues(alloc *ssa.Alloc) []ssa.Value {
	refs := alloc.Referrers()
	if refs == nil {
		return nil
	}
	var out []ssa.Value
	for _, ref := range *refs {
		if ia, ok := ref.(*ssa.IndexAddr); ok && ia.X == alloc {
			out = append(out, storedValues(ia)...)
		}
	}
	return out
}

func orUnknown(vals []string) []string {
	if len(vals) == 0 {
		return []string{model.Unknown}
	}
	return vals
}

func concat(a, b []string, limit int) []string {
	var out []string
	for _, x := range a {
		for _, y := range b {
			out = append(out, x+y)
			if len(out) >= limit {
				return out
			}
		}
	}
	return out
}

var stars = regexp.MustCompile(`\*{2,}`)

func normalize(vals []string, limit int) []string {
	set := map[string]bool{}
	var out []string
	for _, v := range vals {
		v = stars.ReplaceAllString(v, model.Unknown)
		if !set[v] {
			set[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// MethodStrings resolves the strings returned by the named method of the
// dynamic types v may hold, e.g. the Topic() of a library's message passed as a
// types.Data. Unlike plain returns, the method may live outside the module
// (message types usually come from shared libraries).
func (r *Resolver) MethodStrings(v ssa.Value, method string) []string {
	var out []string
	seen := map[types.Type]bool{}
	for _, t := range r.dynamicTypes(v, 0, map[ssa.Value]bool{}, r.base) {
		if seen[t] {
			continue
		}
		seen[t] = true
		if fn := r.methodOrElem(t, method); fn != nil {
			out = append(out, r.returns(fn, 0, []ssa.Value{v}, 0, map[ssa.Value]bool{}, r.base)...)
		}
	}
	return normalize(out, r.maxValues)
}

// MessageTypes labels the concrete types an interface value may hold, e.g.
// the message passed to a publisher as a types.Data.
func (r *Resolver) MessageTypes(v ssa.Value) []string {
	set := map[string]bool{}
	for _, t := range r.dynamicTypes(v, 0, map[ssa.Value]bool{}, r.base) {
		if !symbols.IsInterface(t) {
			set[symbols.TypeLabel(t)] = true
		}
	}
	var out []string
	for l := range set {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// dynamicTypes lists the concrete types an (interface) value may hold.
func (r *Resolver) dynamicTypes(v ssa.Value, depth int, seen map[ssa.Value]bool, fr *frame) []types.Type {
	if v == nil || depth > r.maxDepth || seen[v] {
		return nil
	}
	seen[v] = true
	switch x := v.(type) {
	case *ssa.MakeInterface:
		return []types.Type{x.X.Type()}
	case *ssa.ChangeType:
		return r.dynamicTypes(x.X, depth, seen, fr)
	case *ssa.ChangeInterface:
		return r.dynamicTypes(x.X, depth, seen, fr)
	case *ssa.Convert:
		return r.dynamicTypes(x.X, depth, seen, fr)
	case *ssa.Phi:
		var out []types.Type
		for _, e := range x.Edges {
			out = append(out, r.dynamicTypes(e, depth, seen, fr)...)
		}
		return out
	case *ssa.Parameter:
		if arg, parent, ok := fr.bound(x); ok {
			return r.dynamicTypes(arg, depth, seen, parent)
		}
		var out []types.Type
		fn := x.Parent()
		for i, p := range fn.Params {
			if p != x || r.reach == nil {
				continue
			}
			for _, e := range r.reach.Callers(fn) {
				if e.Site != nil {
					out = append(out, r.dynamicTypes(argFor(e.Site.Common(), fn, i), depth+1, seen, nil)...)
				}
			}
		}
		return out
	case *ssa.Call:
		return r.typesFromCall(x, 0, depth, seen, fr)
	case *ssa.Extract:
		if call, ok := x.Tuple.(*ssa.Call); ok {
			return r.typesFromCall(call, x.Index, depth, seen, fr)
		}
	case *ssa.UnOp:
		if x.Op == token.MUL {
			var vals []ssa.Value
			switch addr := x.X.(type) {
			case *ssa.FieldAddr:
				if k, ok := keyOf(addr.X.Type(), addr.Field); ok {
					vals = r.fields[k]
				}
			case *ssa.Global:
				vals = r.globals[addr]
			}
			var out []types.Type
			for _, val := range vals {
				out = append(out, r.dynamicTypes(val, depth+1, seen, nil)...)
			}
			return out
		}
	}
	if !symbols.IsInterface(v.Type()) {
		return []types.Type{v.Type()}
	}
	return nil
}

// typesFromCall lists the dynamic types returned (result index) by the
// functions a call may run: static, a func value bound in the frame (a
// callback the caller passed), or the call graph's callees.
func (r *Resolver) typesFromCall(call *ssa.Call, index, depth int, seen map[ssa.Value]bool, fr *frame) []types.Type {
	var callees []*ssa.Function
	args := call.Call.Args
	switch {
	case call.Call.StaticCallee() != nil:
		callees = []*ssa.Function{call.Call.StaticCallee()}
	case call.Call.IsInvoke():
	default:
		if p, ok := unwrap(call.Call.Value, false).(*ssa.Parameter); ok {
			if arg, _, ok := fr.bound(p); ok {
				callees = funcsOf(arg)
			}
		}
		if callees == nil && r.reach != nil {
			callees = r.reach.Callees(call)
		}
	}
	var out []types.Type
	for _, fn := range callees {
		if !r.followable(fn) {
			continue
		}
		inner := &frame{fn: fn, args: args, parent: fr}
		for _, b := range fn.Blocks {
			if len(b.Instrs) == 0 {
				continue
			}
			if ret, ok := b.Instrs[len(b.Instrs)-1].(*ssa.Return); ok && index < len(ret.Results) {
				out = append(out, r.dynamicTypes(ret.Results[index], depth+1, seen, inner)...)
			}
		}
	}
	return out
}

// funcsOf returns the function a func value denotes (a function or closure).
func funcsOf(v ssa.Value) []*ssa.Function {
	switch x := unwrap(v, false).(type) {
	case *ssa.Function:
		return []*ssa.Function{x}
	case *ssa.MakeClosure:
		if fn, ok := x.Fn.(*ssa.Function); ok {
			return []*ssa.Function{fn}
		}
	}
	return nil
}

// methodOrElem finds method on t, or on the element type behind pointers,
// slices and arrays (gorm's Find(&[]Model) names Model's table), trying the
// pointer to an element too for pointer-receiver methods.
func (r *Resolver) methodOrElem(t types.Type, method string) *ssa.Function {
	for i := 0; i < 4 && t != nil; i++ {
		if fn := r.method(t, nil, method); fn != nil {
			return fn
		}
		if _, isPtr := types.Unalias(t).(*types.Pointer); !isPtr && !types.IsInterface(t) {
			if fn := r.method(types.NewPointer(t), nil, method); fn != nil {
				return fn
			}
		}
		switch u := types.Unalias(t).Underlying().(type) {
		case *types.Pointer:
			t = u.Elem()
		case *types.Slice:
			t = u.Elem()
		case *types.Array:
			t = u.Elem()
		default:
			return nil
		}
	}
	return nil
}

// globalStores returns the values stored into a package-level variable: by
// reachable module code (the store index), or, for a library variable, by
// its package initializer (var DefaultDriverName = "mysql").
func (r *Resolver) globalStores(g *ssa.Global) []ssa.Value {
	if vals := r.globals[g]; len(vals) > 0 || g.Pkg == nil || r.modulePkgs[g.Pkg.Pkg.Path()] {
		return vals
	}
	inits, ok := r.libInits[g.Pkg]
	if !ok {
		inits = map[*ssa.Global][]ssa.Value{}
		if fn := g.Pkg.Func("init"); fn != nil {
			for _, b := range fn.Blocks {
				for _, instr := range b.Instrs {
					if st, ok := instr.(*ssa.Store); ok {
						if dst, ok := st.Addr.(*ssa.Global); ok {
							inits[dst] = append(inits[dst], st.Val)
						}
					}
				}
			}
		}
		r.libInits[g.Pkg] = inits
	}
	return inits[g]
}

// GRPCServiceName returns the proto-qualified name of a gRPC service
// ("notify.v1.NotifierService") from the ServiceDesc that generated code declares
// next to the client and server (<Svc>_ServiceDesc, or _<Svc>_serviceDesc
// from older generators), or "" when there is none.
func (r *Resolver) GRPCServiceName(pkgPath, service string) string {
	if r.prog == nil || pkgPath == "" || service == "" {
		return ""
	}
	pkg := r.prog.ImportedPackage(pkgPath)
	if pkg == nil {
		return ""
	}
	for _, name := range []string{service + "_ServiceDesc", "_" + service + "_serviceDesc"} {
		g, ok := pkg.Members[name].(*ssa.Global)
		if !ok {
			continue
		}
		init := pkg.Func("init")
		if init == nil {
			continue
		}
		for _, b := range init.Blocks {
			for _, instr := range b.Instrs {
				st, ok := instr.(*ssa.Store)
				if !ok {
					continue
				}
				var vals []string
				switch {
				case st.Addr == g: // *g = *t, t a composite literal built in place (x/tools ≥ v0.30)
					vals = r.fieldStrs(st.Val, "ServiceName", 0, map[ssa.Value]bool{}, r.base)
				default: // the literal's fields stored into g directly
					if fa, ok := st.Addr.(*ssa.FieldAddr); ok && fa.X == g && fieldName(fa.X.Type(), fa.Field) == "ServiceName" {
						vals = r.Strings(st.Val)
					}
				}
				if len(vals) == 1 {
					return vals[0]
				}
			}
		}
	}
	return ""
}

// Stores returns the values stored, anywhere in reachable module code, into
// what a value reads: a struct field (by owner type, through an address or
// of a struct value), a global, or a local variable's allocation.
func (r *Resolver) Stores(addr ssa.Value) []ssa.Value {
	switch a := addr.(type) {
	case *ssa.FieldAddr:
		if k, ok := keyOf(a.X.Type(), a.Field); ok {
			return r.fields[k]
		}
	case *ssa.Field:
		if k, ok := keyOf(a.X.Type(), a.Field); ok {
			return r.fields[k]
		}
	case *ssa.Global:
		return r.globalStores(a)
	case *ssa.Alloc: // a local spilled to the heap (captured by a closure)
		return storedValues(a)
	}
	return nil
}

// ArgFor is the argument a call site passes for parameter idx of callee.
func ArgFor(c *ssa.CallCommon, callee *ssa.Function, idx int) ssa.Value {
	return argFor(c, callee, idx)
}

// zeroField reports whether string field name of a composite literal is
// never set: no store to it, and the struct's address never escapes (to a
// call such as env.Parse(&cfg), or into another value), so it keeps its zero
// value "".
func zeroField(alloc *ssa.Alloc, name string) bool {
	s := structOf(alloc.Type())
	if s == nil {
		return false
	}
	isString := false
	for i := 0; i < s.NumFields(); i++ {
		if f := s.Field(i); f.Name() == name {
			b, ok := f.Type().Underlying().(*types.Basic)
			isString = ok && b.Info()&types.IsString != 0
		}
	}
	refs := alloc.Referrers()
	if !isString || refs == nil {
		return false
	}
	for _, ref := range *refs {
		switch u := ref.(type) {
		case *ssa.FieldAddr:
			if fieldName(u.X.Type(), u.Field) == name {
				return false // written or read through its address
			}
		case *ssa.UnOp:
			if u.Op != token.MUL {
				return false
			}
		case *ssa.DebugRef:
		default:
			return false
		}
	}
	return true
}
