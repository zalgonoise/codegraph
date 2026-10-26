package inventory

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// write creates files under dir: path -> content.
func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestScan(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "shop.git"), map[string]string{
		"go.mod": "module example.com/shop\n\ngo 1.24.2\n\nrequire (\n\texample.com/sdk v1.2.0\n\tgithub.com/x/y v0.1.0 // indirect\n)\n\n" +
			"replace example.com/sdk => ../sdk\n",
		"cmd/api/main.go":          "package main\n\nimport (\n\t\"fmt\"\n\t\"example.com/sdk/client\"\n\t\"example.com/shop/internal\"\n\t\"github.com/x/y\"\n)\n",
		"cmd/gen/main.go":          "//go:build ignore\n\npackage main\n",
		"internal/x.go":            "package internal\n",
		"internal/x_test.go":       "package internal\n\nimport \"example.com/sdk/testkit\"\n",
		"examples/demo/main.go":    "package main\n",
		"tools/go.mod":             "module example.com/shop/tools\n",
		"tools/main.go":            "package main\n",
		"web/package.json":         "{}",
		"Dockerfile":               "FROM scratch\n",
		"deep/down/below/setup.py": "",
	})
	write(t, filepath.Join(root, "sdk.git"), map[string]string{
		"go.mod":      "module example.com/sdk\n\ngo 1.22\n",
		"client/c.go": "package client\n",
	})
	write(t, filepath.Join(root, "docs.git"), map[string]string{"README.md": "docs\n"})

	inv := Scan([]string{filepath.Join(root, "shop.git"), filepath.Join(root, "sdk.git"), filepath.Join(root, "docs.git")}, Conventions{})
	if len(inv.Repos) != 3 || inv.Repos[0].Name != "docs" || inv.Repos[2].Name != "shop" {
		t.Fatalf("repos = %+v", inv.Repos)
	}
	docs, sdk, shop := inv.Repos[0], inv.Repos[1], inv.Repos[2]
	if len(docs.Languages) != 0 || docs.Languages == nil || docs.Service() {
		t.Errorf("docs: %+v", docs)
	}
	if sdk.Service() {
		t.Error("sdk has no main package")
	}
	if want := []string{"docker", "go", "javascript"}; !reflect.DeepEqual(shop.Languages, want) {
		t.Errorf("languages = %v, want %v (markers deeper than one level do not count)", shop.Languages, want)
	}
	if len(shop.Modules) != 2 || shop.Modules[0].Dir != "." || shop.Modules[1].Path != "example.com/shop/tools" {
		t.Fatalf("modules = %+v", shop.Modules)
	}
	m := shop.Modules[0]
	if m.Go != "1.24.2" || !reflect.DeepEqual(m.Requires, []Require{{"example.com/sdk", "v1.2.0"}}) ||
		!reflect.DeepEqual(m.Replaces, []string{"example.com/sdk => ../sdk"}) {
		t.Errorf("go.mod: %+v", m)
	}
	// The ignore-tagged generator, the example and the nested module's main do not count.
	if !reflect.DeepEqual(m.Mains, []string{"cmd/api"}) {
		t.Errorf("mains = %v", m.Mains)
	}
	// Only in-house packages of other modules: not the standard library, not
	// third-party modules, not its own packages, not test imports.
	if !reflect.DeepEqual(m.Imports, []string{"example.com/sdk/client"}) {
		t.Errorf("imports = %v", m.Imports)
	}
	if got := inv.Dependents("example.com/sdk"); !reflect.DeepEqual(got, []Require{{"shop", "v1.2.0"}}) {
		t.Errorf("dependents = %v", got)
	}
	if got := Mains(filepath.Join(root, "shop.git")); !reflect.DeepEqual(got, []string{"cmd/api"}) {
		t.Errorf("Mains = %v", got)
	}

	report := Report(inv)
	for _, want := range []string{"3 repositories. Go: 2 (1 build programs, 1 are libraries only)", "| `example.com/sdk` | sdk | 1 | v1.2.0 (1) |"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
}

