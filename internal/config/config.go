// Package config parses the command-line flags of the codegraph subcommands.
package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Analyze configures `codegraph analyze`.
type Analyze struct {
	Dir            string
	Entries        []string
	Algorithm      string
	Out            string
	Formats        map[string]bool
	Service        string
	Aliases        []string
	Tags           map[string]string
	RuleFiles      []string
	NoDefaultRules bool
	EntryDepth     int
	ResolveDepth   int
	Journeys       bool
	JourneyDepth   int
	Tests          bool
	Libraries      bool
	IncludeStd     bool
	BuildTags      string
	SkipMains      *regexp.Regexp // main packages (path relative to their module) not analysed
	Include        []string       // module patterns analysed like the main module (SDKs)
	Library        bool           // analyse the module through its exported API instead of its mains
	Generated      bool           // with Library: the API includes symbols declared in generated files
	Internal       bool           // with Library: the exported symbols of internal packages too (role internal)
	MainJourney    bool           // a journey of each main's own work, up to the handlers it registers
}

// BuildFlags returns the go build flags for the loader.
func (a *Analyze) BuildFlags() []string {
	if a.BuildTags == "" {
		return nil
	}
	return []string{"-tags=" + a.BuildTags}
}

// Link configures `codegraph link`.
type Link struct {
	Manifests []string
	Out       string
	Formats   map[string]bool
	Deploy    string // a deploy.json (codegraph deploy) whose environment the explorer shows
}

// Query configures `codegraph query`.
type Query struct {
	Run       string
	JSON      bool
	Depth     int
	Diagram   bool
	Explorer  string   // the explorer page answers link to ("" for the run's own, "none" for none)
	Inventory string   // the inventory image questions read ("" for <run>/../inventory)
	Args      []string // the command and its arguments
}

// ParseQuery parses `codegraph query` flags; commands lists the questions.
func ParseQuery(args []string, commands string) (*Query, error) {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	c := &Query{}
	fs.StringVar(&c.Run, "run", "tmp/graph", "run directory: <dir>/<service>/manifest.json, as make graph writes it")
	fs.BoolVar(&c.JSON, "json", false, "answer in JSON instead of Markdown")
	fs.IntVar(&c.Depth, "depth", 2, "journey: how many services beyond its own to follow (0: its own calls only)")
	fs.BoolVar(&c.Diagram, "diagram", false, "add a Mermaid diagram (service, journey, who, path, impact); JSON answers become {answer, diagram, explorer}")
	fs.StringVar(&c.Explorer, "explorer", "", explorerUsage)
	fs.StringVar(&c.Inventory, "inventory", "", inventoryUsage)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: codegraph query [flags] <command> [args]\n\ncommands:\n%s\n\nflags:\n", commands)
		fs.PrintDefaults()
	}
	// Flags may come after the command too (codegraph query journey svc key -depth 4).
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		name, _, hasValue := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
		switch {
		case !strings.HasPrefix(args[i], "-"):
			rest = append(rest, args[i])
		case name == "json" || name == "diagram" || name == "h" || name == "help":
			flags = append(flags, args[i])
		case (name == "run" || name == "depth" || name == "explorer" || name == "inventory") && !hasValue && i+1 < len(args):
			flags = append(flags, args[i], args[i+1])
			i++
		default:
			flags = append(flags, args[i])
		}
	}
	if err := fs.Parse(flags); err != nil {
		return nil, err
	}
	if c.Args = rest; len(c.Args) == 0 {
		fs.Usage()
		return nil, errors.New("query: no command")
	}
	return c, nil
}

// Inventory configures `codegraph inventory`.
type Inventory struct {
	Root  string // the directory holding every repository (<repo>.git)
	Out   string
	Focus string // a file listing the repositories analysed in full, one name per line
	Run   string // a run including the focus repositories: their services' modules, contracts and topics refine the frontier
	Rings int    // frontier rings: each around the focus and the rings before it

	Catalogues    []string // repositories whose proto/ tree defines the contracts
	CataloguesSet bool     // -catalogues was given: it overrides the profile's
	Profile       string   // the organisation's profile directory ("" for $CODEGRAPH_PROFILE)
}

