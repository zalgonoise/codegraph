# CLAUDE.md — codegraph (Go static architecture analysis)

This file carries the design of `codegraph`, so work can continue here: the pipeline, the conventions and decisions
behind them, and what the samples must show. Read it fully before changing code. The README describes the tool for
users.

## Start here

1. **Verify:** `make test` (`gofmt -l . && go vet ./... && go test ./...`). The golden test `cmd/codegraph/main_test.go`
   runs the seven samples (orders, inventory, ledger, billing, gateway, tool, pipeline), the library sample (mailer)
   and the declared sample (`testdata/declared/notifier.json`); unit tests cover `internal/config`, `declared`, `deploy`,
   `diff`, `inventory`, `library`, `link`, `mcpserver`, `model`, `profile`, `query`, `render`, `resolve`, `settings`
   and `transport` (`deploy.TestRender` uses helm when installed). `make samples` writes the samples' run to `tmp/samples`.
2. **Organisations live in profiles.** Nothing here names an organisation: its rules, focus, services described by
   hand, deployment chart, knowledge base check, naming conventions (`profile.json`) and its notes live in a profile
   directory, kept in a repository of its own (README "Profiles"). `make <target> PROFILE=<dir>` (or a profile's own
   Makefile) runs with it. `make audit` with a profile's denylist checks that no file here names the organisation.
3. **Planned work** is in "Next" below, in order; continue from the first item not marked done. As of 2026-10-19
   all of them are done; ask the user what comes next.
4. **The analysed repositories are read-only.** Run with `GOFLAGS=-mod=readonly`; `make graph` checks every clone's
   `git status` before and after. Never write output into them; outputs go to `tmp/` (git-ignored) or a profile's
   `tmp/`.

## Commands

```sh
make test       # gofmt, go vet, go test ./... (with PROFILE, the profile's rule files too)
make cover      # the suite's statement coverage over every package (-coverpkg=./...); fails under COVER_MIN (70)
make samples    # analyse the samples into tmp/samples and link them
make docker-test  # the same tests in the toolchain image (Dockerfile); make docker-graph: make graph in it
make release VERSION=v1.2.0  # bin/codegraph-<version>-<os>-<arch> for darwin and linux, amd64 and arm64, and SHA256SUMS
make help       # every target; make graph, inventory, deploy, query, diff and audit take PROFILE=<dir>

# the samples by hand
go run ./cmd/codegraph analyze -dir testdata/orders -rules testdata/orders/codegraph-rules.json -out /tmp/samples
go run ./cmd/codegraph analyze -dir testdata/inventory -out /tmp/samples
go run ./cmd/codegraph analyze -dir testdata/ledger -rules testdata/rules/inhouse.json -out /tmp/samples
go run ./cmd/codegraph analyze -dir testdata/billing -rules testdata/rules/inhouse.json \
  -include-modules 'example.com/inhouse/*-sdk-go' -out /tmp/samples
go run ./cmd/codegraph analyze -dir testdata/gateway -out /tmp/samples
go run ./cmd/codegraph analyze -dir testdata/tool -entry ./cmd/... -out /tmp/samples
go run ./cmd/codegraph analyze -dir testdata/mailer -library -out /tmp/samples
go run ./cmd/codegraph analyze -dir testdata/pipeline -out /tmp/samples
go run ./cmd/codegraph link -out /tmp/samples/_system /tmp/samples/*/manifest.json
go run ./cmd/codegraph query -run /tmp/samples journey orders "POST /orders"
```

## Goal (from the user)

A tool that parses a Go app using real compiler tooling (go/packages, SSA, CHA/RTA/VTA) rather than hand parsing, and
makes meaningful, visual connections across a codebase. It should answer:

1. What happens in `main` and what is actually in use at runtime.
2. Which in-repo packages are relevant, with their symbols: interfaces (contracts), the concrete types in use, and the
   calls between them.
3. Which transports and messages the service consumes and produces.
4. **Handler-as-entrypoint:** every exposed HTTP route, gRPC method and message-queue consumer is the start of a
   journey. Show what it reaches and how each call connects to another service, a dependency or persistence.

Service diagrams are then linked through tags into a microservice overview, and journeys are chained across services.
Each responsibility is a separate package, initialised and wired in `cmd/codegraph/main.go`.

## Layout and pipeline

```
config → loader → program (SSA) → reach (CHA/RTA/VTA)
       → resolve → transport | entrypoint | components | directives
       → journeys → manifest → render                 (codegraph analyze)
manifest.ReadAll → link (+ journey chains) → render   (codegraph link)
```