// org is an inventory around a focus of "cart" (a service) and "bus" (a
// library whose nested module builds a tool).
func org() *Inventory {
	program := func(name, last string, imports ...string) Repo {
		return Repo{Name: name, LastCommit: last, Languages: []string{"go"}, Modules: []Module{{
			Path: "example.com/" + name, Dir: ".", Mains: []string{"cmd"}, Imports: imports}}}
	}
	return &Inventory{Repos: []Repo{
		program("cart", "2026-10-01"),
		{Name: "bus", Languages: []string{"go"}, Modules: []Module{{Path: "example.com/bus", Dir: "."},
			{Path: "example.com/bus/v2", Dir: "v2", Mains: []string{"cmd/create-topic"}}}},
		{Name: "cart-sdk-go", Languages: []string{"go"}, Modules: []Module{{Path: "example.com/cart-sdk-go", Dir: ".", Mains: []string{"cmd"}}}},
		program("caller", "2026-09-01", "example.com/cart-sdk-go/client", "example.com/bus/v2"),
		program("contract-user", "2026-09-01", "example.com/protos/contracts/shop/carts/v1"),
		program("old-caller", "2020-01-01", "example.com/cart/pkg/api"),
		program("bus-only", "2026-09-01", "example.com/bus/v2/data"),
		program("pricing", "2026-09-01"),
		program("pricing-client", "2026-09-01", "example.com/protos/contracts/catalog/pricing/v1"),
		{Name: "cart-test", Languages: []string{"go"}, LastCommit: "2026-01-01", Modules: []Module{{Path: "example.com/cart", Dir: ".", Mains: []string{"cmd"}}}},
		{Name: "profiles", Languages: []string{"python"}, LastCommit: "2026-09-01"},
	}, Conventions: Conventions{ClientLibrarySuffix: "-sdk-go", ContractImport: "/contracts/{package}"}}
}

// Without the conventions, neither client libraries nor contract imports
// say who calls the focus: only its own modules do.
func TestFrontierWithoutConventions(t *testing.T) {
	inv := org()
	inv.Conventions = Conventions{}
	h := Hints{Modules: []string{"example.com/cart"}, Served: map[string]string{"shop.carts.v1": "example.com/cart"}}
	var got []string
	for _, c := range inv.Frontier([]string{"cart", "bus"}, h).Candidates {
		got = append(got, c.Repo)
	}
	if strings.Join(got, ",") != "old-caller" {
		t.Errorf("candidates = %v", got) // not caller (through the client library) nor contract-user
	}
}

func TestFrontier(t *testing.T) {
	h := Hints{
		Modules: []string{"example.com/cart"},
		Served:  map[string]string{"shop.carts.v1": "example.com/cart"},
		Missing: []string{"profiles.customer.v1", "catalog.pricing.v1", "fraud.v1"},
		Topics:  []string{"audit.cart.create"},
	}
	f := org().Frontier([]string{"cart", "bus"}, h)
	got := map[string]Candidate{}
	for _, c := range f.Candidates {
		got[c.Repo] = c
	}
	for _, name := range []string{"caller", "contract-user", "old-caller", "pricing", "pricing-client", "profiles"} {
		if _, ok := got[name]; !ok {
			t.Errorf("%s missing from the frontier", name)
		}
	}
	// bus is a focus library: importing it does not make a caller. The SDK is
	// analysed with its callers, and the test copy would take cart's names.
	for _, name := range []string{"bus-only", "cart-sdk-go", "cart-test", "cart", "bus"} {
		if _, ok := got[name]; ok {
			t.Errorf("%s should not be in the frontier: %+v", name, got[name])
		}
	}
	if r := got["caller"].Reasons; !reflect.DeepEqual(r, []string{"calls cart: imports example.com/cart-sdk-go/client"}) {
		t.Errorf("caller reasons = %v", r)
	}
	if r := got["contract-user"].Reasons; len(r) != 1 || !strings.Contains(r[0], "imports its contract") {
		t.Errorf("contract-user reasons = %v", r)
	}
	if !got["old-caller"].Dormant || got["caller"].Dormant {
		t.Error("only old-caller is dormant")
	}
	if got["profiles"].Program {
		t.Error("profiles is not a Go program")
	}
	if !reflect.DeepEqual(f.Programs(), []string{"caller", "contract-user", "pricing", "pricing-client"}) {
		t.Errorf("programs = %v", f.Programs())
	}
	if !reflect.DeepEqual(f.Copies, []Copy{{Repo: "cart-test", Of: "cart", Module: "example.com/cart"}}) {
		t.Errorf("copies = %v", f.Copies)
	}
	if !reflect.DeepEqual(f.Unowned, []string{"audit.cart.create", "fraud.v1"}) {
		t.Errorf("unowned = %v", f.Unowned)
	}
	md := FrontierReport(f)
	for _, want := range []string{"| 1 | old-caller | Go program, dormant |", "- cart-test: `example.com/cart`, as cart does", "| 1 | profiles | python |"} {
		if !strings.Contains(md, want) {
			t.Errorf("report lacks %q:\n%s", want, md)
		}
	}

	// Without a run, the focus programs' modules stand in. bus's only main is
	// in its nested v2 module, which fleet does not analyse: a library, so
	// importing it still makes no caller.
	f = org().Frontier([]string{"cart", "bus"}, Hints{})
	for _, c := range f.Candidates {
		if c.Repo == "bus-only" {
			t.Error("without hints, bus's importers are not callers either")
		}
		if c.Repo == "caller" && !reflect.DeepEqual(c.Reasons, []string{"calls cart: imports example.com/cart-sdk-go/client"}) {
			t.Errorf("caller without hints: %v", c.Reasons)
		}
	}
}

