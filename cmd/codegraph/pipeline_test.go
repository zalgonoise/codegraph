package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalgonoise/codegraph/internal/incremental"
)

// TestMain keeps the tests from reading the configuration file and profile of
// whoever runs them.
func TestMain(m *testing.M) {
	os.Setenv("CODEGRAPH_CONFIG", "none")
	os.Unsetenv("CODEGRAPH_PROFILE")
	os.Exit(m.Run())
}

// clone copies a sample into root/<name>.git as a git clone of its own.
func clone(t *testing.T, root, sample string) string {
	t.Helper()
	dir := filepath.Join(root, sample+".git")
	if err := os.CopyFS(dir, os.DirFS(filepath.Join("../../testdata", sample))); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "init", "-q")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-q", "-m", "sample")
	return dir
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// A pipeline of a configuration file over a profile: exec, inventory, graph
// (fleet, the profile's check, the diff with the run before) and bundle; then
// graph again, reusing the analysis of the repositories unchanged.
func TestPipeline(t *testing.T) {
	if testing.Short() {
		t.Skip("builds SSA for two samples, four times")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	root := filepath.Join(dir, "repos")
	orders := clone(t, root, "orders")
	clone(t, root, "inventory")
	write := func(file, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "acme", "profile.yaml"), `version: 1
defaults:
  root: ../repos
  out: ../runs/graph
  inventory: ../runs/inventory
  formats: [json, html]
  checks:
    - name: count
      run: ls "$CODEGRAPH_RUN"/*/manifest.json | wc -l | tr -d ' '
      output: count.txt
`)
	cfg := filepath.Join(dir, "codegraph.yaml")
	write(cfg, `version: 1
profiles: {acme: acme}
profile: acme
default: daily
pipelines:
  daily:
    profile: acme
    bundle: runs/daily.tar.zst
    steps:
      - exec: echo "$CODEGRAPH_ROOT" > synced.txt    # in the file's directory
      - inventory
      - graph
      - bundle
`)
	run := filepath.Join(dir, "runs", "graph")
	if err := runRun([]string{"-config", cfg}); err != nil { // the default pipeline
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "synced.txt")); strings.TrimSpace(string(data)) != root {
		t.Errorf("exec: %q", data)
	}
	for _, f := range []string{filepath.Join(dir, "runs", "inventory", "inventory.json"), filepath.Join(run, "orders", "manifest.json"),
		filepath.Join(run, "_system", "system.html"), filepath.Join(dir, "runs", "daily.tar.zst")} {
		if _, err := os.Stat(f); err != nil {
			t.Error(err)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(run, "count.txt")); strings.TrimSpace(string(data)) != "2" {
		t.Errorf("the check's output: %q", data)
	}
	reused := func() map[string]bool {
		t.Helper()
		rec, err := incremental.Read(run)
		if err != nil || rec == nil || len(rec.Repos) != 2 || rec.Services != 2 || rec.Links == 0 {
			t.Fatalf("fleet.json: %+v, %v", rec, err)
		}
		out := map[string]bool{}
		for _, r := range rec.Repos {
			if r.Key == "" {
				t.Errorf("%s has no key: %s", r.Dir, r.Why)
			}
			out[filepath.Base(r.Dir)] = r.Reused
		}
		return out
	}
	if r := reused(); r["orders.git"] || r["inventory.git"] {
		t.Errorf("the first run reused %v", r)
	}

	// Nothing changed: both reused, the same manifests, no architectural change.
	if err := runRun([]string{"-config", cfg, "graph"}); err != nil {
		t.Fatal(err)
	}
	if r := reused(); !r["orders.git"] || !r["inventory.git"] {
		t.Errorf("an unchanged run reused %v", r)
	}
	for _, svc := range []string{"orders", "inventory"} {
		a, _ := os.ReadFile(filepath.Join(run+".prev", svc, "manifest.json"))
		b, _ := os.ReadFile(filepath.Join(run, svc, "manifest.json"))
		if len(a) == 0 || !bytes.Equal(a, b) {
			t.Errorf("%s's reused manifest differs", svc)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(run, "diff.md")); !strings.Contains(string(data), "No architectural change") {
		t.Errorf("diff.md:\n%s", data)
	}

	// A new commit in orders: only orders is analysed again; -full reuses nothing.
	write(filepath.Join(orders, "NOTES.md"), "a change\n")
	gitIn(t, orders, "add", ".")
	gitIn(t, orders, "commit", "-q", "-m", "notes")
	if err := runRun([]string{"-config", cfg, "graph"}); err != nil {
		t.Fatal(err)
	}
	if r := reused(); r["orders.git"] || !r["inventory.git"] {
		t.Errorf("after a commit in orders: %v", r)
	}
	if err := runRun([]string{"-config", cfg, "graph", "-full"}); err != nil {
		t.Fatal(err)
	}
	if r := reused(); r["orders.git"] || r["inventory.git"] {
		t.Errorf("-full reused %v", r)
	}

	// The settings, checked and shown; the run's state.
	out := captureStdout(t, func() error { return dispatch([]string{"c", "-config", cfg, "-get", "bundle"}) })
	if strings.TrimSpace(out) != filepath.Join(dir, "runs", "daily.tar.zst") {
		t.Errorf("config -get bundle: %q", out)
	}
	// make's targets ask for a step's settings, never the default pipeline's.
	out = captureStdout(t, func() error {
		return runReport("config", []string{"-config", cfg, "-pipeline", "none", "-get", "bundle"})
	})
	if strings.TrimSpace(out) != run+".tar.gz" {
		t.Errorf("config -pipeline none -get bundle: %q", out)
	}
	out = captureStdout(t, func() error { return runReport("status", []string{"-config", cfg}) })
	if !strings.Contains(out, "2 services") || !strings.Contains(out, "No repository has a new commit since.") {
		t.Errorf("status:\n%s", out)
	}
	out = captureStdout(t, func() error { return dispatch([]string{"C", "-config", cfg}) })
	if !strings.Contains(out, "ok    repositories: 2 from") || !strings.Contains(out, "ok    commands: exec: echo") {
		t.Errorf("check:\n%s", out)
	}
	// query and mcp find the run through the configuration file's profile.
	out = captureStdout(t, func() error { return runQuery([]string{"-config", cfg, "-explorer", "none", "services"}) })
	if !strings.Contains(out, "2 services") {
		t.Errorf("query through the configuration:\n%s", out)
	}

	// The same file in the current directory may not run its exec step.
	t.Chdir(dir)
	if err := os.Rename(cfg, filepath.Join(dir, ".codegraph.yaml")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEGRAPH_CONFIG", "")
	t.Setenv("HOME", t.TempDir())
	if err := runRun(nil); err == nil || !strings.Contains(err.Error(), "runs commands") {
		t.Errorf("an untrusted pipeline: %v", err)
	}

	// codegraph init writes a file that reads back.
	init := filepath.Join(dir, "init.yaml")
	if err := runInit([]string{"-out", init, "-profile", filepath.Join(dir, "acme")}); err != nil {
		t.Fatal(err)
	}
	if err := runInit([]string{"-out", init}); err == nil {
		t.Error("init should not overwrite a file without -force")
	}
	out = captureStdout(t, func() error { return runReport("config", []string{"-config", init, "-get", "out"}) })
	if strings.TrimSpace(out) != run {
		t.Errorf("init's file resolves out to %q", out)
	}
}
