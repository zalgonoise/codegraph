package pipeline

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalgonoise/codegraph/internal/configfile"
	"github.com/zalgonoise/codegraph/internal/options"
	"github.com/zalgonoise/codegraph/internal/profile"
)

func write(t *testing.T, file, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// setup writes two profiles and a configuration file naming them.
func setup(t *testing.T, src configfile.Source) (dir string, cf *configfile.File) {
	t.Helper()
	dir = t.TempDir()
	t.Setenv(profile.Env, "")
	write(t, filepath.Join(dir, "acme", profile.File), `defaults:
  root: ../repos           # relative to the profile
  out: ../runs/acme
  chart: "{root}/chart"
  rings: 2
  checks:
    - {name: compare, run: "echo '{}'", output: compare.json}
`)
	write(t, filepath.Join(dir, "other", profile.File), "defaults:\n  out: ../runs/other\n")
	write(t, filepath.Join(dir, configfile.Name), `profiles:
  acme: acme
  other: other
profile: other
default: daily
query:
  run: runs/shared.tar.gz
pipelines:
  daily:
    profile: acme
    rings: 3
    steps:
      - exec: echo pulled > "$CODEGRAPH_RUN.pulled"
      - graph: {rings: 4}
      - bundle
`)
	cf, err := configfile.Read(filepath.Join(dir, configfile.Name), src)
	if err != nil {
		t.Fatal(err)
	}
	return dir, cf
}

func TestResolve(t *testing.T) {
	dir, cf := setup(t, configfile.FromHome)
	p, err := Resolve(Request{Config: cf, Name: "daily", Flags: options.Options{Root: ptr("/mnt/repos")}})
	if err != nil {
		t.Fatal(err)
	}
	o, sources, _ := p.Sources()
	if p.Profile.Dir != filepath.Join(dir, "acme") || !p.Trusted || options.Str(o.Out) != filepath.Join(dir, "runs/acme") ||
		options.Str(o.Chart) != "/mnt/repos/chart" || options.Int(o.Rings) != 3 || sources["rings"] != "pipeline daily" ||
		sources["root"] != "command line" || sources["out"] != "profile" || sources["algo"] != "built-in" {
		t.Errorf("daily: profile %s, %+v, %v", p.Profile.Dir, o, sources)
	}
	if step, _ := p.Options(&p.Steps[1]); options.Int(step.Rings) != 4 {
		t.Errorf("the step's rings: %d", options.Int(step.Rings))
	}
	if cmds := strings.Join(p.Commands(), "; "); cmds != `exec: echo pulled > "$CODEGRAPH_RUN.pulled"; check compare` {
		t.Errorf("commands: %s", cmds)
	}

	// The profile: -profile, else the pipeline's, else $CODEGRAPH_PROFILE, else the file's.
	for _, c := range []struct {
		name, flag, env, want string
	}{
		{"graph", "", "", "other"},
		{"graph", "acme", "", "acme"},
		{"daily", "other", "", "other"},
		{"daily", "", filepath.Join(dir, "other"), "acme"},
		{"graph", "", filepath.Join(dir, "acme"), "acme"},
		{"graph", filepath.Join(dir, "acme"), "", "acme"},
	} {
		t.Setenv(profile.Env, c.env)
		p, err := Resolve(Request{Config: cf, Name: c.name, Profile: c.flag})
		if err != nil || filepath.Base(p.Profile.Dir) != c.want {
			t.Errorf("%+v: %v, %v", c, p.Profile.Dir, err)
		}
	}
	t.Setenv(profile.Env, "")

	if p, err := Resolve(Request{Config: cf, Name: "org"}); err != nil || len(p.Steps) != 3 || p.Steps[1].Kind != "inventory" {
		t.Errorf("org: %+v, %v", p, err)
	}
	if _, err := Resolve(Request{Config: cf, Name: "dialy"}); err == nil || !strings.Contains(err.Error(), "did you mean daily?") {
		t.Errorf("a misspelt pipeline: %v", err)
	}
	if _, err := Resolve(Request{Name: "nosuch"}); err == nil || !strings.Contains(err.Error(), "no configuration file") {
		t.Errorf("no configuration: %v", err)
	}
	if _, err := Resolve(Request{Config: cf, Name: "graph", Flags: options.Options{Algo: ptr("dfs")}}); err == nil {
		t.Error("an invalid flag should fail")
	}

	run, from, err := QueryRun(cf, "")
	if err != nil || run != filepath.Join(dir, "runs/shared.tar.gz") || !strings.Contains(from, "query.run") {
		t.Errorf("QueryRun: %s (%s), %v", run, from, err)
	}
	if run, _, _ := QueryRun(cf, "acme"); run != filepath.Join(dir, "runs/acme") {
		t.Errorf("QueryRun -profile acme: %s", run)
	}
	if run, _, _ := QueryRun(nil, ""); run != "tmp/graph" {
		t.Errorf("QueryRun without anything: %s", run)
	}
}

// A configuration file of the current directory may not run commands, nor
// make its profile's checks run; one named explicitly may.
func TestTrust(t *testing.T) {
	_, cf := setup(t, configfile.FromDir)
	p, err := Resolve(Request{Config: cf, Name: "daily"})
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Dispatch: func([]string) error { t.Error("nothing should run"); return nil }, Log: t.Logf}
	if _, err := r.Run(p); err == nil || !strings.Contains(err.Error(), "runs commands (exec: echo pulled") {
		t.Errorf("an untrusted pipeline ran: %v", err)
	}
	p, _ = Resolve(Request{Config: cf, Name: "checks"}) // the file's profile is acme's sibling: other, without checks
	if err := p.checkTrust(); err != nil {
		t.Errorf("no commands: %v", err)
	}
	p, _ = Resolve(Request{Config: cf, Name: "checks", Profile: "acme"}) // -profile names it: trusted
	if err := p.checkTrust(); err != nil {
		t.Errorf("-profile: %v", err)
	}
	var out bytes.Buffer
	if Check(&out, p); !strings.Contains(out.String(), "ok    commands: check compare") {
		t.Errorf("check:\n%s", out.String())
	}
}

