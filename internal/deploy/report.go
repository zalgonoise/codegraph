package deploy

import (
	"fmt"
	"sort"
	"strings"
)

// Report renders the deployment as Markdown: which analysed services run and
// how, which do not run, what runs that was not analysed, mocks switched on,
// and the public routes. analysed lists the run's services (none without a
// run); languages gives each repository's languages (from the inventory).
func Report(d *Deployment, analysed []string, languages map[string][]string) string {
	var b strings.Builder
	kinds := map[string]int{}
	byService := map[string][]run{}
	byRepo := map[string][]run{} // repositories without an analysed service
	var unknown []run
	var ambiguous []string
	for i := range d.Workloads {
		w := &d.Workloads[i]
		kinds[w.Kind]++
		for _, c := range w.Mains() {
			r := run{w, c}
			switch {
			case c.Service != "":
				byService[c.Service] = append(byService[c.Service], r)
			case c.Repo != "" && c.Match != "repository not analysed":
				ambiguous = append(ambiguous, fmt.Sprintf("%s, repository %s, `%s`: %s", r.label(), c.Repo, command(c), c.Match))
			case c.Repo != "":
				byRepo[c.Repo] = append(byRepo[c.Repo], r)
			default:
				unknown = append(unknown, r)
			}
		}
	}
	declared := ""
	if n := countDeclared(byService); n > 0 {
		declared = fmt.Sprintf(" (%d of them a service declared by hand)", n)
	}
	fmt.Fprintf(&b, "# Deployment: %s\n\nFrom `%s`: %d workloads (%s), %d Services, %d routes. Their main containers: "+
		"%d run an analysed service%s, %d an analysed repository whose main could not be told, %d come from repositories "+
		"without an analysed service, and %d match no repository.\n",
		d.Environment, d.Source, len(d.Workloads), countList(kinds), len(d.Services), len(d.Routes),
		countAll(byService), declared, len(ambiguous), countAll(byRepo), len(unknown))

	if len(analysed) > 0 {
		b.WriteString("\n## Analysed services\n\n| Service | Workloads | Command | Services (host:port) |\n|---|---|---|---|\n")
		var deployed, idle []string
		for _, s := range analysed {
			if len(byService[s]) == 0 {
				idle = append(idle, s)
				continue
			}
			deployed = append(deployed, s)
			for _, r := range byService[s] {
				fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", s, r.label(), code(command(r.c)), strings.Join(hostPorts(d, *r.w), ", "))
			}
		}
		fmt.Fprintf(&b, "\n%d of %d analysed services run in %s. **Not deployed** (no workload matched) (%d): %s.\n",
			len(deployed), len(analysed), d.Environment, len(idle), list(idle))
	}

	if len(ambiguous) > 0 {
		sort.Strings(ambiguous)
		b.WriteString("\n## Workloads whose main could not be told\n\n")
		for _, a := range ambiguous {
			fmt.Fprintf(&b, "- %s\n", a)
		}
	}
	if len(byRepo) > 0 {
		b.WriteString("\n## Repositories running without an analysed service\n\n| Repository | Languages | Workloads |\n|---|---|---|\n")
		for _, repo := range sortedKeys(byRepo) {
			var ws []string
			for _, r := range byRepo[repo] {
				ws = append(ws, r.label())
			}
			fmt.Fprintf(&b, "| %s | %s | %s |\n", repo, strings.Join(languages[repo], ", "), strings.Join(uniq(ws), ", "))
		}
	}
	if len(unknown) > 0 {
		b.WriteString("\n## Workloads matching no repository\n\n")
		for _, r := range unknown {
			fmt.Fprintf(&b, "- %s: image `%s`\n", r.label(), r.c.Image)
		}
	}

	var mocks []string
	for i := range d.Workloads {
		w := &d.Workloads[i]
		for _, c := range w.Mains() {
			for _, k := range sortedKeys(c.Env) {
				if strings.Contains(strings.ToUpper(k), "MOCK") && strings.EqualFold(c.Env[k], "true") {
					mocks = append(mocks, fmt.Sprintf("%s: `%s=true`", run{w, c}.label(), k))
				}
			}
		}
	}
	fmt.Fprintf(&b, "\n## Mocks switched on (%d)\n\n", len(mocks))
	if len(mocks) == 0 {
		b.WriteString("None: no main container sets a `*MOCK*` variable to `true`.\n")
	}
	for _, m := range mocks {
		fmt.Fprintf(&b, "- %s\n", m)
	}

	if n := d.Network; n != nil {
		fmt.Fprintf(&b, "\n## Links and configured addresses\n\nAn address setting (`*HOST`, `*ADDR`, `*URL`, …) naming a "+
			"Kubernetes Service is a call to the services its workloads run; gRPC and HTTP links only (messaging goes through "+
			"a broker). Between services deployed in %s: %d linked pairs are configured too, %d are not; %d configured "+
			"pairs have no link in the code.\n",
			d.Environment, len(n.Confirmed), len(n.CodeOnly), len(n.ConfigOnly))
		if len(n.Settled) > 0 {
			b.WriteString("\n### Contracts served by several services: the one configured\n\n" +
				"| Caller | Contract | Dials | Not dialled | Setting |\n|---|---|---|---|---|\n")
			for _, st := range n.Settled {
				fmt.Fprintf(&b, "| %s | %s | %s | %s | `%s` |\n", st.From, st.Key, st.To, strings.Join(st.NotDialled, ", "), st.Setting)
			}
		}
		if len(n.Mocked) > 0 {
			b.WriteString("\n### Links a mock switched on names (not taken here, if the setting gates them)\n\n")
			for _, m := range n.Mocked {
				fmt.Fprintf(&b, "- %s → %s: `%s`\n", m.From, strings.Join(m.To, ", "), m.Setting)
			}
		}
		pairs := func(title, note string, ps []Pair) {
			if len(ps) == 0 {
				return
			}
			fmt.Fprintf(&b, "\n### %s (%d)\n\n%s\n\n", title, len(ps), note)
			for _, p := range ps {
				line := fmt.Sprintf("- %s → %s", p.From, p.To)
				if len(p.Keys) > 0 {
					line += ": " + strings.Join(p.Keys, ", ")
				}
				if len(p.Settings) > 0 {
					line += " (`" + strings.Join(p.Settings, "`, `") + "`)"
				}
				b.WriteString(line + "\n")
			}
		}
		var read, unread, unchecked []Pair
		for _, p := range n.ConfigOnly {
			switch {
			case !p.Checked:
				unchecked = append(unchecked, p)
			case len(p.Unread) == len(p.Settings):
				unread = append(unread, p)
			default:
				read = append(read, p)
			}
		}
		pairs("Configured, read by the code, no call found", "The caller's code (or a library it uses) reads the setting, "+
			"so it builds the client, but the analysis sees no call: a call it misses, or a client the main never uses "+
			"(configuration shared by several mains).", read)
		pairs("Configured, read by nothing in the caller's code", "No `env` tag or `os.Getenv` of the caller, its client "+
			"libraries or its dependencies names the setting: a stale setting, or one read in a way the analysis does not "+
			"see (a computed name, viper's AutomaticEnv).", unread)
		pairs("Configured, no link in the code", "The run did not record the caller's settings (an older run), so these "+
			"are not told apart.", unchecked)
		pairs("Linked in the code, not configured", "The address comes from elsewhere (a default in the code, a "+
			"setting not named like an address), or the call is not made in this environment.", n.CodeOnly)
	}

	if len(d.Routes) > 0 {
		owner := map[string][]string{} // Service -> what its workloads run
		for _, s := range d.Services {
			for _, wn := range s.Workloads {
				for i := range d.Workloads {
					w := &d.Workloads[i]
					if w.Name != wn {
						continue
					}
					for _, c := range w.Mains() {
						if label := firstOf(c.Service, c.Repo); label != "" {
							owner[s.Name] = append(owner[s.Name], label)
						}
					}
				}
			}
		}
		b.WriteString("\n## Public routes\n\n| Route | Hosts | Paths | Backends (runs) |\n|---|---|---|---|\n")
		for _, r := range d.Routes {
			var backends []string
			for _, be := range r.Backends {
				if o := uniq(owner[be]); len(o) > 0 {
					be += " (" + strings.Join(o, ", ") + ")"
				}
				backends = append(backends, be)
			}
			fmt.Fprintf(&b, "| %s %s | %s | %s | %s |\n", r.Kind, r.Name, strings.Join(r.Hosts, ", "),
				code(strings.Join(r.Paths, " ")), strings.Join(backends, ", "))
		}
	}
	return b.String()
}

