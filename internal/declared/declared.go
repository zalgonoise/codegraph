// Package declared turns services described by hand (declared/<service>.json)
// into manifests, so services codegraph cannot analyse, such as Python ones, still
// link with the analysed services, start journeys and continue chains.
package declared

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/zalgonoise/codegraph/internal/manifest"
	"github.com/zalgonoise/codegraph/internal/model"
)

// File is one service described by hand.
type File struct {
	Service     string     `json:"service"`
	Repository  string     `json:"repository"`
	Language    string     `json:"language,omitempty"`
	Commit      string     `json:"commit,omitempty"` // the commit the description was read at
	Description string     `json:"description,omitempty"`
	Aliases     []string   `json:"aliases,omitempty"`
	Workloads   []string   `json:"workloads,omitempty"` // the deployment's workloads that run it
	Settings    []string   `json:"settings,omitempty"`  // environment variables its code reads
	Endpoints   []Endpoint `json:"endpoints"`

	Path string `json:"-"` // where it was read from
}

// Endpoint is a declared endpoint, written like the endpoints codegraph detects:
// gRPC servers keyed by service with a proto-qualified contract, gRPC calls
// keyed "Service/Method", HTTP routes "METHOD /path/{param}", HTTP calls
// "METHOD http://host:port/path", topics, queues, tables and Redis key
// patterns, with "*" for what is unknown.
//
// An inbound endpoint lists what it uses, as "<transport> <key>" of the file's
// outbound endpoints ("*" for all of them): Reaches gives it one journey, and
// Methods one journey per gRPC method. A table or key held in another
// service's store names that repository as its Owner, so it is shared with
// that repository's services only, not with every same-named table.
type Endpoint struct {
	Direction model.Direction     `json:"direction"`
	Transport string              `json:"transport"`
	Role      string              `json:"role"`
	Kind      string              `json:"kind,omitempty"`
	Keys      []string            `json:"keys"`
	Contract  string              `json:"contract,omitempty"`
	Owner     string              `json:"owner,omitempty"` // a table or Redis key in another repository's store: that repository
	Message   string              `json:"message,omitempty"`
	Handler   string              `json:"handler,omitempty"`
	Component string              `json:"component,omitempty"` // default: the position's directory, or the repository
	Position  string              `json:"position,omitempty"`  // file:line in the repository, the evidence
	Reaches   []string            `json:"reaches,omitempty"`
	Methods   map[string][]string `json:"methods,omitempty"`
}

// ReadDir reads every declared/<service>.json of dir, in name order.
func ReadDir(dir string) ([]*File, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var out []*File
	for _, p := range paths {
		f, err := Read(p)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// Read reads one file, rejecting unknown fields (a misspelt "reaches" would
// otherwise drop a journey silently).
func Read(p string) (*File, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f File
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	f.Path = p
	return &f, nil
}

// Manifest builds the service's manifest.
func (f *File) Manifest() (*model.Manifest, error) {
	if err := f.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", f.Path, err)
	}
	eps := make([]model.Endpoint, 0, len(f.Endpoints))
	for _, d := range f.Endpoints {
		eps = append(eps, f.endpoint(d))
	}
	m := manifest.Build(manifest.Input{
		Service:   f.Service,
		Aliases:   f.Aliases,
		Module:    f.Repository,
		Endpoints: eps,
		Settings:  slices.Compact(slices.Sorted(slices.Values(f.Settings))),
	})
	m.Declared = &model.Declared{
		Repository: f.Repository, Language: f.Language, Commit: f.Commit, Description: f.Description,
		Workloads: f.Workloads, File: f.Path,
	}
	journeys, err := f.journeys(m.Endpoints)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", f.Path, err)
	}
	m.Journeys = journeys
	return m, nil
}

