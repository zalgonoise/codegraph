package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Query flags may follow the command: agents write them where they think of them.
func TestParseQueryFlagsAnywhere(t *testing.T) {
	q, err := ParseQuery([]string{"journey", "shop", "POST /orders", "-depth", "4", "--json", "-run=out"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if q.Depth != 4 || !q.JSON || q.Run != "out" || !slices.Equal(q.Args, []string{"journey", "shop", "POST /orders"}) {
		t.Errorf("parsed %+v", q)
	}
}

func TestParseInventory(t *testing.T) {
	c, err := ParseInventory([]string{"-root", "../org", "-focus", "dept.txt", "-run", "tmp/graph"})
	if err != nil || c.Root != "../org" || c.Out != "tmp/inventory" || c.Focus != "dept.txt" || c.Run != "tmp/graph" {
		t.Fatalf("ParseInventory = %+v, %v", c, err)
	}
	if c.Rings != 1 || len(c.Catalogues) != 0 || c.CataloguesSet {
		t.Errorf("defaults: rings %d, catalogues %v (set %v)", c.Rings, c.Catalogues, c.CataloguesSet)
	}
	if c, err := ParseInventory([]string{"-root", "x", "-run", "tmp/graph", "-catalogues", "protos,more"}); err != nil ||
		strings.Join(c.Catalogues, ",") != "protos,more" || !c.CataloguesSet {
		t.Errorf("a run without a focus, catalogues: %+v, %v", c, err) // -run also gives contracts.md its servers and callers
	}
	if c, _ := ParseInventory([]string{"-root", "x", "-catalogues", ""}); len(c.Catalogues) != 0 || !c.CataloguesSet {
		t.Errorf("-catalogues '' asks for none, over the profile's: %+v", c)
	}
	for _, args := range [][]string{{}, {"-root", "x", "-rings", "0"}} {
		if _, err := ParseInventory(args); err == nil {
			t.Errorf("ParseInventory(%v) should fail", args)
		}
	}
}

func TestParseDeploy(t *testing.T) {
	c, err := ParseDeploy([]string{"-chart", "../org/charts_prod.git/charts", "-values", "values.yaml, overrides.yaml"})
	if err != nil || c.Environment != "production" || c.Out != "tmp/deploy" ||
		strings.Join(c.Values, "|") != "values.yaml|overrides.yaml" || strings.Join(c.Secrets, "|") != "secrets.yaml|secrets.overrides.yaml" {
		t.Fatalf("ParseDeploy = %+v, %v", c, err)
	}
	for _, args := range [][]string{{}, {"-chart", "x", "-rendered", "y"}} {
		if _, err := ParseDeploy(args); err == nil {
			t.Errorf("ParseDeploy(%v) should fail: one of -chart and -rendered", args)
		}
	}
}

func TestParseLibrary(t *testing.T) {
	if c, err := ParseAnalyze([]string{"-dir", "lib", "-library"}); err != nil || !c.Library || !slices.Equal(c.Entries, []string{"./..."}) {
		t.Errorf("analyze -library = %+v, %v", c, err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/lib\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := ParseFleet([]string{"-libraries", dir})
	if err != nil || !f.Libraries || f.Analyze.Library {
		t.Errorf("fleet -libraries = %+v, %v (Library is set per repository)", f, err)
	}
}
