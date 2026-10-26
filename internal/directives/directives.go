// Package directives reads `//codegraph:` comments, the escape hatch for facts that
// static analysis cannot recover (config-driven topics, custom brokers, service
// naming, team ownership):
//
//	//codegraph:service orders                       name the service (main package)
//	//codegraph:alias orders-svc orders.internal     extra host names used by callers
//	//codegraph:tag team=checkout tier=1             service tags
//	//codegraph:produces kafka orders.created msg=orders.OrderCreated
//	//codegraph:consumes nats inventory.reserved role=consumer
//	//codegraph:produces nats stock.check kind=sync  (request/reply)
//
// A directive inside (or in the doc comment of) a function only applies when
// that function is reachable from the analysed entrypoint; file-level
// directives apply when their package is reachable. A `consumes` directive on
// a function makes that function the root of the endpoint's journey, and a
// `produces` directive attributes the endpoint to the journeys reaching it.
package directives

import (
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/zalgonoise/codegraph/internal/loader"
	"github.com/zalgonoise/codegraph/internal/model"
	"github.com/zalgonoise/codegraph/internal/resolve"
	"github.com/zalgonoise/codegraph/internal/symbols"
	"github.com/zalgonoise/codegraph/internal/transport"
)

// Directive is one parsed `//codegraph:` comment.
type Directive struct {
	Verb     string
	Args     []string
	KV       map[string]string
	Pkg      string
	Func     *ssa.Function // enclosing function, nil for file-level directives
	Position string
}

// Scan collects the directives of every module package.
func Scan(ws *loader.Workspace, prog *ssa.Program) []Directive {
	paths := make([]string, 0, len(ws.ModulePkgs))
	for p := range ws.ModulePkgs {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var out []Directive
	for _, path := range paths {
		pkg := ws.All[path]
		if pkg == nil || pkg.TypesInfo == nil {
			continue
		}
		for _, file := range pkg.Syntax {
			var decls []*ast.FuncDecl
			for _, d := range file.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok {
					decls = append(decls, fd)
				}
			}
			for _, group := range file.Comments {
				for _, c := range group.List {
					d, ok := parse(c.Text)
					if !ok {
						continue
					}
					d.Pkg = path
					d.Position = symbols.Pos(ws.Fset, c.Pos(), ws.Dir)
					if fd := enclosing(decls, c.Pos()); fd != nil {
						if obj, ok := pkg.TypesInfo.Defs[fd.Name].(*types.Func); ok {
							d.Func = prog.FuncValue(obj)
						}
					}
					out = append(out, d)
				}
			}
		}
	}
	return out
}

func parse(text string) (Directive, bool) {
	text = strings.TrimSpace(strings.TrimPrefix(text, "//"))
	rest, ok := strings.CutPrefix(text, "codegraph:")
	if !ok {
		return Directive{}, false
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return Directive{}, false
	}
	d := Directive{Verb: fields[0], KV: map[string]string{}}
	for _, f := range fields[1:] {
		if k, v, ok := strings.Cut(f, "="); ok && k != "" {
			d.KV[k] = v
		} else {
			d.Args = append(d.Args, f)
		}
	}
	return d, true
}

func enclosing(decls []*ast.FuncDecl, pos token.Pos) *ast.FuncDecl {
	for _, fd := range decls {
		start := fd.Pos()
		if fd.Doc != nil {
			start = fd.Doc.Pos()
		}
		if start <= pos && pos <= fd.End() {
			return fd
		}
	}
	return nil
}

// Result is what the active directives contribute to a manifest.
type Result struct {
	Service    string
	Aliases    []string
	Tags       map[string]string
	Detections []transport.Detection
}

// Evaluate applies the directives that are active for one entrypoint.
// scope is the set of module packages with reachable code.
func Evaluate(ds []Directive, reachable map[*ssa.Function]bool, scope map[string]bool, entryPkg string) Result {
	res := Result{Tags: map[string]string{}}
	for _, d := range ds {
		if d.Func != nil {
			if !reachable[d.Func] {
				continue
			}
		} else if !scope[d.Pkg] && d.Pkg != entryPkg {
			continue
		}

		switch d.Verb {
		case "service":
			if d.Pkg == entryPkg && len(d.Args) > 0 {
				res.Service = d.Args[0]
			}
		case "alias":
			if d.Pkg == entryPkg {
				res.Aliases = append(res.Aliases, d.Args...)
			}
		case "tag":
			for k, v := range d.KV {
				res.Tags[k] = v
			}
		case "produces", "consumes":
			if len(d.Args) == 0 {
				continue
			}
			ep := model.Endpoint{
				Transport: strings.ToLower(d.Args[0]),
				Direction: model.Outbound,
				Role:      "producer",
				Keys:      []string{model.Unknown},
				Message:   d.KV["msg"],
				Component: d.Pkg,
				Position:  d.Position,
				Source:    "directive",
				Count:     1,
			}
			if d.Verb == "consumes" {
				ep.Direction, ep.Role = model.Inbound, "consumer"
			}
			if ep.Transport == "http" || ep.Transport == "grpc" {
				ep.Role = map[model.Direction]string{model.Outbound: "client", model.Inbound: "server"}[ep.Direction]
			}
			if r := d.KV["role"]; r != "" {
				ep.Role = r
			}
			if k := d.KV["kind"]; k == model.Sync || k == model.Async {
				ep.Kind = k
			}
			if len(d.Args) > 1 {
				ep.Keys = []string{strings.Join(d.Args[1:], " ")}
			}
			det := transport.Detection{Func: d.Func}
			if d.Func != nil {
				ep.Function = symbols.FuncLabel(d.Func)
				if ep.Direction == model.Inbound {
					det.Roots = []resolve.Root{{Func: d.Func}}
				}
			}
			ep.Sites = []model.Site{{Function: ep.Function, Position: ep.Position}}
			det.Endpoint = ep
			res.Detections = append(res.Detections, det)
		}
	}
	return res
}
