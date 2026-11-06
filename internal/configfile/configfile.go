// Package configfile finds and reads codegraph's configuration file,
// .codegraph.yaml: the profiles a person uses, the pipelines codegraph run
// runs (codegraph alone runs the default one), and the run query and mcp
// answer from.
//
// The file is the one -config names, else $CODEGRAPH_CONFIG, else
// .codegraph.yaml in the current directory, else in the home directory. Only a
// file named explicitly or in the home directory may run commands (exec steps,
// and the checks of the profiles it names): a .codegraph.yaml in the current
// directory could come with a repository someone else wrote.
package configfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/zalgonoise/codegraph/internal/options"
)

// Name is the configuration file's name, in the current or home directory.
const Name = ".codegraph.yaml"

// Env names the environment variable pointing at a configuration file.
const Env = "CODEGRAPH_CONFIG"

// Version is the newest format this codegraph reads.
const Version = 1

// Source says where a configuration file was found.
type Source string

const (
	FromFlag Source = "-config"
	FromEnv  Source = "$" + Env
	FromDir  Source = "the current directory"
	FromHome Source = "the home directory"
)

// Steps are the steps a pipeline can run, in the order make's targets ran them.
var Steps = []string{"exec", "inventory", "graph", "deploy", "checks", "diff", "bundle"}

// Builtins are the names codegraph run knows without a configuration file:
// every step but exec, and org (graph, inventory, graph).
var Builtins = []string{"inventory", "graph", "deploy", "checks", "diff", "bundle", "org"}

// File is a configuration file.
type File struct {
	Path    string `json:"-"`
	Source  Source `json:"-"`
	Trusted bool   `json:"-"` // it may run commands

	Version int `json:"version,omitempty"`
	// Profiles name profile directories, for pipelines and -profile.
	Profiles map[string]string `json:"profiles,omitempty"`
	// Profile is the profile used when nothing else names one: a name of
	// Profiles, or a directory.
	Profile string `json:"profile,omitempty"`
	// Default is the pipeline codegraph alone runs.
	Default   string               `json:"default,omitempty"`
	Query     Query                `json:"query"`
	Pipelines map[string]*Pipeline `json:"pipelines,omitempty"`

	Warnings []string `json:"-"`
}

// Query sets what codegraph query and mcp read when no flag does.
type Query struct {
	Run      string `json:"run,omitempty"`      // a run directory or a bundle
	Explorer string `json:"explorer,omitempty"` // the explorer page answers link to
}

// Pipeline is a named sequence of steps with its settings, over its profile's.
type Pipeline struct {
	Profile string `json:"profile,omitempty"`
	Steps   []Step `json:"steps"`
	options.Options
}

// Step is one step of a pipeline: a name (graph), a name with settings over
// the pipeline's ({graph: {api: true}}), or a command ({exec: "…"}).
type Step struct {
	Kind    string
	Command string // exec's
	Options options.Options
}

