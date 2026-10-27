// Package model holds the serialisable description of one service (the
// "manifest") and of a set of linked services (the "system").
//
// The manifest is the contract between `codegraph analyze` and `codegraph link`: each
// service repository produces one, and the link step only needs the JSON.
package model

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
)

// Direction of an endpoint relative to the service that owns it.
type Direction string

const (
	Inbound  Direction = "inbound"
	Outbound Direction = "outbound"
)

// Unknown marks a value (or part of one) that static analysis could not resolve.
const Unknown = "*"

// Interaction kinds: whether the caller waits for the other side.
const (
	Sync  = "sync"
	Async = "async"
)

// Manifest is the per-service output of `codegraph analyze`.
type Manifest struct {
	Codegraph  string            `json:"codegraph,omitempty"` // the codegraph version that wrote it
	Service    string            `json:"service"`
	Aliases    []string          `json:"aliases,omitempty"`
	Tags       map[string]string `json:"tags,omitempty"`
	Module     string            `json:"module"`
	Entry      string            `json:"entry"`
	Algorithm  string            `json:"algorithm"`
	Bootstrap  []Step            `json:"bootstrap"`
	Components []Component       `json:"components"`
	Calls      []Call            `json:"calls"`
	Bindings   []Binding         `json:"bindings"`
	Endpoints  []Endpoint        `json:"endpoints"`
	Libraries  []Library         `json:"libraries,omitempty"`
	Journeys   []Journey         `json:"journeys,omitempty"`
	Settings   []string          `json:"settings,omitempty"` // environment variables it reads (env tags, os.Getenv)
	Stats      Stats             `json:"stats"`
	Declared   *Declared         `json:"declared,omitempty"` // described by hand, not analysed
	Library    bool              `json:"library,omitempty"`  // a module without a main, analysed through its exported API
}

// Declared marks the manifest of a service described by hand (a declared/
// <service>.json file), for services codegraph cannot analyse, such as Python ones.
// Its Module is the repository's name, and its endpoints and journeys are as
// written there.
type Declared struct {
	Repository  string   `json:"repository"`
	Language    string   `json:"language,omitempty"`
	Commit      string   `json:"commit,omitempty"` // the commit the description was read at
	Description string   `json:"description,omitempty"`
	Workloads   []string `json:"workloads,omitempty"` // the deployment's workloads that run it
	File        string   `json:"file,omitempty"`
}

// Stats summarises the size of the analysed program.
type Stats struct {
	ReachableFuncs int `json:"reachableFuncs"`
	ModuleFuncs    int `json:"moduleFuncs"`
	Components     int `json:"components"`
}

// Step is one noteworthy call made while bootstrapping the service from main().
type Step struct {
	Order    int    `json:"order"`
	Depth    int    `json:"depth"`
	Kind     string `json:"kind"` // construct | register | start | shutdown | signal | call | command
	Call     string `json:"call"`
	Command  string `json:"command,omitempty"` // the CLI command whose function the step belongs to; "" for main
	Package  string `json:"package,omitempty"`
	Caller   string `json:"caller"`
	Position string `json:"position,omitempty"`
	Async    bool   `json:"async,omitempty"`
	Deferred bool   `json:"deferred,omitempty"`
}

// Component is a module package that is reachable from the entrypoint.
type Component struct {
	Package    string         `json:"package"`
	Name       string         `json:"name"`
	Layer      string         `json:"layer"`
	Funcs      int            `json:"funcs"`
	Interfaces []Interface    `json:"interfaces,omitempty"`
	Types      []ConcreteType `json:"types,omitempty"`
}

// Interface is a contract declared by a component, with the concrete types
// that implement it *at runtime* (per RTA), not merely in the source tree.
type Interface struct {
	Name            string   `json:"name"`
	Methods         []string `json:"methods"`
	Implementations []string `json:"implementations,omitempty"`
}

// ConcreteType is a named type whose methods are reachable.
type ConcreteType struct {
	Name    string   `json:"name"`
	Methods []string `json:"methods"`
}

// Call aggregates call-graph edges between two components.
type Call struct {
	From         string   `json:"from"`
	To           string   `json:"to"`
	Count        int      `json:"count"`
	ViaInterface bool     `json:"viaInterface,omitempty"`
	Symbols      []string `json:"symbols"`
}

// Binding records a concrete type being converted to an interface, which is
// how dependency injection shows up in SSA (MakeInterface instructions).
type Binding struct {
	Interface string   `json:"interface"`
	Concrete  string   `json:"concrete"`
	Sites     int      `json:"sites"`
	Positions []string `json:"positions,omitempty"`
}

