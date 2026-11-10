package transport

import (
	"go/token"
	"go/types"
	"regexp"
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/zalgonoise/codegraph/internal/model"
	"github.com/zalgonoise/codegraph/internal/reach"
	"github.com/zalgonoise/codegraph/internal/resolve"
	"github.com/zalgonoise/codegraph/internal/symbols"
)

// Detector finds endpoints in reachable module code.
type Detector struct {
	Rules      []Rule
	Resolver   *resolve.Resolver
	Reach      *reach.Result
	Fset       *token.FileSet
	Root       string
	ModulePkgs map[string]bool

	funcs     []*ssa.Function
	scopes    []*Rule
	passIndex map[*ssa.Function][]passUse
}

// Detection is one endpoint occurrence before merging, with the facts that
// merging drops: the function holding the call site and, for inbound
// endpoints, the functions that handle it (the roots of its journey).
type Detection struct {
	Endpoint model.Endpoint
	Func     *ssa.Function       // nil for file-level directives
	Site     ssa.CallInstruction // nil for directives
	Roots    []resolve.Root

	// Callers is set on the per-caller variants of an outbound endpoint whose
	// keys come from its function's parameters (a shared publish(ctx, d)
	// helper): the call sites into Func this variant's keys are resolved for.
	Callers []ssa.CallInstruction
}

// Detect scans every call site of funcs (reachable module functions only, so
// dead code and test helpers never produce endpoints).
func (d *Detector) Detect(funcs []*ssa.Function) []Detection {
	d.funcs = funcs
	var out []Detection
	for _, fn := range funcs {
		if fn.Synthetic != "" { // wrappers would duplicate the real call sites
			continue
		}
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				site, ok := instr.(ssa.CallInstruction)
				if !ok {
					continue
				}
				out = append(out, d.detect(fn, site)...)
			}
		}
	}
	return d.dropCovered(out)
}

// dropCovered removes the keys of fallback detections (a consumer's whole
// queue list) that another inbound detection of the same transport registers
// (a mux's handler for that queue), and fallbacks left without keys. Only a
// queue nothing else registers keeps the consumer's own handler as its root.
func (d *Detector) dropCovered(dets []Detection) []Detection {
	fallback := map[string]bool{}
	for _, r := range d.Rules {
		if r.Fallback {
			fallback[r.Name] = true
		}
	}
	if len(fallback) == 0 {
		return dets
	}
	registered := map[string]bool{}
	for _, det := range dets {
		if ep := det.Endpoint; ep.Direction == model.Inbound && !fallback[ep.Rule] {
			for _, k := range ep.Keys {
				registered[ep.Transport+" "+k] = true
			}
		}
	}
	out := dets[:0]
	for _, det := range dets {
		if ep := det.Endpoint; fallback[ep.Rule] {
			var keys []string
			for _, k := range ep.Keys {
				if !registered[ep.Transport+" "+k] {
					keys = append(keys, k)
				}
			}
			if len(keys) == 0 {
				continue
			}
			det.Endpoint.Keys = keys
		}
		out = append(out, det)
	}
	return out
}

// detect returns the endpoint of a call site (first matching rule), split
// into per-caller variants when its keys depend on who calls its function.
func (d *Detector) detect(fn *ssa.Function, site ssa.CallInstruction) []Detection {
	for _, c := range d.candidates(site) {
		for i := range d.Rules {
			rule := &d.Rules[i]
			if rule.Scope != "" {
				continue // router prefixes, no endpoint
			}
			capture, ok := rule.match(c)
			if !ok {
				continue
			}
			det := d.endpoint(rule, c, capture, fn, site)
			dets := []Detection{det}
			if rule.MapEntries != nil || rule.Elements != nil {
				dets = d.entries(rule, det, resolve.Args(site.Common()))
			}
			if rule.Direction == model.Outbound && rule.MapEntries == nil && rule.Elements == nil {
				if vs := d.variants(rule, c, capture, fn, site, det); len(vs) > 0 {
					dets = vs
				}
			}
			if rule.Access == "statement" {
				dets = d.byStatement(rule, site, dets)
			}
			var out []Detection
			for _, x := range dets {
				if rule.SkipUnknown && len(x.Endpoint.Keys) == 1 && x.Endpoint.Keys[0] == model.Unknown {
					continue
				}
				if rule.RequireHandler && len(x.Roots) == 0 {
					continue
				}
				out = append(out, x)
			}
			return out
		}
	}
	return nil
}

