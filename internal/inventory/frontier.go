package inventory

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Frontier lists the repositories around a focus worth a full analysis.
type Frontier struct {
	Focus      []string    `json:"focus"`
	Rings      int         `json:"rings"`
	Candidates []Candidate `json:"candidates"`
	// Copies declare a module a focus repository declares (test copies, forks):
	// analysing them would duplicate the focus services' names.
	Copies []Copy `json:"copies,omitempty"`
	// Unowned lists the missing contracts and one-sided topics of the hints
	// that no repository of the inventory serves, imports or is named like.
	Unowned []string `json:"unowned,omitempty"`
	Hints   Hints    `json:"hints"`
}

// Candidate is a repository the frontier suggests analysing, with the reasons.
type Candidate struct {
	Repo       string   `json:"repo"`
	Ring       int      `json:"ring"` // 1: around the focus; 2: around the focus and ring 1; ...
	Languages  []string `json:"languages"`
	Program    bool     `json:"program"` // builds a Go program; only those can be analysed
	Dormant    bool     `json:"dormant,omitempty"`
	LastCommit string   `json:"lastCommit,omitempty"`
	Reasons    []string `json:"reasons"`
}

// Copy is a repository declaring a focus repository's module.
type Copy struct {
	Repo   string `json:"repo"`
	Of     string `json:"of"`
	Module string `json:"module"`
}

// Hints come from a run of the focus repositories. Without them, the frontier
// relies on the inventory alone.
type Hints struct {
	// Modules are the modules of the analysed services. A focus repository
	// whose programs are only tools (a library's create-topic) has none, so importing
	// it does not make a caller.
	Modules []string `json:"modules,omitempty"`
	// Served maps the proto package of each gRPC contract a focus service
	// serves to the module serving it ("shop.carts.v1" -> "example.com/wallet").
	Served map[string]string `json:"served,omitempty"`
	// Missing lists the proto packages focus services call and no focus
	// service serves ("catalog.pricing.v1").
	Missing []string `json:"missing,omitempty"`
	// Topics lists the topics and queues with one side only, that no reason
	// explains ("catalog.price.updated").
	Topics []string `json:"topics,omitempty"`
}

// Programs lists the candidates to analyse in full: Go programs with a
// commit in the two years before the inventory's newest.
func (f *Frontier) Programs() []string {
	var out []string
	for _, c := range f.Candidates {
		if c.Program && !c.Dormant {
			out = append(out, c.Repo)
		}
	}
	return out
}

