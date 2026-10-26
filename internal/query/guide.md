# Asking about this system

This is a run of codegraph over {{.Services}} services from {{.Repos}} repositories: a static analysis of their
Go code (type information and call graphs), not of runtime traffic. Use it to answer questions about the architecture,
with evidence.

Below, **`$R`** is the run directory, `{{.Run}}` (one `<service>/manifest.json` per service; `_system/` has the
system view). **`codegraph`** is the installed binary (`go install github.com/zalgonoise/codegraph/cmd/codegraph@latest`),
or `go run ./cmd/codegraph` run from {{if .Tool}}`{{.Tool}}`{{else}}the codegraph repository{{end}}.

## Commands

```
codegraph query -run $R [-json] [-depth N] [-diagram] <command> [args]

{{.Commands}}
```

- **Answers** are Markdown (`-json` for programs), small, and cut with a note past {{.MaxRows}} rows; inline lists
  (the journeys of an endpoint) stop at 12, and `-json` has them all. Flags can come anywhere.
- **`-depth`** (default 2) is how many services beyond the journey's own `journey` follows. 0 lists only its own
  calls, and the journeys they reach without following them. Raise it when an answer says "not followed: deeper than
  asked".
- **`-diagram`** adds a Mermaid diagram to `service` (its connections), `journey` (a sequence diagram), `who`, `path`
  and `impact` (flowcharts), to show people. With `-json`, the answer becomes `{answer, diagram, note, explorer}`.
  - A journey too large to draw (over 200 steps) is drawn following fewer services, and the answer says how many.
  - Flowcharts draw one edge per pair of services, transport and kind: solid sync, dashed async, dotted without an
    arrow for a shared store. They are labelled with the key, or the number of keys.
  - `impact`'s flowchart also draws the links between the services on its journeys' chains, every link between
    each pair, not only the one the chain takes.
- **Explorer links:** when the run has `_system/system.html`, Markdown answers end with links to the explorer's pages
  for what they show (the service, the journey, the key). They are `file://` URLs on the machine that ran codegraph.
- **Libraries:** `uses <module|package|symbol>` lists who calls a library's exported functions and methods: the
  services whose code calls each, and the journeys that do, at their first call site. A journey's answer says how many
  its own code calls, per module (`-json` lists them). Calls the code makes through interfaces of the standard library
  are not counted, and the standard library only counts when the run was made with `-std`. When the run analysed the
  library itself (marked `(library)` in `services`), each symbol names its journey there, and that journey's answer
  lists the journeys calling it.
- **Container images:** `image <name or reference>` lists the repositories whose CI or compose files build an
  image (with the file, line and Dockerfile), those that only use it (Dockerfiles' `FROM`, Helm values,
  manifests), and the containers of the run's deployment running it, with the repository each was matched to. It
  needs the inventory next to the run (`codegraph inventory`); a third-party image has users but no builder.
- **Start broad** (`services`, `search`), then narrow (`service`, `journey`, `who`).
- **What a change affects:** `impact <service>` lists every journey that reaches the service by any call or message,
  each once by its nearest way in, with the chain of services it crosses. For an API change, narrow it to one
  contract: `service <name>` lists the service's journeys, whose gRPC keys are `Service/Method`, then run
  `impact <Service>` (or `impact <topic>`).

## Examples from this run
{{range .Examples}}
- {{index . 0}}: `codegraph query -run $R {{index . 1}}`{{end}}

## Vocabulary

- **Service:** one `main` package of a repository, such as `orders-grpc` (repository orders, cmd/grpc).
  - A **declared** service (marked `(declared, <language>)` in `services`) is one codegraph cannot analyse, such as a Python
    one, described by hand: its endpoints and journeys are as written in its file, with positions in its repository.
  - A **library** (marked `(library)`) is a module without a `main`, analysed through its exported API: one journey
    per exported function or method of its public packages (transport `go`, keys like `pkg.Type.Method`; an interface
    method's journey runs the library's implementations). Libraries are never linked: what their journeys reach is
    what the API can do, not what any service runs. `<name>-api` is the API of a repository that also has a program.
    The symbols of a library's internal packages (role `internal`) are listed apart, under "Internal API".
- **Endpoint:** something a service exposes or uses, made of transport (http, grpc, kafka, amqp = RabbitMQ, nats, sql,
  redis), role (server, client, producer, consumer, query) and key (route, gRPC service or method, topic, queue,
  table, Redis key pattern).
