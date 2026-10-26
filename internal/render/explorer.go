package render

import (
	_ "embed"
	"encoding/json"
	"strings"

	"github.com/zalgonoise/codegraph/internal/deploy"
	"github.com/zalgonoise/codegraph/internal/model"
	"github.com/zalgonoise/codegraph/internal/symbols"
)

//go:embed explorer.html
var explorerPage string

// explorerDataMark is replaced by the explorer's data in explorer.html,
// explorerDiagramsMark by the Mermaid sources it draws on demand, and
// explorerSettingsMark by the deployment's settings, shown on demand.
const (
	explorerDataMark     = "/*CODEGRAPH_DATA*/{}"
	explorerDiagramsMark = "/*CODEGRAPH_DIAGRAMS*/{}"
	explorerSettingsMark = "/*CODEGRAPH_SETTINGS*/{}"
)

// Explorer renders the interactive system explorer: one self-contained HTML
// page with the services, their journeys and endpoints, and where each
// journey's outbound endpoints continue (sys.Continuations). The page expands
// journeys across services on demand, so it stays small where system.json,
// which carries every chain expanded, does not.
//
// The system diagram and each service's diagram (the Mermaid of system.mmd
// and service.mmd) ride along as text in an inert script, parsed only when a
// page asks to draw one; Mermaid itself is fetched then, not on load.
//
// With a deployment (dep, nil without one), each service's page and the home
// page say what that environment runs: workloads, hosts, mocks switched on,
// and the address check (deploy.View). The workloads' settings, the bulk of
// it, ride in another inert script read when a page shows them.
func Explorer(sys *model.System, ms []*model.Manifest, dep *deploy.Deployment) (string, error) {
	d := explorerData(sys, ms)
	settings := map[string]exploreSettings{} // service|workload -> its settings
	if dep != nil {
		d.Deploy = &exploreDeploy{Environment: dep.Environment, Source: dep.Source, Services: map[string]*deploy.ServiceView{}}
		if dep.Network != nil {
			d.Deploy.Mocked = dep.Network.Mocked
		}
		for _, m := range ms {
			v := dep.View(m.Service)
			for i := range v.Workloads {
				w := &v.Workloads[i]
				settings[m.Service+"|"+w.Name] = exploreSettings{Env: w.Env, Secrets: w.Secrets}
				w.Env, w.Secrets = nil, nil
			}
			d.Deploy.Services[m.Service] = v
		}
	}
	data, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	settingsJSON, err := json.Marshal(settings)
	if err != nil {
		return "", err
	}
	diagrams := map[string]string{"system": Mermaid(System(sys))}
	for _, m := range ms {
		diagrams["service:"+m.Service] = Mermaid(Service(m, ServiceOptions{}))
		if HasInternalAPI(m) { // drawn when the page shows the internal API
			diagrams["service+internal:"+m.Service] = Mermaid(Service(m, ServiceOptions{Internal: true}))
		}
	}
	sources, err := json.Marshal(diagrams)
	if err != nil {
		return "", err
	}
	// json.Marshal escapes <, > and &, so neither can close its script tag.
	page := strings.Replace(explorerPage, explorerDataMark, string(data), 1)
	page = strings.Replace(page, explorerSettingsMark, string(settingsJSON), 1)
	return strings.Replace(page, explorerDiagramsMark, string(sources), 1), nil
}

// exploreDeploy is what an environment runs of each service of the run (no
// workloads: not deployed there), without the settings.
type exploreDeploy struct {
	Environment string                         `json:"environment"`
	Source      string                         `json:"source,omitempty"`
	Services    map[string]*deploy.ServiceView `json:"services"`
	Mocked      []deploy.Mock                  `json:"mocked,omitempty"` // mocks switched on, naming services the code calls
}

// exploreSettings is one workload's configuration: values, and secrets by name.
type exploreSettings struct {
	Env     map[string]string `json:"env,omitempty"`
	Secrets []string          `json:"secrets,omitempty"`
}

type exploreData struct {
	Services  []exploreService            `json:"services"`
	Journeys  []exploreJourney            `json:"journeys"`
	Endpoints map[string]exploreEndpoint  `json:"endpoints"`         // service|endpoint ID
	Next      map[string][]exploreNext    `json:"next"`              // service|outbound endpoint ID
	Dangling  map[string]string           `json:"dangling"`          // service|endpoint ID -> reason ("" for none)
	Partial   map[string][]explorePartial `json:"partial,omitempty"` // service|endpoint ID -> its unlinked keys, when others link
	Deploy    *exploreDeploy              `json:"deploy,omitempty"`  // what an environment runs, when given
	Links     []model.Link                `json:"links"`
	API       [][2]int                    `json:"api,omitempty"` // [caller journey, library symbol's journey] (model.APICalls)
}

// explorePartial is a partly linked endpoint's keys nothing answers, for one reason.
type explorePartial struct {
	Keys   []string `json:"keys"`
	Reason string   `json:"reason,omitempty"`
}