// Frontier suggests the repositories around the focus worth a full analysis:
// Go programs that call into it (they import a focus service's module, SDK or
// contract), and repositories that may own what it calls but nobody analysed
// serves (named like a missing contract or a topic's service, or importing
// the missing contract).
func (inv *Inventory) Frontier(focus []string, h Hints) *Frontier {
	inFocus := map[string]bool{}
	for _, f := range focus {
		inFocus[f] = true
	}
	declaredBy := map[string]string{} // module -> focus repository declaring it
	for _, r := range inv.Repos {
		if inFocus[r.Name] {
			for _, m := range r.Modules {
				declaredBy[m.Path] = r.Name
			}
		}
	}

	// What belongs to the focus services: the modules of the analysed services
	// (or, without a run, of the focus programs), and their SDK modules.
	owned := map[string]string{} // module path -> focus repository
	if len(h.Modules) > 0 {
		for _, m := range h.Modules {
			if repo := declaredBy[m]; repo != "" {
				owned[m] = repo
			}
		}
	} else {
		for _, r := range inv.Repos {
			if inFocus[r.Name] && r.Service() {
				for _, m := range r.Modules {
					owned[m.Path] = r.Name
				}
			}
		}
	}
	services := map[string]bool{}
	for _, repo := range owned {
		services[normalize(repo)] = true
	}
	sdkOf := map[string]string{} // client library module -> focus repository
	for _, r := range inv.Repos {
		for _, m := range r.Modules {
			if suffix := inv.Conventions.ClientLibrarySuffix; suffix != "" && strings.HasSuffix(path.Base(m.Path), suffix) {
				name := normalize(strings.TrimSuffix(path.Base(m.Path), suffix))
				for _, repo := range owned {
					if services[name] && normalize(repo) == name {
						sdkOf[m.Path] = repo
					}
				}
			}
		}
	}
	for m, repo := range sdkOf {
		owned[m] = repo
	}
	contractOf := map[string]string{} // "/contracts/shop/carts/v1" -> focus repository
	missing := map[string]string{}    // "/contracts/catalog/pricing/v1" -> "catalog.pricing.v1"
	if tmpl := inv.Conventions.ContractImport; tmpl != "" {
		for pkg, module := range h.Served {
			repo := declaredBy[module]
			if repo == "" {
				repo = path.Base(module)
			}
			contractOf[contractPath(tmpl, pkg)] = repo
		}
		for _, pkg := range h.Missing {
			missing[contractPath(tmpl, pkg)] = pkg
		}
	}

	f := &Frontier{Focus: focus, Rings: 1, Hints: h}
	byRepo := map[string]*Candidate{}
	found := map[string]bool{} // missing contracts and topics some candidate may own
	add := func(r Repo, reason string) {
		c := byRepo[r.Name]
		if c == nil {
			c = &Candidate{Repo: r.Name, Ring: 1, Languages: r.Languages, Program: r.Service(), LastCommit: r.LastCommit,
				Dormant: r.LastCommit != "" && r.LastCommit < cutoff(inv)}
			byRepo[r.Name] = c
		}
		for _, x := range c.Reasons {
			if x == reason {
				return
			}
		}
		c.Reasons = append(c.Reasons, reason)
	}
	skip := map[string]bool{}
	for _, r := range inv.Repos {
		if inFocus[r.Name] {
			skip[r.Name] = true
			continue
		}
		sdk := len(r.Modules) > 0
		for _, m := range r.Modules {
			if of := declaredBy[m.Path]; of != "" {
				f.Copies = append(f.Copies, Copy{Repo: r.Name, Of: of, Module: m.Path})
				skip[r.Name] = true
			}
			if sdkOf[m.Path] == "" {
				sdk = false
			}
		}
		if sdk {
			skip[r.Name] = true // analysed with its callers (-include-modules)
		}
	}

	// Callers: programs importing what a focus service owns or serves.
	for _, r := range inv.Repos {
		if skip[r.Name] || !r.Service() {
			continue
		}
		for _, m := range r.Modules {
			for _, imp := range m.Imports {
				if repo := ownerOf(imp, owned); repo != "" {
					add(r, fmt.Sprintf("calls %s: imports %s", repo, imp))
				}
				for suffix, repo := range contractOf {
					if strings.HasSuffix(imp, suffix) {
						add(r, fmt.Sprintf("calls %s: imports its contract %s", repo, imp))
					}
				}
				for suffix, pkg := range missing {
					if strings.HasSuffix(imp, suffix) {
						add(r, fmt.Sprintf("serves or calls the missing contract %s", pkg))
						found[pkg] = true
					}
				}
			}
		}
	}
	// Owners: repositories named like a missing contract or a topic's service.
	for _, pkg := range h.Missing {
		for _, name := range contractNames(pkg) {
			for _, r := range inv.Repos {
				if !skip[r.Name] && normalize(r.Name) == name {
					add(r, fmt.Sprintf("named like the missing contract %s", pkg))
					found[pkg] = true
				}
			}
		}
	}
	for _, topic := range h.Topics {
		parts := strings.Split(topic, ".")
		if len(parts) < 3 {
			continue
		}
		for _, r := range inv.Repos {
			if !skip[r.Name] && normalize(r.Name) == normalize(parts[1]) {
				add(r, fmt.Sprintf("named like the service of the topic %s", topic))
				found[topic] = true
			}
		}
	}
	for _, x := range append(append([]string{}, h.Missing...), h.Topics...) {
		if !found[x] {
			f.Unowned = append(f.Unowned, x)
		}
	}

	for _, c := range byRepo {
		sort.Strings(c.Reasons)
		f.Candidates = append(f.Candidates, *c)
	}
	sort.Slice(f.Candidates, func(i, j int) bool {
		a, b := f.Candidates[i], f.Candidates[j]
		if a.Program != b.Program {
			return a.Program
		}
		if a.Dormant != b.Dormant {
			return !a.Dormant
		}
		return a.Repo < b.Repo
	})
	sort.Slice(f.Copies, func(i, j int) bool { return f.Copies[i].Repo < f.Copies[j].Repo })
	sort.Strings(f.Unowned)
	return f
}

// Rings computes n rings of the frontier: the Go programs of each ring join
// the focus of the next. hints gives the hints for a focus; they only cover
// the repositories its run analysed, so run the analysis between rounds.
// Hints and Unowned are those of the last ring.
func (inv *Inventory) Rings(focus []string, n int, hints func(focus []string) Hints) *Frontier {
	f := inv.Frontier(focus, hints(focus))
	seen := map[string]bool{}
	for _, c := range f.Candidates {
		seen[c.Repo] = true
	}
	copied := map[string]bool{}
	for _, c := range f.Copies {
		copied[c.Repo] = true
	}
	current := append(append([]string{}, focus...), f.Programs()...)
	for ring := 2; ring <= n; ring++ {
		next := inv.Frontier(current, hints(current))
		var added []string
		for _, c := range next.Candidates {
			if seen[c.Repo] {
				continue
			}
			seen[c.Repo] = true
			c.Ring = ring
			f.Candidates = append(f.Candidates, c)
			if c.Program && !c.Dormant {
				added = append(added, c.Repo)
			}
		}
		for _, c := range next.Copies {
			if !copied[c.Repo] {
				copied[c.Repo] = true
				f.Copies = append(f.Copies, c)
			}
		}
		f.Rings, f.Unowned, f.Hints = ring, next.Unowned, next.Hints
		if len(added) == 0 {
			break
		}
		current = append(current, added...)
	}
	sort.SliceStable(f.Candidates, func(i, j int) bool { return f.Candidates[i].Ring < f.Candidates[j].Ring })
	sort.Slice(f.Copies, func(i, j int) bool { return f.Copies[i].Repo < f.Copies[j].Repo })
	return f
}

