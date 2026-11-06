// Package profile reads an organisation's profile: a directory holding what
// codegraph cannot guess about it. Its profile.yaml gives the naming
// conventions the inventory relies on (inventory.Conventions), the
// repositories defining the gRPC contracts, and the defaults of runs with the
// profile (codegraph run): which repositories, rules, chart and checks. The
// rest of a profile (rule files, services described by hand, a focus list) is
// named from there. Without a profile, codegraph assumes no
// organisation-specific convention.
package profile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/zalgonoise/codegraph/internal/inventory"
	"github.com/zalgonoise/codegraph/internal/options"
)

// Env names the environment variable pointing at a profile, when no flag does.
const Env = "CODEGRAPH_PROFILE"

// File is the profile's settings file, in its directory.
const File = "profile.yaml"

// LegacyFile is the conventions file of profiles written before profile.yaml,
// still read when there is no profile.yaml.
const LegacyFile = "profile.json"

// Version is the newest profile.yaml format this codegraph reads.
const Version = 1

// Profile is a loaded profile.
type Profile struct {
	Dir  string `json:"-"` // "" when there is none
	File string `json:"-"` // the file read; "" when the directory has none

	Version int `json:"version,omitempty"`
	inventory.Conventions
	// Catalogues are the repositories whose proto/ tree defines the gRPC
	// contracts (codegraph inventory's contracts.md).
	Catalogues []string `json:"catalogues,omitempty"`
	// Defaults are the settings of runs with this profile, over the built-in
	// ones; paths are relative to the profile's directory.
	Defaults options.Options `json:"defaults"`

	Warnings []string `json:"-"` // keys this codegraph does not know, ignored
}

// Load reads the profile in dir, or in $CODEGRAPH_PROFILE when dir is "". No
// directory at all is an empty profile; a directory without profile.yaml (or
// profile.json) is a profile without settings.
func Load(dir string) (*Profile, error) {
	if dir == "" {
		dir = os.Getenv(Env)
	}
	p := &Profile{Dir: dir}
	if dir == "" {
		return p, nil
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("profile %s: not a directory", dir)
	}
	if abs, err := filepath.Abs(dir); err == nil {
		p.Dir = abs
	}
	file := filepath.Join(p.Dir, File)
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		file = filepath.Join(p.Dir, LegacyFile)
		data, err = os.ReadFile(file)
	} else if _, legacy := os.Stat(filepath.Join(p.Dir, LegacyFile)); legacy == nil {
		p.Warnings = append(p.Warnings, fmt.Sprintf("%s: not read, %s replaces it", filepath.Join(p.Dir, LegacyFile), File))
	}
	if errors.Is(err, fs.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	p.File = file
	warnings, err := options.DecodeYAML(data, file, p) // JSON is YAML too
	p.Warnings = append(p.Warnings, warnings...)
	if err != nil {
		return nil, err
	}
	if p.Version > Version {
		return nil, fmt.Errorf("%s: version %d, this codegraph reads version %d: use a newer codegraph", file, p.Version, Version)
	}
	if err := p.Defaults.Validate(); err != nil {
		return nil, fmt.Errorf("%s: defaults: %w", file, err)
	}
	p.Defaults.Resolve(p.Dir)
	return p, nil
}