// ParseInventory parses `codegraph inventory` flags.
func ParseInventory(args []string) (*Inventory, error) {
	fs := flag.NewFlagSet("inventory", flag.ContinueOnError)
	c := &Inventory{}
	fs.StringVar(&c.Root, "root", "", "directory holding every repository (<repo>.git or <repo>)")
	fs.StringVar(&c.Out, "out", "tmp/inventory", "output directory: inventory.json, inventory.md, contracts.md, and the frontier with -focus")
	catalogues := fs.String("catalogues", "", "comma-separated repositories whose proto/ tree defines the gRPC contracts (contracts.md); the profile's when not given, and none when given empty")
	fs.StringVar(&c.Profile, "profile", "", "the organisation's profile directory, whose profile.json gives its naming conventions and catalogues (default $CODEGRAPH_PROFILE)")
	fs.StringVar(&c.Focus, "focus", "", "file listing the repositories analysed in full (one name per line, # comments): the frontier is computed around them")
	fs.StringVar(&c.Run, "run", "", "a run (make graph's): who serves and calls each contract (contracts.md); with -focus, it should include those repositories, and only their services give the frontier's hints: modules, served and missing contracts, one-sided topics")
	fs.IntVar(&c.Rings, "rings", 1, "with -focus: frontier rings; ring n is the frontier around the focus and rings 1 to n-1 (its hints need a run that analysed them)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: codegraph inventory -root <dir> [-out dir] [-focus file [-run dir] [-rings n]]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if c.Root == "" {
		return nil, errors.New("inventory: -root is required")
	}
	fs.Visit(func(f *flag.Flag) { c.CataloguesSet = c.CataloguesSet || f.Name == "catalogues" })
	for _, name := range strings.Split(*catalogues, ",") {
		if name = strings.TrimSpace(name); name != "" {
			c.Catalogues = append(c.Catalogues, name)
		}
	}
	if c.Rings < 1 {
		return nil, errors.New("inventory: -rings must be at least 1")
	}
	return c, nil
}

// explorerUsage describes -explorer, for query and mcp.
const explorerUsage = "the explorer page answers link to: empty for the run's own <run>/_system/system.html (a file:// URL), " +
	"\"none\" for no links, or the page's URL when it is served elsewhere"

// Bundle configures `codegraph bundle`.
type Bundle struct {
	Run         string // a run directory, or a bundle to pack again
	Out         string // the bundle file ("" for <run>.tar.gz, or .tar.zst with zstd)
	Compression string // gzip or zstd ("" for what Out's ending says)
	Inventory   string // an inventory.json instead of the run's; "none" for none
	NoSettings  bool
	NoPositions bool
}

// ParseBundle parses `codegraph bundle` flags.
func ParseBundle(args []string) (*Bundle, error) {
	fs := flag.NewFlagSet("bundle", flag.ContinueOnError)
	c := &Bundle{}
	fs.StringVar(&c.Run, "run", "tmp/graph", "the run to pack: a run directory, or a bundle to pack again (with other options)")
	fs.StringVar(&c.Out, "out", "", "the bundle to write; default <run>.tar.gz (<run>.tar.zst with -compress zstd)")
	fs.StringVar(&c.Compression, "compress", "", "gzip or zstd; default: what -out's ending says (.zst: zstd), else gzip")
	fs.StringVar(&c.Inventory, "inventory", "", "the inventory to include (an inventory.json, or its directory); default the run's own "+
		"(<run>/inventory.json, else <run>/../inventory); \"none\" for none")
	fs.BoolVar(&c.NoSettings, "no-settings", false, "leave out the values the environment configures: the deployment's variables keep their names, the address check's settings and mocks their names only")
	fs.BoolVar(&c.NoPositions, "no-positions", false, "leave out every file reference: file:line positions in the code, declaration and Dockerfile names, the chart's directory")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: codegraph bundle [-run dir|bundle] [-out file] [-compress gzip|zstd] [-inventory file|none] [-no-settings] [-no-positions]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("bundle: unexpected arguments %v", fs.Args())
	}
	switch c.Compression {
	case "", "gzip", "zstd":
	default:
		return nil, fmt.Errorf("bundle: -compress must be gzip or zstd, got %q", c.Compression)
	}
	return c, nil
}

// inventoryUsage describes -inventory, for query and mcp.
const inventoryUsage = "the inventory image questions read (an inventory.json, or its directory); empty for the one next to the run, <run>/../inventory"

// MCP configures `codegraph mcp`.
type MCP struct {
	Run       string
	Explorer  string // the explorer page answers link to ("" for the run's own, "none" for none)
	Inventory string // the inventory image questions read ("" for <run>/../inventory)
}