// Endpoint is something the service exposes or consumes over a transport.
type Endpoint struct {
	ID        string    `json:"id"`
	Transport string    `json:"transport"` // http | grpc | kafka | nats | amqp | sql | redis | ...
	Role      string    `json:"role"`      // server | client | producer | consumer | listener | query | connect | dial
	Direction Direction `json:"direction"`
	Kind      string    `json:"kind,omitempty"`     // sync | async; empty = DefaultKind(transport, role)
	Contract  string    `json:"contract,omitempty"` // gRPC: proto-qualified service (notify.v1.NotifierService)
	Owner     string    `json:"owner,omitempty"`    // a declared table or key: the repository whose store holds it
	Keys      []string  `json:"keys"`               // route, topic, subject, table, gRPC service... ("*" = unresolved part)
	Message   string    `json:"message,omitempty"`
	Handler   string    `json:"handler,omitempty"`
	Component string    `json:"component"`
	Function  string    `json:"function,omitempty"`
	Position  string    `json:"position,omitempty"`
	Source    string    `json:"source"` // detector | directive
	Rule      string    `json:"rule,omitempty"`
	Count     int       `json:"count"`
	Sites     []Site    `json:"sites,omitempty"` // every call site merged into this endpoint
	Tags      []string  `json:"tags"`            // "<transport>:<key>", used to link services
}

// Site is one place an endpoint is declared or used.
type Site struct {
	Function string `json:"function,omitempty"`
	Position string `json:"position,omitempty"`
	Via      string `json:"via,omitempty"` // the caller a per-caller variant was resolved for
}

// InteractionKind returns ep.Kind, or the default for its transport and role.
func (ep Endpoint) InteractionKind() string {
	if ep.Kind != "" {
		return ep.Kind
	}
	return DefaultKind(ep.Transport, ep.Role)
}

// The endpoints of a library's exported API (Manifest.Library): one inbound
// endpoint per exported function or method, keyed "fts.Index.Search".
const (
	APITransport = "go"
	APIRole      = "api"
	// The exported symbols of a library's internal packages: callable only
	// from the module itself, analysed like the API but hidden by default.
	APIInternalRole = "internal"
)

// The endpoint of a program's own work (one per main package): its journey is
// what main does that no other journey covers, such as a pipeline or a job.
const (
	MainTransport = "main"
	MainRole      = "run"
	MainKey       = "main"
)

// DefaultKind is the interaction kind of an endpoint without an explicit one:
// producers and consumers are async, request/response roles (clients, servers,
// queries) are sync, and custom roles fall back on the transport.
func DefaultKind(transport, role string) string {
	switch role {
	case "producer", "consumer":
		return Async
	case "client", "server", "query", "connect", "dial", "listener":
		return Sync
	}
	switch transport {
	case "http", "grpc", "sql", "cli", "go", "main":
		return Sync
	}
	return Async
}

// EndpointID derives the stable ID of an endpoint from what identifies it
// (transport, role, direction, keys, component and an explicit kind).
func EndpointID(ep Endpoint) string {
	keys := append([]string(nil), ep.Keys...)
	sort.Strings(keys)
	h := fnv.New64a()
	fmt.Fprintf(h, "%s|%s|%s|%s|%s", ep.Transport, ep.Role, ep.Direction, strings.Join(keys, ","), ep.Component)
	if ep.Kind != "" { // keeps IDs of endpoints without a kind unchanged
		fmt.Fprintf(h, "|%s", ep.Kind)
	}
	if ep.Contract != "" { // same for endpoints without a contract
		fmt.Fprintf(h, "|contract=%s", ep.Contract)
	}
	if ep.Owner != "" {
		fmt.Fprintf(h, "|owner=%s", ep.Owner)
	}
	return fmt.Sprintf("ep_%012x", h.Sum64()&0xffffffffffff)
}

// Journey is what one inbound endpoint (an HTTP route, a gRPC method, a
// consumed topic) reaches when it is triggered: the module functions and
// components it runs through, the calls between them and the outbound
// endpoints (other services, brokers, stores) it ends up using.
type Journey struct {
	Entrypoint string            `json:"entrypoint"` // inbound endpoint ID
	Transport  string            `json:"transport"`
	Key        string            `json:"key"` // endpoint key, "Service/Method" for per-method gRPC journeys
	Handler    string            `json:"handler,omitempty"`
	Component  string            `json:"component"` // package of the handler
	Roots      []Site            `json:"roots"`
	Wrapped    []Site            `json:"wrapped,omitempty"` // the handlers roots that are middleware wrap (the innermost)
	Funcs      int               `json:"funcs"`
	Functions  []string          `json:"functions,omitempty"`
	Components []string          `json:"components"`
	Calls      []Call            `json:"calls,omitempty"`
	Outbound   []JourneyOutbound `json:"outbound,omitempty"`
	Async      []string          `json:"async,omitempty"` // goroutines started along the way
	Truncated  bool              `json:"truncated,omitempty"`
	Uses       []SymbolUse       `json:"uses,omitempty"` // the exported API of other modules its code calls
}

// SymbolUse is an exported function or method of another module that a
// journey's code calls: the libraries' API it depends on.
type SymbolUse struct {
	Module   string `json:"module"`
	Package  string `json:"package"`
	Symbol   string `json:"symbol"`             // pkg.Func, (*pkg.T).Method, pkg.Iface.Method
	Position string `json:"position,omitempty"` // its first call site, in the service's repository
	Calls    int    `json:"calls"`              // call sites in the journey's code
}

