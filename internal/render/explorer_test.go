package render

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/zalgonoise/codegraph/internal/deploy"
	"github.com/zalgonoise/codegraph/internal/link"
	"github.com/zalgonoise/codegraph/internal/model"
)

// The explorer carries each journey's outbound endpoints with the journeys
// they continue into (for a gRPC call, only the method's), embedded in the
// page as JSON.
func TestExplorer(t *testing.T) {
	a := &model.Manifest{
		Service: "a",
		Endpoints: []model.Endpoint{
			{ID: "in", Transport: "http", Role: "server", Direction: model.Inbound, Keys: []string{"POST /pay"}},
			{ID: "call", Transport: "grpc", Role: "client", Direction: model.Outbound, Keys: []string{"Ledger/Credit"}},
			{ID: "q", Transport: "sql", Role: "query", Direction: model.Outbound, Keys: []string{"payments"}},
		},
		Journeys: []model.Journey{{Entrypoint: "in", Transport: "http", Key: "POST /pay",
			Outbound: []model.JourneyOutbound{{Endpoint: "call", Kind: model.Sync}, {Endpoint: "q", Kind: model.Sync}}}},
	}
	b := &model.Manifest{
		Service: "b",
		Endpoints: []model.Endpoint{
			{ID: "srv", Transport: "grpc", Role: "server", Direction: model.Inbound, Keys: []string{"Ledger"}},
		},
		Journeys: []model.Journey{
			{Entrypoint: "srv", Transport: "grpc", Key: "Ledger/Balance"},
			{Entrypoint: "srv", Transport: "grpc", Key: "Ledger/Credit"},
		},
	}
	ms := []*model.Manifest{a, b}
	sys := link.Link(ms)

	d := explorerData(sys, ms)
	next := d.Next["a|call"]
	if len(next) != 1 || next[0].To != "b" || next[0].ToEndpoint != "srv" || len(next[0].Journeys) != 1 ||
		d.Journeys[next[0].Journeys[0]].Key != "Ledger/Credit" {
		t.Errorf("continuations of a|call = %+v, want b's Ledger/Credit journey only", next)
	}
	if _, ok := d.Next["a|q"]; ok {
		t.Errorf("an unlinked store has continuations: %+v", d.Next["a|q"])
	}

	page, err := Explorer(sys, ms, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)<script id="codegraph-data" type="application/json">(.*?)</script>`).FindStringSubmatch(page)
	if m == nil || strings.Contains(page, explorerDataMark) {
		t.Fatal("the page does not embed its data")
	}
	var back exploreData
	if err := json.Unmarshal([]byte(m[1]), &back); err != nil || len(back.Journeys) != 3 {
		t.Errorf("embedded data: %v, %d journeys", err, len(back.Journeys))
	}
	// The diagrams drawn on demand: the system's and each service's, as text
	// in an inert script; Mermaid itself is only fetched when one is drawn.
	dm := regexp.MustCompile(`(?s)<script id="codegraph-diagrams" type="application/json">(.*?)</script>`).FindStringSubmatch(page)
	if dm == nil || strings.Contains(page, explorerDiagramsMark) {
		t.Fatal("the page does not embed its diagrams")
	}
	var diagrams map[string]string
	if err := json.Unmarshal([]byte(dm[1]), &diagrams); err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if !strings.HasPrefix(diagrams["service:"+m.Service], "flowchart") {
			t.Errorf("no service diagram for %s", m.Service)
		}
	}
	if !strings.HasPrefix(diagrams["system"], "flowchart") || strings.Count(page, "mermaid@11") != 1 ||
		strings.Contains(page, `<script src="https://`) {
		t.Error("the system diagram is embedded, and Mermaid is imported on demand only")
	}
}

// A system journey too long to draw is summarised in system.md, with its
// first steps only.
func TestSystemReportLongJourney(t *testing.T) {
	sj := model.SystemJourney{Service: "a", Transport: "http", Key: "POST /x", Kind: model.Sync}
	for i := 0; i < maxSequenceHops+1; i++ {
		sj.Hops = append(sj.Hops, model.Hop{From: "a", External: true, Transport: "sql", Role: "query",
			Key: fmt.Sprint("t", i), Kind: model.Sync})
	}
	md := SystemReport(&model.System{Journeys: []model.SystemJourney{sj}}, "")
	if !strings.Contains(md, "201 steps across 0 other services") || strings.Contains(md, "sequenceDiagram") {
		t.Errorf("long journey not summarised:\n%s", md[:min(len(md), 600)])
	}
}

func TestExplorerDeployment(t *testing.T) {
	a := &model.Manifest{Service: "a", Endpoints: []model.Endpoint{
		{ID: "call", Transport: "grpc", Role: "client", Direction: model.Outbound, Keys: []string{"Ledger/Credit"}}}}
	b := &model.Manifest{Service: "b", Endpoints: []model.Endpoint{
		{ID: "srv", Transport: "grpc", Role: "server", Direction: model.Inbound, Keys: []string{"Ledger"}}}}
	ms := []*model.Manifest{a, b}
	one := 1
	dep := &deploy.Deployment{
		Environment: "production",
		Workloads: []deploy.Workload{{Kind: "Deployment", Name: "a", Replicas: &one, Containers: []deploy.Container{{
			Name: "a", Image: "team-a", Command: []string{"/bin/app"}, Service: "a",
			Env: map[string]string{"LEDGER_HOST": "b", "LEDGER_MOCKED": "true"}, Secrets: []string{"DB_PASSWORD"}}}}},
		Network: &deploy.Network{Mocked: []deploy.Mock{{From: "a", Setting: "LEDGER_MOCKED", To: []string{"b"}}}},
	}
	page, err := Explorer(link.Link(ms), ms, dep)
	if err != nil {
		t.Fatal(err)
	}
	var data exploreData
	if m := regexp.MustCompile(`(?s)<script id="codegraph-data" type="application/json">(.*?)</script>`).FindStringSubmatch(page); m == nil ||
		json.Unmarshal([]byte(m[1]), &data) != nil || data.Deploy == nil {
		t.Fatal("the page does not embed the deployment")
	}
	va, vb := data.Deploy.Services["a"], data.Deploy.Services["b"]
	if va == nil || len(va.Workloads) != 1 || va.Workloads[0].Env != nil || strings.Join(va.Workloads[0].Mocks, ",") != "LEDGER_MOCKED" ||
		vb == nil || len(vb.Workloads) != 0 || len(data.Deploy.Mocked) != 1 {
		t.Errorf("deployment views: a %+v, b %+v, mocked %v (b is not deployed; settings stay out of the data)", va, vb, data.Deploy.Mocked)
	}
	var settings map[string]exploreSettings // read on demand, from their own inert script
	if m := regexp.MustCompile(`(?s)<script id="codegraph-settings" type="application/json">(.*?)</script>`).FindStringSubmatch(page); m == nil ||
		json.Unmarshal([]byte(m[1]), &settings) != nil {
		t.Fatal("the page does not embed the settings")
	}
	if s := settings["a|a"]; s.Env["LEDGER_HOST"] != "b" || strings.Join(s.Secrets, ",") != "DB_PASSWORD" {
		t.Errorf("settings of a's workload: %+v", s)
	}
}
