package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every command answers to its name and its short form, which no other
// shares; dashed forms are flags, but for help's and version's.
func TestCommandNames(t *testing.T) {
	seen, listed := map[string]string{}, strings.Join(strings.Fields(usage), " ")
	for _, c := range commands() {
		for _, n := range []string{c.name, c.short} {
			if other, ok := seen[n]; ok {
				t.Errorf("%s and %s both answer to %q", other, c.name, n)
			}
			seen[n] = c.name
			if got, ok := lookup(n); !ok || got.name != c.name {
				t.Errorf("lookup(%q) = %q, %v", n, got.name, ok)
			}
		}
		if c.short != c.name[:1] && c.short != strings.ToUpper(c.name[:1]) {
			t.Errorf("%s's short form %q is not its first letter", c.name, c.short)
		}
		if !strings.Contains(listed, " "+c.short+" "+c.name) {
			t.Errorf("the usage does not list %s's short form", c.name)
		}
	}
	for _, arg := range []string{"-config", "--run", "-c", "-inventory", "--q", "Q"} {
		if c, ok := lookup(arg); ok {
			t.Errorf("lookup(%q) = %s", arg, c.name)
		}
	}
	for arg, want := range map[string]string{"-h": "help", "-help": "help", "--help": "help", "-v": "version", "-version": "version", "--version": "version"} {
		if c, ok := lookup(arg); !ok || c.name != want {
			t.Errorf("lookup(%q) = %q, %v", arg, c.name, ok)
		}
	}
	for arg, want := range map[string]string{
		"-config":  "commands take no dashes (codegraph config), and flags go after the command",
		"--C":      "commands take no dashes (codegraph check)",
		"-profile": "flags go after the command (codegraph <command> -profile)",
		"querry":   "did you mean query?",
		"x":        `unknown command "x"`,
	} {
		if err := dispatch([]string{arg}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("dispatch(%q): %v", arg, err)
		}
	}
}

// codegraph mcp -entry prints what an MCP client needs to start this binary,
// from anywhere: absolute paths, and no -run only when the home directory's
// configuration file names the run, which the server then finds by itself.
func TestMCPEntry(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(cwd)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	write := func(file, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entry := func(args ...string) string {
		t.Helper()
		return captureStdout(t, func() error { return dispatch(append([]string{"mcp", "-entry"}, args...)) })
	}
	args := func(run string, more ...string) string {
		quoted := []string{`"mcp"`}
		for _, a := range append([]string{"-run", run}, more...) {
			quoted = append(quoted, `"`+a+`"`)
		}
		return `"args": [` + strings.Join(quoted, ", ") + `]`
	}

	// No configuration file (TestMain): the default run, made absolute.
	if out := entry(); !strings.Contains(out, `"command": "`+exe+`"`) || !strings.Contains(out, args(filepath.Join(wd, "tmp/graph"))) {
		t.Errorf("the default run:\n%s", out)
	}
	if out := entry("-run", "runs/x", "-explorer", "none"); !strings.Contains(out, args(filepath.Join(wd, "runs/x"), "-explorer", "none")) {
		t.Errorf("-run and -explorer:\n%s", out)
	}

	// The home directory's file names the run: the server needs no argument.
	t.Setenv("CODEGRAPH_CONFIG", "")
	write(filepath.Join(home, ".codegraph.yaml"), "query:\n  run: runs/daily\n")
	if out := entry(); !strings.Contains(out, `"args": ["mcp"]`+"\n") {
		t.Errorf("a run the home file names:\n%s", out)
	}
	// A profile's run, a client's environment and another file are not what a
	// client starting the server would find: the run is named.
	write(filepath.Join(wd, "acme", "profile.yaml"), "defaults:\n  out: runs/acme\n")
	acme := args(filepath.Join(wd, "acme", "runs/acme"))
	if out := entry("-profile", "acme"); !strings.Contains(out, acme) {
		t.Errorf("-profile:\n%s", out)
	}
	t.Setenv("CODEGRAPH_PROFILE", filepath.Join(wd, "acme"))
	if out := entry(); !strings.Contains(out, acme) {
		t.Errorf("$CODEGRAPH_PROFILE:\n%s", out)
	}
	t.Setenv("CODEGRAPH_PROFILE", "")
	if out := entry("-config", filepath.Join(home, ".codegraph.yaml")); !strings.Contains(out, args(filepath.Join(home, "runs/daily"))) {
		t.Errorf("-config:\n%s", out)
	}
	write(filepath.Join(wd, ".codegraph.yaml"), "query:\n  run: runs/here\n")
	if out := entry(); !strings.Contains(out, args(filepath.Join(wd, "runs/here"))) {
		t.Errorf("the current directory's file:\n%s", out)
	}
}