// maxVariantCallers bounds the callers a helper's keys are resolved for.
const maxVariantCallers = 16

// variants resolves an outbound site's keys and message once per module
// caller of its function, with the function's parameters bound to that
// caller's arguments. When callers disagree, each distinct result becomes a
// detection of its own, remembering the callers it was resolved for, so a
// journey that reaches the helper through one caller only gets that caller's
// endpoint. Otherwise there are no variants and the merged endpoint stays.
func (d *Detector) variants(rule *Rule, c Callee, capture string, fn *ssa.Function, site ssa.CallInstruction, base Detection) []Detection {
	if len(fn.Params) == 0 {
		return nil
	}
	var edges []ssa.CallInstruction
	for _, e := range d.Reach.Callers(fn) {
		if e.Site != nil && d.ModulePkgs[symbols.PkgPath(e.Site.Parent())] {
			edges = append(edges, e.Site)
		}
	}
	if len(edges) < 2 || len(edges) > maxVariantCallers {
		return nil
	}
	args := resolve.Args(site.Common())
	type variant struct {
		keys    []string
		message string
		callers []ssa.CallInstruction
		sites   []model.Site
	}
	var order []string
	groups := map[string]*variant{}
	for _, call := range edges {
		cargs := make([]ssa.Value, len(fn.Params))
		for i := range fn.Params {
			cargs[i] = resolve.ArgFor(call.Common(), fn, i)
		}
		res := d.Resolver.InCall(fn, cargs)
		keys := d.keys(res, rule, c, capture, args)
		msg := d.message(res, rule, args)
		id := strings.Join(keys, ",") + "|" + msg
		g := groups[id]
		if g == nil {
			g = &variant{keys: keys, message: msg}
			groups[id] = g
			order = append(order, id)
		}
		g.callers = append(g.callers, call)
		g.sites = append(g.sites, model.Site{
			Function: base.Endpoint.Function, Position: base.Endpoint.Position, Via: symbols.FuncLabel(call.Parent()),
		})
	}
	if len(groups) < 2 {
		return nil
	}
	out := make([]Detection, 0, len(order))
	for _, id := range order {
		g := groups[id]
		v := base
		v.Endpoint.Keys, v.Endpoint.Message, v.Endpoint.Sites = g.keys, g.message, g.sites
		if v.Endpoint.Message == "" {
			v.Endpoint.Message = base.Endpoint.Message
		}
		v.Callers = g.callers
		out = append(out, v)
	}
	return out
}

// candidates lists what a call site may invoke: the interface method itself
// for dynamic calls (so rules can target interfaces such as sarama.SyncProducer
// or an in-house Publisher), plus the concrete callees from the call graph.
func (d *Detector) candidates(site ssa.CallInstruction) []Callee {
	c := site.Common()
	var out []Callee
	switch {
	case c.IsInvoke():
		pkg := ""
		if c.Method.Pkg() != nil {
			pkg = c.Method.Pkg().Path()
		}
		if p := symbols.TypePkg(c.Value.Type()); p != "" {
			pkg = p
		}
		out = append(out, Callee{
			Pkg:  pkg,
			Recv: symbols.TypeName(c.Value.Type()),
			Name: c.Method.Name(),
			Sig:  types.TypeString(c.Method.Type(), nil),
		})
	case c.StaticCallee() != nil:
		return []Callee{calleeOf(c.StaticCallee())}
	}
	for _, fn := range d.Reach.Callees(site) {
		out = append(out, calleeOf(fn))
	}
	return out
}

