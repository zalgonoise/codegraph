package deploy

import (
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/zalgonoise/codegraph/internal/model"
)

// Network compares the direct links found in the code (gRPC and HTTP) with
// the addresses an environment configures: a setting naming a Kubernetes
// Service (an address setting: *HOST, *ADDR, *URL, …, one host) is a call from
// the services of the container that has it to those the Service's workloads
// run. Messaging links go through a broker, so no setting names their other
// side.
type Network struct {
	Confirmed  []Pair    `json:"confirmed,omitempty"`  // linked in the code, and configured
	CodeOnly   []Pair    `json:"codeOnly,omitempty"`   // linked in the code between deployed services, not configured
	ConfigOnly []Pair    `json:"configOnly,omitempty"` // configured, no link in the code
	Settled    []Settled `json:"settled,omitempty"`    // a contract served by several services: the one configured
	Mocked     []Mock    `json:"mocked,omitempty"`     // a mock switched on, naming a service the code calls
}

// Pair is a caller and a callee: through the code's link keys, the settings
// naming the callee's Service, or both.
type Pair struct {
	From     string   `json:"from"`
	To       string   `json:"to"`
	Keys     []string `json:"keys,omitempty"`     // the code's link keys (gRPC services)
	Settings []string `json:"settings,omitempty"` // "NAME=value"
	// Unread lists, for a configured pair with no link, the settings none of
	// the caller's code reads (by the run's manifest Settings): stale, or
	// read in a way the analysis does not see. Read ones mean a client built
	// but never seen called.
	Unread  []string `json:"unread,omitempty"`
	Checked bool     `json:"checked,omitempty"` // Unread was computed: the run recorded the caller's settings
}

// Settled is a caller whose code links one contract to several services,
// and the one its configuration dials.
type Settled struct {
	From       string   `json:"from"`
	Key        string   `json:"key"`
	To         string   `json:"to"`
	NotDialled []string `json:"notDialled"`
	Setting    string   `json:"setting"`
}

// Mock is a mock switched on in a caller, whose name names a service the
// code calls: if the setting gates those calls, the link is not taken here.
type Mock struct {
	From    string   `json:"from"`
	Setting string   `json:"setting"`
	To      []string `json:"to"`
}

var (
	addressKey = regexp.MustCompile(`(?i)(HOST|ADDR|URL|URI|ENDPOINT|TARGET|SERVER)`)
	mockWords  = regexp.MustCompile(`(?i)(_?MOCKED|_?MOCK|GRPC_)`)
)

// Check compares the deployment with the run's links. Only main containers
// matched to an analysed service count. reads gives each service's settings
// (its manifest's), to tell configured pairs without a link apart.
func (d *Deployment) Check(links []model.Link, reads map[string][]string) *Network {
	services := map[string]*Service{}
	for i := range d.Services {
		services[d.Services[i].Name] = &d.Services[i]
	}
	byName := map[string]*Workload{}
	for i := range d.Workloads {
		byName[d.Workloads[i].Name] = &d.Workloads[i]
	}
	deployed := map[string]bool{}
	own := map[string]map[string]bool{} // service -> Kubernetes Services naming its workloads
	for i := range d.Workloads {
		w := &d.Workloads[i]
		for _, c := range w.Mains() {
			if c.Service == "" {
				continue
			}
			deployed[c.Service] = true
			if own[c.Service] == nil {
				own[c.Service] = map[string]bool{}
			}
			for _, s := range w.Services {
				own[c.Service][s] = true
			}
		}
	}

	// What each analysed service's containers dial, by their settings.
	config := map[[2]string][]string{} // caller, callee -> settings
	mocks := map[string][]string{}     // caller -> mock settings switched on
	for i := range d.Workloads {
		w := &d.Workloads[i]
		for _, c := range w.Mains() {
			from := c.Service
			if from == "" {
				continue
			}
			for _, k := range sortedKeys(c.Env) {
				v := c.Env[k]
				if strings.Contains(strings.ToUpper(k), "MOCK") && strings.EqualFold(v, "true") {
					mocks[from] = uniq(append(mocks[from], k))
					continue
				}
				if !addressKey.MatchString(k) || strings.Contains(strings.ToUpper(k), "PROXY") || strings.Contains(v, ",") {
					continue
				}
				host, port := splitAddress(v)
				if port == 0 {
					port = portSetting(c.Env, k)
				}
				s := services[host]
				if s == nil || own[from][host] {
					continue
				}
				for _, to := range s.runs(byName, port) {
					if to != from {
						key := [2]string{from, to}
						config[key] = uniq(append(config[key], k+"="+v))
					}
				}
			}
		}
	}

	code := map[[2]string][]string{} // caller, callee -> link keys
	targets := map[[2]string][]string{}
	for _, l := range links {
		if (l.Transport != "grpc" && l.Transport != "http") || l.From == l.To {
			continue // messaging goes through a broker, and stores are shared: no setting names the other side
		}
		key := [2]string{l.From, l.To}
		code[key] = uniq(append(code[key], l.Key))
		if l.Transport == "grpc" {
			targets[[2]string{l.From, l.Key}] = uniq(append(targets[[2]string{l.From, l.Key}], l.To))
		}
	}

	n := &Network{}
	settledAway := map[[2]string]bool{}
	for fk, tos := range targets {
		if len(tos) < 2 || !deployed[fk[0]] {
			continue
		}
		var dialled []string
		for _, to := range tos {
			if len(config[[2]string{fk[0], to}]) > 0 {
				dialled = append(dialled, to)
			}
		}
		if len(dialled) != 1 {
			continue
		}
		st := Settled{From: fk[0], Key: fk[1], To: dialled[0], Setting: config[[2]string{fk[0], dialled[0]}][0]}
		for _, to := range tos {
			if to != dialled[0] {
				st.NotDialled = append(st.NotDialled, to)
				settledAway[[2]string{fk[0], to}] = true
			}
		}
		n.Settled = append(n.Settled, st)
	}
	for key, keys := range code {
		if !deployed[key[0]] || !deployed[key[1]] {
			continue
		}
		p := Pair{From: key[0], To: key[1], Keys: keys, Settings: config[key]}
		switch {
		case len(config[key]) > 0:
			n.Confirmed = append(n.Confirmed, p)
		case !settledAway[key]:
			n.CodeOnly = append(n.CodeOnly, p)
		}
	}
	for key, settings := range config {
		if len(code[key]) > 0 {
			continue
		}
		p := Pair{From: key[0], To: key[1], Settings: settings}
		if read, ok := reads[key[0]]; ok {
			p.Checked = true
			for _, s := range settings {
				if name, _, _ := strings.Cut(s, "="); !slices.Contains(read, name) {
					p.Unread = append(p.Unread, s)
				}
			}
		}
		n.ConfigOnly = append(n.ConfigOnly, p)
	}
	for from, ms := range mocks {
		for _, m := range ms {
			name := norm(mockWords.ReplaceAllString(m, ""))
			var to []string
			for key := range code {
				if key[0] != from {
					continue
				}
				repo, _, _ := strings.Cut(key[1], "-")
				if r := norm(repo); len(r) >= 4 && strings.Contains(name, r) {
					to = append(to, key[1])
				}
			}
			if len(to) > 0 {
				n.Mocked = append(n.Mocked, Mock{From: from, Setting: m + "=true", To: uniq(to)})
			}
		}
	}
	sortPairs(n.Confirmed)
	sortPairs(n.CodeOnly)
	sortPairs(n.ConfigOnly)
	sort.Slice(n.Settled, func(i, j int) bool {
		a, b := n.Settled[i], n.Settled[j]
		return a.From < b.From || (a.From == b.From && a.Key < b.Key)
	})
	sort.Slice(n.Mocked, func(i, j int) bool {
		a, b := n.Mocked[i], n.Mocked[j]
		return a.From < b.From || (a.From == b.From && a.Setting < b.Setting)
	})
	return n
}

