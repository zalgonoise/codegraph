package deploy

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zalgonoise/codegraph/internal/model"
)

const rendered = `
apiVersion: v1
kind: ConfigMap
metadata:
  name: shop-config
data:
  PORT: "8080"
  STOCK_ADDRESS: "stock:8082"
  STOCK_MOCKED: "false"
  PAYMENTS_MOCKED: "true"
  PORT: "8081"
  TOKEN: "(secret)"
---
apiVersion: v1
kind: Secret
metadata:
  name: shop-secrets
data:
  password: c2VjcmV0
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: shop
spec:
  replicas: 2
  template:
    metadata:
      labels:
        app: shop
    spec:
      initContainers:
        - name: migrations
          image: registry.example.com/team-shop:v1
          command: ["/usr/bin/app", "migrate"]
      containers:
        - name: fluentd
          image: fluentd:v1.16
        - name: shop
          image: registry.example.com/team-shop:v1
          command: ["/usr/bin/app", "api"]
          ports:
            - containerPort: 8081
          envFrom:
            - configMapRef:
                name: shop-config
            - secretRef:
                name: shop-secrets
          env:
            - name: MODE
              value: live
            - name: STOCK_ADDRESS
              valueFrom:
                configMapKeyRef:
                  name: shop-config
                  key: STOCK_ADDRESS
            - name: DB_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: shop-secrets
                  key: password
---
apiVersion: v1
kind: Service
metadata:
  name: shop
spec:
  selector:
    app: shop
  ports:
    - name: http
      port: 80
      targetPort: 8081
---
apiVersion: batch/v1
kind: CronJob
metadata:
  name: stock-sync
spec:
  schedule: "0 3 * * *"
  jobTemplate:
    spec:
      template:
        spec:
          containers:
            - name: sync
              image: registry.example.com/team-stock@sha256:abc
              command: ["/bin/sync_job"]
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: stock
spec:
  template:
    metadata:
      labels:
        app: stock
    spec:
      containers:
        - name: stock
          image: registry.example.com/team-stock:v2
          command: ["/bin/grpc"]
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: legacy-admin
spec:
  template:
    spec:
      containers:
        - name: admin
          image: registry.example.com/team-hub:v1
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: notify
spec:
  template:
    spec:
      containers:
        - name: notify-v1
          image: registry.example.com/team-notify:v3
          command: ["/bin/service/grpc/v1"]
        - name: notify-v2
          image: registry.example.com/team-notify:v3
          command: ["/bin/service/grpc/v2"]
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: exporter
spec:
  template:
    spec:
      containers:
        - name: exporter
          image: prom/statsd-exporter:v0.26
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: shop-public
spec:
  hostnames: [www.example.com]
  rules:
    - matches:
        - path: {type: PathPrefix, value: /shop}
      backendRefs:
        - name: shop
          port: 80
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: shop-ingress
spec:
  rules:
    - host: api.example.com
      http:
        paths:
          - path: /shop
            backend:
              service:
                name: shop
`

