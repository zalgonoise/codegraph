package query

import (
	"fmt"
	"path"
	"strings"

	"github.com/zalgonoise/codegraph/internal/bundle"
	"github.com/zalgonoise/codegraph/internal/inventory"
)

// ImageAnswer is the answer of Image: the repositories whose CI builds an
// image, those whose files use it without building it, and the containers of
// the run's deployment running it.
type ImageAnswer struct {
	Image     string            `json:"image"` // the name asked about, without registry or tag
	Builders  []inventory.Built `json:"builders"`
	Referrers []inventory.Built `json:"referrers"`
	Deployed  []ImageContainer  `json:"deployed"`
	Note      string            `json:"note,omitempty"` // what the answer could not use
}

// ImageContainer is a container of the deployment running the image.
type ImageContainer struct {
	Environment string   `json:"environment"`
	Workload    string   `json:"workload"`
	Container   string   `json:"container"`
	Image       string   `json:"image"`
	Tag         string   `json:"tag,omitempty"`
	Repo        string   `json:"repo,omitempty"`
	Built       bool     `json:"built,omitempty"` // Repo was chosen because its CI builds the image
	AlsoBuilt   []string `json:"alsoBuilt,omitempty"`
	Service     string   `json:"service,omitempty"`
}

// UseInventory sets the inventory image questions read: an inventory.json,
// or a directory holding one. By default, Load reads the one next to the run
// (<run>/../inventory/inventory.json), as the Makefile lays them out.
func (r *Run) UseInventory(file string) error {
	if file == "" {
		return nil
	}
	inv, err := bundle.ReadInventory(file)
	if err != nil {
		return err
	}
	r.Inventory = inv
	return nil
}

// Image answers which repositories build or use an image, and where the
// deployment runs it. ref is a reference or a name ("registry/team/app:v1",
// "app"); the registry and tag are left out.
func (r *Run) Image(ref string) ImageAnswer {
	name := imageName(ref)
	a := ImageAnswer{Image: name}
	if r.Inventory == nil {
		a.Note = "no inventory next to the run (codegraph inventory writes one; -inventory points at another): " +
			"only the deployment is searched"
	} else {
		a.Builders, a.Referrers = r.Inventory.Builders(ref), r.Inventory.Referrers(ref)
	}
	if r.Deploy == nil {
		if a.Note != "" {
			a.Note += "; "
		}
		a.Note += "no deployment in the run (codegraph deploy)"
		return a
	}
	for _, w := range r.Deploy.Workloads {
		for _, c := range w.Containers {
			if !strings.EqualFold(c.Image, name) && !strings.EqualFold(path.Base(c.Image), path.Base(name)) {
				continue
			}
			a.Deployed = append(a.Deployed, ImageContainer{
				Environment: r.Deploy.Environment, Workload: w.Name, Container: c.Name, Image: c.Image, Tag: c.Tag,
				Repo: c.Repo, Built: c.Built, AlsoBuilt: c.AlsoBuilt, Service: c.Service,
			})
		}
	}
	return a
}

// imageName drops a reference's registry (a first segment with a dot or a
// colon, or a known proxy segment) and tag.
func imageName(ref string) string {
	ref = strings.TrimSpace(ref)
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	if first, rest, ok := strings.Cut(ref, "/"); ok && strings.ContainsAny(first, ".:") {
		ref = rest
	}
	return strings.ToLower(ref)
}

// ImageMarkdown renders Image.
func ImageMarkdown(a ImageAnswer) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Image `%s`\n", a.Image)
	built := func(title string, bs []inventory.Built, none string) {
		fmt.Fprintf(&b, "\n## %s (%d)\n\n", title, len(bs))
		if len(bs) == 0 {
			b.WriteString(none + "\n")
		}
		for i, x := range bs {
			if i == maxRows {
				fmt.Fprintf(&b, "- … %d more (`-json` lists them all)\n", len(bs)-maxRows)
				break
			}
			line := fmt.Sprintf("- **%s** builds `%s`", x.Repo, x.Image.Name)
			if title != "Built by" {
				line = fmt.Sprintf("- **%s** uses `%s`", x.Repo, x.Image.Name)
			}
			if x.Image.Tag != "" {
				line += ":`" + x.Image.Tag + "`"
			}
			line += fmt.Sprintf(" @ `%s` (%s", x.Image.File, x.Image.Via)
			if x.Image.Dockerfile != "" {
				line += ", `" + x.Image.Dockerfile + "`"
			}
			if x.LastCommit != "" {
				line += ", last commit " + x.LastCommit
			}
			b.WriteString(line + ")\n")
		}
	}
	built("Built by", a.Builders, "_No repository's CI or compose files build it: a third-party image, or one built elsewhere._")
	built("Used by, without building it", a.Referrers, "_No repository's Dockerfiles, compose files, Helm values or manifests use it._")
	if len(a.Deployed) > 0 {
		fmt.Fprintf(&b, "\n## Runs in %s (%d containers)\n\n", a.Deployed[0].Environment, len(a.Deployed))
		for _, c := range a.Deployed {
			line := fmt.Sprintf("- workload `%s`, container `%s`: `%s", c.Workload, c.Container, c.Image)
			if c.Tag != "" {
				line += ":" + c.Tag
			}
			line += "`"
			if c.Repo != "" {
				how := "matched by name"
				if c.Built {
					how = "its CI builds the image"
				}
				line += fmt.Sprintf(" → repository **%s** (%s", c.Repo, how)
				if len(c.AlsoBuilt) > 0 {
					line += "; also built by " + strings.Join(c.AlsoBuilt, ", ")
				}
				line += ")"
			}
			if c.Service != "" {
				line += ", service `" + c.Service + "`"
			}
			b.WriteString(line + "\n")
		}
	}
	if a.Note != "" {
		fmt.Fprintf(&b, "\n_%s._\n", a.Note)
	}
	return b.String()
}

// bundleNote says, for a run read from a bundle, when and by which codegraph it
// was made and what it leaves out: answers are as old as the bundle.
func (r *Run) bundleNote() string {
	m := r.Bundle
	if m == nil {
		return ""
	}
	note := fmt.Sprintf("\n_From a bundle made %s by codegraph %s", m.Created.Format("2006-01-02 15:04 MST"), m.Codegraph)
	if len(m.AnalysedBy) > 0 {
		note += " (analysed by " + strings.Join(m.AnalysedBy, ", ") + ")"
	}
	var left []string
	if m.NoSettings {
		left = append(left, "configured values")
	}
	if m.NoPositions {
		left = append(left, "file:line positions")
	}
	if len(left) > 0 {
		note += "; it leaves out " + strings.Join(left, " and ")
	}
	return note + "._\n"
}
