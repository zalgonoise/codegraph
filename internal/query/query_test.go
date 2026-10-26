package query

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalgonoise/codegraph/internal/deploy"
	"github.com/zalgonoise/codegraph/internal/inventory"
	"github.com/zalgonoise/codegraph/internal/link"
	"github.com/zalgonoise/codegraph/internal/model"
)

// shop takes orders over HTTP and publishes orders.created; stock consumes it
// and keeps a table; audit reads the same table from another module.
func testRun(t *testing.T) *Run {
	t.Helper()
	ep := func(id, transport, role string, dir model.Direction, pos string, keys ...string) model.Endpoint {
		return model.Endpoint{ID: id, Transport: transport, Role: role, Direction: dir, Keys: keys, Position: pos}
	}
	ms := []*model.Manifest{
		{Service: "shop", Module: "example.com/shop", Endpoints: []model.Endpoint{
			ep("in", "http", "server", model.Inbound, "api.go:10", "POST /orders"),
			ep("pub", "kafka", "producer", model.Outbound, "events.go:20", "orders.created"),
		}, Journeys: []model.Journey{{Entrypoint: "in", Transport: "http", Key: "POST /orders", Handler: "create",
			Roots: []model.Site{{Position: "api.go:12"}}, Outbound: []model.JourneyOutbound{{Endpoint: "pub", Kind: model.Async}}}}},
		{Service: "stock", Module: "example.com/stock", Endpoints: []model.Endpoint{
			ep("sub", "kafka", "consumer", model.Inbound, "main.go:5", "orders.created"),
			ep("q", "sql", "query", model.Outbound, "store.go:7", "stock_items"),
		}, Journeys: []model.Journey{{Entrypoint: "sub", Transport: "kafka", Key: "orders.created", Handler: "reserve",
			Outbound: []model.JourneyOutbound{{Endpoint: "q", Kind: model.Sync}}}}},
		{Service: "audit", Module: "example.com/audit", Endpoints: []model.Endpoint{
			ep("q", "sql", "query", model.Outbound, "audit.go:3", "stock_items"),
		}},
	}
	return New("run", ms, link.Link(ms))
}

func TestQueries(t *testing.T) {
	r := testRun(t)

	j, err := r.Journey("shop", "orders", 2) // part of the key is enough
	if err != nil {
		t.Fatal(err)
	}
	if len(j.Steps) != 1 || j.Steps[0].To != "stock" || j.Steps[0].Journey != "orders.created" ||
		len(j.Steps[0].Steps) != 1 || j.Steps[0].Steps[0].Unmatched != "store" || j.Steps[0].Position != "events.go:20" {
		t.Errorf("journey steps = %+v", j.Steps)
	}
	if j, _ := r.Journey("shop", "POST /orders", 0); !j.Steps[0].Deeper {
		t.Errorf("depth 0 should not follow into stock: %+v", j.Steps)
	}

	who := r.Who("orders.created")
	if len(who) != 1 || len(who[0].Senders) != 1 || who[0].Senders[0].Service != "shop" ||
		len(who[0].Receivers) != 1 || who[0].Receivers[0].Journeys[0] != "kafka orders.created" {
		t.Errorf("who = %+v", who)
	}

	paths, err := r.Paths("shop", "stock")
	if err != nil || len(paths) != 1 || paths[0][0][0].Key != "orders.created" {
		t.Errorf("paths = %+v, %v", paths, err)
	}

	im, err := r.Impact("stock")
	if err != nil || len(im.Direct) != 1 || len(im.Shares) != 1 || len(im.Journeys) != 1 || im.Journeys[0].Key != "POST /orders" ||
		im.Journeys[0].Distance != 1 || im.Journeys[0].Via != "kafka orders.created" ||
		strings.Join(im.Journeys[0].Chain, ">") != "shop>stock" {
		t.Errorf("impact of stock = %+v, %v", im, err) // shop's link (audit's shared table apart); shop's journey, via the topic
	}
	im, err = r.Impact("stock_items")
	if err != nil || im.Kind != "key" || len(im.Users[0].Users) != 2 || len(im.Journeys) != 2 {
		t.Errorf("impact of the table = %+v, %v", im, err) // stock's consumer (distance 0) and shop's route (1)
	}

	if j, err := r.Journey("stock", "kafka orders.created", 1); err != nil || j.Transport != "kafka" {
		t.Errorf("a journey named with its transport: %+v, %v", j, err)
	}
	if _, err := r.Journey("stock", "amqp orders.created", 1); err == nil {
		t.Error("stock has no amqp journey")
	}
	if _, err := r.Service("stok"); err == nil || !strings.Contains(err.Error(), "stock") {
		t.Errorf("a misspelt service should suggest stock: %v", err)
	}
	if f := r.Search("orders"); len(f.Journeys) != 2 || len(f.Keys) != 2 {
		t.Errorf("search = %+v", f)
	}

	out, err := Answer(r, []string{"who", "orders.created"}, Options{Depth: 2, JSON: true})
	var back []KeyUsers
	if err != nil || json.Unmarshal([]byte(out), &back) != nil || len(back) != 1 {
		t.Errorf("JSON answer = %s, %v", out, err)
	}
	if md, err := Answer(r, []string{"journey", "shop", "POST", "/orders"}, Options{Depth: 2}); err != nil ||
		!strings.Contains(md, "→ **stock** `orders.created`") || !strings.Contains(md, "@ `events.go:20`") {
		t.Errorf("Markdown journey = %s, %v", md, err)
	}
}