func TestRepositories(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"a.git", "b", "c.git", "c", ".hidden"} {
		write(t, filepath.Join(root, d, "go.mod"), "module x\n")
	}
	write(t, filepath.Join(root, "docs.git", "README.md"), "no go.mod")
	inv := t.TempDir()
	focus := filepath.Join(t.TempDir(), "focus.txt")
	write(t, focus, "# the focus\nb\na  # inline comment\nmissing\n")
	write(t, filepath.Join(inv, "frontier.txt"), "c\na\n")
	base := func(dirs []string) string {
		var names []string
		for _, d := range dirs {
			names = append(names, filepath.Base(d))
		}
		return strings.Join(names, ",")
	}
	o := options.Options{Root: &root, Focus: ptr(options.None), Inventory: &inv}
	if repos, _, err := Repositories(&o); err != nil || base(repos) != "a.git,b,c,c.git" {
		t.Errorf("every repository: %v, %v", repos, err)
	}
	o.Focus = &focus
	if repos, from, err := Repositories(&o); err != nil || base(repos) != "b,a.git,c.git" || !strings.Contains(from, "frontier.txt") {
		t.Errorf("focus and frontier: %v (%s), %v", repos, from, err)
	}
	o.Repos = &[]string{"c", filepath.Join(root, "b"), filepath.Join(root, "docs.git")}
	if repos, _, err := Repositories(&o); err != nil || base(repos) != "c.git,b" {
		t.Errorf("given: %v, %v", repos, err)
	}
	o.Repos, o.Focus = nil, ptr(filepath.Join(root, "missing.txt"))
	if _, _, err := Repositories(&o); err == nil {
		t.Error("a missing focus file should fail")
	}
}

// A dry run prints the commands, as a shell script, and runs nothing.
func TestDryRun(t *testing.T) {
	dir, cf := setup(t, configfile.FromHome)
	write(t, filepath.Join(dir, "repos", "shop.git", "go.mod"), "module shop\n")
	write(t, filepath.Join(dir, "runs", "acme", "_system", "system.json"), "{}") // a run before
	p, err := Resolve(Request{Config: cf, Name: "daily", Flags: options.Options{API: ptr(true)}})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	r := &Runner{Dispatch: func(a []string) error { t.Errorf("ran %v", a); return nil }, DryRun: true, Out: &out, Log: t.Logf}
	if _, err := r.Run(p); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	run := filepath.Join(dir, "runs", "acme")
	for _, want := range []string{
		"# step 1: exec: echo pulled",
		"sh -c 'echo pulled > \"$CODEGRAPH_RUN.pulled\"'",
		"rm -rf " + run + ".prev && mv " + run + " " + run + ".prev",
		"GOFLAGS=-mod=readonly codegraph fleet -algo vta -api -format json,md,html,agents -out " + run + " -reuse " + run + ".prev " + filepath.Join(dir, "repos", "shop.git"),
		"# deployment skipped (needs helm and " + filepath.Join(dir, "repos", "chart") + ")",
		"sh -c 'echo '\\''{}'\\''') > " + run + "/compare.json",
		"codegraph diff -base " + run + ".prev -head " + run + " -out " + run + "/diff.md",
		"codegraph bundle -run " + run + " -out " + run + ".tar.gz",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the dry run lacks %q:\n%s", want, s)
		}
	}
	if _, err := os.Stat(run + ".prev"); err == nil {
		t.Error("a dry run moved the run")
	}
}

