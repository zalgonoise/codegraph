package link

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/zalgonoise/codegraph/internal/model"
)

func endpoint(id, transport, role string, dir model.Direction, kind string, keys ...string) model.Endpoint {
	return model.Endpoint{ID: id, Transport: transport, Role: role, Direction: dir, Kind: kind, Keys: keys}
}

func TestLinkKind(t *testing.T) {
	a := &model.Manifest{Service: "a", Endpoints: []model.Endpoint{
		endpoint("pub", "nats", "producer", model.Outbound, "", "stock.changed"),
		endpoint("req", "nats", "producer", model.Outbound, model.Sync, "stock.check"),
	}}
	b := &model.Manifest{Service: "b", Endpoints: []model.Endpoint{
		endpoint("sub1", "nats", "consumer", model.Inbound, "", "stock.changed"),
		endpoint("sub2", "nats", "consumer", model.Inbound, "", "stock.check"),
	}}
	got := map[string]string{}
	for _, l := range Link([]*model.Manifest{a, b}).Links {
		got[l.Key] = l.Kind
	}
	if got["stock.changed"] != model.Async || got["stock.check"] != model.Sync {
		t.Errorf("kinds = %v, want stock.changed async (default) and stock.check sync (endpoint kind)", got)
	}
}

// A journey calling one gRPC method of another service continues into that
// method's journey only, and a call back into a journey already on the path
// is marked as a cycle instead of being followed.
func TestJourneyChain(t *testing.T) {
	a := &model.Manifest{
		Service: "a",
		Endpoints: []model.Endpoint{
			endpoint("a-in", "http", "server", model.Inbound, "", "POST /pay"),
			endpoint("a-call", "grpc", "client", model.Outbound, "", "Ledger/Credit"),
		},
		Journeys: []model.Journey{{Entrypoint: "a-in", Transport: "http", Key: "POST /pay",
			Outbound: []model.JourneyOutbound{{Endpoint: "a-call", Kind: model.Sync}}}},
	}
	b := &model.Manifest{
		Service: "b",
		Endpoints: []model.Endpoint{
			endpoint("b-in", "grpc", "server", model.Inbound, "", "Ledger"),
			endpoint("b-back", "http", "client", model.Outbound, "", "POST http://a/pay"),
		},
		Journeys: []model.Journey{
			{Entrypoint: "b-in", Transport: "grpc", Key: "Ledger/Balance"},
			{Entrypoint: "b-in", Transport: "grpc", Key: "Ledger/Credit", Handler: "credit",
				Outbound: []model.JourneyOutbound{{Endpoint: "b-back", Kind: model.Sync}}},
		},
	}
	sys := Link([]*model.Manifest{a, b})

	var aj *model.SystemJourney
	for i, sj := range sys.Journeys {
		if sj.Service == "a" {
			aj = &sys.Journeys[i]
		}
	}
	if aj == nil || len(aj.Hops) != 1 {
		t.Fatalf("journeys of a: %+v", sys.Journeys)
	}
	hop := aj.Hops[0]
	if hop.To != "b" || hop.Journey != "Ledger/Credit" || hop.Handler != "credit" || hop.Kind != model.Sync {
		t.Errorf("hop = %+v, want a sync hop into b's Ledger/Credit journey", hop)
	}
	if len(hop.Next) != 1 || hop.Next[0].To != "a" || !hop.Next[0].Cycle {
		t.Errorf("next = %+v, want the call back into a marked as a cycle", hop.Next)
	}
}

// A link between endpoints with several keys carries all the matching keys.
func TestLinkAllKeys(t *testing.T) {
	a := &model.Manifest{Service: "a", Endpoints: []model.Endpoint{
		endpoint("pub", "amqp", "producer", model.Outbound, "", "notify.account.changed", "notify.order.changed"),
	}}
	b := &model.Manifest{Service: "b", Endpoints: []model.Endpoint{
		endpoint("sub", "amqp", "consumer", model.Inbound, "", "notify.account.changed", "notify.order.changed", "other"),
	}}
	links := Link([]*model.Manifest{a, b}).Links
	if len(links) != 1 || links[0].Key != "notify.account.changed, notify.order.changed" || links[0].Confidence != "exact" {
		t.Errorf("links = %+v, want one exact link over both orchestrator queues", links)
	}
}

