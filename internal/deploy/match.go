package deploy

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Repo is what Match knows of a repository.
type Repo struct {
	Modules    []string // the module paths it declares
	Mains      int      // its root module's main packages that are services (not migrations or examples)
	Images     []string // the images its CI builds (inventory.Image names; "*" for an unknown part)
	LastCommit string   // YYYY-MM-DD, to choose between repositories building one image
}

// Repos maps repository names to what they declare (from the inventory).
// Without an inventory, the analysed modules stand in, keyed by base name.
type Repos map[string]Repo

// Analysed lists the services of a run by module path.
type Analysed map[string][]string

// Match fills each workload's repository and analysed service.
//
// Each main container is matched on its own. The repository is the one whose
// CI builds its image (Repo.Images: the whole name, else its last segment),
// choosing, among several, the one its name also points to, else the one
// committed to last (Container.Built, AlsoBuilt). Otherwise it comes from its
// image name: the whole name,
// its endings ("payments-ledger" -> "ledger") and its beginnings
// ("billing-connector" -> "billing"), compared with repository names and module base
// names ignoring case and separators, then allowing one edit for long names.
// A beginning that is an image group (a first segment shared by three or more
// images: "platform", "payments") never names the repository. When the image
// is a group's but names no repository ("platform-gate"), the workload's name
// is tried the same way, when all its main containers run that image.
//
// The service is the one the command names, among the repository's analysed
// mains: the binary's path below "bin/" ("/bin/service/grpc/v1",
// "/usr/bin/grpc_app") or a subcommand ("app grpc"). A repository with one
// main runs it whatever the command.
func (d *Deployment) Match(repos Repos, analysed Analysed) {
	names := map[string]string{} // normalized name -> repository
	for repo, r := range repos {
		names[norm(repo)] = repo
		for _, m := range r.Modules {
			if _, ok := names[norm(moduleBase(m))]; !ok {
				names[norm(moduleBase(m))] = repo
			}
		}
	}
	groups := d.imageGroups()
	builders := imageBuilders(repos)
	for i := range d.Workloads {
		w := &d.Workloads[i]
		mains := w.Mains()
		oneImage := true // the workload's name may stand for every container
		for _, c := range mains {
			oneImage = oneImage && c.Image == mains[0].Image
		}
		for _, c := range mains {
			image := path.Base(c.Image)
			byName := findRepo(names, image, groups)
			if first, _, ok := strings.Cut(image, "-"); byName == "" && oneImage && ok && groups[first] {
				byName = findRepo(names, w.Name, groups)
			}
			c.Repo = byName
			if built := builders.of(c.Image); len(built) > 0 {
				c.Repo, c.Built, c.AlsoBuilt = pickBuilder(built, byName, repos), true, nil
				for _, r := range built {
					if r != c.Repo {
						c.AlsoBuilt = append(c.AlsoBuilt, r)
					}
				}
			}
			if c.Repo == "" {
				continue
			}
			var services []string
			for _, m := range repos[c.Repo].Modules {
				services = append(services, analysed[m]...)
			}
			sort.Strings(services)
			if len(services) == 0 {
				c.Match = "repository not analysed"
				continue
			}
			own := []string{norm(c.Repo)}
			for _, m := range repos[c.Repo].Modules {
				own = append(own, norm(moduleBase(m)))
			}
			c.Service, c.Match = byCommand(services, *c, repos[c.Repo].Mains, own)
		}
	}
}

// builders indexes the images repositories build, by whole name and by last
// segment (lowercase); names with "*" are kept apart and matched as patterns.
type builders struct {
	byName, byBase map[string][]string
	patterns       map[string][]string
}

func imageBuilders(repos Repos) builders {
	b := builders{byName: map[string][]string{}, byBase: map[string][]string{}, patterns: map[string][]string{}}
	for repo, r := range repos {
		for _, im := range r.Images {
			im = strings.ToLower(im)
			if strings.Contains(im, "*") {
				b.patterns[im] = append(b.patterns[im], repo)
				continue
			}
			b.byName[im] = append(b.byName[im], repo)
			b.byBase[path.Base(im)] = append(b.byBase[path.Base(im)], repo)
		}
	}
	return b
}