type exploreService struct {
	Name     string          `json:"name"`
	Module   string          `json:"module,omitempty"`
	Declared *model.Declared `json:"declared,omitempty"` // described by hand, not analysed
	Library  bool            `json:"library,omitempty"`  // its journeys are its exported API's
	Journeys []int           `json:"journeys,omitempty"` // indexes into exploreData.Journeys
}

type exploreJourney struct {
	Service    string                  `json:"service"`
	Entrypoint string                  `json:"entrypoint"`
	Transport  string                  `json:"transport"`
	Key        string                  `json:"key"`
	Handler    string                  `json:"handler,omitempty"`
	Funcs      int                     `json:"funcs"`
	Components []string                `json:"components,omitempty"` // relative to the module
	Outbound   []model.JourneyOutbound `json:"outbound,omitempty"`
	Async      int                     `json:"async,omitempty"` // goroutines started
	Truncated  bool                    `json:"truncated,omitempty"`
}

type exploreEndpoint struct {
	Transport string   `json:"transport"`
	Role      string   `json:"role"`
	Inbound   bool     `json:"inbound,omitempty"`
	Kind      string   `json:"kind"`
	Keys      []string `json:"keys"`
	Contract  string   `json:"contract,omitempty"`
	Message   string   `json:"message,omitempty"`
	Handler   string   `json:"handler,omitempty"`
	Position  string   `json:"position,omitempty"`
}

type exploreNext struct {
	To         string `json:"to"`
	ToEndpoint string `json:"toEndpoint"`
	Key        string `json:"key"`
	Kind       string `json:"kind"`
	Confidence string `json:"confidence"`
	Journeys   []int  `json:"journeys,omitempty"` // the target's journeys it continues into
}

func explorerData(sys *model.System, ms []*model.Manifest) exploreData {
	d := exploreData{
		Endpoints: map[string]exploreEndpoint{},
		Next:      map[string][]exploreNext{},
		Dangling:  map[string]string{},
		Partial:   map[string][]explorePartial{},
		Links:     sys.Links,
	}
	for _, dg := range sys.Dangling {
		if dg.Partial {
			k := dg.Service + "|" + dg.Endpoint
			d.Partial[k] = append(d.Partial[k], explorePartial{Keys: strings.Split(dg.Key, ", "), Reason: dg.Reason})
			continue
		}
		d.Dangling[dg.Service+"|"+dg.Endpoint] = dg.Reason
	}
	index := map[string]int{}     // service|entrypoint|key -> journey index
	first := make([]int, len(ms)) // each manifest's first journey
	for mi, m := range ms {
		first[mi] = len(d.Journeys)
		svc := exploreService{Name: m.Service, Module: m.Module, Declared: m.Declared, Library: m.Library}
		for _, ep := range m.Endpoints {
			d.Endpoints[m.Service+"|"+ep.ID] = exploreEndpoint{
				Transport: ep.Transport, Role: ep.Role, Inbound: ep.Direction == model.Inbound,
				Kind: ep.InteractionKind(), Keys: ep.Keys, Contract: ep.Contract, Message: ep.Message,
				Handler: ep.Handler, Position: ep.Position,
			}
		}
		for _, j := range m.Journeys {
			index[m.Service+"|"+j.Entrypoint+"|"+j.Key] = len(d.Journeys)
			svc.Journeys = append(svc.Journeys, len(d.Journeys))
			var comps []string
			for _, c := range j.Components {
				comps = append(comps, symbols.Short(c, m.Module))
			}
			d.Journeys = append(d.Journeys, exploreJourney{
				Service: m.Service, Entrypoint: j.Entrypoint, Transport: j.Transport, Key: j.Key,
				Handler: j.Handler, Funcs: j.Funcs, Components: comps, Outbound: j.Outbound,
				Async: len(j.Async), Truncated: j.Truncated,
			})
		}
		d.Services = append(d.Services, svc)
	}
	for key, cs := range sys.Continuations {
		for _, c := range cs {
			n := exploreNext{To: c.Link.To, ToEndpoint: c.Link.ToEndpoint, Key: c.Link.Key, Kind: c.Link.Kind, Confidence: c.Link.Confidence}
			for _, jk := range c.Journeys {
				if i, ok := index[c.Link.To+"|"+c.Link.ToEndpoint+"|"+jk]; ok {
					n.Journeys = append(n.Journeys, i)
				}
			}
			d.Next[key] = append(d.Next[key], n)
		}
	}
	seen := map[[2]int]bool{}
	for _, c := range model.APICalls(ms) {
		p := [2]int{first[c.Caller.Manifest] + c.Caller.Journey, first[c.API.Manifest] + c.API.Journey}
		if !seen[p] {
			seen[p] = true
			d.API = append(d.API, p)
		}
	}
	return d
}
