// Package transport detects what a service exposes and consumes (HTTP, gRPC,
// Kafka, NATS, AMQP, SQL, Redis, ...) by matching call sites in reachable
// module code against declarative rules. Rules can be extended with JSON, which
// is how in-house wrappers (e.g. an internal events.Publisher) are described.
package transport

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/zalgonoise/codegraph/internal/model"
)

// FieldRef takes a key from a struct field of an argument, e.g. the Topic of
// a kafka.ReaderConfig or the Addr of an *http.Server. A dotted Field goes
// through nested fields and the elements of slices ("Queues.PriorityQueue").
// MapRef names a map argument by index.
type MapRef struct {
	Arg int `json:"arg"`
}

// ElementsRef names a slice argument by index, or a slice field of it.
type ElementsRef struct {
	Arg   int    `json:"arg"`
	Field string `json:"field,omitempty"`
}

type FieldRef struct {
	Arg   int    `json:"arg"`
	Field string `json:"field"`
}

// MethodRef takes a key from what a method of an argument's dynamic type
// returns, e.g. the Topic() of an in-house library's message.
type MethodRef struct {
	Arg    int    `json:"arg"`
	Method string `json:"method"`
}

// CtorField takes a key from the configuration a client was constructed with,
// when the call's own key argument is empty: a broker client's Publish(ctx, "", ...)
// sends to the PriorityQueue given to rabbitmq.New(ctx, &Config{...}).
type CtorField struct {
	Funcs []string `json:"funcs"` // constructor names, in the rule's Pkgs
	Arg   int      `json:"arg"`   // the constructor's config argument
	Field string   `json:"field"`
}

// Rule describes one API call that represents a transport endpoint.
//
// Argument indexes are positional and, for methods, index 0 is the receiver
// (for interface calls too), so `(*Channel).Publish(exchange, key, ...)` has
// the exchange at 1 and the routing key at 2.
type Rule struct {
	Name      string          `json:"name"`
	Transport string          `json:"transport"`
	Role      string          `json:"role"`
	Direction model.Direction `json:"direction"`
	Kind      string          `json:"kind,omitempty"` // sync | async; empty = model.DefaultKind

	// Callee matching. Pkgs entries match exactly or, with a "/..." suffix,
	// a package tree. Recv: "" = package-level function, "*" = anything,
	// otherwise a receiver/interface type name ("DB|Tx" for alternatives).
	Pkgs        []string `json:"pkgs,omitempty"`
	Recv        string   `json:"recv,omitempty"`
	RecvPattern string   `json:"recvPattern,omitempty"` // regexp the receiver type name must match
	Funcs       []string `json:"funcs,omitempty"`
	FuncPattern string   `json:"funcPattern,omitempty"` // regexp; group 1 becomes a key
	SigContains string   `json:"sigContains,omitempty"` // substring of the callee signature

	// Key extraction.
	KeyArgs        []int       `json:"keyArgs,omitempty"` // several args are joined by a space
	KeyFields      []FieldRef  `json:"keyFields,omitempty"`
	KeyMethods     []MethodRef `json:"keyMethods,omitempty"`
	KeyDefault     *CtorField  `json:"keyDefault,omitempty"` // when KeyArgs[0] is "": the receiver's constructor config
	StaticKey      string      `json:"staticKey,omitempty"`
	MethodFromFunc bool        `json:"methodFromFunc,omitempty"` // r.Get("/x") -> "GET /x"
	SkipUnknown    bool        `json:"skipUnknown,omitempty"`    // no endpoint when no key resolves (chained builders)
	Transform      string      `json:"transform,omitempty"`      // "sql-table" | "table-name" | "grpc-method" | "iris-path" | "first-word" | "grpc-gateway"

	MsgArgs       []int      `json:"msgArgs,omitempty"`
	HandlerArgs   []int      `json:"handlerArgs,omitempty"`
	HandlerFields []FieldRef `json:"handlerFields,omitempty"` // the handler is a field of a config argument

	// MapEntries registers one endpoint per key/value pair of a map argument:
	// the key is the endpoint's key and the value its handler, pair by pair
	// (a map of CLI commands to their functions).
	MapEntries *MapRef `json:"mapEntries,omitempty"`
	// Elements registers one endpoint per element of a slice or variadic
	// argument (or of a slice field of it): the KeyFields and HandlerFields
	// naming that argument are read on each element (cobra's
	// AddCommand(cmds ...*Command), each with its Use and RunE).
	Elements *ElementsRef `json:"elements,omitempty"`
	// RequireHandler drops an inbound endpoint whose handler does not
	// resolve to a function (a CLI command that only groups subcommands).
	RequireHandler bool `json:"requireHandler,omitempty"`

	// Fallback marks a rule describing a consumer's whole subscription (a
	// queue list): its keys that another inbound endpoint of the same
	// transport registers more precisely (a mux's per-queue handlers) are
	// dropped, and so is the endpoint when none remain.
	Fallback bool `json:"fallback,omitempty"`

	// Journey roots of inbound endpoints. HandlerMethods limits which methods
	// of an interface-typed handler run per message (all exported methods when
	// empty, e.g. every method of a gRPC server). CallerIsHandler makes the
	// function holding the call site the root, for APIs that return a channel
	// or subscription the caller then loops over (amqp Consume, SubscribeSync).
	HandlerMethods  []string `json:"handlerMethods,omitempty"`
	CallerIsHandler bool     `json:"callerIsHandler,omitempty"`

	// Router prefixes. A rule with Scope produces no endpoint: it declares a
	// call that places routes under the path in KeyArgs[0] ("" without one):
	//   returns: the call returns the scoped router (gin/echo Group, gorilla PathPrefix)
	//   keeps:   the call returns a router with its receiver's prefix (gorilla Subrouter, chi With)
	//   passes:  the scoped router is passed to the func in ScopeArg (chi Route, Group)
	//   mounts:  the router in ScopeArg is mounted under the path (chi Mount)
	// Prefixed route rules compose the prefix of their receiver (arg 0).
	Scope    string `json:"scope,omitempty"`
	ScopeArg int    `json:"scopeArg,omitempty"`
	Prefixed bool   `json:"prefixed,omitempty"`

	re, recvRe *regexp.Regexp
}

