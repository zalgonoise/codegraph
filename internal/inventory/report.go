package inventory

import (
	"fmt"
	"sort"
	"strings"
)

// The public messaging libraries, by the module path a repository requires;
// Conventions.Messaging adds in-house ones before them.
var messaging = []MessagingLibrary{
	{"segmentio/kafka-go", "kafka", "github.com/segmentio/kafka-go"},
	{"sarama", "kafka", "github.com/IBM/sarama"},
	{"sarama (Shopify)", "kafka", "github.com/Shopify/sarama"},
	{"confluent-kafka-go", "kafka", "github.com/confluentinc/confluent-kafka-go"},
	{"amqp091-go", "rabbitmq", "github.com/rabbitmq/amqp091-go"},
	{"streadway/amqp", "rabbitmq", "github.com/streadway/amqp"},
}

// Dependents lists the repositories requiring module (directly), with the
// version each requires.
func (inv *Inventory) Dependents(module string) []Require {
	var out []Require
	for _, r := range inv.Repos {
		for _, m := range r.Modules {
			for _, req := range m.Requires {
				if req.Path == module {
					out = append(out, Require{Path: r.Name, Version: req.Version})
				}
			}
		}
	}
	return out
}

// Owners returns the repositories declaring a module: usually one, but test
// copies, forks and SDKs reusing a module path make several.
func (inv *Inventory) Owners(module string) []string {
	var out []string
	for _, r := range inv.Repos {
		for _, m := range r.Modules {
			if m.Path == module {
				out = append(out, r.Name)
			}
		}
	}
	return out
}

// Report renders the inventory as Markdown: what there is, who depends on
// which in-house module (and at which versions), which messaging libraries
// each repository uses, and which programs are dormant.
func Report(inv *Inventory) string {
	var b strings.Builder
	langs, years, goVersions := map[string]int{}, map[string]int{}, map[string]int{}
	goRepos, programs, libraries, none := 0, 0, 0, 0
	inHouse := map[string]bool{}
	for _, r := range inv.Repos {
		for _, l := range r.Languages {
			langs[l]++
		}
		if len(r.Languages) == 0 {
			none++
		}
		years[yearOf(r.LastCommit)]++
		if len(r.Modules) > 0 {
			goRepos++
			if r.Service() {
				programs++
			} else {
				libraries++
			}
		}
		for _, m := range r.Modules {
			inHouse[m.Path] = true
			if v := minor(m.Go); v != "" {
				goVersions[v]++
			}
		}
	}
	fmt.Fprintf(&b, "# Inventory\n\n%d repositories. Go: %d (%d build programs, %d are libraries only). %d have no "+
		"language marker (configuration, documentation or empty).\n\n", len(inv.Repos), goRepos, programs, libraries, none)
	fmt.Fprintf(&b, "- **Languages** (marker files at the root or one level below): %s.\n", counts(langs))
	fmt.Fprintf(&b, "- **Last commit by year:** %s.\n", countsBy(years, true))
	fmt.Fprintf(&b, "- **Go versions** (`go` line of each module): %s.\n", countsBy(goVersions, true))

	// In-house modules by dependents, with the versions in use.
	type dep struct {
		module   string
		users    int
		versions map[string]int
	}
	var deps []dep
	for mod := range inHouse {
		ds := inv.Dependents(mod)
		if len(ds) == 0 {
			continue
		}
		d := dep{module: mod, users: len(ds), versions: map[string]int{}}
		for _, x := range ds {
			d.versions[x.Version]++
		}
		deps = append(deps, d)
	}
	sort.Slice(deps, func(i, j int) bool {
		if deps[i].users != deps[j].users {
			return deps[i].users > deps[j].users
		}
		return deps[i].module < deps[j].module
	})
	fmt.Fprintf(&b, "\n## In-house modules by dependents (%d modules required by another repository)\n\n", len(deps))
	b.WriteString("Direct requirements only. Versions: how many repositories require each, newest first.\n\n")
	b.WriteString("| Module | Repositories declaring it | Dependents | Versions in use |\n|---|---|---|---|\n")
	for i, d := range deps {
		if i == 50 {
			fmt.Fprintf(&b, "\n%d more in `inventory.json`.\n", len(deps)-50)
			break
		}
		fmt.Fprintf(&b, "| `%s` | %s | %d | %s |\n", d.module, strings.Join(inv.Owners(d.module), ", "), d.users, versions(d.versions, 4))
	}

	// Messaging libraries per repository.
	b.WriteString("\n## Messaging libraries\n\nRepositories requiring each library directly:\n\n")
	b.WriteString("| Library | Transport | Repositories |\n|---|---|---|\n")
	kafka, rabbit := map[string]bool{}, map[string]bool{}
	for _, lib := range append(append([]MessagingLibrary(nil), inv.Conventions.Messaging...), messaging...) {
		users := inv.Dependents(lib.Module)
		if len(users) == 0 {
			continue
		}
		var names []string
		for _, u := range users {
			names = append(names, u.Path)
			if lib.Transport == "kafka" {
				kafka[u.Path] = true
			} else {
				rabbit[u.Path] = true
			}
		}
		fmt.Fprintf(&b, "| %s | %s | %d: %s |\n", lib.Name, lib.Transport, len(names), strings.Join(uniq(names), ", "))
	}
	var both, kafkaOnly, rabbitOnly []string
	for r := range kafka {
		if rabbit[r] {
			both = append(both, r)
		} else {
			kafkaOnly = append(kafkaOnly, r)
		}
	}
	for r := range rabbit {
		if !kafka[r] {
			rabbitOnly = append(rabbitOnly, r)
		}
	}
	fmt.Fprintf(&b, "\nKafka to RabbitMQ: %d repositories use Kafka only, %d both, %d RabbitMQ only.\n\n",
		len(kafkaOnly), len(both), len(rabbitOnly))
	fmt.Fprintf(&b, "- **Kafka only:** %s\n- **Both:** %s\n- **RabbitMQ only:** %s\n",
		list(kafkaOnly), list(both), list(rabbitOnly))

	// Dormant programs.
	var dormant []string
	for _, r := range inv.Repos {
		if r.Service() && r.LastCommit != "" && r.LastCommit < cutoff(inv) {
			dormant = append(dormant, fmt.Sprintf("%s (%s)", r.Name, r.LastCommit))
		}
	}
	fmt.Fprintf(&b, "\n## Go programs without a commit in the two years before the newest commit (%d)\n\n%s\n",
		len(dormant), list(dormant))
	imagesSection(&b, inv)
	return b.String()
}

