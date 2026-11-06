// Package options holds the settings of codegraph's runs (codegraph run): the
// built-in defaults, what a profile sets (profile.yaml's defaults), what a
// pipeline of the configuration file sets, what one of its steps sets, and
// what the command line sets, each layer over the one before. Every option is
// a pointer, nil when a layer leaves it unset, so the layers merge field by
// field, and each value remembers the layer it came from.
package options

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"unicode"
)

// None turns off an option that names a file or directory (focus, chart).
const None = "none"

// Check is a command run after each graph: a comparison of the run with
// another source, such as an architecture knowledge base.
type Check struct {
	Name string `json:"name"`
	// Run is a shell command (sh -c), run in the directory of the file that
	// defines it, with the run's directory in $CODEGRAPH_RUN.
	Run string `json:"run"`
	// Output is the file of the run (relative to it) that its standard output
	// goes to; empty for the terminal.
	Output string `json:"output,omitempty"`
	Dir    string `json:"-"` // where it runs: set by Resolve
}

// Options are a run's settings. The json names are the keys of profile.yaml's
// defaults and of a pipeline in .codegraph.yaml; the flags of codegraph run are
// the same names in kebab case (repoRules: -repo-rules).
type Options struct {
	Root        *string   `json:"root,omitempty" path:"true" help:"directory holding the repositories (<repo>.git or <repo>)"`
	Repos       *[]string `json:"repos,omitempty" path:"true" help:"repositories to analyse: names in root, or paths; default those focus and the frontier name, else every Go repository of root"`
	Focus       *string   `json:"focus,omitempty" path:"true" help:"file naming the repositories analysed in full (one per line, # comments), around which inventory finds the frontier; none: every Go repository of root"`
	Inventory   *string   `json:"inventory,omitempty" path:"true" help:"the inventory's directory (inventory.json, frontier.txt)"`
	Rings       *int      `json:"rings,omitempty" help:"frontier rings around focus (inventory)"`
	Out         *string   `json:"out,omitempty" path:"true" help:"the run's directory; the previous run moves to <out>.prev"`
	Rules       *[]string `json:"rules,omitempty" path:"true" help:"transport rule files for in-house libraries"`
	RepoRules   *string   `json:"repoRules,omitempty" path:"true" help:"directory of per-repository rule files, <module name>.json"`
	Declared    *string   `json:"declared,omitempty" path:"true" help:"directory of services described by hand, <service>.json"`
	Include     *[]string `json:"includeModules,omitempty" help:"module patterns analysed with the services using them, such as example.com/*-sdk-go"`
	Libraries   *bool     `json:"libraries,omitempty" help:"analyse repositories without a main as libraries (their exported API) instead of skipping them"`
	API         *bool     `json:"api,omitempty" help:"also analyse the API of repositories with mains, as <name>-api (implies libraries)"`
	Formats     *[]string `json:"formats,omitempty" help:"outputs: json, mermaid, dot, md, html, agents"`
	Algo        *string   `json:"algo,omitempty" help:"call graph algorithm: cha, rta or vta"`
	GoPrivate   *string   `json:"goprivate,omitempty" help:"GOPRIVATE for the analysis: private module paths fetched directly over git"`
	Full        *bool     `json:"full,omitempty" help:"analyse every repository again, instead of reusing the previous run's analysis of those unchanged since"`
	Chart       *string   `json:"chart,omitempty" path:"true" help:"an environment's Helm chart, rendered offline after each graph (needs helm); none for no deployment"`
	Environment *string   `json:"environment,omitempty" help:"the environment's name, for the reports"`
	Checks      *[]Check  `json:"checks,omitempty" help:"commands run after each graph, given the run's directory ($CODEGRAPH_RUN)"`
	Base        *string   `json:"base,omitempty" path:"true" help:"the run diff compares with"`
	Bundle      *string   `json:"bundle,omitempty" path:"true" help:"the bundle file (.tar.zst for zstd)"`
	Compress    *string   `json:"compress,omitempty" help:"the bundle's compression: gzip or zstd; default what its name ends with"`
	NoSettings  *bool     `json:"noSettings,omitempty" help:"leave the configured values out of the bundle"`
	NoPositions *bool     `json:"noPositions,omitempty" help:"leave every file:line position out of the bundle"`
	Denylist    *string   `json:"denylist,omitempty" path:"true" help:"words no file of codegraph may contain (make audit), one per line"`
}

