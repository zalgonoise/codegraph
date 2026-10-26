// Package entrypoint describes what happens in main(): the ordered bootstrap
// sequence of constructors, registrations, server starts, goroutines and
// shutdown hooks, following calls into module code up to a configurable depth.
// When main hands its work to a CLI library, which dispatches to the command
// functions itself, the trace continues into each command's function.
package entrypoint

import (
	"go/token"
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/zalgonoise/codegraph/internal/model"
	"github.com/zalgonoise/codegraph/internal/reach"
	"github.com/zalgonoise/codegraph/internal/symbols"
)

// Analyzer walks the entrypoint of one service.
type Analyzer struct {
	Fset       *token.FileSet
	Root       string
	ModulePkgs map[string]bool
	Reach      *reach.Result
	MaxDepth   int
}

// Standard-library packages whose calls are never interesting in a bootstrap
// trace even when their names look like constructors (errors.New, ...).
var noisy = map[string]bool{
	"errors": true, "fmt": true, "log": true, "log/slog": true, "strings": true, "strconv": true,
	"bytes": true, "sort": true, "reflect": true, "sync": true, "sync/atomic": true,
	"unicode/utf8": true, "math": true, "encoding/json": true, "": true,
}

// Command is a CLI command and the function it runs.
type Command struct {
	Name string
	Root *ssa.Function
}

// Analyze returns the bootstrap steps of mainPkg.main in program order, then
// those of each command's function, in the order given.
func (a *Analyzer) Analyze(mainPkg *ssa.Package, commands ...Command) []model.Step {
	var steps []model.Step
	visited := map[*ssa.Function]bool{}
	command := ""

	var walk func(fn *ssa.Function, depth int)
	walk = func(fn *ssa.Function, depth int) {
		if fn == nil || visited[fn] || depth > a.MaxDepth {
			return
		}
		visited[fn] = true
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				site, ok := instr.(ssa.CallInstruction)
				if !ok {
					continue
				}
				label, name, pkg, moduleCallees := a.describe(site)
				inModule := a.ModulePkgs[pkg] || len(moduleCallees) > 0
				kind := classify(name)
				if !inModule && (kind == "" || noisy[pkg]) {
					continue
				}
				if kind == "" {
					kind = "call"
				}
				_, async := instr.(*ssa.Go)
				_, deferred := instr.(*ssa.Defer)
				steps = append(steps, model.Step{
					Depth:    depth,
					Kind:     kind,
					Call:     label,
					Command:  command,
					Package:  pkg,
					Caller:   symbols.FuncLabel(fn),
					Position: symbols.Pos(a.Fset, site.Pos(), a.Root),
					Async:    async,
					Deferred: deferred,
				})
				for _, callee := range moduleCallees {
					walk(callee, depth+1)
				}
			}
		}
	}
	walk(mainPkg.Func("main"), 0)
	for _, c := range commands {
		if c.Root == nil {
			continue
		}
		command, visited = c.Name, map[*ssa.Function]bool{} // each command's wiring in full
		steps = append(steps, model.Step{Kind: "command", Call: symbols.FuncLabel(c.Root), Command: c.Name,
			Caller: "cli " + c.Name, Position: symbols.Pos(a.Fset, c.Root.Pos(), a.Root)})
		walk(c.Root, 0)
	}

	for i := range steps {
		steps[i].Order = i + 1
	}
	return steps
}

// describe names a call site and lists the module functions it may run.
func (a *Analyzer) describe(site ssa.CallInstruction) (label, name, pkg string, moduleCallees []*ssa.Function) {
	c := site.Common()
	var callees []*ssa.Function
	switch {
	case c.IsInvoke():
		name = c.Method.Name()
		label = symbols.MethodLabel(c.Value.Type(), name)
		pkg = symbols.TypePkg(c.Value.Type())
		callees = a.Reach.Callees(site)
	case c.StaticCallee() != nil:
		fn := c.StaticCallee()
		o := symbols.Origin(fn)
		name = strings.TrimSuffix(o.Name(), "$bound")
		label = symbols.FuncLabel(o)
		pkg = symbols.PkgPath(o)
		callees = []*ssa.Function{fn}
	default:
		callees = a.Reach.Callees(site)
		label = "func value " + c.Value.Name()
		if len(callees) == 1 {
			label = symbols.FuncLabel(callees[0])
			name = symbols.Origin(callees[0]).Name()
			pkg = symbols.PkgPath(callees[0])
		}
	}
	for _, fn := range callees {
		if a.ModulePkgs[symbols.PkgPath(fn)] && len(fn.Blocks) > 0 {
			moduleCallees = append(moduleCallees, fn)
		}
	}
	return label, name, pkg, moduleCallees
}

func classify(name string) string {
	switch {
	case name == "":
		return ""
	case hasAnyPrefix(name, "Register", "Handle", "Subscribe", "QueueSubscribe", "Route", "Mount", "Use"):
		return "register"
	case hasAnyPrefix(name, "ListenAndServe", "Serve", "Listen", "Run", "Start", "Consume"):
		return "start"
	case hasAnyPrefix(name, "Shutdown", "Close", "Stop", "GracefulStop"):
		return "shutdown"
	case hasAnyPrefix(name, "Notify"):
		return "signal"
	case hasAnyPrefix(name, "New", "Must", "Open", "Dial", "Connect", "Load", "Parse", "Init", "Setup", "Provide", "Build"):
		return "construct"
	}
	return ""
}

func hasAnyPrefix(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
