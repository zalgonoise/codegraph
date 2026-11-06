// Command codegraph statically analyses Go services and maps their architecture.
//
//	codegraph analyze    load a repository, build SSA and a call graph per main package (or
//	                     a library's exported API), and write one manifest (+ diagrams, report) each
//	codegraph link       connect several manifests into a system view and explorer
//	codegraph fleet      analyse several repositories with the same settings, then link them
//	codegraph diff       compare two runs: services, links, endpoints, unanswered topics
//	codegraph query      answer questions about a run (for people and AI agents)
//	codegraph mcp        serve those answers to MCP clients (Claude Code, Claude Desktop)
//	codegraph inventory  scan every repository cheaply (languages, go.mod, imports, container
//	                     images); suggest which to analyse in full
//	codegraph deploy     read what an environment runs from its Helm chart, matched to the run
//	codegraph bundle     pack a run into one file to share
//	codegraph run        run a pipeline of the configuration file (.codegraph.yaml), or one
//	                     step: inventory, graph, deploy, checks, diff, bundle; codegraph
//	                     alone runs the default pipeline
//	codegraph config     show the settings a run resolves (configuration, profile, flags)
//	codegraph check      check the tools, files and repositories a run needs
//	codegraph status     tell how current the last run is
//	codegraph init       write a starting configuration file
//
// Pipeline (each stage is its own package, wired here):
//
//	config → loader → program (SSA) → reach (CHA/RTA/VTA)
//	       → resolve → transport | entrypoint | components | directives
//	       → journeys → manifest → render (analyze)
//	manifest.ReadAll → link → render      (link)
package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/tools/go/ssa"

	"github.com/zalgonoise/codegraph/internal/bundle"
	"github.com/zalgonoise/codegraph/internal/components"
	"github.com/zalgonoise/codegraph/internal/config"
	"github.com/zalgonoise/codegraph/internal/configfile"
	"github.com/zalgonoise/codegraph/internal/declared"
	"github.com/zalgonoise/codegraph/internal/deploy"
	"github.com/zalgonoise/codegraph/internal/diff"
	"github.com/zalgonoise/codegraph/internal/directives"
	"github.com/zalgonoise/codegraph/internal/entrypoint"
	"github.com/zalgonoise/codegraph/internal/incremental"
	"github.com/zalgonoise/codegraph/internal/inventory"
	"github.com/zalgonoise/codegraph/internal/journeys"
	"github.com/zalgonoise/codegraph/internal/library"
	"github.com/zalgonoise/codegraph/internal/link"
	"github.com/zalgonoise/codegraph/internal/loader"
	"github.com/zalgonoise/codegraph/internal/manifest"
	"github.com/zalgonoise/codegraph/internal/mcpserver"
	"github.com/zalgonoise/codegraph/internal/model"
	"github.com/zalgonoise/codegraph/internal/options"
	"github.com/zalgonoise/codegraph/internal/pipeline"
	"github.com/zalgonoise/codegraph/internal/profile"
	"github.com/zalgonoise/codegraph/internal/program"
	"github.com/zalgonoise/codegraph/internal/query"
	"github.com/zalgonoise/codegraph/internal/reach"
	"github.com/zalgonoise/codegraph/internal/render"
	"github.com/zalgonoise/codegraph/internal/resolve"
	"github.com/zalgonoise/codegraph/internal/settings"
	"github.com/zalgonoise/codegraph/internal/symbols"
	"github.com/zalgonoise/codegraph/internal/transport"
)

const usage = `codegraph: static architecture analysis for Go services

Usage:
  codegraph                                 run the configuration file's default pipeline (.codegraph.yaml)
  codegraph run [pipeline|step ...]         run pipelines, or steps: inventory, graph, deploy, checks, diff, bundle (-dry-run: print them)
  codegraph config [-pipeline name]         show every setting a run resolves, and where it comes from
  codegraph check [-pipeline name]          check the tools, files and repositories a run needs
  codegraph status [-pipeline name]         tell how current the last run is, and which repositories moved since
  codegraph init [-profile dir]             write a starting configuration file (~/.codegraph.yaml)
  codegraph analyze [flags]                 analyse a repository (one manifest per main package)
  codegraph link [flags] manifest.json...   connect service manifests into a system view
  codegraph fleet [flags] repo-dir...       analyse several repositories, then link them
  codegraph diff -base dir -head dir        compare two runs (architecture changes, as Markdown)
  codegraph query [-run dir] <command>      answer questions about a run or a bundle ("codegraph query -h" lists them)
  codegraph mcp [-run dir]                  serve those answers over MCP (stdin and stdout)
  codegraph inventory -root dir [-focus f [-run dir] [-rings n]]  scan every repository cheaply; suggest which to analyse in full
  codegraph deploy -chart dir [-inventory f] [-run dir]  what an environment runs (helm template, offline), matched to services
  codegraph bundle [-run dir] [-out file]  pack a run into one file to share (gzip or zstd); query, mcp and diff read it with -run
  codegraph version                         print the version

Short forms: r run, c config, C check, s status, I init, a analyze, l link, f fleet, d diff, q query, m mcp,
i inventory, D deploy, b bundle, v version, h help (codegraph c -get out is codegraph config -get out).

Run "codegraph <command> -h" for a command's flags (query -h also lists the questions).
`

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		args = noCommand()
	}
	err := dispatch(args)
	var unknown errUnknownCommand
	switch {
	case errors.Is(err, flag.ErrHelp):
	case errors.As(err, &unknown):
		fmt.Fprintf(os.Stderr, "codegraph: %v\n\n%s", unknown, usage)
		os.Exit(2)
	case err != nil:
		fmt.Fprintln(os.Stderr, "codegraph:", err)
		os.Exit(1)
	}
}

// noCommand is what codegraph alone does: from a terminal, run the
// configuration file's default pipeline; otherwise (an MCP client configured
// without arguments, most likely, which must never start a run) print the
// usage, saying which arguments such a client must pass.
func noCommand() []string {
	if st, err := os.Stdin.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
		cf, err := configfile.Find("")
		switch {
		case err != nil:
			fmt.Fprintln(os.Stderr, "codegraph:", err)
			os.Exit(1)
		case cf != nil && cf.Default != "":
			return []string{"run"}
		case cf != nil:
			fmt.Fprint(os.Stderr, usage)
			fmt.Fprintf(os.Stderr, "\ncodegraph: %s names no default pipeline: set default to one of its pipelines (%s) to run it with codegraph alone.\n",
				cf.Path, strings.Join(cf.Names(), ", "))
			os.Exit(2)
		}
		fmt.Fprint(os.Stderr, usage)
		fmt.Fprintln(os.Stderr, "\ncodegraph: codegraph init writes a configuration file whose default pipeline codegraph alone then runs.")
		os.Exit(2)
	}
	fmt.Fprint(os.Stderr, usage)
	fmt.Fprintln(os.Stderr, `codegraph: no command given. An MCP client must pass one: "args": ["mcp", "-run", "<absolute path of the run, such as …/codegraph/tmp/graph>"] (make mcp-desktop prints the entry).`)
	os.Exit(2)
	return nil
}

// errUnknownCommand is a command codegraph does not have.
type errUnknownCommand string