// Defaults are the built-in settings, relative to the current directory.
func Defaults() Options {
	return Options{
		Root:        ptr(".."),
		Focus:       ptr(None),
		Inventory:   ptr("tmp/inventory"),
		Rings:       ptr(1),
		Out:         ptr("tmp/graph"),
		Formats:     &[]string{"json", "md", "html", "agents"},
		Algo:        ptr("vta"),
		Full:        ptr(false),
		Chart:       ptr(None),
		Environment: ptr("production"),
		Checks:      &[]Check{},
		Base:        ptr("{out}.prev"),
		Bundle:      ptr("{out}.tar.gz"),
		NoSettings:  ptr(false),
		NoPositions: ptr(false),
		Libraries:   ptr(false),
		API:         ptr(false),
	}
}

func ptr[T any](v T) *T { return &v }

// Field describes one option.
type Field struct {
	Name string // the key in YAML
	Flag string // the flag of codegraph run (without its dash)
	Help string
	Path bool // a file or directory: relative to the file that sets it
	Kind reflect.Kind
	List bool // a list (comma-separated or repeated as a flag)
	idx  int
}

// Fields lists the options in their order.
func Fields() []Field {
	t := reflect.TypeOf(Options{})
	var out []Field
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		ft := f.Type.Elem()
		fl := Field{Name: name, Flag: kebab(name), Help: f.Tag.Get("help"), Path: f.Tag.Get("path") == "true", Kind: ft.Kind(), idx: i}
		if ft.Kind() == reflect.Slice {
			fl.List, fl.Kind = true, ft.Elem().Kind()
		}
		out = append(out, fl)
	}
	return out
}

// Lookup returns the option of a YAML key or a flag name.
func Lookup(name string) (Field, bool) {
	for _, f := range Fields() {
		if f.Name == name || f.Flag == name {
			return f, true
		}
	}
	return Field{}, false
}

