package configfile

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zalgonoise/codegraph/internal/options"
)

// Template writes a starting configuration file (codegraph init): with a
// profile (its directory; "" for none), a daily pipeline running it, and every
// setting listed in comments.
func Template(profileDir string) string {
	var b strings.Builder
	b.WriteString(`# codegraph's configuration, written by codegraph init.
#
#   codegraph                   runs the default pipeline (from a terminal)
#   codegraph run <pipeline>    runs another, or a step: inventory, graph, deploy, checks, diff, bundle
#   codegraph run -dry-run      prints what a run would do, as a shell script
#   codegraph config            shows every setting resolved, and where it comes from
#   codegraph check             checks the tools, files and repositories a run needs
#   codegraph status            tells how current the last run is
#
# Settings layer, each over the one before: the built-in defaults, the
# profile's, the pipeline's, the step's, the command line's. Paths are relative
# to this file; ~ is the home directory. Only a file in the home directory or
# named by -config or $CODEGRAPH_CONFIG may run commands (exec steps, and the
# checks of its profiles).
version: 1

`)
	name := "default"
	if profileDir != "" {
		name = profileName(profileDir)
		fmt.Fprintf(&b, `# Profiles: an organisation's directory with its profile.yaml (repositories,
# rules, chart, checks), by name.
profiles:
  %s: %s

# The profile when nothing else names one: steps run by name (codegraph run
# graph), query and mcp.
profile: %s

`, name, yamlString(profileDir), name)
	} else {
		b.WriteString(`# Profiles: an organisation's directory with its profile.yaml (repositories,
# rules, chart, checks), by name; profile is the one used when nothing else
# names one.
# profiles:
#   acme: ~/src/acme-codegraph
# profile: acme

`)
	}
	b.WriteString(`# What codegraph alone runs.
default: daily

pipelines:
  daily:
`)
	if profileDir != "" {
		fmt.Fprintf(&b, "    profile: %s\n", name)
	} else {
		b.WriteString("    root: ..                 # the directory holding your Go repositories\n")
	}
	b.WriteString(`    steps:
      # - exec: git -C ~/src/repo pull    # a command of yours first, such as updating the clones
      - inventory
      - graph
      - bundle
    # Settings over the profile's, for every step (see the list below):
    # out: ~/codegraph/graph
    # bundle: ~/codegraph/graph.tar.gz
    # focus: none                  # every Go repository of root
    # api: true
    # A step takes settings of its own: - graph: {full: true}

# What codegraph query and mcp read when no -run is given: a run directory or
# a bundle (else the profile's out).
# query:
#   run: ~/codegraph/graph.tar.gz

# Every setting, for a profile's defaults, a pipeline or one of its steps:
`)
	width := 0
	for _, f := range options.Fields() {
		width = max(width, len(f.Name))
	}
	for _, f := range options.Fields() {
		fmt.Fprintf(&b, "#   %-*s  %s\n", width, f.Name, f.Help)
	}
	return b.String()
}

var nonName = regexp.MustCompile(`[^a-z0-9-]+`)

// profileName names a profile after its directory.
func profileName(dir string) string {
	n := strings.TrimSuffix(strings.ToLower(filepath.Base(dir)), "-codegraph")
	n = strings.TrimPrefix(n, "codegraph-")
	if n = strings.Trim(nonName.ReplaceAllString(n, "-"), "-"); n == "" {
		return "default"
	}
	return n
}

func yamlString(s string) string {
	if strings.ContainsAny(s, ":#{}[],&*?|<>=!%@`'\"") || strings.HasPrefix(s, " ") {
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return s
}