func (e errUnknownCommand) Error() string {
	s := string(e)
	switch name := strings.TrimLeft(s, "-"); {
	case name != s && name != "":
		if c, ok := lookup(name); ok {
			return fmt.Sprintf("unknown command %q: commands take no dashes (codegraph %s), and flags go after the command", s, c.name)
		}
		return fmt.Sprintf("unknown command %q: flags go after the command (codegraph <command> %s)", s, s)
	default:
		if n := options.Suggest(s, commandNames()); n != "" && len(s) > 1 {
			return fmt.Sprintf("unknown command %q (did you mean %s?)", s, n)
		}
	}
	return fmt.Sprintf("unknown command %q", s)
}

// command is one of codegraph's commands, with its one-letter short form.
type command struct {
	name, short string
	run         func(args []string) error
}

// commands lists codegraph's commands. A short form is the name's first
// letter, in upper case for the second command of that letter (deploy, check,
// init). Commands take no dashes: -config, -run and -inventory are flags.
func commands() []command {
	report := func(name string) func([]string) error {
		return func(args []string) error { return runReport(name, args) }
	}
	return []command{
		{"analyze", "a", runAnalyze},
		{"link", "l", runLink},
		{"fleet", "f", runFleet},
		{"diff", "d", runDiff},
		{"query", "q", runQuery},
		{"mcp", "m", runMCP},
		{"inventory", "i", runInventory},
		{"deploy", "D", runDeploy},
		{"bundle", "b", runBundle},
		{"run", "r", runRun},
		{"config", "c", report("config")},
		{"check", "C", report("check")},
		{"status", "s", report("status")},
		{"init", "I", runInit},
		{"version", "v", func([]string) error { fmt.Println("codegraph", buildVersion()); return nil }},
		{"help", "h", func([]string) error { fmt.Print(usage); return nil }},
	}
}

func commandNames() []string {
	var names []string
	for _, c := range commands() {
		names = append(names, c.name)
	}
	return names
}

// lookup finds a command by its name or short form; -h, -help and --help are
// help, and -v, -version and --version the version, as is customary.
func lookup(arg string) (command, bool) {
	switch arg {
	case "-h", "-help", "--help":
		arg = "help"
	case "-v", "-version", "--version":
		arg = "version"
	}
	for _, c := range commands() {
		if arg == c.name || arg == c.short {
			return c, true
		}
	}
	return command{}, false
}

// dispatch runs one command; codegraph run's steps go through it too.
func dispatch(args []string) error {
	c, ok := lookup(args[0])
	if !ok {
		return errUnknownCommand(args[0])
	}
	return c.run(args[1:])
}

// ---- run, config, check, status, init -----------------------------------------

// runRun runs pipelines of the configuration file, or steps by name; none
// named, the file's default pipeline.
func runRun(args []string) error {
	cfg, err := config.ParseRun("run", args)
	if err != nil {
		return err
	}
	cf, err := configfile.Find(cfg.Config)
	if err != nil {
		return err
	}
	names := cfg.Names
	if len(names) == 0 {
		if cf == nil || cf.Default == "" {
			return errors.New("run: name a pipeline or a step (inventory, graph, deploy, checks, diff, bundle, org), or set a configuration file's default pipeline (codegraph init)")
		}
		names = []string{cf.Default}
	}
	warned := map[string]bool{}
	for _, name := range names {
		plan, err := pipeline.Resolve(pipeline.Request{Config: cf, Profile: cfg.Profile, Name: name, Flags: cfg.Options})
		if err != nil {
			return err
		}
		for _, w := range plan.Warnings {
			if !warned[w] {
				warned[w] = true
				logf("warning: %s", w)
			}
		}
		r := &pipeline.Runner{Dispatch: dispatch, DryRun: cfg.DryRun, Log: logf}
		if _, err := r.Run(plan); err != nil {
			return err
		}
	}
	return nil
}

// runReport shows the settings a run resolves (config), checks them (check),
// or tells how current the run is (status).
func runReport(cmd string, args []string) error {
	cfg, err := config.ParseRun(cmd, args)
	if err != nil {
		return err
	}
	cf, err := configfile.Find(cfg.Config)
	if err != nil {
		return err
	}
	name := cfg.Pipeline
	switch {
	case name == options.None: // the settings of a step run by name, as make's targets run them
		name = ""
	case name == "" && cf != nil && cf.Pipelines[cf.Default] != nil:
		name = cf.Default
	}
	plan, err := pipeline.Resolve(pipeline.Request{Config: cf, Profile: cfg.Profile, Name: name, Flags: cfg.Options})
	if err != nil {
		return err
	}
	switch {
	case cmd == "config" && cfg.Get != "":
		v, err := pipeline.Get(plan, cfg.Get)
		if err != nil {
			return err
		}
		fmt.Println(v)
		return nil
	case cmd == "config":
		for _, w := range plan.Warnings {
			logf("warning: %s", w)
		}
		if err := pipeline.Describe(os.Stdout, plan); err != nil {
			return err
		}
		run, from, err := pipeline.QueryRun(cf, cfg.Profile)
		if err != nil {
			return err
		}
		fmt.Printf("\nquery and mcp read %s (%s)\n", run, from)
		return nil
	case cmd == "check":
		if !pipeline.Check(os.Stdout, plan) {
			return errors.New("check: something above would make the run fail")
		}
		return nil
	}
	for _, w := range plan.Warnings {
		logf("warning: %s", w)
	}
	return pipeline.Status(os.Stdout, plan)
}

// runInit writes a starting configuration file.
func runInit(args []string) error {
	cfg, err := config.ParseInit(args)
	if err != nil {
		return err
	}
	out := cfg.Out
	if out == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		out = filepath.Join(home, configfile.Name)
	}
	if _, err := os.Stat(out); err == nil && !cfg.Force {
		return fmt.Errorf("init: %s exists; -force overwrites it", out)
	}
	prof := ""
	if cfg.Profile != "" {
		if prof, err = filepath.Abs(cfg.Profile); err != nil {
			return err
		}
		if _, err := profile.Load(prof); err != nil {
			return err
		}
	}
	if err := os.WriteFile(out, []byte(configfile.Template(prof)), 0o644); err != nil {
		return err
	}
	logf("wrote %s: edit it, then codegraph config shows what it resolves to, codegraph check checks it, and codegraph runs its default pipeline", out)
	return nil
}

// ---- analyze ------------------------------------------------------------------

func runAnalyze(args []string) error {
	cfg, err := config.ParseAnalyze(args)
	if err != nil {
		return err
	}
	rules, err := transport.LoadRules(cfg.RuleFiles, !cfg.NoDefaultRules)
	if err != nil {
		return err
	}
	_, err = analyzeRepo(cfg, rules)
	return err
}

