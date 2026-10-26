// Package reach computes what is actually in use at runtime: the call graph
// and the set of functions reachable from a service's main/init.
//
//   - cha: class hierarchy analysis over the whole program (fast, imprecise).
//   - rta: rapid type analysis from the roots; also yields the set of types
//     that are converted to interfaces at runtime (used for implementations).
//   - vta: variable type analysis seeded with the RTA result (most precise
//     dynamic-call resolution; the default).
package reach

import (
	"fmt"
	"go/types"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/callgraph/cha"
	"golang.org/x/tools/go/callgraph/rta"
	"golang.org/x/tools/go/callgraph/vta"
	"golang.org/x/tools/go/ssa"
)

// Algorithm selects the call-graph construction.
type Algorithm string

const (
	CHA Algorithm = "cha"
	RTA Algorithm = "rta"
	VTA Algorithm = "vta"
)

// Result is the call graph and reachability for one entrypoint.
type Result struct {
	Algorithm    Algorithm
	Graph        *callgraph.Graph
	Roots        []*ssa.Function
	Reachable    map[*ssa.Function]bool
	RuntimeTypes []types.Type // types converted to interfaces at runtime (nil for CHA)
}

// Analyze builds the call graph rooted at mainPkg's init and main functions.
func Analyze(prog *ssa.Program, mainPkg *ssa.Package, algo Algorithm) (*Result, error) {
	var roots []*ssa.Function
	for _, name := range []string{"init", "main"} {
		if fn := mainPkg.Func(name); fn != nil {
			roots = append(roots, fn)
		}
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("%s has no main/init function", mainPkg.Pkg.Path())
	}
	return AnalyzeRoots(prog, roots, algo)
}

// AnalyzeRoots builds the call graph from the given roots: a library's
// exported API, or a main package's init and main.
func AnalyzeRoots(prog *ssa.Program, roots []*ssa.Function, algo Algorithm) (*Result, error) {
	if len(roots) == 0 {
		return nil, fmt.Errorf("no root functions")
	}
	res := &Result{Algorithm: algo, Roots: roots}
	switch algo {
	case CHA:
		res.Graph = cha.CallGraph(prog)
		res.Reachable = forward(res.Graph, roots)
	case RTA, VTA:
		r := rta.Analyze(roots, true)
		res.Graph = r.CallGraph
		res.Reachable = make(map[*ssa.Function]bool, len(r.Reachable))
		for fn := range r.Reachable {
			res.Reachable[fn] = true
		}
		r.RuntimeTypes.Iterate(func(t types.Type, _ any) {
			res.RuntimeTypes = append(res.RuntimeTypes, t)
		})
		if algo == VTA {
			res.Graph = vta.CallGraph(res.Reachable, r.CallGraph)
			res.Reachable = forward(res.Graph, roots)
		}
	default:
		return nil, fmt.Errorf("unknown call graph algorithm %q (want cha, rta or vta)", algo)
	}
	// Collapse wrappers/thunks so edges connect "real" functions.
	res.Graph.DeleteSyntheticNodes()
	return res, nil
}

// Callers returns the in-edges of fn.
func (r *Result) Callers(fn *ssa.Function) []*callgraph.Edge {
	if n := r.Graph.Nodes[fn]; n != nil {
		return n.In
	}
	return nil
}

// Callees returns the functions a call site may dispatch to.
func (r *Result) Callees(site ssa.CallInstruction) []*ssa.Function {
	n := r.Graph.Nodes[site.Parent()]
	if n == nil {
		return nil
	}
	var out []*ssa.Function
	for _, e := range n.Out {
		if e.Site == site {
			out = append(out, e.Callee.Func)
		}
	}
	return out
}

func forward(g *callgraph.Graph, roots []*ssa.Function) map[*ssa.Function]bool {
	seen := map[*ssa.Function]bool{}
	var queue []*callgraph.Node
	for _, fn := range roots {
		seen[fn] = true
		if n := g.Nodes[fn]; n != nil {
			queue = append(queue, n)
		}
	}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, e := range n.Out {
			if !seen[e.Callee.Func] {
				seen[e.Callee.Func] = true
				queue = append(queue, e.Callee)
			}
		}
	}
	return seen
}
