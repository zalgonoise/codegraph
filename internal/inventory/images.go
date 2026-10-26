package inventory

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Image is a container image a repository builds: named in its CI or compose
// files, without registry or tag.
type Image struct {
	Name       string `json:"name"`                 // "team/orders-api", "orders-api"; "*" stands for what a variable left unknown
	Tag        string `json:"tag,omitempty"`        // the tag as written: "release-*" (a branch), "latest"
	Dockerfile string `json:"dockerfile,omitempty"` // the Dockerfile it is built from, when the build names it
	File       string `json:"file"`                 // where: relative to the repository, with ":line"
	Via        string `json:"via"`                  // docker push, docker tag, docker build, build-push-action, workflow inputs, compose, Jenkins
}

// ciFiles are the files, relative to a repository, read for the images it
// builds.
var ciFiles = []string{
	"bitbucket-pipelines.yml", ".gitlab-ci.yml", ".gitlab/ci/*.yml", ".gitlab/ci/*.yaml",
	".github/workflows/*.yml", ".github/workflows/*.yaml", ".circleci/config.yml", "azure-pipelines.yml",
	"cloudbuild.yaml", "cloudbuild.yml", "Jenkinsfile", "Jenkinsfile.*", "Makefile", "*.mk", "*.sh",
	"scripts/*.sh", "ci/*.sh", ".ci/*.sh", "build/*.sh", "deploy/*.sh",
	"docker-compose*.yml", "docker-compose*.yaml", "compose*.yml", "compose*.yaml",
}

// Images lists the container images a repository's CI and compose files
// build, one per name, sorted. workflowImage is the organisation's convention
// for reusable CI workflows (Conventions.WorkflowImage).
func Images(dir, workflowImage string) []Image {
	seen := map[string]bool{}
	var files []string
	for _, pattern := range ciFiles {
		matches, _ := filepath.Glob(filepath.Join(dir, pattern))
		for _, m := range matches {
			if st, err := os.Stat(m); err == nil && !st.IsDir() && !seen[m] {
				seen[m] = true
				files = append(files, m)
			}
		}
	}
	sort.Strings(files)
	var found []Image
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil || len(data) > 1<<20 {
			continue
		}
		rel, _ := filepath.Rel(dir, f)
		found = append(found, fileImages(filepath.ToSlash(rel), string(data), workflowImage)...)
	}
	return bestImages(found)
}

// viaRank orders the ways an image is known, most telling first.
var viaRank = map[string]int{
	"docker push": 0, "build-push-action": 1, "workflow inputs": 1, "Jenkins": 1, "compose": 2, "docker tag": 3, "docker build": 4,
}

// bestImages keeps one image per name: the most telling way it is built,
// completed with a tag or a Dockerfile another way found.
func bestImages(found []Image) []Image {
	best := map[string]Image{}
	var names []string
	for _, im := range found {
		b, ok := best[im.Name]
		switch {
		case !ok:
			names = append(names, im.Name)
			best[im.Name] = im
		case viaRank[im.Via] < viaRank[b.Via]:
			if im.Dockerfile == "" {
				im.Dockerfile = b.Dockerfile
			}
			if im.Tag == "" {
				im.Tag = b.Tag
			}
			best[im.Name] = im
		default:
			if b.Dockerfile == "" {
				b.Dockerfile = im.Dockerfile
			}
			if b.Tag == "" {
				b.Tag = im.Tag
			}
			best[im.Name] = b
		}
	}
	sort.Strings(names)
	out := make([]Image, 0, len(names))
	for _, n := range names {
		out = append(out, best[n])
	}
	return out
}

func fileImages(rel, text, workflowImage string) []Image {
	vars := map[string]string{}
	var out []Image
	if ext := path.Ext(rel); ext == ".yml" || ext == ".yaml" {
		var doc yaml.Node
		if yaml.Unmarshal([]byte(text), &doc) == nil && len(doc.Content) > 0 {
			root := doc.Content[0]
			envVars(root, vars)
			switch {
			case strings.HasPrefix(rel, ".github/workflows/"):
				out = append(out, workflowImages(rel, root, vars, workflowImage)...)
			case strings.Contains(path.Base(rel), "compose"):
				out = append(out, composeImages(rel, root, vars)...)
			}
		}
	}
	return append(out, shellImages(rel, text, vars)...)
}