func TestRead(t *testing.T) {
	d, err := Read(strings.NewReader(rendered), "production", "chart")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, w := range d.Workloads {
		names = append(names, w.Kind+" "+w.Name)
	}
	if want := []string{"Deployment exporter", "Deployment legacy-admin", "Deployment notify", "Deployment shop", "Deployment stock", "CronJob stock-sync"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("workloads = %v", names)
	}
	if notify := d.Workloads[2].Mains(); len(notify) != 2 || notify[1].Name != "notify-v2" {
		t.Errorf("a pod with two services has two main containers: %+v", notify)
	}
	shop := d.Workloads[3]
	if shop.Replicas == nil || *shop.Replicas != 2 || !reflect.DeepEqual(shop.Services, []string{"shop"}) {
		t.Errorf("shop: %+v", shop)
	}
	mains := shop.Mains()
	if len(mains) != 1 {
		t.Fatalf("shop's main containers: %+v", mains)
	}
	main := mains[0]
	if main.Name != "shop" || main.Image != "team-shop" || main.Tag != "v1" || !reflect.DeepEqual(main.Ports, []int{8081}) {
		t.Fatalf("main container (skips the init container and the fluentd sidecar): %+v", main)
	}
	// envFrom, then env; a repeated ConfigMap key keeps its last value; secrets
	// are names only, and a masked value stays masked.
	wantEnv := map[string]string{"PORT": "8081", "STOCK_ADDRESS": "stock:8082", "STOCK_MOCKED": "false",
		"PAYMENTS_MOCKED": "true", "TOKEN": Secret, "MODE": "live"}
	if !reflect.DeepEqual(main.Env, wantEnv) {
		t.Errorf("env = %v", main.Env)
	}
	if !reflect.DeepEqual(main.Secrets, []string{"DB_PASSWORD", "secret:shop-secrets"}) {
		t.Errorf("secrets = %v", main.Secrets)
	}
	sync := d.Workloads[5]
	if sync.Schedule != "0 3 * * *" || sync.Mains()[0].Image != "team-stock" || sync.Mains()[0].Tag != "sha256:abc" {
		t.Errorf("cronjob: %+v", sync)
	}
	if len(d.Services) != 1 || !reflect.DeepEqual(d.Services[0].Ports, []Port{{Name: "http", Port: 80, Target: "8081"}}) {
		t.Errorf("services = %+v", d.Services)
	}
	if len(d.Routes) != 2 || d.Routes[0].Kind != "Ingress" || !reflect.DeepEqual(d.Routes[0].Hosts, []string{"api.example.com"}) ||
		!reflect.DeepEqual(d.Routes[1].Backends, []string{"shop"}) || !reflect.DeepEqual(d.Routes[1].Paths, []string{"/shop"}) {
		t.Errorf("routes = %+v", d.Routes)
	}
}

func TestMatch(t *testing.T) {
	d, err := Read(strings.NewReader(rendered), "production", "chart")
	if err != nil {
		t.Fatal(err)
	}
	// "team" is an image group only with three images of its own.
	d.Workloads = append(d.Workloads, Workload{Kind: "Deployment", Name: "other",
		Containers: []Container{{Name: "x", Image: "team-other"}}})
	repos := Repos{
		"shop":     {Modules: []string{"example.com/shop"}, Mains: 1},
		"stock":    {Modules: []string{"example.com/stock"}, Mains: 2},
		"checkout": {Modules: []string{"example.com/checkout"}, Mains: 1},
		"team":     {Modules: []string{"example.com/team"}},
		"notify":   {Modules: []string{"example.com/notify"}, Mains: 2},
	}
	analysed := Analysed{
		"example.com/shop":     {"shop"},
		"example.com/stock":    {"stock-grpc", "stock-jobs-sync"},
		"example.com/checkout": {"checkout"},
		"example.com/notify":   {"notify-service-grpc-v1", "notify-service-grpc-v2"},
	}
	// legacy-admin's image (team-hub) names no repository and team is an image
	// group, so the workload's name decides: renamed checkout, it matches.
	for i := range d.Workloads {
		if d.Workloads[i].Name == "legacy-admin" {
			d.Workloads[i].Name = "checkout"
		}
	}
	d.Match(repos, analysed)
	got := map[string]string{}
	for i := range d.Workloads {
		var ms []string
		for _, c := range d.Workloads[i].Mains() {
			ms = append(ms, c.Repo+" "+c.Service)
		}
		got[d.Workloads[i].Name] = strings.Join(ms, "; ")
	}
	want := map[string]string{
		"shop":       "shop shop",             // the repository's only main, whatever "app api" says
		"stock":      "stock stock-grpc",      // /bin/grpc
		"stock-sync": "stock stock-jobs-sync", // /bin/sync_job
		"checkout":   "checkout checkout",     // a group image (team-hub): the workload's name
		"exporter":   " ",                     // third-party
		"other":      " ",                     // team is a group: never a repository by its beginning
		"notify":     "notify notify-service-grpc-v1; notify notify-service-grpc-v2",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("matches = %v, want %v", got, want)
	}

	md := Report(d, []string{"checkout", "notify-service-grpc-v2", "shop", "stock-grpc", "stock-jobs-sync", "stock-idle"}, nil)
	for _, s := range []string{"5 of 6 analysed services run in production. **Not deployed** (no workload matched) (1): stock-idle.",
		"| notify-service-grpc-v2 | notify [notify-v2] | `/bin/service/grpc/v2` |  |",
		"- shop: `PAYMENTS_MOCKED=true`", "| HTTPRoute shop-public | www.example.com | `/shop` | shop (shop) |",
		"| stock-jobs-sync | stock-sync (cronjob `0 3 * * *`) | `/bin/sync_job` |  |"} {
		if !strings.Contains(md, s) {
			t.Errorf("report lacks %q:\n%s", s, md)
		}
	}
}