func TestDiagrams(t *testing.T) {
	r := testRun(t)
	draw := func(args ...string) string {
		t.Helper()
		md, err := Answer(r, args, Options{Depth: 2, Diagram: true})
		if err != nil {
			t.Fatal(err)
		}
		_, diagram, _ := strings.Cut(md, "```mermaid\n")
		diagram, _, _ = strings.Cut(diagram, "```")
		return diagram
	}
	for _, c := range []struct {
		args []string
		want []string
	}{
		{[]string{"journey", "shop", "POST /orders"}, []string{"sequenceDiagram\n",
			"participant p1 as shop", "p1-)p2: kafka orders.created", "Note over p2: reserve", "p2->>p3: query",
			"participant p3 as SQL stock_items"}},
		{[]string{"service", "stock"}, []string{"flowchart LR\n", `n0("shop")`, `n1("stock")`, `n2("audit")`,
			`n0 -.->|"kafka orders.created"| n1`, `n2 -.-|"shared sql stock_items"| n1`, "class n1 focus"}},
		{[]string{"who", "stock_items"}, []string{`n1[("SQL<br/>stock_items")]`, `n0 -.- n1`, `n2 -.- n1`}},
		{[]string{"path", "shop", "stock"}, []string{`n0 -.->|"kafka orders.created"| n1`, "class n0,n1 focus"}},
		{[]string{"impact", "orders.created"}, []string{`n0("shop")`, `n1(["KAFKA<br/>orders.created"])`, `n0 -.-> n1`, `n1 -.-> n2`}},
	} {
		got := draw(c.args...)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%v: lacks %q:\n%s", c.args, w, got)
			}
		}
	}
	if md, _ := Answer(fanOut(), []string{"journey", "hub", "start"}, Options{Depth: 2, Diagram: true}); !strings.Contains(md,
		"_The diagram follows 0 service(s) beyond this one, not 2: deeper, it is too large to draw") || !strings.Contains(md, "sequenceDiagram") {
		t.Errorf("a journey too large to draw at depth 2 is drawn at 0:\n%s", md)
	}
	if md, _ := Answer(r, []string{"services"}, Options{Diagram: true}); !strings.Contains(md, "draw no diagram") {
		t.Errorf("services draws none:\n%s", md)
	}

	// Explorer links: none without a page, then the page's routes.
	if md, _ := Answer(r, []string{"service", "shop"}, Options{}); strings.Contains(md, "explorer") {
		t.Errorf("no explorer page, no link:\n%s", md)
	}
	r.UseExplorer("file:///run/_system/system.html")
	md, _ := Answer(r, []string{"who", "orders.created"}, Options{})
	if !strings.HasSuffix(md, "\nIn the explorer: [kafka orders.created](file:///run/_system/system.html#/k/kafka/orders.created).\n") {
		t.Errorf("who's explorer link:\n%s", md)
	}
	var ill struct {
		Answer   []KeyUsers
		Diagram  string
		Explorer []string
	}
	out, err := Answer(r, []string{"who", "orders.created"}, Options{JSON: true, Diagram: true})
	if err != nil || json.Unmarshal([]byte(out), &ill) != nil || len(ill.Answer) != 1 ||
		!strings.HasPrefix(ill.Diagram, "flowchart LR") || len(ill.Explorer) != 1 {
		t.Errorf("JSON with a diagram = %s, %v", out, err)
	}
	if r.UseExplorer("none"); r.Explorer != "" {
		t.Error("none drops the links")
	}
}

