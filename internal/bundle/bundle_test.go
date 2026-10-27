package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalgonoise/codegraph/internal/deploy"
	"github.com/zalgonoise/codegraph/internal/inventory"
	"github.com/zalgonoise/codegraph/internal/model"
)

// testRun is a small run with every kind of position and configured value.
func testRun() *Run {
	shop := &model.Manifest{Codegraph: "v1.0.0", Service: "shop", Module: "example.com/shop",
		Endpoints: []model.Endpoint{{ID: "a", Transport: "http", Role: "server", Direction: model.Inbound, Keys: []string{"GET /items"},
			Component: "example.com/shop/api", Position: "api/routes.go:12", Source: "detector",
			Sites: []model.Site{{Function: "api.routes", Position: "api/routes.go:12"}}}},
		Journeys: []model.Journey{{Entrypoint: "a", Transport: "http", Key: "GET /items",
			Roots: []model.Site{{Function: "api.list", Position: "api/list.go:3"}}}},
		Bootstrap: []model.Step{{Call: "main.main", Position: "cmd/shop/main.go:9"}},
		Bindings:  []model.Binding{{Interface: "I", Concrete: "T", Positions: []string{"cmd/shop/main.go:10"}}},
	}
	notes := &model.Manifest{Codegraph: "v0.9.0", Service: "notes", Module: "notes", Library: true,
		Declared: &model.Declared{Repository: "notes", File: "/home/me/profile/declared/notes.json"}}
	dep := &deploy.Deployment{Environment: "staging", Source: "/home/me/charts/staging",
		Workloads: []deploy.Workload{{Kind: "Deployment", Name: "shop", Containers: []deploy.Container{
			{Name: "shop", Image: "team-shop", Env: map[string]string{"DB_HOST": "db.internal", "TOKEN": deploy.Secret}}}}},
		Network: &deploy.Network{
			ConfigOnly: []deploy.Pair{{From: "shop", To: "stock", Settings: []string{"STOCK_HOST=stock.internal"}}},
			Settled:    []deploy.Settled{{From: "shop", Key: "S", To: "s2", Setting: "GRPC_S_HOST=s2"}},
			Mocked:     []deploy.Mock{{From: "shop", Setting: "PAY_MOCKED=true", To: []string{"pay"}}},
		}}
	inv := &inventory.Inventory{Codegraph: "v1.0.0",
		Repos:     []inventory.Repo{{Name: "shop", Images: []inventory.Image{{Name: "team-shop", Dockerfile: "Dockerfile", File: "ci.yml:4", Via: "docker push"}}}},
		Contracts: []inventory.Contract{{Name: "shop.v1.Shop", Repo: "protos", Position: "proto/shop.proto:5"}},
	}
	return &Run{Manifests: []*model.Manifest{shop, notes}, Deploy: dep, Inventory: inv}
}