// runs lists the analysed services a Service reaches: those of the main
// containers of the workloads it selects, narrowed by port when it tells one
// container from another (one pod running a v1 and a v2 behind one Service, on
// ports named grpc-v1 and grpc-v2): the container exposing the port's target,
// or else the containers named like the port.
func (s *Service) runs(workloads map[string]*Workload, port int) []string {
	target, name := 0, "" // the container port behind the Service port (0 when unknown), and the port's name
	for _, p := range s.Ports {
		if p.Port == port {
			name = norm(p.Name)
			if p.Target == "" {
				target = p.Port
			} else {
				target, _ = strconv.Atoi(p.Target)
			}
		}
	}
	var all, byPort, byName []string
	for _, wn := range s.Workloads {
		w := workloads[wn]
		if w == nil {
			continue
		}
		for _, c := range w.Mains() {
			if c.Service == "" {
				continue
			}
			all = append(all, c.Service)
			for _, p := range c.Ports {
				if target != 0 && p == target {
					byPort = append(byPort, c.Service)
				}
			}
			if len(name) >= 3 && (strings.Contains(norm(c.Name), name) || strings.Contains(norm(c.Service), name)) {
				byName = append(byName, c.Service)
			}
		}
	}
	switch {
	case len(byPort) > 0:
		return uniq(byPort)
	case len(byName) > 0:
		return uniq(byName)
	}
	return uniq(all)
}

// portSetting finds the port that goes with a host setting: GRPC_NOTIFY_HOST
// and GRPC_NOTIFY_PORT, or else the one setting with its prefix ending in PORT
// (GRPC_NOTIFY_V2_PORT).
func portSetting(env map[string]string, key string) int {
	for _, suffix := range []string{"HOST", "ADDRESS", "ADDR"} {
		if !strings.HasSuffix(key, suffix) {
			continue
		}
		prefix := strings.TrimSuffix(key, suffix)
		if p, err := strconv.Atoi(env[prefix+"PORT"]); err == nil {
			return p
		}
		var ports []int
		for k, v := range env {
			if strings.HasPrefix(k, prefix) && strings.HasSuffix(k, "PORT") {
				if p, err := strconv.Atoi(v); err == nil {
					ports = append(ports, p)
				}
			}
		}
		if len(ports) == 1 {
			return ports[0]
		}
	}
	return 0
}

// splitAddress reads a host and a port from "host:port", "scheme://host:port/path" or "host".
func splitAddress(v string) (string, int) {
	if _, rest, ok := strings.Cut(v, "://"); ok {
		v = rest
	}
	v, _, _ = strings.Cut(v, "/")
	host, p, _ := strings.Cut(v, ":")
	port, _ := strconv.Atoi(p)
	return strings.ToLower(host), port
}

func sortPairs(ps []Pair) {
	sort.Slice(ps, func(i, j int) bool {
		return ps[i].From < ps[j].From || (ps[i].From == ps[j].From && ps[i].To < ps[j].To)
	})
}
