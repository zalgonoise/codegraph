package transport

import (
	"go/token"
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/zalgonoise/codegraph/internal/model"
	"github.com/zalgonoise/codegraph/internal/resolve"
	"github.com/zalgonoise/codegraph/internal/symbols"
)

// Router prefixes. A route registered on a router value sits under the path
// prefix that router was scoped with (Rule.Scope). The prefix is found by
// walking the router value back: through scoping calls (gin Group, gorilla
// PathPrefix(...).Subrouter()), into the router a chi Route/Group callback is
// given, to where a router is mounted (chi Mount, including routers returned
// by a constructor), through parameters (callers), fields or globals
// (stores), and routers captured by closures: a captured variable lives in a
// heap cell, which the closure reads through a free variable and the code
// after it reads again to mount the router.

const maxPrefixDepth = 16

// prefixes returns the path prefixes of a router value ("" for a root router).
func (d *Detector) prefixes(router ssa.Value) []string {
	return orRoot(dedupe(d.prefixesOf(router, 0, map[ssa.Value]bool{})))
}

func (d *Detector) prefixesOf(v ssa.Value, depth int, seen map[ssa.Value]bool) []string {
	v = unwrapRouter(v)
	if v == nil || depth > maxPrefixDepth || seen[v] {
		return nil
	}
	seen[v] = true
	defer delete(seen, v)

	switch x := v.(type) {
	case *ssa.Call:
		if rule, args := d.scopeCall(x); rule != nil && len(args) > 0 {
			switch rule.Scope {
			case "returns":
				return joinAll(orRoot(d.prefixesOf(args[0], depth+1, seen)), d.scopePaths(rule, args))
			case "keeps":
				return d.prefixesOf(args[0], depth+1, seen)
			}
		}
	case *ssa.Parameter:
		if out := d.passed(x, depth, seen); len(out) > 0 {
			return out
		}
		fn := x.Parent()
		if !d.ModulePkgs[symbols.PkgPath(fn)] {
			return nil
		}
		var out []string
		for i, p := range fn.Params {
			if p != x {
				continue
			}
			for _, e := range d.Reach.Callers(fn) {
				if e.Site != nil {
					out = append(out, d.prefixesOf(resolve.ArgFor(e.Site.Common(), fn, i), depth+1, seen)...)
				}
			}
		}
		return out
	case *ssa.UnOp:
		if x.Op == token.MUL {
			var out []string
			for _, addr := range cells(x.X) {
				for _, val := range d.Resolver.Stores(addr) {
					out = append(out, d.prefixesOf(val, depth+1, seen)...)
				}
			}
			return out
		}
	case *ssa.Field:
		var out []string
		for _, val := range d.Resolver.Stores(x) {
			out = append(out, d.prefixesOf(val, depth+1, seen)...)
		}
		return out
	case *ssa.Phi:
		var out []string
		for _, e := range x.Edges {
			out = append(out, d.prefixesOf(e, depth+1, seen)...)
		}
		return out
	}
	return d.mounted(v, depth, seen)
}

// mounted finds where a router value is mounted under a path, following it
// out of the constructor that returns it, and through the cell it is stored
// in when a closure captures it.
func (d *Detector) mounted(v ssa.Value, depth int, seen map[ssa.Value]bool) []string {
	var out []string
	for _, use := range uses(v) {
		switch u := use.(type) {
		case *ssa.Store:
			if alloc, ok := u.Addr.(*ssa.Alloc); ok && unwrapRouter(u.Val) == v {
				for _, load := range loads(alloc) {
					if !seen[load] {
						seen[load] = true
						out = append(out, d.mounted(load, depth+1, seen)...)
						delete(seen, load)
					}
				}
			}
		case ssa.CallInstruction:
			rule, args := d.scopeCall(u)
			if rule != nil && rule.Scope == "mounts" && rule.ScopeArg < len(args) && unwrapRouter(args[rule.ScopeArg]) == v && len(args) > 0 {
				out = append(out, joinAll(orRoot(d.prefixesOf(args[0], depth+1, seen)), d.scopePaths(rule, args))...)
			}
		case *ssa.Return:
			for _, e := range d.Reach.Callers(u.Parent()) {
				if call, ok := e.Site.(*ssa.Call); ok && !seen[call] {
					seen[call] = true
					out = append(out, d.mounted(call, depth+1, seen)...)
					delete(seen, call)
				}
			}
		}
	}
	return out
}

// passed returns the prefixes of the router a chi Route/Group callback is
// given, when p is that callback's router parameter.
func (d *Detector) passed(p *ssa.Parameter, depth int, seen map[ssa.Value]bool) []string {
	fn := p.Parent()
	if len(fn.Params) == 0 || fn.Params[0] != p {
		return nil
	}
	var out []string
	for _, use := range d.passUses()[fn] {
		out = append(out, joinAll(orRoot(d.prefixesOf(use.args[0], depth+1, seen)), d.scopePaths(use.rule, use.args))...)
	}
	return out
}

