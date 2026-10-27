# codegraph

Static architecture analysis for Go services: what each service exposes, what it calls, and how the services of a
whole system connect, read from the code itself with the compiler's own tooling (type-checked packages, SSA, call
graphs). No instrumentation, no traffic, no hand-drawn diagrams to keep up to date.

## What you can use it for

- **Understand a system you did not write.** An interactive explorer of every service, its routes, gRPC methods,
  topics, queues, tables and CLI commands, and what each of them does: the functions it runs, the services it calls
  and the messages it sends, in program order, followed across services.
- **Know what a change will affect.** Who calls a service, who consumes a topic, which journeys reach a table, how one
  service reaches another (`impact`, `who`, `path`).
- **Review architecture changes.** `codegraph diff` compares two runs: links, services and endpoints added or removed,
  topics left without a consumer. It runs in any CI through the Docker image.
- **Ask an AI assistant about your architecture.** `codegraph mcp` serves the same answers to Claude Code and Claude
  Desktop, with `file:line` evidence and diagrams.
- **Check deployments against the code.** What an environment runs of each service, mocks switched on, settings
  configured but read by nothing, links the code makes but no configured address backs, and which repository
  builds each container image.
- **See an organisation at a glance.** Every repository's languages, Go versions, in-house dependencies and their
  drift, messaging libraries and container images, read in seconds without building anything.
- **Know a library's reach.** What each exported function of a library does, and which services and journeys call it.
- **Share it.** `codegraph bundle` packs a run into one small file (a 90-service run is about 1 MB) that anyone's
  `codegraph query` or MCP server reads directly: one machine analyses, everyone asks.

## What it answers

For each `main` package (a service), and then across services:

1. **What does `main` do?** The ordered bootstrap: constructors, registrations, servers started, goroutines, deferred
   shutdowns, followed into the module's code and into each CLI command's function (cobra, zalgonoise/x/cli).
2. **What is in use at runtime?** Only what is reachable from `main` counts, so dead code, mocks and test helpers
   never show up. Components (packages) are grouped by layer with their **contracts** (declared interfaces), the
   **implementations** in use at runtime, the calls between components, and the dependency-injection **bindings**.
3. **What does it consume and produce?** HTTP routes and clients, gRPC servers and calls, Kafka, NATS, RabbitMQ (AMQP)
   and Redis topics, queues and keys, SQL tables, CLI commands, with message types and handlers where they can be
   resolved.
4. **What does each entry point do?** Every inbound endpoint (an HTTP route, a gRPC method, a consumed topic, a CLI
   command) starts a **journey**: the functions and components its handler reaches, the goroutines it starts, the
   exported API of other modules it calls, and the outbound endpoints (other services, brokers, stores) it uses, in
   program order. `main` has one too, for the work no endpoint starts, such as a pipeline or a job.

