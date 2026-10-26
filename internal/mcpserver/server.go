// Package mcpserver serves a run of codegraph over the Model Context Protocol, so
// MCP clients (Claude Code, Claude Desktop) can ask about the system without
// a shell. Each tool is one question of package query, answered the same
// way as `codegraph query`. The guide and the whole-system summary are resources.
//
// The server reads the run once and reloads it when `make graph` replaces it.
// It never writes anything.
package mcpserver

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zalgonoise/codegraph/internal/query"
)

// Resource URIs.
const (
	GuideURI   = "codegraph://guide"
	ContextURI = "codegraph://context"
)

// instructions reach the client's model with the tool list.
const instructions = `Answers questions about the architecture of the services in a codegraph run: a static analysis of their
Go code. Start broad (services, search), then narrow (service, journey, who, path, impact). Answers carry file:line
positions, each relative to its own service's repository: cite them. "*" is a part the analysis could not resolve;
"name-only" links are weak evidence; missing is not absent (only analysed Go code is covered). Read the resource
` + GuideURI + ` for the vocabulary, and ` + ContextURI + ` for the whole system in one file. Answers end with links to
the run's explorer, an HTML page on this machine: pass them on. To illustrate an answer for the user, ask service, journey,
who, path or impact for a diagram (diagram: true) and show its Mermaid source as a diagram.`

// New returns a server answering from the run in dir. The run is loaded on
// the first question, so the server starts even before a run exists. Answers
// link to the explorer page given (see query.Run.UseExplorer: "" for the
// run's own system.html, "none" for no links). inventory, when set, is the
// inventory image questions read instead of the one next to the run.
func New(dir, version, explorer, inventory string) *mcp.Server {
	s := &state{dir: dir, explorer: explorer, inventory: inventory}
	srv := mcp.NewServer(&mcp.Implementation{Name: "codegraph", Title: "codegraph", Version: version},
		&mcp.ServerOptions{Instructions: instructions})

	addTool(srv, s, "services", "List every service, its repository, how many journeys it offers per transport, "+
		"and how many services link into and out of it.",
		func(noArgs) []string { return []string{"services"} })
	addTool(srv, s, "service", "Describe one service: its journeys (entrypoints), the links into and out of it, "+
		"the stores it shares, and the endpoints no analysed service answers.",
		func(in serviceArgs) []string { return []string{"service", in.Name} })
	addTool(srv, s, "journey", "Follow one journey of a service (an HTTP route, gRPC method or consumed topic or "+
		"queue) across services: its outbound calls in program order, the services and journeys they reach, with "+
		"positions. Part of the key is enough; give the transport when a key is consumed on two.",
		func(in journeyArgs) []string {
			args := []string{"journey", in.Service}
			if in.Transport != "" {
				args = append(args, in.Transport)
			}
			return append(args, in.Key)
		})
	addTool(srv, s, "who", "Find who sends, receives or uses a key: a topic, queue, table, Redis key pattern, HTTP "+
		"route or gRPC service or method. Exact key, or keys containing it.",
		func(in keyArgs) []string { return []string{"who", in.Key} })
	addTool(srv, s, "path", "Find how one service reaches another: the shortest chains of links between them "+
		"(possible routes, not necessarily one journey).",
		func(in pathArgs) []string { return []string{"path", in.From, in.To} })
	addTool(srv, s, "impact", "Find what depends on a service, or on a key (topic, queue, table, gRPC service): the "+
		"direct links or endpoints, and every journey that reaches it with the chain of services it crosses.",
		func(in impactArgs) []string { return []string{"impact", in.Target} })
	addTool(srv, s, "uses", "Find who calls a library's exported functions and methods: give a module or package path "+
		"(every symbol of it) or part of a symbol's name (Process, cli.Run). Lists the services whose code calls each "+
		"symbol and the journeys that do, at their first call site.",
		func(in usesArgs) []string { return []string{"uses", in.Target} })
	addTool(srv, s, "image", "Find where a container image comes from: the repositories whose CI or compose files build "+
		"it, those whose Dockerfiles, Helm values or manifests use it, and the containers of the run's deployment "+
		"running it, with the repository each was matched to. Give a name or a whole reference (registry/team/app:tag).",
		func(in imageArgs) []string { return []string{"image", in.Image} })
	addTool(srv, s, "search", "Find services, journeys and keys whose names contain every word.",
		func(in searchArgs) []string { return []string{"search", in.Text} })

	srv.AddResource(&mcp.Resource{URI: GuideURI, Name: "guide", MIMEType: "text/markdown",
		Description: "How to read the answers: vocabulary, annotations and rules (the run's AGENTS.md)."},
		s.resource(func(r *query.Run) string { return query.Guide(r, "") }))
	srv.AddResource(&mcp.Resource{URI: ContextURI, Name: "context", MIMEType: "text/markdown",
		Description: "The whole system in one compact file: services, journeys, links, stores, unanswered topics."},
		s.resource(query.Context))
	return srv
}