func kebab(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsUpper(r) {
			b.WriteByte('-')
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Merge sets every option src sets, recording source as their origin in
// sources (when not nil).
func (o *Options) Merge(src *Options, source string, sources map[string]string) {
	if src == nil {
		return
	}
	dst, from := reflect.ValueOf(o).Elem(), reflect.ValueOf(src).Elem()
	for _, f := range Fields() {
		if v := from.Field(f.idx); !v.IsNil() {
			dst.Field(f.idx).Set(v)
			if sources != nil {
				sources[f.Name] = source
			}
		}
	}
}

// Resolve makes the paths o sets absolute: ~ is the home directory, and a
// relative path is relative to base (the directory of the file setting it).
// none, empty values and values starting with a placeholder ({root}, {out})
// stay as they are. In repos, only entries that look like paths are paths.
// Checks run in base.
func (o *Options) Resolve(base string) {
	v := reflect.ValueOf(o).Elem()
	for _, f := range Fields() {
		fv := v.Field(f.idx)
		if fv.IsNil() || !f.Path {
			continue
		}
		switch p := fv.Interface().(type) {
		case *string:
			*p = resolvePath(base, *p)
		case *[]string:
			list := make([]string, len(*p))
			for i, s := range *p {
				if f.Name != "repos" || looksLikePath(s) {
					s = resolvePath(base, s)
				}
				list[i] = s
			}
			*p = list
		}
	}
	if o.Checks != nil {
		checks := make([]Check, len(*o.Checks))
		for i, c := range *o.Checks {
			c.Dir = base
			checks[i] = c
		}
		o.Checks = &checks
	}
}

func looksLikePath(s string) bool {
	return strings.ContainsRune(s, '/') || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "~")
}

func resolvePath(base, p string) string {
	switch {
	case p == "" || p == None || strings.HasPrefix(p, "{"):
		return p
	case p == "~" || strings.HasPrefix(p, "~/"):
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
		return p
	case filepath.IsAbs(p) || base == "":
		return filepath.Clean(p)
	}
	return filepath.Join(base, p)
}

// Expand replaces the placeholders {root} and {out} in the paths, once every
// layer is merged: a profile's chart under {root} follows a root given on the
// command line.
func (o *Options) Expand() {
	root, out := Str(o.Root), Str(o.Out)
	repl := strings.NewReplacer("{root}", root, "{out}", out)
	v := reflect.ValueOf(o).Elem()
	for _, f := range Fields() {
		fv := v.Field(f.idx)
		if fv.IsNil() || !f.Path {
			continue
		}
		switch p := fv.Interface().(type) {
		case *string:
			if s := repl.Replace(*p); s != *p {
				fv.Set(reflect.ValueOf(ptr(filepath.Clean(s))))
			}
		case *[]string:
			list := make([]string, len(*p))
			for i, s := range *p {
				if list[i] = repl.Replace(s); list[i] != s {
					list[i] = filepath.Clean(list[i])
				}
			}
			fv.Set(reflect.ValueOf(&list))
		}
	}
}

// Get returns an option's value as text: lists comma-separated, checks by name.
func (o *Options) Get(name string) (string, bool) {
	f, ok := Lookup(name)
	if !ok {
		return "", false
	}
	fv := reflect.ValueOf(o).Elem().Field(f.idx)
	if fv.IsNil() {
		return "", true
	}
	switch p := fv.Interface().(type) {
	case *string:
		return *p, true
	case *int:
		return strconv.Itoa(*p), true
	case *bool:
		return strconv.FormatBool(*p), true
	case *[]string:
		return strings.Join(*p, ","), true
	case *[]Check:
		names := make([]string, len(*p))
		for i, c := range *p {
			names[i] = c.Name
		}
		return strings.Join(names, ","), true
	}
	return "", true
}

// Str, Int, Bool and List read an option, with its zero value when unset.
func Str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func Int(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func Bool(p *bool) bool { return p != nil && *p }

func List[T any](p *[]T) []T {
	if p == nil {
		return nil
	}
	return *p
}

// IsNone reports whether a file option is turned off (empty or none).
func IsNone(p *string) bool { s := Str(p); return s == "" || s == None }

// Register adds a flag for every option to fs, setting o; lists take
// comma-separated values or the flag repeated, and a flag given empty sets an
// empty list. -no-checks empties the checks.
func Register(fs *flag.FlagSet, o *Options) {
	v := reflect.ValueOf(o).Elem()
	given := map[string]bool{} // the list flags given so far: a repeated flag appends
	for _, f := range Fields() {
		fv := v.Field(f.idx)
		help := f.Help
		switch {
		case f.Name == "checks":
			fs.BoolFunc("no-checks", "run no check after the graph (the profile's checks)", func(string) error {
				o.Checks = &[]Check{}
				return nil
			})
		case f.List:
			fs.Func(f.Flag, help+" (comma-separated, or repeated)", func(s string) error {
				p, _ := fv.Interface().(*[]string)
				if p == nil || !given[f.Name] {
					p = &[]string{}
					fv.Set(reflect.ValueOf(p))
					given[f.Name] = true
				}
				for _, x := range strings.Split(s, ",") {
					if x = strings.TrimSpace(x); x != "" {
						*p = append(*p, x)
					}
				}
				return nil
			})
		case f.Kind == reflect.Bool:
			fs.BoolFunc(f.Flag, help, func(s string) error {
				b, err := strconv.ParseBool(s)
				if err != nil {
					return err
				}
				fv.Set(reflect.ValueOf(&b))
				return nil
			})
		case f.Kind == reflect.Int:
			fs.Func(f.Flag, help, func(s string) error {
				n, err := strconv.Atoi(s)
				if err != nil {
					return err
				}
				fv.Set(reflect.ValueOf(&n))
				return nil
			})
		default:
			fs.Func(f.Flag, help, func(s string) error {
				fv.Set(reflect.ValueOf(&s))
				return nil
			})
		}
	}
}

// Validate checks the values a layer cannot check alone.
func (o *Options) Validate() error {
	switch a := Str(o.Algo); a {
	case "", "cha", "rta", "vta":
	default:
		return fmt.Errorf("algo must be cha, rta or vta, got %q", a)
	}
	switch c := Str(o.Compress); c {
	case "", "gzip", "zstd":
	default:
		return fmt.Errorf("compress must be gzip or zstd, got %q", c)
	}
	if o.Rings != nil && *o.Rings < 1 {
		return fmt.Errorf("rings must be at least 1, got %d", *o.Rings)
	}
	for _, c := range List(o.Checks) {
		if c.Name == "" || c.Run == "" {
			return fmt.Errorf("a check needs a name and a run command: %+v", c)
		}
	}
	return nil
}