| Package | Responsibility |
|---|---|
| `internal/config` | Flags for every command: `analyze`, `link`, `fleet`, `diff`, `query`, `mcp`, `inventory` and `deploy`. For `analyze` and `fleet` (shared analysis flags in `analysisFlags`): `-dir -entry -algo -out -format -service -alias -tag -rules -no-default-rules -entry-depth -resolve-depth -journeys -journey-depth -tests -libs -std -build-tags`, plus `fleet`'s repo arguments and `-skip-mains` |
| `internal/loader` | `packages.Load` with full syntax and types. Records `ModulePkgs` (packages with `Module.Main`, plus packages of modules matching `-include-modules`) and `PkgModule` (`"std"` for the standard library). Fails on package errors, because SSA needs well-typed code |
| `internal/program` | `ssautil.AllPackages(..., ssa.InstantiateGenerics)`, `prog.Build()`, `ssautil.MainPackages` sorted by path (none needed with `-library`), and every built package (`Packages`) |
| `internal/reach` | Roots are `main.init` and `main.main` (`Analyze`), or any functions (`AnalyzeRoots`: a library's API). CHA uses a whole-program graph plus forward BFS. RTA is `rta.Analyze(roots, true)` and also gives `RuntimeTypes`. VTA is `vta.CallGraph(rtaReachable, rta.CallGraph)` plus forward BFS. The result then goes through `DeleteSyntheticNodes()`. Provides `Callers(fn)` and `Callees(site)` |
| `internal/resolve` | `resolve.go`: value recovery (constants, conversions, phi nodes, `+`, `fmt.Sprintf`, parameters via callers, fields and globals via a store index, calls followed with a `frame` binding the callee's parameters to the call's arguments, fields of specific struct values and slice elements (`fieldStrs`, `elements`), `append`, range loops, slice/variadic elements), `MethodStrings(v, m)` (what method `m` returns on v's dynamic types), `Handler(v)` label and `MessageType(v)`. `handlers.go`: `HandlerFuncs(v, methods) []Root`, handler values → the module functions they run, with factory `Bind`ings |
| `internal/transport` | `rules.go`: declarative `Rule` and built-in defaults. `detector.go`: matches call sites in reachable, non-synthetic module functions and returns a `Detection{Endpoint, Func, Site, Roots}` per site. `prefix.go`: router prefixes of `prefixed` routes from `scope` rules |
| `internal/entrypoint` | Ordered bootstrap trace of `main`, following module callees up to `-entry-depth`, classifying each step as construct, register, start, shutdown, signal or call, and flagging `go` and `defer`; then each CLI command's function (`Command`), its steps marked with the command |
| `internal/components` | One component per module package, with a layer guessed from path segments; declared interfaces with contracts and runtime implementations; types in use with their reachable methods; calls between components (with a via-interface flag, package-init edges excluded); `MakeInterface` bindings; third-party libraries |
| `internal/directives` | `//codegraph:service`, `alias`, `tag k=v`, `produces`, `consumes` (with `msg=`, `role=`, `kind=`). A directive inside a function only counts if the function is reachable; returns detections (a `consumes` function is a journey root) |
| `internal/journeys` | One journey per inbound endpoint with resolved handlers (per interface method for gRPC): BFS for reachability, component edges, goroutines and background-only use; DFS in instruction order for the outbound endpoints in program order. `MainDetection`: the main journey's endpoint |
| `internal/manifest` | Builds the manifest: merges endpoints by `model.EndpointID`, counts call sites and keeps them in `Sites`, adds `Tags` of the form `transport:key`, carries journeys; reads and writes JSON |
| `internal/render` | Format-neutral `Graph` in `graph.go`; service and system views in `views.go`; Mermaid and DOT in `formats.go`; Markdown reports in `markdown.go`; journey flowcharts and system sequence diagrams in `journeys.go`; the interactive explorer in `explorer.go` and `explorer.html` (embedded); the answers' diagrams in `answers.go` (`Flow`, `SequenceDiagram`) |
| `internal/link` | `link.go`: matches outbound client/producer endpoints to other services' inbound server/consumer endpoints, adds shared SQL tables, lists dangling endpoints. `journeys.go`: computes each linked outbound endpoint's `Continuation`s (the link and the target journeys) and chains journeys across services into `SystemJourney` hop trees |
| `internal/library` | `-library`: a module's exported API (`API`: functions, methods of exported types, methods of exported interfaces, in public packages), the roots (`Roots`), interface implementations (`Implement`, `PublicTypes`, `ModuleTypes`), the CHA fallback for generic roots (`Generic`), and one inbound `go api` detection per symbol (`Detections`) |
| `internal/model`, `internal/symbols` | Serialised data model (plus `EndpointID`, `DefaultKind`; `api.go`: API keys, `APIIndex` and `APICalls`, pairing journeys' symbol uses with analysed libraries' journeys); naming and position helpers, and `Called` (what a call site calls of another package's exported API) |
| `internal/query` | `codegraph query` (`Answer` dispatches `services`, `service`, `journey`, `who`, `path`, `impact`, `uses`, `search`; Markdown or JSON; `Options`), the answers' diagrams and explorer links (`diagram.go`), and the agent files: `Guide` (AGENTS.md) and `Context` (context.md) |
| `internal/mcpserver` | `codegraph mcp`: the MCP server (official Go SDK, stdio). One tool per query command over `query.Answer` (`diagram` on five of them), resources `codegraph://guide` and `codegraph://context`, the run reloaded when replaced |
| `.mcp.json` | Starts the MCP server for Claude Code sessions in this repository (`go run ./cmd/codegraph mcp -run tmp/graph`) |
| `.claude/skills/codegraph/SKILL.md` | Claude Code skill: answer architecture questions from a run with `codegraph query` or the MCP tools |
| `internal/inventory` | `codegraph inventory`: `scan.go` reads every repository without building (languages from marker files, last commit, each Go module's direct requirements, replaces, mains and in-house imports; `Mains(dir)` is also `fleet`'s main discovery); `report.go` writes `inventory.md`; `frontier.go` finds the repositories around a focus worth a full analysis; `contracts.go` reads the gRPC services of the contracts catalogues (their `proto/` trees) and writes `contracts.md` against a run; the organisation's naming conventions come from `Conventions` (a profile's `profile.json`) |
| `internal/declared` | Services codegraph cannot analyse (other languages), described by hand in `<dir>/<service>.json`: `File.Manifest` turns one into an ordinary manifest (marked `Manifest.Declared`, its Module the repository's name) with journeys from `methods`/`reaches`; `fleet -declared` writes them into the run before linking |
| `internal/settings` | The environment variables a main reads: `env` tags of the types its reachable code converts to interfaces (with `envPrefix`), the fields of specs passed to kelseyhightower/envconfig's `Process`, and constant `os.Getenv` names outside the standard library; `Manifest.Settings` |
| `internal/inventory/images.go` | Container images: `Images` (what a repository's CI and compose files build: shell `docker build/tag/push` with variables expanded, `build-push-action` tags, reusable workflows by `Conventions.WorkflowImage`, compose `build`+`image`, Jenkins `docker.build`), `References` (what it uses without building: `FROM`, compose, CI step images, Helm values, manifests), `Inventory.Builders`/`Referrers` |
| `internal/deploy` | `codegraph deploy`: `render.go` runs `helm template` with masked secret values files; `deploy.go` reads the rendered objects (workloads, containers, settings, Services, routes); `match.go` matches each main container to a repository and an analysed service; `network.go` checks the run's gRPC and HTTP links against the configured addresses; `report.go` writes `deploy.md`; `view.go` reads `deploy.json` back and gives one service's view (`Deployment.View`), which `codegraph query service` and the explorer show |
| `internal/diff` | `codegraph diff`: compares two runs (links both): links, services, endpoint signatures per service, unanswered topics; Markdown report, `-fail-on-links` for CI |
| `Dockerfile`, `.dockerignore` | The toolchain image (Go, pkg-config + libxml2, python3, make; `CODEGRAPH=codegraph` prebuilt) for `make docker-*` |
| `Makefile` | `make graph` (fleet, then with a profile the deployment and the knowledge base check, and the diff with the previous run; clones checked unchanged), `inventory`, `diff`, `open`, `samples`, `test`, `audit`, …; a profile's `profile.mk` sets its variables |
| `internal/profile` | An organisation's profile: `Load` reads `<dir>/profile.json` (`-profile`, or `$CODEGRAPH_PROFILE`) into `inventory.Conventions` and the contracts catalogues |
| `testdata/stubs/*` | Offline stand-ins for grpc, grpc-gateway's runtime (`gateway`), gorm (+ mysql driver), chi, iris, cobra, zalgonoise/x/cli v2 (`xcli`), kelseyhightower/envconfig, and in-house style libraries (`kafkabus` for Kafka, `rabbitbus` for RabbitMQ, `kvcache` for Redis) and a `*-sdk-go`, wired into the samples with `replace` |
| `testdata/rules/inhouse.json` | Rules for the in-house style stubs: what an organisation's profile holds for its own libraries |

Outputs per service in `<out>/<service>/`: `manifest.json`, `service.mmd`, `service.dot`, `report.md` (with a
"Journeys" section: a summary table and one Mermaid flowchart per journey). The link step writes
`system.{json,mmd,dot,md,html}`; `system.md` has "Journeys across services" with one Mermaid `sequenceDiagram` per
journey that reaches another service (a summary and the first steps above `maxSequenceHops` = 200 steps or
`maxMermaidText` = 45,000 characters), and `system.html` is the interactive explorer.

## Conventions and design decisions

- **Rule argument indexes include the receiver at index 0 for methods**, including interface ("invoke") calls. Use
  `resolve.Args(common)` everywhere, never `common.Args` directly.
- **`*` (`model.Unknown`) marks unresolved parts** of a value. Consecutive stars collapse into one. Linking treats `*`
  as a glob, and a key that is only `*` never links.
- **Only reachable module code is scanned.** Dead code, mocks and test helpers never produce endpoints or
  implementations. Interface implementations come from RTA runtime types; with CHA they fall back to all named module
  types (so CHA lists `orders.Mock`).
- **Synthetic functions** (wrappers) are skipped by the detector and component counting. They are still indexed by
  the resolver, because the package `init` holds global stores. Package initialisers never count as component calls.
- **User rules come before the defaults**; the first matching rule wins, one endpoint per call site.
- **Inbound endpoints are attributed to the handler's package** when that package is in the module, otherwise to the
  package of the registering call. For a single wrapped handler (`mw.Wrap(h.X)`), the wrapped handler's package and
  name win: the label becomes `(*mw.M).Wrap((*pkg.H).X)`.
- **Rule `Recv` matching:** `""` means a package-level function, `"*"` means anything, `"A|B"` lists alternatives;
  `RecvPattern` is an extra regexp on the receiver type name. `Pkgs` entries match exactly, or as a whole tree with a
  `/...` suffix. `Transform` is `sql-table`, `table-name`, `grpc-method` or `iris-path`, validated in `Rule.compile`,
  like `Kind`.
  `KeyMethods` (`{arg, method}`) takes the key from what a method returns on the argument's dynamic types (looked up
  on element types too, so gorm's `Find(&[]Model)` finds `Model.TableName()`); an in-house `Publish(ctx, data)` uses it
  with `Topic` (`testdata/rules/inhouse.json`), gorm with `TableName`. `SkipUnknown` drops a call site whose keys resolve to only `*` (gorm's
  `Where(…).Find(&rows)` after a `Table`). Remember the receiver is index 0: rabbitbus's `Publish(ctx, queue, …)` has the
  queue at 2.
- **Per-caller variants:** an outbound site whose keys or message come from its function's parameters (a shared
  `publish(ctx, d)` helper) is resolved once per module caller, with `Resolver.InCall(fn, callerArgs)` binding the
  parameters (2–16 callers). When the callers disagree, `detect` returns one detection per distinct key set and
  message, each with `Detection.Callers`; each site records its caller in `model.Site.Via`. The walk records which
  call sites each function was entered through (`walk.via`), and `outbound.forJourney` keeps only the variants of
  those callers, or all of them when the helper was reached another way. `dynamicTypes` takes the frame too, so a
  callback the caller passes (`Publish(ctx, buildPayload(id))`) resolves to that caller's closure.
  `Detector.message` falls back to an interface argument's single dynamic type.
- **Config-held keys and handlers:** a dotted `keyFields` field (`Queues.PriorityQueue`) goes through nested fields
  and slice elements (`Resolver.fieldPath`), from the particular value only, with no fallback to every store.
  `handlerFields` takes the handler from a config argument's field (`Resolver.FieldValues`). `fieldStrs` is now
  `fieldVals` (the values a field holds) plus `strs`, and an unset string field is the constant `""`.
- **Fallback rules** (`Rule.Fallback`): a rule describing a consumer's whole subscription. `Detector.dropCovered`
  removes the keys another inbound endpoint of the same transport registers in the service, and the endpoint when
  none remain. rabbitbus's `NewConsumer(ConsumerConfig{Handler, Queues})` is one: with a `ConsumeMux` handler, a mux
  wrapped in middleware or a service's own router, the registrations describe every queue and the consumer adds
  nothing. Only a plain handler (billing's `billing.refunds`) gives an endpoint of its own.
- **`-include-modules`** (`path.Match` on the module path, with or without `/vN`): matching modules' packages join
  `ModulePkgs`, so they are scanned, become components (layer `adapters` for `*-sdk-go`) and are walked by journeys.
  Use it for an organisation's SDKs (`example.com/*-sdk-go`). Without it, every gRPC call made through an SDK is
  invisible, because the generated client is called inside the SDK.
- **`KeyDefault`** (`{funcs, arg, field}`): when `KeyArgs[0]` resolves to `""`, the key is that field of the config the
  receiver was constructed with (`Detector.ctorCalls` walks the receiver back to the constructor call). The walk goes
  through fields, also of struct values (`*ssa.Field`), heap-spilled locals (`Resolver.Stores` handles `*ssa.Alloc`),
  free variables, parameters, phi nodes and module constructors. rabbitbus's `Publish`/`Consume(ctx, "", …)` use
  `rabbitmq.New`'s `PriorityQueue`.
- **Router prefixes:** rules with `Scope` (`returns`, `keeps`, `passes`, `mounts`, with the router argument in
  `ScopeArg`) describe chi `Route`/`Group`/`Mount`/`With`, gin/echo `Group`, gorilla `PathPrefix`/`Subrouter` and
  iris `Party`.
  They produce no endpoint (the detector skips them). `Prefixed` route rules compose `Detector.prefixes(receiver)`:
  through scoping calls, the router a `Route`/`Group` callback is given (an index of functions passed to `passes`
  calls), where a router value or a constructor's returned router is mounted, parameters (callers), and fields or
  globals (`Resolver.Stores`). A router captured by a closure lives in a heap cell: its free variable is read
  through the cells its `MakeClosure`s bind (`cells`), and a router stored in a cell is mounted wherever a read of
  that cell is, in its function or in closures (`loads`). `joinPath` composes `/api` + `/x` (a `/` route under a
  mount keeps its trailing slash; linking trims slashes); a config-driven prefix is `*`. `maxPrefixDepth` is 16.
- **Resolver call-site sensitivity:** when a value is resolved through a call (`fromCall`, `fieldStrs`, `elements`), the
  callee's parameters are bound to that call's arguments (`frame`). A parameter reached directly (no frame) still
  resolves to the union over all callers. Module callees are followed fully. Library callees are followed only when
  they have a single block (`Topic()`, `Topic.String()`), so branching code like `os.Getenv` stays unknown, and a
  library function's unbound parameter is never traced to its callers. A field of a value that `fieldStrs` cannot
  follow falls back to every store of that field in module code. A library package-level variable resolves from its
  package initializer (gorm's mysql dialector `Name()` returns `var DefaultDriverName = "mysql"`).
- **More resolver facts:**
  - A string field of a composite literal that is never set, and whose address does not escape, is `""`
    (`zeroField`). Without it, an unset `ChiServerOptions.BaseURL` gave every route of a service a `*` prefix.
  - `fieldStrs` of an unbound struct parameter takes what each caller passes.
  - A closure's free variable resolves to its `MakeClosure` binding (`freeVarBindings`), whether captured by value
    or by reference.
  - Every unwrapping step looks through `ChangeInterface` (a `chi.Router` passed as an `http.Handler`), which used to
    cut resolution chains silently.
  - A struct field with no value from its particular struct (`fieldStrs`) nor any store in module code
    (`fromField`) takes its `envDefault` tag next to an `env` tag, or its `default` tag next to an `envconfig` tag
    (`Resolver.envDefault`): an env library fills it by reflection. Only for types of module packages (a broker library's `RABBITMQ_PRIORITY_QUEUE` defaulting to
    `priority_queue` would link every user of the library), and not for defaults naming the local machine
    (`localAddress`: `:8082`, `127.0.0.1:50051`, `http://localhost:8080`, development placeholders).
  - A key of several arguments skips an empty first part: amqp091's `PublishWithContext(ctx, "", queue, …)` (the
    default exchange) is keyed by the queue alone, not `" queue"`.
- **Interaction kind:** `Endpoint.Kind` (from `Rule.Kind` or `kind=` in directives) or `model.DefaultKind(transport,
  role)`: producers and consumers are async, client/server/query/connect/dial/listener are sync, and custom roles fall
  back on the transport. Links and journey outbound entries use `Endpoint.InteractionKind()`. `Kind` enters
  `EndpointID` only when set, so the IDs of existing endpoints did not change.
- **Library symbols used** (`symbols.Called`, `Journey.Uses`, `Library.Symbols`): what a call site calls of another
  package's exported API: the static callee (generic origin; a synthetic wrapper names the method it stands for), or
  the interface method an invoke calls (an interface of another module counts as that module's API). Calls of func
  values, closures, package initializers and unexported names are not counted. A journey records, for every function
  its walk reaches, the calls into packages outside `ModulePkgs` (so included modules' code counts as the service's
  own, and their calls count too), grouped by module and symbol with the first call site and the number of sites; the
  standard library only with `-std`. `Library.Symbols` is the service-wide set, from all reachable module code. A
  library reached only through interfaces of the standard library (a `fmt.Stringer` implemented by `uuid.UUID`) has
  call edges but no symbol. `query.Uses` (`uses`, MCP `uses`) matches a module or package path exactly, else part of a
  symbol's name.
- **Main journeys** (`journeys.MainDetection`, `-main-journey`, default on): each main package gets an inbound
  endpoint of transport `main` (`model.MainTransport`), role `run`, key `main`, `Source` `main`, rooted at
  `main.main`; `Analyze` gives its walk and its program-order DFS a stop set, the roots of every other inbound entry,
  so the handlers main registers keep their own journeys (closures, factories' closures and middleware closures are
  roots, so they stop too). What remains is the work no endpoint starts: a pipeline, a job, a cron, or a server's
  startup (configuration, connections, dials, listeners). The role is neither initiating nor accepting, so the
  endpoint never links nor dangles; `query.keyEndpoints` (`who`), `search`'s keys and the explorer's key index and
  directory chips leave it out. A main whose calls link elsewhere (a client built in `main`) becomes a system
  journey. Asked for by the user on 2026-10-19 after zalgonoise/x/audio, a pipeline whose only route serves
  Prometheus's handler, had no journey: its main journey covers 273 functions over 19 components (wav, fft, the
  exporters), with the sqlite connect, migrations and `headers`/`chunks` queries before the stream's `GET`, in
  program order. Libraries have none.
- **Container images** (`internal/inventory/images.go`, 2026-10-26): `Repo.Images` are the images a repository
  builds, `Repo.References` those its files use without building (minus its own). Shell lines of CI files
  (`ciFiles`: Bitbucket, GitLab, GitHub workflows, CircleCI, Azure, Cloud Build, Jenkinsfiles, Makefiles, `*.mk`,
  scripts at the root and in `scripts/`, `ci/`, `.ci/`, `build/`, `deploy/`) are read in order with a variable map:
  `X=…`, `export X=…`, `- X=…` (YAML list items), Make's `X ?= …`, and the `variables`/`env` maps of YAML files;
  `expand` resolves `$X`, `${X}`, `${X:-default}`, `$(X)` (Make), `${{ env.X }}`/`inputs`/`vars`, `${env.X}`
  (Groovy), and makes everything else (`$(command)`, secrets, Make's `$*`) `*`. `docker build -t`, `docker tag` and
  `docker push` (also `buildx`, `image push`) give references; a build's `-f` follows the reference as written
  through `docker tag` to what is pushed. `imageOf` drops the registry (a first segment with `.`/`:`, `localhost`,
  or one holding `*`) and the tag, lowercases, and fails when the last segment is unknown (`docker/*` would match
  every image). One image per name, the most telling way first (`viaRank`: push, then build-push-action/workflow
  inputs/Jenkins, compose, tag, build). `Scan` takes the conventions (`WorkflowImage`, `{INPUT}` placeholders filled
  from a job's `with`). References skip templated files (`{{`): `codegraph deploy` renders those. The deployment
  matcher (`deploy.Match`) takes `Repo.Images`: builders by whole name, else last segment; among several, the one
  `findRepo` names, else the latest `LastCommit` (`Container.Built`, `AlsoBuilt`). `query.Image` (`image`, MCP
  `image`) answers from `Run.Inventory` (`<run>/../inventory/inventory.json`, or `-inventory`) and `Run.Deploy`. Asked
  for by the user on 2026-10-26, after images found only by reading pipelines and Helm values, and one matched by
  name to an unrelated repository.
- **Libraries** (`internal/library`, `analyze -library`, `fleet -libraries`, Makefile `LIBRARIES`): a module without a
  `main`, analysed through its exported API. The API is the exported functions, the exported methods declared on
  exported types, and the methods of exported interfaces, of the packages `library.Public` keeps (no `internal`,
  `testdata`, `examples`, `demos` or `cmd` segment, never `main`) and, with `-internal` (default on), those
  `library.Internal` keeps (an `internal` segment, not under the others): `Symbol.Internal`, endpoints of role
  `internal` (`model.APIInternalRole`) instead of `api`. Internal symbols are analysed like the rest but hidden by
  default: `render.Service` leaves them out unless `ServiceOptions.Internal` (so `service.mmd`), the report lists them
  in an "Internal API" table and draws no flowchart for them, the explorer embeds a second service diagram
  (`service+internal:<name>`, only when there are any) and shows them, in lists, counts, search and that diagram,
  behind a header toggle ("Internal API", off by default, `localStorage` `codegraph-internal`), and the query marks
  them (`JourneyRef.Internal`) and lists them apart (`service`, `services`' "go internal", `context.md`'s count).
  `fleet -api` (Makefile `API`, implies `-libraries`) also analyses every repository with mains as a library, from
  its own load of `./...` (a failure there leaves the program's analysis alone), named `<defaultName>-api`; a module
  without an API (`errNoAPI`) is only logged, without type aliases (an alias of grpc's generic
  stream interfaces listed grpc's methods under a contracts module's packages) and, unless `-generated`, without the symbols of
  generated files (`library.Generated`: `ast.IsGenerated` over the loader's syntax; a protobuf contracts
  module's generated messages alone have thousands of getters and boilerplate methods, which made manifests of tens
  of MB). Each symbol is an inbound endpoint (transport `go`,
  role `api`, `Source` `library`, `Rule` its kind) keyed `qualifier.Member` (`model.APIQualifier`: the module's base
  name without `/vN`, then the package's path below it; `model.APIMember`: the label without type arguments or
  pointer marks), its `Component` the package's import path; journeys come from the ordinary `journeys.Analyze`.
  Roots: the symbols' functions and the module's non-main package initializers. An interface method runs the
  implementations among the module's types: `PublicTypes` (exported types of public packages) plus the RTA runtime
  types of a pass (types the API converts to interfaces; `ModuleTypes` for CHA), up to 4 passes until no root is added.
  Generic interfaces and types are matched by method names (`types.Implements` fails across type parameters), and a
  method is looked up through the method set, so promoted methods count. RTA and VTA panic on a generic origin as a
  root (`ForEachElement called on type containing *types.TypeParam`), so an API with generic roots (`Generic`) is
  analysed with CHA, which handles generic bodies. A library manifest has `Library`, no bootstrap, `Entry` the module
  and the name `-service`, `//codegraph:service` or `defaultName(module, module)`. `link.Link` lists it
  (`SystemService.Library`) but links nothing to or from it (no links, dangling, shared stores or chains). The query
  marks it `(library)`, `uses` names each symbol's journey (`SymbolUsers.API`, by `model.APIIndex.Find` on the use's
  package, or the module's root package for symbols recorded without one), a library journey lists its callers
  (`JourneyDetail.UsedBy`); the explorer gets `api` pairs (caller journey, symbol journey), shows "Called by" on a
  library's page and journeys and "Library API it calls" on callers' journeys, and colours the `go` transport.
- **Ordering is deterministic:** functions are sorted by `fn.String()` and outputs are sorted. Journey outbound
  endpoints are in program order, which is deterministic too. Keep it that way so manifests can be diffed.
- **Service names:** `-service` (only when a single main package matches), then `//codegraph:service`, then
  `defaultName`: the module's base name (without a `/vN` suffix) for `<module>/cmd` or the module root,
  `<module>-<role>[-…]` for `<module>/cmd/<role>[/…]` (`orders-grpc`, `orders-jobs-sendreminder`,
  `notifier-service-grpc-v1`) unless the role repeats the module name, and the directory name otherwise. `fleet`
  refuses to link a second service with an existing name.
- **`fleet`** analyses each repository in-process with the same flags. Without `-entry`, `findMains` reads package
  clauses (no loading): `./cmd/...` when `cmd/` holds a `main`, `./...` when mains are elsewhere only (a root
  `main.go` whose `cmd/` holds cobra commands), and no `main` at all is a library, skipped and reported as one
  (also a library whose only mains are examples). Test files, vendor, testdata,
  `example(s)`/`demo(s)` directories, hidden dirs, nested modules and `//go:build ignore` files do not count. Mains
  matching `-skip-mains` are skipped (migrations, rollback, seed, anything under `examples/` or `demo/`). Transport and observability libraries stay out of the graph by design: rules describe their APIs. It recovers a panic per
  repository and reports failed repositories without stopping, always writes JSON, then links into `<out>/_system`.
- **Inventory and frontier** (`internal/inventory`): "in-house" is whatever module some scanned repository declares
  (longest prefix wins). go.mod is parsed strictly, then laxly (`ParseLax` drops `replace`). A repository is a
  program when its root module has a main (`Repo.Service`), as `fleet` loads the root module only: a library whose
  nested `v2` module builds a `create-topic` tool would otherwise make every importer of the library a caller. With a run, the frontier
  takes the focus services' modules from it (`Hints.Modules`); without one, it finds only the importers of the focus
  programs' modules and SDKs (5 programs instead of 21), hence `make org`. Hints
  come from the focus services only, linked on their own (`frontierHints`), so a run that already includes the
  frontier gives the same frontier (checked: 16 programs either way). Proto package → import path is
  `/contracts/<package with dots as slashes>` (43 of 44 served contracts; the other is grpc health). Left out of
  the frontier: copies of focus modules (`orders-test`, a fork: their services would take the focus
  services' names), the focus services' own `<name>-sdk-go` repositories, and, from `frontier.txt`, programs with no
  commit in the two years before the inventory's newest commit.
- **Deployments** (`internal/deploy`): `helm template release <chart> -f values.yaml -f overrides.yaml -f <masked
  secrets>` with `KUBECONFIG=/dev/null`. The chart's templates read secret values (`<service>.bucket.s3_key_id`), so
  rendering needs the secrets files' keys: `maskSecrets` copies them to a temporary directory with every value
  `(secret)` and the `sops` key dropped; ciphertext never reaches helm or the outputs (`masked` also hides any value
  containing `ENC[`). Rendered YAML is decoded through `yaml.Node` (`value`), last key wins, because rendered
  ConfigMaps repeat keys (`COMPLIANCE_URL`). Settings: `envFrom` ConfigMaps, then `env` literals and
  `configMapKeyRef`s; secret variables by name only. A pod's main containers (`Workload.Mains`) are all its
  non-init containers except known sidecars (fluentd, linkerd, envoy, …): one pod may run a v1 and a v2, or a
  repository's grpc and subscribers mains. Each is matched on its own (`Match`): the image name gives the repository (whole name,
  endings, beginnings that are not an image group, then one edit for long names; a group image naming none falls back
  to the workload's name when all its containers run that image), and the command gives the main (binary path below
  `bin/`, `_app`/`-cli`/`job` suffixes, subcommands, one letter apart, the repository's only main unless the binary
  is specific and the repository has several mains, a generic binary's main named like the repository, then a unique
  part of the name). What remains is reported as ambiguous.
- **Settings** (`internal/settings`, `Manifest.Settings`): the environment variables a main reads. The `env` tags
  (name before the first comma; `-` skipped) of every type reachable code converts to an interface (`MakeInterface`:
  `env.Parse(&cfg)`, `env.ParseAs[T]()`'s instance), walking pointers and nested struct fields with `envPrefix`
  composed, plus constant `os.Getenv`/`os.LookupEnv`/`syscall.Getenv` arguments outside the standard library. RTA's
  `RuntimeTypes` would not do: they include every type reachable from a converted one by reflection, so a nested
  `kafkaConfig` became a root of its own (bare `BROKERS` next to `KAFKA_BROKERS`). Computed names are not seen.
  kelseyhightower/envconfig reads every exported field, so its names come only from the spec passed to
  `envconfig.Process`/`MustProcess` (`envconfigSpec`), as its `gatherInfo` builds them: the field name (camel case
  split with `split_words`) or the `envconfig` tag, after the constant prefix and `_`, uppercased; a nested struct's
  key prefixes its fields (embedded ones keep the prefix); a type with `Decode`, `Set`, `UnmarshalText` or
  `UnmarshalBinary` is one setting; `ignored:"true"` is skipped. For a tagged field the tag alone is listed: the
  library always looks it up, and it is the name people set.
- **CLI commands** (transport `cli`, role `command`, inbound, sync): a command is an endpoint keyed by its name whose
  journey starts at the function it runs. Rules: cobra's `AddCommand` (`elements`: one endpoint per command given,
  `Use` and `RunE`/`Run` read on each) and `Execute*` (the root itself), `first-word` of `Use`, `requireHandler`
  dropping groups without `Run`/`RunE`; zalgonoise/x/cli v2's `WithExecutors` (`mapEntries`: one endpoint per
  key/value pair, at the pair's own line) and v1's `WithFlag`. urfave/cli is not covered: none of the code it was
  checked against (2026-10-19) uses it. The resolver's `MapEntries` follows a map to its literal's
  `MapUpdate`s (phi nodes, calls, stores, parameters); `Elements` exposes the slice elements `elements` finds; a
  global's fields are read from the value copied into it (`var cmd = cobra.Command{…}`: SSA builds the literal in a
  local). The bootstrap trace continues into each command's root after `main`'s own steps (`entrypoint.Command`), a
  step of kind `command` opening each, and the report gives each its own table.
- **Links against configured addresses** (`Deployment.Check`, `Network`): a setting whose name looks like an address
  (`HOST|ADDR|URL|URI|ENDPOINT|TARGET|SERVER`, not `PROXY`, one value) and whose host is a Kubernetes Service (not the
  caller's own) is a call to the services that Service runs. The port (in the value, or `<PREFIX>PORT`, or the one
  `<PREFIX>…PORT`: `GRPC_NOTIFY_V2_PORT`) picks the container when one pod runs several mains: a container exposing
  the Service port's target, or else containers named like the port (`grpc-v1`, `grpc-v2`, `rest`). Only gRPC and
  HTTP links are compared: messaging goes through a broker. A caller linked to several servers of one contract is
  `settled` to the one it dials, and the others are not counted as unconfigured. A `*MOCK*=true` setting whose
  name contains a linked callee's repository name (4+ letters) is listed as `mocked`. A configured pair with no
  link records the settings none of the caller's code reads (`Pair.Unread`, `Checked` when the run recorded the
  caller's settings): all unread is "read by nothing in the caller's code", otherwise "read by the code, no call
  found".
- **`INCLUDE_MODULES`** (Makefile, space-separated, one `-include-modules` each; a profile sets it): an
  organisation's SDKs (`example.com/*-sdk-go`) plus its client libraries not named like an SDK, which the address
  check finds (configured, no link in the code). Packages of any included module are components of
  layer `adapters`.
- **HTTP linking** requires the URL host (lowercased, port stripped) to equal the target's name, one of its aliases,
  or `tags["host"]`. A host that is unknown or `*` links on path alone, with confidence `path-only`, but only when
  the client path has a literal segment and the route is not a catch-all or only `*`. Without that, the generic
  external-API clients (`GET */*/*`, `POST */*`) linked falsely to every service's `/metrics`
  and `/health` routes. Paths containing `*` get confidence `pattern`.
- **Shared stores** (`link.sharedStores`), only for keys with a letter or digit (`named`; `*:*:*` is not a store):
  SQL tables and Redis key patterns (`redis query` endpoints from the
  built-in `redis-key` rule and in-house rules such as `kvcache-key`; `fmt.Sprintf` verbs become `*`), reported only
  across modules, as `name-only`.
- **Declared services** (`internal/declared`): `declared/<service>.json` (strict JSON, unknown fields rejected) lists
  endpoints written as the detector would (gRPC servers keyed by service with `contract`, calls `Service/Method`,
  routes, `METHOD http://host:port/path` calls, topics, tables, keys, `*` for unknowns), each with its `position` in
  the service's repository. Inbound endpoints list what they use as `"<transport> <key>"` of the file's outbound
  endpoints (`"*"`: all): `reaches` gives one journey, `methods` (gRPC servers) one per method; an unknown reference
  is an error. The manifest's `Module` is the repository's name and `Declared` records language, commit, description,
  workloads and file. `fleet -declared dir` writes them next to the analysed services (a name already analysed is an
  error); `codegraph deploy` matches the `workloads` they name (`Deployment.Declare`, match `declared`); their `settings`
  feed the address check; `codegraph inventory -run` warns when a repository moved past the declared `commit`.
- **Store owners:** a declared table or key in another repository's database names that repository as `owner`
  (`Endpoint.Owner`, in the ID when set). `link.sharedStores` then pairs it only with that repository's services, or
  with services declaring the same owner (two declared services reading a third repository's database). Without
  owners, a declared back office's tables matched unrelated same-named tables of other services (`merchants`,
  `requests`).
- **Chains** (`link.chain`): a journey already on the path is a `cycle`. One already followed in this chain at the
  same or a shallower depth is `repeated` (`Hop.Repeated`, no `Next`), so diamonds (a→b→d, a→c→d) are followed
  once. `maxHops` = 6 still caps the depth.
- **Explorer** (`render.Explorer`, format `html`, default on): one self-contained page, with no external scripts or
  fonts on load, embedding compact JSON: services, every endpoint (`service|ID`), journeys (outbound in program order), links,
  and `model.System.Continuations` mapped to journey indexes. The page expands chains on demand with cycle marks,
  and computes reverse indexes ("reached from", callers, producers and consumers per key) in the browser. The home
  page starts with a services directory: a card per repository, each service with its journey counts per transport.
  A pill inside a link row is a `span` (`pillText`), since anchors cannot nest. Expand-all
  stops at 6 services deep and 1,500 steps. `Continuations` is `json:"-"`: `system.json` keeps the expanded chains.
  Light and dark tokens, a collapsible service list under 760px, hash routes `#/s/<svc>`, `#/j/<index>` (or
  `#/j/<svc>/<transport>/<key>`, the form `codegraph query` links to: indexes differ between runs linked apart) and
  `#/k/<transport>/<key>`, each part URL-encoded.
- **Explorer, production** (`codegraph link -deploy deploy.json`; `make deploy` redraws `system.html` with it, since
  `fleet` writes the page before `codegraph deploy` runs): `exploreData.Deploy` holds every service's `deploy.View`
  without settings, plus the mocks naming callees; the settings (values, secrets by name) ride in a third inert
  script, `codegraph-settings`, keyed `service|workload` and parsed when a workload's settings are shown (the bulk of a large run's
  deployment data). Home: services deployed, not deployed, mocks grouped per service and setting, the
  address check's counts; directory: a "not deployed" chip; service page: a chip under the title and an "In
  <environment>" section (workloads, settings on request, the address check as in `codegraph query service`); journey
  steps: "mocked in <environment>" when a mock of the caller names the callee.
- **Explorer diagrams, on demand** (`diagramPanel` in `explorer.html`): a "Show …" button per page draws a Mermaid
  diagram in place, with Redraw, Hide, Fit width, Copy source and Open as SVG.
  - Home: the system diagram; service page: its connections and its service diagram; journey page: its sequence
    diagram, 0–6 services deep (default 2), following the path's filters.
  - The system and service diagrams are `Mermaid(System(sys))` and `Mermaid(Service(m, ServiceOptions{}))`, as JSON
    text in an inert `<script id="codegraph-diagrams" type="application/json">`, parsed on first use (about 0.7 MB for 87
    services; the page is 1.9 MB).
  - Journey and connection diagrams are written in the browser (`sequenceOf`, `connectionsOf`): `sequenceOf`
    mirrors `render.Sequence` (participants, `->>+`/`-->>-` activations, `-)` for async, `externalTarget`, the
    `seqText`/`seqAlias` escapes) and stops at `MAX_SEQ_STEPS` = 200 with a note; every note goes through `seqText`
    (an unescaped `;` broke 214 capped diagrams).
  - Mermaid 11 is a dynamic `import()` of `cdn.jsdelivr.net/npm/mermaid@11/dist/mermaid.esm.min.mjs` on the first
    click (retried after a failure); offline, the panel shows the source. `securityLevel: "strict"`, the page's
    theme, `useMaxWidth: false` (natural size, scrolled in a box), `maxTextSize` 400,000 and `maxEdges` 2,000.
    A failed render's leftover `#d<id>` element is removed.
- **Unmatched endpoints** (`System.Dangling`) carry a `Reason` when they are not orphans (`link/dangling.go`):
  - `same-service`: a counterpart in the service itself (links never join a service to itself);
  - `broker`: every key is a `.retry`, `.dlq` or `.delay.<ttl>` queue;
  - `other-transport`: the same name is answered on another messaging transport. The user is moving Kafka to
    RabbitMQ, and both stay live meanwhile.

  A messaging endpoint of several keys that links through some of them also gets one `Partial` entry per reason
  for the keys nothing answers (`unlinkedKeys`, each key probed alone with `counterpart`; keys that are only `*` are
  skipped). Lookups by endpoint (journey leaves, `who`, explorer chips, `query`'s `dangling` map) skip partial
  entries; lists by key (`system.md`, the system diagram, a service's unmatched list, the orphans in `context.md`,
  the explorer and `codegraph diff`, the frontier's one-sided topics) include them.

  The system diagram skips the first two. `system.md` has a Reason column (rows de-duplicated across per-caller
  variants). The explorer shows reason chips per service, notes in journeys, and on its home page lists messaging
  endpoints without a reason or a resolved key ("nothing analysed answers").
- **Links list every matching key** (`notify.account.changed, notify.order.changed`), with the weakest
  confidence among them.
- **gRPC:** servers are keyed by service (`RegisterXServer`), constructors by service (`NewXClient`), and RPCs on an
  exported `XServiceClient` by `XService/Method` (rule `grpc-call`, `sigContains` `grpc.CallOption`). Linking matches
  on the service part. Every gRPC endpoint also gets `Contract`, the proto-qualified service read from the generated
  `<Service>_ServiceDesc.ServiceName` (or `_<Service>_serviceDesc`) in the callee's package
  (`resolve.GRPCServiceName`). When both sides have one they must be equal, so `notify.v1.NotifierService` and
  `notify.v2.NotifierService` (one repository's two mains) no longer cross-link. `Contract` enters `EndpointID` only when set. A server whose handler implements several methods gets one journey per method (key
  `XService/Method`); methods only provided by an embedded `Unimplemented*` type get none. Chains into a gRPC server
  continue only into the journey of the method called.
- **grpc-gateway** (`internal/transport/gateway.go`): rules `grpc-gateway-handle` (`ServeMux.Handle(meth, pattern, h)`,
  transform `grpc-gateway`: the last key argument is a `runtime.Pattern`, followed to the package variable the
  generated code stores `runtime.MustPattern(runtime.NewPattern(version, ops, pool, verb))` in, whose literal slices
  `resolve.SliceLiteral` reads in index order, and `decodePattern` replays: `LitPush`, `Push` `*`, `PushM` `**`,
  `ConcatN`, `Capture` `{name}` or `{name=sub/pattern}`, then `:verb`) and `grpc-gateway-handle-path`
  (`HandlePath(meth, path, h)`). A route's journey reaches `request_X_M_0`'s client call. Linking lets an outbound
  gRPC call made in generated gateway code (`link.gatewayCall`: a site in a `*.pb.gw.go` file) match its own
  service's server: the match goes into the continuations only (confidence `gateway`), never into `System.Links`, so
  the route's journey continues into the method's and nothing else changes. Any other self-call stays unmatched,
  `same-service`.

### Journeys (how they are computed)

- **Roots:** `resolve.HandlerFuncs(v, rule.HandlerMethods)` handles functions, closures, bound method values
  (resolved to the real method through `MethodSets.Lookup` + `MethodValue`, falling back to `FuncValue` for wrappers
  without syntax; `LookupMethod` panics when the method is missing, so don't use it), `ChangeType`/`Convert`,
  `MakeInterface` of a concrete type (its interface methods) or of a func type (the func), `Phi`, variadic chains (the
  last element), parameters via callers, fields and globals via the store index, module factories (their return
  values) and library middleware (arguments of the result's own type, e.g. `http.StripPrefix(p, h)`,
  `bus.WithRetry(h, …)`). `Rule.CallerIsHandler` makes the calling function the root (amqp `Consume`, NATS
  `SubscribeSync`, redis `Subscribe`, sarama `ConsumePartition`, confluent `Subscribe`). A `//codegraph:consumes` function is
  its own root.
- **Factory bindings:** a closure returned by a module factory carries `Root.Bind`, mapping its free variables to the
  handlers passed at that call. In SSA, captured variables are heap `Alloc`s: the binding is the `Alloc` (resolve
  the values stored into it), the free var is a pointer, and calls go through `*fv`. The walk then sends calls of a
  bound free variable only to those handlers. Without this, a shared middleware (`ConsumerMiddleware.Wrap`)
  made every consumer journey reach every handler.
- **Walk:** traverses module functions only. Library code is not entered, because it re-enters the module through
  interfaces. To compensate, anonymous functions defined in a reached function are reached: `http.HandlerFunc(func…)`
  then `handler.ServeHTTP` goes through net/http, which is exactly how oapi-codegen wrappers dispatch. Module
  functions or method values passed as arguments to library calls are reached too (`retry.Do(f)`, `errgroup.Go`).
  `go` marks async steps, and outbound endpoints reached only from goroutines are `background`.
- **Outbound attribution** is by call site (`Detection.Site`), or by function for directives.

## Clarifications

- **"Does it only find connections in main.go?"** No. `main` and `init` are only the *roots* of reachability; the
  detector scans every reachable function in every module package. Only the bootstrap trace is anchored at `main`.
  Journeys go further: each inbound endpoint is a root of its own.
- **Kafka and RabbitMQ are built in** (segmentio/kafka-go, sarama, confluent v1/v2, amqp091, streadway). Custom rules
  are needed for in-house wrappers: the orders sample does this for `events.Publisher`, and
  `testdata/rules/inhouse.json` for the in-house style stubs. An organisation keeps its rules in its profile.
- **Sync vs. async** is per endpoint (rule `kind`, directive `kind=`, else role-based default). NATS
  `Request`/`RequestWithContext` are sync.

**Known caveats (documented in the README):** reflection-based routing hides handlers; shared dispatch outside factory
closures over-approximates; a handler only reachable from library code must be dispatched dynamically by that
library to count as reachable; router prefixes from configuration are `*`; gRPC services without a `ServiceDesc` match
by service name only.

## Next (planned 2026-10-19; work through in order, one item at a time)

Asked by the user on 2026-10-19, after codegraph found authz's gRPC servers and tables but little else. The baseline
for items 1 to 3 is `~/go/src/github.com/zalgonoise/x/authz` (the user's repository: read-only, outputs in a scratch
directory; `codegraph analyze -dir . -entry ./cmd/...` from there). What authz showed on 2026-10-19: 15 components, 32
endpoints, 11 gRPC method journeys with their SQL tables; a bootstrap trace of 5 steps ending at `cli.Run`; no HTTP
route; settings with only gRPC and OTel library variables; ports `:*`.

1. **Done (2026-10-19): CLI commands as entry points.** See Conventions ("CLI commands"). authz's `ca` and `authz`
   commands are endpoints at their map entries' lines (`cmd/authz/main.go:34`, `:35`) with journeys rooted at
   `ExecCertificateAuthority` (128 functions, 17 outbound endpoints) and `ExecAuthz` (133, 21), and the bootstrap
   trace continues into both (config, tracing, database and migrations, keys, metrics, servers). cobra and
   zalgonoise/x/cli v1/v2 have built-in rules; urfave/cli is parked (no user found). Sample: `testdata/tool`. On a
   larger stack, every command found had a journey, and nothing else changed (`codegraph diff`: endpoints added only).
2. **Done (2026-10-19): kelseyhightower/envconfig.** See Conventions ("Settings"). authz's settings went from 18
   library variables to 35, its 17 `AUTHZ_*` among them (two spelled `…_DUR_MOTNHS` in authz's tags). Its ports stay
   `:*`: they are `fmt.Sprintf(":%d", conf.HTTPPort)`, and the resolver makes every `Sprintf` verb `*`, even with a
   constant argument (`defaultConfig()`'s 8080, merged with the environment's and the flags'). Substituting resolvable
   arguments would name them, but changes keys across every analysed service: a separate item, if wanted.
3. **Done (2026-10-19): grpc-gateway.** See Conventions ("grpc-gateway"). authz's 11 gateway routes are found with the
   paths of its `.proto` annotations (`POST /v1/login`, `POST /v1/ca/services/{service}/certs`, …), plus `GET /ready`
   and `GET /metrics` (`HandlePath`); each route's journey continues into its gRPC method's (`POST /v1/login` →
   `Authz/Login` → `services`, `challenges`), and authz's unmatched endpoints went from 16 to 5. Sample: ledger's
   gateway.
4. **Done (2026-10-19): symbols a journey touches.** See Conventions ("Library symbols used"). authz calls 172
   exported symbols of 27 other modules; its `authz` command's journey calls 111 of them (16 modules), `Authz/Login`
   16 (4 modules), each gateway route 9 to 13 (the generated client and grpc-gateway's runtime). `uses <module|symbol>`
   (MCP `uses`) lists the services and journeys calling a module's API. On a larger stack, `codegraph diff` reports no
   change, and the run grows by about 12%. Samples: billing's and tool's journeys.
5. **Done (2026-10-19): library mode.** See Conventions ("Libraries"). zalgonoise/x/cli/v2: 7 symbols, 7 journeys
   (`cli.Runner.Run` and `cli.Executor.Exec` run the exported `Runnable` and `Executable`); analysed with authz
   (`fleet -libraries`), `uses github.com/zalgonoise/x/cli/v2` names the journeys of the three symbols authz's `main`
   calls. zalgonoise/x/fts (generic throughout: CHA): 51 symbols and journeys, `fts.New` reaching the sqlite connect
   and the `fulltext_search` queries, `fts.Indexer.Search` its 5 implementations. Sample: `testdata/mailer`, called
   by tool-admin's commands. The explorer's pages and the 172 diagrams they draw were checked in the browser pane
   (Mermaid 11 parses all; services without links have no connections diagram), as were the answers' diagrams. Also
   checked on a set of in-house libraries (`fleet -libraries`): every one analysed, clones unchanged.

6. **Done (2026-10-19, asked after item 5): main journeys.** See Conventions ("Main journeys"). Sample:
   `testdata/pipeline`. zalgonoise/x/audio, which had no journey, now has its pipeline as its main journey.

7. **Done (2026-10-19, asked after item 6): `fleet -api` and the internal API.** See Conventions ("Libraries").
   zalgonoise/x with `fleet -api` over audio, authz, cli/v2 and fts (22 s): `audio-api` (299 symbols) and `authz-api`
   (281, all internal, 1,060 generated left out; 273 journeys, 13 of them HTTP routes its internal packages register)
   next to their programs. Checked in the browser pane: the toggle hides 260 of authz-api's journeys until pressed,
   then lists 273 and draws the internal diagram (338 nodes); every embedded diagram parses. Test:
   `TestFleetAPI` (orders, whose code is all internal, and mailer).

8. **Done (2026-10-26, asked after item 7): container images.** See Conventions ("Container images"). Sample: the orders
   sample's `bitbucket-pipelines.yml` (its image matched by its build in `TestCommands`); `TestImages`,
   `TestMatchBuilders`, `TestImage`. On a large organisation the inventory still takes seconds; matching a production
   chart with it corrected the repositories that name matching got wrong, and left only third-party images
   unmatched.

For each item: a sample in `testdata` and the golden test, the check on authz (or a real library for item 5),
README and this file updated, `make test`, `make samples` and `make audit` (with a profile's denylist) passing.

## Expected sample results (the golden test checks these)

**orders** (`github.com/zalgonoise/codegraph/testdata/orders`, alias `orders-svc`, tag `team=checkout`):
- Its `bitbucket-pipelines.yml` builds and pushes `team-orders` (tag `release-*`, `Dockerfile`), read by `codegraph
  inventory`; in `TestCommands`, a rendered `team-orders` container is matched to orders by that build (`built`), and
  `image` names both.
- Inbound:
    - `http server POST /orders`, handler `(*httpapi.Handler).create`, component `internal/httpapi`.
    - `http server GET /orders/{id}`, handler `(*httpapi.Handler).get`, component `internal/httpapi`.
    - `http listener :8080`.
- Outbound:
    - `http client GET http://inventory:8081/items/*` from `internal/clients/inventory`.
    - `kafka producer orders.created`, message `orders.OrderCreated`, from `internal/domain/orders` via the custom rule.
    - `sql connect postgres`.
    - `sql query orders`, 2 call sites.
- Runtime implementations:
    - `orders.Repository` → `*storage.OrderRepo`.
    - `orders.Inventory` → `*inventory.Client`, and **not** `orders.Mock`.
    - `events.Publisher` → `*events.LogPublisher`.
- Calls from `domain/orders` to storage, inventory and events are via-interface (dashed); there are no `init` edges.
- Journeys:
    - `POST /orders` reaches httpapi, domain/orders, clients/inventory, storage and events. Outbound, in program
      order: the inventory HTTP call, `sql orders`, `orders.created`.
    - `GET /orders/{id}` reaches only `sql orders`.

**inventory** (`github.com/zalgonoise/codegraph/testdata/inventory`):
- `http server GET /items/{sku}`, with handler `main.itemHandler(…)`.
- `GET /stock` (`main.logged((*main.memStore).stock)`) and `GET /health` (`main.logged(main.health)`). Each journey
  reaches only its own handler plus `main.logged$1`.
- `http listener :8081` (from `http.Server{Addr}`).
- `GET /v1/version` (`main.version`): `"GET "+cfg.BasePath+"/version"`, where `BasePath` has `envDefault:"/v1"` and
  nothing sets it in code. `http listener *` from `cfg.MetricsAddr`, whose `envDefault:":9090"` names the local
  machine and stays unknown.
- `kafka consumer orders.created`, from a directive; its journey root is `main.consumeOrders`.
- The bootstrap trace includes `signal.NotifyContext`, `go consumeOrders`, and `Shutdown` inside a goroutine.
- Settings `INVENTORY_ADDR`, `INVENTORY_BASE_PATH`, `INVENTORY_METRICS_ADDR`, `INVENTORY_REGION` and `KAFKA_BROKERS`:
  a `config` with `env` tags (one nested under
  `envPrefix:"KAFKA_"`, one `env:"-"`) passed to `parseEnv(any)` in a package initializer, and an `os.Getenv`.

**ledger** (`github.com/zalgonoise/codegraph/testdata/ledger`, analysed with `testdata/rules/inhouse.json`):
- `grpc server LedgerService` (handler `*main.ledger`), `grpc listener *`, `grpc dial notifier:9090`,
  `grpc client NotifierService`, `grpc client NotifierService/Notify` (message `*pb.NotifyRequest`), and
  `kafka producer billing.invoice.requested` (message `billing.DataInvoiceRequested`, topic from `Topic()`).
- Journeys `LedgerService/Credit` (outbound `NotifierService/Notify`, sync, and `billing.invoice.requested`, async)
  and `LedgerService/Balance`; none for `Debit`, which is only the Unimplemented stub.
- `redis query billing:invoice:*` in the `LedgerService/Credit` journey (kvcache `Get`), shared with billing.
- The server's contract is `ledger.LedgerService` (from `LedgerService_ServiceDesc`). The notifier stub has no
  ServiceDesc, so its clients have none.

**billing** (`github.com/zalgonoise/codegraph/testdata/billing`, analysed with `testdata/rules/inhouse.json`):
- `kafka consumer billing.invoice.requested` (`(*main.middleware).Wrap((*main.handler).invoiceRequested)`) and
  `billing.invoice.paid` (`…Wrap((*main.handler).invoicePaid)`). Each journey reaches only its own handler plus
  `main.Wrap$1`.
- `amqp consumer billing.reminders,billing.reminders.urgent`
  (`(*main.middleware).WrapRabbit((*main.handler).remind)`); the names are read back from `QueueConfig`s built by a
  helper. The `NewConsumer` around that mux adds no endpoint of its own (fallback, every queue is registered).
- `amqp consumer billing.refunds` (handler `*main.refundHandler`, from `NewConsumer`'s `Queues` and `Handler`); its
  journey root is `(*main.refundHandler).ProcessMessage` and it uses `sql invoices`, then
  `http client POST http://gateway:8091/backoffice/payments/*/refund` (gateway's iris admin API).
- `kafka producer billing.invoice.issued` (message `*billing.DataInvoiceIssued`, via `invoiceRequested`) and
  `billing.payment.recorded` (message `billing.DataPaymentRecorded`, via `invoicePaid`): one shared `emit(ctx, d)`
  publish site split per caller, each journey getting only its own. Plus `amqp producer billing.receipts` (a queue named `(&billing.DataReceipt{}).Topic().String()`) from the
  `invoice.paid` journey.
- `redis query billing:invoice:*` (kvcache `Set(ctx, fmt.Sprintf(InvoiceKey, id), …)`) in the `invoice.requested`
  journey.
- gorm: `sql connect mysql` (the dialector's `Name()`), `sql query invoices` with 3 sites (`Create(inv)` and two
  `Model(&invoice{})`, via `TableName()`), and `sql query receipts` (`Table("receipts")`). The `Find(&rs)` after it
  adds nothing. `invoice.requested` uses `invoices`; `invoice.paid` uses `invoices` and `receipts`.
- Analysed with `-include-modules 'example.com/inhouse/*-sdk-go'`: `grpc client LedgerService/Balance` (contract
  `ledger.LedgerService`, message `*pb.BalanceRequest`), `grpc client LedgerService` and `grpc dial ledger:9091`, all
  in component `example.com/inhouse/ledger-sdk-go`, through the `testdata/stubs/ledger-sdk` stub.
- A rabbitbus broker repository from `rabbitmq.New(ctx, &Config{PriorityQueue: "billing.audit"})`, captured by a closure
  and passed in a config struct: `amqp producer billing.audit` (`Publish(ctx, "", …)`) and
  `amqp consumer billing.audit` (`Consume(ctx, "", h.audited)`).

**gateway** (`github.com/zalgonoise/codegraph/testdata/gateway`, chi and iris stubs):
- `GET /health`, `GET /v1/balances/{account}` (`Route`), `POST /v1/credits` (`Group` inside `Route`),
  `GET /admin/stats` (`Mount` of the router `adminRouter()` returns), `GET /version` (an options struct whose
  `BaseURL` is never set, read inside a `Group` closure), and `http listener :8090`.
- Routers captured by `Group` closures and mounted after them give
  `GET /api/storefront/init`, `GET /users/{user_id}/` and, through a helper mounting its router on the one it is
  given, `POST /users/{user_id}/documents/`.
- An iris admin API: `http listener :8091` (`Listen`), `GET /ping` (`main.pong`), and
  under `Party("/backoffice")` then `Party("/payments/{payment_id:string uuid() else 400}")`:
  `GET /backoffice/payments/{payment_id}` (registered as `/`, the trailing slash dropped),
  `POST /backoffice/payments/{payment_id}/refund` (handler `(*main.adminHandler).refund`, after an `audit`
  middleware), and `GET /backoffice/files/{name...}` (`{name:path}`).

**ledger, its gateway** (`internal/pb/ledger.pb.gw.go`, a generated-style file, and the grpc-gateway stub):
`http listener :8092`; `http server POST /v1/accounts/{account}/credit` and `GET /v1/accounts/{account}/balance`
(handlers `pb.RegisterLedgerServiceHandlerClient$1`, `$2`, in `internal/pb`, paths decoded from the patterns) and
`GET /healthz` (`HandlePath`); `grpc dial localhost:9091`, `grpc client LedgerService` (main's, unmatched
`same-service`) and `LedgerService/Credit`, `/Balance` (the gateway's, in `internal/pb`). The routes' journeys use
those calls, and their system journeys continue into ledger's own `LedgerService/Credit` (with its notifier and
billing chain) and `LedgerService/Balance`.

**tool** (`github.com/zalgonoise/codegraph/testdata/tool`, cmd/tool, x/cli v2 and envconfig stubs):
- `cli command serve` (`main.serve`) and `cli command migrate` (`main.migrate`), each at its map entry's line;
  `http listener :8095` and `http server GET /status` (`main.serve$1`), registered inside `serve`; `sql connect
  postgres` (2 sites) and `sql query schema_migrations`, its table from the `default` tag of an `envconfig` field
  nothing sets in code.
- Journeys: `cli migrate` uses `sql connect postgres` and `sql query schema_migrations`; `cli serve` uses `sql connect
  postgres`; `http GET /status` uses nothing.
- Settings `DSN`, `MIGRATIONS_TABLE` (tags), `TOOL_HTTP_PORT` (nested), `TOOL_LOG_LEVEL` (`split_words`),
  `TOOL_STARTED` (a `time.Time`); `Debug` is `ignored`.
- The bootstrap trace: `cli.NewRunner`, `cli.Run`, then `migrate` (`main.load`, `sql.Open`) and `serve` (`sql.Open`,
  `http.NewServeMux`, `HandleFunc`, `http.ListenAndServe`).

**tool-admin** (the same module, cmd/admin, cobra stub): `cli command users` (`main.newUsersCmd$1`, a constructor's
closure) and `cli command purge` (`main.runPurge`, a package-level `cobra.Command`); the root `admin` has no `RunE`,
so no endpoint. `sql connect postgres` (2 sites), `sql query users` and `sql query sessions`. Both send mail with the
mailer library: `users` calls `mailer.New` and `(*mailer.Client).Send`, `purge` `mailer.NewQueued` and the interface
method `mailer.Sender.Send` (each journey's `uses`, at `cmd/admin/main.go:38` and `:54`).

**mailer** (`github.com/zalgonoise/codegraph/testdata/mailer`, `-library`): a library, no bootstrap. `go api`
endpoints `mailer.New`, `mailer.NewQueued`, `mailer.Client.Send` (handler `(*mailer.Client).Send`) and
`mailer.Sender.Send` (handler `mailer.Sender.Send → (*mailer.Client).Send, (*mailer.queued).Send`: the exported
`Client` and the unexported `queued` that `NewQueued` converts to a `Sender`), and `go internal`
`mailer/internal/tmpl.Render` (its internal API: its own journey, in the report's "Internal API" table, out of the
service diagram unless asked for); not `(*queued).Flush` nor `Version` (declared in a generated file). `http client POST http://relay:8025/send` and `sql query outbox` (not `outbox_sent`: `Flush` is never
reached). Journeys: `mailer.Client.Send` (components mailer, internal/tmpl; the HTTP call), `mailer.New` and
`mailer.NewQueued` (nothing), `mailer.Sender.Send` (both roots; the HTTP call and `outbox`), and
`mailer/internal/tmpl.Render` (nothing outbound). `model.APICalls` pairs
`purge` with `mailer.NewQueued` and `mailer.Sender.Send`, `users` with `mailer.New` and `mailer.Client.Send`. The system
lists mailer as a library with no link and no unmatched endpoint; `uses Sender.Send` names `journey mailer go
mailer.Sender.Send`, whose answer lists tool-admin's `purge`.

**pipeline** (`github.com/zalgonoise/codegraph/testdata/pipeline`): `main` opens `sql connect postgres`, registers
`GET /health` (`main.health(…)`, a factory's closure: journey rooted at `main.health$1`, using `sql query checkpoints`)
and `GET /metrics` (`http.NotFoundHandler(…)`, a library's handler: no journey), serves `http listener :9100` from a
goroutine, starts `go store(…)` (`sql query readings`) and runs `pull` (`http client GET
http://feed:8080/readings`, unmatched). Its main journey: functions `main.main`, `main.health`, `main.main$1`,
`main.pull`, `main.store` (not `main.health$1`), goroutines `main.main$1` and `main.store`, outbound in program order
`sql postgres`, `sql readings` (background), `http GET http://feed:8080/readings`.

**Main journeys of the other samples:** orders' reaches the wiring of every component and `sql connect postgres`;
ledger's its two dials and its two service-level clients (`NotifierService`, linked to notifier, and the gateway's
`LedgerService`); billing's `sql connect mysql`, the SDK's dial and its `LedgerService` client (linked to ledger);
inventory's, gateway's, tool's and tool-admin's nothing outbound (tool's and tool-admin's call `cli.NewRunner`,
`cli.Run`, `cli.WithExecutors` and cobra's `AddCommand`, `Execute`). billing's and ledger's are system journeys. mailer,
a library, has none.

**notifier** (`testdata/declared/notifier.json`, declared, Python): `grpc server NotifierService` (contract
`notifier.v1.NotifierService`, handler `NotifierServicer`), `sql query notifications` and `http client POST
http://smtp-relay/send`, component `notifier` (the positions' directory); one journey, `NotifierService/Notify`, using
both; settings `DB_HOST`, `SMTP_HOST`.

**system:** `orders → inventory` over HTTP (sync, confidence `pattern`) and Kafka `orders.created` (async, `exact`),
`ledger → billing` over Kafka `billing.invoice.requested` (async, `exact`), `ledger → notifier` over gRPC
`NotifierService` (sync, `exact`: ledger's clients have no contract, so the service name decides), and `billing → ledger` over gRPC
`LedgerService` (sync, `exact`, the SDK's call matched by contract), `billing → gateway` over HTTP
`POST /backoffice/payments/{payment_id}/refund` (sync, `pattern`: the iris route's typed parameter), plus a
`billing ↔ ledger` shared store on Redis `billing:invoice:*` (`name-only`).

- **Dangling:**
  - notifier's `POST http://smtp-relay/send` (ledger's two `NotifierService` clients now link to the declared notifier);
  - billing's `billing.invoice.issued`, `billing.payment.recorded` and `billing.receipts` producers;
  - billing's `billing.invoice.paid`, reminder and `billing.refunds` consumers;
  - billing's `billing.audit` producer and consumer, reason `same-service` (no self-link is made).
- **System journeys:**
  - `POST /orders` continues into inventory `GET /items/{sku}` (sync, `main.itemHandler(…)`) and `orders.created`
    (async, `main.consumeOrders`), with `sql orders` as an external participant.
  - `LedgerService/Credit` continues into notifier's `NotifierService/Notify` (its table and mail relay) and billing's
    `invoice.requested` consumer, which uses `sql invoices` and `billing.invoice.issued`.
  - `billing.invoice.paid` continues into ledger's `LedgerService/Balance`.
  - `billing.refunds` continues into gateway's `POST /backoffice/payments/{payment_id}/refund`.

## Working agreements

- Changes go directly in the repository, with concise explanations.
- Commit only when asked: one commit per finished item, with a `Co-Authored-By` trailer, never including `tmp/`,
  `.idea/`, `.DS_Store` or a profile.
- As each agreed item is finished, update this file for it: its decisions (conventions above) and the expected sample
  results. Propose other changes to this file (restructuring, new goals) for review first. An organisation's history
  and numbers belong in its profile's notes, not here.
- Check that a feature is used in the code it is meant for before building it; park unused ones, with the evidence.
- Each responsibility stays in its own package, wired in `cmd/codegraph/main.go`.
- Run build, vet, gofmt, `go test ./...` and the samples after each change, and keep `make cover` at 70% or more
  (85.5% on 2026-10-22; every package above 70% under that measure, `cmd/codegraph` lowest at 72%: `TestCommands`
  runs `query`, `inventory` and `deploy` end to end). Report honestly what was verified and what wasn't.
- The README is for users: what codegraph does, "How it works", then "Getting started, step by step" and "Running
  with Docker" before the reference sections. When a command or flag changes, update those first. `make docker-test`
  and `make docker-graph` (and the direct `docker run` example) were checked on 2026-10-22; check them again when the
  Dockerfile or the docker targets change.
- When changing rendering, check that the Mermaid output parses. One way: collect every mermaid block from the
  outputs into an HTML page that imports Mermaid 11 from jsdelivr and calls `mermaid.parse` on each. Serve it with a
  temporary `.claude/launch.json` (`python3 -m http.server`), open it in the browser pane, and remove the launch
  file afterwards. Labels with `*`, `{}`, `:` and `…` from real repositories, and Mermaid's 500-edge limit, are the
  usual failures.
- Keep this repository free of any organisation's names (`make audit`).