// imagesSection lists the container images the repositories build, and
// which repositories build each, flagging an image several of them build.
func imagesSection(b *strings.Builder, inv *Inventory) {
	type builder struct{ repo, at string }
	by := map[string][]builder{}
	var names []string
	for _, r := range inv.Repos {
		for _, im := range r.Images {
			if by[im.Name] == nil {
				names = append(names, im.Name)
			}
			at := fmt.Sprintf("`%s` (%s", im.File, im.Via)
			if im.Tag != "" {
				at += ", tag `" + im.Tag + "`"
			}
			if im.Dockerfile != "" {
				at += ", `" + im.Dockerfile + "`"
			}
			by[im.Name] = append(by[im.Name], builder{r.Name, at + ")"})
		}
	}
	sort.Strings(names)
	fmt.Fprintf(b, "\n## Container images (%d)\n\nThe images each repository's CI and compose files build, without registry or tag "+
		"(`*` is what a variable left unknown). `codegraph query image <name>` answers for one, with where it runs.\n\n", len(names))
	if len(names) == 0 {
		b.WriteString("_none found_\n")
		return
	}
	b.WriteString("| Image | Built by |\n|---|---|\n")
	for _, n := range names {
		var cells []string
		for _, x := range by[n] {
			cells = append(cells, fmt.Sprintf("**%s** %s", x.repo, x.at))
		}
		if len(by[n]) > 1 {
			cells[0] = "(several) " + cells[0]
		}
		fmt.Fprintf(b, "| `%s` | %s |\n", n, strings.Join(cells, "<br>"))
	}
}

// cutoff is two years before the newest commit of the inventory, so the
// report does not depend on when it is made.
func cutoff(inv *Inventory) string {
	newest := ""
	for _, r := range inv.Repos {
		if r.LastCommit > newest {
			newest = r.LastCommit
		}
	}
	if len(newest) < 4 {
		return ""
	}
	var y int
	fmt.Sscanf(newest[:4], "%d", &y)
	return fmt.Sprintf("%04d%s", y-2, newest[4:])
}

func yearOf(date string) string {
	if len(date) < 4 {
		return "unknown"
	}
	return date[:4]
}

// minor reduces a go directive to its minor version ("1.24.1" -> "1.24").
func minor(v string) string {
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return v
	}
	return parts[0] + "." + parts[1]
}

func counts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return m[keys[i]] > m[keys[j]] || (m[keys[i]] == m[keys[j]] && keys[i] < keys[j]) })
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

func countsBy(m map[string]int, byKey bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s: %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

// versions lists the versions in use, newest first by semantic order of the
// strings (good enough for v0.x.y and pseudo-versions), up to limit.
func versions(m map[string]int, limit int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return versionLess(keys[j], keys[i]) })
	var parts []string
	for i, k := range keys {
		if i == limit {
			parts = append(parts, fmt.Sprintf("… %d more", len(keys)-limit))
			break
		}
		parts = append(parts, fmt.Sprintf("%s (%d)", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

// versionLess compares versions numerically by their dot-separated parts.
func versionLess(a, b string) bool {
	pa, pb := strings.FieldsFunc(a, sep), strings.FieldsFunc(b, sep)
	for i := 0; i < len(pa) && i < len(pb); i++ {
		var na, nb int
		_, ea := fmt.Sscanf(pa[i], "%d", &na)
		_, eb := fmt.Sscanf(pb[i], "%d", &nb)
		switch {
		case ea == nil && eb == nil && na != nb:
			return na < nb
		case (ea != nil || eb != nil) && pa[i] != pb[i]:
			return pa[i] < pb[i]
		}
	}
	return len(pa) < len(pb)
}

func sep(r rune) bool { return r == '.' || r == '-' || r == 'v' || r == '+' }

func uniq(xs []string) []string {
	sort.Strings(xs)
	var out []string
	for i, x := range xs {
		if i == 0 || x != xs[i-1] {
			out = append(out, x)
		}
	}
	return out
}

func list(xs []string) string {
	if len(xs) == 0 {
		return "none"
	}
	sort.Strings(xs)
	return strings.Join(xs, ", ")
}

// Modules returns the modules the named repositories declare.
func (inv *Inventory) Modules(repos []string) map[string]bool {
	named := map[string]bool{}
	for _, r := range repos {
		named[r] = true
	}
	out := map[string]bool{}
	for _, r := range inv.Repos {
		if named[r.Name] {
			for _, m := range r.Modules {
				out[m.Path] = true
			}
		}
	}
	return out
}