// JourneyOutbound is an outbound endpoint used by a journey.
type JourneyOutbound struct {
	Endpoint   string `json:"endpoint"`
	Kind       string `json:"kind"`                 // sync | async
	Background bool   `json:"background,omitempty"` // only reached from a goroutine
}

// Library is a third-party module used by reachable module code.
type Library struct {
	Module   string   `json:"module"`
	Packages []string `json:"packages"`
	UsedBy   []string `json:"usedBy"`
	Calls    int      `json:"calls"`
	Symbols  []string `json:"symbols,omitempty"` // its exported functions and methods the service's code calls
}

// System is the output of `codegraph link`.
type System struct {
	Services []SystemService `json:"services"`
	Links    []Link          `json:"links"`
	Dangling []Dangling      `json:"dangling"`
	Journeys []SystemJourney `json:"journeys,omitempty"`

	// Continuations maps "service|outbound endpoint ID" to where that endpoint
	// leads, one level at a time, for renderers that expand journeys on
	// demand. Not serialised: Journeys carries the chains, expanded.
	Continuations map[string][]Continuation `json:"-"`
}

// Continuation is where an outbound endpoint leads: a link into another
// service and the keys of the journeys there it continues into (none when the
// target endpoint has no journey).
type Continuation struct {
	Link     Link     `json:"link"`
	Journeys []string `json:"journeys,omitempty"`
}

// SystemService is a service node in the system graph.
type SystemService struct {
	Name     string            `json:"name"`
	Module   string            `json:"module,omitempty"` // services of one module are mains of one repository
	Aliases  []string          `json:"aliases,omitempty"`
	Tags     map[string]string `json:"tags,omitempty"`
	Inbound  int               `json:"inbound"`
	Outbound int               `json:"outbound"`
	Library  bool              `json:"library,omitempty"` // analysed through its exported API; never linked
}

// Link connects an outbound endpoint of one service to an inbound endpoint of
// another (or two services sharing a store).
type Link struct {
	From         string `json:"from"`
	To           string `json:"to"`
	Transport    string `json:"transport"`
	Key          string `json:"key"`
	Kind         string `json:"kind"`       // sync | async | shared-store
	Confidence   string `json:"confidence"` // exact | pattern | path-only | name-only
	FromEndpoint string `json:"fromEndpoint,omitempty"`
	ToEndpoint   string `json:"toEndpoint,omitempty"`
}

// Dangling is an endpoint with no counterpart among the linked services
// (an external API, a topic produced by a system that was not analysed, ...).
type Dangling struct {
	Service   string    `json:"service"`
	Transport string    `json:"transport"`
	Role      string    `json:"role"`
	Direction Direction `json:"direction"`
	Key       string    `json:"key"`
	Endpoint  string    `json:"endpoint"`
	Reason    string    `json:"reason,omitempty"` // why it is not an orphan; empty when nothing analysed answers it
	// Partial: the endpoint links through some of its keys (a consumer of
	// several queues), and Key lists the others, those sharing this Reason.
	Partial bool `json:"partial,omitempty"`
}

// Reasons an unmatched endpoint is not an orphan.
const (
	// DanglingSameService: its counterpart is in the service itself (a retry
	// topic it publishes and consumes); links never join a service to itself.
	DanglingSameService = "same-service"
	// DanglingBroker: a retry, delay or dead-letter queue (<queue>.retry,
	// .dlq, .delay.<ttl>) that the broker library feeds itself.
	DanglingBroker = "broker"
	// DanglingOtherTransport: the same name has a counterpart on another
	// messaging transport, e.g. a topic moving from Kafka to RabbitMQ.
	DanglingOtherTransport = "other-transport"
)

// SystemJourney follows a service journey across the services it reaches.
type SystemJourney struct {
	Service    string `json:"service"`
	Entrypoint string `json:"entrypoint"`
	Transport  string `json:"transport"`
	Key        string `json:"key"`
	Kind       string `json:"kind"`
	Handler    string `json:"handler,omitempty"`
	Hops       []Hop  `json:"hops"`
}

// Hop is one outbound call of a journey: to another analysed service (whose
// journey continues in Next) or to something outside the system (External).
type Hop struct {
	From         string `json:"from"`
	To           string `json:"to,omitempty"` // target service; empty when External
	External     bool   `json:"external,omitempty"`
	Transport    string `json:"transport"`
	Role         string `json:"role"`
	Key          string `json:"key"`
	Message      string `json:"message,omitempty"`
	Kind         string `json:"kind"`
	Background   bool   `json:"background,omitempty"`
	Confidence   string `json:"confidence,omitempty"`
	FromEndpoint string `json:"fromEndpoint"`
	ToEndpoint   string `json:"toEndpoint,omitempty"`
	Journey      string `json:"journey,omitempty"` // key of the target journey
	Handler      string `json:"handler,omitempty"`
	Cycle        bool   `json:"cycle,omitempty"`
	Repeated     bool   `json:"repeated,omitempty"` // the journey was followed earlier in this chain; not listed again
	Truncated    bool   `json:"truncated,omitempty"`
	Next         []Hop  `json:"next,omitempty"`
}
