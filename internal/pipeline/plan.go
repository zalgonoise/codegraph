// Package pipeline runs codegraph's pipelines (codegraph run): the steps of a
// pipeline of the configuration file, or one built-in step, with the options
// resolved in layers: the built-in defaults, then the profile's, then the
// pipeline's, then the step's, then the command line's. The steps are what
// make graph used to do: select the repositories, keep the previous run, analyse
// and link them (reusing the analysis of those unchanged), check that no clone
// changed, render an environment's deployment, run the profile's checks,
// compare with the previous run, pack a bundle; plus exec, a command of the
// person's own (updating the clones, say). It also tells what a configuration
// resolves to (Describe), checks it (Check) and how current a run is (Status).
package pipeline

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zalgonoise/codegraph/internal/configfile"
	"github.com/zalgonoise/codegraph/internal/options"
	"github.com/zalgonoise/codegraph/internal/profile"
)

// Request is what the command line asks for.
type Request struct {
	Config  *configfile.File // nil without one
	Profile string           // -profile: a name of the file's profiles, or a directory
	Name    string           // a pipeline of the file, a built-in step, or "" for the settings alone
	Flags   options.Options  // the command line's options (paths absolute)
}

// Plan is what a pipeline runs, with what.
type Plan struct {
	Name    string
	Steps   []configfile.Step
	Config  *configfile.File
	Profile *profile.Profile
	// ProfileFrom says what chose the profile.
	ProfileFrom string
	// Trusted says whether the plan may run commands: exec steps, and checks.
	Trusted bool
	// Dir is where exec steps run: the configuration file's directory.
	Dir      string
	Warnings []string

	layers []layer
}

type layer struct {
	opts   *options.Options
	source string
}

// Resolve works out the plan of a request.
func Resolve(req Request) (*Plan, error) {
	cf := req.Config
	p := &Plan{Name: req.Name, Config: cf, Trusted: true}
	if wd, err := os.Getwd(); err == nil {
		p.Dir = wd
	}
	var pl *configfile.Pipeline
	switch {
	case req.Name == "":
	case cf != nil && cf.Pipelines[req.Name] != nil:
		pl = cf.Pipelines[req.Name]
		p.Steps, p.Dir = pl.Steps, filepath.Dir(cf.Path)
		p.Trusted = cf.Trusted
	case req.Name == "org":
		p.Steps = []configfile.Step{{Kind: "graph"}, {Kind: "inventory"}, {Kind: "graph"}}
	case slices.Contains(configfile.Builtins, req.Name):
		p.Steps = []configfile.Step{{Kind: req.Name}}
	default:
		return nil, unknownName(cf, req.Name)
	}

	dir := ""
	switch env := os.Getenv(profile.Env); {
	case req.Profile != "":
		if named, ok := cf.Named(req.Profile); ok {
			dir, p.ProfileFrom = named, "-profile "+req.Profile
		} else {
			dir, p.ProfileFrom = abs(req.Profile), "-profile"
		}
	case pl != nil && pl.Profile != "":
		dir, p.ProfileFrom = cf.ProfileDir(pl.Profile), fmt.Sprintf("pipeline %s of %s", req.Name, cf.Path)
	case env != "":
		dir, p.ProfileFrom = abs(env), "$"+profile.Env
	case cf != nil && cf.Profile != "":
		dir, p.ProfileFrom = cf.ProfileDir(cf.Profile), "profile of "+cf.Path
		p.Trusted = p.Trusted && cf.Trusted
	}
	p.Profile = &profile.Profile{}
	if dir != "" {
		prof, err := profile.Load(dir)
		if err != nil {
			return nil, err
		}
		p.Profile = prof
	}
	if cf != nil {
		p.Warnings = append(p.Warnings, cf.Warnings...)
	}
	p.Warnings = append(p.Warnings, p.Profile.Warnings...)

	defaults := options.Defaults()
	p.layers = []layer{{&defaults, "built-in"}}
	if p.Profile.Dir != "" {
		p.layers = append(p.layers, layer{&p.Profile.Defaults, "profile"})
	}
	if pl != nil {
		p.layers = append(p.layers, layer{&pl.Options, fmt.Sprintf("pipeline %s", req.Name)})
	}
	p.layers = append(p.layers, layer{&req.Flags, "command line"})
	if _, err := p.Options(nil); err != nil {
		return nil, err
	}
	return p, nil
}