// A run: each step in order, through Dispatch; a failing step stops it.
func TestRun(t *testing.T) {
	dir, cf := setup(t, configfile.FromFlag)
	run := filepath.Join(dir, "runs", "acme")
	write(t, filepath.Join(run, "_system", "system.json"), "{}")
	write(t, filepath.Join(run, "shop", "manifest.json"), "{}")
	write(t, filepath.Join(run+".prev", "shop", "manifest.json"), "{}")
	var ran []string
	var logs bytes.Buffer
	r := &Runner{Log: func(f string, a ...any) { fmt.Fprintf(&logs, f+"\n", a...) }, Dispatch: func(args []string) error {
		ran = append(ran, strings.Join(args, " "))
		switch args[0] {
		case "diff":
			write(t, args[len(args)-1], "# Architecture changes\n\n## Cross-service links (2 added, 1 removed)\n")
		case "bundle":
			write(t, args[len(args)-1], "bundle")
		}
		return nil
	}}
	p, err := Resolve(Request{Config: cf, Name: "checks"})
	if err != nil {
		t.Fatal(err)
	}
	p.Steps = []configfile.Step{{Kind: "exec", Command: `echo pulled > "$CODEGRAPH_RUN.pulled"`}, {Kind: "checks"}, {Kind: "diff"}, {Kind: "bundle"}}
	p.Profile.Defaults.Checks = &[]options.Check{{Name: "compare", Run: `echo "{\"run\": \"$CODEGRAPH_RUN\"}"`, Output: "compare.json", Dir: dir}}
	p.Profile.Defaults.Out = &run
	results, err := r.Run(p)
	if err != nil {
		t.Fatalf("%v\n%s", err, logs.String())
	}
	if data, _ := os.ReadFile(run + ".pulled"); string(data) != "pulled\n" {
		t.Errorf("exec: %q", data)
	}
	if data, _ := os.ReadFile(filepath.Join(run, "compare.json")); !strings.Contains(string(data), run) {
		t.Errorf("the check's output: %q", data)
	}
	if len(ran) != 2 || !strings.HasPrefix(ran[0], "diff -base "+run+".prev") || !strings.HasPrefix(ran[1], "bundle -run "+run) {
		t.Errorf("dispatched: %q", ran)
	}
	if len(results) != 4 || results[2].Note != "+2 −1 links since the run before ("+run+"/diff.md)" || !strings.Contains(results[3].Note, "(1 KB)") {
		t.Errorf("results: %+v", results)
	}
	if !strings.Contains(logs.String(), "run checks: 4 of 4 steps in") {
		t.Errorf("summary:\n%s", logs.String())
	}

	p.Steps = []configfile.Step{{Kind: "exec", Command: "exit 3"}, {Kind: "bundle"}}
	ran = nil
	if results, err := r.Run(p); err == nil || len(results) != 1 || len(ran) != 0 {
		t.Errorf("a failing step should stop the run: %v, %+v, %q", err, results, ran)
	}
}

// config, check and status on a profile and a run.
func TestReports(t *testing.T) {
	dir, cf := setup(t, configfile.FromHome)
	write(t, filepath.Join(dir, "repos", "shop.git", "go.mod"), "module shop\n")
	p, err := Resolve(Request{Config: cf, Name: "daily"})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Describe(&out, p); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pipeline       daily: exec: echo pulled", "rings           3                ", "pipeline daily", "checks          compare"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("config lacks %q:\n%s", want, out.String())
		}
	}
	for key, want := range map[string]string{"out": filepath.Join(dir, "runs/acme"), "profile": filepath.Join(dir, "acme"), "pipelines": "daily", "config": cf.Path} {
		if v, err := Get(p, key); err != nil || v != want {
			t.Errorf("Get(%s) = %q, %v", key, v, err)
		}
	}
	if _, err := Get(p, "outt"); err == nil || !strings.Contains(err.Error(), "did you mean out?") {
		t.Errorf("Get of a misspelt key: %v", err)
	}

	out.Reset()
	Check(&out, p)
	for _, want := range []string{"ok    repositories: 1 from", "warn  chart " + filepath.Join(dir, "repos/chart") + ": not a directory", "info  no run in"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("check lacks %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if err := Status(&out, p); err != nil || !strings.Contains(out.String(), "No run in") {
		t.Errorf("status without a run: %v\n%s", err, out.String())
	}
	run := filepath.Join(dir, "runs", "acme")
	write(t, filepath.Join(run, "shop", "manifest.json"), "{}")
	write(t, filepath.Join(run, "diff.md"), "No architectural change")
	write(t, run+".tar.gz", "bundle")
	out.Reset()
	if err := Status(&out, p); err != nil || !strings.Contains(out.String(), "It predates fleet.json") ||
		!strings.Contains(out.String(), "Against the run before: no architectural change.") || !strings.Contains(out.String(), "Bundle "+run+".tar.gz: 1 KB") {
		t.Errorf("status:\n%s", out.String())
	}
}

func ptr[T any](v T) *T { return &v }