// analyzeRepo analyses every main package of one repository and returns the
// paths of the manifests written.
func analyzeRepo(cfg *config.Analyze, rules []transport.Rule) ([]string, error) {

	logf("loading %v in %s", cfg.Entries, cfg.Dir)
	ws, err := loader.Load(loader.Options{
		Dir:        cfg.Dir,
		Patterns:   cfg.Entries,
		Tests:      cfg.Tests,
		BuildFlags: cfg.BuildFlags(),
		Include:    cfg.Include,
	})
	if err != nil {
		return nil, err
	}

	logf("building SSA for %d packages (%d in module %s)", len(ws.All), len(ws.ModulePkgs), ws.ModulePath)
	prog, err := program.Build(ws, cfg.Library)
	if err != nil {
		return nil, err
	}
	dirs := directives.Scan(ws, prog.SSA)

	var written []string
	write := func(m *model.Manifest) error {
		dir := filepath.Join(cfg.Out, m.Service)
		if err := writeService(dir, m, cfg); err != nil {
			return err
		}
		written = append(written, filepath.Join(dir, "manifest.json"))
		logf("%s: %d components, %d endpoints, %d bindings, %d journeys -> %s",
			m.Service, len(m.Components), len(m.Endpoints), len(m.Bindings), len(m.Journeys), dir)
		return nil
	}
	if cfg.Library {
		m, err := analyzeLibrary(cfg, ws, prog, rules, dirs)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ws.ModulePath, err)
		}
		return written, write(m)
	}
	for _, mainPkg := range prog.Mains {
		entry := mainPkg.Pkg.Path()
		if skipMain(cfg.SkipMains, entry, ws.PkgModule[entry]) {
			logf("%s: skipped (-skip-mains)", entry)
			continue
		}
		m, err := analyzeService(cfg, ws, prog, mainPkg, rules, dirs)
		if err != nil {
			return written, fmt.Errorf("%s: %w", entry, err)
		}
		if err := write(m); err != nil {
			return written, err
		}
	}
	return written, nil
}

// skipMain reports whether a main package (matched relative to its module)
// is a tool to leave out, such as a migration runner.
func skipMain(re *regexp.Regexp, entry, module string) bool {
	if re == nil {
		return false
	}
	rel := strings.TrimPrefix(strings.TrimPrefix(entry, module), "/")
	return re.MatchString(rel)
}

func analyzeService(cfg *config.Analyze, ws *loader.Workspace, prog *program.Program, mainPkg *ssa.Package,
	rules []transport.Rule, dirs []directives.Directive) (*model.Manifest, error) {

	entry := mainPkg.Pkg.Path()
	logf("%s: call graph (%s)", entry, cfg.Algorithm)
	res, err := reach.Analyze(prog.SSA, mainPkg, reach.Algorithm(cfg.Algorithm))
	if err != nil {
		return nil, err
	}
	return analyzeReach(cfg, ws, prog, rules, dirs, target{entry: entry, mainPkg: mainPkg, res: res})
}

// analyzeLibrary analyses a module without a main through its exported API
// (internal/library): reachability from every exported function and method of
// its public packages, and one inbound endpoint per symbol.
func analyzeLibrary(cfg *config.Analyze, ws *loader.Workspace, prog *program.Program,
	rules []transport.Rule, dirs []directives.Directive) (*model.Manifest, error) {

	var skip map[string]bool
	if !cfg.Generated {
		files := map[string][]*ast.File{}
		for path, p := range ws.All {
			if ws.ModulePkgs[path] {
				files[path] = p.Syntax
			}
		}
		skip = library.Generated(prog.SSA.Fset, files)
	}
	api := library.API(prog.Packages, ws.ModulePath, skip, cfg.Internal)
	if all := library.API(prog.Packages, ws.ModulePath, nil, cfg.Internal); len(all) > len(api) {
		logf("%s: %d exported symbols of generated files left out (-generated includes them)", ws.ModulePath, len(all)-len(api))
	}
	if len(api) == 0 {
		return nil, errNoAPI
	}
	internal := 0
	for _, s := range api {
		if s.Internal {
			internal++
		}
	}
	if internal > 0 {
		logf("%s: %d of the %d exported symbols are in internal packages (role internal)", ws.ModulePath, internal, len(api))
	}
	algo := reach.Algorithm(cfg.Algorithm)
	roots := library.Roots(api, prog.Packages, ws.ModulePkgs)
	if n := library.Generic(roots); n > 0 && algo != reach.CHA {
		// RTA and VTA cannot start from a generic function's body.
		logf("%s: %d generic functions in the API: call graph cha instead of %s", ws.ModulePath, n, algo)
		algo = reach.CHA
	}
	logf("%s: library, %d exported symbols: call graph (%s)", ws.ModulePath, len(api), algo)
	// An interface method runs the library's types implementing it: those the
	// API converts to interfaces (a constructor returning one), which only a
	// first pass finds; their methods are then roots too.
	var res *reach.Result
	for pass := 0; ; pass++ {
		var err error
		if res, err = reach.AnalyzeRoots(prog.SSA, roots, algo); err != nil {
			return nil, err
		}
		if pass == 3 {
			break
		}
		typs := res.RuntimeTypes
		if typs == nil {
			typs = library.ModuleTypes(prog.Packages, ws.ModulePkgs)
		}
		library.Implement(api, prog.SSA, append(library.PublicTypes(prog.Packages, ws.ModulePath), typs...), ws.ModulePkgs)
		next := library.Roots(api, prog.Packages, ws.ModulePkgs)
		if len(next) == len(roots) {
			break
		}
		roots = next
	}
	lcfg := *cfg
	lcfg.Algorithm = string(algo)
	return analyzeReach(&lcfg, ws, prog, rules, dirs, target{
		entry: ws.ModulePath, res: res, library: true,
		api: library.Detections(api, prog.SSA.Fset, ws.Dir, ws.ModulePath),
	})
}

// errNoAPI: a module with no exported function or method to analyse.
var errNoAPI = errors.New("no exported function or method (outside generated files, examples and cmd)")

// target is what one manifest describes: a main package, or a library's API.
type target struct {
	entry   string // the main package, or the library's module
	mainPkg *ssa.Package
	res     *reach.Result
	library bool
	api     []transport.Detection // a library's exported symbols
}

func analyzeReach(cfg *config.Analyze, ws *loader.Workspace, prog *program.Program,
	rules []transport.Rule, dirs []directives.Directive, t target) (*model.Manifest, error) {

	entry, res := t.entry, t.res
	funcs, scope := moduleFuncs(res, ws.ModulePkgs)
	fset := prog.SSA.Fset

	resolver := resolve.New(prog.SSA, res, funcs, ws.ModulePkgs, cfg.ResolveDepth)

	detector := &transport.Detector{
		Rules: rules, Resolver: resolver, Reach: res, Fset: fset, Root: ws.Dir, ModulePkgs: ws.ModulePkgs,
	}
	bootstrap := &entrypoint.Analyzer{
		Fset: fset, Root: ws.Dir, ModulePkgs: ws.ModulePkgs, Reach: res, MaxDepth: cfg.EntryDepth,
	}
	entryPkg := entry
	if t.library {
		entryPkg = ""
	}
	comps := &components.Analyzer{
		Fset: fset, Root: ws.Dir, ModulePath: ws.ModulePath, EntryPkg: entryPkg,
		ModulePkgs: ws.ModulePkgs, PkgModule: ws.PkgModule, Packages: ws.All,
		Reach: res, IncludeStd: cfg.IncludeStd,
	}
	dres := directives.Evaluate(dirs, res.Reachable, scope, entry)

	detections := append(append(detector.Detect(funcs), dres.Detections...), t.api...)
	if cfg.MainJourney && t.mainPkg != nil {
		if fn := t.mainPkg.Func("main"); fn != nil {
			detections = append(detections, journeys.MainDetection(fn, fset, ws.Dir))
		}
	}
	endpoints := make([]model.Endpoint, len(detections))
	for i, d := range detections {
		endpoints[i] = d.Endpoint
	}
	var paths []model.Journey
	if cfg.Journeys {
		paths = (&journeys.Analyzer{
			Reach: res, Resolver: resolver, ModulePkgs: ws.ModulePkgs, Fset: fset, Root: ws.Dir, MaxDepth: cfg.JourneyDepth,
			PkgModule: ws.PkgModule, IncludeStd: cfg.IncludeStd,
		}).Analyze(detections)
	}

	tags := map[string]string{}
	for k, v := range dres.Tags {
		tags[k] = v
	}
	for k, v := range cfg.Tags { // CLI wins over directives
		tags[k] = v
	}
	aliases := dres.Aliases
	var name string
	var steps []model.Step
	if t.library {
		aliases = append(aliases, cfg.Aliases...)
		name = cmp.Or(cfg.Service, dres.Service, defaultName(ws.ModulePath, ws.ModulePath))
	} else {
		name = serviceName(cfg, dres.Service, entry, ws.PkgModule[entry], len(prog.Mains))
		if len(prog.Mains) == 1 {
			aliases = append(aliases, cfg.Aliases...)
		}
		steps = bootstrap.Analyze(t.mainPkg, commandRoots(detections)...)
	}

	return manifest.Build(manifest.Input{
		Codegraph:  buildVersion(),
		Service:    name,
		Aliases:    aliases,
		Tags:       tags,
		Module:     ws.ModulePath,
		Entry:      entry,
		Algorithm:  cfg.Algorithm,
		Library:    t.library,
		Bootstrap:  steps,
		Components: comps.Analyze(funcs),
		Endpoints:  endpoints,
		Journeys:   paths,
		Settings:   settings.Names(res),
		Stats:      model.Stats{ReachableFuncs: len(res.Reachable), ModuleFuncs: len(funcs)},
	}), nil
}