func calleeOf(fn *ssa.Function) Callee {
	o := symbols.Origin(fn)
	return Callee{
		Pkg:  symbols.PkgPath(o),
		Recv: symbols.RecvName(o),
		Name: o.Name(),
		Sig:  types.TypeString(o.Signature, nil),
	}
}

func (d *Detector) endpoint(rule *Rule, c Callee, capture string, fn *ssa.Function, site ssa.CallInstruction) Detection {
	args := resolve.Args(site.Common())
	keys := d.keys(d.Resolver, rule, c, capture, args)

	ep := model.Endpoint{
		Transport: rule.Transport,
		Role:      rule.Role,
		Direction: rule.Direction,
		Kind:      rule.Kind,
		Keys:      keys,
		Component: symbols.PkgPath(fn),
		Function:  symbols.FuncLabel(fn),
		Position:  d.pos(site, fn),
		Source:    "detector",
		Rule:      rule.Name,
		Count:     1,
	}
	ep.Sites = []model.Site{{Function: ep.Function, Position: ep.Position}}
	ep.Access = d.access(rule, c, site)
	if rule.Transport == "grpc" && keys[0] != model.Unknown {
		svc, _, _ := strings.Cut(keys[0], "/")
		ep.Contract = d.Resolver.GRPCServiceName(c.Pkg, svc)
	}
	ep.Message = d.message(d.Resolver, rule, args)
	det := Detection{Endpoint: ep, Func: fn, Site: site}
	d.attach(rule, &det, d.handlers(rule, args))
	return det
}

// entries splits a call registering several endpoints at once into one
// detection per item, each with its own keys and handler: the key/value pairs
// of a map argument (Rule.MapEntries), or the elements of a slice argument,
// whose key and handler fields are read on each element (Rule.Elements).
// Without resolvable items, the call's own detection stays.
func (d *Detector) entries(rule *Rule, base Detection, args []ssa.Value) []Detection {
	type item struct {
		keys     []string
		handlers []ssa.Value
		pos      token.Pos
	}
	var items []item
	switch {
	case rule.MapEntries != nil && rule.MapEntries.Arg < len(args):
		for _, e := range d.Resolver.MapEntries(args[rule.MapEntries.Arg]) {
			items = append(items, item{d.Resolver.StringsOrUnknown(e.Key), []ssa.Value{e.Value}, e.Pos})
		}
	case rule.Elements != nil && rule.Elements.Arg < len(args):
		ref := rule.Elements
		slices := []ssa.Value{args[ref.Arg]}
		if ref.Field != "" {
			slices = d.Resolver.FieldValues(args[ref.Arg], ref.Field)
		}
		for _, sl := range slices {
			for _, el := range d.Resolver.Elements(sl) {
				var it item
				for _, f := range rule.KeyFields {
					if f.Arg == ref.Arg {
						it.keys = append(it.keys, d.Resolver.FieldStrings(el, f.Field)...)
					}
				}
				for _, f := range rule.HandlerFields {
					if f.Arg == ref.Arg {
						it.handlers = append(it.handlers, d.Resolver.FieldValues(el, f.Field)...)
					}
				}
				it.pos = el.Pos()
				items = append(items, it)
			}
		}
	}
	var out []Detection
	for _, it := range items {
		det := base
		keys := it.keys
		if rule.Transform == "first-word" {
			keys = firstWords(keys)
		}
		if keys = dedupe(keys); len(keys) == 0 {
			keys = []string{model.Unknown}
		}
		det.Endpoint.Keys = keys
		det.Endpoint.Handler, det.Roots = "", nil
		det.Endpoint.Component = symbols.PkgPath(base.Func)
		if it.pos.IsValid() { // the item's own line, rather than the call's
			det.Endpoint.Position = symbols.Pos(d.Fset, it.pos, d.Root)
			det.Endpoint.Sites = []model.Site{{Function: det.Endpoint.Function, Position: det.Endpoint.Position}}
		}
		d.attach(rule, &det, it.handlers)
		out = append(out, det)
	}
	if len(out) == 0 {
		return []Detection{base}
	}
	return out
}