func abs(p string) string {
	o := options.Options{Out: &p}
	o.Resolve(".")
	if a, err := filepath.Abs(*o.Out); err == nil {
		return a
	}
	return *o.Out
}

func unknownName(cf *configfile.File, name string) error {
	known := append(slices.Clone(configfile.Builtins), cf.Names()...)
	msg := fmt.Sprintf("no pipeline or step %q", name)
	if s := options.Suggest(name, known); s != "" {
		msg += fmt.Sprintf(" (did you mean %s?)", s)
	}
	msg += "; the steps are " + strings.Join(configfile.Builtins, ", ")
	if cf == nil {
		return errors.New(msg + "; no configuration file (" + configfile.Name + ") names pipelines")
	}
	if names := cf.Names(); len(names) > 0 {
		return fmt.Errorf("%s; the pipelines of %s are %s", msg, cf.Path, strings.Join(names, ", "))
	}
	return fmt.Errorf("%s; %s names no pipeline", msg, cf.Path)
}

// Options resolves the options of a step (nil for the pipeline's own): every
// layer merged, the step's just under the command line's, and the
// placeholders expanded. Sources maps each option to the layer that set it.
func (p *Plan) Options(step *configfile.Step) (*options.Options, error) {
	o, _, err := p.resolve(step)
	return o, err
}

// Sources returns the pipeline's options and where each comes from.
func (p *Plan) Sources() (*options.Options, map[string]string, error) {
	return p.resolve(nil)
}

func (p *Plan) resolve(step *configfile.Step) (*options.Options, map[string]string, error) {
	o, sources := &options.Options{}, map[string]string{}
	last := len(p.layers) - 1
	for i, l := range p.layers {
		if i == last && step != nil {
			o.Merge(&step.Options, "step "+step.Kind, sources)
		}
		o.Merge(l.opts, l.source, sources)
	}
	o.Expand()
	if err := o.Validate(); err != nil {
		return nil, nil, err
	}
	return o, sources, nil
}

// Commands lists the commands the plan would run (exec steps and checks),
// which only a trusted plan may.
func (p *Plan) Commands() []string {
	var cmds []string
	for i := range p.Steps {
		s := &p.Steps[i]
		switch s.Kind {
		case "exec":
			cmds = append(cmds, "exec: "+firstLine(s.Command))
		case "graph", "checks":
			o, err := p.Options(s)
			if err != nil {
				continue
			}
			for _, c := range options.List(o.Checks) {
				if !slices.Contains(cmds, "check "+c.Name) {
					cmds = append(cmds, "check "+c.Name)
				}
			}
		}
	}
	return cmds
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}

// checkTrust refuses a plan that would run commands it may not.
func (p *Plan) checkTrust() error {
	cmds := p.Commands()
	if p.Trusted || len(cmds) == 0 {
		return nil
	}
	return fmt.Errorf("%s runs commands (%s), which codegraph runs only from a configuration file in the home directory or named by -config or $%s, "+
		"and from the profiles it or -profile names: %s is in the current directory",
		p.Name, strings.Join(cmds, "; "), configfile.Env, p.Config.Path)
}

// QueryRun resolves the run codegraph query and mcp read when -run is not
// given: -profile's, else $CODEGRAPH_PROFILE's, else the configuration file's
// query.run, else its profile's, else tmp/graph. A profile's run is its out.
func QueryRun(cf *configfile.File, profileFlag string) (run, from string, err error) {
	outOf := func(dir string) (string, error) {
		prof, err := profile.Load(dir)
		if err != nil {
			return "", err
		}
		o := options.Defaults()
		o.Merge(&prof.Defaults, "", nil)
		o.Expand()
		return abs(options.Str(o.Out)), nil
	}
	switch env := os.Getenv(profile.Env); {
	case profileFlag != "":
		dir, ok := cf.Named(profileFlag)
		if !ok {
			dir = abs(profileFlag)
		}
		run, err = outOf(dir)
		return run, "the out of -profile " + profileFlag, err
	case env != "":
		run, err = outOf(abs(env))
		return run, "the out of $" + profile.Env, err
	case cf != nil && cf.Query.Run != "":
		return cf.Query.Run, "query.run of " + cf.Path, nil
	case cf != nil && cf.Profile != "":
		run, err = outOf(cf.ProfileDir(cf.Profile))
		return run, "the out of " + cf.Path + "'s profile", err
	}
	return "tmp/graph", "the default", nil
}
