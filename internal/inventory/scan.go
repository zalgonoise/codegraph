// Package inventory reads every repository of an organisation without
// building anything: the languages each uses and, for Go, its modules with
// their direct requirements, main packages and the in-house packages they
// import. It is the cheap, organisation-wide view next to the full analysis
// of a few repositories, and it suggests which repositories deserve that
// analysis (Frontier).
//
// "In-house" is defined by the data: a module some scanned repository
// declares. Only imports of in-house packages are kept.
package inventory

import (
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode"

	"golang.org/x/mod/modfile"
)

// Inventory is the scan of a set of repositories.
type Inventory struct {
	Codegraph string     `json:"codegraph,omitempty"` // the codegraph version that wrote it
	Repos     []Repo     `json:"repos"`
	Contracts []Contract `json:"contracts,omitempty"` // the gRPC services the contracts catalogues define

	// Conventions are the organisation's naming conventions (a profile's
	// profile.json), which the report and the frontier rely on.
	Conventions Conventions `json:"-"`
}

// Conventions are an organisation's naming conventions, from its profile. The
// zero value assumes none.
type Conventions struct {
	// Messaging lists in-house messaging libraries, reported next to the
	// public ones (sarama, confluent-kafka-go, kafka-go, amqp091, ...).
	Messaging []MessagingLibrary `json:"messaging,omitempty"`
	// ClientLibrarySuffix names a service's client library: the repository and
	// module <service><suffix> ("-sdk-go"). A focus service's client library is
	// analysed with its callers rather than on its own, and importing it makes
	// a caller.
	ClientLibrarySuffix string `json:"clientLibrarySuffix,omitempty"`
	// ContractImport is the end of the import path of a proto package's
	// generated code, with {package} standing for the package with dots as
	// slashes ("/contracts/{package}"). Importing it makes a caller of the
	// contract. Without it, contracts are matched by name only.
	ContractImport string `json:"contractImport,omitempty"`
	// WorkflowImage names the image a reusable CI workflow builds from the
	// inputs a job passes it, with {INPUT} placeholders
	// ("{IMAGE_PREFIX}-{PROJECT}"). A GitHub workflow job calling another
	// workflow with every input it names builds that image.
	WorkflowImage string `json:"workflowImage,omitempty"`
}

// MessagingLibrary is a messaging library, by the module path a repository requires.
type MessagingLibrary struct {
	Name      string `json:"name"`
	Transport string `json:"transport"` // kafka or rabbitmq
	Module    string `json:"module"`
}

// Repo is one repository (a directory, often <name>.git).
type Repo struct {
	Name       string   `json:"name"`                 // the directory name without .git
	Head       string   `json:"head,omitempty"`       // the checked-out commit
	LastCommit string   `json:"lastCommit,omitempty"` // its date, YYYY-MM-DD
	Languages  []string `json:"languages"`            // from marker files: go, python, javascript, ...
	Modules    []Module `json:"modules,omitempty"`    // Go modules, the root one first
	Images     []Image  `json:"images,omitempty"`     // the container images its CI and compose files build
	References []Image  `json:"references,omitempty"` // images its files use without building them: FROM, compose, Helm values, manifests

	refFiles []string // the files References reads, during a scan
}

// Module is a Go module of a repository.
type Module struct {
	Path     string    `json:"path"`
	Dir      string    `json:"dir"` // relative to the repository; "." for the root
	Go       string    `json:"go,omitempty"`
	Requires []Require `json:"requires,omitempty"` // direct requirements only
	Replaces []string  `json:"replaces,omitempty"` // "old => new"
	Mains    []string  `json:"mains,omitempty"`    // directories of main packages, relative to the module
	Imports  []string  `json:"imports,omitempty"`  // in-house packages of other modules
}

// Require is a direct requirement.
type Require struct {
	Path    string `json:"path"`
	Version string `json:"version"`
}

// Service reports whether a repository's root module builds a program: what
// codegraph fleet analyses (it loads the root module only), so a repository whose
// only mains are in nested modules (a library's v2/cmd/create-topic) is a library.
func (r Repo) Service() bool {
	for _, m := range r.Modules {
		if m.Dir == "." && len(m.Mains) > 0 {
			return true
		}
	}
	return false
}