// Tool inputs. Every tool also takes json; service, journey, who, path and
// impact take diagram, and journey takes depth.
type (
	common struct {
		JSON bool `json:"json,omitempty" jsonschema:"answer in JSON instead of Markdown"`
	}
	drawn struct {
		common
		Diagram bool `json:"diagram,omitempty" jsonschema:"add a Mermaid diagram of the answer (a sequence diagram for a journey, a flowchart otherwise), to show the user"`
	}
	noArgs      struct{ common }
	serviceArgs struct {
		drawn
		Name string `json:"name" jsonschema:"the service name, e.g. from the services tool"`
	}
	journeyArgs struct {
		drawn
		Service   string `json:"service" jsonschema:"the service the journey starts in"`
		Key       string `json:"key" jsonschema:"the journey's key (route, Service/Method, topic, queue, CLI command, or a library's exported symbol such as pkg.Type.Method), or part of it"`
		Transport string `json:"transport,omitempty" jsonschema:"http, grpc, kafka, amqp, nats, redis, cli, go (a library's API) or main (the program's own work, key main), when a key starts journeys on two transports"`
		Depth     *int   `json:"depth,omitempty" jsonschema:"how many services beyond its own to follow (default 2; 0 lists only its own calls)"`
	}
	keyArgs struct {
		drawn
		Key string `json:"key" jsonschema:"a topic, queue, table, Redis key pattern, route, or gRPC service or method"`
	}
	pathArgs struct {
		drawn
		From string `json:"from" jsonschema:"the service the chain starts from"`
		To   string `json:"to" jsonschema:"the service it reaches"`
	}
	impactArgs struct {
		drawn
		Target string `json:"target" jsonschema:"a service name, or a key (topic, queue, table, gRPC service)"`
	}
	usesArgs struct {
		common
		Target string `json:"target" jsonschema:"a module or package path, or part of a symbol's name"`
	}
	imageArgs struct {
		common
		Image string `json:"image" jsonschema:"an image name or reference, such as team/app or registry.example.com/team/app:v1"`
	}
	searchArgs struct {
		common
		Text string `json:"text" jsonschema:"words that must all appear in the name"`
	}
)

// options is implemented by every input, for the answer's format, diagram and depth.
type options interface {
	opts() query.Options
}

func (c common) opts() query.Options { return query.Options{JSON: c.JSON, Depth: 2} }

func (d drawn) opts() query.Options {
	o := d.common.opts()
	o.Diagram = d.Diagram
	return o
}

func (in journeyArgs) opts() query.Options {
	o := in.drawn.opts()
	if in.Depth != nil {
		o.Depth = *in.Depth
	}
	return o
}

// addTool registers a question: args turns the input into the command of
// query.Answer, whose answer (or error, which the model can read) is the
// tool's result.
func addTool[In options](srv *mcp.Server, s *state, name, description string, args func(In) []string) {
	mcp.AddTool(srv, &mcp.Tool{Name: name, Description: description,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)}},
		func(_ context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
			r, err := s.run()
			if err != nil {
				return nil, nil, err
			}
			answer, err := query.Answer(r, args(in), in.opts())
			if err != nil {
				return nil, nil, err
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: answer}}}, nil, nil
		})
}

func ptr[T any](v T) *T { return &v }

// state holds the loaded run, and reloads it when its directory changes
// (make graph moves the previous run aside and writes a new one).
type state struct {
	inventory string // an inventory.json for image questions; "" for the one next to the run
	dir       string
	explorer  string
	mu        sync.Mutex
	r         *query.Run
	info      os.FileInfo
}

func (s *state) run() (*query.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := os.Stat(s.dir)
	if err != nil {
		return nil, fmt.Errorf("no run in %s yet: `make graph` in the codegraph repository makes one", s.dir)
	}
	if s.r != nil && os.SameFile(info, s.info) && info.ModTime().Equal(s.info.ModTime()) {
		return s.r, nil
	}
	r, err := query.Load(s.dir)
	if err != nil {
		return nil, err
	}
	r.UseExplorer(s.explorer)
	if err := r.UseInventory(s.inventory); err != nil {
		return nil, err
	}
	s.r, s.info = r, info
	return r, nil
}

func (s *state) resource(render func(*query.Run) string) mcp.ResourceHandler {
	return func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		r, err := s.run()
		if err != nil {
			return nil, err
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{
			{URI: req.Params.URI, MIMEType: "text/markdown", Text: render(r)},
		}}, nil
	}
}