// of lists the repositories building an image, sorted: those building its
// whole name, else its last segment, else a pattern matching it.
func (b builders) of(image string) []string {
	image = strings.ToLower(image)
	out := b.byName[image]
	if len(out) == 0 {
		out = b.byBase[path.Base(image)]
	}
	if len(out) == 0 {
		for p, repos := range b.patterns {
			if ok, _ := path.Match(p, image); ok {
				out = append(out, repos...)
			} else if ok, _ := path.Match(path.Base(p), path.Base(image)); ok {
				out = append(out, repos...)
			}
		}
	}
	out = uniq(out)
	sort.Strings(out)
	return out
}

// pickBuilder chooses among the repositories building an image: the one its
// name points to, else the one committed to last.
func pickBuilder(built []string, byName string, repos Repos) string {
	best := built[0]
	for _, r := range built {
		if r == byName {
			return r
		}
		if repos[r].LastCommit > repos[best].LastCommit {
			best = r
		}
	}
	return best
}

// Declared is a service described by hand, with the workloads that run it.
type Declared struct {
	Service, Repo string
	Workloads     []string
}

// Declare matches the workloads that services described by hand name: their
// images and commands (a Python script, gunicorn) are not Go mains, so every
// main container of a named workload runs that service.
func (d *Deployment) Declare(services []Declared) {
	byWorkload := map[string]Declared{}
	for _, s := range services {
		for _, w := range s.Workloads {
			byWorkload[w] = s
		}
	}
	for i := range d.Workloads {
		s, ok := byWorkload[d.Workloads[i].Name]
		if !ok {
			continue
		}
		for _, c := range d.Workloads[i].Mains() {
			c.Repo, c.Service, c.Match = s.Repo, s.Service, "declared"
		}
	}
}

// imageGroups lists the first segments shared by three or more image names.
func (d *Deployment) imageGroups() map[string]bool {
	images := map[string]map[string]bool{}
	for _, w := range d.Workloads {
		for _, c := range w.Containers {
			if first, _, ok := strings.Cut(path.Base(c.Image), "-"); ok {
				if images[first] == nil {
					images[first] = map[string]bool{}
				}
				images[first][c.Image] = true
			}
		}
	}
	groups := map[string]bool{}
	for g, is := range images {
		if len(is) >= 3 {
			groups[g] = true
		}
	}
	return groups
}

func findRepo(names map[string]string, name string, groups map[string]bool) string {
	parts := strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' || r == '.' })
	candidates := []string{strings.Join(parts, "")}
	for i := 1; i < len(parts); i++ { // endings, longest first
		candidates = append(candidates, strings.Join(parts[i:], ""))
	}
	approx := len(candidates)
	for i := len(parts) - 1; i > 0; i-- { // beginnings, longest first
		if b := strings.Join(parts[:i], ""); !groups[b] {
			candidates = append(candidates, b)
		}
	}
	for _, c := range candidates {
		if repo, ok := names[norm(c)]; ok {
			return repo
		}
	}
	// One edit away, for long names (accountcreditsvc, account_credits_svc).
	for _, c := range candidates[:approx] {
		if n := norm(c); len(n) >= 8 {
			var close []string
			for k, repo := range names {
				if editDistance(n, k) == 1 {
					close = append(close, repo)
				}
			}
			if close = uniq(close); len(close) == 1 {
				return close[0]
			}
		}
	}
	return ""
}