// envVars collects the scalar values of every mapping named variables or env
// (GitLab's variables, GitHub's env at any level), as the variables commands
// can expand.
func envVars(n *yaml.Node, vars map[string]string) {
	if n.Kind != yaml.MappingNode {
		for _, c := range n.Content {
			envVars(c, vars)
		}
		return
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if (k.Value == "variables" || k.Value == "env") && v.Kind == yaml.MappingNode {
			for j := 0; j+1 < len(v.Content); j += 2 {
				if s := scalar(v.Content[j+1]); s != "" {
					vars[v.Content[j].Value] = s
				}
			}
		}
		envVars(v, vars)
	}
}

// scalar is a node's value: a scalar's, or GitLab's {value: …} form.
func scalar(n *yaml.Node) string {
	if n.Kind == yaml.ScalarNode {
		return n.Value
	}
	if v := child(n, "value"); v != nil && v.Kind == yaml.ScalarNode {
		return v.Value
	}
	return ""
}

func child(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// childFold is child with the key compared ignoring case.
func childFold(n *yaml.Node, keys ...string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		for _, k := range keys {
			if strings.EqualFold(n.Content[i].Value, k) {
				return n.Content[i+1]
			}
		}
	}
	return nil
}

// workflowImages reads a GitHub workflow: jobs calling a reusable workflow,
// whose inputs name the image by the organisation's convention, and steps
// using docker/build-push-action, whose tags do.
func workflowImages(rel string, root *yaml.Node, vars map[string]string, template string) []Image {
	jobs := child(root, "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return nil
	}
	var out []Image
	for i := 0; i+1 < len(jobs.Content); i += 2 {
		key, job := jobs.Content[i], jobs.Content[i+1]
		if with := child(job, "with"); with != nil && template != "" && child(job, "uses") != nil {
			inputs := map[string]string{}
			for j := 0; j+1 < len(with.Content); j += 2 {
				inputs[with.Content[j].Value] = expand(scalar(with.Content[j+1]), vars, 0)
			}
			if name, ok := fill(template, inputs); ok {
				if im, ok := imageOf(name); ok {
					im.File, im.Via = lineOf(rel, key), "workflow inputs"
					if f := childFold(with, "dockerfile", "file"); f != nil {
						im.Dockerfile = expand(scalar(f), vars, 0)
					}
					out = append(out, im)
				}
			}
		}
		steps := child(job, "steps")
		if steps == nil {
			continue
		}
		for _, step := range steps.Content {
			uses := child(step, "uses")
			if uses == nil || !strings.HasPrefix(uses.Value, "docker/build-push-action") {
				continue
			}
			with := child(step, "with")
			tags := child(with, "tags")
			if tags == nil {
				continue
			}
			for _, ref := range strings.FieldsFunc(scalar(tags), func(r rune) bool { return r == '\n' || r == ',' }) {
				if im, ok := imageOf(expand(strings.TrimSpace(ref), vars, 0)); ok {
					im.File, im.Via = lineOf(rel, tags), "build-push-action"
					if f := child(with, "file"); f != nil {
						im.Dockerfile = expand(scalar(f), vars, 0)
					}
					out = append(out, im)
				}
			}
		}
	}
	return out
}

// fill replaces the {INPUT} placeholders of template with inputs, and fails
// when one is missing.
func fill(template string, inputs map[string]string) (string, bool) {
	ok := true
	out := regexp.MustCompile(`\{([^{}]+)\}`).ReplaceAllStringFunc(template, func(m string) string {
		v, found := inputs[m[1:len(m)-1]]
		if !found || v == "" {
			ok = false
		}
		return v
	})
	return out, ok
}

// composeImages reads a compose file: services that build an image and name it.
func composeImages(rel string, root *yaml.Node, vars map[string]string) []Image {
	services := child(root, "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return nil
	}
	var out []Image
	for i := 0; i+1 < len(services.Content); i += 2 {
		svc := services.Content[i+1]
		build, image := child(svc, "build"), child(svc, "image")
		if build == nil || image == nil {
			continue
		}
		if im, ok := imageOf(expand(image.Value, vars, 0)); ok {
			im.File, im.Via = lineOf(rel, image), "compose"
			if f := child(build, "dockerfile"); f != nil {
				im.Dockerfile = f.Value
			}
			out = append(out, im)
		}
	}
	return out
}

func lineOf(rel string, n *yaml.Node) string {
	return rel + ":" + strconv.Itoa(n.Line)
}