// moduleFuncs returns the reachable functions with bodies that belong to the
// analysed module(s), in a deterministic order, plus the packages they span.
func moduleFuncs(res *reach.Result, modulePkgs map[string]bool) ([]*ssa.Function, map[string]bool) {
	type entry struct {
		name string
		fn   *ssa.Function
	}
	scope := map[string]bool{}
	var list []entry
	for fn := range res.Reachable {
		pkg := symbols.PkgPath(fn)
		if !modulePkgs[pkg] || len(fn.Blocks) == 0 {
			continue
		}
		scope[pkg] = true
		list = append(list, entry{fn.String(), fn})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].name < list[j].name })
	funcs := make([]*ssa.Function, len(list))
	for i, e := range list {
		funcs[i] = e.fn
	}
	return funcs, scope
}

func serviceName(cfg *config.Analyze, fromDirective, entry, module string, mains int) string {
	switch {
	case cfg.Service != "" && mains == 1:
		return cfg.Service
	case fromDirective != "":
		return fromDirective
	}
	if cfg.Service != "" {
		logf("ignoring -service for %s: %d main packages matched (use //codegraph:service instead)", entry, mains)
	}
	return defaultName(entry, module)
}

var majorVersion = regexp.MustCompile(`^v[0-9]+$`)

// defaultName names a service after its repository, so that the many repos
// with cmd/grpc and cmd/subscribers mains do not collide: <module>/cmd is the
// module's name, <module>/cmd/<role>[/...] is "<module>-<role>[-...]"
// (orders-grpc, notifier-service-grpc-v1) unless the role repeats the module
// name, and any other main package keeps its directory name.
func defaultName(entry, module string) string {
	name := path.Base(module)
	if majorVersion.MatchString(name) && path.Dir(module) != "." { // example.com/bus/v2
		name = path.Base(path.Dir(module))
	}
	rel, ok := strings.CutPrefix(entry, module)
	switch {
	case module == "" || !ok:
		return path.Base(entry)
	case rel == "" || rel == "/cmd":
		return name
	case strings.HasPrefix(rel, "/cmd/"):
		roles := strings.Split(strings.TrimPrefix(rel, "/cmd/"), "/")
		if len(roles) == 1 && roles[0] == name {
			return name
		}
		return name + "-" + strings.Join(roles, "-")
	}
	return path.Base(entry)
}

func writeService(dir string, m *model.Manifest, cfg *config.Analyze) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	g := render.Service(m, render.ServiceOptions{Libraries: cfg.Libraries})
	mmd := render.Mermaid(g)
	if cfg.Formats["json"] {
		if err := manifest.WriteJSON(filepath.Join(dir, "manifest.json"), m); err != nil {
			return err
		}
	}
	return writeTexts(dir, cfg.Formats, map[string]string{
		"mermaid": "service.mmd",
		"dot":     "service.dot",
		"md":      "report.md",
	}, map[string]func() string{
		"mermaid": func() string { return mmd },
		"dot":     func() string { return render.DOT(g) },
		"md":      func() string { return render.ServiceReport(m, mmd) },
	})
}

// ---- link ---------------------------------------------------------------------

func runLink(args []string) error {
	cfg, err := config.ParseLink(args)
	if err != nil {
		return err
	}
	_, err = linkRun(cfg)
	return err
}

// linkRun links the manifests and writes the system's outputs.
func linkRun(cfg *config.Link) (*model.System, error) {
	ms, err := manifest.ReadAll(cfg.Manifests)
	if err != nil {
		return nil, err
	}
	sys := link.Link(ms)
	if err := os.MkdirAll(cfg.Out, 0o755); err != nil {
		return nil, err
	}
	g := render.System(sys)
	mmd := render.Mermaid(g)
	if cfg.Formats["json"] {
		if err := manifest.WriteJSON(filepath.Join(cfg.Out, "system.json"), sys); err != nil {
			return nil, err
		}
	}
	if err := writeTexts(cfg.Out, cfg.Formats, map[string]string{
		"mermaid": "system.mmd",
		"dot":     "system.dot",
		"md":      "system.md",
	}, map[string]func() string{
		"mermaid": func() string { return mmd },
		"dot":     func() string { return render.DOT(g) },
		"md":      func() string { return render.SystemReport(sys, mmd) },
	}); err != nil {
		return nil, err
	}
	if cfg.Formats["html"] {
		var dep *deploy.Deployment
		if cfg.Deploy != "" {
			if dep, err = deploy.ReadFile(cfg.Deploy); err != nil {
				return nil, err
			}
		}
		page, err := render.Explorer(sys, ms, dep)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(cfg.Out, "system.html"), []byte(page), 0o644); err != nil {
			return nil, err
		}
	}
	if cfg.Formats["agents"] {
		if err := writeAgentFiles(cfg.Out, cfg.Manifests, ms, sys); err != nil {
			return nil, err
		}
	}
	logf("%d services, %d links, %d unmatched endpoints -> %s", len(sys.Services), len(sys.Links), len(sys.Dangling), cfg.Out)
	return sys, nil
}

// writeAgentFiles writes the guide for AI agents and the compact system
// summary. AGENTS.md goes to the run directory (where the manifests are) when
// the link output is inside it, as with fleet; otherwise next to context.md.
func writeAgentFiles(out string, paths []string, ms []*model.Manifest, sys *model.System) error {
	runDir := ""
	for _, p := range paths {
		dir := filepath.Dir(filepath.Dir(p))
		if runDir != "" && dir != runDir {
			runDir = ""
			break
		}
		runDir = dir
	}
	guideDir := out
	if abs, err := filepath.Abs(runDir); err == nil && runDir != "" {
		runDir = abs
		if o, err := filepath.Abs(out); err == nil && filepath.Dir(o) == abs {
			guideDir = abs
		}
	}
	r := query.New(runDir, ms, sys)
	if err := os.WriteFile(filepath.Join(guideDir, "AGENTS.md"), []byte(query.Guide(r, codegraphDir())), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "context.md"), []byte(query.Context(r)), 0o644)
}