func TestDeclare(t *testing.T) {
	d := &Deployment{Environment: "production", Workloads: []Workload{
		{Kind: "Deployment", Name: "admin", Containers: []Container{{Name: "admin", Image: "team-admin", Command: []string{"gunicorn", "wsgi"}}}},
		{Kind: "Deployment", Name: "shop", Containers: []Container{{Name: "shop", Image: "team-shop"}}},
	}}
	d.Match(Repos{"admin": {}, "shop": {Modules: []string{"example.com/shop"}, Mains: 1}}, Analysed{"example.com/shop": {"shop"}})
	if c := d.Workloads[0].Mains()[0]; c.Repo != "admin" || c.Match != "repository not analysed" {
		t.Fatalf("before: %+v", c)
	}
	d.Declare([]Declared{{Service: "admin", Repo: "admin", Workloads: []string{"admin"}}})
	if c := d.Workloads[0].Mains()[0]; c.Service != "admin" || c.Match != "declared" {
		t.Errorf("declared workload: %+v", c)
	}
	if c := d.Workloads[1].Mains()[0]; c.Service != "shop" {
		t.Errorf("analysed workload changed: %+v", c)
	}
	if md := Report(d, []string{"admin", "shop"}, nil); !strings.Contains(md, "2 run an analysed service (1 of them a service declared by hand)") {
		t.Errorf("report:\n%s", md)
	}
}

func TestCommandRoles(t *testing.T) {
	for cmd, want := range map[string][]string{
		"/bin/service/grpc/v1": {"servicegrpcv1", "grpcv1", "v1"},
		"/usr/bin/grpc_app":    {"grpcapp", "grpc"},
		"/bin/app api public":  {"apipublic", "api"},
		"pricerefreshjob":      {"pricerefreshjob", "pricerefresh"},
	} {
		roles, _ := commandRoles(Container{Command: strings.Fields(cmd)})
		if !reflect.DeepEqual(roles, want) {
			t.Errorf("commandRoles(%q) = %v, want %v", cmd, roles, want)
		}
	}
	if s, _ := byCommand([]string{"invoices-grpc", "invoices-jobs-sendreminder"}, Container{Command: []string{"/bin/sendreminders"}}, 2, nil); s != "invoices-jobs-sendreminder" {
		t.Errorf("one letter apart: %q", s)
	}
	if s, _ := byCommand([]string{"catalog", "catalog-jobs-scan"}, Container{Command: []string{"/usr/bin/app", "grpc"}}, 2, []string{"catalog"}); s != "catalog" {
		t.Errorf("a generic binary runs the repository's main: %q", s)
	}
	if s, m := byCommand([]string{"quotes-grpc"}, Container{Command: []string{"/usr/bin/nightly_report_app"}}, 3, nil); s != "" || !strings.Contains(m, "names none") {
		t.Errorf("a specific binary that names no analysed main: %q, %q", s, m)
	}
}