var (
	// assignShell: X=value, export X=value, and a YAML list item "- X=value".
	assignShell = regexp.MustCompile(`^\s*(?:-\s+)?(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)
	// assignMake: X = value, X := value, X ?= value.
	assignMake    = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)\s*[:?]?=\s*(.*)$`)
	jenkinsBuild  = regexp.MustCompile(`docker\.build\(\s*["']([^"']+)["']`)
	shellComments = regexp.MustCompile(`\s+#.*$`)
)

// shellImages follows the docker build, tag and push commands of a file's
// shell lines, in order, with the variables assigned before them expanded:
// what each build names (-t), each tag names, and each push pushes, and the
// Dockerfile a build used (-f), carried through tags to what is pushed.
func shellImages(rel, text string, vars map[string]string) []Image {
	isMake := path.Base(rel) == "Makefile" || path.Ext(rel) == ".mk"
	built := map[string]string{} // a reference as written -> the Dockerfile it was built from
	var out []Image
	for i, line := range strings.Split(text, "\n") {
		at := rel + ":" + strconv.Itoa(i+1)
		if m := assignShell.FindStringSubmatch(line); m != nil {
			vars[m[1]] = unquote(shellComments.ReplaceAllString(m[2], ""))
			continue
		}
		if isMake {
			if m := assignMake.FindStringSubmatch(line); m != nil && !strings.Contains(line, "\t") {
				vars[m[1]] = unquote(m[2])
				continue
			}
		}
		if m := jenkinsBuild.FindStringSubmatch(line); m != nil {
			if im, ok := imageOf(expand(m[1], vars, 0)); ok {
				im.File, im.Via = at, "Jenkins"
				out = append(out, im)
			}
			continue
		}
		words := fields(line)
		for j, w := range words {
			if w != "docker" && !strings.HasSuffix(w, "/docker") {
				continue
			}
			args := words[j+1:]
			if len(args) > 0 && args[0] == "buildx" {
				args = args[1:]
			}
			if len(args) > 1 && args[0] == "image" { // docker image push|tag|build
				args = args[1:]
			}
			if len(args) == 0 {
				continue
			}
			switch args[0] {
			case "build":
				tags, dockerfile := buildFlags(args[1:])
				for _, t := range tags {
					built[t] = dockerfile
					if im, ok := imageOf(expand(t, vars, 0)); ok {
						im.File, im.Via, im.Dockerfile = at, "docker build", dockerfile
						out = append(out, im)
					}
				}
			case "tag":
				refs := positional(args[1:])
				if len(refs) == 2 {
					built[refs[1]] = built[refs[0]]
					if im, ok := imageOf(expand(refs[1], vars, 0)); ok {
						im.File, im.Via, im.Dockerfile = at, "docker tag", built[refs[0]]
						out = append(out, im)
					}
				}
			case "push":
				for _, r := range positional(args[1:]) {
					if im, ok := imageOf(expand(r, vars, 0)); ok {
						im.File, im.Via, im.Dockerfile = at, "docker push", built[r]
						out = append(out, im)
					}
				}
			}
		}
	}
	return out
}

// buildFlags reads docker build's -t/--tag and -f/--file.
func buildFlags(args []string) (tags []string, dockerfile string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case (a == "-t" || a == "--tag") && i+1 < len(args):
			tags = append(tags, args[i+1])
			i++
		case strings.HasPrefix(a, "--tag="):
			tags = append(tags, strings.TrimPrefix(a, "--tag="))
		case strings.HasPrefix(a, "-t") && len(a) > 2 && !strings.HasPrefix(a, "--"):
			tags = append(tags, a[2:])
		case (a == "-f" || a == "--file") && i+1 < len(args):
			dockerfile = args[i+1]
			i++
		case strings.HasPrefix(a, "--file="):
			dockerfile = strings.TrimPrefix(a, "--file=")
		case a == "&&" || a == ";" || a == "|":
			return tags, dockerfile
		}
	}
	return tags, dockerfile
}

// positional lists a command's arguments up to the end of the command,
// leaving flags out.
func positional(args []string) []string {
	var out []string
	for _, a := range args {
		if a == "&&" || a == ";" || a == "|" || a == "||" {
			break
		}
		if !strings.HasPrefix(a, "-") {
			out = append(out, a)
		}
	}
	return out
}