// firstWords keeps the first word of each key: a cobra command's Use
// ("serve [flags]") names it.
func firstWords(keys []string) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if f := strings.Fields(k); len(f) > 0 {
			out = append(out, f[0])
		}
	}
	return out
}

// attach names an endpoint's handler from the values a call registers as
// handlers and, for inbound endpoints, finds its journey roots.
func (d *Detector) attach(rule *Rule, det *Detection, handlers []ssa.Value) {
	ep := &det.Endpoint
	for _, h := range handlers {
		if label, pkg := d.Resolver.Handler(h); label != "" {
			ep.Handler = label
			// Inbound traffic lands where the handler lives, not where it was registered.
			if rule.Direction == model.Inbound && d.ModulePkgs[pkg] {
				ep.Component = pkg
			}
			break
		}
	}
	fn := det.Func
	if rule.Direction == model.Inbound {
		for _, h := range handlers {
			if det.Roots = d.Resolver.HandlerFuncs(h, rule.HandlerMethods); len(det.Roots) > 0 {
				break
			}
		}
		// A single wrapped handler (mw.Wrap(h.Handle)) names the endpoint and
		// owns it, rather than the middleware factory; so does the method a
		// library adapter runs (queue.Adapt(h): (*H).Work).
		if len(det.Roots) == 1 && strings.HasSuffix(ep.Handler, "(…)") {
			if inner := det.Roots[0].Innermost(); inner != det.Roots[0].Func || inner.Parent() == nil {
				det.Endpoint.Handler = strings.TrimSuffix(ep.Handler, "…)") + symbols.FuncLabel(inner) + ")"
				if pkg := symbols.PkgPath(inner); d.ModulePkgs[pkg] {
					det.Endpoint.Component = pkg
				}
			}
		}
		if len(det.Roots) == 0 && rule.CallerIsHandler {
			det.Roots = []resolve.Root{{Func: fn}}
			if ep.Handler == "" {
				ep.Handler = ep.Function
			}
		}
	}
}

// handlers lists the values a call registers as handlers: the rule's handler
// arguments, then the handler fields of its config arguments.
func (d *Detector) handlers(rule *Rule, args []ssa.Value) []ssa.Value {
	var out []ssa.Value
	for _, i := range rule.HandlerArgs {
		if i < len(args) {
			out = append(out, args[i])
		}
	}
	for _, f := range rule.HandlerFields {
		if f.Arg < len(args) {
			out = append(out, d.Resolver.FieldValues(args[f.Arg], f.Field)...)
		}
	}
	return out
}

