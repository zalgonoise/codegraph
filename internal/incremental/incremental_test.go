package incremental

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, file, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A repository's key changes with its files, the settings and the rule
// files; uncommitted changes, local replaces and a missing git clone leave it
// without one.
func TestKey(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo, rules := t.TempDir(), filepath.Join(t.TempDir(), "rules.json")
	write(t, filepath.Join(repo, "go.mod"), "module example.com/shop\n\ngo 1.22\n")
	write(t, filepath.Join(repo, "main.go"), "package main\n\nfunc main() {}\n")
	write(t, rules, `{"rules": []}`)
	git(t, repo, "init", "-q")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "one")

	head, key, why := Key("tool", repo, map[string]int{"depth": 3}, []string{rules})
	if head == "" || key == "" || why != "" {
		t.Fatalf("a clean clone: %q %q %q", head, key, why)
	}
	if _, k, _ := Key("tool", repo, map[string]int{"depth": 3}, []string{rules}); k != key {
		t.Error("the same inputs should give the same key")
	}
	for name, k := range map[string]string{
		"toolchain": func() string { _, k, _ := Key("other", repo, map[string]int{"depth": 3}, []string{rules}); return k }(),
		"settings":  func() string { _, k, _ := Key("tool", repo, map[string]int{"depth": 4}, []string{rules}); return k }(),
		"no rules":  func() string { _, k, _ := Key("tool", repo, map[string]int{"depth": 3}, nil); return k }(),
	} {
		if k == key || k == "" {
			t.Errorf("%s: key %q", name, k)
		}
	}
	write(t, rules, `{"rules": [ ]}`)
	if _, k, _ := Key("tool", repo, map[string]int{"depth": 3}, []string{rules}); k == key {
		t.Error("a changed rule file should change the key")
	}

	write(t, filepath.Join(repo, "main.go"), "package main\n\nfunc main() { println() }\n")
	if _, k, why := Key("tool", repo, nil, nil); k != "" || why != "uncommitted changes" {
		t.Errorf("uncommitted changes: %q %q", k, why)
	}
	git(t, repo, "commit", "-q", "-am", "two")
	h2, k2, _ := Key("tool", repo, map[string]int{"depth": 3}, nil)
	if h2 == head || k2 == "" {
		t.Errorf("a new commit: %q %q", h2, k2)
	}

	write(t, filepath.Join(repo, "go.mod"), "module example.com/shop\n\ngo 1.22\n\nreplace example.com/lib => ../lib\n")
	git(t, repo, "commit", "-q", "-am", "replace")
	if _, k, why := Key("tool", repo, nil, nil); k != "" || why != "its go.mod replaces a module with a local directory (../lib)" {
		t.Errorf("a local replace: %q %q", k, why)
	}
	if _, k, why := Key("tool", t.TempDir(), nil, nil); k != "" || why != "not a git clone" {
		t.Errorf("not a clone: %q %q", k, why)
	}
}

func TestRecord(t *testing.T) {
	prev, out := t.TempDir(), t.TempDir()
	write(t, filepath.Join(prev, "shop", "manifest.json"), "{}")
	write(t, filepath.Join(prev, "shop", "report.md"), "# shop")
	write(t, filepath.Join(out, "shop", "stale.md"), "old")
	r := &Record{Codegraph: "v1", Created: time.Now().UTC(), Services: 1, Repos: []Repo{
		{Dir: "/src/shop", Key: "k", Services: []string{"shop"}, Reused: true},
		{Dir: "/src/lib", Library: true},
		{Dir: "/src/api", Services: []string{"api"}},
	}}
	if err := r.Write(prev); err != nil {
		t.Fatal(err)
	}
	got, err := Read(prev)
	if err != nil || got.Find("/src/shop").Key != "k" || got.Find("/src/none") != nil {
		t.Fatalf("read back: %+v, %v", got, err)
	}
	if reused, analysed := got.Reused(); reused != 1 || analysed != 1 {
		t.Errorf("Reused() = %d, %d", reused, analysed)
	}
	if !Has(prev, []string{"shop"}) || Has(prev, []string{"shop", "api"}) || Has(prev, nil) {
		t.Error("Has")
	}
	if err := Copy(prev, out, []string{"shop"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "shop", "report.md")); err != nil {
		t.Error("copied files missing")
	}
	if _, err := os.Stat(filepath.Join(out, "shop", "stale.md")); err == nil {
		t.Error("a service's old files should go")
	}
	if r, err := Read(t.TempDir()); r != nil || err != nil {
		t.Errorf("no record: %+v, %v", r, err)
	}
	if tc, err := Toolchain(); err != nil || tc == "" {
		t.Errorf("Toolchain: %q, %v", tc, err)
	}
}
