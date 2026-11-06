// Package incremental lets fleet reuse a previous run's analysis of the
// repositories that have not changed since. A repository's key covers what
// its analysis reads: its files (the git tree of its directory, with no
// uncommitted change), codegraph itself (its executable), the Go toolchain and
// its environment, the analysis settings and the rule files. A repository
// whose go.mod replaces a module with a local directory reads files outside
// it, so it is always analysed. fleet.json records each repository's key next
// to the services its analysis wrote, and what the link found.
package incremental

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/mod/modfile"
)

// File is the record of a fleet run, in its directory.
const File = "fleet.json"

// Record is what a fleet run analysed.
type Record struct {
	Codegraph string    `json:"codegraph"`
	Created   time.Time `json:"created"`
	Services  int       `json:"services"`
	Links     int       `json:"links"`
	Unmatched int       `json:"unmatched"`
	Repos     []Repo    `json:"repos"`
}

// Repo is one repository of a run.
type Repo struct {
	Dir      string   `json:"dir"`
	Head     string   `json:"head,omitempty"` // the commit checked out
	Key      string   `json:"key,omitempty"`  // "" when its analysis cannot be reused
	Why      string   `json:"why,omitempty"`  // why not
	Services []string `json:"services,omitempty"`
	Library  bool     `json:"library,omitempty"` // no main: skipped, or analysed as a library
	Failed   bool     `json:"failed,omitempty"`
	Reused   bool     `json:"reused,omitempty"`
}

// Read reads the record of the run in dir; nil when it has none (a run made
// before codegraph recorded them).
func Read(dir string) (*Record, error) {
	data, err := os.ReadFile(filepath.Join(dir, File))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, File), err)
	}
	return &r, nil
}

// Write writes the record into the run's directory.
func (r *Record) Write(dir string) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, File), append(data, '\n'), 0o644)
}

// Find returns the repository of the record in dir.
func (r *Record) Find(dir string) *Repo {
	if r == nil {
		return nil
	}
	for i := range r.Repos {
		if r.Repos[i].Dir == dir {
			return &r.Repos[i]
		}
	}
	return nil
}

// Reused counts the repositories whose analysis came from the run before.
func (r *Record) Reused() (reused, analysed int) {
	for _, repo := range r.Repos {
		switch {
		case repo.Reused:
			reused++
		case !repo.Library || len(repo.Services) > 0:
			analysed++
		}
	}
	return reused, analysed
}

// Toolchain identifies codegraph and the Go toolchain: the SHA-256 of
// codegraph's executable (a development build changes with its code, its
// version does not), and go env's version, platform and flags.
func Toolchain() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	f, err := os.Open(exe)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	out, err := exec.Command("go", "env", "GOVERSION", "GOOS", "GOARCH", "GOFLAGS", "CGO_ENABLED", "GOEXPERIMENT").Output()
	if err != nil {
		return "", fmt.Errorf("go env: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)) + "\n" + string(out), nil
}

// Key computes a repository's key from the toolchain's, the settings of its
// analysis (anything JSON encodes) and the rule files it reads; with the
// commit checked out. A repository whose analysis cannot be reused has no key,
// and why says why.
func Key(toolchain, repo string, settings any, files []string) (head, key, why string) {
	out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD", "HEAD:./").Output()
	lines := strings.Fields(string(out))
	if err != nil || len(lines) != 2 {
		return "", "", "not a git clone"
	}
	head, tree := lines[0], lines[1]
	status, err := exec.Command("git", "-C", repo, "status", "--porcelain", "--untracked-files=all", "--", ".").Output()
	if err != nil {
		return head, "", "git status failed"
	}
	if len(bytes.TrimSpace(status)) > 0 {
		return head, "", "uncommitted changes"
	}
	if dir := localReplace(filepath.Join(repo, "go.mod")); dir != "" {
		return head, "", "its go.mod replaces a module with a local directory (" + dir + ")"
	}
	if toolchain == "" {
		return head, "", "no toolchain"
	}
	h := sha256.New()
	js, err := json.Marshal(settings)
	if err != nil {
		return head, "", err.Error()
	}
	fmt.Fprintf(h, "%s\n%s\n%s\n", toolchain, tree, js)
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return head, "", fmt.Sprintf("rule file %s: %v", f, err)
		}
		fmt.Fprintf(h, "%s %x\n", f, sha256.Sum256(data))
	}
	return head, hex.EncodeToString(h.Sum(nil)), ""
}

// localReplace returns the first local directory gomod replaces a module
// with, "" when none.
func localReplace(gomod string) string {
	data, err := os.ReadFile(gomod)
	if err != nil {
		return ""
	}
	f, err := modfile.Parse(gomod, data, nil)
	if err != nil {
		return ""
	}
	for _, r := range f.Replace {
		if r.New.Version == "" { // a directory, not a module version
			return r.New.Path
		}
	}
	return ""
}

// Has reports whether the run in dir holds every service's manifest.
func Has(dir string, services []string) bool {
	for _, s := range services {
		if st, err := os.Stat(filepath.Join(dir, s, "manifest.json")); err != nil || st.IsDir() {
			return false
		}
	}
	return len(services) > 0
}

// Copy copies the services' directories of the run in prev into out.
func Copy(prev, out string, services []string) error {
	for _, s := range services {
		dst := filepath.Join(out, s)
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
		if err := os.CopyFS(dst, os.DirFS(filepath.Join(prev, s))); err != nil {
			return err
		}
	}
	return nil
}