type passUse struct {
	rule *Rule
	args []ssa.Value
}

// passUses indexes the functions handed to "passes" scope calls (chi Route,
// Group) in the scanned module code.
func (d *Detector) passUses() map[*ssa.Function][]passUse {
	if d.passIndex != nil {
		return d.passIndex
	}
	d.passIndex = map[*ssa.Function][]passUse{}
	for _, fn := range d.funcs {
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				site, ok := instr.(ssa.CallInstruction)
				if !ok {
					continue
				}
				rule, args := d.scopeCall(site)
				if rule == nil || rule.Scope != "passes" || rule.ScopeArg >= len(args) || len(args) == 0 {
					continue
				}
				var target *ssa.Function
				switch a := unwrapRouter(args[rule.ScopeArg]).(type) {
				case *ssa.MakeClosure:
					target, _ = a.Fn.(*ssa.Function)
				case *ssa.Function:
					target = a
				}
				if target != nil {
					d.passIndex[target] = append(d.passIndex[target], passUse{rule, args})
				}
			}
		}
	}
	return d.passIndex
}

// scopeCall returns the scope rule a call matches, with its arguments.
func (d *Detector) scopeCall(site ssa.CallInstruction) (*Rule, []ssa.Value) {
	for _, c := range d.candidates(site) {
		for _, rule := range d.scopeRules() {
			if _, ok := rule.match(c); ok {
				return rule, resolve.Args(site.Common())
			}
		}
	}
	return nil, nil
}

func (d *Detector) scopeRules() []*Rule {
	if d.scopes == nil {
		d.scopes = []*Rule{}
		for i := range d.Rules {
			if d.Rules[i].Scope != "" {
				d.scopes = append(d.scopes, &d.Rules[i])
			}
		}
	}
	return d.scopes
}

func (d *Detector) scopePaths(rule *Rule, args []ssa.Value) []string {
	if len(rule.KeyArgs) == 0 || rule.KeyArgs[0] >= len(args) {
		return []string{""}
	}
	return d.Resolver.StringsOrUnknown(args[rule.KeyArgs[0]])
}

func unwrapRouter(v ssa.Value) ssa.Value {
	for {
		switch x := v.(type) {
		case *ssa.MakeInterface:
			v = x.X
		case *ssa.ChangeType:
			v = x.X
		case *ssa.ChangeInterface: // chi.Router passed as an http.Handler
			v = x.X
		default:
			return v
		}
	}
}

// uses lists the instructions using v, looking through interface conversions.
func uses(v ssa.Value) []ssa.Instruction {
	var out []ssa.Instruction
	refs := v.Referrers()
	if refs == nil {
		return nil
	}
	for _, ref := range *refs {
		switch r := ref.(type) {
		case *ssa.MakeInterface:
			out = append(out, uses(r)...)
		case *ssa.ChangeType:
			out = append(out, uses(r)...)
		case *ssa.ChangeInterface:
			out = append(out, uses(r)...)
		default:
			out = append(out, ref)
		}
	}
	return out
}

// cells returns the addresses a pointer stands for: a closure's free variable
// stands for the cells its MakeClosure instructions bind to it.
func cells(addr ssa.Value) []ssa.Value {
	fv, ok := addr.(*ssa.FreeVar)
	if !ok {
		return []ssa.Value{addr}
	}
	var out []ssa.Value
	for _, b := range resolve.FreeVarBindings(fv) {
		out = append(out, cells(b)...)
	}
	return out
}

// loads lists the reads of a cell: in its function, and in the closures it is
// bound to (through their free variables).
func loads(addr ssa.Value) []ssa.Value {
	refs := addr.Referrers()
	if refs == nil {
		return nil
	}
	var out []ssa.Value
	for _, ref := range *refs {
		switch r := ref.(type) {
		case *ssa.UnOp:
			if r.Op == token.MUL && r.X == addr {
				out = append(out, r)
			}
		case *ssa.MakeClosure:
			fn, ok := r.Fn.(*ssa.Function)
			if !ok {
				continue
			}
			for i, b := range r.Bindings {
				if b == addr && i < len(fn.FreeVars) {
					out = append(out, loads(fn.FreeVars[i])...)
				}
			}
		}
	}
	return out
}

func orRoot(prefixes []string) []string {
	if len(prefixes) == 0 {
		return []string{""}
	}
	return prefixes
}

func joinAll(prefixes, paths []string) []string {
	var out []string
	for _, p := range prefixes {
		for _, q := range paths {
			out = append(out, joinPath(p, q))
		}
	}
	return out
}

// joinPath places a route path under a prefix ("/api" + "/x" = "/api/x").
func joinPath(prefix, path string) string {
	switch {
	case prefix == "" || prefix == "/":
		return path
	case path == "":
		return prefix
	case prefix == model.Unknown && strings.HasPrefix(path, model.Unknown):
		return path
	}
	return strings.TrimSuffix(prefix, "/") + "/" + strings.TrimPrefix(path, "/")
}