A module without a `main`, a **library**, can be analysed through its exported API instead (see
[Libraries](#libraries)): each exported function and method starts a journey.

Each service produces a `manifest.json`; `codegraph link` matches the endpoints of every manifest (a client to a
server, a producer to a consumer, the same table or key) into a system view, and chains journeys across services: an
outbound call continues with the journey of the endpoint it reaches.

## How it works

1. **Load.** `go/packages` loads the repository's packages with full syntax and types, and every dependency
   (`internal/loader`). The code must build: codegraph sees what the compiler sees.
2. **Build SSA and a call graph.** Every package becomes SSA (generics instantiated), and a call graph is built from
   each `main` (`init` and `main`) with RTA, then VTA for precise dynamic calls (`-algo`; CHA for comparison). Only
   what is reachable from `main` counts from here on: dead code, mocks and test helpers never show up.
3. **Detect endpoints.** Declarative transport rules match call sites in reachable module code: routes, gRPC servers and
   calls, topics and queues, tables, Redis keys, CLI commands. A resolver recovers their keys (routes, topics, table
   names) from constants, configuration fields, helpers' callers and router prefixes; what it cannot resolve is `*`.
4. **Follow journeys.** Every inbound endpoint is a root: a walk through module code records what its handler reaches,
   in program order (components, goroutines, outbound endpoints, the exported API of other modules it calls).
5. **Write a manifest per service**, plus its diagram and report.
6. **Link the services.** `codegraph link` matches outbound endpoints to other services' inbound ones (a client to a
   server, a producer to a consumer, the same table or key), and chains journeys across services into a system
   view, an interactive explorer, and files for AI agents.

Around this core: `fleet` runs the analysis over many repositories, `inventory` reads a whole organisation cheaply,
`deploy` reads what an environment runs from its Helm chart, `diff` compares two runs, and `query` and `mcp` answer
questions about a run.

## Getting started, step by step

**1. Install the prerequisites.**
- **Go**, as recent as the code you analyse needs (codegraph itself needs Go 1.27): codegraph type-checks that code
  with your toolchain.
- **git** and **make**.
- **Optional:** Docker, to run everything in a container instead (see [Running with Docker](#running-with-docker));
  helm, for the deployment step; `pkg-config` and the C libraries of repositories that use cgo; Graphviz, to render
  the `.dot` diagrams.

**2. Get codegraph and check it works.**

```sh
git clone https://github.com/zalgonoise/codegraph && cd codegraph
make test     # gofmt, go vet, the unit tests and the golden test over the bundled samples
make cover    # the same tests' statement coverage (fails under COVER_MIN, 70%)
```

`go install github.com/zalgonoise/codegraph/cmd/codegraph@latest` installs the binary alone, and the GitHub releases
have binaries for macOS and Linux (amd64 and arm64). Either way Go must be installed: codegraph loads the analysed
code through the `go` command. The rest of this guide runs
from a clone, with `go run ./cmd/codegraph` or the Makefile; with the binary installed, `codegraph` does the same.

**3. Try the samples.** `testdata/` holds small services (an HTTP API with a Kafka producer, a gRPC service, consumers
of Kafka and RabbitMQ, a chi and iris gateway, CLI tools, a pipeline) and a library, all offline.

```sh
make samples                         # analyse them into tmp/samples and link them
open tmp/samples/_system/system.html  # the explorer (xdg-open on Linux)
go run ./cmd/codegraph query -run tmp/samples services
go run ./cmd/codegraph query -run tmp/samples journey orders "POST /orders"
```

In the explorer, open a service, then a journey: it expands across services step by step. [Outputs and the
explorer](#outputs-and-the-explorer) says what each file and page shows.

**4. Analyse your first repository.** Point `fleet` at the repository's root (the directory with its `go.mod`). It
finds the main packages, analyses each, and links them; keep the output outside the repository:

```sh
GOFLAGS=-mod=readonly go run ./cmd/codegraph fleet -out tmp/first /path/to/repo
open tmp/first/_system/system.html
go run ./cmd/codegraph query -run tmp/first services
```

- `GOFLAGS=-mod=readonly` makes sure Go never rewrites the repository's `go.mod` or `go.sum`.
- Its dependencies must be downloadable, or already in your module cache. For private modules, add
  `GOPRIVATE=<module prefix>` (fetched over git with your credentials).
- `tmp/first/<service>/report.md` describes each main package: its endpoints, components, settings and journeys.
- No journeys? A service whose work no endpoint starts (a pipeline, a job) still has a `main` journey
  (`journey <service> main`). An endpoint whose handler is a library's (a metrics handler) has no journey of its own.

**5. Analyse several repositories together.** Links between services only appear when both sides are in the same run:

```sh
GOFLAGS=-mod=readonly go run ./cmd/codegraph fleet -out tmp/graph /path/to/repo-a /path/to/repo-b ~/src/*.git
```

or with the Makefile, over a directory of clones named `<repo>.git` (every one with a `go.mod`), or a list:

```sh
make graph REPOS_DIR=~/src                                     # every ~/src/*.git
make graph REPOS="/path/to/repo-a /path/to/repo-b" OUT=tmp/graph  # these repositories
```

`make graph` also checks that no repository's `git status` changed, and from the second run on writes `diff.md`, what
changed since the previous run.

**6. Teach it your in-house code** (only if you have some):
- **Your own messaging or cache libraries:** describe their APIs in a rules file (`-rules rules.json`, `RULES=` for
  make); see [In-house libraries](#in-house-libraries). Without rules, calls through them are invisible.
- **Client libraries (SDKs) that make the actual gRPC calls:** `-include-modules 'example.com/*-sdk-go'`
  (`INCLUDE_MODULES=` for make), so their calls count as the service's own.
- **Repositories without a `main`:** `-libraries` analyses them through their exported API; `-api` also analyses the
  API of the programs (see [Libraries](#libraries)).
- **Services in other languages:** describe them by hand (see [Services described by hand](#services-described-by-hand)).
- **For a whole organisation**, keep all of this in a [profile](#profiles) and run `make graph PROFILE=<dir>`.

**7. Ask questions.** `codegraph query` (see [Asking questions](#asking-questions-people-and-ai-agents)) answers in
Markdown or JSON, with `file:line` positions; `codegraph mcp` serves the same answers to Claude Code and Claude Desktop
(see [MCP server](#mcp-server-claude-code-claude-desktop)).

**8. Keep it current.** Pull the repositories and run `make graph` again: the previous run moves to `<OUT>.prev`, and
`<OUT>/diff.md` lists the links, services and endpoints that changed (see [Reviewing changes](#reviewing-changes)).

## Running with Docker

The `Dockerfile` holds the whole toolchain: Go, `pkg-config` and libxml2 for cgo repositories, python3 for a knowledge
base check, helm for the deployment step (from get.helm.sh, checksum verified), git and make. Only Docker is needed on
the host, and the Makefile's `docker-*` targets run the usual targets in the image, so any CI runner with Docker can
run them, whatever the git host.

**1. Build the image** (tag `IMAGE`, `codegraph` by default):

```sh
make docker-image
```

**2. Run the tests in it:**

```sh
make docker-test
```

**3. Analyse repositories in it.** `docker-graph` is `make graph` in the image, with the same variables:

```sh
make docker-graph REPOS_DIR=~/src OUT=tmp/graph
make docker-graph REPOS="$HOME/src/repo-a.git $HOME/src/repo-b.git" REPOS_DIR=~/src OUT=tmp/graph
```

Then open `tmp/graph/_system/system.html` on the host, or query it there. What the container sees:
- **Repositories:** `REPOS_DIR`, mounted read-only (`REPOS` must be under it); the environment's chart too, when it is
  under it.
- **Outputs:** `OUT`'s parent directory, mounted read-write, so the run lands on the host and the previous one can move
  aside.
- **Go module cache:** the host's (`GOMODCACHE`), so modules already downloaded need neither network nor credentials;
  the build cache is a named volume (`codegraph-gocache`).
- **Private modules not cached yet:** a `~/.netrc` (`NETRC=`) or a `GOPROXY` is passed in, with `GOPRIVATE`. A netrc
  works for Go and git with any host: `machine github.com login x-access-token password <token>`.
- **Profile:** mounted read-only when `PROFILE` is set; a knowledge base file (`KB`) likewise.

The image also runs codegraph directly, for one repository without the Makefile:

```sh
docker run --rm -v /path/to/repo:/repo:ro -v "$PWD/out":/out -v "$(go env GOMODCACHE)":/go/pkg/mod \
  -e GOFLAGS=-mod=readonly codegraph codegraph fleet -out /out /repo
```

**In CI:** populate a module cache (or provide a netrc), then run `make docker-test` and `make docker-graph`. Publish
`tmp/graph/_system/system.html` and `tmp/graph/diff.md` as build artifacts, or post `diff.md` on the pull request.

## Commands at a glance

```sh
make graph      # analyse every Go clone of REPOS_DIR (../*.git), link them, compare with the previous run
make graph REPOS_DIR=~/src OUT=/tmp/graph   # other repositories or output
make inventory  # read every repository of REPOS_DIR (seconds), and with FOCUS find the frontier around it
make org        # the first time with a FOCUS: graph the focus, find its frontier from that run, graph both
make deploy     # what an environment runs (DEPLOY_CHART; helm template, offline), matched to the last run
make open       # open the explorer of the last run (tmp/graph/_system/system.html)
make diff       # what changed since the previous run (make graph also does it; report in tmp/graph/diff.md)
make query Q='services'   # ask the last run (also: service, journey, who, path, impact, search)
make samples    # analyse the bundled samples
make test       # gofmt, go vet and the tests;  make cover: their coverage (minimum COVER_MIN, 70%)
make mcp-build  # build tmp/bin/codegraph for MCP clients;  make mcp-desktop: Claude Desktop's entry too
make audit      # with a profile's DENYLIST: check that no file here names the organisation
make docker-test  # the tests in the toolchain image;  make docker-graph: make graph in it
make bundle     # pack the last run into one file to share (tmp/graph.tar.gz; BUNDLE_FLAGS=-no-settings -no-positions)
make help       # every target
```

Every target and variable: `make help`, and the top of the `Makefile`. `make graph` runs with `GOFLAGS=-mod=readonly`.
With `FOCUS` (a file naming repositories, one per line), it analyses those and the frontier `make inventory` last
wrote (`tmp/inventory/frontier.txt`, see [Inventory and frontier](#inventory-and-frontier)) instead of every clone;
the frontier is found best from a run of the focus, hence `make org` the first time.

## Many repositories: `codegraph fleet`

`codegraph fleet` takes repository directories (globs are expanded, directories without a `go.mod` are
skipped) and analyses each with the same flags as `analyze`. Without `-entry`, it reads package clauses
first: `./cmd/...` when `cmd/` holds a `main` package, `./...` when the mains live elsewhere (a `main.go`
at the root), and a repository without any `main` is a library, skipped before loading and reported as
such, or, with `-libraries` (`LIBRARIES=1` for `make graph`), analysed through its exported API (see
[Libraries](#libraries)). `-api` (`API=1`) analyses every repository's API: those without a `main` as libraries, and
those with mains also as a library named `<name>-api`, next to their programs. Example and demo directories don't count as mains. Main packages matching `-skip-mains`
(default: migration, rollback and seed tools, and anything under `examples/` or `demo/`) are left out, a repository that fails to load is reported and skipped, and
the manifests are linked into `<out>/_system`. Rules for one repository only (its own dispatch
code) come from its `codegraph-rules.json`, if it has one, and from `-repo-rules <dir>`, which holds
`<module name>.json` files kept outside the repositories (`rules/repos/orders.json` for
`example.com/orders/v2`); they take precedence over the shared rules. Loading needs every dependency in the module cache;
run with `GOFLAGS=-mod=readonly` so the repositories are never modified, and with
`GOPRIVATE=<your module prefix>` so private modules are fetched directly over git (SSH). A repository
with cgo dependencies needs their C toolchain: `lestrrat-go/libxml2`, for instance, needs `pkg-config`
(on macOS, `brew install pkgconf`; Homebrew provides `libxml-2.0.pc` for the SDK's libxml2).

## Outputs and the explorer

Outputs per service: `manifest.json`, `service.mmd` (Mermaid), `service.dot` (Graphviz),
`report.md` (diagram, tables and one flowchart per journey). The manifest's `settings` lists the environment variables
the main reads: the `env` tags of the configuration types its reachable code passes to an interface (`env.Parse(&cfg)`,
`env.ParseAs[T]()`; caarlos0/env's `envPrefix` composed through nested structs), its client libraries' and
dependencies' included; the fields of the spec passed to kelseyhightower/envconfig's `Process(prefix, &spec)` (an
`envconfig` tag names a field, otherwise the prefix and the field name, `split_words` applied, with nested structs'
names as prefixes, `ignored:"true"` skipped); and the constant names given to `os.Getenv`/`os.LookupEnv` outside the
standard library. Names computed at run time (viper's AutomaticEnv, a prefix passed as a Parse option) are not seen. The service diagram draws at most 450
edges (Mermaid's default limit is 500): for very large services it keeps every endpoint and the
heaviest component calls, and says how many calls it left out (all are in the report's tables). The link step writes
`system.{json,mmd,dot,md}`; `system.md` has one sequence diagram per journey that crosses services, or for a journey
too large to draw (over 200 steps, or past Mermaid's text limit), a summary with its first steps.

`system.html` (format `html`, on by default) is an interactive explorer of the whole system: one self-contained page
that opens from disk, and loads nothing else until a diagram is asked for.
- **Services:** the home page lists every repository's services with the journeys each offers, per transport. A
  service page shows its upstream and downstream services, its journeys, and the endpoints nothing analysed
  answers.
- **Journeys:** each expands across services on demand, step by step in program order. A step shows sync or async, the
  link's confidence and stores, marks a journey already on the path as a cycle, and lists the journeys it is reached
  from.
- **Topics, queues, tables and routes:** who produces or calls them, who consumes or serves them, and through which
  journeys.
- **Search** covers all of these; views have bookmarkable URLs.
- **Diagrams on demand:** a "Show …" button draws a Mermaid diagram in place. The home page has the system
  diagram, and a service page its connections (the services linking in and out, per transport) and its service
  diagram (components, calls, endpoints: `service.mmd`). A journey page has its sequence diagram across services,
  as deep as you choose (0 to 6 services beyond its own) and with the path's store and endpoint filters, cut at 200
  steps like `system.md`'s. Each can be redrawn, fitted to the width, copied as Mermaid source or opened alone as
  SVG. Mermaid 11 is imported from cdn.jsdelivr.net on the first click only; offline, the panel shows the Mermaid
  source instead (it renders on GitHub, GitLab or mermaid.live).
- **Production** (with `codegraph link -deploy <run>/deploy/deploy.json`, which `make deploy` and `make graph` run after
  `codegraph deploy`): the home page says how many services run there, lists those not deployed and the mocks switched on,
  and counts the address check; the services directory marks the services not deployed. A service page lists its
  workloads (kind, replicas or schedule, command, Kubernetes hosts, mocks), each workload's settings on request
  (secrets by name only; they ride in the page as text, parsed only when shown), and its links checked against the
  configured addresses. A journey step whose link a mock switches off in production is marked.

The page holds only the continuations of each endpoint (`model.System.Continuations`) and expands chains in the
browser, so it stays small where `system.json` carries every chain expanded (a few MB against tens of MB for a
large run). The system and service diagrams ride along as Mermaid text in an inert script block, parsed only when one
is drawn; journey and connection diagrams are written in the browser from the page's data.
Render DOT with `dot -Tsvg service.dot -o service.svg`; Mermaid renders natively on GitHub/GitLab.

For the samples you should see: `orders` exposes `POST /orders` and `GET /orders/{id}`, calls
`GET http://inventory:8081/items/*`, produces `kafka orders.created` (message
`orders.OrderCreated`, via the custom rule) and queries table `orders`; `orders.Inventory` is
implemented by `*inventory.Client` at runtime while `orders.Mock` is not listed. The journey of
`POST /orders` reaches httpapi, domain/orders, clients/inventory, storage and events and uses the
inventory HTTP call, `sql orders` and `orders.created`, in that order; `GET /orders/{id}` only
uses `sql orders`. `ledger` gets one journey per implemented gRPC method (`LedgerService/Credit`,
`LedgerService/Balance`) and asks `billing` for an invoice over Kafka (kafkabus); `billing` consumes kafkabus
topics and rabbitbus queues through shared middleware, each journey reaching only its own handler, plus a
rabbitbus queue whose handler is set on the consumer itself (`billing.refunds`). The
system diagram links `orders → inventory` over HTTP (sync) and Kafka (async, from a directive) and
`ledger → billing` over Kafka; the system journey of `POST /orders` continues into inventory's
`GET /items/{sku}` and `consumeOrders`.

## Layout

The tool is its own module (`github.com/zalgonoise/codegraph`), so `golang.org/x/tools` stays out of
the analysed services' dependencies. Every responsibility is a separate package, wired in
`cmd/codegraph/main.go`:

| Package | Responsibility |
|---|---|
| `internal/config` | CLI flags for every command: `analyze`, `link`, `fleet`, `diff`, `query`, `mcp`, `inventory`, `deploy` |
| `internal/loader` | `go/packages` load with full syntax/types; module vs. third-party vs. std bookkeeping |
| `internal/program` | SSA construction (`ssautil.AllPackages`, generics instantiated), main packages |
| `internal/reach` | Call graph + reachability from `init`/`main` (or any roots): `cha`, `rta`, `vta` (default, seeded by RTA) |
| `internal/library` | A library's exported API as entry points: the symbols, their roots and implementations, their endpoints (`-library`) |
| `internal/resolve` | Value recovery: constants, concatenation, `fmt.Sprintf`, parameters via callers, struct fields/globals via a store index, calls followed with their arguments bound (one level of call-site sensitivity), fields of specific struct values and slice elements, `append`, method returns of dynamic types (`MethodStrings`), message types; handler values → the functions they run (`HandlerFuncs`) |
| `internal/transport` | Declarative rules + detector over call sites of reachable module code |
| `internal/journeys` | Per-entrypoint journeys: what each inbound endpoint's handler reaches, and main's own work |
| `internal/entrypoint` | Bootstrap trace of `main` |
| `internal/components` | Components, layers, interfaces + runtime implementations, types in use, component calls, bindings, libraries |
| `internal/directives` | `//codegraph:` comments for what analysis cannot see |
| `internal/manifest` | Assembles and (de)serialises the manifest; merges endpoints, assigns IDs and tags |
| `internal/render` | Format-neutral graph → Mermaid, DOT, Markdown; the interactive system explorer (`system.html`) |
| `internal/link` | Cross-service matching into a system graph; journeys chained across services |
| `internal/diff` | Compares two runs: links, services, endpoint signatures, unanswered endpoints (`codegraph diff`) |
| `internal/bundle` | Packs a run into one gzip or zstd file and reads runs back, from a directory or a bundle (`codegraph bundle`) |
| `internal/query` | Answers questions about a run (`codegraph query`: services, journeys, keys, paths, impact, library API use, container images), and writes `AGENTS.md` and `context.md` for AI agents |
| `internal/mcpserver` | The same answers over MCP (`codegraph mcp`) |
| `internal/inventory` | Reads every repository of the organisation without building it (languages, Go modules, the container images each builds and uses), and finds the frontier around a focus (`codegraph inventory`) |
| `internal/deploy` | Reads what an environment runs from its rendered chart, matches it to repositories (by the images they build, else by name) and services, and checks the links against its configured addresses (`codegraph deploy`) |
| `internal/settings` | The environment variables a main reads (`env` tags of parsed configuration types, `os.Getenv`), for the manifest |
| `internal/declared` | Services described by hand (`declared/<service>.json`), turned into manifests for the run |
| `internal/profile` | An organisation's profile (`profile.json`): the naming conventions the inventory relies on |
| `internal/model`, `internal/symbols` | Shared data model and naming helpers |

## Flags (`codegraph analyze`)

`-dir` repository root · `-entry` package pattern(s), repeatable (default `./...`) · `-algo`
`cha|rta|vta` · `-out` · `-format json,mermaid,dot,md` · `-service` / `-alias` / `-tag k=v` ·
`-rules file.json` (repeatable) · `-no-default-rules` · `-entry-depth 3` · `-resolve-depth 4` ·
`-journeys` (default on) · `-journey-depth 0` (max call depth from a handler, 0 = unlimited) ·
`-main-journey` (default on: a journey of each main's own work, see [Journeys](#journeys)) ·
`-include-modules pattern` (repeatable; modules analysed like the service's own code, e.g.
`example.com/*-sdk-go`: SDKs that make the actual gRPC calls, which would otherwise be invisible; their packages
are components of layer `adapters`. List client libraries not named like an SDK the same way, in the Makefile's
`INCLUDE_MODULES`) ·
`-libs` (draw third-party modules) · `-std` (count stdlib as libraries) · `-build-tags` · `-tests` ·
`-library` (analyse the module through its exported API instead of its main packages; see [Libraries](#libraries)) ·
`-generated` (with `-library`: include the symbols of generated files) · `-internal` (with `-library`, default on:
include the exported symbols of internal packages, as role `internal`).

Services are named after their repository (the module path, without a `/vN` suffix):
`<module>/cmd` is `<module>`, `<module>/cmd/<role>` is `<module>-<role>` (`orders-grpc`,
`orders-subscribers`, `notifier-service-grpc-v1`), and other main packages keep their directory
name. `-service` (single main package) or `//codegraph:service` override it.

`-algo rta` is noticeably faster on large programs; `cha` is mainly for comparison.

## Transport rules

Built-in rules cover `net/http` (server, `ServeMux`, `Server.Addr`, clients, `NewRequest*`), chi,
gin, echo, gorilla/mux, iris v12, gRPC generated code (`RegisterXServer`, `NewXClient`, RPCs on an
`XServiceClient` keyed `XService/Method`, `grpc.NewClient/Dial`), segmentio/kafka-go, sarama,
confluent-kafka-go, nats.go, amqp091/streadway, `database/sql`, pgx, gorm (tables from
`Table(name)`, `Raw`/`Exec` SQL or the model's `TableName()`; the connection from the dialector's
`Name()`), go-redis (publish/subscribe, and key operations such as `Get`/`Set`/`HSet`/`Del` keyed by
key pattern, e.g. `customers:profile:*`), grpc-gateway (the routes the generated code registers on a
`runtime.ServeMux`, their paths decoded from the generated patterns' op codes, and `HandlePath`), and CLI commands: cobra (the subcommands given to `AddCommand` and the root
run by `Execute`, named by the first word of `Use`; a command without `Run` or `RunE` only groups others and is left
out) and zalgonoise/x/cli (v2's map of command names to executors, v1's `WithFlag`). A command is an inbound endpoint
(`cli command <name>`) whose journey starts at the function it runs. Add your own (they take precedence) for in-house
wrappers:

```json
[{
  "name": "events-publisher", "transport": "kafka", "role": "producer", "direction": "outbound",
  "pkgs": ["github.com/zalgonoise/codegraph/testdata/orders/internal/events"], "recv": "Publisher", "funcs": ["Publish"],
  "keyArgs": [2], "msgArgs": [3]
}]
```

Argument indexes include the receiver at 0 for methods (also for interface calls). Other fields:
`recv` (`""` package function, `"*"` any, `"DB|Tx"` alternatives), `recvPattern` (regexp on the
receiver type name), `pkgs` (exact or `path/...`), `funcPattern` (regexp; group 1 becomes the
key), `sigContains`, `keyFields` (`[{"arg":0,"field":"Topic"}]`; a dotted field goes through
nested fields and slice elements, `"Queues.PriorityQueue"`), `keyMethods`
(`[{"arg":2,"method":"Topic"}]`: the key is what that method returns on the argument's dynamic
type, e.g. an in-house message's topic), `staticKey`, `methodFromFunc` (`r.Get("/x")` → `GET /x`),
`msgArgs`, `keyDefault` (`{"funcs":["New"],"arg":1,"field":"PriorityQueue"}`: when the key
argument is `""`, the field of the configuration the receiver was constructed with), `prefixed`
(compose the router prefix of the receiver, see below), `transform`
(`sql-table`; `table-name`, a table expression without its alias, `t AS x` → `t`;
`grpc-method` for `Service/Method` keys; `iris-path`, see below; `first-word`, cobra's
`Use: "serve [flags]"` → `serve`; or `grpc-gateway`, the last key argument a grpc-gateway `runtime.Pattern`),
`skipUnknown`
(emit nothing when no key resolves, for chained builders such as gorm's `Where(…).Find(&rows)` after
a `Table`) and:

- `kind`: `sync` or `async`. Without it, producers and consumers are async and request/response
  roles (client, server, query, …) are sync. NATS `Request`/`RequestWithContext` are `sync`.
- `handlerArgs`: the argument(s) holding the handler of an inbound endpoint; it becomes the root of
  the endpoint's journey. `handlerMethods` limits which methods of an interface-typed handler count
  (sarama: `ConsumeClaim`; default all exported methods, one journey per method for gRPC).
- `handlerFields`: like `keyFields`, for a handler held in a config argument
  (`rabbitmq.NewConsumer(ConsumerConfig{Handler: h})`).
- `fallback`: the rule describes a consumer's whole subscription (its queue list). Keys that another
  inbound endpoint of the same transport registers more precisely, such as a mux's per-queue
  handler, are dropped, and so is the endpoint when none remain.
- `callerIsHandler`: for APIs that return a channel or subscription the caller loops over (amqp
  `Consume`, NATS `SubscribeSync`), the function making the call is the journey root.
- `mapEntries` (`{"arg": 0}`): one endpoint per key/value pair of a map argument, the key its key and
  the value its handler (`WithExecutors(map[string]Executor{"serve": serve})`).
- `elements` (`{"arg": 1}`, optionally `"field"`): one endpoint per element of a slice or variadic
  argument, with the `keyFields` and `handlerFields` naming that argument read on each element
  (cobra's `AddCommand(cmds ...*Command)`, each with its `Use` and `RunE`).
- `requireHandler`: drop an inbound endpoint whose handler resolves to no function.

Rules can target interfaces: an invoke on `events.Publisher` matches `recv: "Publisher"` directly,
and concrete callees found by the call graph are tried too.

### Router prefixes

Routes registered on a scoped router get the scope's path: chi `Route`, `Group` and `Mount` (also
of a router a constructor returns), gin and echo `Group`, gorilla `PathPrefix(…).Subrouter()`, iris
`Party`. Rules
with `scope` declare the scoping calls and produce no endpoint: `returns` (the call returns the
scoped router; path in `keyArgs[0]`), `keeps` (same prefix as the receiver), `passes` (the scoped
router is passed to the func in `scopeArg`) and `mounts` (the router in `scopeArg` is mounted under
the path). Route rules with `prefixed` walk their receiver back through those calls, parameters,
fields, returns and closures, including a router captured by a closure and mounted after it
(`r.Group(func(chi.Router) { sub.Get("/x", h) }); r.Mount("/api", sub)` is `GET /api/x`). A route
reached through several mounts has a key per mount. A prefix from configuration shows as `*` (`*/cancel`), and an unset
option such as oapi-codegen's `ChiServerOptions.BaseURL` counts as `""`. iris routes are written the
way iris matches them (transform `iris-path`): a typed parameter loses its macros
(`{id:string uuid() else 400}` is `{id}`), a `path` parameter is a catch-all (`{file...}`), and a
trailing slash is dropped.

### In-house libraries

Organisations often wrap Kafka, RabbitMQ or Redis in libraries of their own; rules describe their APIs, kept in the
organisation's [profile](#profiles) and passed with `-rules`. `testdata/rules/inhouse.json` is an example, for the
samples' in-house style libraries (`testdata/stubs/kafkabus`, `rabbitbus`, `kvcache`):

| Rule | Call | Endpoint |
|---|---|---|
| `kafkabus-handle` | `Subscriber.Handle(topic, handler)` | Kafka consumer; the handler is the journey root |
| `kafkabus-publish` | `Publisher` / `AsyncPublisher.Publish(ctx, data)` | Kafka producer keyed by `data.Topic()` (`keyMethods`), message type from `data` |
| `kvcache-key` | key operations (`Get`, `Set`, `Del`, `SetNX`, …) on an embedded cache repository | Redis query keyed by key pattern |
| `rabbitbus-mux-handle` | `types.ConsumeMux.Handle` / `HandleFunc(queue, handler)` | RabbitMQ (`amqp`) consumer per queue |
| `rabbitbus-new-consumer` | `rabbitmq.NewConsumer(ConsumerConfig{Handler, Queues})` | RabbitMQ consumer of `Queues[].PriorityQueue` with `Handler` as root, for the queues no mux registers (`fallback`) |
| `rabbitbus-broker-consume` | `ports.BrokerRepository` / `rabbitmq.RabbitMQ.Consume(ctx, queue, handler)` | RabbitMQ consumer; `""` is the queue given to `rabbitmq.New(ctx, &Config{PriorityQueue})` (`keyDefault`) |
| `rabbitbus-publish` | `ports.Publisher` / `BrokerRepository.Publish(ctx, queue, …)` | RabbitMQ producer keyed by queue (`""` as above) |

A service that dispatches queues with its own router instead of a library's mux needs a rule for that router, kept
per repository (`-repo-rules <dir>`, a `<module name>.json` per repository):

```json
[{"name": "my-router", "transport": "amqp", "role": "consumer", "direction": "inbound",
  "pkgs": ["example.com/myservice/internal/handlers/subscriber"], "recv": "Router", "funcs": ["Handle"],
  "keyArgs": [2], "handlerArgs": [1], "handlerMethods": ["ProcessMessage"]}]
```

## Directives

```go
//codegraph:service orders                        // name the service (in the main package)
//codegraph:alias orders-svc orders.internal      // host names callers use (HTTP linking)
//codegraph:tag team=checkout tier=1
//codegraph:produces kafka orders.created msg=orders.OrderCreated
//codegraph:consumes nats inventory.reserved
//codegraph:produces nats stock.check kind=sync   // request/reply
```

Directives inside a function or its doc comment only count when that function is reachable, so a
directive on dead code never shows up. A `consumes` directive on a function makes that function
the root of the endpoint's journey; a `produces` directive in a function counts as an outbound
endpoint of every journey reaching it.

## Journeys

For every inbound endpoint with a resolvable handler, `codegraph` walks the call graph from the handler
(`resolve.HandlerFuncs`: functions, closures, method values, `http.HandlerFunc(f)`, concrete
types behind interfaces, handler factories, variadic chains, parameters, fields). The walk only
enters module code (going through libraries would re-enter the module through interfaces:
net/http leads back to every handler), but treats anonymous functions defined along the way and
module functions handed to library calls (`retry.Do(f)`, `errgroup.Go`) as reached. A closure
returned by a module handler factory (`middleware(next)`, `mw.Wrap(h)`) remembers which handler was
wrapped at each registration, so wrapped routes and consumers stay separate, and the endpoint is
named after the wrapped handler (`Wrap((*compliancehdl.Handler).MarkMandateAsRejected)`). The
journey records that handler next to its middleware root (`wrapped`), and answers cite its position.

**Main journeys.** Every main package also gets a journey of its own work: an inbound endpoint of transport `main`,
role `run` and key `main` (`journey <service> main`), rooted at `main`. Its walk does not enter the roots of the
service's other journeys, so the handlers `main` registers keep their own journeys, and what remains is what no
endpoint starts. For a pipeline or a job (a program that pulls from a feed and stores what it reads, a cron), that is
all of its work, with its goroutines and their outbound calls marked `background`. For a server, it is what it
does at startup: configuration, connections, dials, listeners. A main whose own calls reach another service (a
client built in `main`) is a system journey too. The endpoint is never linked, and `who` and the explorer's key
pages leave it out. `-main-journey=false` turns it off.

Each journey records its roots, reached functions and components, the component edges used (dashed
through an interface), the goroutines it starts, the exported functions and methods of other modules its code calls
(`uses`: module, symbol, first call site and number of sites; the service's whole list is in its `libraries`), and its
outbound endpoints in program order
(`background` when only reached from a goroutine). An outbound endpoint inside a shared helper whose
key comes from the helper's parameters (`publish(ctx, d)` called with different messages) is
resolved once per caller and split into one endpoint per distinct key (each site records the caller
in `via`); a journey only gets the endpoints of the callers it went through. `report.md` renders one
flowchart per journey; `codegraph link` follows outbound endpoints into other services' journeys (gRPC
calls into the called method's journey) with cycle and depth guards, and renders each as a sequence
diagram, with stores and unmatched endpoints as their own participants. Within one chain a journey is followed once:
a second route to it (a→b→d and a→c→d) is marked `repeated` rather than listed again, unless it arrives at a
shallower depth and can be followed further. Without that, chains grow exponentially with depth.

## Libraries

`codegraph analyze -library -dir <module>` (and `fleet -libraries` for the repositories without a `main`) analyses a
library through its exported API, the way a service is analysed through its entry points:

- **The API:** every exported function, every exported method declared on an exported type, and every method of
  an exported interface, in the module's packages (not under `testdata/`, `examples/`, `demo/` or `cmd/`; never a
  `main` package). Those of `internal/` packages, which only the module itself can call, are its **internal API**:
  endpoints of role `internal` instead of `api`, analysed the same way but left out of the service diagram, the
  report's journeys and the explorer's lists unless asked for (the explorer's "Internal API" toggle, off by default,
  remembered per browser; the query lists them apart). `-internal=false` leaves them out altogether. Each is an inbound endpoint of transport `go`, role `api`, keyed by its package
  (named after the module, then its path: `fts`, `fts/log`) and member: `fts.New`, `fts.Index.Search`,
  `fts.Indexer.Search`. Promoted methods appear under the type that declares them, and type aliases add nothing.
  Symbols declared in generated files (`// Code generated … DO NOT EDIT.`) are left out unless `-generated` is given:
  a contracts module's protobuf messages alone have thousands of methods (`Reset`, `String`, getters).
- **Journeys:** one per symbol, rooted at its function, with what it reaches in the library, the endpoints it uses
  (queries, HTTP calls, topics: everything a service's journey shows) and the exported symbols of other modules it
  calls. An interface method's journey runs every implementation in the library: its exported types implementing the
  interface (a caller can pass any of them) and the unexported ones the API converts to an interface (a constructor
  returning `&impl{}` as the interface), found by a first reachability pass.
- **Reachability** starts from the API and the module's package initializers. RTA and VTA cannot start from a
  generic function's body, so a library whose API has generic functions or methods of generic types is analysed with
  CHA (logged; the manifest says `cha`). Generic interfaces are matched to their implementations by method names.
- **A library is never linked:** what its journeys reach is what its API can do, not what runs. `link` lists it as a
  service (`library` in `system.json`) without links or unmatched endpoints, and has no bootstrap trace.
- **Its callers:** journeys record the exported symbols of other modules they call (see [Journeys](#journeys)). When
  the run also analysed the library, `uses <module|symbol>` names each symbol's journey there, the library journey's
  answer lists the journeys calling it, and the explorer shows "Called by" on a library's pages and "Library API it
  calls" on its callers' journeys.

```sh
codegraph analyze -library -dir ../fts -out /tmp/libs                        # one library
codegraph fleet -libraries -out /tmp/stack ../authz ../cli/v2 ../fts         # services and libraries together
codegraph fleet -api -out /tmp/stack ../audio ../authz ../cli/v2 ../fts      # and the API of the programs too (audio-api, authz-api)
codegraph query -run /tmp/stack uses github.com/example/cli/v2               # who calls it, and each symbol's journey
codegraph query -run /tmp/stack journey fts go fts.Indexer.Search            # what a symbol does, and who calls it
```

## Inventory and frontier

The full analysis builds every program, so it is for the repositories that matter: a focus (`FOCUS`) and the
programs around it. `codegraph inventory` finds those among an organisation's hundreds of repositories in a few
seconds, without building anything:

```sh
make inventory            # tmp/inventory: inventory.{json,md}, contracts.md, frontier.{json,md,txt}
make graph                # now also analyses the programs in frontier.txt
```

- **Inventory** (`inventory.json`, `inventory.md`): for every repository, its languages (from marker files:
  `go.mod`, `package.json`, `pyproject.toml`, `Dockerfile`, `Chart.yaml`, `*.tf`, …), its last commit and, for Go,
  each module's direct requirements, replaces, main packages and the in-house packages it imports (in-house: a module
  some repository of the organisation declares). The report counts languages, Go versions and dormant programs, ranks
  in-house modules by dependents with the versions in use, and lists which repositories use Kafka, RabbitMQ or both.
- **Container images:** what each repository builds, and what it only uses. The report lists every image with the
  repositories building it, flagging an image several of them build.
  - **Built** (`images`): read from its CI and compose files, with the variables those files assign expanded
    (`PROJECT=shop` then `docker push $REGISTRY/team-${PROJECT}:release-$TAG` builds `team-shop`, tag `release-*`):
    - `docker build -t`, `docker tag` and `docker push` in shell steps: Bitbucket and GitLab pipelines, GitHub
      workflows' `run:` steps, Jenkinsfiles (and `docker.build(…)`), Makefiles, scripts;
    - GitHub's `docker/build-push-action` tags;
    - jobs calling a reusable workflow, by the profile's `workflowImage` convention (below);
    - compose services that build and name an image.

    The Dockerfile a build names (`-f`) is kept, through `docker tag` to what is pushed.
  - **Used without building** (`references`): base images (a Dockerfile's `FROM`), compose images, the images CI
    steps run in, Helm values (`image: {repository, tag}`) and plain manifests (`image: …`); templated files are
    left to `codegraph deploy`, which renders them.
  - **Names** drop the registry and the tag. A part a variable leaves unknown is `*`; a name whose last segment is
    unknown is dropped, since it would name any image.
- **Frontier** (`-focus focus.txt`, and `-run <run dir>` when a run exists): the repositories worth a full
  analysis next to the focus, each with its reasons:
  - **callers:** Go programs importing a focus service's module, its client library (named `<service><suffix>`, the
    profile's `clientLibrarySuffix`, such as `-sdk-go`), or the generated code of a contract it serves (the
    profile's `contractImport`, such as `/contracts/{package}`);
  - **owners:** repositories named like a contract the focus calls and does not serve (`media.thumbnails.v1` →
    `media_thumbnails`) or like a one-sided topic's service, and programs importing such a contract.

  `-rings n` repeats this: ring 2 is the frontier around the focus and ring 1, and so on (`make inventory` uses
  `RINGS`). A ring's hints need a run that analysed the rings before it, so run `make graph` between rounds until
  a round adds few links.

  `frontier.txt` lists the Go programs among them with a commit in the last two years. Dormant programs, libraries and
  other languages are in `frontier.md` only; add one to the `FOCUS` file to analyse it anyway. Copies of a focus
  repository (declaring its module, like `orders-test`) are left out, since their services would take the focus
  services' names, and so are the focus services' client libraries, which `-include-modules` already analyses with
  their callers.
  `frontier.md` also lists the missing contracts and topics that no repository matches.

The hints of `-run` are read from the focus services only, which are linked on their own, so a run that already
includes the frontier gives the same frontier. Without `-run`, callers are found through the focus programs' modules
and client libraries only, a much smaller frontier. A program is a repository whose root module has a `main`, since
`codegraph fleet` loads the root module only: a library whose only `main` is a tool in a nested module stays a library.

### Contracts catalogue

`codegraph inventory` also reads the gRPC services the contract repositories define (`-catalogues protos,more`, or the
profile's `catalogues`; their `proto/` trees) into `inventory.json` and `contracts.md`. With `-run` (`make inventory` passes the last run), the
report says who serves and who calls each one: contracts the run calls with no server in it (their owners were not
analysed, or are not Go: see below), contracts it serves, and contracts nothing in it uses.

## Services described by hand

Some services cannot be analysed: they are not Go (a Python service, say). A file `declared/<service>.json` describes
one by hand, with the evidence for each endpoint, and `codegraph fleet -declared <dir>` (`make graph` with `DECLARED`)
writes it into the run as an ordinary manifest, marked as declared. It then links with the analysed services, its
journeys continue chains, `codegraph deploy` matches the workloads it names, and the address check uses the settings it
lists. The sample `testdata/declared/notifier.json`:

```json
{
  "service": "notifier", "repository": "notifier", "language": "python", "commit": "0000000",
  "description": "Sample: the Python service ledger notifies over gRPC, described by hand.",
  "workloads": ["notifier"],
  "settings": ["SMTP_HOST", "DB_HOST"],
  "endpoints": [
    {"direction": "inbound", "transport": "grpc", "role": "server", "keys": ["NotifierService"],
     "contract": "notifier.v1.NotifierService", "handler": "NotifierServicer", "position": "notifier/server.py:21",
     "methods": {"Notify": ["sql notifications", "http POST http://smtp-relay/send"]}},
    {"direction": "outbound", "transport": "sql", "role": "query", "keys": ["notifications"],
     "position": "notifier/models.py:9"},
    {"direction": "outbound", "transport": "http", "role": "client", "keys": ["POST http://smtp-relay/send"],
     "position": "notifier/mail.py:33"}
  ]
}
```

Endpoints are written as codegraph detects them: gRPC servers keyed by service with the proto-qualified `contract`, gRPC
calls `Service/Method`, HTTP routes `METHOD /path/{param}`, HTTP calls `METHOD http://host:port/path`, topics, queues,
tables and Redis key patterns, with `*` for what is unknown. An inbound endpoint lists what it uses as `"<transport>
<key>"` of the file's outbound endpoints (`"*"` for all of them): `reaches` gives it one journey, and `methods` one
journey per gRPC method. Unknown fields are rejected. A declared service is only as precise as its description, and
its `commit` records when it was read.

## Deployments: what an environment runs

`codegraph deploy` reads an environment's Helm chart (its `values.yaml` and the environment's overrides) and matches what
it runs to the analysed services:

```sh
make deploy               # DEPLOY_CHART -> tmp/graph/deploy
go run ./cmd/codegraph deploy -chart <chart dir> [-values values.yaml,overrides.yaml] [-env production] \
  [-inventory tmp/inventory/inventory.json] [-run tmp/graph] -out tmp/deploy
go run ./cmd/codegraph deploy -rendered objects.yaml …   # objects rendered already
```

- **Rendering:** `helm template` only (needs helm; `brew install helm`), with `KUBECONFIG=/dev/null`, so no cluster
  is ever contacted, and nothing is written into the chart (`make deploy` checks its `git status`). Secret values
  files (sops) are never decrypted or passed on: helm gets masked copies, in a temporary directory, with the same
  keys and every value `(secret)`. `Secret` objects are skipped, and a variable from a secret is listed by name only.
- **`deploy.json`:** every Deployment, StatefulSet, DaemonSet, CronJob and Job with its containers (image without
  registry, tag, command, ports, settings from literals and ConfigMaps), the Services selecting it, and the
  HTTPRoutes and Ingresses exposing them.
- **Matching:** with `-inventory`, the repository is first the one whose CI builds the image (whole name, else its
  last segment); among several (a repository and its copy), the one the image name points to, else the one committed
  to last, and the others are listed (`alsoBuilt`). Otherwise the repository comes from the image name (`payments-ledger` → ledger; `billing-connector` → billing;
  a group prefix shared by three or more images, like `platform-`, never names a repository on its own, and a group
  image naming none falls back to the workload's name). The analysed main comes from the command: the binary's path
  below `bin/` (`/bin/service/grpc/v1` → `notifier-service-grpc-v1`, `/usr/bin/grpc_app` → `orders-grpc`) or a
  subcommand, else the repository's only main, else the main named like the repository for a generic binary
  (`app grpc`). What cannot be told is listed, not guessed.
- **The explorer** (`codegraph link -deploy deploy.json`, run by `make deploy`): the same, per service page; see
  "Diagrams on demand" above.
- **`deploy.md`:** the analysed services and their workloads (command, schedule, hosts), the analysed services not
  deployed, the workloads whose main could not be told, repositories running without an analysed service (with their
  languages), workloads matching no repository, mocks switched on (`*MOCK*=true`), and the public routes.
- **Links against configured addresses** (with `-run`): a setting named like an address (`*HOST`, `*ADDR`, `*URL`,
  …) whose value is a Kubernetes Service (`GRPC_SENDER_HOST=sender-grpc`, or `GRPC_NOTIFY_HOST=notify` with
  `GRPC_NOTIFY_PORT=8080`, the port picking the container when one pod runs several mains) is a call to the services
  that Service runs. Compared with the run's gRPC and HTTP links (messaging goes through a broker):
  - **configured** links are confirmed by the deployment;
  - **in the code, not configured**: the address comes from elsewhere, or the call is not made there;
  - **configured, no link in the code**: a client the analysis does not read (a client library to add to
    `INCLUDE_MODULES`), or a setting a main does not use. The
    run's `settings` tell them apart: **read by the code, no call found** (the main builds the client; a call the
    analysis misses, or a client it never uses) and **read by nothing in the caller's code** (a stale setting, or
    one read in a way the analysis does not see);
  - **settled**: a contract served by several services (`SenderService`: sender and sender-grpc) linked to all of
    them in the code, and the one the caller dials;
  - **mocked**: a `*MOCK*=true` setting naming a service the caller links to (`PAYOUTS_MOCKED` → payouts).
- **Agents:** `codegraph query service <name>` (and the MCP `service` tool) ends with what the environment runs of the
  service when the run has `deploy/deploy.json`.

## Profiles

What codegraph cannot guess about an organisation lives in a profile: a directory kept apart from this repository (a
private repository of its own, or `profile/` here, which git ignores). `PROFILE=<dir>` on make, or
`CODEGRAPH_PROFILE=<dir>` in the environment, points at it. It holds:

- `profile.mk`: the Makefile's settings, read before their defaults (`REPOS_DIR`, `FOCUS`, `RULES`, `REPO_RULES`,
  `DECLARED`, `INCLUDE_MODULES`, `GOPRIVATE`, `DEPLOY_CHART`, `OUT`, `KBCHECK`, `DENYLIST`, …), with paths through
  `$(PROFILE)` since make runs in this directory;
- `profile.json`: the naming conventions `codegraph inventory -profile` relies on, all optional:

  ```json
  {
    "messaging": [{"name": "kafkabus v2", "transport": "kafka", "module": "example.com/kafkabus/v2"}],
    "clientLibrarySuffix": "-sdk-go",
    "contractImport": "/contracts/{package}",
    "catalogues": ["protos"],
    "workflowImage": "{IMAGE_PREFIX}-{PROJECT}"
  }
  ```

  `messaging` adds in-house libraries to the inventory's Kafka and RabbitMQ report; `clientLibrarySuffix` names a
  service's client library (`<service>-sdk-go`); `contractImport` is where a proto package's generated code lives
  (`{package}` with dots as slashes); `catalogues` are the repositories whose `proto/` trees define the contracts;
  `workflowImage` is the image an organisation's reusable CI workflow builds from a calling job's inputs
  (`{INPUT}` placeholders), for the inventory's container images;
- the rule files, the services described by hand, a `FOCUS` file, and whatever `KBCHECK` runs: a command given the run
  directory, printing JSON (`make kbcheck`, run by `make graph`), such as a comparison with an architecture
  knowledge base;
- a denylist for `make audit`: words (the organisation's name, its repositories and services) no file of this
  repository may contain.

A profile can carry its own `Makefile` that runs this one's targets with `PROFILE=$(CURDIR)` (`make -C <codegraph>
graph PROFILE=$(CURDIR)`), its runs (`OUT`, `INVENTORY`), and its notes for people and agents. With a profile,
`make test` also loads its rule files.

## Asking questions (people and AI agents)

`codegraph query` answers questions about a run in small Markdown (or `-json`) answers, with `file:line` positions to cite:

```sh
go run ./cmd/codegraph query -run tmp/graph services                      # every service and the journeys it offers
codegraph query -run tmp/graph service orders                  # journeys, upstream and downstream, unmatched
codegraph query -run tmp/graph journey orders "POST /orders"    # what it does across services (-depth N)
codegraph query -run tmp/graph who orders.created               # who sends, receives or uses a key
codegraph query -run tmp/graph path orders inventory            # how one service reaches another
codegraph query -run tmp/graph impact inventory                 # what depends on a service or a key
codegraph query -run tmp/graph uses envconfig.Process           # who calls a library's exported API (module, package or name)
codegraph query -run tmp/graph image registry.example.com/team/app:v1  # which repositories build or use an image, where it runs
codegraph query -run tmp/graph search invoice                   # services, journeys and keys by name
make query Q='who orders.created'                               # the same, on the last make graph run
```

`image` reads the inventory next to the run (`<run>/../inventory/inventory.json`, as the Makefile lays them out; `-inventory`
points elsewhere) and the run's deployment (`<run>/deploy/deploy.json`).

A journey is named by its key, or part of it, after an optional transport for a key consumed on two transports
(`journey billing amqp billing.refunds`). Flags can come anywhere.

Answers can illustrate themselves:
- **`-diagram`** adds a Mermaid diagram: a sequence diagram for `journey`, and a flowchart of services and links for
  `service` (its connections), `who`, `path` and `impact`. A journey over 200 steps is drawn following fewer services,
  and the answer says how many. With `-json`, the answer becomes `{answer, diagram, note, explorer}`.
- **Explorer links:** Markdown answers end with links to the explorer's pages for what they show, such as
  `file:///…/tmp/graph/_system/system.html#/j/ledger/grpc/LedgerService%2FCredit`. `-explorer <url>`
  points them at a page served elsewhere, and `-explorer none` drops them.

Each run also gets the files AI agents need (format `agents`, on by default):
- `AGENTS.md` at the run's root: what the run is, the commands with examples taken from it, the vocabulary, and
  rules for answers (cite positions, `*` is unknown, `name-only` is weak evidence, missing is not absent).
- `_system/context.md`: the whole system in one compact file (tens of KB for a large run), to attach to a chat.

In this repository, the Claude Code skill `.claude/skills/codegraph` points at `tmp/graph` and its `AGENTS.md`, so
architecture questions asked in Claude Code use the run.

### MCP server (Claude Code, Claude Desktop)

`codegraph mcp -run <run dir>` serves the same answers over the Model Context Protocol on stdin and stdout, to clients
without a shell. It is a local process; nothing listens on the network.
- **Tools:** `services`, `service`, `journey`, `who`, `path`, `impact`, `uses`, `image` and `search`, each answering in Markdown
  (`json: true` for JSON). A mistake comes back as an error the model can read, such as a misspelt service with
  suggestions.
- **Diagrams and links:** `service`, `journey`, `who`, `path` and `impact` take `diagram: true` for a Mermaid diagram,
  which Claude Desktop and claude.ai can draw in the chat. Answers end with links to the explorer, as with `codegraph query`
  (`codegraph mcp -explorer <url|none>`).
- **Resources:** `codegraph://guide` (the vocabulary and rules of `AGENTS.md`) and `codegraph://context` (the whole
  system in one file).
- **Run:** loaded on the first question and reloaded when `make graph` replaces it. Before any run exists, questions
  say to run `make graph`.

To connect it:
- **Claude Code:** `.mcp.json` in this repository starts it (`go run ./cmd/codegraph mcp -run tmp/graph`) in sessions opened
  here; Claude Code asks to approve it the first time.
- **Claude Desktop:** `make mcp-desktop` builds `tmp/bin/codegraph` and prints the entry to add under `"mcpServers"` in
  `~/Library/Application Support/Claude/claude_desktop_config.json`, then restart Claude Desktop:

  ```json
  "codegraph": {
    "command": "/…/codegraph/tmp/bin/codegraph",
    "args": ["mcp", "-run", "/…/codegraph/tmp/graph"]
  }
  ```

  - Both paths must be absolute, and `args` is required: Claude Desktop starts servers outside the repository, and
    without `mcp` codegraph prints its usage and exits ("Server disconnected"). Its log (`~/Library/Logs/Claude/
    mcp-server-codegraph.log`) then shows a line saying which arguments are missing, or that `-run` holds no run.
  - The binary does not follow code changes: `make mcp-build` and restart Claude Desktop after pulling them. To
    always run the current code instead, use `"command": "go"` with `"args": ["run", "-C", "/…/codegraph", "./cmd/codegraph",
    "mcp", "-run", "/…/codegraph/tmp/graph"]`; it starts in about two seconds, but needs `go` on Claude Desktop's
    `PATH` and fails while the code does not compile.

## Sharing a run: bundles

A run is a directory of manifests, tens of MB with the system view. To share it, `codegraph bundle` packs what
questions need into one file: the manifests, the deployment, the inventory, the explorer, and metadata. A 90-service
run makes a bundle of about 1.4 MB with gzip, or 0.9 MB with zstd.

```sh
codegraph bundle -run tmp/graph                       # tmp/graph.tar.gz
codegraph bundle -run tmp/graph -out run.tar.zst      # zstd (also -compress zstd)
codegraph bundle -run tmp/graph -no-settings -no-positions -out shared.tar.gz
make bundle BUNDLE_FLAGS=-no-settings                 # the last make graph run, with its inventory
```

Every command that reads a run reads a bundle the same way: `-run run.tar.gz` for `query`, `mcp`, `diff`, `deploy`
and `inventory`. A central machine can run `make inventory`, `make graph` and `make bundle` on a schedule and publish
the file; everyone else points their own `codegraph mcp` at their copy, which it reloads when the file is replaced.

- **Contents:** `bundle.json` (the metadata), `<service>/manifest.json`, `deploy/deploy.json` and `inventory.json`
  when the run has them, and `_system/system.html`. Extracted (`tar xf`), it is a run directory. The system view is
  left out: readers link the manifests again on load, in about a second.
- **Options:**
  - `-no-settings`: leave out the values an environment configures. The deployment's variables keep their names, and
    the address check's settings and the mocks keep their names only.
  - `-no-positions`: leave out every file reference: `file:line` positions, declaration and Dockerfile names, the
    chart's directory.
  - `-inventory <file|none>`: an inventory other than the run's own (`<run>/inventory.json`, else `<run>/../inventory`),
    or none.
  - `-compress gzip|zstd`: gzip by default, zstd when `-out` ends in `.zst`.

  A bundle can be packed again (`-run run.tar.gz`) with other options, to strip it before sharing it further.
- **Answers** from a bundle say when it was made, by which codegraph, and what it leaves out. Its explorer page is
  written once to the user's cache (`~/Library/Caches/codegraph/bundles/` on macOS, `~/.cache/codegraph/bundles/` on
  Linux) for answers to link to.
- **Versions:** the metadata records its format, the codegraph version that wrote it and those that analysed its
  manifests (each manifest, the inventory and the deployment record their own). A codegraph reading a bundle of a
  newer format refuses it and says which version wrote it.
- **What it holds:** the architecture of every service in it, and, without `-no-settings`, the configuration values
  of the deployment (never secrets: their values are not read). Share it as you would the code.

## Reviewing changes

`codegraph diff -base <run> -head <run>` compares two runs, each a directory of service manifests as `analyze` and `fleet`
write them. It links both and reports in Markdown, ready for a review comment:
- cross-service links added and removed;
- services added and removed, with their endpoint counts;
- per service, the endpoints added and removed;
- topics and queues nothing analysed answers.

`-out` writes the report to a file. `-fail-on-links` exits with status 1 when a cross-service link changed, so a CI
job can ask for a second look.

`make graph` keeps the previous run in `tmp/graph.prev` and writes `tmp/graph/diff.md` against it. In CI, run `codegraph`
on a change's base and head and diff the two runs. A service's own repository gives its endpoints. The other
services' manifests, from a stored run of the main branches, give the links.

## How linking works

Outbound `client`/`producer` endpoints are matched against inbound `server`/`consumer` endpoints of
other services on the same transport. HTTP requires the URL host to be the target's name, an alias
or `tags["host"]` (an unresolved host still matches on the path and is marked `path-only`); routes
support `{id}`, `{rest...}`, `:id` and `*`; a path-only client must have at least one literal path
segment and never links to a catch-all or unresolved route (generic clients for external APIs would
otherwise reach every `/metrics` and `/health`). gRPC calls keyed `Service/Method` link to the
server registered for `Service`; every gRPC endpoint also records its `contract`, the
proto-qualified service from the generated `<Service>_ServiceDesc` (`notify.v1.NotifierService`), and when
both sides know it they must agree, which tells API versions apart. A grpc-gateway route forwards through
the generated client (a call in a `*.pb.gw.go` file): when that reaches the service's own server, the route's journey
continues into the method's journey (confidence `gateway`), though no link joins a service to itself. A link between endpoints with
several keys lists every matching key. `*` in any key is a wildcard for the part the resolver could
not determine (confidence `pattern`). A link is `sync` or `async` from the outbound endpoint's kind.
SQL tables and Redis key patterns used by several services become `shared-store` links when the
services come from different modules (confidence `name-only`: the same name may still be two
databases or Redis instances; a key with no letter or digit, such as `*:*:*`, is never shared); mains of one module sharing their tables is not reported, and the
system diagram groups them per module. Anything unmatched is listed as dangling, with a reason when
it is not an orphan:
- **`same-service`:** the counterpart is in the service itself, such as a retry topic it publishes and consumes.
- **`broker`:** a `.retry`, `.dlq` or `.delay.<ttl>` queue that the broker library feeds itself.
- **`other-transport`:** the same name is answered on another messaging transport, such as a topic moving from Kafka
  to RabbitMQ while both run.

A topic or queue endpoint with several keys (a consumer registering several queues) that links through some of them
also lists the others, one entry per reason, marked partial ("the endpoint's other keys link"), so one linked queue no
longer hides the rest.

The system diagram leaves out the first two. `system.md` lists every unmatched endpoint with its reason, and the
explorer shows reasons per service and lists the topics and queues nothing analysed answers. Those may belong to a
repository that was not analysed, or be unused.

## Limitations

codegraph reads code, not traffic: what it reports is what the code can do, and what it cannot resolve it says (`*`)
rather than guesses.

- **Values.** Keys (routes, topics, tables, hosts) are resolved from constants, configuration fields, helpers'
  callers and router prefixes. The resolver is partly context-sensitive: a value resolved through a call binds the
  callee's parameters to that call's arguments, and an endpoint inside a helper is resolved per immediate caller (up
  to 16), but a helper called through another helper is the union over the outer helper's callers. Library functions
  are followed only when they are straight-line (`Topic()`, `Topic.String()`).
- **Configuration.** Values read from files or the environment at run time are unknown (use inline defaults,
  `-alias`, `tags["host"]` or directives). A configuration field an env library fills and nothing sets in code takes
  its `envDefault` tag (caarlos0/env) or its `default` tag next to an `envconfig` tag (kelseyhightower/envconfig), for
  the analysed module's own types only: a library's defaults (`localhost`, a broker client's default queue) would make
  unrelated services look connected. Router prefixes from configuration stay `*`.
- **Formatting.** `fmt.Sprintf`'s verbs are always `*`, even with constant arguments (`fmt.Sprintf(":%d", cfg.Port)` is
  `:*`).
- **Messaging.** AMQP exchange bindings and NATS wildcards (`>`, `*` tokens) are not modelled, confluent `Produce`
  topics (a nested pointer field) are not resolved, and kafka-go readers have no handler to root a journey at.
- **Journeys.** Reflection-based routing and struct-based middleware (`&wrapper{next: h}`) can hide the real handler;
  shared dispatch code outside factory closures over-approximates; a handler only called from library code must be
  dispatched to dynamically by that library (as grpc and net/http do) to be reachable.
- **gRPC and SQL.** Services whose generated code has no `ServiceDesc` are matched by service name only; gorm tables
  from gorm's default naming (no `TableName()` and no `Table(name)`) are not inferred.
- **Building.** The code must type-check for the chosen build tags (cgo packages need their C toolchain), and
  `golang.org/x/tools` must be recent enough for the Go version of the analysed code. VTA on very large monorepos can
  take a while; start with `-algo rta`.

## Development

```sh
make test       # gofmt, go vet, the unit tests and the golden test over testdata/ (go test -short skips the golden test)
make cover      # statement coverage of the whole suite; fails under COVER_MIN (70%)
make samples    # the samples' run, in tmp/samples
make release VERSION=v1.2.0   # the release binaries, in bin/
```

Each responsibility is its own package under `internal/` (see [Layout](#layout)), wired in `cmd/codegraph/main.go`.
The samples under `testdata/` (each its own module, offline, with local stubs for third-party libraries) are what the
golden test, `cmd/codegraph/main_test.go`, checks. `.github/workflows/test.yml` runs `make test` and `make cover` on
every push and pull request. `CLAUDE.md` holds the design decisions and conventions behind the code, for contributors
and coding agents alike.

**Versions:** codegraph writes its version into what it produces: each manifest, the inventory, the deployment and
every bundle record it (`codegraph`), and `codegraph version` prints it. The Makefile builds and runs codegraph with
`git describe`'s version (the latest tag, plus the commits past it and `-dirty`), or `VERSION=` when set.

**Releases:** `make release VERSION=v1.2.0` builds `bin/codegraph-v1.2.0-{darwin,linux}-{amd64,arm64}` (static,
stripped, the version stamped in: `codegraph version` prints it) and `bin/SHA256SUMS`; upload those to the GitHub
release. Without `VERSION`, `git describe` names them (the latest tag, plus `-dirty` with uncommitted changes).
`PLATFORMS` and `DIST` change the targets and the directory; `bin/` is git-ignored.

## License

MIT; see [LICENSE](LICENSE).
