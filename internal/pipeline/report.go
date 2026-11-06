package pipeline

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/zalgonoise/codegraph/internal/configfile"
	"github.com/zalgonoise/codegraph/internal/declared"
	"github.com/zalgonoise/codegraph/internal/incremental"
	"github.com/zalgonoise/codegraph/internal/options"
	"github.com/zalgonoise/codegraph/internal/transport"
)

// Describe writes what the plan resolves to: the configuration file, the
// profile, the steps, the run query and mcp read, and every option with the
// layer it comes from.
func Describe(w io.Writer, p *Plan) error {
	o, sources, err := p.Sources()
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "configuration  %s\n", p.Config.Describe())
	fmt.Fprintf(w, "profile        %s\n", p.describeProfile())
	if p.Name != "" {
		steps := make([]string, len(p.Steps))
		for i, s := range p.Steps {
			steps[i] = s.String()
		}
		fmt.Fprintf(w, "pipeline       %s: %s\n", p.Name, strings.Join(steps, ", "))
	} else if p.Config != nil {
		if p.Config.Default != "" {
			fmt.Fprintf(w, "pipeline       %s runs by default (codegraph alone); -pipeline <name> shows a pipeline's settings\n", p.Config.Default)
		}
	}
	if names := p.Config.Names(); len(names) > 0 {
		fmt.Fprintf(w, "pipelines      %s\n", strings.Join(names, ", "))
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "option\tvalue\tfrom")
	for _, f := range options.Fields() {
		v, _ := o.Get(f.Name)
		if v == "" {
			v = "-"
		}
		src := sources[f.Name]
		if src == "" {
			src = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", f.Name, v, src)
	}
	return tw.Flush()
}

func (p *Plan) describeProfile() string {
	switch {
	case p.Profile.Dir == "":
		return "none (-profile, $CODEGRAPH_PROFILE or the configuration file's profile names one)"
	case p.Profile.File == "":
		return fmt.Sprintf("%s, without profile.yaml (from %s)", p.Profile.Dir, p.ProfileFrom)
	}
	return fmt.Sprintf("%s (from %s)", p.Profile.File, p.ProfileFrom)
}

// Get returns one resolved value, for scripts (the Makefile): an option, or
// profile, config or pipelines.
func Get(p *Plan, key string) (string, error) {
	switch key {
	case "profile":
		return p.Profile.Dir, nil
	case "config":
		if p.Config == nil {
			return "", nil
		}
		return p.Config.Path, nil
	case "pipelines":
		return strings.Join(p.Config.Names(), ","), nil
	}
	o, err := p.Options(nil)
	if err != nil {
		return "", err
	}
	v, ok := o.Get(key)
	if !ok {
		names := []string{"profile", "config", "pipelines"}
		for _, f := range options.Fields() {
			names = append(names, f.Name)
		}
		hint := ""
		if s := options.Suggest(key, names); s != "" {
			hint = fmt.Sprintf(" (did you mean %s?)", s)
		}
		return "", fmt.Errorf("no setting %q%s", key, hint)
	}
	return v, nil
}