// fields splits a shell line into words, keeping quoted strings whole and
// splitting command separators off.
func fields(line string) []string {
	var out []string
	var b strings.Builder
	quote := rune(0)
	depth := 0 // inside $( ) or ${ }
	flush := func() {
		if b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
	}
	rs := []rune(line)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				b.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote = r
		case r == '$' && i+1 < len(rs) && (rs[i+1] == '(' || rs[i+1] == '{'):
			depth++
			b.WriteRune(r)
			b.WriteRune(rs[i+1])
			i++
		case depth > 0 && (r == ')' || r == '}'):
			depth--
			b.WriteRune(r)
		case depth == 0 && (r == ' ' || r == '\t'):
			flush()
		case depth == 0 && (r == ';' || r == '|' || r == '&'):
			flush()
			out = append(out, string(r))
			if i+1 < len(rs) && rs[i+1] == r {
				out[len(out)-1] += string(r)
				i++
			}
		default:
			b.WriteRune(r)
		}
	}
	flush()
	return out
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}

var (
	ghExpr  = regexp.MustCompile(`\$\{\{\s*([^}]*?)\s*\}\}`)
	braced  = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::?-([^}]*))?\}`)
	parened = regexp.MustCompile(`\$\(([^()]*)\)`)
	bare    = regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*)`)
	// anyBraced is any ${…} the forms above leave.
	anyBraced = regexp.MustCompile(`\$\{[^{}]*\}`)
)

// expand replaces the variables of s with their values, and whatever it
// cannot know (a command's output, a secret, an unknown variable) with "*".
func expand(s string, vars map[string]string, depth int) string {
	if depth > 4 || !strings.Contains(s, "$") {
		return s
	}
	lookup := func(name string) string {
		if v, ok := vars[name]; ok {
			return expand(v, vars, depth+1)
		}
		return "*"
	}
	s = ghExpr.ReplaceAllStringFunc(s, func(m string) string {
		inner := ghExpr.FindStringSubmatch(m)[1]
		for _, p := range []string{"env.", "inputs.", "vars.", "github.event.inputs."} {
			if name, ok := strings.CutPrefix(inner, p); ok {
				return lookup(name)
			}
		}
		return "*"
	})
	s = braced.ReplaceAllStringFunc(s, func(m string) string {
		sub := braced.FindStringSubmatch(m)
		if v, ok := vars[sub[1]]; ok {
			return expand(v, vars, depth+1)
		}
		if strings.Contains(m, "-") && sub[2] != "" {
			return expand(sub[2], vars, depth+1)
		}
		return "*"
	})
	s = anyBraced.ReplaceAllStringFunc(s, func(m string) string { // ${env.X} (Groovy), other expressions
		if name, ok := strings.CutPrefix(m[2:len(m)-1], "env."); ok {
			return lookup(name)
		}
		return "*"
	})
	for i := 0; i < 4 && strings.Contains(s, "$("); i++ { // innermost first: Make's $(VAR), else a command's output
		s = parened.ReplaceAllStringFunc(s, func(m string) string {
			inner := parened.FindStringSubmatch(m)[1]
			if v, ok := vars[inner]; ok {
				return expand(v, vars, depth+1)
			}
			return "*"
		})
	}
	s = makeAuto.ReplaceAllString(s, "*") // Make's automatic variables: $*, $@, $<
	return bare.ReplaceAllStringFunc(s, func(m string) string { return lookup(m[1:]) })
}

var makeAuto = regexp.MustCompile(`\$[*@<^%?]`)

// imageOf reads an image reference into a name without registry or tag; it
// fails when nothing of the name is known.
func imageOf(ref string) (Image, bool) {
	ref = strings.TrimSpace(ref)
	var im Image
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref, im.Tag = ref[:i], ref[i+1:]
	}
	parts := strings.Split(ref, "/")
	for len(parts) > 1 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost" || strings.Contains(parts[0], "*")) {
		parts = parts[1:] // a registry, or the unknown variable holding it
	}
	name := strings.ToLower(strings.Join(parts, "/"))
	name = regexp.MustCompile(`\*+`).ReplaceAllString(name, "*")
	// A name whose last segment is unknown ("team/*") names nothing.
	if name == "" || strings.Trim(path.Base(name), "*-_.") == "" || strings.ContainsAny(name, " \t=,$") || strings.HasPrefix(name, "-") {
		return Image{}, false
	}
	return Image{Name: name, Tag: im.Tag}, true
}

// referenceFile reports whether a file (its name and path relative to the
// repository) may reference images it does not build: a Dockerfile, a YAML
// file (Helm values, Kubernetes manifests, compose files).
func referenceFile(name string) bool {
	return strings.HasPrefix(name, "Dockerfile") || strings.HasSuffix(name, ".dockerfile") ||
		strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}