// byCommand picks the service a command runs among a repository's analysed
// mains; mains counts the repository's mains (analysed or not), and own holds
// the repository's normalized names: a generic binary is the main named like
// the repository (its cmd/ or root main), when there is one.
func byCommand(services []string, c Container, mains int, own []string) (string, string) {
	roles, generic := commandRoles(c)
	match := func(ok func(service, role string) bool) string {
		for _, role := range roles {
			var hits []string
			for _, s := range services {
				if ok(norm(s), role) {
					hits = append(hits, s)
				}
			}
			if len(hits) == 1 {
				return hits[0]
			}
		}
		return ""
	}
	if s := match(func(s, r string) bool { return s == r || strings.HasSuffix(s, r) }); s != "" {
		return s, "command"
	}
	if s := match(nearSuffix); s != "" {
		return s, "command, one letter apart"
	}
	if len(services) == 1 && (mains <= 1 || generic) {
		return services[0], "the repository's only main"
	}
	if generic {
		for _, s := range services {
			for _, o := range own {
				if norm(s) == o {
					return s, "a generic binary: the repository's main"
				}
			}
		}
	}
	if s := match(func(s, r string) bool { return len(r) >= 5 && strings.Contains(s, r) }); s != "" {
		return s, "command, part of the name"
	}
	if len(services) == 1 {
		return "", fmt.Sprintf("the command names none of the repository's analysed mains (%s)", services[0])
	}
	return "", fmt.Sprintf("ambiguous: %d mains (%s)", len(services), strings.Join(services, ", "))
}

// nearSuffix reports whether a long role is one edit from the end of a
// service name (sendreminders, billing-jobs-sendreminder).
func nearSuffix(service, role string) bool {
	if len(role) < 8 {
		return false
	}
	for n := len(role) - 1; n <= len(role)+1; n++ {
		if n <= len(service) && editDistance(service[len(service)-n:], role) <= 1 {
			return true
		}
	}
	return false
}

// commandRoles lists what a command may name, most specific first:
// "/bin/service/grpc/v1" -> servicegrpcv1, grpcv1, v1; "/usr/bin/grpc_app"
// -> grpcapp, grpc; "app api public" -> apipublic, api. generic reports a
// binary that names no main (app, service, an empty command), so a
// repository's one main is what it runs.
func commandRoles(c Container) (roles []string, generic bool) {
	argv := append(append([]string{}, c.Command...), c.Args...)
	if len(argv) == 0 {
		return nil, true
	}
	bin := argv[0]
	if _, rest, ok := strings.Cut(bin, "bin/"); ok {
		bin = rest
	} else {
		bin = path.Base(bin)
	}
	parts := strings.Split(bin, "/")
	last := len(parts) - 1
	trimmed := parts[last]
	for _, suffix := range []string{"_app", "-app", "_job", "-job", "_cron", "-cron", "-cli"} {
		trimmed = strings.TrimSuffix(trimmed, suffix)
	}
	for i := range parts {
		roles = append(roles, norm(strings.Join(parts[i:], "")))
		if trimmed != parts[last] {
			roles = append(roles, norm(strings.Join(append(append([]string{}, parts[i:last]...), trimmed), "")))
		}
	}
	if n := norm(trimmed); trimmed == parts[last] && len(n) > 8 { // pricerefreshjob
		for _, suffix := range []string{"job", "app", "cron", "cli"} {
			if strings.HasSuffix(n, suffix) {
				roles = append(roles, strings.TrimSuffix(n, suffix))
			}
		}
	}
	generic = len(parts) == 1 && (trimmed == "app" || trimmed == "main" || trimmed == "service" || trimmed == "server")
	// Subcommands: the words after the binary, up to the first flag.
	var words []string
	for _, a := range argv[1:] {
		if strings.HasPrefix(a, "-") || strings.ContainsAny(a, "/=:") {
			break
		}
		words = append(words, a)
	}
	for i := len(words); i > 0; i-- {
		roles = append(roles, norm(strings.Join(words[:i], "")))
	}
	var out []string
	for _, r := range roles {
		if r != "" && r != "app" && r != "main" && r != "service" && r != "server" {
			out = append(out, r)
		}
	}
	return out, generic
}

// moduleBase is a module path's last element without a major version.
func moduleBase(m string) string {
	b := path.Base(m)
	if len(b) > 1 && b[0] == 'v' && strings.Trim(b[1:], "0123456789") == "" {
		b = path.Base(path.Dir(m))
	}
	return b
}

func norm(s string) string {
	return strings.NewReplacer("-", "", "_", "", ".", "", "/", "").Replace(strings.ToLower(s))
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}