// ParseMCP parses `codegraph mcp` flags.
func ParseMCP(args []string) (*MCP, error) {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	c := &MCP{}
	fs.StringVar(&c.Run, "run", "tmp/graph", "run directory to answer from: <dir>/<service>/manifest.json, as make graph writes it")
	fs.StringVar(&c.Explorer, "explorer", "", explorerUsage)
	fs.StringVar(&c.Inventory, "inventory", "", inventoryUsage)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: codegraph mcp [-run dir] [-explorer url|none]   (an MCP server on stdin and stdout, for Claude Code and Claude Desktop)")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return c, nil
}

// Deploy configures `codegraph deploy`.
type Deploy struct {
	Chart       string   // the environment's chart directory, rendered with helm
	Values      []string // its values files, in order (relative to the chart)
	Secrets     []string // its secret values files: only their keys are used
	Rendered    string   // or already rendered objects ("-" for stdin)
	Environment string
	Inventory   string // inventory.json, to match images to repositories
	Run         string // a run, to match workloads to the services analysed
	Out         string
}

// ParseDeploy parses `codegraph deploy` flags.
func ParseDeploy(args []string) (*Deploy, error) {
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	c := &Deploy{}
	var values, secrets string
	fs.StringVar(&c.Chart, "chart", "", "the environment's Helm chart directory, rendered with `helm template` (offline; nothing is written to it)")
	fs.StringVar(&values, "values", "values.yaml,overrides.yaml", "comma-separated values files, in order, relative to the chart")
	fs.StringVar(&secrets, "secrets", "secrets.yaml,secrets.overrides.yaml", "comma-separated secret values files: masked to their keys, values never read (missing files are skipped)")
	fs.StringVar(&c.Rendered, "rendered", "", "instead of -chart: rendered Kubernetes objects (a file, or - for stdin)")
	fs.StringVar(&c.Environment, "env", "production", "the environment's name, for the reports")
	fs.StringVar(&c.Inventory, "inventory", "", "inventory.json (codegraph inventory), to match images to repositories")
	fs.StringVar(&c.Run, "run", "", "a run (make graph's), to match workloads to the services analysed")
	fs.StringVar(&c.Out, "out", "tmp/deploy", "output directory: deploy.json and deploy.md")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: codegraph deploy (-chart dir | -rendered file) [-env name] [-inventory file] [-run dir] [-out dir]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if (c.Chart == "") == (c.Rendered == "") {
		return nil, errors.New("deploy: give one of -chart or -rendered")
	}
	c.Values, c.Secrets = splitList(values), splitList(secrets)
	return c, nil
}

func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// Diff configures `codegraph diff`.
type Diff struct {
	Base, Head  string // run directories: <dir>/*/manifest.json
	Out         string // report file; stdout when empty
	FailOnLinks bool
}

// ParseDiff parses `codegraph diff` flags.
func ParseDiff(args []string) (*Diff, error) {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	c := &Diff{}
	fs.StringVar(&c.Base, "base", "", "directory of the base run (<dir>/*/manifest.json, as analyze and fleet write them)")
	fs.StringVar(&c.Head, "head", "", "directory of the head run")
	fs.StringVar(&c.Out, "out", "", "write the Markdown report to this file (default: stdout)")
	fs.BoolVar(&c.FailOnLinks, "fail-on-links", false, "exit with status 1 when cross-service links are added or removed")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: codegraph diff -base <run dir> -head <run dir> [flags]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if c.Base == "" || c.Head == "" {
		return nil, errors.New("diff: -base and -head are required")
	}
	return c, nil
}

// ParseAnalyze parses `codegraph analyze` flags.
func ParseAnalyze(args []string) (*Analyze, error) {
	fs := flag.NewFlagSet("analyze", flag.ContinueOnError)
	c := &Analyze{Tags: map[string]string{}}
	var aliases listFlag
	fs.StringVar(&c.Dir, "dir", ".", "root of the Go module or workspace to analyse")
	fs.StringVar(&c.Out, "out", "codegraph-out", "output directory; one sub-directory per service")
	fs.StringVar(&c.Service, "service", "", "service name (only when a single main package is analysed)")
	fs.Var(&aliases, "alias", "extra host name callers use for this service (repeatable)")
	fs.Var(kvFlag(c.Tags), "tag", "service tag key=value (repeatable)")
	fs.BoolVar(&c.Library, "library", false, "analyse the module as a library: one journey per exported function and method of its public packages (not internal), instead of its main packages")
	finish := analysisFlags(fs, c, "./...")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments %v (use -entry for package patterns)", fs.Args())
	}
	c.Aliases = aliases
	if err := finish(); err != nil {
		return nil, err
	}
	return c, nil
}