// keys extracts an endpoint's keys from a call's arguments with res (which
// may be bound to one caller of the enclosing function).
func (d *Detector) keys(res *resolve.Resolver, rule *Rule, c Callee, capture string, args []ssa.Value) []string {
	var keys []string
	if len(rule.KeyArgs) > 0 {
		parts := []string{""}
		for n, i := range rule.KeyArgs {
			vals := []string{model.Unknown}
			switch {
			case i >= len(args):
			case rule.Transform == "grpc-gateway" && n == len(rule.KeyArgs)-1: // a runtime.Pattern
				vals = d.gatewayPaths(args[i])
			default:
				vals = res.StringsOrUnknown(args[i])
			}
			if n == 0 && rule.KeyDefault != nil && len(args) > 0 {
				vals = d.withCtorDefault(res, rule, args[0], vals)
			}
			var next []string
			for _, p := range parts {
				for _, v := range vals {
					if n == 0 || p == "" { // RabbitMQ's default exchange is "": the key is the queue
						next = append(next, v)
					} else {
						next = append(next, p+" "+v)
					}
				}
			}
			parts = next
		}
		if rule.Prefixed && len(args) > 0 {
			if ps := d.prefixes(args[0]); len(ps) > 1 || ps[0] != "" {
				parts = joinAll(ps, parts)
			}
		}
		keys = append(keys, parts...)
	}
	for _, f := range rule.KeyFields {
		if f.Arg < len(args) {
			keys = append(keys, res.FieldStrings(args[f.Arg], f.Field)...)
		}
	}
	for _, m := range rule.KeyMethods {
		if m.Arg < len(args) {
			keys = append(keys, res.MethodStrings(args[m.Arg], m.Method)...)
		}
	}
	if capture != "" {
		keys = append(keys, capture)
	}
	if rule.StaticKey != "" {
		keys = append(keys, rule.StaticKey)
	}
	if method := strings.ToUpper(c.Name); rule.MethodFromFunc && httpMethods[method] {
		for i, k := range keys {
			keys[i] = method + " " + k
		}
	}
	switch rule.Transform {
	case "sql-table":
		keys = sqlTables(keys)
	case "table-name":
		keys = tableNames(keys)
	case "grpc-method":
		keys = []string{strings.TrimSuffix(c.Recv, "Client") + "/" + c.Name}
	case "iris-path":
		keys = irisPaths(keys)
	case "first-word":
		keys = firstWords(keys)
	}
	keys = dedupe(keys)
	if len(keys) == 0 {
		keys = []string{model.Unknown}
	}
	return keys
}

// message names the message type of a call: the static type of a message
// argument, or its only dynamic type when it is passed as an interface.
func (d *Detector) message(res *resolve.Resolver, rule *Rule, args []ssa.Value) string {
	for _, i := range rule.MsgArgs {
		if i >= len(args) {
			continue
		}
		if m := resolve.MessageType(args[i]); m != "" {
			return m
		}
		if ts := res.MessageTypes(args[i]); len(ts) == 1 {
			return ts[0]
		}
	}
	return ""
}

// access is what a store call does with its keys: the rule's, the called
// function's (AccessFuncs), or, with Chain, that of the calls made on its
// result. A "statement" rule's access is set table by table (byStatement).
func (d *Detector) access(rule *Rule, c Callee, site ssa.CallInstruction) string {
	if a, ok := rule.AccessFuncs[c.Name]; ok {
		return a
	}
	if rule.Chain {
		if v := site.Value(); v != nil {
			return model.Accesses(d.chainAccess(v, rule.AccessFuncs, map[ssa.Value]bool{}, 0)...)
		}
	}
	if rule.Access == "statement" {
		return ""
	}
	return rule.Access
}

// maxChainDepth bounds the helpers a builder is followed into.
const maxChainDepth = 3

// chainAccess follows a builder's result (gorm's Model(&x), Table(name))
// through the calls made on it, collecting the access of each call funcs
// names. Calls it does not name are followed when they return the builder
// again (Where, Order, Session), as are phis, the functions the builder is
// passed to (a scope helper), into their parameter and, when they return the
// builder's type, their result, and the callers of a function returning it
// (a query-building helper). A builder passed as a value (converted to an
// interface: Where("id IN (?)", subquery)) is a subquery: it is read.
func (d *Detector) chainAccess(v ssa.Value, funcs map[string]string, seen map[ssa.Value]bool, depth int) []string {
	if v == nil || seen[v] || depth > maxChainDepth {
		return nil
	}
	seen[v] = true
	refs := v.Referrers()
	if refs == nil {
		return nil
	}
	var out []string
	for _, ref := range *refs {
		switch x := ref.(type) {
		case *ssa.Phi:
			out = append(out, d.chainAccess(x, funcs, seen, depth)...)
		case *ssa.MakeInterface:
			out = append(out, model.Read)
		case *ssa.Return:
			if depth >= maxChainDepth || d.Reach == nil {
				continue
			}
			for i, r := range x.Results {
				if r != v {
					continue
				}
				for _, e := range d.Reach.Callers(x.Parent()) {
					if e.Site != nil {
						out = append(out, d.chainAccess(result(e.Site, i, len(x.Results)), funcs, seen, depth+1)...)
					}
				}
			}
		case ssa.CallInstruction:
			c := x.Common()
			callee := c.StaticCallee()
			name, onV := "", false
			switch {
			case c.IsInvoke():
				name, onV = c.Method.Name(), c.Value == v
			case callee != nil && callee.Signature.Recv() != nil:
				name, onV = callee.Name(), len(c.Args) > 0 && c.Args[0] == v
			}
			call, _ := x.(*ssa.Call)
			sameType := call != nil && types.Identical(call.Type(), v.Type())
			if onV {
				if a, ok := funcs[name]; ok {
					out = append(out, a)
				} else if sameType {
					out = append(out, d.chainAccess(call, funcs, seen, depth)...)
				}
				continue
			}
			if callee == nil || len(callee.Blocks) == 0 {
				continue
			}
			for i, a := range c.Args {
				if a == v && i < len(callee.Params) {
					out = append(out, d.chainAccess(callee.Params[i], funcs, seen, depth+1)...)
				}
			}
			if sameType {
				out = append(out, d.chainAccess(call, funcs, seen, depth+1)...)
			}
		}
	}
	return out
}