// codegraphDir is the working directory when it is the codegraph repository
// (as with make graph), so the guide can say where to run codegraph from.
func codegraphDir() string {
	dir, err := os.Getwd()
	if err != nil || modulePath(filepath.Join(dir, "go.mod")) != "github.com/zalgonoise/codegraph" {
		return ""
	}
	return dir
}

// ---- fleet --------------------------------------------------------------------

// runFleet analyses every repository with the same settings (one failing
// repository is reported and skipped), then links all their manifests. With
// -reuse, a repository unchanged since that run (incremental.Key) has its
// services copied from it instead of analysed again; fleet.json records the
// keys for the next run.
func runFleet(args []string) error {
	f, err := config.ParseFleet(args)
	if err != nil {
		return err
	}
	rec := &incremental.Record{Codegraph: buildVersion(), Created: time.Now().UTC()}
	var prev *incremental.Record
	if f.Reuse != "" {
		if same, _ := sameDir(f.Reuse, f.Analyze.Out); same {
			return errors.New("fleet: -reuse names the output directory; give the previous run's (make graph moves it to <out>.prev)")
		}
		if prev, err = incremental.Read(f.Reuse); err != nil {
			logf("%v: every repository is analysed", err)
		} else if prev == nil {
			logf("%s: no record of its repositories (%s): every repository is analysed", f.Reuse, incremental.File)
		}
	}
	toolchain, err := incremental.Toolchain()
	if err != nil {
		logf("no key for the next run to reuse this one by: %v", err)
	}
	settings := fleetSettings(f)
	var manifests, libraries, failed, apis, apiFailed, reused []string
	owner := map[string]string{} // service -> repository
	for _, repo := range f.Repos {
		entry := incremental.Repo{Dir: repo}
		if abs, err := filepath.Abs(repo); err == nil {
			entry.Dir = abs
		}
		cfg := f.Analyze
		cfg.Dir, cfg.Entries = repo, f.Entries
		if len(cfg.Entries) == 0 {
			switch inCmd, elsewhere := findMains(repo); {
			case inCmd:
				cfg.Entries = []string{"./cmd/..."}
			case elsewhere: // main.go at the root, and cmd/ holds its (cobra) commands
				cfg.Entries = []string{"./..."}
			case f.Libraries:
				logf("%s: no main package, a library (analysed through its exported API)", repo)
				libraries = append(libraries, repo)
				cfg.Entries, cfg.Library = []string{"./..."}, true
			default:
				logf("%s: no main package, a library (skipped)", repo)
				libraries = append(libraries, repo)
				entry.Library = true
				rec.Repos = append(rec.Repos, entry)
				continue
			}
		}
		entry.Library = cfg.Library
		files := append(repoRules(repo, f.RepoRules), f.Analyze.RuleFiles...) // repository rules first, so they win
		rules, err := transport.LoadRules(files, !f.Analyze.NoDefaultRules)
		if err != nil {
			return fmt.Errorf("%s: %w", repo, err)
		}
		entry.Head, entry.Key, entry.Why = incremental.Key(toolchain, repo, settings, files)
		apiName := defaultName(modulePath(filepath.Join(repo, "go.mod")), modulePath(filepath.Join(repo, "go.mod"))) + "-api"
		var written []string
		if old := prev.Find(entry.Dir); old != nil && entry.Key != "" && old.Key == entry.Key && incremental.Has(f.Reuse, old.Services) {
			if err := incremental.Copy(f.Reuse, f.Analyze.Out, old.Services); err != nil {
				return fmt.Errorf("%s: reusing its analysis: %w", repo, err)
			}
			for _, svc := range old.Services {
				written = append(written, filepath.Join(f.Analyze.Out, svc, "manifest.json"))
				if f.API && svc == apiName {
					apis = append(apis, svc)
				}
			}
			entry.Reused = true
			reused = append(reused, repo)
			logf("%s: unchanged since %s, its analysis reused (%s)", repo, f.Reuse, strings.Join(old.Services, ", "))
		} else {
			if written, err = analyzeRepoSafely(&cfg, rules); err != nil {
				logf("%s: %v (skipped)", repo, err)
				failed = append(failed, repo)
				entry.Failed = true
			}
			if f.API && !cfg.Library {
				// The API of a repository with mains, as a library of its own.
				api := cfg
				api.Entries, api.Library, api.Service = []string{"./..."}, true, apiName
				w, err := analyzeRepoSafely(&api, rules)
				switch {
				case errors.Is(err, errNoAPI):
					logf("%s: no exported API to analyse", repo)
				case err != nil:
					logf("%s: its API: %v (skipped)", repo, err)
					apiFailed = append(apiFailed, repo)
					entry.Failed = true
				default:
					apis = append(apis, api.Service)
				}
				written = append(written, w...)
			}
			if entry.Failed {
				entry.Key, entry.Why = "", "its analysis failed"
			}
		}
		for _, p := range written {
			entry.Services = append(entry.Services, filepath.Base(filepath.Dir(p)))
		}
		rec.Repos = append(rec.Repos, entry)
		for _, p := range written {
			svc := filepath.Base(filepath.Dir(p))
			if prev, ok := owner[svc]; ok {
				logf("%s: service %q already comes from %s; not linked (use //codegraph:service)", repo, svc, prev)
				continue
			}
			owner[svc] = repo
			manifests = append(manifests, p)
		}
	}
	var declared []string
	if f.Declared != "" {
		written, err := writeDeclared(f.Declared, &f.Analyze)
		if err != nil {
			return err
		}
		for _, p := range written {
			svc := filepath.Base(filepath.Dir(p))
			if repo, ok := owner[svc]; ok {
				return fmt.Errorf("declared service %q is analysed from %s: remove it from %s", svc, repo, f.Declared)
			}
			declared = append(declared, svc)
		}
		manifests = append(manifests, written...)
	}
	if len(manifests) == 0 {
		return errors.New("fleet: no manifest written")
	}
	linkArgs := []string{"-out", filepath.Join(f.Analyze.Out, "_system"), "-format", formatList(f.Analyze.Formats)}
	lc, err := config.ParseLink(append(linkArgs, manifests...))
	if err != nil {
		return err
	}
	sys, err := linkRun(lc)
	if err != nil {
		return err
	}
	rec.Services, rec.Links, rec.Unmatched = len(sys.Services), len(sys.Links), len(sys.Dangling)
	if err := rec.Write(f.Analyze.Out); err != nil {
		return err
	}
	logf("fleet: %d repositories, %d services (%d declared %v), %d libraries %v, %d failed %v",
		len(f.Repos), len(manifests), len(declared), declared, len(libraries), libraries, len(failed), failed)
	if f.Reuse != "" {
		logf("fleet: %d repositories unchanged since %s, their analysis reused", len(reused), f.Reuse)
	}
	if f.API {
		logf("fleet: the API of %d repositories with mains %v, %d failed %v", len(apis), apis, len(apiFailed), apiFailed)
	}
	return nil
}

