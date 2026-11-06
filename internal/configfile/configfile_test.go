package configfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalgonoise/codegraph/internal/options"
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

func TestRead(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, Name)
	write(t, file, `version: 1
profiles:
  acme: profiles/acme
profile: acme
default: daily
query:
  run: runs/daily.tar.gz
pipelines:
  daily:
    profile: acme
    out: runs/daily
    bundel: x            # a warning
    steps:
      - exec: git -C ~/src pull
      - inventory
      - graph: {api: true, foo: 1}
      - bundle
`)
	f, err := Read(file, FromHome)
	if err != nil {
		t.Fatal(err)
	}
	p := f.Pipelines["daily"]
	if !f.Trusted || f.Profiles["acme"] != filepath.Join(dir, "profiles/acme") || f.Query.Run != filepath.Join(dir, "runs/daily.tar.gz") ||
		options.Str(p.Out) != filepath.Join(dir, "runs/daily") || len(p.Steps) != 4 || p.Steps[0].Command != "git -C ~/src pull" ||
		p.Steps[2].Kind != "graph" || !options.Bool(p.Steps[2].Options.API) || p.Steps[3].String() != "bundle" {
		t.Errorf("read: %+v, pipeline %+v", f, p)
	}
	if w := strings.Join(f.Warnings, "\n"); len(f.Warnings) != 2 || !strings.Contains(w, `unknown key "bundel", ignored (did you mean "bundle"?)`) ||
		!strings.Contains(w, `:16: unknown key "foo", ignored`) {
		t.Errorf("warnings: %q", f.Warnings)
	}
	if f.ProfileDir("acme") != filepath.Join(dir, "profiles/acme") || f.ProfileDir("other") != filepath.Join(dir, "other") ||
		f.Describe() != file+" (from the home directory)" || strings.Join(f.Names(), ",") != "daily" {
		t.Errorf("profiles and names: %q %q %q", f.ProfileDir("acme"), f.ProfileDir("other"), f.Describe())
	}
	if f, err := Read(file, FromDir); err != nil || f.Trusted || !strings.Contains(f.Describe(), "may not run commands") {
		t.Errorf("a file of the current directory is not trusted: %+v, %v", f, err)
	}

	for name, body := range map[string]string{
		"version":       "version: 2\n",
		"unknown step":  "pipelines:\n  p:\n    steps: [grpah]\n",
		"reserved name": "pipelines:\n  graph:\n    steps: [graph]\n",
		"no steps":      "pipelines:\n  p:\n    out: x\n",
		"empty exec":    "pipelines:\n  p:\n    steps: [{exec: \"\"}]\n",
		"two names":     "pipelines:\n  p:\n    steps: [{graph: {}, bundle: {}}]\n",
		"bad option":    "pipelines:\n  p:\n    algo: dfs\n    steps: [graph]\n",
		"bad type":      "pipelines:\n  p:\n    rings: many\n    steps: [graph]\n",
	} {
		write(t, file, body)
		_, err := Read(file, FromHome)
		if err == nil {
			t.Errorf("%s: read without an error", name)
		} else if name == "unknown step" && !strings.Contains(err.Error(), "did you mean graph?") {
			t.Errorf("unknown step: %v", err)
		}
	}
	write(t, file, "default: nosuch\n")
	if f, err := Read(file, FromHome); err != nil || len(f.Warnings) != 1 {
		t.Errorf("a default naming no pipeline is a warning: %v, %v", f, err)
	}
	if _, err := Read(filepath.Join(dir, "missing.yaml"), FromFlag); err == nil {
		t.Error("a missing file named explicitly should fail")
	}
}

// -config, then $CODEGRAPH_CONFIG, then the current directory (not trusted,
// unless it is the home directory), then the home directory.
func TestFind(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(Env, "")
	t.Chdir(cwd)
	if f, err := Find(""); err != nil || f != nil {
		t.Fatalf("none: %+v, %v", f, err)
	}
	write(t, filepath.Join(home, Name), "default: home\n")
	if f, _ := Find(""); f == nil || f.Source != FromHome || !f.Trusted {
		t.Errorf("home: %+v", f)
	}
	write(t, filepath.Join(cwd, Name), "default: here\n")
	if f, _ := Find(""); f == nil || f.Source != FromDir || f.Trusted || f.Default != "here" {
		t.Errorf("current directory: %+v", f)
	}
	other := filepath.Join(cwd, "other.yaml")
	write(t, other, "default: env\n")
	t.Setenv(Env, other)
	if f, _ := Find(""); f == nil || f.Source != FromEnv || !f.Trusted || f.Default != "env" {
		t.Errorf("environment: %+v", f)
	}
	if f, _ := Find(filepath.Join(home, Name)); f == nil || f.Source != FromFlag || f.Default != "home" {
		t.Errorf("flag: %+v", f)
	}
	if f, err := Find(options.None); f != nil || err != nil {
		t.Errorf("-config none: %+v, %v", f, err)
	}
	t.Setenv(Env, options.None)
	if f, err := Find(""); f != nil || err != nil {
		t.Errorf("CODEGRAPH_CONFIG=none: %+v, %v", f, err)
	}
	t.Setenv(Env, "")
	t.Chdir(home) // the home directory's file found as the current directory's is trusted
	if f, _ := Find(""); f == nil || f.Source != FromHome || !f.Trusted {
		t.Errorf("home as the current directory: %+v", f)
	}
}

// codegraph init's file reads without a warning, with or without a profile.
func TestTemplate(t *testing.T) {
	dir := t.TempDir()
	for _, prof := range []string{"", "/src/acme-codegraph"} {
		file := filepath.Join(dir, Name)
		write(t, file, Template(prof))
		f, err := Read(file, FromHome)
		if err != nil {
			t.Fatalf("profile %q: %v\n%s", prof, err, Template(prof))
		}
		if len(f.Warnings) != 0 || f.Default != "daily" || len(f.Pipelines["daily"].Steps) != 3 {
			t.Errorf("profile %q: %+v", prof, f)
		}
		if prof != "" && (f.Profiles["acme"] != prof || f.Profile != "acme" || f.Pipelines["daily"].Profile != "acme") {
			t.Errorf("the profile: %+v", f)
		}
	}
	if n := profileName("/x/Codegraph-My Org"); n != "my-org" {
		t.Errorf("profileName = %q", n)
	}
}