// Fleet configures `codegraph fleet`: analyse several repositories with the same
// settings, then link them.
type Fleet struct {
	Analyze   Analyze  // template; Dir and Entries are set per repository
	Repos     []string // repository roots (directories with a go.mod)
	Entries   []string // explicit -entry patterns; empty = ./cmd/... when present, else ./...
	RepoRules string   // directory of per-repository rule files, <module name>.json
	Declared  string   // directory of services described by hand, <service>.json
	Libraries bool     // analyse repositories without a main as libraries instead of skipping them
	API       bool     // also analyse the API of repositories with mains, as <name>-api (implies Libraries)
}

// DefaultSkipMains matches main packages that are tools or demos rather than services.
const DefaultSkipMains = `(^|/)(migrat\w*|makemigration\w*|rollback|seed\w*)$|(^|/)(demos?|examples?)(/|$)`

// ParseFleet parses `codegraph fleet` flags; positional arguments are repository
// directories (globs are expanded; directories without a go.mod are skipped).
func ParseFleet(args []string) (*Fleet, error) {
	fs := flag.NewFlagSet("fleet", flag.ContinueOnError)
	f := &Fleet{Analyze: Analyze{Tags: map[string]string{}}}
	c := &f.Analyze
	fs.StringVar(&c.Out, "out", "codegraph-out", "output directory; one sub-directory per service, the system view in _system")
	skip := fs.String("skip-mains", DefaultSkipMains, "regexp of main packages (path relative to the module) to skip; empty skips none")
	fs.StringVar(&f.RepoRules, "repo-rules", "", "directory of per-repository rule files named <module name>.json (a repository's own codegraph-rules.json is always used)")
	fs.StringVar(&f.Declared, "declared", "", "directory of services described by hand, <service>.json (services codegraph cannot analyse, e.g. Python ones), linked with the analysed ones")
	fs.BoolVar(&f.Libraries, "libraries", false, "analyse repositories without a main as libraries (one journey per exported symbol, never linked) instead of skipping them")
	fs.BoolVar(&f.API, "api", false, "analyse every repository's API: those with mains also as a library named <name>-api, those without as libraries (implies -libraries)")
	finish := analysisFlags(fs, c, "")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: codegraph fleet [flags] repo-dir...")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if err := finish(); err != nil {
		return nil, err
	}
	f.Entries, c.Entries = c.Entries, nil
	f.Libraries = f.Libraries || f.API
	c.Formats["json"] = true // linking reads the manifests
	if *skip != "" {
		re, err := regexp.Compile(*skip)
		if err != nil {
			return nil, fmt.Errorf("-skip-mains: %w", err)
		}
		c.SkipMains = re
	}
	for _, a := range fs.Args() {
		matches, err := filepath.Glob(a)
		if err != nil {
			return nil, err
		}
		if len(matches) == 0 {
			matches = []string{a}
		}
		for _, m := range matches {
			if st, err := os.Stat(filepath.Join(m, "go.mod")); err == nil && !st.IsDir() {
				f.Repos = append(f.Repos, m)
			}
		}
	}
	if len(f.Repos) == 0 {
		return nil, errors.New("fleet: no repository with a go.mod given")
	}
	return f, nil
}

