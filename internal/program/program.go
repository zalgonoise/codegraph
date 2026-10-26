// Package program builds the SSA representation of the loaded packages and
// finds the `package main` entrypoints among them.
package program

import (
	"errors"
	"sort"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"

	"github.com/zalgonoise/codegraph/internal/loader"
)

// Program is the SSA program plus the main packages to analyse.
type Program struct {
	SSA      *ssa.Program
	Mains    []*ssa.Package
	Packages []*ssa.Package // the packages the patterns matched, sorted by path
}

// Build creates and builds SSA for every loaded package (generics
// instantiated). Unless library is set, the patterns must match a main package.
func Build(ws *loader.Workspace, library bool) (*Program, error) {
	prog, pkgs := ssautil.AllPackages(ws.Initial, ssa.InstantiateGenerics)
	prog.Build()

	var built []*ssa.Package
	for _, p := range pkgs {
		if p != nil {
			built = append(built, p)
		}
	}
	sort.Slice(built, func(i, j int) bool { return built[i].Pkg.Path() < built[j].Pkg.Path() })
	mains := ssautil.MainPackages(built)
	if len(mains) == 0 && !library {
		return nil, errors.New("no main package among the loaded patterns; point -entry at a `package main` (e.g. ./cmd/server), or analyse a library with -library")
	}
	sort.Slice(mains, func(i, j int) bool { return mains[i].Pkg.Path() < mains[j].Pkg.Path() })
	return &Program{SSA: prog, Mains: mains, Packages: built}, nil
}