func write(t *testing.T, r *Run, opts Options) []byte {
	t.Helper()
	var buf bytes.Buffer
	if opts.Now.IsZero() {
		opts.Now = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	}
	if _, err := Write(&buf, r, opts); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A bundle reads back what was packed, with gzip or zstd, and its metadata
// counts and versions it.
func TestRoundTrip(t *testing.T) {
	for _, c := range []string{Gzip, Zstd} {
		data := write(t, testRun(), Options{Compression: c, Version: "v1.0.1"})
		r, err := ReadFrom(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: %v", c, err)
		}
		m := r.Meta
		if m.Format != Format || m.Codegraph != "v1.0.1" || m.Compression != c || m.Services != 1 || m.Libraries != 1 ||
			strings.Join(m.AnalysedBy, ",") != "v0.9.0,v1.0.0" || m.Environment != "staging" || !m.Inventory || m.NoSettings || m.NoPositions {
			t.Errorf("%s metadata: %+v", c, m)
		}
		if len(r.Manifests) != 2 || r.Manifests[0].Service != "notes" || r.Manifests[1].Endpoints[0].Position != "api/routes.go:12" ||
			r.Manifests[1].Codegraph != "v1.0.0" {
			t.Errorf("%s manifests: %+v", c, r.Manifests)
		}
		if r.Deploy.Workloads[0].Containers[0].Env["DB_HOST"] != "db.internal" || r.Inventory.Repos[0].Images[0].File != "ci.yml:4" {
			t.Errorf("%s: deployment or inventory changed", c)
		}
		if !bytes.Contains(r.Explorer, []byte("<html")) {
			t.Errorf("%s: no explorer page", c)
		}
	}
	// The same contents and date give the same bytes.
	if a, b := write(t, testRun(), Options{Version: "v1"}), write(t, testRun(), Options{Version: "v1"}); !bytes.Equal(a, b) {
		t.Error("two bundles of the same run differ")
	}
}

// -no-settings keeps the names of configured values, -no-positions drops every
// file reference; the run they come from is left as it was.
func TestOmissions(t *testing.T) {
	src := testRun()
	r, err := ReadFrom(bytes.NewReader(write(t, src, Options{NoSettings: true, NoPositions: true})))
	if err != nil {
		t.Fatal(err)
	}
	if !r.Meta.NoSettings || !r.Meta.NoPositions {
		t.Errorf("metadata: %+v", r.Meta)
	}
	c := r.Deploy.Workloads[0].Containers[0]
	n := r.Deploy.Network
	if c.Env["DB_HOST"] != "" || len(c.Env) != 2 || n.ConfigOnly[0].Settings[0] != "STOCK_HOST" || n.Settled[0].Setting != "GRPC_S_HOST" ||
		n.Mocked[0].Setting != "PAY_MOCKED" || r.Deploy.Source != "" {
		t.Errorf("settings: env %v, network %+v, source %q", c.Env, n, r.Deploy.Source)
	}
	shop := r.Manifests[1]
	if shop.Endpoints[0].Position != "" || shop.Endpoints[0].Sites[0].Position != "" || shop.Journeys[0].Roots[0].Position != "" ||
		shop.Bootstrap[0].Position != "" || len(shop.Bindings[0].Positions) != 0 || r.Manifests[0].Declared.File != "" {
		t.Errorf("positions left: %+v", shop)
	}
	if shop.Endpoints[0].Source != "detector" || shop.Journeys[0].Roots[0].Function != "api.list" {
		t.Error("only positions should go")
	}
	if im := r.Inventory.Repos[0].Images[0]; im.File != "" || im.Dockerfile != "" || r.Inventory.Contracts[0].Position != "" {
		t.Errorf("inventory positions left: %+v %+v", im, r.Inventory.Contracts[0])
	}
	if src.Deploy.Workloads[0].Containers[0].Env["DB_HOST"] != "db.internal" || src.Manifests[0].Endpoints[0].Position != "api/routes.go:12" {
		t.Error("the source run was changed")
	}
}

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestReadErrors(t *testing.T) {
	manifest := `{"service":"shop"}`
	for name, data := range map[string][]byte{
		"newer format": tarGz(t, map[string]string{MetadataFile: `{"format": 2, "codegraph": "v9"}`, "shop/manifest.json": manifest}),
		"no metadata":  tarGz(t, map[string]string{"shop/manifest.json": manifest}),
		"no manifest":  tarGz(t, map[string]string{MetadataFile: `{"format": 1}`}),
		"twice":        tarGz(t, map[string]string{MetadataFile: `{"format": 1}`, "a/manifest.json": manifest, "b/manifest.json": manifest}),
		"not a bundle": []byte("hello, world"),
		"too short":    []byte("hi"),
	} {
		if _, err := ReadFrom(bytes.NewReader(data)); err == nil {
			t.Errorf("%s: read without an error", name)
		} else if name == "newer format" && !strings.Contains(err.Error(), "format 2") {
			t.Errorf("newer format: %v", err)
		}
	}
	if _, err := Write(&bytes.Buffer{}, &Run{Manifests: []*model.Manifest{{Service: "../x"}}}, Options{}); err == nil {
		t.Error("a service name with a slash should be refused")
	}
	if _, err := Write(&bytes.Buffer{}, testRun(), Options{Compression: "lz4"}); err == nil {
		t.Error("an unknown compression should be refused")
	}
}

// A run directory: its manifests, its deployment and the inventory next to it
// (or one given instead); a bundle file the same way; the explorer page goes
// to the user's cache once.
func TestLoad(t *testing.T) {
	root := t.TempDir()
	run := filepath.Join(root, "graph")
	src := testRun()
	for _, m := range src.Manifests {
		writeJSON(t, filepath.Join(run, m.Service, "manifest.json"), m)
	}
	writeJSON(t, filepath.Join(run, DeployFile), src.Deploy)
	writeJSON(t, filepath.Join(root, "inventory", InventoryFile), src.Inventory)
	r, err := Load(run, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Manifests) != 2 || r.Deploy == nil || r.Inventory == nil || r.Meta != nil {
		t.Fatalf("run directory: %+v", r)
	}
	other := filepath.Join(root, "other.json")
	writeJSON(t, other, inventory.Inventory{Repos: []inventory.Repo{{Name: "x"}}})
	if r, err := Load(run, other); err != nil || r.Inventory.Repos[0].Name != "x" {
		t.Errorf("-inventory: %v, %+v", err, r)
	}
	if _, err := Load(filepath.Join(root, "inventory"), ""); err == nil {
		t.Error("a directory without manifests should be an error")
	}

	file := filepath.Join(root, "graph.tar.zst")
	if err := os.WriteFile(file, write(t, r, Options{Compression: Compression(file)}), 0o644); err != nil {
		t.Fatal(err)
	}
	if !IsBundle(file) || IsBundle(run) {
		t.Error("IsBundle")
	}
	b, err := Load(file, "")
	if err != nil || b.Meta == nil || b.Meta.Compression != Zstd || len(b.Manifests) != 2 {
		t.Fatalf("bundle: %v, %+v", err, b)
	}
	t.Setenv("HOME", root)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, ".cache"))
	page, err := ExplorerPage(b, file)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := ExplorerPage(b, file); again != page || !strings.Contains(page, "graph-") {
		t.Errorf("explorer page %q, then %q", page, again)
	}
	if data, err := os.ReadFile(page); err != nil || !bytes.Equal(data, b.Explorer) {
		t.Errorf("explorer page content: %v", err)
	}
}

func TestCompression(t *testing.T) {
	for name, want := range map[string]string{"run.tar.zst": Zstd, "run.tzst": Zstd, "run.tar.gz": Gzip, "run.tgz": Gzip, "": Gzip} {
		if got := Compression(name); got != want {
			t.Errorf("Compression(%q) = %s, want %s", name, got, want)
		}
	}
}

func writeJSON(t *testing.T, file string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