// analysisFlags registers the flags shared by analyze and fleet; the returned
// func validates them after parsing.
func analysisFlags(fs *flag.FlagSet, c *Analyze, defaultEntry string) func() error {
	var entries, rules, include listFlag
	help := "package pattern of the service main package(s), relative to the repository (repeatable; default ./...)"
	if defaultEntry == "" {
		help = "package pattern of the service main package(s) in every repository (repeatable; default ./cmd/... when present, else ./...)"
	}
	fs.Var(&entries, "entry", help)
	fs.StringVar(&c.Algorithm, "algo", "vta", "call graph algorithm: cha | rta | vta")
	formats := fs.String("format", "json,mermaid,dot,md,html,agents", "comma-separated outputs: json, mermaid, dot, md, html (link: the interactive system.html), agents (link: AGENTS.md and context.md)")
	fs.Var(&rules, "rules", "JSON file with extra transport rules (repeatable)")
	fs.BoolVar(&c.NoDefaultRules, "no-default-rules", false, "disable the built-in transport rules")
	fs.Var(&include, "include-modules", "module path pattern analysed like the service's own code, e.g. example.com/*-sdk-go for SDKs wrapping gRPC calls (repeatable)")
	fs.IntVar(&c.EntryDepth, "entry-depth", 3, "how deep to follow module calls from main when tracing bootstrap")
	fs.IntVar(&c.ResolveDepth, "resolve-depth", 4, "interprocedural depth when resolving topics/routes/URLs")
	fs.BoolVar(&c.Journeys, "journeys", true, "compute what each inbound endpoint (route, RPC, consumer) reaches")
	fs.BoolVar(&c.MainJourney, "main-journey", true, "add a journey of each main's own work (transport main, key main): what main reaches up to the handlers it registers, such as a pipeline or a job that no endpoint starts")
	fs.IntVar(&c.JourneyDepth, "journey-depth", 0, "max call depth followed from a handler (0 = unlimited)")
	fs.BoolVar(&c.Tests, "tests", false, "include test packages")
	fs.BoolVar(&c.Libraries, "libs", false, "draw third-party libraries in the service diagram")
	fs.BoolVar(&c.IncludeStd, "std", false, "include the standard library in library dependencies")
	fs.StringVar(&c.BuildTags, "build-tags", "", "comma-separated build tags")
	fs.BoolVar(&c.Internal, "internal", true, "for a library (-library, fleet -libraries, -api): also the exported symbols of its internal packages, as endpoints of role internal, hidden by default in the explorer and the diagrams")
	fs.BoolVar(&c.Generated, "generated", false, "for a library (-library, fleet -libraries): include the symbols declared in generated files (\"Code generated ... DO NOT EDIT.\"), such as protobuf messages' methods")
	return func() error {
		switch c.Algorithm {
		case "cha", "rta", "vta":
		default:
			return fmt.Errorf("-algo must be cha, rta or vta, got %q", c.Algorithm)
		}
		c.Entries, c.RuleFiles, c.Include = entries, rules, include
		if len(c.Entries) == 0 && defaultEntry != "" {
			c.Entries = []string{defaultEntry}
		}
		var err error
		c.Formats, err = parseFormats(*formats)
		return err
	}
}

// ParseLink parses `codegraph link` flags; positional arguments are manifest files
// (globs are expanded here as well, for shells that do not).
func ParseLink(args []string) (*Link, error) {
	fs := flag.NewFlagSet("link", flag.ContinueOnError)
	c := &Link{}
	fs.StringVar(&c.Out, "out", "codegraph-out/_system", "output directory")
	formats := fs.String("format", "json,mermaid,dot,md,html,agents", "comma-separated outputs: json, mermaid, dot, md, html (the interactive system.html), agents (AGENTS.md and context.md)")
	fs.StringVar(&c.Deploy, "deploy", "", "a deploy.json (codegraph deploy): system.html shows what that environment runs of each service")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: codegraph link [flags] manifest.json...")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	for _, a := range fs.Args() {
		matches, err := filepath.Glob(a)
		if err != nil {
			return nil, err
		}
		if len(matches) == 0 {
			matches = []string{a}
		}
		c.Manifests = append(c.Manifests, matches...)
	}
	if len(c.Manifests) == 0 {
		return nil, errors.New("link: no manifest files given")
	}
	var err error
	if c.Formats, err = parseFormats(*formats); err != nil {
		return nil, err
	}
	return c, nil
}

func parseFormats(s string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, f := range strings.Split(s, ",") {
		switch f = strings.ToLower(strings.TrimSpace(f)); f {
		case "":
		case "json", "mermaid", "dot", "md", "html", "agents":
			out[f] = true
		case "mmd":
			out["mermaid"] = true
		default:
			return nil, fmt.Errorf("unknown format %q (want json, mermaid, dot, md, html, agents)", f)
		}
	}
	return out, nil
}

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

type kvFlag map[string]string

func (m kvFlag) String() string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		keys[i] = k + "=" + m[k]
	}
	return strings.Join(keys, ",")
}

func (m kvFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("expected key=value, got %q", v)
	}
	m[k] = val
	return nil
}
