package declared

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalgonoise/codegraph/internal/model"
)

func TestManifest(t *testing.T) {
	f := &File{
		Service: "profiles", Repository: "profiles", Language: "python", Workloads: []string{"profiles"},
		Settings: []string{"DB_HOST", "AML_HOST", "DB_HOST"},
		Endpoints: []Endpoint{
			{Direction: model.Inbound, Transport: "grpc", Role: "server", Keys: []string{"IdentityService"},
				Contract: "profiles.identity.v1.IdentityService", Handler: "IdentityServicer", Position: "profiles/grpc/identity.py:10",
				Methods: map[string][]string{"GetIdentity": {"sql identity"}, "ListIdentities": {"sql identity", "sql identity"}}},
			{Direction: model.Inbound, Transport: "kafka", Role: "consumer", Keys: []string{"compliance.identity.status.declined"},
				Handler: "on_declined", Position: "profiles/consumers.py:40", Reaches: []string{"*"}},
			{Direction: model.Inbound, Transport: "http", Role: "server", Keys: []string{"GET /health"}},
			{Direction: model.Outbound, Transport: "sql", Role: "query", Keys: []string{"identity"}, Position: "profiles/models/identity.py:7"},
			{Direction: model.Outbound, Transport: "grpc", Role: "client", Keys: []string{"IdentityService/GetSession"},
				Contract: "compliance.identity.v1.IdentityService", Position: "clients/identity.py:22"},
		},
	}
	m, err := f.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if m.Module != "profiles" || m.Declared == nil || m.Declared.Language != "python" || strings.Join(m.Settings, ",") != "AML_HOST,DB_HOST" {
		t.Errorf("manifest: module %q declared %+v settings %v", m.Module, m.Declared, m.Settings)
	}
	ids := map[string]model.Endpoint{}
	for _, ep := range m.Endpoints {
		ids[ep.ID] = ep
		if ep.Source != "declared" || len(ep.Tags) != 1 {
			t.Errorf("endpoint %v: source %q tags %v", ep.Keys, ep.Source, ep.Tags)
		}
	}
	var got []string
	for _, j := range m.Journeys {
		if _, ok := ids[j.Entrypoint]; !ok {
			t.Errorf("journey %s: entrypoint %s is no endpoint", j.Key, j.Entrypoint)
		}
		var outs []string
		for _, o := range j.Outbound {
			outs = append(outs, ids[o.Endpoint].Transport+" "+ids[o.Endpoint].Keys[0]+" "+o.Kind)
		}
		got = append(got, j.Key+" in="+j.Component+": "+strings.Join(outs, ", "))
	}
	want := []string{
		"IdentityService/GetIdentity in=profiles/grpc: sql identity sync",
		"IdentityService/ListIdentities in=profiles/grpc: sql identity sync", // listed twice, used once
		"compliance.identity.status.declined in=profiles: sql identity sync, grpc IdentityService/GetSession sync",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("journeys:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestInvalid(t *testing.T) {
	for name, tc := range map[string]struct {
		ep   Endpoint
		want string
	}{
		"direction":   {Endpoint{Direction: "in", Transport: "grpc", Role: "server", Keys: []string{"X"}}, "direction"},
		"keys":        {Endpoint{Direction: model.Inbound, Transport: "grpc", Role: "server"}, "keys are required"},
		"reaches out": {Endpoint{Direction: model.Outbound, Transport: "sql", Role: "query", Keys: []string{"t"}, Reaches: []string{"*"}}, "only inbound"},
		"methods":     {Endpoint{Direction: model.Inbound, Transport: "http", Role: "server", Keys: []string{"GET /"}, Methods: map[string][]string{"X": nil}}, "gRPC servers"},
		"unknown ref": {Endpoint{Direction: model.Inbound, Transport: "kafka", Role: "consumer", Keys: []string{"t"}, Reaches: []string{"sql nope"}}, "no outbound endpoint"},
	} {
		f := &File{Service: "s", Repository: "r", Endpoints: []Endpoint{tc.ep}}
		if _, err := f.Manifest(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestReadUnknownField(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(p, []byte(`{"service":"s","repository":"r","endpoints":[{"direction":"inbound","transport":"kafka","role":"consumer","keys":["t"],"reach":["*"]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(p); err == nil || !strings.Contains(err.Error(), `unknown field "reach"`) {
		t.Errorf("err = %v, want the misspelt field", err)
	}
}
