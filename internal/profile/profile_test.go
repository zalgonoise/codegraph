package profile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	t.Setenv(Env, "")
	if p, err := Load(""); err != nil || p.Dir != "" || len(p.Catalogues) != 0 || p.ClientLibrarySuffix != "" {
		t.Errorf("no profile: %+v, %v", p, err)
	}

	dir := t.TempDir()
	if p, err := Load(dir); err != nil || p.Dir != dir || len(p.Messaging) != 0 {
		t.Errorf("a profile without profile.json: %+v, %v", p, err)
	}

	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, File), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"messaging": [{"name": "bus", "transport": "kafka", "module": "example.com/bus"}],
		"clientLibrarySuffix": "-sdk-go", "contractImport": "/contracts/{package}", "catalogues": ["protos"]}`)
	t.Setenv(Env, dir) // found through the environment when no directory is given
	p, err := Load("")
	if err != nil || p.Dir != dir || len(p.Messaging) != 1 || p.Messaging[0].Module != "example.com/bus" ||
		p.ClientLibrarySuffix != "-sdk-go" || p.ContractImport != "/contracts/{package}" || strings.Join(p.Catalogues, ",") != "protos" {
		t.Errorf("profile = %+v, %v", p, err)
	}

	write(`{"clientLibrarySufix": "-sdk-go"}`)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "clientLibrarySufix") {
		t.Errorf("a misspelt setting should fail: %v", err)
	}
	if _, err := Load(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing profile directory should fail")
	}
}