// Without a known host, an HTTP client links on its path only, and never
// when the path is all wildcards (a generic client to some external API).
func TestPathOnlyHTTP(t *testing.T) {
	a := &model.Manifest{Service: "a", Endpoints: []model.Endpoint{
		endpoint("generic", "http", "client", model.Outbound, "", "POST */*"),
		endpoint("cb", "http", "client", model.Outbound, "", "POST */undertaker/callback"),
	}}
	b := &model.Manifest{Service: "b", Endpoints: []model.Endpoint{
		endpoint("metrics", "http", "server", model.Inbound, "", "GET /metrics"),
		endpoint("health", "http", "server", model.Inbound, "", "/health"),
		endpoint("any", "http", "server", model.Inbound, "", "*"),
		endpoint("callback", "http", "server", model.Inbound, "", "POST /undertaker/callback"),
	}}
	var got []string
	for _, l := range Link([]*model.Manifest{a, b}).Links {
		got = append(got, l.FromEndpoint+" -> "+l.Key+" ("+l.Confidence+")")
	}
	if len(got) != 1 || got[0] != "cb -> POST /undertaker/callback (path-only)" {
		t.Errorf("links = %v, want only the callback route, path-only", got)
	}
}

// Two servers of the same gRPC service name but different proto services
// (API versions) only link to clients of their own contract.
func TestGRPCContract(t *testing.T) {
	call := endpoint("call", "grpc", "client", model.Outbound, "", "NotifierService/Trigger")
	call.Contract = "notify.v1.NotifierService"
	v1 := endpoint("v1", "grpc", "server", model.Inbound, "", "NotifierService")
	v1.Contract = "notify.v1.NotifierService"
	v2 := endpoint("v2", "grpc", "server", model.Inbound, "", "NotifierService")
	v2.Contract = "notify.v2.NotifierService"
	legacy := endpoint("legacy", "grpc", "server", model.Inbound, "", "NotifierService") // contract unknown
	ms := []*model.Manifest{
		{Service: "caller", Endpoints: []model.Endpoint{call}},
		{Service: "notify-v1", Endpoints: []model.Endpoint{v1}},
		{Service: "notify-v2", Endpoints: []model.Endpoint{v2}},
		{Service: "notify-legacy", Endpoints: []model.Endpoint{legacy}},
	}
	var to []string
	for _, l := range Link(ms).Links {
		to = append(to, l.To)
	}
	if len(to) != 2 || to[0] != "notify-legacy" || to[1] != "notify-v1" {
		t.Errorf("links to %v, want notify-legacy (no contract) and notify-v1", to)
	}
}

// Tables used by mains of one module are internal to that repository; the
// same table name across modules is reported, as name-only evidence.
func TestSharedStores(t *testing.T) {
	q := endpoint("q", "sql", "query", model.Outbound, "", "refunds")
	ms := []*model.Manifest{
		{Service: "returns-grpc", Module: "example.com/returns", Endpoints: []model.Endpoint{q}},
		{Service: "returns-subscribers", Module: "example.com/returns", Endpoints: []model.Endpoint{q}},
		{Service: "cart", Module: "example.com/cart", Endpoints: []model.Endpoint{q}},
	}
	var got []string
	for _, l := range Link(ms).Links {
		got = append(got, l.From+"-"+l.To+" "+l.Confidence)
	}
	if len(got) != 2 || got[0] != "cart-returns-grpc name-only" || got[1] != "cart-returns-subscribers name-only" {
		t.Errorf("shared stores = %v, want cart with each returns main, name-only", got)
	}
}

// A Redis key pattern used by services of different modules is a shared
// store, like an SQL table.
func TestSharedStoreOwner(t *testing.T) {
	docs := endpoint("d", "sql", "query", model.Outbound, "", "documents")
	owned := func(owner string) model.Endpoint { ep := docs; ep.Owner = owner; ep.ID += owner; return ep }
	ms := []*model.Manifest{
		{Service: "people", Module: "example.com/people", Endpoints: []model.Endpoint{docs}},
		{Service: "documents", Module: "example.com/documents", Endpoints: []model.Endpoint{docs}},
		{Service: "identity-api", Module: "example.com/identity", Endpoints: []model.Endpoint{docs}},
		// a declared back office reading people's and identity's documents tables
		{Service: "backoffice", Module: "backoffice", Endpoints: []model.Endpoint{owned("people"), owned("identity"), owned("core")}},
		// another declared service using core's documents (core is not analysed)
		{Service: "reconcile", Module: "reconcile", Endpoints: []model.Endpoint{owned("core")}},
	}
	var got []string
	for _, l := range Link(ms).Links {
		got = append(got, l.From+"-"+l.To)
	}
	sort.Strings(got)
	want := "backoffice-identity-api backoffice-people backoffice-reconcile documents-identity-api documents-people identity-api-people"
	if strings.Join(got, " ") != want {
		t.Errorf("shared stores = %v, want %s (owned tables only with their owners, or the same owner)", got, want)
	}
}

