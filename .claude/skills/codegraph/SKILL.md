---
name: codegraph
description: Answer questions about the architecture of Go services analysed by codegraph - which service calls, publishes to or consumes from which; what an HTTP route, gRPC method or queue consumer does across services; who uses a topic, queue, table or route; how one service reaches another; what a change would affect. Uses a codegraph run (the one the configuration file or profile names, else tmp/graph) and cites file:line evidence.
---

# Architecture questions with codegraph

1. **Find the run.** Without `-run`, `codegraph query` and the MCP server read the run of `-profile`, else of
   `$CODEGRAPH_PROFILE`, else the configuration file's `query.run` (`~/.codegraph.yaml`, or `.codegraph.yaml` here),
   else its profile's, else `tmp/graph` in this repository. `go run ./cmd/codegraph config` prints which one (its
   last line), and a run can also be a bundle file someone shared (`codegraph bundle`; every `-run` takes one).
2. **Check it is current.** `go run ./cmd/codegraph status` says when the run was made, by which codegraph, and which
   repositories have new commits since. Mention it when the answer may be out of date. If there is no run, or the user
   wants a fresh one, ask before making it, and say what it would do: `codegraph run graph` (`make graph`) analyses
   every repository it selects, which can take a while the first time (later runs reuse the analysis of the
   repositories without new commits); `codegraph` alone runs the configuration file's default pipeline, whose steps
   can run the user's own commands, such as pulling every clone. `codegraph run -dry-run` shows the steps without
   running anything.
3. **Read the run's `AGENTS.md` once.** It has the vocabulary, the commands with examples from the run, and the rules
   for answers.
4. **Ask.** When the `codegraph` MCP server's tools are available (`services`, `service`, `journey`, `who`, `path`,
   `impact`, `uses`, `image`, `search`), use them. Otherwise run `go run ./cmd/codegraph query [-run <run>] <command>`
   from this repository. Either way, start with `services` or `search`, then narrow with `service`, `journey`, `who`,
   `path`, `impact`, `uses` (who calls a library's API) or `image` (which repository builds or uses a container image,
   and where it runs); ask for JSON to process an answer. A service marked `(library)`
   is a module analysed through its exported API (`codegraph analyze -library`): its journeys (transport `go`, keys
   like `pkg.Type.Method`) say what each exported symbol does, and list the journeys calling it.
5. **Answer with evidence:** the services, keys and `file:line` positions from the answers. Say when a part is `*`
   (unresolved), a link is `name-only`, or something is outside the analysed services. A hop keyed
   `<database>.<table>` (`same-database`) is a hand-off, not a call: one journey writes the table and a scheduled job
   or a worker reads it later, as the deployment configures their databases; say so, and give the schedule when the
   answer has one. Say so when the answer comes from a bundle that leaves out settings or positions. To confirm a detail, read the analysed repository's code, and
   never write there.
6. **Illustrate when it helps:** ask `service`, `journey`, `who`, `path` or `impact` for a diagram (`diagram: true`,
   or `-diagram`) and show its Mermaid source as a diagram; pass on the explorer links answers end with.
