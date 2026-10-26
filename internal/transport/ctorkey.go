package transport

import (
	"go/token"

	"golang.org/x/tools/go/ssa"

	"github.com/zalgonoise/codegraph/internal/resolve"
	"github.com/zalgonoise/codegraph/internal/symbols"
)

// withCtorDefault replaces empty key values ("" means "the configured
// destination") with the field of the configuration the receiver was
// constructed with (Rule.KeyDefault).
func (d *Detector) withCtorDefault(res *resolve.Resolver, rule *Rule, recv ssa.Value, vals []string) []string {
	empty := false
	var out []string
	for _, v := range vals {
		if v == "" {
			empty = true
		} else {
			out = append(out, v)
		}
	}
	if !empty {
		return vals
	}
	for _, call := range d.ctorCalls(rule, recv, 0, map[ssa.Value]bool{}) {
		if args := resolve.Args(call); rule.KeyDefault.Arg < len(args) {
			out = append(out, res.FieldStrings(args[rule.KeyDefault.Arg], rule.KeyDefault.Field)...)
		}
	}
	return dedupe(out)
}

// ctorCalls walks a client value back to the constructor calls that created
// it: through fields and globals (stores), parameters (callers), returns of
// module functions and phi nodes.
func (d *Detector) ctorCalls(rule *Rule, v ssa.Value, depth int, seen map[ssa.Value]bool) []*ssa.CallCommon {
	v = unwrapRouter(v)
	if v == nil || depth > maxPrefixDepth || seen[v] {
		return nil
	}
	seen[v] = true
	var out []*ssa.CallCommon
	switch x := v.(type) {
	case *ssa.Extract:
		return d.ctorCalls(rule, x.Tuple, depth, seen)
	case *ssa.Call:
		fn := x.Call.StaticCallee()
		if fn == nil {
			return nil
		}
		if contains(rule.KeyDefault.Funcs, fn.Name()) && pkgMatch(rule.Pkgs, symbols.PkgPath(fn)) {
			return []*ssa.CallCommon{&x.Call}
		}
		if d.ModulePkgs[symbols.PkgPath(fn)] { // a module constructor wrapping the library one
			for _, b := range fn.Blocks {
				if ret, ok := b.Instrs[len(b.Instrs)-1].(*ssa.Return); ok && len(ret.Results) > 0 {
					out = append(out, d.ctorCalls(rule, ret.Results[0], depth+1, seen)...)
				}
			}
		}
	case *ssa.UnOp:
		if x.Op != token.MUL {
			break
		}
		if fv, ok := x.X.(*ssa.FreeVar); ok { // captured by reference: what is stored into it
			for _, b := range resolve.FreeVarBindings(fv) {
				for _, val := range d.Resolver.Stores(b) {
					out = append(out, d.ctorCalls(rule, val, depth+1, seen)...)
				}
			}
			break
		}
		for _, val := range d.Resolver.Stores(x.X) {
			out = append(out, d.ctorCalls(rule, val, depth+1, seen)...)
		}
	case *ssa.FreeVar: // captured by value
		for _, b := range resolve.FreeVarBindings(x) {
			out = append(out, d.ctorCalls(rule, b, depth+1, seen)...)
		}
	case *ssa.Field: // cfg.Repository of a config struct passed by value
		for _, val := range d.Resolver.Stores(x) {
			out = append(out, d.ctorCalls(rule, val, depth+1, seen)...)
		}
	case *ssa.Phi:
		for _, e := range x.Edges {
			out = append(out, d.ctorCalls(rule, e, depth+1, seen)...)
		}
	case *ssa.Parameter:
		fn := x.Parent()
		for i, p := range fn.Params {
			if p != x {
				continue
			}
			for _, e := range d.Reach.Callers(fn) {
				if e.Site != nil {
					out = append(out, d.ctorCalls(rule, resolve.ArgFor(e.Site.Common(), fn, i), depth+1, seen)...)
				}
			}
		}
	}
	return out
}