func TestSharedRedisKeys(t *testing.T) {
	write := endpoint("w", "redis", "query", model.Outbound, "", "customers:customer:*", "*:*:*")
	read := endpoint("r", "redis", "query", model.Outbound, "", "customers:customer:*", "*:*:*") // a blank pattern links nothing
	other := endpoint("o", "sql", "query", model.Outbound, "", "customers:customer:*")           // another store kind
	ms := []*model.Manifest{
		{Service: "customers", Module: "example.com/customers", Endpoints: []model.Endpoint{write}},
		{Service: "fraud", Module: "example.com/fraud", Endpoints: []model.Endpoint{read}},
		{Service: "rates", Module: "example.com/rates", Endpoints: []model.Endpoint{other}},
	}
	var got []string
	for _, l := range Link(ms).Links {
		got = append(got, l.From+"-"+l.To+" "+l.Transport+" "+l.Key)
	}
	if len(got) != 1 || got[0] != "customers-fraud redis customers:customer:*" {
		t.Errorf("links = %v, want customers-fraud over the shared Redis key only", got)
	}
}

// Unmatched endpoints that are not orphans say why: a counterpart in the
// same service, a queue the broker feeds itself, or the same name on the
// other messaging transport (Kafka topics moving to RabbitMQ).
func TestDanglingReasons(t *testing.T) {
	a := &model.Manifest{Service: "a", Endpoints: []model.Endpoint{
		endpoint("retry-out", "kafka", "producer", model.Outbound, "", "order.tips.retry"),
		endpoint("retry-in", "kafka", "consumer", model.Inbound, "", "order.tips.retry"),
		endpoint("dlq", "amqp", "consumer", model.Inbound, "", "fraud.payment.dlq"),
		endpoint("moving", "kafka", "producer", model.Outbound, "", "fraud.chargeback"),
	}}
	b := &model.Manifest{Service: "b", Endpoints: []model.Endpoint{
		endpoint("moved", "amqp", "consumer", model.Inbound, "", "fraud.chargeback"),
		endpoint("legacy", "kafka", "consumer", model.Inbound, "", "returns.order.returned"),
	}}
	got := map[string]string{}
	for _, d := range Link([]*model.Manifest{a, b}).Dangling {
		got[d.Service+" "+d.Endpoint] = d.Reason
	}
	want := map[string]string{
		"a retry-out": model.DanglingSameService,
		"a retry-in":  model.DanglingSameService,
		"a dlq":       model.DanglingBroker,
		"a moving":    model.DanglingOtherTransport,
		"b moved":     model.DanglingOtherTransport,
		"b legacy":    "", // an orphan
	}
	if len(got) != len(want) {
		t.Fatalf("dangling = %v, want %v", got, want)
	}
	for k, w := range want {
		if r, ok := got[k]; !ok || r != w {
			t.Errorf("%s: reason %q, want %q", k, r, w)
		}
	}
}

