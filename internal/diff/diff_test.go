package diff

import (
	"strings"
	"testing"

	"github.com/zalgonoise/codegraph/internal/model"
)

func ep(id, transport, role string, dir model.Direction, keys ...string) model.Endpoint {
	return model.Endpoint{ID: id, Transport: transport, Role: role, Direction: dir, Keys: keys}
}

// The head publishes a new topic another service consumes (a new link), stops
// calling a gRPC service (a removed link) and adds a topic nobody consumes.
func TestCompare(t *testing.T) {
	base := []*model.Manifest{
		{Service: "a", Endpoints: []model.Endpoint{ep("call", "grpc", "client", model.Outbound, "Ledger/Credit")}},
		{Service: "b", Endpoints: []model.Endpoint{
			ep("srv", "grpc", "server", model.Inbound, "Ledger"),
			ep("sub", "kafka", "consumer", model.Inbound, "orders.created"),
		}},
	}
	head := []*model.Manifest{
		{Service: "a", Endpoints: []model.Endpoint{
			ep("pub", "kafka", "producer", model.Outbound, "orders.created"),
			ep("lost", "kafka", "producer", model.Outbound, "orders.audited"),
		}},
		base[1],
	}
	r := Compare(base, head)
	if len(r.Links.Added) != 1 || r.Links.Added[0].Key != "orders.created" ||
		len(r.Links.Removed) != 1 || r.Links.Removed[0].Transport != "grpc" {
		t.Errorf("links = %+v", r.Links)
	}
	if d := r.Endpoints["a"]; len(d.Added) != 2 || len(d.Removed) != 1 || d.Removed[0] != "outbound grpc client Ledger/Credit" {
		t.Errorf("endpoints of a = %+v", d)
	}
	if _, ok := r.Endpoints["b"]; ok {
		t.Errorf("b did not change: %+v", r.Endpoints["b"])
	}
	if len(r.Orphans.Added) != 1 || r.Orphans.Added[0] != "a kafka producer orders.audited" {
		t.Errorf("orphans = %+v", r.Orphans)
	}
	if !r.LinksChanged() {
		t.Error("links changed, LinksChanged says no")
	}
	md := Markdown(r, "base", "head")
	for _, want := range []string{"## Cross-service links (1 added, 1 removed)", "| + | a | b | kafka | `orders.created` |", "- + `a kafka producer orders.audited`"} {
		if !strings.Contains(md, want) {
			t.Errorf("report lacks %q:\n%s", want, md)
		}
	}
	if same := Compare(base, base); !same.Empty() || !strings.Contains(Markdown(same, "x", "x"), "No architectural change") {
		t.Errorf("a run compared with itself: %+v", same)
	}
}