// contractPath is the end of the import path of a proto package's generated
// code, from Conventions.ContractImport: with "/contracts/{package}",
// "shop.carts.v1" -> "/contracts/shop/carts/v1".
func contractPath(tmpl, pkg string) string {
	return strings.ReplaceAll(tmpl, "{package}", strings.ReplaceAll(pkg, ".", "/"))
}

// ownerOf returns the focus repository owning the module a package belongs to.
func ownerOf(pkg string, owned map[string]string) string {
	best, repo := "", ""
	for m, r := range owned {
		if (pkg == m || strings.HasPrefix(pkg, m+"/")) && len(m) > len(best) {
			best, repo = m, r
		}
	}
	return repo
}

// contractNames lists the repository names a proto package may belong to:
// its first word, its last word before the version, and all its words joined
// ("catalog.pricing.v1": catalog, pricing, catalogpricing).
func contractNames(pkg string) []string {
	var words []string
	for _, w := range strings.Split(pkg, ".") {
		if len(w) > 1 && w[0] == 'v' && strings.Trim(w[1:], "0123456789") == "" {
			continue // version
		}
		words = append(words, normalize(w))
	}
	if len(words) == 0 {
		return nil
	}
	names := []string{words[0], words[len(words)-1], strings.Join(words, "")}
	sort.Strings(names)
	var out []string
	for i, n := range names {
		if i == 0 || n != names[i-1] {
			out = append(out, n)
		}
	}
	return out
}

// normalize ignores case and separators: order_history, order-history.
func normalize(s string) string {
	return strings.NewReplacer("-", "", "_", "", ".", "").Replace(strings.ToLower(s))
}

// FrontierReport renders the frontier as Markdown.
func FrontierReport(f *Frontier) string {
	var b strings.Builder
	programs := f.Programs()
	rings := ""
	if f.Rings > 1 {
		rings = fmt.Sprintf(" in %d rings (each around the focus and the rings before it)", f.Rings)
	}
	fmt.Fprintf(&b, "# Frontier\n\n%d repositories around the %d focus repositories%s. %d Go programs to analyse in full "+
		"(`frontier.txt`); the others are dormant programs (no commit in the two years before the newest), "+
		"libraries, or not Go: add them by hand if they matter.\n", len(f.Candidates), len(f.Focus), rings, len(programs))
	if len(f.Hints.Modules) == 0 {
		b.WriteString("\nWithout a run of the focus (`-run`), callers are found through the focus programs' modules only.\n")
	}
	b.WriteString("\n| Ring | Repository | Kind | Last commit | Reasons |\n|---|---|---|---|---|\n")
	for _, c := range f.Candidates {
		kind := "Go program"
		if c.Dormant && c.Program {
			kind = "Go program, dormant"
		}
		if !c.Program {
			kind = strings.Join(c.Languages, ", ")
			if kind == "" {
				kind = "unknown"
			}
		}
		reasons := c.Reasons
		more := ""
		if len(reasons) > 4 {
			more = fmt.Sprintf("; … %d more", len(reasons)-4)
			reasons = reasons[:4]
		}
		fmt.Fprintf(&b, "| %d | %s | %s | %s | %s%s |\n", c.Ring, c.Repo, kind, c.LastCommit, strings.Join(reasons, "; "), more)
	}
	if len(f.Copies) > 0 {
		b.WriteString("\n## Copies of focus repositories (left out)\n\nThey declare a focus repository's module, so their " +
			"services would take the focus services' names.\n\n")
		for _, c := range f.Copies {
			fmt.Fprintf(&b, "- %s: `%s`, as %s does\n", c.Repo, c.Module, c.Of)
		}
	}
	if len(f.Unowned) > 0 {
		fmt.Fprintf(&b, "\n## Not found in any repository (%d)\n\nMissing contracts and one-sided topics or queues that no "+
			"repository serves, imports or is named like. Running the frontier may still answer them; otherwise they are external, retired or named differently.\n\n%s\n",
			len(f.Unowned), list(f.Unowned))
	}
	return b.String()
}
