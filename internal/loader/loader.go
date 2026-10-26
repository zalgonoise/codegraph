// Package loader loads the target repository with golang.org/x/tools/go/packages
// (type-checked, with syntax) and records which packages belong to the
// analysed module(s) versus third-party modules and the standard library.
package loader

import (
	"fmt"
	"go/token"
	"path"
	"path/filepath"
	"regexp"

	"golang.org/x/tools/go/packages"
)

// StdModule is the pseudo module path used for standard-library packages.
const StdModule = "std"

// ssautil.AllPackages needs full syntax and type info for every dependency.
const loadMode = packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
	packages.NeedImports | packages.NeedDeps | packages.NeedTypes | packages.NeedTypesSizes |
	packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedModule

// Options control what gets loaded.
type Options struct {
	Dir        string   // repository / module root
	Patterns   []string // package patterns relative to Dir, e.g. ./cmd/...
	Tests      bool
	BuildFlags []string
	// Include lists module path patterns (path.Match, e.g. example.com/*-sdk-go)
	// whose packages are analysed like the main module's: SDKs that wrap the
	// actual gRPC calls a service makes.
	Include []string
}

// Workspace is the loaded program plus module bookkeeping.
type Workspace struct {
	Dir        string
	Fset       *token.FileSet
	Initial    []*packages.Package
	All        map[string]*packages.Package // every package in the import graph
	ModulePkgs map[string]bool              // packages of the main module(s) and of -include-modules
	PkgModule  map[string]string            // package path -> module path ("std" for stdlib)
	ModulePath string
}

// Load type-checks the requested packages and all of their dependencies.
func Load(opts Options) (*Workspace, error) {
	dir, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	cfg := &packages.Config{
		Mode:       loadMode,
		Dir:        dir,
		Tests:      opts.Tests,
		BuildFlags: opts.BuildFlags,
		Fset:       fset,
	}
	pkgs, err := packages.Load(cfg, opts.Patterns...)
	if err != nil {
		return nil, fmt.Errorf("load packages: %w", err)
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no packages matched %v in %s", opts.Patterns, dir)
	}
	if n := packages.PrintErrors(pkgs); n > 0 {
		return nil, fmt.Errorf("%d error(s) while loading packages; SSA needs well-typed code (fix the build or pass -build-tags)", n)
	}

	ws := &Workspace{
		Dir:        dir,
		Fset:       fset,
		Initial:    pkgs,
		All:        map[string]*packages.Package{},
		ModulePkgs: map[string]bool{},
		PkgModule:  map[string]string{},
	}
	for _, p := range pkgs {
		if p.Module != nil {
			ws.ModulePath = p.Module.Path
			break
		}
	}
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		ws.All[p.PkgPath] = p
		if p.Module == nil {
			ws.PkgModule[p.PkgPath] = StdModule
			return
		}
		ws.PkgModule[p.PkgPath] = p.Module.Path
		if p.Module.Main || included(opts.Include, p.Module.Path) { // Main: every module of a go.work too
			ws.ModulePkgs[p.PkgPath] = true
		}
	})
	return ws, nil
}

var majorSuffix = regexp.MustCompile(`/v[0-9]+$`)

// included reports whether a module matches one of the patterns, with or
// without its major-version suffix.
func included(patterns []string, module string) bool {
	for _, p := range patterns {
		for _, m := range []string{module, majorSuffix.ReplaceAllString(module, "")} {
			if ok, _ := path.Match(p, m); ok {
				return true
			}
		}
	}
	return false
}