// A journey reached by two routes in one chain (a→b→d and a→c→d) is
// followed once; the second route is marked repeated.
func TestUnlinkedKeys(t *testing.T) {
	// cart's RabbitMQ mux registers several queues; only some are published to over RabbitMQ.
	cart := &model.Manifest{Service: "cart", Module: "example.com/cart", Endpoints: []model.Endpoint{
		endpoint("mux", "amqp", "consumer", model.Inbound, "", "invoice.pdf.requested",
			"review.order.status.accepted", "review.payment.status.pending",
			"review.order.status.accepted.dlq", "*"),
	}}
	invoicing := &model.Manifest{Service: "invoicing", Module: "example.com/invoicing", Endpoints: []model.Endpoint{
		endpoint("req", "amqp", "producer", model.Outbound, "", "invoice.pdf.requested"),
	}}
	fraud := &model.Manifest{Service: "fraud", Module: "example.com/fraud", Endpoints: []model.Endpoint{
		endpoint("status", "kafka", "producer", model.Outbound, "", "review.order.status.accepted"),
	}}
	sys := Link([]*model.Manifest{cart, invoicing, fraud})
	var got []string
	for _, d := range sys.Dangling {
		got = append(got, fmt.Sprintf("%s %s %s partial=%v reason=%s", d.Service, d.Endpoint, d.Key, d.Partial, d.Reason))
	}
	want := []string{ // fraud's own producer links to nothing on Kafka; cart's linked mux gets one entry per reason
		"fraud status review.order.status.accepted partial=false reason=other-transport",
		"cart mux review.order.status.accepted partial=true reason=other-transport",
		"cart mux review.payment.status.pending partial=true reason=",
		"cart mux review.order.status.accepted.dlq partial=true reason=broker",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("dangling:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(sys.Links) != 1 || sys.Links[0].Key != "invoice.pdf.requested" {
		t.Errorf("links = %+v, want invoicing -> cart only", sys.Links)
	}
}

func TestJourneyRepeated(t *testing.T) {
	ms := []*model.Manifest{{
		Service: "a",
		Endpoints: []model.Endpoint{
			endpoint("in", "http", "server", model.Inbound, "", "POST /x"),
			endpoint("to-b", "kafka", "producer", model.Outbound, "", "b.in"),
			endpoint("to-c", "kafka", "producer", model.Outbound, "", "c.in"),
		},
		Journeys: []model.Journey{{Entrypoint: "in", Transport: "http", Key: "POST /x", Outbound: []model.JourneyOutbound{
			{Endpoint: "to-b", Kind: model.Async}, {Endpoint: "to-c", Kind: model.Async}}}},
	}}
	for _, svc := range []string{"b", "c"} {
		ms = append(ms, &model.Manifest{
			Service: svc,
			Endpoints: []model.Endpoint{
				endpoint("in", "kafka", "consumer", model.Inbound, "", svc+".in"),
				endpoint("to-d", "kafka", "producer", model.Outbound, "", "d.in"),
			},
			Journeys: []model.Journey{{Entrypoint: "in", Transport: "kafka", Key: svc + ".in",
				Outbound: []model.JourneyOutbound{{Endpoint: "to-d", Kind: model.Async}}}},
		})
	}
	ms = append(ms, &model.Manifest{
		Service: "d",
		Endpoints: []model.Endpoint{
			endpoint("in", "kafka", "consumer", model.Inbound, "", "d.in"),
			endpoint("q", "sql", "query", model.Outbound, "", "d_rows"),
		},
		Journeys: []model.Journey{{Entrypoint: "in", Transport: "kafka", Key: "d.in",
			Outbound: []model.JourneyOutbound{{Endpoint: "q", Kind: model.Sync}}}},
	})
	var hops []model.Hop
	for _, sj := range Link(ms).Journeys {
		if sj.Service == "a" {
			hops = sj.Hops
		}
	}
	if len(hops) != 2 || len(hops[0].Next) != 1 || len(hops[1].Next) != 1 {
		t.Fatalf("hops = %+v", hops)
	}
	first, second := hops[0].Next[0], hops[1].Next[0]
	if first.To != "d" || first.Repeated || len(first.Next) != 1 {
		t.Errorf("first route to d = %+v, want it followed", first)
	}
	if second.To != "d" || !second.Repeated || len(second.Next) != 0 {
		t.Errorf("second route to d = %+v, want it marked repeated", second)
	}
}

// A grpc-gateway route's call to its own service's server (generated code in a
// *.pb.gw.go file) continues into the method's journey, but makes no link; the
// same call from elsewhere stays unmatched, same-service.
func TestGatewaySelfContinuation(t *testing.T) {
	server := endpoint("srv", "grpc", "server", model.Inbound, "", "Ledger")
	gw := endpoint("gw", "grpc", "client", model.Outbound, "", "Ledger/Credit")
	gw.Position = "internal/pb/ledger.pb.gw.go:15"
	gw.Sites = []model.Site{{Position: gw.Position}}
	direct := endpoint("direct", "grpc", "client", model.Outbound, "", "Ledger/Balance")
	direct.Position = "main.go:63"
	m := &model.Manifest{Service: "ledger", Module: "example.com/ledger", Endpoints: []model.Endpoint{server, gw, direct},
		Journeys: []model.Journey{{Entrypoint: "srv", Transport: "grpc", Key: "Ledger/Credit"}}}
	sys := Link([]*model.Manifest{m})
	if len(sys.Links) != 0 {
		t.Errorf("links = %+v, want none: a service never links to itself", sys.Links)
	}
	cs := sys.Continuations["ledger|gw"]
	if len(cs) != 1 || cs[0].Link.To != "ledger" || cs[0].Link.Confidence != "gateway" || len(cs[0].Journeys) != 1 {
		t.Errorf("gateway continuations = %+v", cs)
	}
	var dangling []string
	for _, d := range sys.Dangling {
		dangling = append(dangling, d.Endpoint+" "+d.Reason)
	}
	if strings.Join(dangling, ",") != "direct same-service" {
		t.Errorf("dangling = %v, want only the direct client, same-service", dangling)
	}
}
