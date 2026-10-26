// Package deploy reads what an environment runs from its rendered
// Kubernetes objects (`helm template` output): the workloads with their
// images, commands, ports and configuration, the Services that name them,
// and the routes that expose them. It matches workloads to repositories (by
// image name) and to analysed services (by command).
//
// Secret objects are skipped and secret values are never read: an
// environment variable from a Secret is listed by name only.
package deploy

import (
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Secret is the value shown for anything that comes from a secret.
const Secret = "(secret)"

// Deployment is one environment.
type Deployment struct {
	Environment string     `json:"environment"`
	Source      string     `json:"source"`
	Workloads   []Workload `json:"workloads"`
	Services    []Service  `json:"services"`
	Routes      []Route    `json:"routes,omitempty"`
	Network     *Network   `json:"network,omitempty"` // with a run: its links against the configured addresses
}

// Workload is a Deployment, StatefulSet, DaemonSet, CronJob or Job.
type Workload struct {
	Kind       string      `json:"kind"`
	Name       string      `json:"name"`
	Replicas   *int        `json:"replicas,omitempty"`
	Schedule   string      `json:"schedule,omitempty"`
	Containers []Container `json:"containers"`
	Services   []string    `json:"services,omitempty"` // the Services selecting its pods
	labels     map[string]string
}

// Container is one container of a workload.
type Container struct {
	Name    string            `json:"name"`
	Init    bool              `json:"init,omitempty"`
	Image   string            `json:"image"` // without registry and tag: "payments-orders"
	Tag     string            `json:"tag,omitempty"`
	Command []string          `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Ports   []int             `json:"ports,omitempty"`
	Env     map[string]string `json:"env,omitempty"`     // literal and ConfigMap values; Secret where a secret is involved
	Secrets []string          `json:"secrets,omitempty"` // variables (or whole Secrets, "secret:<name>") taken from Secrets
	// Filled by Match, for main containers.
	Repo      string   `json:"repo,omitempty"`      // the repository its image is built from
	Built     bool     `json:"built,omitempty"`     // Repo's CI builds the image (the inventory's images), rather than a name match
	AlsoBuilt []string `json:"alsoBuilt,omitempty"` // other repositories whose CI builds the same image
	Service   string   `json:"service,omitempty"`   // the analysed service (main) it runs
	Match     string   `json:"match,omitempty"`     // how Service was chosen, or why it was not
}

// Service is a Kubernetes Service: the host name other workloads use.
type Service struct {
	Name      string            `json:"name"`
	Ports     []Port            `json:"ports,omitempty"`
	Selector  map[string]string `json:"selector,omitempty"`
	Workloads []string          `json:"workloads,omitempty"`
}

// Port is a Service port.
type Port struct {
	Name   string `json:"name,omitempty"`
	Port   int    `json:"port"`
	Target string `json:"target,omitempty"`
}

// Route exposes Services: an HTTPRoute or an Ingress.
type Route struct {
	Kind     string   `json:"kind"`
	Name     string   `json:"name"`
	Hosts    []string `json:"hosts,omitempty"`
	Paths    []string `json:"paths,omitempty"`
	Backends []string `json:"backends,omitempty"`
}

// Read parses rendered Kubernetes objects (a multi-document YAML stream).
func Read(r io.Reader, environment, source string) (*Deployment, error) {
	d := &Deployment{Environment: environment, Source: source}
	configMaps := map[string]map[string]string{}
	var pods []podSpec
	dec := yaml.NewDecoder(r)
	for {
		var node yaml.Node
		err := dec.Decode(&node)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("deploy: %w", err)
		}
		obj := asMap(value(&node))
		if obj == nil {
			continue
		}
		kind, name := str(obj["kind"]), str(dig(obj, "metadata", "name"))
		switch kind {
		case "ConfigMap":
			data := map[string]string{}
			for k, v := range asMap(obj["data"]) {
				data[k] = masked(scalar(v))
			}
			configMaps[name] = data
		case "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob":
			w := Workload{Kind: kind, Name: name}
			spec := asMap(obj["spec"])
			if kind == "CronJob" {
				w.Schedule = str(spec["schedule"])
				spec = asMap(dig(spec, "jobTemplate", "spec"))
			}
			if n, ok := spec["replicas"].(int); ok {
				w.Replicas = &n
			}
			tmpl := asMap(spec["template"])
			w.labels = strMap(dig(tmpl, "metadata", "labels"))
			pods = append(pods, podSpec{w: len(d.Workloads), spec: asMap(tmpl["spec"])})
			d.Workloads = append(d.Workloads, w)
		case "Service":
			s := Service{Name: name, Selector: strMap(dig(obj, "spec", "selector"))}
			for _, p := range asList(dig(obj, "spec", "ports")) {
				pm := asMap(p)
				port, _ := pm["port"].(int)
				s.Ports = append(s.Ports, Port{Name: str(pm["name"]), Port: port, Target: scalar(pm["targetPort"])})
			}
			d.Services = append(d.Services, s)
		case "HTTPRoute":
			rt := Route{Kind: kind, Name: name}
			for _, h := range asList(dig(obj, "spec", "hostnames")) {
				rt.Hosts = append(rt.Hosts, str(h))
			}
			for _, rule := range asList(dig(obj, "spec", "rules")) {
				for _, m := range asList(asMap(rule)["matches"]) {
					if p := str(dig(asMap(m), "path", "value")); p != "" {
						rt.Paths = append(rt.Paths, p)
					}
				}
				for _, b := range asList(asMap(rule)["backendRefs"]) {
					rt.Backends = append(rt.Backends, str(asMap(b)["name"]))
				}
			}
			d.Routes = append(d.Routes, rt.sorted())
		case "Ingress":
			rt := Route{Kind: kind, Name: name}
			for _, rule := range asList(dig(obj, "spec", "rules")) {
				rm := asMap(rule)
				if h := str(rm["host"]); h != "" {
					rt.Hosts = append(rt.Hosts, h)
				}
				for _, p := range asList(dig(rm, "http", "paths")) {
					pm := asMap(p)
					if v := str(pm["path"]); v != "" {
						rt.Paths = append(rt.Paths, v)
					}
					rt.Backends = append(rt.Backends, str(dig(pm, "backend", "service", "name")))
				}
			}
			d.Routes = append(d.Routes, rt.sorted())
		}
	}
	for _, p := range pods {
		d.Workloads[p.w].Containers = containers(p.spec, configMaps)
	}
	// Services select workloads by their pod labels.
	for i := range d.Services {
		s := &d.Services[i]
		for j := range d.Workloads {
			w := &d.Workloads[j]
			if len(s.Selector) > 0 && selects(s.Selector, w.labels) {
				s.Workloads = append(s.Workloads, w.Name)
				w.Services = append(w.Services, s.Name)
			}
		}
	}
	sort.Slice(d.Workloads, func(i, j int) bool {
		a, b := d.Workloads[i], d.Workloads[j]
		return a.Name < b.Name || (a.Name == b.Name && a.Kind < b.Kind)
	})
	sort.Slice(d.Services, func(i, j int) bool { return d.Services[i].Name < d.Services[j].Name })
	sort.Slice(d.Routes, func(i, j int) bool {
		return d.Routes[i].Name < d.Routes[j].Name || (d.Routes[i].Name == d.Routes[j].Name && d.Routes[i].Kind < d.Routes[j].Kind)
	})
	for i := range d.Workloads {
		sort.Strings(d.Workloads[i].Services)
	}
	return d, nil
}

type podSpec struct {
	w    int
	spec map[string]any
}

// containers reads a pod spec's containers, init containers first.
func containers(spec map[string]any, configMaps map[string]map[string]string) []Container {
	var out []Container
	for _, group := range []struct {
		key  string
		init bool
	}{{"initContainers", true}, {"containers", false}} {
		for _, c := range asList(spec[group.key]) {
			cm := asMap(c)
			image, tag := splitImage(str(cm["image"]))
			ct := Container{Name: str(cm["name"]), Init: group.init, Image: image, Tag: tag,
				Command: strList(cm["command"]), Args: strList(cm["args"])}
			for _, p := range asList(cm["ports"]) {
				if n, ok := asMap(p)["containerPort"].(int); ok {
					ct.Ports = append(ct.Ports, n)
				}
			}
			env := map[string]string{}
			// envFrom first: env entries take precedence, as in Kubernetes.
			for _, from := range asList(cm["envFrom"]) {
				fm := asMap(from)
				prefix := str(fm["prefix"])
				if ref := str(dig(fm, "configMapRef", "name")); ref != "" {
					for k, v := range configMaps[ref] {
						env[prefix+k] = v
					}
				}
				if ref := str(dig(fm, "secretRef", "name")); ref != "" {
					ct.Secrets = append(ct.Secrets, "secret:"+ref)
				}
			}
			for _, e := range asList(cm["env"]) {
				em := asMap(e)
				name := str(em["name"])
				switch {
				case em["value"] != nil:
					env[name] = masked(scalar(em["value"]))
				case dig(em, "valueFrom", "configMapKeyRef") != nil:
					ref := asMap(dig(em, "valueFrom", "configMapKeyRef"))
					if v, ok := configMaps[str(ref["name"])][str(ref["key"])]; ok {
						env[name] = v
					}
				case dig(em, "valueFrom", "secretKeyRef") != nil:
					delete(env, name)
					ct.Secrets = append(ct.Secrets, name)
				}
			}
			if len(env) > 0 {
				ct.Env = env
			}
			sort.Strings(ct.Secrets)
			out = append(out, ct)
		}
	}
	return out
}

// Mains returns the workload's main containers: those that are not init
// containers or known sidecars (or, failing that, the first that is not an
// init container). A pod can run several services (a repository's v1 and v2).
func (w *Workload) Mains() []*Container {
	var mains []*Container
	var first *Container
	for i := range w.Containers {
		c := &w.Containers[i]
		if c.Init {
			continue
		}
		if !sidecar(c.Image) {
			mains = append(mains, c)
		} else if first == nil {
			first = c
		}
	}
	if len(mains) == 0 && first != nil {
		mains = []*Container{first}
	}
	return mains
}

// sidecar reports images that run next to a service, not as one.
func sidecar(image string) bool {
	base := path.Base(image)
	for _, s := range []string{"fluentd", "fluent-bit", "linkerd", "envoy", "otel-collector", "datadog-agent", "alloy"} {
		if base == s || strings.HasPrefix(base, s+"-") || strings.HasPrefix(base, s+":") {
			return true
		}
	}
	return strings.HasSuffix(base, "py-profiler")
}

// splitImage drops the registry and the tag (or digest) of an image reference.
func splitImage(ref string) (image, tag string) {
	if i := strings.Index(ref, "@"); i >= 0 {
		ref, tag = ref[:i], ref[i+1:]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref, tag = ref[:i], ref[i+1:]
	}
	if first, rest, ok := strings.Cut(ref, "/"); ok && strings.ContainsAny(first, ".:") {
		ref = rest // a registry host
	}
	return ref, tag
}

func selects(selector, labels map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// masked hides values that carry a secret: the placeholder of masked
// secret values files, or sops ciphertext.
func masked(v string) string {
	if strings.Contains(v, Secret) || strings.Contains(v, "ENC[") {
		return Secret
	}
	return v
}

func (r Route) sorted() Route {
	r.Hosts, r.Paths, r.Backends = uniq(r.Hosts), uniq(r.Paths), uniq(r.Backends)
	return r
}

func uniq(xs []string) []string {
	sort.Strings(xs)
	var out []string
	for i, x := range xs {
		if x != "" && (i == 0 || x != xs[i-1]) {
			out = append(out, x)
		}
	}
	return out
}

// value converts a YAML node. A key repeated in a mapping keeps its last
// value, as Kubernetes does (rendered ConfigMaps repeat keys).
func value(n *yaml.Node) any {
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil
		}
		return value(n.Content[0])
	case yaml.MappingNode:
		m := map[string]any{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Value == "<<" { // merge key
				for mk, mv := range asMap(value(v)) {
					if _, ok := m[mk]; !ok {
						m[mk] = mv
					}
				}
				continue
			}
			m[k.Value] = value(v)
		}
		return m
	case yaml.SequenceNode:
		l := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			l = append(l, value(c))
		}
		return l
	case yaml.AliasNode:
		return value(n.Alias)
	default:
		var v any
		if err := n.Decode(&v); err != nil {
			return n.Value
		}
		return v
	}
}

// Helpers over decoded YAML.

func dig(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// scalar renders a YAML scalar as a string ("8080", "true").
func scalar(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case int:
		return strconv.Itoa(x)
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return fmt.Sprint(x)
	}
}

func strMap(v any) map[string]string {
	m := asMap(v)
	if len(m) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, x := range m {
		out[k] = scalar(x)
	}
	return out
}

func strList(v any) []string {
	var out []string
	for _, x := range asList(v) {
		out = append(out, scalar(x))
	}
	return out
}