// fanOut is a run whose hub journey sends 10 topics, each consumed by a
// journey of 25 queries: 10 steps at depth 0, too many to draw at depth 1.
func fanOut() *Run {
	hub := &model.Manifest{Service: "hub", Module: "example.com/hub", Endpoints: []model.Endpoint{
		{ID: "in", Transport: "http", Role: "server", Direction: model.Inbound, Keys: []string{"start"}}}}
	sink := &model.Manifest{Service: "sink", Module: "example.com/sink"}
	j := model.Journey{Entrypoint: "in", Transport: "http", Key: "start"}
	for i := range 10 {
		topic := fmt.Sprintf("t%d", i)
		hub.Endpoints = append(hub.Endpoints, model.Endpoint{ID: "p" + topic, Transport: "kafka", Role: "producer",
			Direction: model.Outbound, Keys: []string{topic}})
		j.Outbound = append(j.Outbound, model.JourneyOutbound{Endpoint: "p" + topic, Kind: model.Async})
		sink.Endpoints = append(sink.Endpoints, model.Endpoint{ID: "c" + topic, Transport: "kafka", Role: "consumer",
			Direction: model.Inbound, Keys: []string{topic}})
		sj := model.Journey{Entrypoint: "c" + topic, Transport: "kafka", Key: topic}
		for q := range 25 {
			id := fmt.Sprintf("q%d.%d", i, q)
			sink.Endpoints = append(sink.Endpoints, model.Endpoint{ID: id, Transport: "sql", Role: "query",
				Direction: model.Outbound, Keys: []string{id}})
			sj.Outbound = append(sj.Outbound, model.JourneyOutbound{Endpoint: id, Kind: model.Sync})
		}
		sink.Journeys = append(sink.Journeys, sj)
	}
	hub.Journeys = []model.Journey{j}
	ms := []*model.Manifest{hub, sink}
	return New("run", ms, link.Link(ms))
}

func TestDeployed(t *testing.T) {
	r := testRun(t)
	if d, _ := r.Service("shop"); d.Deployed != nil || strings.Contains(ServiceMarkdown(d), "eployed") {
		t.Error("without a deploy.json, nothing is said about deployment")
	}
	two := 2
	r.Deploy = &deploy.Deployment{Environment: "production",
		Workloads: []deploy.Workload{{Kind: "Deployment", Name: "shop-api", Replicas: &two, Services: []string{"shop"},
			Containers: []deploy.Container{{Name: "shop", Service: "shop", Command: []string{"/bin/app", "api"},
				Env: map[string]string{"STOCK_MOCKED": "true", "PORT": "8080", "TOKEN": deploy.Secret}}}}},
		Services: []deploy.Service{{Name: "shop", Ports: []deploy.Port{{Port: 80}}}}}
	d, err := r.Service("shop")
	if err != nil {
		t.Fatal(err)
	}
	md := ServiceMarkdown(d)
	if !strings.Contains(md, "## Deployed in production (1 workloads)") ||
		!strings.Contains(md, "- shop-api (deployment, 2 replicas): `/bin/app api`; hosts shop:80; **mocked:** STOCK_MOCKED; 3 settings") {
		t.Errorf("deployed:\n%s", md)
	}
	if d, _ := r.Service("stock"); !strings.Contains(ServiceMarkdown(d), "## Not deployed in production") {
		t.Error("stock runs in no workload")
	}
}

func TestDeployedAddresses(t *testing.T) {
	r := testRun(t)
	r.Deploy = &deploy.Deployment{Environment: "production",
		Workloads: []deploy.Workload{{Kind: "Deployment", Name: "shop", Containers: []deploy.Container{{Name: "shop", Service: "shop"}}}},
		Network: &deploy.Network{
			ConfigOnly: []deploy.Pair{{From: "shop", To: "stock", Settings: []string{"STOCK_HOST=stock"}}},
			Settled:    []deploy.Settled{{From: "shop", Key: "SenderService", To: "sender-grpc", NotDialled: []string{"sender"}, Setting: "GRPC_SENDER_HOST=sender-grpc"}},
		}}
	d, _ := r.Service("shop")
	md := ServiceMarkdown(d)
	for _, s := range []string{"- **configured, no link in the code** (1; a client the analysis does not read, or an unused setting): stock (`STOCK_HOST=stock`)",
		"- `SenderService` is served by several services: it dials **sender-grpc** (`GRPC_SENDER_HOST=sender-grpc`), not sender"} {
		if !strings.Contains(md, s) {
			t.Errorf("lacks %q:\n%s", s, md)
		}
	}
}

