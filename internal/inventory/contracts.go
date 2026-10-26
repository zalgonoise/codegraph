package inventory

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Contract is a gRPC service defined in a contracts catalogue (a repository
// whose proto/ tree defines the organisation's contracts), named as the
// analysis names contracts: the proto package and the service,
// customers.kyc.v1.KYCService.
type Contract struct {
	Name     string   `json:"name"`
	Repo     string   `json:"repo"`
	Position string   `json:"position"` // the service's definition, file:line in the repository
	Methods  []string `json:"methods,omitempty"`
}

var (
	protoPackageRe = regexp.MustCompile(`^\s*package\s+([\w.]+)\s*;`)
	protoServiceRe = regexp.MustCompile(`^\s*service\s+(\w+)\s*\{`)
	protoRPCRe     = regexp.MustCompile(`\brpc\s+(\w+)\s*\(`)
)

// Catalogue reads the gRPC services defined by the .proto files under each
// catalogue repository's proto/ directory (dir is the repository), sorted by
// name.
func Catalogue(dirs []string) ([]Contract, error) {
	var out []Contract
	for _, dir := range dirs {
		repo := strings.TrimSuffix(filepath.Base(dir), ".git")
		root := filepath.Join(dir, "proto")
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || filepath.Ext(p) != ".proto" {
				return err
			}
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			cs, err := protoServices(p, filepath.ToSlash(rel))
			for i := range cs {
				cs[i].Repo = repo
			}
			out = append(out, cs...)
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("catalogue %s: %w", repo, err)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name || out[i].Name == out[j].Name && out[i].Repo < out[j].Repo
	})
	return out, nil
}

// protoServices reads the services of one .proto file and their methods.
// Comments are dropped and braces counted, so options inside a method's body
// do not end the service.
func protoServices(file, rel string) ([]Contract, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var (
		pkg     string
		out     []Contract
		current = -1 // index in out of the service being read
		depth   int
		block   bool // inside a /* */ comment
	)
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if block {
			end := strings.Index(line, "*/")
			if end < 0 {
				continue
			}
			line, block = line[end+2:], false
		}
		if i := strings.Index(line, "/*"); i >= 0 {
			if end := strings.Index(line[i:], "*/"); end >= 0 {
				line = line[:i] + line[i+end+2:]
			} else {
				line, block = line[:i], true
			}
		}
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		switch {
		case depth == 0 && protoPackageRe.MatchString(line):
			pkg = protoPackageRe.FindStringSubmatch(line)[1]
		case depth == 0 && protoServiceRe.MatchString(line):
			name := protoServiceRe.FindStringSubmatch(line)[1]
			if pkg != "" {
				name = pkg + "." + name
			}
			out = append(out, Contract{Name: name, Position: fmt.Sprintf("%s:%d", rel, n)})
			current = len(out) - 1
			for _, m := range protoRPCRe.FindAllStringSubmatch(line, -1) { // service S { rpc M(…) … }
				out[current].Methods = append(out[current].Methods, m[1])
			}
		case depth == 1 && current >= 0 && protoRPCRe.MatchString(line):
			out[current].Methods = append(out[current].Methods, protoRPCRe.FindStringSubmatch(line)[1])
		}
		depth += strings.Count(line, "{") - strings.Count(line, "}")
		if depth <= 0 {
			depth, current = 0, -1
		}
	}
	return out, sc.Err()
}

// ContractUse is who serves and who calls a contract in an analysed run.
type ContractUse struct {
	Servers, Callers []string // services; declared ones marked "(declared)"
}

// ContractsReport renders the catalogue against a run: the contracts the run
// calls that nothing in it serves (their owners were not analysed), the ones
// it serves, and the ones nothing in it uses. use is keyed by contract name;
// a nil use (no run) lists the catalogue alone.
func ContractsReport(cs []Contract, use map[string]ContractUse) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Contracts\n\n%d gRPC services defined in the contracts catalogues (%s).\n", len(cs), strings.Join(catalogueRepos(cs), ", "))
	if use == nil {
		b.WriteString("\n| Contract | Methods | Defined in |\n|---|---|---|\n")
		for _, c := range cs {
			fmt.Fprintf(&b, "| `%s` | %d | %s `%s` |\n", c.Name, len(c.Methods), c.Repo, c.Position)
		}
		return b.String()
	}
	var unserved, served, unused []Contract
	for _, c := range cs {
		u := use[c.Name]
		switch {
		case len(u.Servers) > 0:
			served = append(served, c)
		case len(u.Callers) > 0:
			unserved = append(unserved, c)
		default:
			unused = append(unused, c)
		}
	}
	fmt.Fprintf(&b, "Against the run: %d called with no server in it, %d served, %d neither served nor called by it.\n",
		len(unserved), len(served), len(unused))
	section := func(title, note string, list []Contract) {
		if len(list) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n## %s (%d)\n\n%s\n\n| Contract | Methods | Defined in | Served by | Called by |\n|---|---|---|---|---|\n", title, len(list), note)
		for _, c := range list {
			u := use[c.Name]
			fmt.Fprintf(&b, "| `%s` | %d | %s `%s` | %s | %s |\n", c.Name, len(c.Methods), c.Repo, c.Position,
				strings.Join(u.Servers, ", "), strings.Join(u.Callers, ", "))
		}
	}
	section("Called, with no server in the run", "Their servers are in repositories that were not analysed, or not written in Go (describe them in `declared/`).", unserved)
	section("Served", "Servers marked (declared) were described by hand.", served)
	section("Neither served nor called by the run", "Used outside the analysed services, or no longer used.", unused)
	return b.String()
}

func catalogueRepos(cs []Contract) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range cs {
		if !seen[c.Repo] {
			seen[c.Repo] = true
			out = append(out, c.Repo)
		}
	}
	sort.Strings(out)
	return out
}
