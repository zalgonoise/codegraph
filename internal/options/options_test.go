package options

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Layers merge field by field, each value remembering the layer it came from;
// placeholders follow the merged root and out.
func TestMerge(t *testing.T) {
	o, sources := Defaults(), map[string]string{}
	profile := Options{Root: ptr("/org"), Chart: ptr("{root}/charts/prod"), Include: &[]string{"example.com/*-sdk-go"}}
	pipeline := Options{Out: ptr("/runs/daily"), API: ptr(true), Include: &[]string{}}
	flags := Options{Root: ptr("/mnt/org")}
	o.Merge(&profile, "profile", sources)
	o.Merge(&pipeline, "pipeline", sources)
	o.Merge(&flags, "command line", sources)
	o.Merge(nil, "nothing", sources)
	o.Expand()
	if Str(o.Root) != "/mnt/org" || Str(o.Chart) != "/mnt/org/charts/prod" || Str(o.Out) != "/runs/daily" ||
		Str(o.Base) != "/runs/daily.prev" || Str(o.Bundle) != "/runs/daily.tar.gz" || !Bool(o.API) || o.Include == nil || len(*o.Include) != 0 {
		t.Errorf("merged: %+v", o)
	}
	if sources["root"] != "command line" || sources["chart"] != "profile" || sources["includeModules"] != "pipeline" || sources["algo"] != "" {
		t.Errorf("sources: %v", sources)
	}
}

func TestResolve(t *testing.T) {
	home, _ := os.UserHomeDir()
	o := Options{Root: ptr("~/src"), Focus: ptr(None), Out: ptr("tmp/graph"), Chart: ptr("{root}/chart"), Bundle: ptr("/abs/b.tar.gz"),
		Repos: &[]string{"orders", "./billing", "~/ledger"}, Rules: &[]string{"rules/a.json"}, Include: &[]string{"x/*"},
		Checks: &[]Check{{Name: "c", Run: "true"}}}
	o.Resolve("/profile")
	if Str(o.Root) != filepath.Join(home, "src") || Str(o.Focus) != None || Str(o.Out) != "/profile/tmp/graph" || Str(o.Chart) != "{root}/chart" ||
		Str(o.Bundle) != "/abs/b.tar.gz" || strings.Join(*o.Repos, ",") != "orders,/profile/billing,"+filepath.Join(home, "ledger") ||
		(*o.Rules)[0] != "/profile/rules/a.json" || (*o.Include)[0] != "x/*" || (*o.Checks)[0].Dir != "/profile" {
		t.Errorf("resolved: %+v", o)
	}
}

// Flags are the options' names in kebab case; lists take commas or repeats,
// an empty list flag empties the list, -no-checks empties the checks.
func TestRegister(t *testing.T) {
	var o Options
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	Register(fs, &o)
	err := fs.Parse([]string{"-repo-rules", "r", "-include-modules", "a,b", "-include-modules", "c", "-rules", "",
		"-api", "-full=false", "-rings", "3", "-no-checks", "-no-settings"})
	if err != nil {
		t.Fatal(err)
	}
	if Str(o.RepoRules) != "r" || strings.Join(*o.Include, ",") != "a,b,c" || o.Rules == nil || len(*o.Rules) != 0 ||
		!Bool(o.API) || o.Full == nil || *o.Full || Int(o.Rings) != 3 || o.Checks == nil || len(*o.Checks) != 0 || !Bool(o.NoSettings) || o.Out != nil {
		t.Errorf("flags: %+v", o)
	}
	if v, _ := o.Get("includeModules"); v != "a,b,c" {
		t.Errorf("Get(includeModules) = %q", v)
	}
	if v, ok := o.Get("include-modules"); !ok || v != "a,b,c" {
		t.Errorf("Get by flag name = %q, %v", v, ok)
	}
	if _, ok := o.Get("nosuch"); ok {
		t.Error("Get of an unknown option")
	}
	if err := fs.Parse([]string{"-rings", "x"}); err == nil {
		t.Error("a bad number should fail")
	}
}

func TestValidate(t *testing.T) {
	for _, o := range []Options{{Algo: ptr("dfs")}, {Compress: ptr("lz4")}, {Rings: ptr(0)}, {Checks: &[]Check{{Name: "x"}}}} {
		if err := o.Validate(); err == nil {
			t.Errorf("%+v should be invalid", o)
		}
	}
	d := Defaults()
	if err := d.Validate(); err != nil {
		t.Error(err)
	}
}

// Unknown keys are warnings with a suggestion, wherever they are; values of
// the wrong type are errors.
func TestDecodeYAML(t *testing.T) {
	var v struct {
		Defaults Options `json:"defaults"`
		Named    map[string]Options
		List     []Options `json:"list"`
	}
	warnings, err := DecodeYAML([]byte(`
defaults:
  repo-rules: r        # kebab: did you mean repoRules
  outt: x              # one edit away from out
  checks:
    - {name: a, run: "true", ouput: a.json}
list:
  - {zzz: 1}
nosuch: 1
`), "f.yaml", &v)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`f.yaml:3: unknown key "repo-rules", ignored (did you mean "repoRules"?)`, `f.yaml:4: unknown key "outt", ignored (did you mean "out"?)`,
		`f.yaml:6: unknown key "ouput", ignored (did you mean "output"?)`, `f.yaml:8: unknown key "zzz", ignored`, `f.yaml:9: unknown key "nosuch", ignored`}
	if strings.Join(warnings, "\n") != strings.Join(want, "\n") {
		t.Errorf("warnings:\n%s", strings.Join(warnings, "\n"))
	}
	if (*v.Defaults.Checks)[0].Name != "a" {
		t.Errorf("decoded: %+v", v.Defaults)
	}
	if _, err := DecodeYAML([]byte("defaults: {rings: two}"), "f.yaml", &v); err == nil {
		t.Error("a wrong type should fail")
	}
	if _, err := DecodeYAML([]byte("defaults: [unclosed"), "f.yaml", &v); err == nil {
		t.Error("invalid YAML should fail")
	}
	if w, err := DecodeYAML(nil, "empty.yaml", &v); err != nil || len(w) != 0 {
		t.Errorf("an empty file: %v, %v", w, err)
	}
}
