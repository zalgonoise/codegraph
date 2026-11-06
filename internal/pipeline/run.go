package pipeline

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zalgonoise/codegraph/internal/configfile"
	"github.com/zalgonoise/codegraph/internal/incremental"
	"github.com/zalgonoise/codegraph/internal/options"
)

// Runner runs plans.
type Runner struct {
	// Dispatch runs a codegraph command in this process (codegraph's main).
	Dispatch func(args []string) error
	// DryRun writes what would run, as a shell script, to Out instead.
	DryRun bool
	Out    io.Writer // the dry run's script; os.Stdout when nil
	// Log reports progress (codegraph's log, on standard error).
	Log func(format string, args ...any)
}

// Result is what one step did.
type Result struct {
	Step string
	Took time.Duration
	Note string
	Err  error
}

// Run runs the plan's steps in order, stopping at the first that fails, and
// logs a summary of them.
func (r *Runner) Run(p *Plan) ([]Result, error) {
	if err := p.checkTrust(); err != nil {
		return nil, err
	}
	if r.DryRun {
		r.printf("# codegraph run %s (dry run): what it would run, as a shell script\n", p.Name)
	}
	start := time.Now()
	var results []Result
	var err error
	for i := range p.Steps {
		s := &p.Steps[i]
		if r.DryRun {
			r.printf("\n# step %d: %s\n", i+1, s)
		}
		t := time.Now()
		res := Result{Step: s.String()}
		var o *options.Options
		if o, err = p.Options(s); err == nil {
			res.Note, err = r.step(p, s, o)
		}
		res.Took, res.Err = time.Since(t), err
		results = append(results, res)
		if err != nil {
			err = fmt.Errorf("%s: %w", s.Kind, err)
			break
		}
	}
	if !r.DryRun {
		r.summary(p, results, time.Since(start))
	}
	return results, err
}

func (r *Runner) summary(p *Plan, results []Result, took time.Duration) {
	r.Log("run %s: %d of %d steps in %s", p.Name, len(results), len(p.Steps), round(took))
	width := 0
	for _, res := range results {
		width = max(width, len(stepName(res.Step)))
	}
	for _, res := range results {
		note := res.Note
		if res.Err != nil {
			note = "failed: " + res.Err.Error()
		}
		r.Log("  %-*s %7s  %s", width, stepName(res.Step), round(res.Took), note)
	}
}

func stepName(s string) string {
	if strings.HasPrefix(s, "exec: ") {
		return "exec"
	}
	return s
}