func TestMaskSecrets(t *testing.T) {
	out, err := maskSecrets([]byte("db:\n  password: ENC[AES256_GCM,data:abc,type:str]\n  port: 3306\nlist:\n  - ENC[x]\n  - ENC[y]\nsops:\n  kms: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, "ENC[") || strings.Contains(s, "sops") || strings.Contains(s, "3306") ||
		!strings.Contains(s, "password: (secret)") || strings.Count(s, Secret) != 4 {
		t.Errorf("masked:\n%s", s)
	}
}

// TestRender renders a tiny chart when helm is installed.
func TestRender(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	dir := t.TempDir()
	files := map[string]string{
		"Chart.yaml":         "apiVersion: v2\nname: test\nversion: 0.1.0\n",
		"values.yaml":        "app:\n  enabled: false\n",
		"overrides.yaml":     "app:\n  enabled: true\n",
		"secrets.yaml":       "app:\n  token: ENC[AES256_GCM,data:abc]\nsops:\n  version: 3\n",
		"templates/app.yaml": "{{- if .Values.app.enabled }}\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\ndata:\n  TOKEN: {{ .Values.app.token | b64enc | quote }}\n  PLAIN: {{ .Values.app.token | quote }}\n{{- end }}\n",
	}
	for p, c := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := Render(context.Background(), Chart{Dir: dir, Values: []string{"values.yaml", "overrides.yaml"}, Secrets: []string{"secrets.yaml"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `PLAIN: "(secret)"`) || strings.Contains(string(out), "ENC[") {
		t.Errorf("rendered:\n%s", out)
	}
}

func TestCheck(t *testing.T) {
	ctr := func(name, service string, env map[string]string) Container {
		return Container{Name: name, Service: service, Env: env}
	}
	d := &Deployment{Environment: "production",
		Workloads: []Workload{
			{Kind: "Deployment", Name: "shop", Containers: []Container{ctr("shop", "shop", map[string]string{
				"GRPC_NOTIFY_HOST": "notify", "GRPC_NOTIFY_V2_PORT": "8080", // v2, by the Service port's name
				"GRPC_SENDER_HOST": "sender-grpc:8080",
				"LOYALTY_ADDR":     "loyalty:5051",
				"NO_PROXY_HOSTS":   "sender",
				"PAYMENTS_MOCKED":  "true",
				"PAYMENTS_URL":     "http://payments/api",
			})}},
			{Kind: "Deployment", Name: "notify", Services: []string{"notify"}, Containers: []Container{
				ctr("notify-grpc-v1", "notify-v1", nil), ctr("notify-grpc-v2", "notify-v2", nil)}},
			{Kind: "Deployment", Name: "sender", Services: []string{"sender"}, Containers: []Container{ctr("sender", "sender", nil)}},
			{Kind: "Deployment", Name: "sender-grpc", Services: []string{"sender-grpc"}, Containers: []Container{ctr("grpc", "sender-grpc", nil)}},
			{Kind: "Deployment", Name: "loyalty", Services: []string{"loyalty"}, Containers: []Container{ctr("app", "loyalty", nil)}},
			{Kind: "Deployment", Name: "payments", Services: []string{"payments"}, Containers: []Container{ctr("app", "payments", nil)}},
		},
		Services: []Service{
			{Name: "notify", Workloads: []string{"notify"}, Ports: []Port{{Name: "grpc-v1", Port: 8081}, {Name: "grpc-v2", Port: 8080}}},
			{Name: "sender", Workloads: []string{"sender"}, Ports: []Port{{Port: 8080}}},
			{Name: "sender-grpc", Workloads: []string{"sender-grpc"}, Ports: []Port{{Name: "grpc", Port: 8080}}},
			{Name: "loyalty", Workloads: []string{"loyalty"}, Ports: []Port{{Port: 5051}}},
			{Name: "payments", Workloads: []string{"payments"}, Ports: []Port{{Port: 80}}},
		},
	}
	link := func(to, transport, key string) model.Link {
		return model.Link{From: "shop", To: to, Transport: transport, Key: key}
	}
	n := d.Check([]model.Link{
		link("notify-v2", "grpc", "HookService"),
		link("sender", "grpc", "SenderService"),
		link("sender-grpc", "grpc", "SenderService"),
		link("payments", "grpc", "PaymentsService"),
		link("loyalty", "kafka", "loyalty.updated"), // messaging: never compared
	}, map[string][]string{"shop": {"GRPC_NOTIFY_HOST", "PAYMENTS_URL"}})
	pairs := func(ps []Pair) []string {
		var out []string
		for _, p := range ps {
			out = append(out, p.From+"→"+p.To)
		}
		return out
	}
	if got := pairs(n.Confirmed); !reflect.DeepEqual(got, []string{"shop→notify-v2", "shop→payments", "shop→sender-grpc"}) {
		t.Errorf("confirmed = %v", got)
	}
	if len(n.CodeOnly) != 0 {
		t.Errorf("code only = %v (sender is settled away)", pairs(n.CodeOnly))
	}
	if got := pairs(n.ConfigOnly); !reflect.DeepEqual(got, []string{"shop→loyalty"}) {
		t.Errorf("config only = %v (the proxy list and the port-narrowed notify-v1 do not count)", got)
	} else if p := n.ConfigOnly[0]; !p.Checked || !reflect.DeepEqual(p.Unread, []string{"LOYALTY_ADDR=loyalty:5051"}) {
		t.Errorf("shop's code reads no LOYALTY_ADDR: %+v", p)
	}
	if n := d.Check(nil, nil); len(n.ConfigOnly) == 0 || n.ConfigOnly[0].Checked {
		t.Error("without the run's settings, configured pairs are not told apart")
	}
	d.Network = n
	md := Report(d, nil, nil)
	if !strings.Contains(md, "### Configured, read by nothing in the caller's code (1)") {
		t.Errorf("report:\n%s", md)
	}
	if len(n.Settled) != 1 || n.Settled[0].To != "sender-grpc" || !reflect.DeepEqual(n.Settled[0].NotDialled, []string{"sender"}) {
		t.Errorf("settled = %+v", n.Settled)
	}
	if len(n.Mocked) != 1 || !reflect.DeepEqual(n.Mocked[0].To, []string{"payments"}) {
		t.Errorf("mocked = %+v", n.Mocked)
	}
}

// A repository whose CI builds a container's image wins over the name
// heuristics; among several, the one the name points to, else the one
// committed to last.
func TestMatchBuilders(t *testing.T) {
	d := &Deployment{Workloads: []Workload{
		{Kind: "Deployment", Name: "checker", Containers: []Container{{Name: "checker", Image: "platform-checker"}}},
		{Kind: "Deployment", Name: "jobs", Containers: []Container{{Name: "jobs", Image: "platform-jobs"}}},
		{Kind: "Deployment", Name: "shop", Containers: []Container{{Name: "shop", Image: "platform-shop"}}},
		{Kind: "Deployment", Name: "proxy", Containers: []Container{{Name: "proxy", Image: "vendor/proxy"}}},
		{Kind: "Deployment", Name: "ledger", Containers: []Container{{Name: "ledger", Image: "platform-ledger"}}},
	}}
	repos := Repos{
		"checker":  {},                                                                                        // only its name matches
		"monolith": {Images: []string{"platform-checker", "platform-jobs"}, LastCommit: "2026-09-01"},         // builds both
		"copy":     {Images: []string{"platform-jobs"}, LastCommit: "2026-01-01"},                             // an older copy
		"shop":     {Modules: []string{"example.com/shop"}, Mains: 1, Images: []string{"team/platform-shop"}}, // only its last segment
		"fork":     {Images: []string{"platform-shop"}, LastCommit: "2027-01-01"},
		"ledger":   {Images: []string{"platform-ledger"}, LastCommit: "2025-01-01"}, // older, but named like the image
		"ledger2":  {Images: []string{"platform-ledger"}, LastCommit: "2026-06-01"}, // the exact name: it wins
	}
	d.Match(repos, Analysed{"example.com/shop": {"shop"}})
	var got []string
	for _, w := range d.Workloads {
		c := w.Mains()[0]
		got = append(got, fmt.Sprintf("%s=%s built=%v also=%v service=%s", w.Name, c.Repo, c.Built, c.AlsoBuilt, c.Service))
	}
	want := []string{
		"checker=monolith built=true also=[] service=",
		"jobs=monolith built=true also=[copy] service=",
		"shop=fork built=true also=[] service=",
		"proxy= built=false also=[] service=",
		"ledger=ledger built=true also=[ledger2] service=",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("matches:\n got %q\nwant %q", got, want)
	}
}