// run is a main container of a workload.
type run struct {
	w *Workload
	c *Container
}

// label names the workload, and the container when the workload has several.
func (r run) label() string {
	l := workloadLabel(*r.w)
	if len(r.w.Mains()) > 1 {
		l += " [" + r.c.Name + "]"
	}
	return l
}

func firstOf(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

func workloadLabel(w Workload) string {
	l := w.Name
	if w.Kind != "Deployment" {
		l += " (" + strings.ToLower(w.Kind)
		if w.Schedule != "" {
			l += " `" + w.Schedule + "`"
		}
		l += ")"
	}
	return l
}

// Command is a container's command line.
func Command(c *Container) string {
	return strings.Join(append(append([]string{}, c.Command...), c.Args...), " ")
}

func command(c *Container) string { return Command(c) }

// hostPorts lists the Services naming a workload, with their ports.
func hostPorts(d *Deployment, w Workload) []string {
	var out []string
	for _, name := range w.Services {
		for _, s := range d.Services {
			if s.Name != name {
				continue
			}
			var ports []string
			for _, p := range s.Ports {
				ports = append(ports, fmt.Sprint(p.Port))
			}
			out = append(out, name+":"+strings.Join(ports, ","))
		}
	}
	return out
}

func code(s string) string {
	if s == "" {
		return ""
	}
	return "`" + strings.ReplaceAll(s, "|", `\|`) + "`"
}

func countList(m map[string]int) string {
	var parts []string
	for _, k := range sortedKeys(m) {
		parts = append(parts, fmt.Sprintf("%d %s", m[k], k))
	}
	return strings.Join(parts, ", ")
}

func countAll(m map[string][]run) int {
	n := 0
	for _, ws := range m {
		n += len(ws)
	}
	return n
}

func list(xs []string) string {
	if len(xs) == 0 {
		return "none"
	}
	return strings.Join(xs, ", ")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func countDeclared(byService map[string][]run) int {
	n := 0
	for _, rs := range byService {
		for _, r := range rs {
			if r.c.Match == "declared" {
				n++
			}
		}
	}
	return n
}
