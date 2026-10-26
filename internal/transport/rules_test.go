package transport

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalgonoise/codegraph/internal/model"
)

func TestDefaultsCompile(t *testing.T) {
	rules, err := LoadRules(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, r := range rules {
		kinds[r.Name] = r.Kind
	}
	for _, name := range []string{"nats-request", "nats-request-ctx"} {
		if kinds[name] != model.Sync {
			t.Errorf("%s: kind %q, want sync (request/reply)", name, kinds[name])
		}
	}
	if kinds["nats-publish"] != "" {
		t.Errorf("nats-publish: kind %q, want the async default", kinds["nats-publish"])
	}
}

func TestRuleValidation(t *testing.T) {
	for _, r := range []Rule{
		{Name: "bad-kind", Transport: "nats", Direction: model.Outbound, Kind: "maybe"},
		{Name: "bad-transform", Transport: "sql", Direction: model.Outbound, Transform: "tables"},
		{Name: "bad-recv", Transport: "grpc", Direction: model.Outbound, RecvPattern: "("},
		{Name: "bad-method", Transport: "kafka", Direction: model.Outbound, KeyMethods: []MethodRef{{Arg: 2}}},
	} {
		if err := r.compile(); err == nil {
			t.Errorf("%s: compiled without error", r.Name)
		}
	}
}

func TestGRPCCallRule(t *testing.T) {
	rules, err := LoadRules(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	var call *Rule
	for i := range rules {
		if rules[i].Name == "grpc-call" {
			call = &rules[i]
		}
	}
	sig := "func(ctx context.Context, in *pb.Req, opts ...google.golang.org/grpc.CallOption) (*pb.Resp, error)"
	for _, tc := range []struct {
		recv string
		want bool
	}{
		{"HookServiceClient", true},
		{"hookServiceClient", false},   // generated implementation
		{"ClientConnInterface", false}, // cc.Invoke inside generated code
	} {
		if _, ok := call.match(Callee{Recv: tc.recv, Name: "Trigger", Sig: sig}); ok != tc.want {
			t.Errorf("grpc-call on %s: match %v, want %v", tc.recv, ok, tc.want)
		}
	}
}

// The samples' in-house rules (kafkabus, rabbitbus, kvcache) must stay loadable.
func TestInhouseRules(t *testing.T) {
	rules, err := LoadRules([]string{"../../testdata/rules/inhouse.json"}, false)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, r := range rules {
		names[r.Name] = true
	}
	for _, want := range []string{"kafkabus-handle", "kafkabus-publish", "kvcache-key", "rabbitbus-mux-handle", "rabbitbus-broker-consume", "rabbitbus-publish"} {
		if !names[want] {
			t.Errorf("testdata/rules/inhouse.json misses %s", want)
		}
	}
}

func TestJoinPath(t *testing.T) {
	for _, tc := range [][3]string{
		{"", "/x", "/x"},
		{"/", "/x", "/x"},
		{"/api", "/x", "/api/x"},
		{"/api/", "x", "/api/x"},
		{"/api", "", "/api"},
		{"*", "/x", "*/x"},
		{"*", "*/x", "*/x"},
	} {
		if got := joinPath(tc[0], tc[1]); got != tc[2] {
			t.Errorf("joinPath(%q, %q) = %q, want %q", tc[0], tc[1], got, tc[2])
		}
	}
}

// Every rule file of the profile (CODEGRAPH_PROFILE) must stay loadable: its
// shared rules and its per-repository ones. Without a profile, nothing to check.
func TestProfileRuleFiles(t *testing.T) {
	dir := os.Getenv("CODEGRAPH_PROFILE")
	if dir == "" {
		t.Skip("no profile (CODEGRAPH_PROFILE)")
	}
	var files []string
	err := filepath.WalkDir(filepath.Join(dir, "rules"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".json") {
			files = append(files, p)
		}
		return err
	})
	if err != nil || len(files) == 0 {
		t.Fatalf("no rule files in %s/rules (%v)", dir, err)
	}
	for _, f := range files {
		if _, err := LoadRules([]string{f}, false); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

// A fallback consumer (rabbitbus NewConsumer's queue list) keeps only the queues
// no other inbound endpoint of its transport registers, and disappears when
// a mux registers them all.
func TestDropCovered(t *testing.T) {
	det := func(rule, transport string, keys ...string) Detection {
		return Detection{Endpoint: model.Endpoint{Transport: transport, Direction: model.Inbound, Rule: rule, Keys: keys}}
	}
	d := &Detector{Rules: []Rule{{Name: "consumer", Fallback: true}, {Name: "mux"}}}
	got := d.dropCovered([]Detection{
		det("mux", "amqp", "a"),
		det("consumer", "amqp", "a", "b"), // b has no mux handler
		det("consumer", "amqp", "a"),      // fully covered
		det("consumer", "kafka", "a"),     // another transport
	})
	var facts []string
	for _, g := range got {
		facts = append(facts, g.Endpoint.Rule+" "+g.Endpoint.Transport+" "+strings.Join(g.Endpoint.Keys, ","))
	}
	if strings.Join(facts, "; ") != "mux amqp a; consumer amqp b; consumer kafka a" {
		t.Errorf("dropCovered = %v", facts)
	}
}

// gorm's Table() takes a table expression: the alias and quotes go, and a
// subquery names no table.
func TestTableNames(t *testing.T) {
	got := tableNames([]string{"daily_balance_snapshots AS s", "`orders` o", "receipts", "(?) as t", "*"})
	want := "daily_balance_snapshots,orders,receipts,*,*"
	if strings.Join(got, ",") != want {
		t.Errorf("tableNames = %v, want %s", got, want)
	}
}

func TestIrisPaths(t *testing.T) {
	got := irisPaths([]string{
		"POST /users/{user_id:string uuid() else 400}/carts/{id:string}",
		"GET /files/{name:path}",
		"GET /users/{user_id:string}/carts/",
		"GET /",
		"GET /items/{sku}",
		"GET */health",
	})
	want := "POST /users/{user_id}/carts/{id}|GET /files/{name...}|GET /users/{user_id}/carts|GET /|GET /items/{sku}|GET */health"
	if strings.Join(got, "|") != want {
		t.Errorf("irisPaths = %v, want %s", got, want)
	}
}