// Callee identifies a called function for rule matching.
type Callee struct {
	Pkg, Recv, Name, Sig string
}

// LoadRules reads extra rules from JSON files (each an array of Rule) and
// appends the built-in defaults. User rules come first, so they win.
func LoadRules(files []string, withDefaults bool) ([]Rule, error) {
	var rules []Rule
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("read rules: %w", err)
		}
		var extra []Rule
		if err := json.Unmarshal(data, &extra); err != nil {
			return nil, fmt.Errorf("parse rules %s: %w", f, err)
		}
		rules = append(rules, extra...)
	}
	if withDefaults {
		rules = append(rules, Defaults()...)
	}
	for i := range rules {
		if err := rules[i].compile(); err != nil {
			return nil, err
		}
	}
	return rules, nil
}

func (r *Rule) compile() error {
	if r.Name == "" {
		r.Name = r.Transport + "-" + r.Role
	}
	if r.Transport == "" {
		return fmt.Errorf("rule %q: transport is required", r.Name)
	}
	if r.Direction != model.Inbound && r.Direction != model.Outbound {
		return fmt.Errorf("rule %q: direction must be %q or %q", r.Name, model.Inbound, model.Outbound)
	}
	if r.Kind != "" && r.Kind != model.Sync && r.Kind != model.Async {
		return fmt.Errorf("rule %q: kind must be %q or %q", r.Name, model.Sync, model.Async)
	}
	switch r.Scope {
	case "", "returns", "keeps", "passes", "mounts":
	default:
		return fmt.Errorf("rule %q: scope must be returns, keeps, passes or mounts", r.Name)
	}
	for _, m := range r.KeyMethods {
		if m.Method == "" {
			return fmt.Errorf("rule %q: keyMethods entries need a method", r.Name)
		}
	}
	switch r.Transform {
	case "", "sql-table", "table-name", "grpc-method", "iris-path", "first-word", "grpc-gateway":
	default:
		return fmt.Errorf("rule %q: unknown transform %q (want sql-table, table-name, grpc-method or iris-path)", r.Name, r.Transform)
	}
	var err error
	if r.FuncPattern != "" {
		if r.re, err = regexp.Compile(r.FuncPattern); err != nil {
			return fmt.Errorf("rule %q: %w", r.Name, err)
		}
	}
	if r.RecvPattern != "" {
		if r.recvRe, err = regexp.Compile(r.RecvPattern); err != nil {
			return fmt.Errorf("rule %q: %w", r.Name, err)
		}
	}
	return nil
}

