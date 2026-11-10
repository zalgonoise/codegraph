package pipeline

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/zalgonoise/codegraph/internal/configfile"
	"github.com/zalgonoise/codegraph/internal/options"
)

// header logs what a run is about to do (a dry run's script has it as
// comments): codegraph's version, the pipeline and the file naming it, the
// profile, the steps, what it reads and writes, and the settings that decide
// what it does.
func (r *Runner) header(p *Plan) {
	line := r.Log
	if r.DryRun {
		line = func(format string, args ...any) { r.printf("# "+format+"\n", args...) }
	}
	version := r.Version
	if version == "" {
		version = "devel"
	}
	line("codegraph %s (%s/%s, %s)", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
	row := func(name, value string) { line("  %-9s %s", name, value) }

	run := p.Name
	if p.Config != nil && p.Config.Pipelines[p.Name] != nil {
		run += ", from " + home(p.Config.Path)
	} else {
		run += " (a step)"
	}
	row("run", run)
	if p.Profile.Dir != "" {
		row("profile", profileName(p.Config, p.Profile.Dir)+home(p.Profile.Dir))
	}
	steps := make([]string, len(p.Steps))
	for i, s := range p.Steps {
		steps[i] = s.Kind
		if s.Kind == "exec" {
			if f := strings.Fields(s.Command); len(f) > 0 {
				steps[i] = "exec (" + filepath.Base(f[0]) + ")"
			}
		}
	}
	row("steps", strings.Join(steps, " → "))

	o, err := p.Options(p.first("graph", "inventory"))
	if err != nil {
		return
	}
	if p.first("graph", "inventory") != nil { // the steps reading the repositories
		input := home(options.Str(o.Root))
		if p.first("graph") != nil {
			if repos, _, err := Repositories(o); err == nil {
				input += fmt.Sprintf(" (%d Go repositories)", len(repos))
			}
		}
		row("input", input)
	}
	output := home(options.Str(o.Out))
	if p.first("bundle") != nil {
		if bo, err := p.Options(p.first("bundle")); err == nil {
			output += ", bundle " + home(options.Str(bo.Bundle))
		}
	}
	row("output", output)
	if s := settings(o); s != "" {
		row("settings", s)
	}
}

// first returns the plan's first step of one of the kinds, nil without one.
func (p *Plan) first(kinds ...string) *configfile.Step {
	for i := range p.Steps {
		for _, k := range kinds {
			if p.Steps[i].Kind == k {
				return &p.Steps[i]
			}
		}
	}
	return nil
}

// settings lists what decides what a run does, in short.
func settings(o *options.Options) string {
	var out []string
	if options.IsNone(o.Focus) {
		out = append(out, "focus none")
	} else {
		out = append(out, fmt.Sprintf("focus %s, %d rings", filepath.Base(options.Str(o.Focus)), options.Int(o.Rings)))
	}
	switch {
	case options.Bool(o.API):
		out = append(out, "api")
	case options.Bool(o.Libraries):
		out = append(out, "libraries")
	}
	if options.Bool(o.Full) {
		out = append(out, "full")
	} else {
		out = append(out, "reuse")
	}
	if a := options.Str(o.Algo); a != "" && a != "vta" {
		out = append(out, a)
	}
	if options.IsNone(o.Chart) {
		out = append(out, "no chart")
	} else {
		out = append(out, fmt.Sprintf("chart %s (%s)", filepath.Base(options.Str(o.Chart)), options.Str(o.Environment)))
	}
	add := func(n int, one, many string) {
		switch {
		case n == 1:
			out = append(out, "1 "+one)
		case n > 1:
			out = append(out, fmt.Sprintf("%d %s", n, many))
		}
	}
	add(len(options.List(o.Rules)), "rule file", "rule files")
	add(len(options.List(o.Include)), "included module pattern", "included module patterns")
	if d := options.Str(o.Declared); d != "" {
		files, _ := filepath.Glob(filepath.Join(d, "*.json"))
		add(len(files), "declared service", "declared services")
	}
	add(len(options.List(o.Checks)), "check", "checks")
	return strings.Join(out, ", ")
}

// profileName is "<name>: " when the configuration file names the profile's
// directory, else "".
func profileName(cf *configfile.File, dir string) string {
	if cf == nil {
		return ""
	}
	names := make([]string, 0, len(cf.Profiles))
	for name := range cf.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if cf.ProfileDir(name) == dir {
			return name + ": "
		}
	}
	return ""
}