// Scan reads the repositories in dirs, with an organisation's conventions
// (the zero value for none).
func Scan(dirs []string, conv Conventions) *Inventory {
	repos := make([]Repo, len(dirs))
	imports := make([][]map[string]bool, len(dirs)) // per repo, per module: every non-standard import
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	for i, dir := range dirs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			repos[i], imports[i] = scanRepo(dir)
			repos[i].Images = Images(dir, conv.WorkflowImage)
			repos[i].References = References(dir, repos[i].refFiles, repos[i].Images)
			repos[i].refFiles = nil
		}()
	}
	wg.Wait()

	// Keep the imports of in-house modules: those some scanned repository declares.
	var modules []string
	for _, r := range repos {
		for _, m := range r.Modules {
			modules = append(modules, m.Path)
		}
	}
	for i := range repos {
		for j := range repos[i].Modules {
			m := &repos[i].Modules[j]
			for imp := range imports[i][j] {
				if owner := moduleOf(imp, modules); owner != "" && owner != m.Path {
					m.Imports = append(m.Imports, imp)
				}
			}
			sort.Strings(m.Imports)
		}
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].Name < repos[j].Name })
	return &Inventory{Repos: repos, Conventions: conv}
}

// Builders lists the repositories that build an image, given as a
// reference or a name ("registry/team/app:v1", "team/app", "app"): those
// building that name, else that last segment, compared ignoring case. A
// name with "*" (a variable left unknown) matches any text there.
func (inv *Inventory) Builders(ref string) []Built {
	return inv.find(ref, func(r Repo) []Image { return r.Images })
}

// Referrers lists the repositories whose files use an image they do not
// build (Repo.References), the same way.
func (inv *Inventory) Referrers(ref string) []Built {
	return inv.find(ref, func(r Repo) []Image { return r.References })
}

