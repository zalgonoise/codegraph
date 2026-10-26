package resolve

import (
	"go/constant"
	"go/token"

	"golang.org/x/tools/go/ssa"
)

// MapEntry is one key/value pair set on a map, and where it is set.
type MapEntry struct {
	Key, Value ssa.Value
	Pos        token.Pos
}

// MapEntries lists the key/value pairs set on the map v holds: the updates of
// the map literal (or make) it comes from, through phi nodes, calls returning
// it (followed with their arguments bound), loads of where it is stored, and
// parameters (their callers' arguments). Pairs come in the order of their
// updates.
func (r *Resolver) MapEntries(v ssa.Value) []MapEntry {
	return r.mapEntries(v, 0, map[ssa.Value]bool{}, nil)
}

func (r *Resolver) mapEntries(v ssa.Value, depth int, seen map[ssa.Value]bool, fr *frame) []MapEntry {
	v = unwrap(v, false)
	if v == nil || depth > r.maxDepth || seen[v] {
		return nil
	}
	seen[v] = true
	defer delete(seen, v)

	var out []MapEntry
	switch x := v.(type) {
	case *ssa.MakeMap:
		if refs := x.Referrers(); refs != nil {
			for _, ref := range *refs {
				if u, ok := ref.(*ssa.MapUpdate); ok && u.Map == x {
					out = append(out, MapEntry{Key: u.Key, Value: u.Value, Pos: u.Pos()})
				}
			}
		}
	case *ssa.Phi:
		for _, e := range x.Edges {
			out = append(out, r.mapEntries(e, depth, seen, fr)...)
		}
	case *ssa.Parameter:
		if arg, parent, ok := fr.bound(x); ok {
			return r.mapEntries(arg, depth, seen, parent)
		}
		fn := x.Parent()
		if r.reach == nil {
			return nil
		}
		for i, p := range fn.Params {
			if p != x {
				continue
			}
			for _, e := range r.reach.Callers(fn) {
				if e.Site != nil {
					out = append(out, r.mapEntries(argFor(e.Site.Common(), fn, i), depth+1, seen, nil)...)
				}
			}
		}
	case *ssa.Call, *ssa.Extract:
		for _, res := range r.results(x, depth, fr) {
			out = append(out, r.mapEntries(res.v, depth+1, seen, res.fr)...)
		}
	case *ssa.UnOp:
		if x.Op == token.MUL {
			for _, val := range r.Stores(x.X) {
				out = append(out, r.mapEntries(val, depth+1, seen, fr)...)
			}
		}
	}
	return out
}

// SliceLiteral returns the elements of a slice literal in index order
// ([]int{2, 0, 2, 1}: an Alloc of an array, its elements stored through
// IndexAddrs with constant indexes), and false when v is not one.
func SliceLiteral(v ssa.Value) ([]ssa.Value, bool) {
	sl, ok := unwrap(v, false).(*ssa.Slice)
	if !ok {
		return nil, false
	}
	alloc, ok := sl.X.(*ssa.Alloc)
	if !ok || alloc.Referrers() == nil {
		return nil, false
	}
	byIndex := map[int64]ssa.Value{}
	var n int64
	for _, ref := range *alloc.Referrers() {
		ia, ok := ref.(*ssa.IndexAddr)
		if !ok || ia.X != alloc {
			continue
		}
		c, ok := ia.Index.(*ssa.Const)
		if !ok || c.Value == nil {
			return nil, false
		}
		i, ok := constant.Int64Val(c.Value)
		if !ok {
			return nil, false
		}
		vals := storedValues(ia)
		if len(vals) != 1 {
			return nil, false
		}
		byIndex[i] = vals[0]
		n = max(n, i+1)
	}
	out := make([]ssa.Value, n)
	for i := range out {
		if out[i] = byIndex[int64(i)]; out[i] == nil {
			return nil, false
		}
	}
	return out, true
}
