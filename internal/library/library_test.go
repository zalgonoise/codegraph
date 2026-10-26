package library

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"

	"github.com/zalgonoise/codegraph/internal/symbols"
)

const generic = `package fts

type SQLType interface{ ~int | ~string }

// Indexer is implemented by the exported Index and the unexported logged.
type Indexer[K SQLType, V SQLType] interface {
	Search(v V) []K
	Close() error
}

type Index[K SQLType, V SQLType] struct{ rows map[V][]K }

func New[K SQLType, V SQLType]() Indexer[K, V] { return &Index[K, V]{} }

func (i *Index[K, V]) Search(v V) []K { return i.rows[v] }
func (i *Index[K, V]) Close() error  { return nil }

type logged[K SQLType, V SQLType] struct{ next Indexer[K, V] }

func WithLogs[K SQLType, V SQLType](next Indexer[K, V]) Indexer[K, V] { return logged[K, V]{next} }

func (l logged[K, V]) Search(v V) []K { return l.next.Search(v) }
func (l logged[K, V]) Close() error  { return l.next.Close() }

type Plain struct{}

func (Plain) Name() string { return "plain" }
func (*Plain) hidden()     {}

func helper() {}

// An alias declares no API: Indexer's methods are listed once, under Indexer.
type Alias = Indexer[int, string]
`

func build(t *testing.T, src string) *ssa.Package {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fts.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	pkg, _, err := ssautil.BuildPackage(&types.Config{Importer: importer.Default()}, fset,
		types.NewPackage("example.com/fts", "fts"), []*ast.File{f}, ssa.InstantiateGenerics)
	if err != nil {
		t.Fatal(err)
	}
	return pkg
}

// A generic API: functions and methods of exported types, interface methods
// whose implementations are matched by name (their type parameters differ),
// generic roots counted for the CHA fallback.
func TestAPI(t *testing.T) {
	pkg := build(t, generic)
	pkgs := []*ssa.Package{pkg}
	module := "example.com/fts"
	api := API(pkgs, module, nil, true)
	var keys []string
	for _, s := range api {
		keys = append(keys, s.Key+" "+s.Kind)
	}
	want := []string{
		"fts.Index.Close method", "fts.Index.Search method", "fts.Indexer.Close interface", "fts.Indexer.Search interface",
		"fts.New func", "fts.Plain.Name method", "fts.WithLogs func",
	}
	if !slices.Equal(keys, want) {
		t.Fatalf("API:\n got %v\nwant %v", keys, want)
	}
	modulePkgs := map[string]bool{module: true}
	if n := Generic(Roots(api, pkgs, modulePkgs)); n != 4 { // New, WithLogs, Index.Search, Index.Close
		t.Errorf("generic roots = %d, want 4", n)
	}
	Implement(api, pkg.Prog, ModuleTypes(pkgs, modulePkgs), modulePkgs)
	for _, s := range api {
		if s.Key != "fts.Indexer.Search" {
			continue
		}
		var impl []string
		for _, fn := range s.Funcs {
			impl = append(impl, symbols.FuncLabel(fn))
		}
		if got := strings.Join(impl, ", "); got != "(*fts.Index[K, V]).Search, fts.logged[K, V].Search" {
			t.Errorf("Indexer.Search runs %s", got)
		}
	}
	dets := Detections(api, pkg.Prog.Fset, "", module)
	if len(dets) != len(api) || dets[3].Endpoint.Transport != Transport || dets[3].Endpoint.Keys[0] != "fts.Indexer.Search" ||
		len(dets[3].Roots) != 2 || !strings.HasPrefix(dets[3].Endpoint.Handler, "fts.Indexer.Search → ") {
		t.Errorf("Detections[3] = %+v", dets[3])
	}
}

func TestPublic(t *testing.T) {
	for rel, want := range map[string]bool{
		"": true, "log": true, "internal": false, "a/internal/b": false, "examples/x": false, "cmd/tool": false, "testdata": false,
		"internals": true,
	} {
		if Public(rel) != want {
			t.Errorf("Public(%q) = %v", rel, !want)
		}
	}
	for rel, want := range map[string]bool{"internal": true, "a/internal/b": true, "cmd/x/internal": false, "log": false} {
		if Internal(rel) != want {
			t.Errorf("Internal(%q) = %v", rel, !want)
		}
	}
}