func round(d time.Duration) string {
	if d < time.Second {
		return d.Round(10 * time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

// note reports what a step decided: a comment of the dry run's script, else
// the log.
func (r *Runner) note(format string, args ...any) {
	if r.DryRun {
		r.printf("# "+format+"\n", args...)
		return
	}
	r.Log(format, args...)
}

func (r *Runner) printf(format string, args ...any) {
	w := r.Out
	if w == nil {
		w = os.Stdout
	}
	fmt.Fprintf(w, format, args...)
}

func (r *Runner) step(p *Plan, s *configfile.Step, o *options.Options) (string, error) {
	switch s.Kind {
	case "exec":
		return r.exec(p, s, o)
	case "inventory":
		return r.inventory(p, o)
	case "graph":
		return r.graph(p, o)
	case "deploy":
		return r.deploy(o)
	case "checks":
		return r.checks(p, o)
	case "diff":
		return r.diff(o, options.Str(o.Base))
	case "bundle":
		return r.bundle(o)
	}
	return "", fmt.Errorf("unknown step %q", s.Kind)
}

// command runs a codegraph command, with env set for its duration.
func (r *Runner) command(env []string, args ...string) error {
	text := strings.TrimSpace(strings.Join(env, " ") + " codegraph " + shellJoin(args))
	if r.DryRun {
		r.printf("%s\n", text)
		return nil
	}
	r.Log("%s", text)
	restore := setenv(env)
	defer restore()
	return r.Dispatch(args)
}

// setenv sets NAME=value pairs and returns what restores them.
func setenv(env []string) func() {
	var undo []func()
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		old, had := os.LookupEnv(k)
		_ = os.Setenv(k, v)
		undo = append(undo, func() {
			if had {
				_ = os.Setenv(k, old)
			} else {
				_ = os.Unsetenv(k)
			}
		})
	}
	return func() {
		for _, u := range undo {
			u()
		}
	}
}

// shell runs a command of the person's (sh -c) in dir, its output to stdout
// (the terminal when nil), which a dry run shows as a redirection to output.
func (r *Runner) shell(dir string, env []string, script string, stdout io.Writer, output string) error {
	if r.DryRun {
		redirect := ""
		if output != "" {
			redirect = " > " + shellQuote(output)
		}
		r.printf("(cd %s && %s sh -c %s)%s\n", shellQuote(dir), strings.Join(env, " "), shellQuote(script), redirect)
		return nil
	}
	r.Log("(in %s) %s", dir, firstLine(script))
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir, cmd.Env = dir, append(os.Environ(), env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if stdout != nil {
		cmd.Stdout = stdout
	}
	return cmd.Run()
}

// action does something to the files that make's recipes did in shell (shown
// as that shell in a dry run).
func (r *Runner) action(shell string, fn func() error) error {
	if r.DryRun {
		r.printf("%s\n", shell)
		return nil
	}
	return fn()
}

func (r *Runner) exec(p *Plan, s *configfile.Step, o *options.Options) (string, error) {
	env := []string{"CODEGRAPH_RUN=" + options.Str(o.Out), "CODEGRAPH_ROOT=" + options.Str(o.Root)}
	if err := r.shell(p.Dir, env, s.Command, nil, ""); err != nil {
		return "", err
	}
	return firstLine(s.Command), nil
}

func (r *Runner) inventory(p *Plan, o *options.Options) (string, error) {
	out, inv := options.Str(o.Out), options.Str(o.Inventory)
	system := filepath.Join(out, "_system", "system.json")
	focus := !options.IsNone(o.Focus)
	if focus && !exists(system) {
		r.note("no run in %s: the frontier is found from the inventory alone (graph, then inventory again, is more precise)", out)
	}
	args := []string{"inventory", "-root", options.Str(o.Root), "-out", inv}
	if p.Profile.Dir != "" {
		args = append(args, "-profile", p.Profile.Dir)
	}
	if focus {
		args = append(args, "-focus", options.Str(o.Focus), "-rings", fmt.Sprint(options.Int(o.Rings)))
	}
	if exists(system) {
		args = append(args, "-run", out)
	}
	if err := r.command(nil, args...); err != nil {
		return "", err
	}
	if r.DryRun {
		return "", nil
	}
	note := filepath.Join(inv, "inventory.md")
	var counts struct{ Repos []json.RawMessage }
	if data, err := os.ReadFile(filepath.Join(inv, "inventory.json")); err == nil && json.Unmarshal(data, &counts) == nil {
		note = fmt.Sprintf("%d repositories", len(counts.Repos))
	}
	if focus {
		names, _ := readNames(filepath.Join(inv, "frontier.txt"))
		note += fmt.Sprintf(", %d in the frontier", len(names))
	}
	return note + " (" + inv + ")", nil
}

func (r *Runner) graph(p *Plan, o *options.Options) (string, error) {
	repos, from, err := Repositories(o)
	if err != nil {
		return "", err
	}
	if len(repos) == 0 {
		return "", fmt.Errorf("no Go repositories %s (set root or repos)", from)
	}
	if _, err := exec.LookPath("go"); err != nil {
		return "", errors.New("go is not installed: the analysis needs the go command")
	}
	if _, err := exec.LookPath("pkg-config"); err != nil && !r.DryRun {
		r.Log("warning: pkg-config is missing, so cgo repositories needing C libraries will fail; on macOS: brew install pkgconf")
	}
	out := options.Str(o.Out)
	prev := out + ".prev"
	r.note("%d repositories %s -> %s", len(repos), from, out)
	hadRun := exists(filepath.Join(out, "_system"))
	if hadRun {
		if err := r.action(fmt.Sprintf("rm -rf %s && mv %s %s", shellQuote(prev), shellQuote(out), shellQuote(prev)), func() error {
			if err := os.RemoveAll(prev); err != nil {
				return err
			}
			return os.Rename(out, prev)
		}); err != nil {
			return "", err
		}
	}
	if err := r.action("mkdir -p "+shellQuote(out), func() error { return os.MkdirAll(out, 0o755) }); err != nil {
		return "", err
	}
	var before map[string]string
	if !r.DryRun {
		before = gitStatus(repos)
	}

	args := []string{"fleet", "-algo", options.Str(o.Algo)}
	for _, f := range options.List(o.Rules) {
		args = append(args, "-rules", f)
	}
	if d := options.Str(o.RepoRules); d != "" {
		args = append(args, "-repo-rules", d)
	}
	if d := options.Str(o.Declared); d != "" {
		if files, _ := filepath.Glob(filepath.Join(d, "*.json")); len(files) > 0 {
			args = append(args, "-declared", d)
		}
	}
	for _, m := range options.List(o.Include) {
		args = append(args, "-include-modules", m)
	}
	if options.Bool(o.Libraries) {
		args = append(args, "-libraries")
	}
	if options.Bool(o.API) {
		args = append(args, "-api")
	}
	args = append(args, "-format", strings.Join(options.List(o.Formats), ","), "-out", out)
	if !options.Bool(o.Full) && (hadRun || exists(filepath.Join(prev, incremental.File))) {
		args = append(args, "-reuse", prev)
	}
	args = append(args, repos...)
	env := []string{"GOFLAGS=-mod=readonly"}
	if gp := options.Str(o.GoPrivate); gp != "" {
		env = append([]string{"GOPRIVATE=" + gp}, env...)
	}
	if err := r.command(env, args...); err != nil {
		return "", err
	}
	if !r.DryRun {
		if err := statusChanged(before, gitStatus(repos), "a repository's git status changed during the run. codegraph never writes to them: if someone edited a clone meanwhile, run again"); err != nil {
			return "", err
		}
	}

	var notes []string
	if rec, _ := incremental.Read(out); rec != nil {
		reused, analysed := rec.Reused()
		n := fmt.Sprintf("%d services, %d links, %d unmatched; %d repositories analysed", rec.Services, rec.Links, rec.Unmatched, analysed)
		if reused > 0 {
			n += fmt.Sprintf(", %d reused", reused)
		}
		notes = append(notes, n)
	}
	switch chart := options.Str(o.Chart); {
	case options.IsNone(o.Chart):
	case !hasTool("helm") || !isDir(chart):
		r.note("deployment skipped (needs helm and %s)", chart)
		notes = append(notes, "no deployment (needs helm and the chart)")
	default:
		note, err := r.deploy(o)
		if err != nil {
			return "", fmt.Errorf("deploy: %w", err)
		}
		notes = append(notes, note)
	}
	if len(options.List(o.Checks)) > 0 {
		note, err := r.checks(p, o)
		if err != nil {
			return "", fmt.Errorf("checks: %w", err)
		}
		notes = append(notes, note)
	}
	if hadRun || hasManifests(prev) {
		note, err := r.diff(o, prev)
		if err != nil {
			return "", fmt.Errorf("diff: %w", err)
		}
		notes = append(notes, note)
	}
	if !r.DryRun {
		r.Log("explorer: %s", filepath.Join(out, "_system", "system.html"))
	}
	return strings.Join(notes, "; "), nil
}

func (r *Runner) deploy(o *options.Options) (string, error) {
	chart, out, env := options.Str(o.Chart), options.Str(o.Out), options.Str(o.Environment)
	if !hasTool("helm") {
		return "", errors.New("helm is not installed (on macOS: brew install helm)")
	}
	if options.IsNone(o.Chart) || !isDir(chart) {
		return "", fmt.Errorf("no chart in %q (set chart)", chart)
	}
	var before map[string]string
	if !r.DryRun {
		before = gitStatus([]string{chart})
	}
	args := []string{"deploy", "-chart", chart, "-env", env, "-out", filepath.Join(out, "deploy")}
	if inv := filepath.Join(options.Str(o.Inventory), "inventory.json"); exists(inv) {
		args = append(args, "-inventory", inv)
	}
	runs := hasManifests(out) || r.DryRun
	if runs {
		args = append(args, "-run", out)
	}
	if err := r.command(nil, args...); err != nil {
		return "", err
	}
	if !r.DryRun {
		if err := statusChanged(before, gitStatus([]string{chart}), "the chart's git status changed during the run"); err != nil {
			return "", err
		}
	}
	report := filepath.Join(out, "deploy", "deploy.md")
	if runs { // the explorer again, with what the environment runs
		if err := r.command(nil, "link", "-format", "html", "-deploy", filepath.Join(out, "deploy", "deploy.json"),
			"-out", filepath.Join(out, "_system"), filepath.Join(out, "*", "manifest.json")); err != nil {
			return "", err
		}
	}
	return env + " (" + report + ")", nil
}

func (r *Runner) checks(p *Plan, o *options.Options) (string, error) {
	out := options.Str(o.Out)
	if !r.DryRun && !exists(filepath.Join(out, "_system", "system.json")) {
		return "", fmt.Errorf("no run in %s yet: run graph first", out)
	}
	checks := options.List(o.Checks)
	if len(checks) == 0 {
		return "no checks", nil
	}
	env := []string{"CODEGRAPH_RUN=" + out}
	if p.Profile.Dir != "" {
		env = append(env, "CODEGRAPH_PROFILE="+p.Profile.Dir)
	}
	var notes []string
	for _, c := range checks {
		var buf bytes.Buffer
		var stdout io.Writer
		output := ""
		if c.Output != "" {
			stdout, output = &buf, filepath.Join(out, c.Output)
		}
		if err := r.shell(c.Dir, env, c.Run, stdout, output); err != nil {
			return "", fmt.Errorf("%s: %w", c.Name, err)
		}
		note := c.Name
		if c.Output != "" && buf.Len() > 0 && !r.DryRun {
			file := filepath.Join(out, c.Output)
			if err := os.WriteFile(file, buf.Bytes(), 0o644); err != nil {
				return "", err
			}
			r.Log("%s: %s", c.Name, file)
			note += " (" + file + ")"
		}
		notes = append(notes, note)
	}
	return "checks: " + strings.Join(notes, ", "), nil
}

var linksLine = regexp.MustCompile(`## Cross-service links \((\d+) added, (\d+) removed\)`)

func (r *Runner) diff(o *options.Options, base string) (string, error) {
	out := options.Str(o.Out)
	if !r.DryRun && !hasManifests(base) {
		return "", fmt.Errorf("no run to compare with in %s (set base)", base)
	}
	report := filepath.Join(out, "diff.md")
	if err := r.command(nil, "diff", "-base", base, "-head", out, "-out", report); err != nil {
		return "", err
	}
	data, _ := os.ReadFile(report)
	switch m := linksLine.FindSubmatch(data); {
	case bytes.Contains(data, []byte("No architectural change")):
		return "no change since the run before", nil
	case m != nil:
		return fmt.Sprintf("+%s −%s links since the run before (%s)", m[1], m[2], report), nil
	}
	return "changes: " + report, nil
}

func (r *Runner) bundle(o *options.Options) (string, error) {
	out, file := options.Str(o.Out), options.Str(o.Bundle)
	if !r.DryRun && !hasManifests(out) {
		return "", fmt.Errorf("no run in %s yet: run graph first", out)
	}
	args := []string{"bundle", "-run", out}
	if inv := filepath.Join(options.Str(o.Inventory), "inventory.json"); exists(inv) {
		args = append(args, "-inventory", inv)
	}
	args = append(args, "-out", file)
	if c := options.Str(o.Compress); c != "" {
		args = append(args, "-compress", c)
	}
	if options.Bool(o.NoSettings) {
		args = append(args, "-no-settings")
	}
	if options.Bool(o.NoPositions) {
		args = append(args, "-no-positions")
	}
	if err := r.command(nil, args...); err != nil {
		return "", err
	}
	if st, err := os.Stat(file); err == nil {
		return fmt.Sprintf("%s (%s)", file, size(st.Size())), nil
	}
	return file, nil
}

func size(n int64) string {
	if n < 1<<20 {
		return fmt.Sprintf("%d KB", (n+1023)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

// Repositories selects the repositories a graph analyses: repos (names in
// root, or paths), else those focus and the inventory's frontier.txt name,
// else every Go repository of root. A name is <root>/<name>.git, else
// <root>/<name>; only directories with a go.mod count. It also says where
// they come from.
func Repositories(o *options.Options) (repos []string, from string, err error) {
	root := options.Str(o.Root)
	seen := map[string]bool{}
	add := func(dir string) {
		if dir != "" && !seen[dir] {
			seen[dir] = true
			repos = append(repos, dir)
		}
	}
	switch {
	case len(options.List(o.Repos)) > 0:
		for _, r := range options.List(o.Repos) {
			if strings.ContainsRune(r, '/') {
				if isFile(filepath.Join(r, "go.mod")) {
					add(r)
				}
				continue
			}
			add(findRepo(root, r))
		}
		return repos, "given", nil
	case !options.IsNone(o.Focus):
		focus := options.Str(o.Focus)
		names, err := readNames(focus)
		if err != nil {
			return nil, "", fmt.Errorf("focus: %w", err)
		}
		from = fmt.Sprintf("from %s (%s", root, focus)
		frontier := filepath.Join(options.Str(o.Inventory), "frontier.txt")
		if more, err := readNames(frontier); err == nil {
			names = append(names, more...)
			from += " and " + frontier
		}
		for _, n := range names {
			add(findRepo(root, n))
		}
		return repos, from + ")", nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, "", fmt.Errorf("root: %w", err)
	}
	for _, e := range entries {
		if dir := filepath.Join(root, e.Name()); e.IsDir() && !strings.HasPrefix(e.Name(), ".") && isFile(filepath.Join(dir, "go.mod")) {
			add(dir)
		}
	}
	return repos, "from " + root, nil
}

// findRepo returns <root>/<name>.git, else <root>/<name>, when it has a
// go.mod; else "".
func findRepo(root, name string) string {
	for _, dir := range []string{filepath.Join(root, name+".git"), filepath.Join(root, name)} {
		if isFile(filepath.Join(dir, "go.mod")) {
			return dir
		}
	}
	return ""
}

// readNames reads a list of names, one per line, # starting a comment.
func readNames(file string) ([]string, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var names []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line, _, _ := strings.Cut(sc.Text(), "#")
		names = append(names, strings.Fields(line)...)
	}
	return names, sc.Err()
}

// gitStatus records every directory's git status, eight at a time.
func gitStatus(dirs []string) map[string]string {
	out := make(map[string]string, len(dirs))
	var mu sync.Mutex
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, d := range dirs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			s, err := exec.Command("git", "-C", d, "status", "--porcelain").Output()
			text := string(s)
			if err != nil {
				text = "(not a git repository)"
			}
			mu.Lock()
			out[d] = text
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// statusChanged fails with msg when a directory's git status differs.
func statusChanged(before, after map[string]string, msg string) error {
	var changed []string
	for d, b := range before {
		if a := after[d]; a != b {
			changed = append(changed, fmt.Sprintf("%s:\n  before: %q\n  after:  %q", d, strings.TrimSpace(b), strings.TrimSpace(a)))
		}
	}
	if len(changed) == 0 {
		return nil
	}
	sort.Strings(changed)
	return fmt.Errorf("%s\n%s", msg, strings.Join(changed, "\n"))
}

func hasManifests(dir string) bool {
	ms, _ := filepath.Glob(filepath.Join(dir, "*", "manifest.json"))
	return len(ms) > 0
}

func hasTool(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// shellJoin quotes args for a shell, as a dry run prints them.
func shellJoin(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shellQuote(a)
	}
	return strings.Join(q, " ")
}

func shellQuote(s string) string {
	if s != "" && !strings.ContainsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || slices.Contains([]rune("-_./=,:@%+"), r))
	}) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