func (f *File) validate() error {
	if f.Service == "" || f.Repository == "" {
		return errors.New("service and repository are required")
	}
	for i, d := range f.Endpoints {
		what := fmt.Sprintf("endpoint %d (%s %s %v)", i, d.Transport, d.Role, d.Keys)
		switch {
		case d.Direction != model.Inbound && d.Direction != model.Outbound:
			return fmt.Errorf("%s: direction must be %q or %q", what, model.Inbound, model.Outbound)
		case d.Transport == "" || d.Role == "" || len(d.Keys) == 0:
			return fmt.Errorf("%s: transport, role and keys are required", what)
		case d.Kind != "" && d.Kind != model.Sync && d.Kind != model.Async:
			return fmt.Errorf("%s: kind must be %q or %q", what, model.Sync, model.Async)
		case d.Direction == model.Outbound && (len(d.Reaches) > 0 || len(d.Methods) > 0):
			return fmt.Errorf("%s: only inbound endpoints have reaches or methods", what)
		case len(d.Methods) > 0 && (d.Transport != "grpc" || len(d.Reaches) > 0):
			return fmt.Errorf("%s: methods are for gRPC servers, instead of reaches", what)
		case d.Owner != "" && d.Role != "query":
			return fmt.Errorf("%s: only tables and Redis keys (role query) have an owner", what)
		}
	}
	return nil
}

// endpoint converts a declared endpoint, as the detector would have found it.
func (f *File) endpoint(d Endpoint) model.Endpoint {
	component := d.Component
	if component == "" {
		file, _, _ := strings.Cut(d.Position, ":")
		if component = path.Dir(file); file == "" || component == "." {
			component = f.Repository
		}
	}
	ep := model.Endpoint{
		Transport: d.Transport, Role: d.Role, Direction: d.Direction, Kind: d.Kind, Contract: d.Contract, Owner: d.Owner,
		Keys: append([]string(nil), d.Keys...), Message: d.Message, Handler: d.Handler,
		Component: component, Position: d.Position, Source: "declared",
	}
	if d.Position != "" {
		ep.Sites = []model.Site{{Function: d.Handler, Position: d.Position}}
	}
	return ep
}

// journeys gives each inbound endpoint that lists what it uses a journey (one
// per gRPC method with Methods), in the file's order.
func (f *File) journeys(eps []model.Endpoint) ([]model.Journey, error) {
	byRef := map[string]model.Endpoint{} // "<transport> <key>" -> outbound endpoint
	var all []string                     // every outbound endpoint's reference, in the file's order
	for _, ep := range eps {
		if ep.Direction != model.Outbound {
			continue
		}
		for _, k := range ep.Keys {
			byRef[ep.Transport+" "+k] = ep
		}
	}
	for _, d := range f.Endpoints {
		if d.Direction == model.Outbound {
			all = append(all, d.Transport+" "+d.Keys[0])
		}
	}
	var out []model.Journey
	for _, d := range f.Endpoints {
		if d.Direction != model.Inbound || (len(d.Reaches) == 0 && len(d.Methods) == 0) {
			continue
		}
		in := f.endpoint(d)
		sort.Strings(in.Keys)
		in.ID = model.EndpointID(in)
		j := model.Journey{
			Entrypoint: in.ID, Transport: in.Transport, Handler: d.Handler, Component: in.Component,
			Components: []string{in.Component},
		}
		if d.Handler != "" || d.Position != "" {
			j.Roots = []model.Site{{Function: d.Handler, Position: d.Position}}
		}
		uses := func(key string, refs []string) error {
			if len(refs) == 1 && refs[0] == "*" {
				refs = all
			}
			jj := j
			jj.Key = key
			seen := map[string]bool{}
			for _, ref := range refs {
				ep, ok := byRef[ref]
				if !ok {
					return fmt.Errorf("%s %s uses %q, which is no outbound endpoint of the file", d.Transport, key, ref)
				}
				if !seen[ep.ID] {
					seen[ep.ID] = true
					jj.Outbound = append(jj.Outbound, model.JourneyOutbound{Endpoint: ep.ID, Kind: ep.InteractionKind()})
				}
			}
			out = append(out, jj)
			return nil
		}
		if len(d.Methods) > 0 {
			methods := make([]string, 0, len(d.Methods))
			for name := range d.Methods {
				methods = append(methods, name)
			}
			sort.Strings(methods)
			for _, name := range methods {
				if err := uses(d.Keys[0]+"/"+name, d.Methods[name]); err != nil {
					return nil, err
				}
			}
			continue
		}
		if err := uses(strings.Join(in.Keys, ", "), d.Reaches); err != nil {
			return nil, err
		}
	}
	return out, nil
}