// result is the value a call site gives for result i of n: the call itself
// for a single result, else the Extract of that index (nil when unused).
func result(site ssa.CallInstruction, i, n int) ssa.Value {
	call, ok := site.(*ssa.Call)
	if !ok {
		return nil
	}
	if n == 1 {
		return call
	}
	if refs := call.Referrers(); refs != nil {
		for _, r := range *refs {
			if e, ok := r.(*ssa.Extract); ok && e.Index == i {
				return e
			}
		}
	}
	return nil
}

// byStatement sets the access of a "statement" rule's detections table by
// table, from the SQL of its key arguments: a detection whose tables are
// accessed differently (INSERT INTO a SELECT … FROM b) becomes one per access.
func (d *Detector) byStatement(rule *Rule, site ssa.CallInstruction, dets []Detection) []Detection {
	args := resolve.Args(site.Common())
	var stmts []string
	for _, i := range rule.KeyArgs {
		if i < len(args) {
			stmts = append(stmts, d.Resolver.StringsOrUnknown(args[i])...)
		}
	}
	tables := sqlAccess(stmts)
	var out []Detection
	for _, det := range dets {
		var order []string
		byAccess := map[string][]string{}
		for _, k := range det.Endpoint.Keys {
			a := tables[strings.ToLower(k)]
			if _, ok := byAccess[a]; !ok {
				order = append(order, a)
			}
			byAccess[a] = append(byAccess[a], k)
		}
		for _, a := range order {
			x := det
			x.Endpoint.Keys, x.Endpoint.Access = byAccess[a], a
			out = append(out, x)
		}
	}
	return out
}

func (d *Detector) pos(site ssa.CallInstruction, fn *ssa.Function) string {
	p := site.Pos()
	if !p.IsValid() {
		p = fn.Pos()
	}
	return symbols.Pos(d.Fset, p, d.Root)
}

var httpMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "HEAD": true, "OPTIONS": true,
}

// sqlTableRe matches a table after the keyword that introduces it.
var sqlTableRe = regexp.MustCompile("(?i)\\b(from|into|update|join)\\s+[`\"]?([a-zA-Z_][\\w.]*)")

// sqlNotTable matches what comes before an UPDATE that names no table: a
// locking read (SELECT … FOR UPDATE SKIP LOCKED) and an upsert's assignments
// (ON DUPLICATE KEY UPDATE status = …).
var sqlNotTable = regexp.MustCompile(`(?i)\b(for|key)\s+$`)