// fleetSettings is what a repository's analysis reads besides its own files
// and rule files: every analysis setting (part of its incremental.Key).
func fleetSettings(f *config.Fleet) any {
	a := f.Analyze
	a.Dir, a.Out = "", ""
	return struct {
		Analyze   config.Analyze
		Entries   []string
		Libraries bool
		API       bool
	}{a, f.Entries, f.Libraries, f.API}
}

// sameDir reports whether two paths name the same existing directory.
func sameDir(a, b string) (bool, error) {
	sa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	sb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(sa, sb), nil
}

// writeDeclared writes the manifests of the services described by hand in dir
// (<service>.json) next to the analysed ones, and returns their paths.
func writeDeclared(dir string, cfg *config.Analyze) ([]string, error) {
	files, err := declared.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var written []string
	for _, f := range files {
		m, err := f.Manifest()
		if err != nil {
			return nil, err
		}
		m.Codegraph = buildVersion()
		out := filepath.Join(cfg.Out, m.Service)
		if err := writeService(out, m, cfg); err != nil {
			return nil, err
		}
		logf("%s: declared (%s, %d endpoints, %d journeys) -> %s", m.Service, m.Declared.Language, len(m.Endpoints), len(m.Journeys), out)
		written = append(written, filepath.Join(out, "manifest.json"))
	}
	return written, nil
}

// ---- diff ---------------------------------------------------------------------

// runDiff compares two runs and writes the changes as Markdown; with
// -fail-on-links it fails when cross-service links changed, for CI.
func runDiff(args []string) error {
	cfg, err := config.ParseDiff(args)
	if err != nil {
		return err
	}
	base, err := readRun(cfg.Base)
	if err != nil {
		return err
	}
	head, err := readRun(cfg.Head)
	if err != nil {
		return err
	}
	r := diff.Compare(base, head)
	report := diff.Markdown(r, cfg.Base, cfg.Head)
	if cfg.Out == "" {
		fmt.Print(report)
	} else if err := os.WriteFile(cfg.Out, []byte(report), 0o644); err != nil {
		return err
	}
	logf("diff: %d links added, %d removed; %d services with endpoint changes",
		len(r.Links.Added), len(r.Links.Removed), len(r.Endpoints))
	if cfg.FailOnLinks && r.LinksChanged() {
		return errors.New("cross-service links changed (-fail-on-links)")
	}
	return nil
}

// ---- query --------------------------------------------------------------------

// runQuery answers one question about a run, as Markdown or JSON.
func runQuery(args []string) error {
	cfg, err := config.ParseQuery(args, query.Commands)
	if err != nil {
		return err
	}
	if cfg.Run, cfg.Explorer, err = queryRun(cfg.Config, cfg.Profile, cfg.Run, cfg.Explorer); err != nil {
		return err
	}
	run, err := query.Load(cfg.Run)
	if err != nil {
		return err
	}
	run.UseExplorer(cfg.Explorer)
	if err := run.UseInventory(cfg.Inventory); err != nil {
		return err
	}
	answer, err := query.Answer(run, cfg.Args, query.Options{Depth: cfg.Depth, JSON: cfg.JSON, Diagram: cfg.Diagram})
	if err != nil {
		return err
	}
	fmt.Print(answer)
	return nil
}

// ---- inventory ----------------------------------------------------------------

// runInventory scans every repository under -root and writes inventory.json
// and its report.
func runInventory(args []string) error {
	cfg, err := config.ParseInventory(args)
	if err != nil {
		return err
	}
	dirs, err := repoDirs(cfg.Root)
	if err != nil {
		return err
	}
	prof, err := profile.Load(cfg.Profile)
	if err != nil {
		return err
	}
	inv := inventory.Scan(dirs, prof.Conventions)
	inv.Codegraph = buildVersion()
	names := prof.Catalogues
	if cfg.CataloguesSet {
		names = cfg.Catalogues
	}
	var catalogues []string
	for _, name := range names {
		for _, dir := range []string{filepath.Join(cfg.Root, name+".git"), filepath.Join(cfg.Root, name)} {
			if st, err := os.Stat(dir); err == nil && st.IsDir() {
				catalogues = append(catalogues, dir)
				break
			}
		}
	}
	if inv.Contracts, err = inventory.Catalogue(catalogues); err != nil {
		return err
	}
	var run *query.Run
	if cfg.Run != "" {
		if run, err = query.Load(cfg.Run); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(cfg.Out, 0o755); err != nil {
		return err
	}
	if err := manifest.WriteJSON(filepath.Join(cfg.Out, "inventory.json"), inv); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(cfg.Out, "inventory.md"), []byte(inventory.Report(inv)), 0o644); err != nil {
		return err
	}
	if len(catalogues) > 0 {
		if err := os.WriteFile(filepath.Join(cfg.Out, "contracts.md"), []byte(inventory.ContractsReport(inv.Contracts, contractUse(run))), 0o644); err != nil {
			return err
		}
	}
	logf("inventory: %d repositories, %d contracts in %d catalogues -> %s", len(inv.Repos), len(inv.Contracts), len(catalogues), cfg.Out)
	if run != nil {
		staleDeclared(run, inv)
	}
	if cfg.Focus == "" {
		return nil
	}
	focus, err := readNames(cfg.Focus)
	if err != nil {
		return err
	}
	hints := func([]string) inventory.Hints { return inventory.Hints{} }
	if run != nil {
		hints = func(focus []string) inventory.Hints { return frontierHints(run, inv.Modules(focus)) }
	}
	f := inv.Rings(focus, cfg.Rings, hints)
	programs := f.Programs()
	if err := manifest.WriteJSON(filepath.Join(cfg.Out, "frontier.json"), f); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(cfg.Out, "frontier.md"), []byte(inventory.FrontierReport(f)), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(cfg.Out, "frontier.txt"), []byte(strings.Join(programs, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	logf("frontier: %d repositories in %d rings around %d, %d Go programs to analyse (frontier.txt)", len(f.Candidates), f.Rings, len(focus), len(programs))
	return nil
}

// readNames reads a list of repository names: one per line, # comments.
// staleDeclared warns about services described by hand whose repository has
// moved on since the description was read.
func staleDeclared(run *query.Run, inv *inventory.Inventory) {
	heads := map[string]string{}
	for _, r := range inv.Repos {
		heads[r.Name] = r.Head
	}
	for _, m := range run.Manifests {
		d := m.Declared
		if d == nil || d.Commit == "" {
			continue
		}
		if head, ok := heads[d.Repository]; ok && !strings.HasPrefix(head, d.Commit) {
			logf("declared %s was read at %s; %s is now at %.9s: check %s", m.Service, d.Commit, d.Repository, head, d.File)
		}
	}
}

// contractUse lists, per gRPC contract, the services of a run that serve and
// call it (nil without a run).
func contractUse(run *query.Run) map[string]inventory.ContractUse {
	if run == nil {
		return nil
	}
	sets := map[string][2]map[string]bool{}
	for _, m := range run.Manifests {
		name := m.Service
		if m.Declared != nil {
			name += " (declared)"
		}
		for _, ep := range m.Endpoints {
			if ep.Transport != "grpc" || ep.Contract == "" {
				continue
			}
			if _, ok := sets[ep.Contract]; !ok {
				sets[ep.Contract] = [2]map[string]bool{{}, {}}
			}
			switch ep.Role {
			case "server":
				sets[ep.Contract][0][name] = true
			case "client":
				sets[ep.Contract][1][name] = true
			}
		}
	}
	use := map[string]inventory.ContractUse{}
	for c, s := range sets {
		use[c] = inventory.ContractUse{Servers: slices.Sorted(maps.Keys(s[0])), Callers: slices.Sorted(maps.Keys(s[1]))}
	}
	return use
}

func readNames(file string) ([]string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if line, _, _ = strings.Cut(line, "#"); strings.TrimSpace(line) != "" {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out, nil
}

// frontierHints reads, from a run, the focus services' modules, the
// contracts they serve, the contracts they call that none of them serves,
// and the topics and queues with one side only among them. The focus
// services are linked on their own, so a run that already includes the
// frontier gives the same hints.
func frontierHints(run *query.Run, focusModules map[string]bool) inventory.Hints {
	var ms []*model.Manifest
	for _, m := range run.Manifests {
		if focusModules[m.Module] {
			ms = append(ms, m)
		}
	}
	h := inventory.Hints{Served: map[string]string{}}
	modules := map[string]bool{}
	byID := map[string]model.Endpoint{}
	for _, m := range ms {
		modules[m.Module] = true
		for _, ep := range m.Endpoints {
			byID[m.Service+"|"+ep.ID] = ep
			if ep.Transport == "grpc" && ep.Role == "server" && ep.Contract != "" {
				h.Served[protoPackage(ep.Contract)] = m.Module
			}
		}
	}
	for m := range modules {
		h.Modules = append(h.Modules, m)
	}
	missing, topics := map[string]bool{}, map[string]bool{}
	for _, d := range link.Link(ms).Dangling {
		ep := byID[d.Service+"|"+d.Endpoint]
		switch {
		case d.Transport == "grpc" && d.Role == "client" && ep.Contract != "":
			if pkg := protoPackage(ep.Contract); h.Served[pkg] == "" {
				missing[pkg] = true
			}
		case (d.Transport == "kafka" || d.Transport == "amqp" || d.Transport == "nats") && d.Reason == "":
			for _, k := range strings.Split(d.Key, ", ") { // a partly linked endpoint's unlinked keys only
				if strings.IndexFunc(k, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0 {
					topics[k] = true // a name, not an unresolved "*"
				}
			}
		}
	}
	for k := range missing {
		h.Missing = append(h.Missing, k)
	}
	for k := range topics {
		h.Topics = append(h.Topics, k)
	}
	sort.Strings(h.Modules)
	sort.Strings(h.Missing)
	sort.Strings(h.Topics)
	return h
}

// protoPackage strips the service name from a contract: "a.b.v1.Svc" -> "a.b.v1".
func protoPackage(contract string) string {
	if i := strings.LastIndex(contract, "."); i > 0 {
		return contract[:i]
	}
	return contract
}

// repoDirs lists the repositories under root: every directory.
func repoDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, filepath.Join(root, e.Name()))
		}
	}
	return out, nil
}

