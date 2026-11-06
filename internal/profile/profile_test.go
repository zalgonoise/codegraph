package profile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalgonoise/codegraph/internal/options"
)

func TestLoad(t *testing.T) {
	t.Setenv(Env, "")
	if p, err := Load(""); err != nil || p.Dir != "" || len(p.Catalogues) != 0 || p.ClientLibrarySuffix != "" {
		t.Errorf("no profile: %+v, %v", p, err)
	}

	dir := t.TempDir()
	if p, err := Load(dir); err != nil || p.Dir != dir || len(p.Messaging) != 0 || p.File != "" {
		t.Errorf("a profile without profile.yaml: %+v, %v", p, err)
	}

	write := func(name, s string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A profile written before profile.yaml: its conventions in profile.json.
	write(LegacyFile, `{"messaging": [{"name": "bus", "transport": "kafka", "module": "example.com/bus"}],
		"clientLibrarySuffix": "-sdk-go", "contractImport": "/contracts/{package}", "catalogues": ["protos"]}`)
	t.Setenv(Env, dir) // found through the environment when no directory is given
	p, err := Load("")
	if err != nil || p.Dir != dir || len(p.Messaging) != 1 || p.Messaging[0].Module != "example.com/bus" ||
		p.ClientLibrarySuffix != "-sdk-go" || p.ContractImport != "/contracts/{package}" || strings.Join(p.Catalogues, ",") != "protos" {
		t.Errorf("profile = %+v, %v", p, err)
	}

	write(File, `version: 1
clientLibrarySufix: -sdk-go     # misspelt: a warning
catalogues: [protos]
defaults:
  root: ../repos
  focus: focus.txt
  chart: "{root}/charts/prod"
  rules: [rules/inhouse.json]
  includeModules: [example.com/*-sdk-go]
  rings: 2
  api: true
  checks:
    - name: compare
      run: ./compare.sh "$CODEGRAPH_RUN"
      output: compare.json
`)
	p, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	d := p.Defaults
	if p.File != filepath.Join(dir, File) || p.ClientLibrarySuffix != "" || options.Str(d.Root) != filepath.Join(filepath.Dir(dir), "repos") ||
		options.Str(d.Focus) != filepath.Join(dir, "focus.txt") || options.Str(d.Chart) != "{root}/charts/prod" ||
		options.List(d.Rules)[0] != filepath.Join(dir, "rules/inhouse.json") || options.List(d.Include)[0] != "example.com/*-sdk-go" ||
		options.Int(d.Rings) != 2 || !options.Bool(d.API) || options.List(d.Checks)[0].Dir != dir {
		t.Errorf("profile.yaml: %+v, defaults %+v", p, d)
	}
	if w := strings.Join(p.Warnings, "\n"); len(p.Warnings) != 2 || !strings.Contains(w, `profile.yaml:2: unknown key "clientLibrarySufix", ignored (did you mean "clientLibrarySuffix"?)`) ||
		!strings.Contains(w, "profile.json: not read") {
		t.Errorf("warnings: %q", p.Warnings)
	}

	write(File, "version: 2\n")
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "version 2") {
		t.Errorf("a newer version should fail: %v", err)
	}
	write(File, "defaults:\n  rings: two\n")
	if _, err := Load(dir); err == nil {
		t.Error("a value of the wrong type should fail")
	}
	write(File, "defaults:\n  algo: dfs\n")
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "algo") {
		t.Errorf("an invalid value should fail: %v", err)
	}
	if _, err := Load(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing profile directory should fail")
	}
}