// UnmarshalJSON reads a step's three forms.
func (s *Step) UnmarshalJSON(data []byte) error {
	if json.Unmarshal(data, &s.Kind) == nil {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil || len(m) != 1 {
		return errors.New("a step is a name (graph), a name with its settings ({graph: {api: true}}), or {exec: <command>}")
	}
	for k, v := range m {
		s.Kind = k
		if k == "exec" {
			if err := json.Unmarshal(v, &s.Command); err != nil {
				return errors.New("exec takes a command, as one string")
			}
			return nil
		}
		if string(v) != "null" {
			return json.Unmarshal(v, &s.Options)
		}
	}
	return nil
}

// CheckKeys warns about unknown settings of a step (options.KeyChecker).
func (s *Step) CheckKeys(n *yaml.Node, warn func(*yaml.Node, string)) {
	if n.Kind == yaml.MappingNode && len(n.Content) == 2 && n.Content[0].Value != "exec" {
		options.CheckKeys(n.Content[1], typeOfOptions, warn)
	}
}

// String writes the step as YAML would.
func (s Step) String() string {
	if s.Kind == "exec" {
		return "exec: " + s.Command
	}
	return s.Kind
}

// Find returns the configuration file: flag's (none for none), else
// $CODEGRAPH_CONFIG's, else .codegraph.yaml in the current directory, else in
// the home directory; nil when there is none.
func Find(flag string) (*File, error) {
	switch {
	case flag == options.None:
		return nil, nil
	case flag != "":
		return Read(flag, FromFlag)
	}
	if p := os.Getenv(Env); p == options.None {
		return nil, nil
	} else if p != "" {
		return Read(p, FromEnv)
	}
	home, _ := os.UserHomeDir()
	if wd, err := os.Getwd(); err == nil {
		if p := filepath.Join(wd, Name); exists(p) {
			src := FromDir
			if home != "" && sameDir(wd, home) {
				src = FromHome
			}
			return Read(p, src)
		}
	}
	if home != "" {
		if p := filepath.Join(home, Name); exists(p) {
			return Read(p, FromHome)
		}
	}
	return nil, nil
}

func exists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func sameDir(a, b string) bool {
	sa, err1 := os.Stat(a)
	sb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(sa, sb)
}

var typeOfOptions = reflect.TypeOf(options.Options{})

// Read reads the configuration file at path, found from src.
func Read(path string, src Source) (*File, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("configuration file %s (from %s): not found", path, src)
	}
	if err != nil {
		return nil, err
	}
	f := &File{Path: abs, Source: src, Trusted: src != FromDir}
	if f.Warnings, err = options.DecodeYAML(data, abs, f); err != nil {
		return nil, err
	}
	if f.Version > Version {
		return nil, fmt.Errorf("%s: version %d, this codegraph reads version %d: use a newer codegraph", abs, f.Version, Version)
	}
	dir := filepath.Dir(abs)
	for name, p := range f.Profiles {
		f.Profiles[name] = resolve(dir, p)
	}
	if f.Query.Run != "" {
		f.Query.Run = resolve(dir, f.Query.Run)
	}
	for _, name := range f.Names() {
		p := f.Pipelines[name]
		if p == nil {
			return nil, fmt.Errorf("%s: pipeline %s has no steps", abs, name)
		}
		if slices.Contains(Builtins, name) || name == "exec" {
			return nil, fmt.Errorf("%s: pipeline %s: %s is a step's name (%s); name the pipeline otherwise", abs, name, name, strings.Join(Builtins, ", "))
		}
		if len(p.Steps) == 0 {
			return nil, fmt.Errorf("%s: pipeline %s has no steps", abs, name)
		}
		if err := p.Options.Validate(); err != nil {
			return nil, fmt.Errorf("%s: pipeline %s: %w", abs, name, err)
		}
		p.Options.Resolve(dir)
		for i := range p.Steps {
			s := &p.Steps[i]
			if !slices.Contains(Steps, s.Kind) {
				hint := ""
				if g := options.Suggest(s.Kind, Steps); g != "" {
					hint = fmt.Sprintf(" (did you mean %s?)", g)
				}
				return nil, fmt.Errorf("%s: pipeline %s: unknown step %q%s; the steps are %s", abs, name, s.Kind, hint, strings.Join(Steps, ", "))
			}
			if s.Kind == "exec" && strings.TrimSpace(s.Command) == "" {
				return nil, fmt.Errorf("%s: pipeline %s: exec without a command", abs, name)
			}
			if err := s.Options.Validate(); err != nil {
				return nil, fmt.Errorf("%s: pipeline %s, step %s: %w", abs, name, s.Kind, err)
			}
			s.Options.Resolve(dir)
		}
	}
	if f.Default != "" && f.Pipelines[f.Default] == nil {
		f.Warnings = append(f.Warnings, fmt.Sprintf("%s: default names %q, which is not a pipeline of the file", abs, f.Default))
	}
	return f, nil
}

func resolve(dir, p string) string {
	o := options.Options{Out: &p}
	o.Resolve(dir)
	return *o.Out
}

// Names lists the pipelines, sorted.
func (f *File) Names() []string {
	if f == nil {
		return nil
	}
	names := make([]string, 0, len(f.Pipelines))
	for n := range f.Pipelines {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ProfileDir returns the directory a profile reference of the file names:
// one of Profiles, or a directory (relative to the file).
func (f *File) ProfileDir(ref string) string {
	if dir, ok := f.Named(ref); ok || ref == "" {
		return dir
	}
	return resolve(filepath.Dir(f.Path), ref)
}

// Named returns the directory of a profile named in Profiles.
func (f *File) Named(ref string) (string, bool) {
	if f == nil {
		return "", false
	}
	dir, ok := f.Profiles[ref]
	return dir, ok
}

// Describe says which file this is and whether it may run commands.
func (f *File) Describe() string {
	if f == nil {
		return "none"
	}
	s := fmt.Sprintf("%s (from %s)", f.Path, f.Source)
	if !f.Trusted {
		s += "; it may not run commands: name it with -config or " + Env + " to allow them"
	}
	return s
}