- **Journey:** what happens when an inbound endpoint is hit: an HTTP route, a gRPC method, a consumed topic or queue,
  or a Redis subscription. It is named by transport and key, since one key can start a journey on two transports
  (`journey <service> amqp <key>` picks one). Its outbound calls are in program order, and it continues into the
  journey of the endpoint each call links to.
  - Every service also has a **main journey** (`journey <service> main`, transport `main`): what its `main` does
    itself, up to the handlers it registers, whose journeys are their own. For a job or a pipeline that no endpoint
    starts, that is all its work; for a server, the connections, dials and listeners it opens at startup.
  - A gRPC journey is one method (`Service/Method`), or the whole service when the handler's methods could not be
    told apart.
  - A handler built by a constructor is named after it (`pkg.New(…)`).
  - One contract can be served by two services, such as two mains of one repository.
- **Link:** an outbound endpoint matched to another service's inbound one.
  - Kind is `sync` (a call) or `async` (a message).
  - Confidence: `exact`; `pattern` (wildcards); `path-only` (HTTP host unknown); `name-only` (a table or Redis key
    used by two repositories, only the name is shared).
- **Unmatched endpoint:** one no analysed service answers. A reason says why it is not an orphan: `same-service`,
  `broker` (retry, delay or dead-letter queues), `other-transport` (the same name on another messaging transport). A
  topic or queue without a reason is answered by nothing analysed: a repository that was not analysed, or something
  unused. An endpoint of several topics or queues that links through some of them lists the others as unmatched,
  marked "the endpoint's other keys link".

## Annotations in answers

- **store:** a table or Redis key the journey uses.
- **outside the analysed services:** a call to something not in the run (another repository, another language, an
  external API).
- **no consumer among the analysed services:** a message nothing analysed receives.
- **followed above:** this journey is already listed earlier in the answer.
- **cycle:** the journey is already on the path.
- **not followed: deeper than asked:** past `-depth`.
- In `who`, an inbound endpoint shows where its handler is **registered** and the **handler** itself (for a handler
  wrapped in middleware, the handler's own position, not the middleware's). An endpoint with several call sites lists
  them all.
- **Unmatched reasons:** `same-service` means its counterpart is in the same service (it publishes and consumes the
  name itself; links never join a service to itself); `broker` is a retry, delay or dead-letter queue the broker
  library feeds; `other-transport` means the same name is answered on another messaging transport. "nothing analysed
  answers it" is a real loose end, or a counterpart outside the run.
- **whole service:** a gRPC journey covering every method of a service; a call in it may come from any method.

## Rules for answers

- **Cite positions.** They are `file:line` relative to the repository of the service the line belongs to: in a
  journey across services, each step's position is in that step's service. Dependencies appear as
  `module@version/file`.
- **`*` is a part the analysis could not resolve**, usually configuration. Say so; do not guess it.
- **`name-only` links are weak evidence:** the same table name may be two databases.
- **Missing is not absent.** The analysis covers reachable Go code only. Reflection-based routing, values from
  configuration and services that were not analysed (other repositories, other languages) can be missing.
- **Declared services are as precise as their description.** Say when an answer rests on one, and that its file
  records the commit it was read at.
- **More is not certain.** Shared dispatch code, whole-service gRPC journeys and mocks a service can switch to by
  configuration can make a journey list more than one request does at runtime. `path` lists chains of links between
  services that some code takes, not necessarily one journey: check with `journey`.
- **Deployment:** when the run has `deploy/deploy.json` (`make graph` with helm and the environment's chart), `service`
  ends with what that environment runs of it: workloads, commands, Kubernetes hosts, mocks switched on, (with
  `-json`) its settings, and its gRPC and HTTP links checked against the addresses it is configured with (which of
  several servers of a contract it dials, links its settings do not explain, settings no link explains). "Not deployed" means no workload was matched to that main; `deploy/deploy.md` lists the
  workloads it could not tell. Secret values are never read.
- **Prefer `codegraph query` to reading files.** Never load `_system/system.json` whole: it is large. `_system/context.md`
  summarises the whole system in one file.

## Other files

- `_system/system.html`: the interactive explorer, for people. Its pages are `#/s/<service>`,
  `#/j/<service>/<transport>/<key>` and `#/k/<transport>/<key>`, with each part URL-encoded.
- `_system/system.md`: links, unmatched endpoints, and journeys across services as sequence diagrams.
- `<service>/report.md`: one service's components, endpoints and journeys.
- `kbcheck.json`: when `make graph` made the run, the comparison with the architecture knowledge base.
- `diff.md`: when `make graph` made the run and there was a previous one, what changed since.
- `deploy/deploy.md`: when `make graph` read the environment's chart, what it runs: workloads matched to services,
  services not deployed, other repositories' workloads, mocks switched on, and public routes.