// ---- deploy -------------------------------------------------------------------

// runDeploy reads what an environment runs, from its chart rendered with helm
// or from objects rendered already, matches it to repositories and analysed
// services, and writes deploy.json and deploy.md.
func runDeploy(args []string) error {
	cfg, err := config.ParseDeploy(args)
	if err != nil {
		return err
	}
	var rendered io.Reader
	source := cfg.Rendered
	switch {
	case cfg.Chart != "":
		var secrets []string
		for _, s := range cfg.Secrets {
			if fileExists(filepath.Join(cfg.Chart, s)) {
				secrets = append(secrets, s)
			}
		}
		out, err := deploy.Render(context.Background(), deploy.Chart{Dir: cfg.Chart, Values: cfg.Values, Secrets: secrets})
		if err != nil {
			return err
		}
		rendered, source = bytes.NewReader(out), cfg.Chart
	case cfg.Rendered == "-":
		rendered = os.Stdin
	default:
		f, err := os.Open(cfg.Rendered)
		if err != nil {
			return err
		}
		defer f.Close()
		rendered = f
	}
	d, err := deploy.Read(rendered, cfg.Environment, source)
	if err != nil {
		return err
	}
	d.Codegraph = buildVersion()

	// Match workloads to repositories (inventory) and analysed services (run).
	repos, analysed, languages := deploy.Repos{}, deploy.Analysed{}, map[string][]string{}
	var services []string
	var ms []*model.Manifest
	if cfg.Run != "" {
		if ms, err = readRun(cfg.Run); err != nil {
			return err
		}
		for _, m := range ms {
			analysed[m.Module] = append(analysed[m.Module], m.Service)
			services = append(services, m.Service)
		}
		sort.Strings(services)
	}
	if cfg.Inventory != "" {
		var inv inventory.Inventory
		data, err := os.ReadFile(cfg.Inventory)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, &inv); err != nil {
			return fmt.Errorf("%s: %w", cfg.Inventory, err)
		}
		skip := regexp.MustCompile(config.DefaultSkipMains)
		for _, r := range inv.Repos {
			info := deploy.Repo{LastCommit: r.LastCommit}
			for _, im := range r.Images {
				info.Images = append(info.Images, im.Name)
			}
			for _, m := range r.Modules {
				info.Modules = append(info.Modules, m.Path)
				for _, dir := range m.Mains {
					if m.Dir == "." && !skip.MatchString(dir) {
						info.Mains++
					}
				}
			}
			repos[r.Name], languages[r.Name] = info, r.Languages
		}
	} else {
		for module, services := range analysed {
			name := path.Base(strings.TrimSuffix(module, "/v2"))
			info := repos[name]
			info.Modules = append(info.Modules, module)
			info.Mains += len(services)
			repos[name] = info
		}
	}
	d.Match(repos, analysed)
	var declared []deploy.Declared
	for _, m := range ms {
		if m.Declared != nil {
			declared = append(declared, deploy.Declared{Service: m.Service, Repo: m.Declared.Repository, Workloads: m.Declared.Workloads})
		}
	}
	d.Declare(declared)
	if len(ms) > 0 {
		reads := map[string][]string{}
		for _, m := range ms {
			if m.Settings != nil {
				reads[m.Service] = m.Settings
			}
		}
		d.Network = d.Check(link.Link(ms).Links, reads)
	}

	if err := os.MkdirAll(cfg.Out, 0o755); err != nil {
		return err
	}
	if err := manifest.WriteJSON(filepath.Join(cfg.Out, "deploy.json"), d); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(cfg.Out, "deploy.md"), []byte(deploy.Report(d, services, languages)), 0o644); err != nil {
		return err
	}
	matched := 0
	for i := range d.Workloads {
		for _, c := range d.Workloads[i].Mains() {
			if c.Service != "" {
				matched++
			}
		}
	}
	logf("deploy: %s: %d workloads (%d main containers run an analysed service), %d services, %d routes -> %s",
		d.Environment, len(d.Workloads), matched, len(d.Services), len(d.Routes), cfg.Out)
	return nil
}

// ---- mcp ----------------------------------------------------------------------

