package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/zalgonoise/codegraph/internal/bundle"
	"github.com/zalgonoise/codegraph/internal/config"
	"github.com/zalgonoise/codegraph/internal/deploy"
	"github.com/zalgonoise/codegraph/internal/manifest"
	"github.com/zalgonoise/codegraph/internal/model"
	"github.com/zalgonoise/codegraph/internal/query"
	"github.com/zalgonoise/codegraph/internal/render"
	"github.com/zalgonoise/codegraph/internal/symbols"
)

// TestSamples runs analyze and link on the bundled samples and compares the
// facts listed in CLAUDE.md (endpoint keys, handlers, runtime implementations,
// component calls, links and journeys). Positions are deliberately ignored.
func TestSamples(t *testing.T) {
	if testing.Short() {
		t.Skip("builds SSA for the samples and the standard library")
	}
	out := t.TempDir()
	for _, args := range [][]string{
		{"-dir", "../../testdata/orders", "-rules", "../../testdata/orders/codegraph-rules.json", "-out", out, "-format", "json"},
		{"-dir", "../../testdata/inventory", "-out", out, "-format", "json"},
		{"-dir", "../../testdata/ledger", "-rules", "../../testdata/rules/inhouse.json", "-out", out, "-format", "json"},
		{"-dir", "../../testdata/billing", "-rules", "../../testdata/rules/inhouse.json", "-include-modules", "example.com/inhouse/*-sdk-go", "-out", out, "-format", "json"},
		{"-dir", "../../testdata/gateway", "-out", out, "-format", "json"},
		{"-dir", "../../testdata/tool", "-entry", "./cmd/...", "-out", out, "-format", "json"},
		{"-dir", "../../testdata/mailer", "-library", "-out", out, "-format", "json,md"},
		{"-dir", "../../testdata/pipeline", "-out", out, "-format", "json"},
	} {
		if err := runAnalyze(args); err != nil {
			t.Fatalf("analyze %v: %v", args, err)
		}
	}
	orders := readManifest(t, filepath.Join(out, "orders", "manifest.json"))
	inventory := readManifest(t, filepath.Join(out, "inventory", "manifest.json"))
	ledger := readManifest(t, filepath.Join(out, "ledger", "manifest.json"))
	billing := readManifest(t, filepath.Join(out, "billing", "manifest.json"))
	gateway := readManifest(t, filepath.Join(out, "gateway", "manifest.json"))
	tool := readManifest(t, filepath.Join(out, "tool", "manifest.json"))
	toolAdmin := readManifest(t, filepath.Join(out, "tool-admin", "manifest.json"))
	mailer := readManifest(t, filepath.Join(out, "mailer", "manifest.json"))
	pipeline := readManifest(t, filepath.Join(out, "pipeline", "manifest.json"))
	if _, err := writeDeclared("../../testdata/declared", &config.Analyze{Out: out, Formats: map[string]bool{"json": true, "md": true}}); err != nil {
		t.Fatalf("declared: %v", err)
	}
	notifier := readManifest(t, filepath.Join(out, "notifier", "manifest.json"))

	sysDir := filepath.Join(out, "_system")
	if err := runLink([]string{"-out", sysDir, "-format", "json,html,agents", filepath.Join(out, "*", "manifest.json")}); err != nil {
		t.Fatalf("link: %v", err)
	}
	var sys model.System
	data, err := os.ReadFile(filepath.Join(sysDir, "system.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &sys); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{filepath.Join(sysDir, "system.html"), filepath.Join(out, "AGENTS.md"), filepath.Join(sysDir, "context.md")} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("missing output: %v", err)
		}
	}

	t.Run("orders/service", func(t *testing.T) {
		if orders.Service != "orders" || !slices.Contains(orders.Aliases, "orders-svc") || orders.Tags["team"] != "checkout" {
			t.Errorf("identity: service %q aliases %v tags %v", orders.Service, orders.Aliases, orders.Tags)
		}
	})
	t.Run("orders/endpoints", func(t *testing.T) {
		expect(t, endpointFacts(orders), []string{
			"inbound http listener :8080 in=cmd/orders",
			"inbound http server GET /orders/{id} handler=(*httpapi.Handler).get in=internal/httpapi",
			"inbound http server POST /orders handler=(*httpapi.Handler).create in=internal/httpapi",
			"outbound http client GET http://inventory:8081/items/* in=internal/clients/inventory",
			"outbound kafka producer orders.created msg=orders.OrderCreated in=internal/domain/orders",
			"outbound sql connect postgres in=cmd/orders",
			"outbound sql query orders in=internal/storage x2",
		})
	})
	t.Run("orders/implementations", func(t *testing.T) {
		expect(t, implementationFacts(orders), []string{
			"events.Publisher: *events.LogPublisher",
			"orders.Inventory: *inventory.Client", // and not orders.Mock
			"orders.Repository: *storage.OrderRepo",
		})
	})
	t.Run("orders/calls", func(t *testing.T) {
		expect(t, callFacts(orders.Module, orders.Calls), []string{
			"cmd/orders -> internal/clients/inventory",
			"cmd/orders -> internal/config",
			"cmd/orders -> internal/domain/orders",
			"cmd/orders -> internal/events",
			"cmd/orders -> internal/httpapi",
			"cmd/orders -> internal/storage",
			"internal/domain/orders -.-> internal/clients/inventory",
			"internal/domain/orders -.-> internal/events",
			"internal/domain/orders -.-> internal/storage",
			"internal/httpapi -> internal/domain/orders",
		})
	})
	t.Run("orders/journeys", func(t *testing.T) {
		expect(t, journeyFacts(orders), []string{
			"http GET /orders/{id} roots=(*httpapi.Handler).get" +
				" components=internal/domain/orders,internal/httpapi,internal/storage" +
				" outbound=sql query orders (sync)",
			"http POST /orders roots=(*httpapi.Handler).create" +
				" components=internal/clients/inventory,internal/domain/orders,internal/events,internal/httpapi,internal/storage" +
				" outbound=http client GET http://inventory:8081/items/* (sync),kafka producer orders.created (async),sql query orders (sync)",
		})
		// Outbound endpoints are in program order: reserve, save, publish.
		eps := map[string]model.Endpoint{}
		for _, ep := range orders.Endpoints {
			eps[ep.ID] = ep
		}
		for _, j := range orders.Journeys {
			if j.Key != "POST /orders" {
				continue
			}
			var order []string
			for _, o := range j.Outbound {
				order = append(order, eps[o.Endpoint].Transport)
			}
			if !slices.Equal(order, []string{"http", "sql", "kafka"}) {
				t.Errorf("POST /orders outbound order = %v, want [http sql kafka]", order)
			}
		}
	})

	t.Run("inventory/endpoints", func(t *testing.T) {
		expect(t, endpointFacts(inventory), []string{
			"inbound http listener :8081 in=inventory",
			"inbound http listener * in=inventory", // its envDefault (":9090") names the local machine: a placeholder
			"inbound http server GET /health handler=main.logged(main.health) in=inventory",
			"inbound http server GET /items/{sku} handler=main.itemHandler(…) in=inventory",
			"inbound http server GET /stock handler=main.logged((*main.memStore).stock) in=inventory",
			"inbound http server GET /v1/version handler=main.version in=inventory", // a config field's envDefault: nothing sets it in code
			"inbound kafka consumer orders.created msg=orders.OrderCreated in=inventory",
		})
	})
	t.Run("inventory/settings", func(t *testing.T) {
		// env tags of the parsed config (envPrefix composed), and os.Getenv
		expect(t, inventory.Settings, []string{"INVENTORY_ADDR", "INVENTORY_BASE_PATH", "INVENTORY_METRICS_ADDR", "INVENTORY_REGION", "KAFKA_BROKERS"})
	})
	t.Run("inventory/bootstrap", func(t *testing.T) {
		var steps []string
		for _, s := range inventory.Bootstrap {
			f := s.Kind + " " + s.Call + " caller=" + s.Caller
			if s.Async {
				f += " go"
			}
			steps = append(steps, f)
		}
		for _, want := range []string{
			"signal signal.NotifyContext caller=main.main",
			"call main.consumeOrders caller=main.main go",
			"shutdown (*http.Server).Shutdown caller=main.main$1",
			"start (*http.Server).ListenAndServe caller=main.main",
		} {
			if !slices.Contains(steps, want) {
				t.Errorf("bootstrap misses %q; got:\n%s", want, strings.Join(steps, "\n"))
			}
		}
	})
	t.Run("inventory/journeys", func(t *testing.T) {
		expect(t, journeyFacts(inventory), []string{
			"http GET /health roots=main.logged$1 components=inventory outbound=",
			"http GET /items/{sku} roots=main.itemHandler$1 components=inventory outbound=",
			"http GET /stock roots=main.logged$1 components=inventory outbound=",
			"http GET /v1/version roots=main.version components=inventory outbound=",
			"kafka orders.created roots=main.consumeOrders components=inventory outbound=",
		})
		// Both routes share the logged middleware; each reaches only its own handler.
		funcs := map[string][]string{}
		for _, j := range inventory.Journeys {
			funcs[j.Key] = j.Functions
		}
		expect(t, funcs["GET /health"], []string{"main.health", "main.logged$1"})
		expect(t, funcs["GET /stock"], []string{"(*main.memStore).stock", "main.logged$1"})
	})

	t.Run("ledger/endpoints", func(t *testing.T) {
		expect(t, endpointFacts(ledger), []string{
			"inbound grpc listener * in=ledger",
			"inbound grpc server LedgerService handler=*main.ledger in=ledger",
			"outbound grpc client NotifierService in=ledger",
			"outbound grpc client NotifierService/Notify msg=*pb.NotifyRequest in=ledger",
			"outbound grpc dial notifier:9090 in=ledger",
			"outbound kafka producer billing.invoice.requested msg=billing.DataInvoiceRequested in=ledger", // topic from Topic()
			"outbound redis query billing:invoice:* in=ledger",                                             // billing's cache, via kvcache
			// grpc-gateway: routes decoded from the generated patterns, forwarding to
			// the gRPC client; a route registered with HandlePath; the gateway's own
			// listener and its client of this service's server.
			"inbound http listener :8092 in=ledger",
			"inbound http server GET /healthz handler=main.serveGateway$1 in=ledger",
			"inbound http server GET /v1/accounts/{account}/balance handler=pb.RegisterLedgerServiceHandlerClient$2 in=internal/pb",
			"inbound http server POST /v1/accounts/{account}/credit handler=pb.RegisterLedgerServiceHandlerClient$1 in=internal/pb",
			"outbound grpc client LedgerService in=ledger",
			"outbound grpc client LedgerService/Balance msg=*pb.BalanceRequest in=internal/pb",
			"outbound grpc client LedgerService/Credit msg=*pb.CreditRequest in=internal/pb",
			"outbound grpc dial localhost:9091 in=ledger",
		})
	})
	t.Run("ledger/contract", func(t *testing.T) {
		for _, ep := range ledger.Endpoints {
			if ep.Transport == "grpc" && ep.Role == "server" && ep.Contract != "ledger.LedgerService" {
				t.Errorf("LedgerService server contract = %q, want ledger.LedgerService (from LedgerService_ServiceDesc)", ep.Contract)
			}
		}
	})
	t.Run("ledger/journeys", func(t *testing.T) {
		// One journey per implemented method; Debit is only the Unimplemented stub.
		expect(t, journeyFacts(ledger), []string{
			"grpc LedgerService/Balance roots=(*main.ledger).Balance components=ledger outbound=",
			"http GET /healthz roots=main.serveGateway$1 components=ledger outbound=",
			"http GET /v1/accounts/{account}/balance roots=pb.RegisterLedgerServiceHandlerClient$2 components=internal/pb outbound=grpc client LedgerService/Balance (sync)",
			"http POST /v1/accounts/{account}/credit roots=pb.RegisterLedgerServiceHandlerClient$1 components=internal/pb outbound=grpc client LedgerService/Credit (sync)",
			"grpc LedgerService/Credit roots=(*main.ledger).Credit components=ledger,internal/pb" +
				" outbound=grpc client NotifierService/Notify (sync),kafka producer billing.invoice.requested (async)," +
				"redis query billing:invoice:* (sync)",
		})
	})

	t.Run("billing/endpoints", func(t *testing.T) {
		expect(t, endpointFacts(billing), []string{
			// queue names read back from QueueConfigs built by a helper; the consumer's
			// own NewConsumer queue list adds nothing, since the mux registers both
			"inbound amqp consumer billing.reminders,billing.reminders.urgent handler=(*main.middleware).WrapRabbit((*main.handler).remind) in=billing",
			// NewConsumer with a handler of its own: Queues[].PriorityQueue, the Handler field
			"inbound amqp consumer billing.refunds handler=*main.refundHandler in=billing",
			// rabbitbus broker repository: Consume/Publish with "" use rabbitmq.New's PriorityQueue
			"inbound amqp consumer billing.audit handler=(*main.handler).audited in=billing",
			"outbound amqp producer billing.audit in=billing",
			// through the SDK (-include-modules example.com/inhouse/*-sdk-go)
			"outbound grpc client LedgerService in=example.com/inhouse/ledger-sdk-go",
			"outbound grpc client LedgerService/Balance msg=*pb.BalanceRequest in=example.com/inhouse/ledger-sdk-go",
			"outbound grpc dial ledger:9091 in=example.com/inhouse/ledger-sdk-go",
			"outbound http client POST http://gateway:8091/backoffice/payments/*/refund in=billing",
			"inbound kafka consumer billing.invoice.paid handler=(*main.middleware).Wrap((*main.handler).invoicePaid) in=billing",
			"inbound kafka consumer billing.invoice.requested handler=(*main.middleware).Wrap((*main.handler).invoiceRequested) in=billing",
			"outbound amqp producer billing.receipts in=billing", // (&billing.DataReceipt{}).Topic().String()
			// both go through the shared emit(ctx, d) helper: one endpoint per caller
			"outbound kafka producer billing.invoice.issued msg=*billing.DataInvoiceIssued in=billing",
			"outbound kafka producer billing.payment.recorded msg=billing.DataPaymentRecorded in=billing",
			"outbound redis query billing:invoice:* in=billing", // kvcache Set(ctx, fmt.Sprintf(InvoiceKey, id), ...)
			// gorm: TableName() of the model (Create, Model), Table(name) without its alias, the dialector's Name();
			// Find(&[]receipt) after Table() adds no "*" endpoint
			"outbound sql connect mysql in=billing",
			"outbound sql query invoices in=billing x3",
			"outbound sql query receipts in=billing",
		})
	})
	t.Run("tool/commands", func(t *testing.T) {
		// x/cli v2: a map of command names to functions, each an entry point.
		expect(t, endpointFacts(tool), []string{
			"inbound cli command migrate handler=main.migrate in=cmd/tool",
			"inbound cli command serve handler=main.serve in=cmd/tool",
			"inbound http listener :8095 in=cmd/tool",
			"inbound http server GET /status handler=main.serve$1 in=cmd/tool",
			"outbound sql query schema_migrations in=cmd/tool",
			"outbound sql connect postgres in=cmd/tool x2",
		})
		expect(t, journeyFacts(tool), []string{
			"cli migrate roots=main.migrate components=cmd/tool outbound=sql connect postgres (sync),sql query schema_migrations (sync)",
			"cli serve roots=main.serve components=cmd/tool outbound=sql connect postgres (sync)",
			"http GET /status roots=main.serve$1 components=cmd/tool outbound=",
		})
		// cobra: the subcommands added to a root that only groups them, one built
		// by a constructor (a closure), one a package-level command.
		expect(t, endpointFacts(toolAdmin), []string{
			"inbound cli command purge handler=main.runPurge in=cmd/admin",
			"inbound cli command users handler=main.newUsersCmd$1 in=cmd/admin",
			"outbound sql connect postgres in=cmd/admin x2",
			"outbound sql query sessions in=cmd/admin",
			"outbound sql query users in=cmd/admin",
		})
		// kelseyhightower/envconfig: tagged fields by their tag, untagged ones by
		// the prefix and their name (split_words, a nested struct's name, a type
		// decoding itself); ignored fields are not read.
		expect(t, tool.Settings, []string{"DSN", "MIGRATIONS_TABLE", "TOOL_HTTP_PORT", "TOOL_LOG_LEVEL", "TOOL_STARTED"})
		// The libraries' API each journey's code calls, and the service's.
		expect(t, usesFacts(tool), []string{
			"cli migrate: envconfig.Process (github.com/kelseyhightower/envconfig) at cmd/tool/main.go:33",
			"main main: cli.NewRunner (github.com/zalgonoise/x/cli/v2) at cmd/tool/main.go:39",
			"main main: cli.Run (github.com/zalgonoise/x/cli/v2) at cmd/tool/main.go:43",
			"main main: cli.WithExecutors (github.com/zalgonoise/x/cli/v2) at cmd/tool/main.go:39",
		})
		expect(t, librarySymbols(tool), []string{
			"github.com/kelseyhightower/envconfig: envconfig.Process",
			"github.com/zalgonoise/x/cli/v2: cli.NewRunner, cli.Run, cli.WithExecutors",
		})
		// The bootstrap trace continues into each command's function.
		var steps []string
		for _, s := range tool.Bootstrap {
			steps = append(steps, strings.TrimSpace(s.Command+" "+s.Kind+" "+s.Call))
		}
		expect(t, steps, []string{
			"construct cli.NewRunner", "start cli.Run",
			"migrate command main.migrate", "migrate call main.load", "migrate construct sql.Open",
			"serve command main.serve", "serve construct sql.Open", "serve construct http.NewServeMux",
			"serve register (*http.ServeMux).HandleFunc", "serve start http.ListenAndServe",
		})
	})

	t.Run("mailer/library", func(t *testing.T) {
		// A library: its exported API (not queued's Flush, not Version, declared in
		// a generated file; internal/tmpl's Render as its internal API), one journey per symbol; Sender.Send runs the exported Client
		// and the unexported queued type that NewQueued converts to a Sender.
		if !mailer.Library || len(mailer.Bootstrap) != 0 {
			t.Errorf("library %v, bootstrap %v", mailer.Library, mailer.Bootstrap)
		}
		expect(t, endpointFacts(mailer), []string{
			"inbound go api mailer.Client.Send handler=(*mailer.Client).Send in=mailer",
			"inbound go api mailer.New handler=mailer.New in=mailer",
			"inbound go api mailer.NewQueued handler=mailer.NewQueued in=mailer",
			"inbound go api mailer.Sender.Send handler=mailer.Sender.Send → (*mailer.Client).Send, (*mailer.queued).Send in=mailer",
			"inbound go internal mailer/internal/tmpl.Render handler=tmpl.Render in=internal/tmpl",
			"outbound http client POST http://relay:8025/send in=mailer",
			"outbound sql query outbox in=mailer",
		})
		expect(t, journeyFacts(mailer), []string{
			"go mailer.Client.Send roots=(*mailer.Client).Send components=mailer,internal/tmpl outbound=http client POST http://relay:8025/send (sync)",
			"go mailer.New roots=mailer.New components=mailer outbound=",
			"go mailer.NewQueued roots=mailer.NewQueued components=mailer outbound=",
			"go mailer.Sender.Send roots=(*mailer.Client).Send,(*mailer.queued).Send components=mailer,internal/tmpl outbound=http client POST http://relay:8025/send (sync),sql query outbox (sync)",
			"go mailer/internal/tmpl.Render roots=tmpl.Render components=internal/tmpl outbound=",
		})
		// The commands calling it: a method, and an interface method.
		expect(t, usesFacts(toolAdmin), []string{
			"cli purge: mailer.NewQueued (github.com/zalgonoise/codegraph/testdata/mailer) at cmd/admin/main.go:54",
			"cli purge: mailer.Sender.Send (github.com/zalgonoise/codegraph/testdata/mailer) at cmd/admin/main.go:54",
			"cli users: mailer.New (github.com/zalgonoise/codegraph/testdata/mailer) at cmd/admin/main.go:38",
			"cli users: (*mailer.Client).Send (github.com/zalgonoise/codegraph/testdata/mailer) at cmd/admin/main.go:38",
			"main main: (*cobra.Command).AddCommand (github.com/spf13/cobra) at cmd/admin/main.go:19",
			"main main: (*cobra.Command).Execute (github.com/spf13/cobra) at cmd/admin/main.go:20",
		})
		var calls []string
		ms := []*model.Manifest{toolAdmin, mailer}
		for _, c := range model.APICalls(ms) {
			calls = append(calls, ms[c.Caller.Manifest].Journeys[c.Caller.Journey].Key+" -> "+ms[c.API.Manifest].Journeys[c.API.Journey].Key)
		}
		expect(t, calls, []string{
			"purge -> mailer.NewQueued", "purge -> mailer.Sender.Send", "users -> mailer.New", "users -> mailer.Client.Send",
		})
		// Never linked: no link, no unmatched endpoint, but listed.
		for _, l := range sys.Links {
			if l.From == "mailer" || l.To == "mailer" {
				t.Errorf("a library linked: %+v", l)
			}
		}
		for _, d := range sys.Dangling {
			if d.Service == "mailer" {
				t.Errorf("a library's endpoint unmatched: %+v", d)
			}
		}
		listed := false
		for _, s := range sys.Services {
			listed = listed || s.Name == "mailer" && s.Library
		}
		if !listed {
			t.Error("the library is not listed among the system's services")
		}
		// The internal API: its own table in the report, out of the diagram unless asked for.
		if data, err := os.ReadFile(filepath.Join(out, "mailer", "report.md")); err != nil || !strings.Contains(string(data), "# Library `mailer`") ||
			!strings.Contains(string(data), "## Internal API (1)") || strings.Contains(string(data), "### `go mailer/internal/tmpl.Render`") {
			t.Errorf("report: %v\n%.300s", err, data)
		}
		if strings.Contains(render.Mermaid(render.Service(mailer, render.ServiceOptions{})), "mailer/internal/tmpl.Render") ||
			!strings.Contains(render.Mermaid(render.Service(mailer, render.ServiceOptions{Internal: true})), "mailer/internal/tmpl.Render") {
			t.Error("the service diagram should draw the internal API only when asked")
		}
		// The explorer marks it and pairs the calls with its symbols' journeys.
		if data, err := os.ReadFile(filepath.Join(sysDir, "system.html")); err != nil ||
			!strings.Contains(string(data), `"name":"mailer","module":"github.com/zalgonoise/codegraph/testdata/mailer","library":true`) ||
			!strings.Contains(string(data), `"api":[[`) || !strings.Contains(string(data), `"service+internal:mailer"`) {
			t.Errorf("explorer: %v", err)
		}
	})
	t.Run("billing/journeys", func(t *testing.T) {
		expect(t, journeyFacts(billing), []string{
			"amqp billing.reminders, billing.reminders.urgent roots=main.WrapRabbit$1 components=billing outbound=",
			"amqp billing.audit roots=(*main.handler).audited components=billing outbound=",
			"amqp billing.refunds roots=(*main.refundHandler).ProcessMessage components=billing outbound=http client POST http://gateway:8091/backoffice/payments/*/refund (sync),sql query invoices (sync)",
			"kafka billing.invoice.paid roots=main.Wrap$1 components=example.com/inhouse/ledger-sdk-go,example.com/inhouse/ledger-sdk-go/pb,billing" +
				" outbound=amqp producer billing.audit (async),amqp producer billing.receipts (async)," +
				"grpc client LedgerService/Balance (sync),kafka producer billing.payment.recorded (async)," +
				"sql query invoices (sync),sql query receipts (sync)",
			"kafka billing.invoice.requested roots=main.Wrap$1 components=billing" +
				" outbound=kafka producer billing.invoice.issued (async),redis query billing:invoice:* (sync),sql query invoices (sync)",
		})
		// Both topics share the Wrap middleware; each reaches only its own handler.
		funcs := map[string][]string{}
		for _, j := range billing.Journeys {
			funcs[j.Key] = j.Functions
		}
		expect(t, funcs["billing.invoice.paid"], []string{"(*ledgersdk.client).Balance", "(*main.handler).emit", "(*main.handler).invoicePaid",
			"(*main.store).markPaid", "(*main.store).receipts", "(*pb.ledgerServiceClient).Balance", "main.Wrap$1"})
		expect(t, funcs["billing.invoice.requested"], []string{"(*main.handler).emit", "(*main.handler).invoiceRequested", "(*main.store).save", "main.Wrap$1"})
		// The helper's endpoints say which caller they were resolved for.
		via := map[string]string{}
		for _, ep := range billing.Endpoints {
			if ep.Transport == "kafka" && ep.Role == "producer" && len(ep.Sites) == 1 {
				via[ep.Keys[0]] = ep.Sites[0].Via
			}
		}
		if via["billing.invoice.issued"] != "(*main.handler).invoiceRequested" || via["billing.payment.recorded"] != "(*main.handler).invoicePaid" {
			t.Errorf("emit variants via = %v", via)
		}
		// What their code calls of the in-house libraries and gorm (a call inside
		// the SDK, outside the repository, has an absolute position: left out).
		var uses []string
		for _, f := range usesFacts(billing) {
			if strings.HasPrefix(f, "kafka billing.invoice.requested:") {
				uses = append(uses, f)
			}
		}
		expect(t, uses, []string{
			"kafka billing.invoice.requested: (*gorm.DB).Create (gorm.io/gorm) at main.go:199",
			"kafka billing.invoice.requested: (*gorm.DB).WithContext (gorm.io/gorm) at main.go:199",
			"kafka billing.invoice.requested: kafkabus.Publisher.Publish (example.com/inhouse/kafkabus/v2) at main.go:158",
			"kafka billing.invoice.requested: ports.CacheRepository.Set (example.com/inhouse/kvcache) at main.go:149",
		})
	})

	t.Run("gateway/routes", func(t *testing.T) {
		expect(t, endpointFacts(gateway), []string{
			"inbound http listener :8090 in=gateway",
			"inbound http server GET /health handler=main.ok in=gateway",
			"inbound http server GET /v1/balances/{account} handler=main.ok in=gateway", // chi Route
			"inbound http server POST /v1/credits handler=main.ok in=gateway",           // Group inside Route
			"inbound http server GET /admin/stats handler=main.ok in=gateway",           // Mount of a returned router
			"inbound http server GET /version handler=main.ok in=gateway",               // unset BaseURL is ""
			// Routers captured by Group closures, mounted after them (onboarding): the
			// mount's prefix, also through a router passed to a helper.
			"inbound http server GET /api/storefront/init handler=main.ok in=gateway",
			"inbound http server GET /users/{user_id}/ handler=main.ok in=gateway",
			"inbound http server POST /users/{user_id}/documents/ handler=main.ok in=gateway",
			// iris: routes under nested Party prefixes, typed parameters without
			// their macros, the handler after a route's middleware.
			"inbound http listener :8091 in=gateway",
			"inbound http server GET /ping handler=main.pong in=gateway",
			"inbound http server GET /backoffice/payments/{payment_id} handler=(*main.adminHandler).payment in=gateway",
			"inbound http server POST /backoffice/payments/{payment_id}/refund handler=(*main.adminHandler).refund in=gateway",
			"inbound http server GET /backoffice/files/{name...} handler=(*main.adminHandler).file in=gateway",
		})
	})

	t.Run("journeys/wrapped", func(t *testing.T) {
		// Middleware roots keep their own position; the handler each wraps is recorded for answers to cite.
		var got []string
		for _, m := range []*model.Manifest{billing, inventory, gateway} {
			for _, j := range m.Journeys {
				for _, w := range j.Wrapped {
					got = append(got, m.Service+" "+j.Transport+" "+j.Key+" wraps "+w.Function)
				}
			}
		}
		expect(t, got, []string{
			"billing amqp billing.reminders, billing.reminders.urgent wraps (*main.handler).remind",
			"billing kafka billing.invoice.paid wraps (*main.handler).invoicePaid",
			"billing kafka billing.invoice.requested wraps (*main.handler).invoiceRequested",
			"inventory http GET /health wraps main.health",
			"inventory http GET /stock wraps (*main.memStore).stock",
		})
	})

	t.Run("notifier/declared", func(t *testing.T) {
		if d := notifier.Declared; d == nil || d.Language != "python" || notifier.Module != "notifier" || !slices.Equal(notifier.Settings, []string{"DB_HOST", "SMTP_HOST"}) {
			t.Errorf("declared %+v, module %q, settings %v", d, notifier.Module, notifier.Settings)
		}
		expect(t, endpointFacts(notifier), []string{
			"inbound grpc server NotifierService handler=NotifierServicer in=notifier",
			"outbound http client POST http://smtp-relay/send in=notifier",
			"outbound sql query notifications in=notifier",
		})
		expect(t, journeyFacts(notifier), []string{ // one per method, using what the file says it uses
			"grpc NotifierService/Notify roots=NotifierServicer components=notifier outbound=http client POST http://smtp-relay/send (sync),sql query notifications (sync)",
		})
	})

	t.Run("system/links", func(t *testing.T) {
		var got []string
		for _, l := range sys.Links {
			got = append(got, fmt.Sprintf("%s -> %s %s %s %s %s", l.From, l.To, l.Transport, l.Key, l.Kind, l.Confidence))
		}
		expect(t, got, []string{
			"orders -> inventory http GET /items/{sku} sync pattern",
			"orders -> inventory kafka orders.created async exact",
			"ledger -> billing kafka billing.invoice.requested async exact",
			"ledger -> notifier grpc NotifierService sync exact",                                 // a declared service
			"billing -> ledger grpc LedgerService sync exact",                                    // the SDK's call, matched by contract
			"billing -> gateway http POST /backoffice/payments/{payment_id}/refund sync pattern", // an iris typed parameter
			"billing -> ledger redis billing:invoice:* shared-store name-only",                   // the same Redis key pattern
		})
		var dangling []string
		for _, d := range sys.Dangling {
			dangling = append(dangling, strings.TrimSpace(fmt.Sprintf("%s %s %s %s %s", d.Service, d.Transport, d.Role, d.Key, d.Reason)))
		}
		expect(t, dangling, []string{
			"notifier http client POST http://smtp-relay/send",   // the declared service's mail relay
			"pipeline http client GET http://feed:8080/readings", // the main journey sample's feed
			"ledger grpc client LedgerService same-service",      // the gateway's client, outside generated code: no continuation
			"billing amqp consumer billing.reminders, billing.reminders.urgent",
			"billing amqp consumer billing.audit same-service", // the broker repository's own queue
			"billing amqp consumer billing.refunds",
			"billing amqp producer billing.audit same-service",
			"billing amqp producer billing.receipts",
			"billing kafka consumer billing.invoice.paid",
			"billing kafka producer billing.invoice.issued",
			"billing kafka producer billing.payment.recorded",
		})
	})
	t.Run("query", func(t *testing.T) {
		run, err := query.Load(out)
		if err != nil {
			t.Fatal(err)
		}
		md, err := query.Answer(run, []string{"journey", "orders", "POST /orders"}, query.Options{Depth: 2})
		if err != nil || !strings.Contains(md, "→ **inventory** `orders.created` (`main.consumeOrders` @ `main.go:84`)") {
			t.Errorf("journey answer:\n%s\n%v", md, err)
		}
		var handlerAt string // the wrapped handler's position, not the middleware's
		for _, j := range billing.Journeys {
			if j.Key == "billing.invoice.requested" && len(j.Wrapped) == 1 {
				handlerAt = j.Wrapped[0].Position
			}
		}
		if md, err := query.Answer(run, []string{"journey", "billing", "billing.invoice.requested"}, query.Options{}); err != nil || handlerAt == "" ||
			!strings.Contains(md, "@ `"+handlerAt+"`") {
			t.Errorf("journey answer should cite the wrapped handler (%q):\n%s\n%v", handlerAt, md, err)
		}
		if md, _ := query.Answer(run, []string{"path", "ledger", "billing"}, query.Options{Depth: 2}); !strings.Contains(md, "kafka `billing.invoice.requested` async") {
			t.Errorf("path answer:\n%s", md)
		}
		// A program's own work: `journey <service> main`; `who main` is not every service's main.
		if md, err := query.Answer(run, []string{"journey", "pipeline", "main"}, query.Options{}); err != nil ||
			!strings.Contains(md, "`GET http://feed:8080/readings`") || strings.Contains(md, "checkpoints") {
			t.Errorf("main journey answer:\n%s\n%v", md, err)
		}
		if md, _ := query.Answer(run, []string{"who", "main"}, query.Options{}); strings.Contains(md, "main.main") {
			t.Errorf("who main lists the mains:\n%s", md)
		}
		// A library's symbol names its journey, which lists its callers.
		if md, _ := query.Answer(run, []string{"uses", "Sender.Send"}, query.Options{}); !strings.Contains(md, "`journey mailer go mailer.Sender.Send`") {
			t.Errorf("uses answer:\n%s", md)
		}
		if md, _ := query.Answer(run, []string{"journey", "mailer", "go", "mailer.Sender.Send"}, query.Options{}); !strings.Contains(md, "- **tool-admin** cli `purge` @ `cmd/admin/main.go:54`") {
			t.Errorf("library journey answer:\n%s", md)
		}
	})
	t.Run("diff/self", func(t *testing.T) {
		report := filepath.Join(t.TempDir(), "diff.md")
		if err := runDiff([]string{"-base", out, "-head", out, "-out", report, "-fail-on-links"}); err != nil {
			t.Fatal(err)
		}
		if data, _ := os.ReadFile(report); !strings.Contains(string(data), "No architectural change") {
			t.Errorf("a run compared with itself:\n%s", data)
		}
	})
	t.Run("main/journeys", func(t *testing.T) {
		// A program whose work no endpoint starts: main's journey covers it, and
		// stops at the health route's handler, a journey of its own.
		expect(t, endpointFacts(pipeline), []string{
			"inbound http listener :9100 in=pipeline",
			"inbound http server GET /health handler=main.health(…) in=pipeline",
			"inbound http server GET /metrics handler=http.NotFoundHandler(…) in=pipeline", // a library's handler: no journey
			"outbound http client GET http://feed:8080/readings in=pipeline",
			"outbound sql connect postgres in=pipeline",
			"outbound sql query checkpoints in=pipeline",
			"outbound sql query readings in=pipeline",
		})
		expect(t, journeyFacts(pipeline), []string{"http GET /health roots=main.health$1 components=pipeline outbound=sql query checkpoints (sync)"})
		expect(t, mainFacts(pipeline), []string{
			"main main roots=main.main components=pipeline outbound=http client GET http://feed:8080/readings (sync),sql connect postgres (sync),sql query readings (sync)",
		})
		for _, j := range pipeline.Journeys {
			if j.Transport != model.MainTransport {
				continue
			}
			var order []string
			eps := map[string]model.Endpoint{}
			for _, ep := range pipeline.Endpoints {
				eps[ep.ID] = ep
			}
			for _, o := range j.Outbound {
				f := eps[o.Endpoint].Transport + " " + strings.Join(eps[o.Endpoint].Keys, ",")
				if o.Background {
					f += " (background)"
				}
				order = append(order, f)
			}
			if got := strings.Join(order, "; "); got != "sql postgres; sql readings (background); http GET http://feed:8080/readings" {
				t.Errorf("main's outbound in program order: %s", got)
			}
			if !slices.Equal(j.Async, []string{"main.main$1", "main.store"}) || slices.Contains(j.Functions, "main.health$1") {
				t.Errorf("main's goroutines %v, functions %v (not the health handler)", j.Async, j.Functions)
			}
		}
		// Every service's main: its own work, up to the handlers it registers.
		var got []string
		for _, m := range []*model.Manifest{orders, inventory, ledger, billing, gateway, tool, toolAdmin} {
			for _, f := range mainFacts(m) {
				got = append(got, m.Service+": "+f)
			}
		}
		expect(t, got, []string{
			"orders: main main roots=main.main components=cmd/orders,internal/clients/inventory,internal/config,internal/domain/orders,internal/events,internal/httpapi,internal/storage outbound=sql connect postgres (sync)",
			"inventory: main main roots=main.main components=inventory outbound=",
			"ledger: main main roots=main.main components=ledger,internal/pb outbound=grpc client LedgerService (sync),grpc client NotifierService (sync),grpc dial localhost:9091 (sync),grpc dial notifier:9090 (sync)",
			"billing: main main roots=main.main components=example.com/inhouse/ledger-sdk-go,example.com/inhouse/ledger-sdk-go/pb,billing outbound=grpc client LedgerService (sync),grpc dial ledger:9091 (sync),sql connect mysql (sync)",
			"gateway: main main roots=main.main components=gateway outbound=",
			"tool: main main roots=main.main components=cmd/tool outbound=",
			"tool-admin: main main roots=main.main components=cmd/admin outbound=",
		})
		if len(mailer.Journeys) != 5 { // a library has no main
			t.Errorf("mailer journeys: %d", len(mailer.Journeys))
		}
		// Mains whose own calls reach other services are system journeys too.
		var crossing []string
		for _, sj := range sys.Journeys {
			if sj.Transport == model.MainTransport {
				crossing = append(crossing, fmt.Sprintf("%s %s %s (%s)", sj.Service, sj.Transport, sj.Key, sj.Kind))
				crossing = append(crossing, hopFacts(sj.Hops, "  ")...)
			}
		}
		expect(t, crossing, []string{
			"billing main main (sync)",
			"  billing -> external sql mysql (sync)",
			"  billing -> external grpc ledger:9091 (sync)",
			"  billing -> ledger grpc LedgerService (sync)", // the SDK's client, built in main: a link, no method to continue into
			"ledger main main (sync)",
			"  ledger -> external grpc notifier:9090 (sync)",
			"  ledger -> notifier grpc NotifierService (sync)",
			"  ledger -> external grpc localhost:9091 (sync)",
			"  ledger -> external grpc LedgerService (sync)", // the gateway's client of its own server
		})
	})
	t.Run("system/journeys", func(t *testing.T) {
		var got []string
		for _, sj := range sys.Journeys {
			if sj.Transport == model.MainTransport {
				continue // main/journeys
			}
			got = append(got, fmt.Sprintf("%s %s %s (%s)", sj.Service, sj.Transport, sj.Key, sj.Kind))
			got = append(got, hopFacts(sj.Hops, "  ")...)
		}
		expect(t, got, []string{
			"orders http POST /orders (sync)",
			"  orders -> inventory http GET /items/{sku} (sync) => GET /items/{sku}",
			"  orders -> inventory kafka orders.created (async) => orders.created",
			"  orders -> external sql orders (sync)",
			"billing kafka billing.invoice.paid (async)",
			"  billing -> external sql invoices (sync)",
			"  billing -> external sql receipts (sync)",
			"  billing -> external kafka billing.payment.recorded (async)",
			"  billing -> ledger grpc LedgerService/Balance (sync) => LedgerService/Balance",
			"  billing -> external amqp billing.audit (async)",
			"  billing -> external amqp billing.receipts (async)",
			"billing amqp billing.refunds (async)",
			"  billing -> external sql invoices (sync)",
			"  billing -> gateway http POST /backoffice/payments/{payment_id}/refund (sync) => POST /backoffice/payments/{payment_id}/refund",
			"ledger grpc LedgerService/Credit (sync)",
			"  ledger -> external redis billing:invoice:* (sync)",
			"  ledger -> notifier grpc NotifierService/Notify (sync) => NotifierService/Notify",
			"    notifier -> external sql notifications (sync)",
			"    notifier -> external http POST http://smtp-relay/send (sync)",
			"  ledger -> billing kafka billing.invoice.requested (async) => billing.invoice.requested",
			"    billing -> external sql invoices (sync)",
			"    billing -> external redis billing:invoice:* (sync)",
			"    billing -> external kafka billing.invoice.issued (async)",
			// The gateway's routes continue into the methods they forward to, in
			// the same service.
			"ledger http POST /v1/accounts/{account}/credit (sync)",
			"  ledger -> ledger grpc LedgerService/Credit (sync) => LedgerService/Credit",
			"    ledger -> external redis billing:invoice:* (sync)",
			"    ledger -> notifier grpc NotifierService/Notify (sync) => NotifierService/Notify",
			"      notifier -> external sql notifications (sync)",
			"      notifier -> external http POST http://smtp-relay/send (sync)",
			"    ledger -> billing kafka billing.invoice.requested (async) => billing.invoice.requested",
			"      billing -> external sql invoices (sync)",
			"      billing -> external redis billing:invoice:* (sync)",
			"      billing -> external kafka billing.invoice.issued (async)",
			"ledger http GET /v1/accounts/{account}/balance (sync)",
			"  ledger -> ledger grpc LedgerService/Balance (sync) => LedgerService/Balance",
		})
	})
}

