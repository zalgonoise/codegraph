---
name: codegraph
description: Answer questions about the architecture of Go services analysed by codegraph - which service calls, publishes to or consumes from which; what an HTTP route, gRPC method or queue consumer does across services; who uses a topic, queue, table or route; how one service reaches another; what a change would affect. Uses a codegraph run (tmp/graph by default) and cites file:line evidence.
---

# Architecture questions with codegraph

1. **Find the run.** It is `tmp/graph` in this repository (`make graph`), or the `OUT` of the profile in use. If it
   has no `AGENTS.md`, ask the user before making one: `make graph` analyses every repository it is given and can
   take a while.
2. **Read the run's `AGENTS.md` once.** It has the vocabulary, the commands with examples from the run, and the rules
   for answers.
3. **Ask.** When the `codegraph` MCP server's tools are available (`services`, `service`, `journey`, `who`, `path`,
   `impact`, `uses`, `image`, `search`), use them. Otherwise run `go run ./cmd/codegraph query -run <run> <command>` from this
   repository. Either way, start with `services` or `search`, then narrow with `service`, `journey`, `who`, `path`,
   `impact`, `uses` (who calls a library's API) or `image` (which repository builds or uses a container image, and
   where it runs); ask for JSON to process an answer. A service marked `(library)`
   is a module analysed through its exported API (`codegraph analyze -library`): its journeys (transport `go`, keys
   like `pkg.Type.Method`) say what each exported symbol does, and list the journeys calling it.
4. **Answer with evidence:** the services, keys and `file:line` positions from the answers. Say when a part is `*`
   (unresolved), a link is `name-only`, or something is outside the analysed services. To confirm a detail, read the
   analysed repository's code, and never write there.
5. **Illustrate when it helps:** ask `service`, `journey`, `who`, `path` or `impact` for a diagram (`diagram: true`,
   or `-diagram`) and show its Mermaid source as a diagram; pass on the explorer links answers end with.