// Check checks what the plan needs: the tools, the files its options name,
// the repositories it selects, whether it may run its commands. It writes one
// line per finding and returns false when something would make the run fail.
func Check(w io.Writer, p *Plan) bool {
	o, err := p.Options(nil)
	if err != nil {
		fmt.Fprintf(w, "fail  %v\n", err)
		return false
	}
	ok := true
	line := func(level, format string, args ...any) {
		if level == "fail" {
			ok = false
		}
		fmt.Fprintf(w, "%-5s %s\n", level, fmt.Sprintf(format, args...))
	}
	line("info", "configuration: %s", p.Config.Describe())
	line("info", "profile: %s", p.describeProfile())
	for _, warn := range p.Warnings {
		line("warn", "%s", warn)
	}
	if p.Name != "" {
		line("info", "pipeline %s: %d steps", p.Name, len(p.Steps))
	}

	if v, err := exec.Command("go", "env", "GOVERSION").Output(); err != nil {
		line("fail", "go: not installed; the analysis needs the go command")
	} else {
		line("ok", "go: %s", strings.TrimSpace(string(v)))
	}
	if hasTool("git") {
		line("ok", "git: installed")
	} else {
		line("fail", "git: not installed; runs check that no clone changes with it")
	}
	if hasTool("pkg-config") {
		line("ok", "pkg-config: installed")
	} else {
		line("warn", "pkg-config: missing, so cgo repositories needing C libraries will fail (on macOS: brew install pkgconf)")
	}

	root := options.Str(o.Root)
	if !isDir(root) {
		line("fail", "root %s: not a directory", root)
	}
	repos, from, err := Repositories(o)
	switch {
	case err != nil:
		line("fail", "repositories: %v", err)
	case len(repos) == 0:
		line("fail", "repositories: none %s", from)
	default:
		line("ok", "repositories: %d %s", len(repos), from)
	}
	if !options.IsNone(o.Focus) && options.List(o.Repos) == nil {
		names, _ := readNames(options.Str(o.Focus))
		var missing []string
		for _, n := range names {
			if findRepo(root, n) == "" {
				missing = append(missing, n)
			}
		}
		if len(missing) > 0 {
			line("warn", "focus: %d of %d names have no Go repository in %s: %s", len(missing), len(names), root, list(missing, 10))
		}
	}
	if inv := filepath.Join(options.Str(o.Inventory), "inventory.json"); exists(inv) {
		line("ok", "inventory: %s (%s)", inv, age(inv))
	} else if !options.IsNone(o.Focus) {
		line("info", "inventory: none yet in %s (codegraph run inventory finds the frontier around focus)", options.Str(o.Inventory))
	}

	for _, f := range options.List(o.Rules) {
		if _, err := transport.LoadRules([]string{f}, false); err != nil {
			line("fail", "rules: %v", err)
		} else {
			line("ok", "rules: %s", f)
		}
	}
	if d := options.Str(o.RepoRules); d != "" {
		files, _ := filepath.Glob(filepath.Join(d, "*.json"))
		bad := 0
		for _, f := range files {
			if _, err := transport.LoadRules([]string{f}, false); err != nil {
				line("fail", "repoRules: %v", err)
				bad++
			}
		}
		if !isDir(d) {
			line("fail", "repoRules %s: not a directory", d)
		} else if bad == 0 {
			line("ok", "repoRules: %s in %s", count(len(files), "file"), d)
		}
	}
	if d := options.Str(o.Declared); d != "" {
		if files, err := declared.ReadDir(d); err != nil {
			line("fail", "declared: %v", err)
		} else {
			line("ok", "declared: %s in %s", count(len(files), "service"), d)
		}
	}
	if chart := options.Str(o.Chart); !options.IsNone(o.Chart) {
		switch {
		case !isDir(chart):
			line("warn", "chart %s: not a directory, so graph skips the deployment", chart)
		case !hasTool("helm"):
			line("warn", "helm: not installed, so graph skips the deployment of %s (on macOS: brew install helm)", chart)
		default:
			line("ok", "chart: %s (%s)", chart, options.Str(o.Environment))
		}
	}
	if p.Name == "" {
		var names []string
		for _, c := range options.List(o.Checks) {
			names = append(names, c.Name)
		}
		if len(names) > 0 {
			line("info", "checks after each graph: %s", strings.Join(names, ", "))
		}
	}
	if cmds := p.Commands(); len(cmds) > 0 {
		if err := p.checkTrust(); err != nil {
			line("fail", "%v", err)
		} else {
			line("ok", "commands: %s", strings.Join(cmds, "; "))
		}
		if !hasTool("sh") {
			line("fail", "sh: not installed; the commands run with it")
		}
	}
	for _, s := range p.Steps {
		if s.Kind == "exec" {
			if prog := strings.Fields(s.Command); len(prog) > 0 && !strings.ContainsAny(prog[0], "=$(") && !hasTool(prog[0]) {
				line("warn", "exec: %s is not installed or not in PATH", prog[0])
			}
		}
	}
	out := options.Str(o.Out)
	if !isDir(filepath.Dir(out)) {
		line("warn", "out %s: its directory does not exist yet", out)
	}
	if d := options.Str(o.Denylist); d != "" && !isFile(d) {
		line("warn", "denylist %s: not found", d)
	}
	if hasManifests(out) {
		line("info", "last run: %s (codegraph status tells more)", out)
	} else {
		line("info", "no run in %s yet", out)
	}
	return ok
}

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func list(names []string, n int) string {
	if len(names) <= n {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:n], ", ") + fmt.Sprintf(", … (%d more)", len(names)-n)
}

func age(file string) string {
	st, err := os.Stat(file)
	if err != nil {
		return "?"
	}
	return since(st.ModTime())
}