func TestContractNames(t *testing.T) {
	if got := contractNames("catalog.pricing.v1"); !reflect.DeepEqual(got, []string{"catalog", "catalogpricing", "pricing"}) {
		t.Errorf("contractNames = %v", got)
	}
	if got := contractNames("gatekeeper.v1"); !reflect.DeepEqual(got, []string{"gatekeeper"}) {
		t.Errorf("contractNames = %v", got)
	}
}

func TestRings(t *testing.T) {
	// pricing-client calls the focus's missing catalog.pricing.v1; pricing serves it.
	// In ring 2, pricing-client's own missing contract leads to profiles.
	hints := func(focus []string) Hints {
		h := Hints{Modules: []string{"example.com/cart"}, Served: map[string]string{}}
		if slices.Contains(focus, "pricing-client") {
			h.Missing = []string{"profiles.customer.v1"}
		} else {
			h.Missing = []string{"catalog.pricing.v1"}
		}
		return h
	}
	f := org().Rings([]string{"cart", "bus"}, 3, hints)
	rings := map[string]int{}
	for _, c := range f.Candidates {
		rings[c.Repo] = c.Ring
	}
	if rings["pricing-client"] != 1 || rings["pricing"] != 1 || rings["profiles"] != 2 || f.Rings != 2 {
		t.Errorf("rings = %v (%d computed)", rings, f.Rings)
	}
	if !strings.Contains(FrontierReport(f), "in 2 rings") {
		t.Error("the report names the rings")
	}
}

func TestCatalogue(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "protos.git")
	write(t, dir, map[string]string{"proto/profiles/identity/v1/identity.proto": `syntax = "proto3";
// service Commented { is not a service
package profiles.identity.v1;

/* a block comment
   service Hidden {
*/
service IdentityService {
  rpc GetIdentity(GetIdentityRequest) returns (Identity) {
    option (google.api.http) = { get: "/v1/identity/{id}" };
  }
  rpc ListIdentities(ListIdentitiesRequest) returns (ListIdentitiesResponse);
}

message Identity { string id = 1; }

service Health { rpc Check(Req) returns (Res); }
`, "README.md": "service NotProto {"})
	cs, err := Catalogue([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range cs {
		got = append(got, c.Name+" "+c.Repo+" "+c.Position+" "+strings.Join(c.Methods, ","))
	}
	want := []string{
		"profiles.identity.v1.Health protos proto/profiles/identity/v1/identity.proto:17 Check",
		"profiles.identity.v1.IdentityService protos proto/profiles/identity/v1/identity.proto:8 GetIdentity,ListIdentities",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("Catalogue:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	md := ContractsReport(cs, map[string]ContractUse{
		"profiles.identity.v1.IdentityService": {Callers: []string{"identity-api"}},
		"profiles.identity.v1.Health":          {Servers: []string{"profiles (declared)"}},
	})
	for _, s := range []string{"1 called with no server in it, 1 served, 0 neither",
		"| `profiles.identity.v1.IdentityService` | 2 | protos `proto/profiles/identity/v1/identity.proto:8` |  | identity-api |"} {
		if !strings.Contains(md, s) {
			t.Errorf("report lacks %q:\n%s", s, md)
		}
	}
}
