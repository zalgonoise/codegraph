package main

import (
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