func TestUses(t *testing.T) {
	r := testRun(t)
	shop := r.Manifests[0]
	shop.Libraries = []model.Library{{Module: "example.com/bus", Symbols: []string{"(*bus.Client).Publish", "bus.New"}}}
	shop.Journeys[0].Uses = []model.SymbolUse{{Module: "example.com/bus", Package: "example.com/bus", Symbol: "(*bus.Client).Publish", Position: "events.go:20", Calls: 2}}
	r = New("run", r.Manifests, r.System)

	us := r.Uses("example.com/bus") // a module: every symbol of it
	if len(us) != 2 || us[0].Symbol != "(*bus.Client).Publish" || len(us[0].Journeys) != 1 || us[0].Journeys[0].Site != "events.go:20" ||
		us[0].Journeys[0].Calls != 2 || strings.Join(us[0].Services, ",") != "shop" || us[1].Symbol != "bus.New" || len(us[1].Journeys) != 0 {
		t.Errorf("uses of the module = %+v", us)
	}
	if us := r.Uses("publish"); len(us) != 1 || us[0].Symbol != "(*bus.Client).Publish" { // part of a name, any case
		t.Errorf("uses of publish = %+v", us)
	}
	md, err := Answer(r, []string{"uses", "bus.New"}, Options{})
	if err != nil || !strings.Contains(md, "## `bus.New` (example.com/bus)") || !strings.Contains(md, "Services: shop.") {
		t.Errorf("Markdown uses = %s, %v", md, err)
	}
	if md, _ := Answer(r, []string{"uses", "nothing"}, Options{}); !strings.Contains(md, "Nothing analysed calls") {
		t.Errorf("no match: %s", md)
	}
}

// image: the repositories building or using an image, and the deployment's
// containers running it, from a reference with registry and tag.
func TestImage(t *testing.T) {
	r := testRun(t)
	if a := r.Image("shop"); a.Note == "" || len(a.Builders) > 0 {
		t.Errorf("without an inventory or a deployment: %+v", a)
	}
	dir := t.TempDir()
	inv := inventory.Inventory{Repos: []inventory.Repo{
		{Name: "shop", LastCommit: "2026-10-01", Images: []inventory.Image{{Name: "team-shop", Tag: "release-*", Dockerfile: "Dockerfile",
			File: "bitbucket-pipelines.yml:12", Via: "docker push"}}},
		{Name: "charts", References: []inventory.Image{{Name: "team-shop", File: "values.yaml:3", Via: "helm values"}}},
	}}
	data, _ := json.Marshal(inv)
	if err := os.WriteFile(filepath.Join(dir, "inventory.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.UseInventory(dir); err != nil { // a directory holding inventory.json
		t.Fatal(err)
	}
	r.Deploy = &deploy.Deployment{Environment: "staging", Workloads: []deploy.Workload{{Kind: "Deployment", Name: "shop-api",
		Containers: []deploy.Container{{Name: "api", Image: "team-shop", Tag: "release-v2", Repo: "shop", Built: true, Service: "shop"}}}}}
	a := r.Image("123.dkr.ecr.example.com/team-shop:release-v2")
	if len(a.Builders) != 1 || len(a.Referrers) != 1 || len(a.Deployed) != 1 || a.Note != "" {
		t.Fatalf("answer: %+v", a)
	}
	md, err := Answer(r, []string{"image", "proxy/team/team-shop:latest"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"- **shop** builds `team-shop`:`release-*` @ `bitbucket-pipelines.yml:12` (docker push, `Dockerfile`, last commit 2026-10-01)",
		"- **charts** uses `team-shop` @ `values.yaml:3` (helm values)",
		"## Runs in staging (1 containers)",
		"- workload `shop-api`, container `api`: `team-shop:release-v2` → repository **shop** (its CI builds the image), service `shop`",
	} {
		if !strings.Contains(md, s) {
			t.Errorf("lacks %q:\n%s", s, md)
		}
	}
	if err := r.UseInventory(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("a missing inventory should be an error")
	}
}