// home writes a path under the home directory with ~.
func home(p string) string {
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return p
	}
	if rest, ok := strings.CutPrefix(p, h+string(filepath.Separator)); ok {
		return "~/" + rest
	}
	return p
}

// Failure is one item a step's command could not do, and why: a repository
// whose analysis failed.
type Failure struct {
	Item string
	Err  string
}

// Partial is the error of a command that did its work but not for every item
// (fleet's repositories): the step goes on, and the run lists the failures
// at its end and fails.
type Partial interface {
	error
	Failures() []Failure
}

// ShellError is a command of the person's that failed, with the last lines
// it wrote to standard error, for the run's summary.
type ShellError struct {
	Err  error
	Tail []string
}

func (e *ShellError) Error() string { return e.Err.Error() }

func (e *ShellError) Unwrap() error { return e.Err }

// tailLines keeps the last lines written to it.
type tailLines struct {
	mu    sync.Mutex
	max   int
	part  []byte
	lines []string
}

func (t *tailLines) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.part = append(t.part, b...)
	for {
		i := bytes.IndexByte(t.part, '\n')
		if i < 0 {
			break
		}
		t.add(string(t.part[:i]))
		t.part = t.part[i+1:]
	}
	return len(b), nil
}

func (t *tailLines) add(line string) {
	if line = strings.TrimRight(line, "\r"); strings.TrimSpace(line) == "" {
		return
	}
	t.lines = append(t.lines, line)
	if len(t.lines) > t.max {
		t.lines = t.lines[len(t.lines)-t.max:]
	}
}

// Lines returns the lines kept, the last one even without its newline.
func (t *tailLines) Lines() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := append([]string(nil), t.lines...)
	if len(t.part) > 0 {
		out = append(out, string(t.part))
	}
	if len(out) > t.max {
		out = out[len(out)-t.max:]
	}
	return out
}

// tailSummary renders what a failed command last wrote, for the run's
// summary. Structured log records (one JSON object per line, as slog writes
// them) keep their errors and warnings only, each as its message with its
// attributes, and an object attribute as one line per key: a summary mapping
// items to their errors (gitpull's "repositories failed"), which stands for
// the error records before it. A record repeating the error of the one before
// it (a command's error logged again on the way out) is left out. Other
// lines are kept as they are.
func tailSummary(lines []string) []string {
	type entry struct {
		lines   []string
		summary bool // it maps items to errors
	}
	var entries []entry
	structured, lastErr := false, ""
	for _, line := range lines {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil || rec["msg"] == nil {
			entries = append(entries, entry{lines: []string{line}})
			continue
		}
		structured = true
		level, _ := rec["level"].(string)
		if level != "ERROR" && level != "WARN" {
			continue
		}
		if e, ok := rec["error"].(string); ok && e == lastErr {
			continue
		}
		lastErr, _ = rec["error"].(string)
		var attrs, nested []string
		keys := make([]string, 0, len(rec))
		for k := range rec {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		summary := false
		for _, k := range keys {
			switch k {
			case "time", "level", "msg", "source", "trace_id", "span_id":
				continue
			}
			obj, ok := rec[k].(map[string]any)
			if !ok {
				attrs = append(attrs, fmt.Sprintf("%s=%v", k, rec[k]))
				continue
			}
			summary = true
			sub := make([]string, 0, len(obj))
			for sk := range obj {
				sub = append(sub, sk)
			}
			sort.Strings(sub)
			for _, sk := range sub {
				nested = append(nested, fmt.Sprintf("  %s: %v", sk, obj[sk]))
			}
		}
		head := fmt.Sprintf("%s %v", level, rec["msg"])
		if len(attrs) > 0 {
			head += " (" + strings.Join(attrs, ", ") + ")"
		}
		if summary {
			entries = nil // it stands for what came before
		}
		entries = append(entries, entry{lines: append([]string{head}, nested...), summary: summary})
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.lines...)
	}
	if structured && len(out) == 0 {
		return []string{"(no error among its last log records)"}
	}
	return out
}