// runMCP serves the run's answers over MCP on stdin and stdout until the
// client disconnects. Nothing but the protocol may reach stdout; logs go to
// stderr.
func runMCP(args []string) error {
	cfg, err := config.ParseMCP(args)
	if err != nil {
		return err
	}
	if cfg.Run, cfg.Explorer, err = queryRun(cfg.Config, cfg.Profile, cfg.Run, cfg.Explorer); err != nil {
		return err
	}
	dir, err := filepath.Abs(cfg.Run)
	if err != nil {
		return err
	}
	logf("mcp: answering from %s", dir)
	if ms, _ := filepath.Glob(filepath.Join(dir, "*", "manifest.json")); len(ms) == 0 && !bundle.IsBundle(dir) {
		// Clients such as Claude Desktop start servers outside the repository, so a relative -run misses.
		logf("mcp: no run in %s yet: answers will fail until make graph writes one there, or -run names the run's absolute path", dir)
	}
	return mcpserver.New(dir, buildVersion(), cfg.Explorer, cfg.Inventory).Run(context.Background(), &mcp.StdioTransport{})
}

// queryRun resolves the run (and the explorer) query and mcp read when -run
// (-explorer) is not given: from the profile or the configuration file.
func queryRun(configFlag, profileFlag, run, explorer string) (string, string, error) {
	if run != "" && explorer != "" {
		return run, explorer, nil
	}
	cf, err := configfile.Find(configFlag)
	if err != nil {
		return "", "", err
	}
	if explorer == "" && cf != nil {
		explorer = cf.Query.Explorer
	}
	if run == "" {
		if run, _, err = pipeline.QueryRun(cf, profileFlag); err != nil {
			return "", "", err
		}
	}
	return run, explorer, nil
}

// version is set by release builds (make release: -ldflags "-X main.version=v1.2.0").
var version string

// buildVersion is the release build's version, else the module version go
// install recorded (codegraph@v1.2.0), else "devel".
func buildVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "devel"
}

// commandRoots lists the CLI commands detected (transport cli, role command)
// and the functions they run, for the bootstrap trace, by name.
func commandRoots(dets []transport.Detection) []entrypoint.Command {
	var out []entrypoint.Command
	seen := map[string]bool{}
	for _, d := range dets {
		ep := d.Endpoint
		if ep.Transport != "cli" || ep.Role != "command" || len(d.Roots) == 0 || len(ep.Keys) == 0 || seen[ep.Keys[0]] {
			continue
		}
		seen[ep.Keys[0]] = true
		out = append(out, entrypoint.Command{Name: ep.Keys[0], Root: d.Roots[0].Func})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// readRun reads the service manifests of a run directory.
// ---- bundle -------------------------------------------------------------------

// runBundle packs a run (or a bundle, to change its options) into one file.
func runBundle(args []string) error {
	cfg, err := config.ParseBundle(args)
	if err != nil {
		return err
	}
	inv := cfg.Inventory
	if inv == "none" {
		inv = ""
	}
	r, err := bundle.Load(cfg.Run, inv)
	if err != nil {
		return err
	}
	if cfg.Inventory == "none" {
		r.Inventory = nil
	}
	compression := cfg.Compression
	if compression == "" {
		compression = bundle.Compression(cfg.Out)
	}
	out := cfg.Out
	if out == "" {
		base := strings.TrimRight(filepath.Clean(cfg.Run), string(filepath.Separator))
		if bundle.IsBundle(cfg.Run) {
			base = strings.TrimSuffix(strings.TrimSuffix(base, ".gz"), ".zst")
			base = strings.TrimSuffix(strings.TrimSuffix(base, ".tar"), ".tgz") + ".repacked"
		}
		out = base + ".tar.gz"
		if compression == bundle.Zstd {
			out = base + ".tar.zst"
		}
	}
	if abs, _ := filepath.Abs(out); abs != "" {
		if in, _ := filepath.Abs(cfg.Run); in == abs {
			return fmt.Errorf("bundle: -out %s would overwrite the bundle it reads", out)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(out), "."+filepath.Base(out)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	meta, err := bundle.Write(tmp, r, bundle.Options{
		Compression: compression, NoSettings: cfg.NoSettings, NoPositions: cfg.NoPositions, Version: buildVersion(),
	})
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), out); err != nil { // whole, or not at all: an MCP server may be reading it
		return err
	}
	st, err := os.Stat(out)
	if err != nil {
		return err
	}
	what := count(meta.Services, "service", "services")
	if meta.Libraries > 0 {
		what += ", " + count(meta.Libraries, "library", "libraries")
	}
	if meta.Environment != "" {
		what += ", the " + meta.Environment + " deployment"
	}
	if meta.Inventory {
		what += ", the inventory"
	}
	var left []string
	if meta.NoSettings {
		left = append(left, "configured values")
	}
	if meta.NoPositions {
		left = append(left, "file references")
	}
	if len(left) > 0 {
		what += "; left out: " + strings.Join(left, ", ")
	}
	size := fmt.Sprintf("%.1f MB", float64(st.Size())/(1<<20))
	if st.Size() < 1<<20 {
		size = fmt.Sprintf("%d KB", (st.Size()+1023)/1024)
	}
	logf("bundle: %s (%s, %s): %s", out, compression, size, what)
	return nil
}

// readRun reads the manifests of a run directory or a bundle.
func readRun(path string) ([]*model.Manifest, error) {
	r, err := bundle.Load(path, "")
	if err != nil {
		return nil, err
	}
	return r.Manifests, nil
}

// repoRules returns the rule files that apply to one repository only: its own
// codegraph-rules.json, and <dir>/<module name>.json (module path without a /vN
// suffix) for rules kept outside the repository.
func repoRules(repo, dir string) []string {
	var files []string
	if own := filepath.Join(repo, "codegraph-rules.json"); fileExists(own) {
		files = append(files, own)
	}
	if dir == "" {
		return files
	}
	if module := modulePath(filepath.Join(repo, "go.mod")); module != "" {
		if p := filepath.Join(dir, defaultName(module, module)+".json"); fileExists(p) {
			files = append(files, p)
		}
	}
	return files
}

// modulePath reads the module path from a go.mod file ("" if it has none).
func modulePath(gomod string) string {
	data, err := os.ReadFile(gomod)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`)
		}
	}
	return ""
}

// findMains reports whether repo has main packages under cmd/ and elsewhere
// (inventory.Mains: package clauses only, without loading anything).
func findMains(repo string) (inCmd, elsewhere bool) {
	for _, dir := range inventory.Mains(repo) {
		if dir == "cmd" || strings.HasPrefix(dir, "cmd/") {
			inCmd = true
		} else {
			elsewhere = true
		}
	}
	return inCmd, elsewhere
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// analyzeRepoSafely turns a panic while analysing an unfamiliar repository
// into an error, so the rest of the fleet still runs.
func analyzeRepoSafely(cfg *config.Analyze, rules []transport.Rule) (written []string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return analyzeRepo(cfg, rules)
}

func formatList(formats map[string]bool) string {
	var out []string
	for _, f := range []string{"json", "mermaid", "dot", "md", "html", "agents"} {
		if formats[f] {
			out = append(out, f)
		}
	}
	return strings.Join(out, ",")
}

// ---- helpers ------------------------------------------------------------------

func writeTexts(dir string, formats map[string]bool, names map[string]string, gen map[string]func() string) error {
	for _, f := range []string{"mermaid", "dot", "md"} {
		if !formats[f] {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, names[f]), []byte(gen[f]()), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "codegraph: "+format+"\n", args...)
}

// count writes n with the singular or plural noun.
func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
