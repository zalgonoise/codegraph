// Package manifest assembles the per-service manifest from the analysis
// stages (merging and tagging endpoints) and reads/writes it as JSON.
package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/zalgonoise/codegraph/internal/components"
	"github.com/zalgonoise/codegraph/internal/model"
)

// Input bundles the outputs of the analysis stages for one service.
type Input struct {
	Codegraph  string // the codegraph version analysing it
	Service    string
	Aliases    []string
	Tags       map[string]string
	Module     string
	Entry      string
	Algorithm  string
	Bootstrap  []model.Step
	Components *components.Result
	Endpoints  []model.Endpoint
	Journeys   []model.Journey
	Settings   []string
	Stats      model.Stats
	Library    bool
}

// Build assembles a manifest.
func Build(in Input) *model.Manifest {
	m := &model.Manifest{
		Codegraph: in.Codegraph,
		Service:   in.Service,
		Aliases:   dedupe(in.Aliases),
		Tags:      in.Tags,
		Module:    in.Module,
		Entry:     in.Entry,
		Algorithm: in.Algorithm,
		Bootstrap: in.Bootstrap,
		Endpoints: mergeEndpoints(in.Endpoints),
		Journeys:  in.Journeys,
		Settings:  in.Settings,
		Stats:     in.Stats,
		Library:   in.Library,
	}
	if len(m.Tags) == 0 {
		m.Tags = nil
	}
	if c := in.Components; c != nil {
		m.Components, m.Calls, m.Bindings, m.Libraries = c.Components, c.Calls, c.Bindings, c.Libraries
	}
	m.Stats.Components = len(m.Components)
	return m
}

// mergeEndpoints collapses endpoints that describe the same thing (e.g. the
// same table queried from several functions), counting the call sites.
func mergeEndpoints(eps []model.Endpoint) []model.Endpoint {
	byID := map[string]int{}
	var out []model.Endpoint
	for _, ep := range eps {
		ep.Keys = append([]string(nil), ep.Keys...)
		sort.Strings(ep.Keys)
		ep.ID = model.EndpointID(ep)
		if i, ok := byID[ep.ID]; ok {
			out[i].Count += max(ep.Count, 1)
			out[i].Sites = append(out[i].Sites, ep.Sites...)
			if out[i].Handler == "" {
				out[i].Handler = ep.Handler
			}
			if out[i].Message == "" {
				out[i].Message = ep.Message
			}
			continue
		}
		ep.Sites = append([]model.Site(nil), ep.Sites...)
		if ep.Count == 0 {
			ep.Count = 1
		}
		ep.Tags = nil
		for _, k := range ep.Keys {
			ep.Tags = append(ep.Tags, ep.Transport+":"+k)
		}
		byID[ep.ID] = len(out)
		out = append(out, ep)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Direction != b.Direction {
			return a.Direction < b.Direction
		}
		if a.Transport != b.Transport {
			return a.Transport < b.Transport
		}
		return strings.Join(a.Keys, ",") < strings.Join(b.Keys, ",")
	})
	return out
}

// WriteJSON writes v as indented JSON.
func WriteJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// Read loads one manifest.
func Read(path string) (*model.Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m model.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if m.Service == "" {
		return nil, fmt.Errorf("%s: not a codegraph manifest (no service name)", path)
	}
	return &m, nil
}

// ReadAll loads several manifests, rejecting duplicate service names.
func ReadAll(paths []string) ([]*model.Manifest, error) {
	seen := map[string]string{}
	var out []*model.Manifest
	for _, p := range paths {
		m, err := Read(p)
		if err != nil {
			return nil, err
		}
		if prev, ok := seen[m.Service]; ok {
			return nil, fmt.Errorf("service %q defined twice (%s and %s); use -service or //codegraph:service", m.Service, prev, p)
		}
		seen[m.Service] = p
		out = append(out, m)
	}
	return out, nil
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