func readManifest(t *testing.T, p string) *model.Manifest {
	t.Helper()
	m, err := manifest.Read(p)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// expect compares sorted fact lists.
func expect(t *testing.T, got, want []string) {
	t.Helper()
	got, want = slices.Clone(got), slices.Clone(want)
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("got:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// endpointFacts lists a manifest's endpoints, except the main one (mainFacts).
func endpointFacts(m *model.Manifest) []string {
	var out []string
	for _, ep := range m.Endpoints {
		if ep.Transport == model.MainTransport {
			continue
		}
		f := fmt.Sprintf("%s %s %s %s", ep.Direction, ep.Transport, ep.Role, strings.Join(ep.Keys, ","))
		if ep.Message != "" {
			f += " msg=" + ep.Message
		}
		if ep.Handler != "" {
			f += " handler=" + ep.Handler
		}
		f += " in=" + symbols.Short(ep.Component, m.Module)
		if ep.Count > 1 {
			f += fmt.Sprintf(" x%d", ep.Count)
		}
		out = append(out, f)
	}
	return out
}

func implementationFacts(m *model.Manifest) []string {
	var out []string
	for _, c := range m.Components {
		for _, i := range c.Interfaces {
			out = append(out, path.Base(c.Package)+"."+i.Name+": "+strings.Join(i.Implementations, ","))
		}
	}
	return out
}

func callFacts(module string, calls []model.Call) []string {
	var out []string
	for _, c := range calls {
		arrow := "->"
		if c.ViaInterface {
			arrow = "-.->"
		}
		out = append(out, symbols.Short(c.From, module)+" "+arrow+" "+symbols.Short(c.To, module))
	}
	return out
}

// usesFacts lists the library symbols each journey calls, with the first call
// site when it is in the analysed repository.
func usesFacts(m *model.Manifest) []string {
	var out []string
	for _, j := range m.Journeys {
		for _, u := range j.Uses {
			f := fmt.Sprintf("%s %s: %s (%s)", j.Transport, j.Key, u.Symbol, u.Module)
			if !filepath.IsAbs(u.Position) {
				f += " at " + u.Position
			}
			out = append(out, f)
		}
	}
	return out
}

func librarySymbols(m *model.Manifest) []string {
	var out []string
	for _, l := range m.Libraries {
		if len(l.Symbols) > 0 {
			out = append(out, l.Module+": "+strings.Join(l.Symbols, ", "))
		}
	}
	return out
}

// journeyFacts lists a manifest's journeys, except the main one (mainFacts).
func journeyFacts(m *model.Manifest) []string {
	var out []string
	for _, f := range allJourneyFacts(m) {
		if !strings.HasPrefix(f, model.MainTransport+" ") {
			out = append(out, f)
		}
	}
	return out
}

// mainFacts is the main journey's facts.
func mainFacts(m *model.Manifest) []string {
	var out []string
	for _, f := range allJourneyFacts(m) {
		if strings.HasPrefix(f, model.MainTransport+" ") {
			out = append(out, f)
		}
	}
	return out
}

func allJourneyFacts(m *model.Manifest) []string {
	eps := map[string]model.Endpoint{}
	for _, ep := range m.Endpoints {
		eps[ep.ID] = ep
	}
	var out []string
	for _, j := range m.Journeys {
		var roots, comps, outs []string
		for _, r := range j.Roots {
			roots = append(roots, r.Function)
		}
		for _, c := range j.Components {
			comps = append(comps, symbols.Short(c, m.Module))
		}
		for _, o := range j.Outbound {
			ep := eps[o.Endpoint]
			outs = append(outs, fmt.Sprintf("%s %s %s (%s)", ep.Transport, ep.Role, strings.Join(ep.Keys, ","), o.Kind))
		}
		sort.Strings(outs)
		out = append(out, fmt.Sprintf("%s %s roots=%s components=%s outbound=%s",
			j.Transport, j.Key, strings.Join(roots, ","), strings.Join(comps, ","), strings.Join(outs, ",")))
	}
	return out
}

func hopFacts(hops []model.Hop, indent string) []string {
	var out []string
	for _, h := range hops {
		to := h.To
		if h.External {
			to = "external"
		}
		f := fmt.Sprintf("%s%s -> %s %s %s (%s)", indent, h.From, to, h.Transport, h.Key, h.Kind)
		if h.Journey != "" {
			f += " => " + h.Journey
		}
		out = append(out, f)
		out = append(out, hopFacts(h.Next, indent+"  ")...)
	}
	return out
}

// fleet -api analyses every repository's API: a program's as <name>-api next
// to the program (orders: its code is all internal), a library as itself.
func TestFleetAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("builds SSA for two samples")
	}
	out := t.TempDir()
	if err := runFleet([]string{"-api", "-format", "json,html", "-out", out, "../../testdata/orders", "../../testdata/mailer"}); err != nil {
		t.Fatal(err)
	}
	orders := readManifest(t, filepath.Join(out, "orders", "manifest.json"))
	api := readManifest(t, filepath.Join(out, "orders-api", "manifest.json"))
	mailer := readManifest(t, filepath.Join(out, "mailer", "manifest.json"))
	if orders.Library || !api.Library || !mailer.Library || api.Module != orders.Module {
		t.Errorf("libraries: orders %v, orders-api %v (%s), mailer %v", orders.Library, api.Library, api.Module, mailer.Library)
	}
	roles := map[string]int{}
	for _, ep := range api.Endpoints {
		if ep.Transport == model.APITransport {
			roles[ep.Role]++
		}
	}
	if roles[model.APIRole] != 0 || roles[model.APIInternalRole] == 0 {
		t.Errorf("orders-api's symbols are all internal: %v", roles)
	}
	var sys model.System
	data, err := os.ReadFile(filepath.Join(out, "_system", "system.json"))
	if err != nil || json.Unmarshal(data, &sys) != nil {
		t.Fatalf("system: %v", err)
	}
	for _, l := range sys.Links {
		if l.From == "orders-api" || l.To == "orders-api" {
			t.Errorf("an API linked: %+v", l)
		}
	}
}

// The commands that read a run: query, inventory (over testdata as an
// organisation, with orders as its focus) and deploy (rendered objects
// matched to the inventory and the run).
func TestCommands(t *testing.T) {
	if testing.Short() {
		t.Skip("builds SSA for two samples")
	}
	dir := t.TempDir()
	run := filepath.Join(dir, "run")
	if err := runFleet([]string{"-format", "json,html,agents", "-out", run, "../../testdata/orders", "../../testdata/inventory"}); err != nil {
		t.Fatal(err)
	}

	answer := captureStdout(t, func() error { return runQuery([]string{"-run", run, "-explorer", "none", "services"}) })
	if !strings.Contains(answer, "2 services") {
		t.Errorf("query services:\n%s", answer)
	}
	answer = captureStdout(t, func() error {
		return runQuery([]string{"journey", "orders", "POST /orders", "-run", run, "-json", "-diagram"})
	})
	if !strings.Contains(answer, `"diagram"`) || !strings.Contains(answer, "orders.created") {
		t.Errorf("query journey -json -diagram:\n%.400s", answer)
	}
	if err := runQuery([]string{"-run", run, "journey", "nosuch", "x"}); err == nil {
		t.Error("an unknown service should be an error")
	}

	focus := filepath.Join(dir, "focus.txt")
	if err := os.WriteFile(focus, []byte("# the focus\norders\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inv := filepath.Join(dir, "inventory")
	if err := runInventory([]string{"-root", "../../testdata", "-out", inv, "-focus", focus, "-run", run, "-catalogues", ""}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"inventory.json", "inventory.md", "frontier.json", "frontier.md", "frontier.txt"} {
		if _, err := os.Stat(filepath.Join(inv, f)); err != nil {
			t.Errorf("inventory output: %v", err)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(inv, "inventory.json")); !strings.Contains(string(data), `"name":"orders"`) &&
		!strings.Contains(string(data), `"name": "orders"`) {
		t.Errorf("inventory.json lists no sample:\n%.300s", data)
	}

	rendered := filepath.Join(dir, "rendered.yaml")
	if err := os.WriteFile(rendered, []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: orders
spec:
  replicas: 2
  template:
    spec:
      containers:
        - name: orders
          image: registry.example.com/team-orders:v1
          command: ["/usr/bin/orders"]
          env:
            - name: INVENTORY_URL
              value: "http://inventory:8081"
---
apiVersion: v1
kind: Service
metadata:
  name: orders
spec:
  selector:
    app: orders
  ports:
    - port: 8080
`), 0o644); err != nil {
		t.Fatal(err)
	}
	dep := filepath.Join(dir, "deploy")
	if err := runDeploy([]string{"-rendered", rendered, "-env", "staging", "-inventory", filepath.Join(inv, "inventory.json"), "-run", run, "-out", dep}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dep, "deploy.md"))
	if err != nil || !strings.Contains(string(data), "orders") || !strings.Contains(string(data), "staging") {
		t.Errorf("deploy.md: %v\n%.400s", err, data)
	}
	// The orders sample's pipeline builds team-orders: the inventory has it, the
	// deployment matches the container by that build, and image answers.
	d, err := deploy.ReadFile(filepath.Join(dep, "deploy.json"))
	if err != nil {
		t.Fatal(err)
	}
	if c := d.Workloads[0].Mains()[0]; c.Repo != "orders" || !c.Built {
		t.Errorf("the orders container: %+v", c)
	}
	if err := os.Rename(dep, filepath.Join(run, "deploy")); err != nil { // where a run keeps its deployment
		t.Fatal(err)
	}
	answer = captureStdout(t, func() error {
		return runQuery([]string{"-run", run, "-inventory", inv, "-explorer", "none", "image", "registry.example.com/team-orders:v1"})
	})
	for _, s := range []string{"- **orders** builds `team-orders`:`release-*` @ `bitbucket-pipelines.yml:12` (docker push, `Dockerfile`",
		"→ repository **orders** (its CI builds the image)"} {
		if !strings.Contains(answer, s) {
			t.Errorf("query image lacks %q:\n%s", s, answer)
		}
	}

	// The run as one file: with the inventory next to it and the deployment;
	// questions, diff and a second bundle read it like the directory.
	gz, zst := filepath.Join(dir, "run.tar.gz"), filepath.Join(dir, "private.tar.zst")
	if err := runBundle([]string{"-run", run, "-out", gz}); err != nil {
		t.Fatal(err)
	}
	if err := runBundle([]string{"-run", gz, "-out", zst, "-no-settings", "-no-positions"}); err != nil { // packed again, stripped
		t.Fatal(err)
	}
	answer = captureStdout(t, func() error { return runQuery([]string{"-run", gz, "-explorer", "none", "image", "team-orders"}) })
	if !strings.Contains(answer, "- **orders** builds `team-orders`:`release-*` @ `bitbucket-pipelines.yml:12`") {
		t.Errorf("image from a bundle:\n%s", answer)
	}
	answer = captureStdout(t, func() error { return runQuery([]string{"-run", zst, "-explorer", "none", "services"}) })
	if !strings.Contains(answer, "2 services") || !strings.Contains(answer, "it leaves out configured values and file:line positions") {
		t.Errorf("services from a stripped bundle:\n%s", answer)
	}
	b, err := bundle.Read(zst)
	if err != nil {
		t.Fatal(err)
	}
	if env := b.Deploy.Workloads[0].Containers[0].Env; env["INVENTORY_URL"] != "" || len(env) != 1 {
		t.Errorf("-no-settings left values: %v", env)
	}
	for _, m := range b.Manifests {
		for _, ep := range m.Endpoints {
			if ep.Position != "" {
				t.Errorf("-no-positions left %s's %s", m.Service, ep.Position)
			}
		}
	}
	report := filepath.Join(dir, "bundle-diff.md")
	if err := runDiff([]string{"-base", run, "-head", gz, "-out", report, "-fail-on-links"}); err != nil {
		t.Errorf("a run against its own bundle: %v", err)
	}
	if err := runBundle([]string{"-run", gz, "-out", gz}); err == nil {
		t.Error("a bundle written over the one it reads should be refused")
	}
}

// captureStdout runs fn and returns what it printed.
func captureStdout(t *testing.T, fn func() error) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	done := make(chan []byte)
	go func() {
		data, _ := io.ReadAll(r)
		done <- data
	}()
	err = fn()
	os.Stdout = stdout
	_ = w.Close()
	out := <-done
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestDefaultName(t *testing.T) {
	for _, tc := range []struct{ entry, module, want string }{
		{"example.com/cart/cmd", "example.com/cart", "cart"},
		{"example.com/returns/cmd/grpc", "example.com/returns", "returns-grpc"},
		{"example.com/returns/cmd/subscribers", "example.com/returns", "returns-subscribers"},
		{"example.com/notify/cmd/service/grpc/v1", "example.com/notify", "notify-service-grpc-v1"},
		{"example.com/bus/v2/cmd/create-topic", "example.com/bus/v2", "bus-create-topic"},
		{"example.com/orders/cmd/orders", "example.com/orders", "orders"}, // role repeats the module
		{"example.com/inventory", "example.com/inventory", "inventory"},
		{"example.com/x/tools/gen", "example.com/x", "gen"},
	} {
		if got := defaultName(tc.entry, tc.module); got != tc.want {
			t.Errorf("defaultName(%s) = %q, want %q", tc.entry, got, tc.want)
		}
	}
}

func TestSkipMain(t *testing.T) {
	re := regexp.MustCompile(config.DefaultSkipMains)
	for entry, want := range map[string]bool{
		"example.com/returns/cmd/grpc":          false,
		"example.com/invoicing/cmd/migration":   true,
		"example.com/guardian/cmd/migrations":   true,
		"example.com/notify/cmd/makemigration":  true,
		"example.com/notify/cmd/rollback":       true,
		"example.com/identity/cmd/consumer":     false,
		"example.com/bus/cmd/demo":              true, // a library's demo, not a service
		"example.com/protos/examples/go/sender": true, // nested below examples/
		"example.com/cart/cmd/examplesvc":       false,
	} {
		module := entry[:strings.Index(entry[len("example.com/"):], "/")+len("example.com/")]
		if got := skipMain(re, entry, module); got != want {
			t.Errorf("skipMain(%s) = %v, want %v", entry, got, want)
		}
	}
}

func TestRepoRules(t *testing.T) {
	repo, dir := t.TempDir(), t.TempDir()
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(repo, "go.mod"), "module example.com/orchestrator/v2\n\ngo 1.27\n")
	if got := repoRules(repo, dir); len(got) != 0 {
		t.Errorf("no rule files yet, got %v", got)
	}
	write(filepath.Join(repo, "codegraph-rules.json"), "[]")
	write(filepath.Join(dir, "orchestrator.json"), "[]") // named after the module, without /v2
	want := []string{filepath.Join(repo, "codegraph-rules.json"), filepath.Join(dir, "orchestrator.json")}
	if got := repoRules(repo, dir); !slices.Equal(got, want) {
		t.Errorf("repoRules = %v, want %v", got, want)
	}
}

// fleet hands every requested format on to its link step.
func TestFormatList(t *testing.T) {
	got := formatList(map[string]bool{"html": true, "json": true, "md": true})
	if got != "json,md,html" {
		t.Errorf("formatList = %q, want json,md,html", got)
	}
}

// fleet finds main packages from package clauses: under cmd/ (the usual
// layout), elsewhere only (connector: main.go at the root, cmd/ holds cobra
// commands), or none (a library, skipped before loading).
func TestFindMains(t *testing.T) {
	write := func(root, rel, src string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	usual, rootMain, library := t.TempDir(), t.TempDir(), t.TempDir()
	write(usual, "cmd/server/main.go", "package main\n")
	write(usual, "main.go", "package main\n")
	write(rootMain, "main.go", "package main\n")
	write(rootMain, "cmd/cmd.go", "package cmd\n")
	write(library, "lib.go", "package lib\n")
	write(library, "gen.go", "//go:build ignore\n\npackage main\n") // a go:generate helper
	write(library, "lib_test.go", "package main\n")                 // tests do not count
	write(library, "testdata/x/main.go", "package main\n")          // nor testdata
	write(library, "tools/go.mod", "module tools\n")                // nor nested modules
	write(library, "tools/main.go", "package main\n")
	write(library, "examples/go/sender/main.go", "package main\n") // nor examples (protos)
	for _, c := range []struct {
		name             string
		dir              string
		inCmd, elsewhere bool
	}{
		{"cmd layout", usual, true, true},
		{"root main", rootMain, false, true},
		{"library", library, false, false},
	} {
		if inCmd, elsewhere := findMains(c.dir); inCmd != c.inCmd || elsewhere != c.elsewhere {
			t.Errorf("%s: findMains = %v, %v, want %v, %v", c.name, inCmd, elsewhere, c.inCmd, c.elsewhere)
		}
	}
}