func (inv *Inventory) find(ref string, images func(Repo) []Image) []Built {
	want, ok := imageOf(ref)
	if !ok {
		return nil
	}
	var exact, base []Built
	for _, r := range inv.Repos {
		for _, im := range images(r) {
			switch {
			case globMatch(im.Name, want.Name):
				exact = append(exact, Built{Repo: r.Name, LastCommit: r.LastCommit, Image: im})
			case globMatch(path.Base(im.Name), path.Base(want.Name)):
				base = append(base, Built{Repo: r.Name, LastCommit: r.LastCommit, Image: im})
			}
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return base
}

// Built is an image one repository builds.
type Built struct {
	Repo       string `json:"repo"`
	LastCommit string `json:"lastCommit,omitempty"`
	Image      Image  `json:"image"`
}

// globMatch compares an image name that may hold "*" with a name.
func globMatch(pattern, name string) bool {
	pattern, name = strings.ToLower(pattern), strings.ToLower(name)
	if !strings.Contains(pattern, "*") {
		return pattern == name
	}
	ok, _ := path.Match(pattern, name)
	return ok
}

// moduleOf returns the module (of modules) that package path belongs to: the
// longest module path it starts with.
func moduleOf(path string, modules []string) string {
	best := ""
	for _, m := range modules {
		if (path == m || strings.HasPrefix(path, m+"/")) && len(m) > len(best) {
			best = m
		}
	}
	return best
}

// languageMarkers name the files that tell a repository's languages, at its
// root or one directory below.
var languageMarkers = map[string]string{
	"go.mod": "go", "package.json": "javascript", "pyproject.toml": "python", "requirements.txt": "python",
	"setup.py": "python", "Pipfile": "python", "composer.json": "php", "pom.xml": "java", "build.gradle": "java",
	"build.gradle.kts": "kotlin", "Gemfile": "ruby", "Cargo.toml": "rust", "Dockerfile": "docker",
	"helmfile.yaml": "helm", "Chart.yaml": "helm",
}

func scanRepo(dir string) (Repo, []map[string]bool) {
	r := Repo{Name: strings.TrimSuffix(filepath.Base(dir), ".git"), Languages: []string{}}
	if out, err := exec.Command("git", "-C", dir, "log", "-1", "--format=%H %cs").Output(); err == nil {
		r.Head, r.LastCommit, _ = strings.Cut(strings.TrimSpace(string(out)), " ")
	}
	langs := map[string]bool{}
	var gomods, refFiles []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		depth := strings.Count(filepath.ToSlash(rel), "/")
		if d.IsDir() {
			if p != dir && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if lang, ok := languageMarkers[name]; ok && depth <= 1 {
			langs[lang] = true
		}
		if strings.HasSuffix(name, ".tf") && depth <= 1 {
			langs["terraform"] = true
		}
		if name == "go.mod" && !strings.Contains(rel, "{{") { // not a project template
			gomods = append(gomods, p)
		}
		if referenceFile(name) {
			refFiles = append(refFiles, rel)
		}
		return nil
	})
	r.refFiles = refFiles
	sort.Slice(gomods, func(i, j int) bool {
		return len(gomods[i]) < len(gomods[j]) || (len(gomods[i]) == len(gomods[j]) && gomods[i] < gomods[j])
	})
	var imports []map[string]bool
	for _, gm := range gomods {
		m, imp, ok := scanModule(dir, gm)
		if ok {
			r.Modules = append(r.Modules, m)
			imports = append(imports, imp)
			langs["go"] = true
		}
	}
	for l := range langs {
		r.Languages = append(r.Languages, l)
	}
	sort.Strings(r.Languages)
	return r, imports
}

// skipDir leaves out directories that hold no code of the repository's own.
func skipDir(name string) bool {
	return strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules" || name == "testdata"
}

func scanModule(repo, gomod string) (Module, map[string]bool, bool) {
	data, err := os.ReadFile(gomod)
	if err != nil {
		return Module{}, nil, false
	}
	f, err := modfile.Parse(gomod, data, nil)
	if err != nil {
		f, err = modfile.ParseLax(gomod, data, nil) // ignores replace directives, but tolerates more
	}
	if err != nil || f.Module == nil {
		return Module{}, nil, false
	}
	dir := filepath.Dir(gomod)
	rel, _ := filepath.Rel(repo, dir)
	m := Module{Path: f.Module.Mod.Path, Dir: filepath.ToSlash(rel)}
	if f.Go != nil {
		m.Go = f.Go.Version
	}
	for _, r := range f.Require {
		if !r.Indirect {
			m.Requires = append(m.Requires, Require{r.Mod.Path, r.Mod.Version})
		}
	}
	for _, r := range f.Replace {
		m.Replaces = append(m.Replaces, strings.TrimSpace(r.Old.String()+" => "+r.New.String()))
	}
	mains, imports := walkPackages(dir, true)
	m.Mains = mains
	delete(imports, m.Path)
	return m, imports, true
}

// Mains lists the directories (relative to dir) of the main packages of the
// module in dir: test files, vendor, testdata, examples and demos, hidden
// directories, nested modules and files built only with the "ignore" tag
// (go:generate helpers) do not count.
func Mains(dir string) []string {
	mains, _ := walkPackages(dir, false)
	return mains
}

// exampleDirs hold demo programs, not services (a library's examples/go/notifier).
var exampleDirs = map[string]bool{"example": true, "examples": true, "demo": true, "demos": true}

// walkPackages reads the package clause (and, with imports, the imports) of
// every Go file of the module in dir, without the nested modules.
func walkPackages(dir string, withImports bool) ([]string, map[string]bool) {
	fset := token.NewFileSet()
	mode := parser.PackageClauseOnly | parser.ParseComments
	if withImports {
		mode = parser.ImportsOnly | parser.ParseComments
	}
	mains, imports := map[string]bool{}, map[string]bool{}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if p != dir && (skipDir(name) || strings.HasPrefix(name, "_") || exampleDirs[name] ||
				fileExists(filepath.Join(p, "go.mod"))) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, mode)
		if err != nil || ignoredFile(f) {
			return nil
		}
		if f.Name.Name == "main" {
			rel, _ := filepath.Rel(dir, filepath.Dir(p))
			mains[filepath.ToSlash(rel)] = true
		}
		for _, s := range f.Imports {
			if path := strings.Trim(s.Path.Value, `"`); !standard(path) {
				imports[path] = true
			}
		}
		return nil
	})
	out := make([]string, 0, len(mains))
	for m := range mains {
		out = append(out, m)
	}
	sort.Strings(out)
	return out, imports
}

// standard reports whether an import path is of the standard library (no dot
// in its first element).
func standard(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

// ignoredFile reports whether a file's build constraint names the "ignore" tag.
func ignoredFile(f *ast.File) bool {
	for _, g := range f.Comments {
		if g.Pos() >= f.Package {
			break
		}
		for _, c := range g.List {
			if constraint.IsGoBuild(c.Text) || constraint.IsPlusBuild(c.Text) {
				if slices.Contains(strings.FieldsFunc(c.Text, func(r rune) bool {
					return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
				}), "ignore") {
					return true
				}
			}
		}
	}
	return false
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
