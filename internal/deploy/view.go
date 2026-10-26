package deploy

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// ReadFile reads a deploy.json written by codegraph deploy.
func ReadFile(file string) (*Deployment, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var d Deployment
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return &d, nil
}

// ServiceView is what an environment runs of one service: its workloads, and
// its direct links checked against the addresses it is configured with. codegraph
// query and the explorer show it.
type ServiceView struct {
	Environment string         `json:"environment"`
	Workloads   []WorkloadView `json:"workloads"`           // none: not deployed there
	Addresses   *Addresses     `json:"addresses,omitempty"` // its direct links against its configured addresses
}

// Addresses compares a service's direct links (gRPC, HTTP) with the
// addresses its deployment configures.
type Addresses struct {
	Confirmed  []Pair    `json:"confirmed,omitempty"`
	CodeOnly   []Pair    `json:"codeOnly,omitempty"`
	ConfigOnly []Pair    `json:"configOnly,omitempty"`
	Settled    []Settled `json:"settled,omitempty"`
	Mocked     []Mock    `json:"mocked,omitempty"`
}

// WorkloadView is one workload running the service.
type WorkloadView struct {
	Name     string            `json:"name"`
	Kind     string            `json:"kind"`
	Schedule string            `json:"schedule,omitempty"`
	Replicas *int              `json:"replicas,omitempty"`
	Command  string            `json:"command,omitempty"`
	Hosts    []string          `json:"hosts,omitempty"`   // Kubernetes Services naming it, with their ports
	Mocks    []string          `json:"mocks,omitempty"`   // *MOCK* variables set to true
	Env      map[string]string `json:"env,omitempty"`     // its configuration; secrets are never read
	Secrets  []string          `json:"secrets,omitempty"` // the variables (or whole Secrets) taken from Secrets, by name
}

// View describes what d runs of service.
func (d *Deployment) View(service string) *ServiceView {
	v := &ServiceView{Environment: d.Environment, Workloads: []WorkloadView{}}
	ports := map[string]string{}
	for _, s := range d.Services {
		var ps []string
		for _, p := range s.Ports {
			ps = append(ps, strconv.Itoa(p.Port))
		}
		ports[s.Name] = s.Name + ":" + strings.Join(ps, ",")
	}
	for i := range d.Workloads {
		w := &d.Workloads[i]
		for _, c := range w.Mains() {
			if c.Service != service {
				continue
			}
			wv := WorkloadView{Name: w.Name, Kind: w.Kind, Schedule: w.Schedule, Replicas: w.Replicas,
				Command: Command(c), Env: c.Env, Secrets: c.Secrets}
			if len(w.Mains()) > 1 {
				wv.Name += " [" + c.Name + "]"
			}
			for _, s := range w.Services {
				wv.Hosts = append(wv.Hosts, ports[s])
			}
			for k, val := range c.Env {
				if strings.Contains(strings.ToUpper(k), "MOCK") && strings.EqualFold(val, "true") {
					wv.Mocks = append(wv.Mocks, k)
				}
			}
			sort.Strings(wv.Mocks)
			v.Workloads = append(v.Workloads, wv)
		}
	}
	if n := d.Network; n != nil && len(v.Workloads) > 0 {
		a := &Addresses{}
		from := func(ps []Pair) []Pair {
			var out []Pair
			for _, p := range ps {
				if p.From == service {
					out = append(out, p)
				}
			}
			return out
		}
		a.Confirmed, a.CodeOnly, a.ConfigOnly = from(n.Confirmed), from(n.CodeOnly), from(n.ConfigOnly)
		for _, st := range n.Settled {
			if st.From == service {
				a.Settled = append(a.Settled, st)
			}
		}
		for _, m := range n.Mocked {
			if m.From == service {
				a.Mocked = append(a.Mocked, m)
			}
		}
		v.Addresses = a
	}
	return v
}