// References lists the images a repository's files use without building
// them: base images (a Dockerfile's FROM), compose services' images, Helm
// values (image: {repository, tag}) and manifests (image: ref). files are
// relative to dir. Images the repository builds are left out.
func References(dir string, files []string, builds []Image) []Image {
	own := map[string]bool{}
	for _, im := range builds {
		own[im.Name] = true
	}
	var found []Image
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil || len(data) > 512<<10 {
			continue
		}
		rel = filepath.ToSlash(rel)
		if name := path.Base(rel); strings.HasPrefix(name, "Dockerfile") || strings.HasSuffix(name, ".dockerfile") {
			found = append(found, fromImages(rel, string(data))...)
			continue
		}
		if strings.Contains(string(data), "{{") { // a template: rendered elsewhere
			continue
		}
		var doc yaml.Node
		if yaml.Unmarshal(data, &doc) != nil || len(doc.Content) == 0 {
			continue
		}
		via := "manifest"
		switch base := path.Base(rel); {
		case base == "bitbucket-pipelines.yml" || base == ".gitlab-ci.yml" || strings.HasPrefix(rel, ".gitlab/") ||
			strings.HasPrefix(base, "cloudbuild.") || base == "azure-pipelines.yml":
			via = "CI" // the image a pipeline step runs in
		case strings.HasPrefix(base, "values"):
			via = "helm values"
		case strings.Contains(base, "compose"):
			via = "compose"
		}
		yamlImages(rel, doc.Content[0], via, &found)
	}
	var out []Image
	for _, im := range bestImages(found) {
		if !own[im.Name] {
			out = append(out, im)
		}
	}
	return out
}

var fromLine = regexp.MustCompile(`(?i)^\s*FROM\s+(?:--platform=\S+\s+)?(\S+)(?:\s+AS\s+(\S+))?`)

// fromImages reads a Dockerfile's base images, with its ARG defaults
// expanded and its own stages left out.
func fromImages(rel, text string) []Image {
	args, stages := map[string]string{}, map[string]bool{}
	var out []Image
	for i, line := range strings.Split(text, "\n") {
		if f := strings.Fields(line); len(f) >= 2 && strings.EqualFold(f[0], "ARG") {
			if k, v, ok := strings.Cut(f[1], "="); ok {
				args[k] = unquote(v)
			}
			continue
		}
		m := fromLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		ref := expand(m[1], args, 0)
		if !stages[strings.ToLower(ref)] && !strings.EqualFold(ref, "scratch") {
			if im, ok := imageOf(ref); ok {
				im.File, im.Via = rel+":"+strconv.Itoa(i+1), "FROM"
				out = append(out, im)
			}
		}
		if m[2] != "" {
			stages[strings.ToLower(m[2])] = true
		}
	}
	return out
}

// yamlImages collects image: references from a YAML tree: a scalar that
// looks like an image reference, or a mapping with a repository (Helm's
// image: {registry, repository, tag}). A compose service that builds its
// image is the compose file's own.
func yamlImages(rel string, n *yaml.Node, via string, out *[]Image) {
	if n.Kind == yaml.MappingNode {
		if child(n, "build") != nil && child(n, "image") != nil {
			return // built here (Images has it)
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Value == "image" {
				ref, tag, line := "", "", v.Line
				switch v.Kind {
				case yaml.ScalarNode:
					ref = v.Value
				case yaml.MappingNode:
					if r := child(v, "repository"); r != nil && r.Kind == yaml.ScalarNode {
						ref, line = r.Value, r.Line
						if t := child(v, "tag"); t != nil && t.Kind == yaml.ScalarNode && t.Value != "" {
							tag = t.Value
						}
					}
				}
				if looksLikeImage(ref) {
					if im, ok := imageOf(ref); ok {
						if tag != "" {
							im.Tag = tag
						}
						im.File, im.Via = rel+":"+strconv.Itoa(line), via
						*out = append(*out, im)
					}
				}
			}
			yamlImages(rel, v, via, out)
		}
		return
	}
	for _, c := range n.Content {
		yamlImages(rel, c, via, out)
	}
}

// looksLikeImage leaves out values an image: key holds that are no image
// reference (flags, empty values, sentences).
func looksLikeImage(s string) bool {
	if s == "" || strings.ContainsAny(s, " \t$") || strings.EqualFold(s, "true") || strings.EqualFold(s, "false") {
		return false
	}
	return regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]*$`).MatchString(s)
}