func since(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

// Status writes how current the plan's run is: when and by what it was made,
// what it found, which repositories moved since, and the inventory, the
// deployment and the bundle next to it.
func Status(w io.Writer, p *Plan) error {
	o, err := p.Options(nil)
	if err != nil {
		return err
	}
	out := options.Str(o.Out)
	if !hasManifests(out) {
		fmt.Fprintf(w, "No run in %s yet: codegraph run graph makes one.\n", out)
		return nil
	}
	rec, err := incremental.Read(out)
	if err != nil {
		return err
	}
	made := time.Time{}
	if rec == nil {
		if st, err := os.Stat(filepath.Join(out, "_system")); err == nil {
			made = st.ModTime()
		}
		ms, _ := filepath.Glob(filepath.Join(out, "*", "manifest.json"))
		fmt.Fprintf(w, "Run %s: made %s, %d services. It predates %s, so which repositories moved since is unknown: the next graph records it.\n",
			out, since(made), len(ms), incremental.File)
	} else {
		made = rec.Created
		reused, analysed := rec.Reused()
		fmt.Fprintf(w, "Run %s: made %s (%s) by codegraph %s.\n", out, since(made), made.Local().Format("2006-01-02 15:04"), rec.Codegraph)
		fmt.Fprintf(w, "  %d services, %d links, %d unmatched endpoints; %d repositories analysed, %d reused from the run before.\n",
			rec.Services, rec.Links, rec.Unmatched, analysed, reused)
		var moved, gone []string
		changed := 0
		for _, repo := range rec.Repos {
			if repo.Head == "" {
				continue
			}
			head, err := exec.Command("git", "-C", repo.Dir, "rev-parse", "HEAD").Output()
			switch {
			case err != nil:
				gone = append(gone, filepath.Base(repo.Dir))
			case strings.TrimSpace(string(head)) != repo.Head:
				moved = append(moved, strings.TrimSuffix(filepath.Base(repo.Dir), ".git"))
			}
			if err == nil {
				if st, _ := exec.Command("git", "-C", repo.Dir, "status", "--porcelain", "--", ".").Output(); len(strings.TrimSpace(string(st))) > 0 {
					changed++
				}
			}
		}
		switch {
		case len(moved) == 0:
			fmt.Fprintln(w, "  No repository has a new commit since.")
		default:
			fmt.Fprintf(w, "  %d repositories have new commits since: %s.\n", len(moved), list(moved, 12))
			fmt.Fprintf(w, "  The next graph analyses them again and reuses the other %d.\n", max(0, len(rec.Repos)-len(moved)-len(gone)))
		}
		if changed > 0 {
			fmt.Fprintf(w, "  %d repositories have uncommitted changes, analysed again every time.\n", changed)
		}
		if len(gone) > 0 {
			fmt.Fprintf(w, "  %d repositories are gone or not git clones any more: %s.\n", len(gone), list(gone, 12))
		}
	}
	if data, err := os.ReadFile(filepath.Join(out, "diff.md")); err == nil {
		switch m := linksLine.FindSubmatch(data); {
		case strings.Contains(string(data), "No architectural change"):
			fmt.Fprintln(w, "  Against the run before: no architectural change.")
		case m != nil:
			fmt.Fprintf(w, "  Against the run before: %s links added, %s removed (%s).\n", m[1], m[2], filepath.Join(out, "diff.md"))
		}
	}
	if inv := filepath.Join(options.Str(o.Inventory), "inventory.json"); exists(inv) {
		var head struct{ Codegraph string }
		data, _ := os.ReadFile(inv)
		_ = json.Unmarshal(data, &head)
		by := ""
		if head.Codegraph != "" {
			by = " by codegraph " + head.Codegraph
		}
		fmt.Fprintf(w, "Inventory %s: made %s%s.\n", inv, age(inv), by)
	}
	if dep := filepath.Join(out, "deploy", "deploy.json"); exists(dep) {
		var head struct{ Environment string }
		data, _ := os.ReadFile(dep)
		_ = json.Unmarshal(data, &head)
		fmt.Fprintf(w, "Deployment of %s: read %s.\n", head.Environment, age(dep))
	}
	if b := options.Str(o.Bundle); b != "" {
		if st, err := os.Stat(b); err == nil {
			note := ""
			if !made.IsZero() && st.ModTime().Before(made) {
				note = ", older than the run (codegraph run bundle packs it again)"
			}
			fmt.Fprintf(w, "Bundle %s: %s, made %s%s.\n", b, size(st.Size()), since(st.ModTime()), note)
		}
	}
	return nil
}

// DefaultName is the pipeline the configuration file runs by default, if any.
func DefaultName(cf *configfile.File) string {
	if cf == nil {
		return ""
	}
	return cf.Default
}
