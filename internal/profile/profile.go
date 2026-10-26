// Package profile reads an organisation's profile: a directory holding what
// codegraph cannot guess about it. Its profile.json gives the naming
// conventions the inventory relies on (inventory.Conventions) and the
// repositories defining the gRPC contracts. The rest of a profile (rules,
// services described by hand, the Makefile's settings in profile.mk) reaches
// codegraph as ordinary flags. Without a profile, codegraph assumes no
// organisation-specific convention.
package profile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/zalgonoise/codegraph/internal/inventory"
)

// Env names the environment variable pointing at a profile, when no flag does.
const Env = "CODEGRAPH_PROFILE"

// File is the profile's settings file, in its directory.
const File = "profile.json"

// Profile is a loaded profile.
type Profile struct {
	Dir string `json:"-"` // "" when there is none

	inventory.Conventions
	// Catalogues are the repositories whose proto/ tree defines the gRPC
	// contracts (codegraph inventory's contracts.md).
	Catalogues []string `json:"catalogues,omitempty"`
}

// Load reads the profile in dir, or in $CODEGRAPH_PROFILE when dir is "". No
// directory at all is an empty profile; a directory without profile.json is a
// profile without conventions.
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
	data, err := os.ReadFile(filepath.Join(dir, File))
	if errors.Is(err, fs.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(p); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, File), err)
	}
	return p, nil
}