func (r *Rule) match(c Callee) (capture string, ok bool) {
	if len(r.Pkgs) > 0 && !pkgMatch(r.Pkgs, c.Pkg) {
		return "", false
	}
	if !recvMatch(r.Recv, c.Recv) || (r.recvRe != nil && !r.recvRe.MatchString(c.Recv)) {
		return "", false
	}
	if len(r.Funcs) > 0 && !contains(r.Funcs, c.Name) {
		return "", false
	}
	if r.SigContains != "" && !strings.Contains(c.Sig, r.SigContains) {
		return "", false
	}
	if r.re != nil {
		m := r.re.FindStringSubmatch(c.Name)
		if m == nil {
			return "", false
		}
		if len(m) > 1 {
			capture = m[1]
		}
	}
	return capture, true
}

func pkgMatch(patterns []string, pkg string) bool {
	for _, p := range patterns {
		if base, ok := strings.CutSuffix(p, "/..."); ok {
			if pkg == base || strings.HasPrefix(pkg, base+"/") {
				return true
			}
		} else if p == pkg {
			return true
		}
	}
	return false
}

func recvMatch(want, got string) bool {
	if want == "*" {
		return true
	}
	for _, w := range strings.Split(want, "|") {
		if w == got {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Defaults covers the standard library and common Go infrastructure clients.
func Defaults() []Rule {
	const in, out = model.Inbound, model.Outbound
	nethttp := []string{"net/http"}
	upperMethods := []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}
	chiFuncs := []string{"Get", "Post", "Put", "Patch", "Delete", "Head", "Options", "Handle", "HandleFunc"}
	grpc := []string{"google.golang.org/grpc"}
	chi := []string{"github.com/go-chi/chi/v5", "github.com/go-chi/chi"}
	gin := []string{"github.com/gin-gonic/gin"}
	echo := []string{"github.com/labstack/echo/v4"}
	gorilla := []string{"github.com/gorilla/mux"}
	iris := []string{"github.com/kataras/iris/v12"}
	cobra := []string{"github.com/spf13/cobra"}
	gateway := []string{"github.com/grpc-ecosystem/grpc-gateway/v2/runtime", "github.com/grpc-ecosystem/grpc-gateway/runtime"}
	irisRouter := []string{"github.com/kataras/iris/v12/core/router"}
	segmentio := []string{"github.com/segmentio/kafka-go"}
	sarama := []string{"github.com/IBM/sarama", "github.com/Shopify/sarama"}
	confluent := []string{"github.com/confluentinc/confluent-kafka-go/v2/kafka", "github.com/confluentinc/confluent-kafka-go/kafka"}
	nats := []string{"github.com/nats-io/nats.go"}
	amqp := []string{"github.com/rabbitmq/amqp091-go", "github.com/streadway/amqp"}
	sqlPkg := []string{"database/sql"}
	pgx := []string{"github.com/jackc/pgx/v5/...", "github.com/jackc/pgx/v4/..."}
	redis := []string{"github.com/redis/go-redis/v9", "github.com/go-redis/redis/v8"}
	gorm := []string{"gorm.io/gorm"}

	return []Rule{
		// ---- HTTP servers ---------------------------------------------------
		{Name: "http-handle", Transport: "http", Role: "server", Direction: in, Pkgs: nethttp, Recv: "",
			Funcs: []string{"Handle", "HandleFunc"}, KeyArgs: []int{0}, HandlerArgs: []int{1}},
		{Name: "http-mux-handle", Transport: "http", Role: "server", Direction: in, Pkgs: nethttp, Recv: "ServeMux",
			Funcs: []string{"Handle", "HandleFunc"}, KeyArgs: []int{1}, HandlerArgs: []int{2}},
		{Name: "http-listen", Transport: "http", Role: "listener", Direction: in, Pkgs: nethttp, Recv: "",
			Funcs: []string{"ListenAndServe", "ListenAndServeTLS"}, KeyArgs: []int{0}},
		{Name: "http-server-listen", Transport: "http", Role: "listener", Direction: in, Pkgs: nethttp, Recv: "Server",
			Funcs: []string{"ListenAndServe", "ListenAndServeTLS", "Serve", "ServeTLS"}, KeyFields: []FieldRef{{Arg: 0, Field: "Addr"}}},
		{Name: "chi-route", Transport: "http", Role: "server", Direction: in, Pkgs: chi, Recv: "*",
			Funcs: chiFuncs, KeyArgs: []int{1}, HandlerArgs: []int{2}, MethodFromFunc: true, Prefixed: true},
		{Name: "gin-route", Transport: "http", Role: "server", Direction: in, Pkgs: gin, Recv: "*",
			Funcs: append([]string{"Any"}, upperMethods...), KeyArgs: []int{1}, HandlerArgs: []int{2}, MethodFromFunc: true, Prefixed: true},
		{Name: "echo-route", Transport: "http", Role: "server", Direction: in, Pkgs: echo, Recv: "*",
			Funcs: upperMethods, KeyArgs: []int{1}, HandlerArgs: []int{2}, MethodFromFunc: true, Prefixed: true},
		{Name: "gorilla-route", Transport: "http", Role: "server", Direction: in, Pkgs: gorilla, Recv: "Router",
			Funcs: []string{"Handle", "HandleFunc"}, KeyArgs: []int{1}, HandlerArgs: []int{2}, Prefixed: true},
		// iris: an Application's routing methods are promoted from its
		// router.APIBuilder; a Party is the router.Party interface. The handler
		// is the last of the variadic handlers, after any middleware.
		{Name: "iris-route", Transport: "http", Role: "server", Direction: in, Pkgs: irisRouter, Recv: "APIBuilder|Party",
			Funcs: []string{"Get", "Post", "Put", "Patch", "Delete", "Head", "Options", "Any"}, KeyArgs: []int{1}, HandlerArgs: []int{2}, MethodFromFunc: true,
			Prefixed: true, Transform: "iris-path"},
		{Name: "iris-listen", Transport: "http", Role: "listener", Direction: in, Pkgs: iris, Recv: "Application",
			Funcs: []string{"Listen"}, KeyArgs: []int{1}},

		// ---- router prefixes (no endpoints) ---------------------------------
		{Name: "chi-route-scope", Transport: "http", Direction: in, Pkgs: chi, Recv: "*",
			Funcs: []string{"Route"}, KeyArgs: []int{1}, Scope: "passes", ScopeArg: 2},
		{Name: "chi-group-scope", Transport: "http", Direction: in, Pkgs: chi, Recv: "*",
			Funcs: []string{"Group"}, Scope: "passes", ScopeArg: 1},
		{Name: "chi-mount-scope", Transport: "http", Direction: in, Pkgs: chi, Recv: "*",
			Funcs: []string{"Mount"}, KeyArgs: []int{1}, Scope: "mounts", ScopeArg: 2},
		{Name: "chi-with-scope", Transport: "http", Direction: in, Pkgs: chi, Recv: "*",
			Funcs: []string{"With"}, Scope: "keeps"},
		{Name: "gin-group-scope", Transport: "http", Direction: in, Pkgs: gin, Recv: "*",
			Funcs: []string{"Group"}, KeyArgs: []int{1}, Scope: "returns"},
		{Name: "echo-group-scope", Transport: "http", Direction: in, Pkgs: echo, Recv: "*",
			Funcs: []string{"Group"}, KeyArgs: []int{1}, Scope: "returns"},
		{Name: "gorilla-prefix-scope", Transport: "http", Direction: in, Pkgs: gorilla, Recv: "Router|Route",
			Funcs: []string{"PathPrefix"}, KeyArgs: []int{1}, Scope: "returns"},
		{Name: "gorilla-subrouter-scope", Transport: "http", Direction: in, Pkgs: gorilla, Recv: "Route",
			Funcs: []string{"Subrouter"}, Scope: "keeps"},
		{Name: "iris-party-scope", Transport: "http", Direction: in, Pkgs: irisRouter, Recv: "APIBuilder|Party",
			Funcs: []string{"Party"}, KeyArgs: []int{1}, Scope: "returns"},

		// ---- HTTP clients ---------------------------------------------------
		{Name: "http-get", Transport: "http", Role: "client", Direction: out, Pkgs: nethttp, Recv: "",
			Funcs: []string{"Get", "Head", "Post", "PostForm"}, KeyArgs: []int{0}, MethodFromFunc: true},
		{Name: "http-client-get", Transport: "http", Role: "client", Direction: out, Pkgs: nethttp, Recv: "Client",
			Funcs: []string{"Get", "Head", "Post", "PostForm"}, KeyArgs: []int{1}, MethodFromFunc: true},
		{Name: "http-new-request", Transport: "http", Role: "client", Direction: out, Pkgs: nethttp, Recv: "",
			Funcs: []string{"NewRequest"}, KeyArgs: []int{0, 1}},
		{Name: "http-new-request-ctx", Transport: "http", Role: "client", Direction: out, Pkgs: nethttp, Recv: "",
			Funcs: []string{"NewRequestWithContext"}, KeyArgs: []int{1, 2}},

		// ---- gRPC (generated code: RegisterXServer / NewXClient) -------------
		{Name: "grpc-register", Transport: "grpc", Role: "server", Direction: in, Recv: "",
			FuncPattern: `^Register(\w+)Server$`, SigContains: "google.golang.org/grpc.", HandlerArgs: []int{1}},
		{Name: "grpc-client", Transport: "grpc", Role: "client", Direction: out, Recv: "",
			FuncPattern: `^New(\w+)Client$`, SigContains: "google.golang.org/grpc."},
		// RPCs on a generated client (XServiceClient.Method): the call sites
		// journeys reach, keyed "XService/Method".
		{Name: "grpc-call", Transport: "grpc", Role: "client", Direction: out, Recv: "*", RecvPattern: `^[A-Z]\w*Client$`,
			SigContains: "google.golang.org/grpc.CallOption", Transform: "grpc-method", MsgArgs: []int{2}},
		{Name: "grpc-dial", Transport: "grpc", Role: "dial", Direction: out, Pkgs: grpc, Recv: "",
			Funcs: []string{"NewClient", "Dial"}, KeyArgs: []int{0}},
		{Name: "grpc-dial-ctx", Transport: "grpc", Role: "dial", Direction: out, Pkgs: grpc, Recv: "",
			Funcs: []string{"DialContext"}, KeyArgs: []int{1}},
		{Name: "grpc-serve", Transport: "grpc", Role: "listener", Direction: in, Pkgs: grpc, Recv: "Server",
			Funcs: []string{"Serve"}},

		// ---- Kafka ----------------------------------------------------------
		{Name: "kafka-go-reader", Transport: "kafka", Role: "consumer", Direction: in, Pkgs: segmentio, Recv: "",
			Funcs: []string{"NewReader"}, KeyFields: []FieldRef{{Arg: 0, Field: "Topic"}, {Arg: 0, Field: "GroupTopics"}}},
		{Name: "kafka-go-writer", Transport: "kafka", Role: "producer", Direction: out, Pkgs: segmentio, Recv: "Writer",
			Funcs: []string{"WriteMessages"}, KeyFields: []FieldRef{{Arg: 0, Field: "Topic"}}},
		{Name: "sarama-send", Transport: "kafka", Role: "producer", Direction: out, Pkgs: sarama, Recv: "*",
			Funcs: []string{"SendMessage"}, KeyFields: []FieldRef{{Arg: 1, Field: "Topic"}}},
		{Name: "sarama-group-consume", Transport: "kafka", Role: "consumer", Direction: in, Pkgs: sarama, Recv: "*",
			Funcs: []string{"Consume"}, KeyArgs: []int{2}, HandlerArgs: []int{3}, HandlerMethods: []string{"ConsumeClaim"}},
		{Name: "sarama-partition-consume", Transport: "kafka", Role: "consumer", Direction: in, Pkgs: sarama, Recv: "*",
			Funcs: []string{"ConsumePartition"}, KeyArgs: []int{1}, CallerIsHandler: true},
		{Name: "confluent-subscribe", Transport: "kafka", Role: "consumer", Direction: in, Pkgs: confluent, Recv: "Consumer",
			Funcs: []string{"Subscribe", "SubscribeTopics"}, KeyArgs: []int{1}, CallerIsHandler: true},
		{Name: "confluent-produce", Transport: "kafka", Role: "producer", Direction: out, Pkgs: confluent, Recv: "Producer",
			Funcs: []string{"Produce"}},

		// ---- NATS -----------------------------------------------------------
		{Name: "nats-publish", Transport: "nats", Role: "producer", Direction: out, Pkgs: nats, Recv: "Conn",
			Funcs: []string{"Publish"}, KeyArgs: []int{1}},
		{Name: "nats-request", Transport: "nats", Role: "producer", Direction: out, Kind: model.Sync, Pkgs: nats, Recv: "Conn",
			Funcs: []string{"Request"}, KeyArgs: []int{1}},
		{Name: "nats-request-ctx", Transport: "nats", Role: "producer", Direction: out, Kind: model.Sync, Pkgs: nats, Recv: "Conn",
			Funcs: []string{"RequestWithContext"}, KeyArgs: []int{2}},
		{Name: "nats-publish-msg", Transport: "nats", Role: "producer", Direction: out, Pkgs: nats, Recv: "Conn",
			Funcs: []string{"PublishMsg"}, KeyFields: []FieldRef{{Arg: 1, Field: "Subject"}}},
		{Name: "nats-subscribe", Transport: "nats", Role: "consumer", Direction: in, Pkgs: nats, Recv: "Conn",
			Funcs: []string{"Subscribe"}, KeyArgs: []int{1}, HandlerArgs: []int{2}},
		{Name: "nats-queue-subscribe", Transport: "nats", Role: "consumer", Direction: in, Pkgs: nats, Recv: "Conn",
			Funcs: []string{"QueueSubscribe"}, KeyArgs: []int{1}, HandlerArgs: []int{3}},
		{Name: "nats-subscribe-sync", Transport: "nats", Role: "consumer", Direction: in, Pkgs: nats, Recv: "Conn",
			Funcs: []string{"ChanSubscribe", "ChanQueueSubscribe", "SubscribeSync", "QueueSubscribeSync"}, KeyArgs: []int{1}, CallerIsHandler: true},

		// ---- AMQP (RabbitMQ) --------------------------------------------------
		{Name: "amqp-publish", Transport: "amqp", Role: "producer", Direction: out, Pkgs: amqp, Recv: "Channel",
			Funcs: []string{"Publish"}, KeyArgs: []int{1, 2}},
		{Name: "amqp-publish-ctx", Transport: "amqp", Role: "producer", Direction: out, Pkgs: amqp, Recv: "Channel",
			Funcs: []string{"PublishWithContext"}, KeyArgs: []int{2, 3}},
		{Name: "amqp-consume", Transport: "amqp", Role: "consumer", Direction: in, Pkgs: amqp, Recv: "Channel",
			Funcs: []string{"Consume"}, KeyArgs: []int{1}, CallerIsHandler: true},
		{Name: "amqp-consume-ctx", Transport: "amqp", Role: "consumer", Direction: in, Pkgs: amqp, Recv: "Channel",
			Funcs: []string{"ConsumeWithContext"}, KeyArgs: []int{2}, CallerIsHandler: true},

		// ---- SQL ------------------------------------------------------------
		{Name: "sql-open", Transport: "sql", Role: "connect", Direction: out, Pkgs: sqlPkg, Recv: "",
			Funcs: []string{"Open"}, KeyArgs: []int{0}},
		{Name: "sql-query", Transport: "sql", Role: "query", Direction: out, Pkgs: sqlPkg, Recv: "DB|Tx",
			Funcs: []string{"Query", "QueryRow", "Exec", "Prepare"}, KeyArgs: []int{1}, Transform: "sql-table"},
		{Name: "sql-query-ctx", Transport: "sql", Role: "query", Direction: out, Pkgs: sqlPkg, Recv: "DB|Tx|Conn",
			Funcs: []string{"QueryContext", "QueryRowContext", "ExecContext", "PrepareContext"}, KeyArgs: []int{2}, Transform: "sql-table"},
		{Name: "pgx-query", Transport: "sql", Role: "query", Direction: out, Pkgs: pgx, Recv: "*",
			Funcs: []string{"Query", "QueryRow", "Exec"}, KeyArgs: []int{2}, Transform: "sql-table"},
		{Name: "pgx-connect", Transport: "sql", Role: "connect", Direction: out, Pkgs: pgx, Recv: "",
			Funcs: []string{"Connect", "ConnectConfig", "New", "NewWithConfig"}, StaticKey: "postgres"},

		// ---- gorm: tables from Table(name), raw SQL, or the model's TableName() ----
		// Chained builders (Where, Find on a Table query) whose table cannot be
		// told from the call itself are skipped rather than reported as "*".
		{Name: "gorm-open", Transport: "sql", Role: "connect", Direction: out, Pkgs: gorm, Recv: "",
			Funcs: []string{"Open"}, KeyMethods: []MethodRef{{Arg: 0, Method: "Name"}}},
		{Name: "gorm-table", Transport: "sql", Role: "query", Direction: out, Pkgs: gorm, Recv: "DB",
			Funcs: []string{"Table"}, KeyArgs: []int{1}, Transform: "table-name", SkipUnknown: true},
		{Name: "gorm-raw", Transport: "sql", Role: "query", Direction: out, Pkgs: gorm, Recv: "DB",
			Funcs: []string{"Raw", "Exec"}, KeyArgs: []int{1}, Transform: "sql-table", SkipUnknown: true},
		{Name: "gorm-model", Transport: "sql", Role: "query", Direction: out, Pkgs: gorm, Recv: "DB",
			Funcs: []string{"Model", "Create", "CreateInBatches", "Save", "First", "Take", "Last", "Find", "FindInBatches",
				"FirstOrInit", "FirstOrCreate", "Delete", "Scan"},
			KeyMethods: []MethodRef{{Arg: 1, Method: "TableName"}}, SkipUnknown: true},

		// ---- Redis ----------------------------------------------------------
		{Name: "redis-publish", Transport: "redis", Role: "producer", Direction: out, Pkgs: redis, Recv: "*",
			Funcs: []string{"Publish", "SPublish"}, KeyArgs: []int{2}},
		{Name: "redis-subscribe", Transport: "redis", Role: "consumer", Direction: in, Pkgs: redis, Recv: "*",
			Funcs: []string{"Subscribe", "PSubscribe", "SSubscribe"}, KeyArgs: []int{2}, CallerIsHandler: true},
		{Name: "redis-connect", Transport: "redis", Role: "connect", Direction: out, Pkgs: redis, Recv: "",
			Funcs: []string{"NewClient", "NewClusterClient", "NewFailoverClient", "NewUniversalClient"}, StaticKey: "redis"},
		// Key operations, keyed by key pattern ("customers:profile:*"): services of
		// different modules using the same pattern share a store.
		{Name: "redis-key", Transport: "redis", Role: "query", Direction: out, Pkgs: redis, Recv: "*",
			Funcs: []string{"Get", "Set", "SetNX", "SetEX", "GetSet", "GetDel", "Del", "Exists", "Expire", "TTL",
				"Incr", "IncrBy", "Decr", "DecrBy", "HGet", "HSet", "HSetNX", "HGetAll", "HDel", "HIncrBy", "HExists",
				"LPush", "RPush", "LPop", "RPop", "LRange", "LLen", "SAdd", "SRem", "SMembers", "SIsMember",
				"ZAdd", "ZRem", "ZRange", "ZRangeByScore", "ZScore"},
			KeyArgs: []int{2}, SkipUnknown: true},

		// ---- grpc-gateway: HTTP routes forwarding to gRPC methods --------------
		// The generated code registers each route with Handle and a pattern built
		// from op codes (transform grpc-gateway decodes it into a path template);
		// HandlePath takes the path itself. A route's journey reaches the gRPC
		// call it forwards (request_X_M_0 calls the client), which continues into
		// the method's journey, in the same service too (see link).
		{Name: "grpc-gateway-handle", Transport: "http", Role: "server", Direction: in, Pkgs: gateway, Recv: "ServeMux",
			Funcs: []string{"Handle"}, KeyArgs: []int{1, 2}, HandlerArgs: []int{3}, Transform: "grpc-gateway"},
		{Name: "grpc-gateway-handle-path", Transport: "http", Role: "server", Direction: in, Pkgs: gateway, Recv: "ServeMux",
			Funcs: []string{"HandlePath"}, KeyArgs: []int{1, 2}, HandlerArgs: []int{3}},

		// ---- CLI commands ---------------------------------------------------
		// A command is an inbound endpoint (transport cli, role command) whose
		// journey starts at the function it runs. cobra: the root command run by
		// Execute and the subcommands added to it, named by the first word of
		// Use; a command without Run or RunE only groups others and is left out.
		// zalgonoise/x/cli v2 maps command names to executors; v1 registers one
		// executor per flag.
		{Name: "cobra-add-command", Transport: "cli", Role: "command", Direction: in, Pkgs: cobra, Recv: "Command",
			Funcs: []string{"AddCommand"}, Elements: &ElementsRef{Arg: 1}, KeyFields: []FieldRef{{Arg: 1, Field: "Use"}},
			HandlerFields: []FieldRef{{Arg: 1, Field: "RunE"}, {Arg: 1, Field: "Run"}}, Transform: "first-word", RequireHandler: true},
		{Name: "cobra-execute", Transport: "cli", Role: "command", Direction: in, Pkgs: cobra, Recv: "Command",
			Funcs: []string{"Execute", "ExecuteC", "ExecuteContext", "ExecuteContextC"}, KeyFields: []FieldRef{{Arg: 0, Field: "Use"}},
			HandlerFields: []FieldRef{{Arg: 0, Field: "RunE"}, {Arg: 0, Field: "Run"}}, Transform: "first-word", RequireHandler: true},
		{Name: "xcli-executors", Transport: "cli", Role: "command", Direction: in, Pkgs: []string{"github.com/zalgonoise/x/cli/v2"},
			Recv: "", Funcs: []string{"WithExecutors"}, MapEntries: &MapRef{Arg: 0}},
		{Name: "xcli-flag", Transport: "cli", Role: "command", Direction: in, Pkgs: []string{"github.com/zalgonoise/x/cli"},
			Recv: "", Funcs: []string{"WithFlag"}, KeyArgs: []int{0}, HandlerArgs: []int{3}},
	}
}