// sqlTableRefs returns the tables a statement names, lower-cased, each with
// the keyword before it: from, into, update or join.
func sqlTableRefs(stmt string) [][2]string {
	var out [][2]string
	for _, m := range sqlTableRe.FindAllStringSubmatchIndex(stmt, -1) {
		kw, table := strings.ToLower(stmt[m[2]:m[3]]), strings.ToLower(stmt[m[4]:m[5]])
		if kw == "update" && sqlNotTable.MatchString(stmt[:m[0]]) {
			continue
		}
		out = append(out, [2]string{kw, table})
	}
	return out
}

// sqlTables turns SQL statements into the tables they touch.
func sqlTables(keys []string) []string {
	var out []string
	for _, k := range keys {
		refs := sqlTableRefs(k)
		if len(refs) == 0 {
			out = append(out, model.Unknown)
			continue
		}
		for _, r := range refs {
			out = append(out, r[1])
		}
	}
	return out
}

var (
	sqlVerbRe = regexp.MustCompile(`^[\s(]*([a-zA-Z]+)`)
	sqlDMLRe  = regexp.MustCompile(`(?i)\b(insert|replace|update|delete|merge)\b`)
	// sqlTargets is the keyword before the table each writing verb changes.
	sqlTargets = map[string]string{"insert": "into", "replace": "into", "merge": "into", "update": "update", "delete": "from"}
)

// sqlAccess tells, for each table of some statements, whether they read it,
// write it or both. A statement writes the table its verb targets (INSERT
// INTO, REPLACE INTO, MERGE INTO, UPDATE, the first DELETE FROM) and reads
// the others; a SELECT reads all; a WITH takes the verb of the statement
// after its common table expressions. A statement whose verb is unknown, or
// unresolved, says nothing.
func sqlAccess(stmts []string) map[string]string {
	all := map[string][]string{}
	for _, s := range stmts {
		verb := ""
		if m := sqlVerbRe.FindStringSubmatch(s); m != nil {
			verb = strings.ToLower(m[1])
		}
		if verb == "with" {
			verb = "select"
			if m := sqlDMLRe.FindStringSubmatch(s); m != nil {
				verb = strings.ToLower(m[1])
			}
		}
		target := sqlTargets[verb]
		reads := verb == "select" || verb == "show" || verb == "describe" || verb == "explain"
		for i, r := range sqlTableRefs(s) {
			a := ""
			switch {
			case reads:
				a = model.Read
			case target == "":
			case r[0] == target && (verb != "delete" || i == 0):
				a = model.Write
			default:
				a = model.Read
			}
			all[r[1]] = append(all[r[1]], a)
		}
	}
	out := make(map[string]string, len(all))
	for t, as := range all {
		out[t] = model.Accesses(as...)
	}
	return out
}

// tableNames keeps the table of a table expression, dropping its alias and
// quotes: gorm's Table("daily_balance_snapshots AS s"). A subquery
// (Table("(?) as t", q)) names no table.
func tableNames(keys []string) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if f := strings.Fields(k); len(f) > 0 {
			k = strings.Trim(f[0], "`\"")
		}
		if strings.HasPrefix(k, "(") {
			k = model.Unknown
		}
		out = append(out, k)
	}
	return out
}

// typedParamRe matches a route parameter with a type and macros, as iris
// writes them: {id:string uuid() else 400}, {file:path}.
var typedParamRe = regexp.MustCompile(`\{(\w+):(\w+)[^{}]*\}`)

// irisPaths writes iris routes the way they are matched, so they link like
// any other: a typed parameter loses its macros ({id:string uuid() else 400}
// is {id}), a path parameter takes the rest of the path ({file...}), and a
// trailing slash is dropped, as iris does (/carts/ is /carts).
func irisPaths(keys []string) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		k = typedParamRe.ReplaceAllStringFunc(k, func(p string) string {
			m := typedParamRe.FindStringSubmatch(p)
			if m[2] == "path" {
				return "{" + m[1] + "...}"
			}
			return "{" + m[1] + "}"
		})
		if strings.HasSuffix(k, "/") && !strings.HasSuffix(k, " /") && k != "/" {
			k = strings.TrimSuffix(k, "/")
		}
		out = append(out, k)
	}
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
