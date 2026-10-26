package query

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zalgonoise/codegraph/internal/deploy"
	"github.com/zalgonoise/codegraph/internal/model"
)

// The answers as Markdown: compact, with positions (relative to the
// service's repository) to cite, and a note whenever a list is cut.

// ServicesMarkdown renders Services.
func ServicesMarkdown(ss []ServiceSummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d services. Journeys per transport; upstream and downstream count linked services.\n\n", len(ss))
	b.WriteString("| Service | Repository | Journeys | Upstream | Downstream |\n|---|---|---|---|---|\n")
	for _, s := range ss {
		name := s.Name
		if s.Declared != "" {
			name += " (declared, " + s.Declared + ")"
		}
		if s.Library {
			name += " (library)"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %d |\n", name, repo(s.Module), perTransport(s.Journeys), s.Upstream, s.Downstream)
	}
	return b.String()
}

// ServiceMarkdown renders Service.
func ServiceMarkdown(d *ServiceDetail) string {
	var b strings.Builder
	if dd := d.Declared; dd != nil {
		fmt.Fprintf(&b, "# %s\n\nDeclared by hand, not analysed: %s (%s, repository %s at `%s`). Its endpoints and "+
			"journeys are as written in `%s`; positions are in its repository.\n", d.Name, dd.Description, dd.Language,
			dd.Repository, dd.Commit, dd.File)
	} else if d.Library {
		fmt.Fprintf(&b, "# %s\n\nA library, module `%s`, analysed through its exported API: each journey is an exported "+
			"function or method of its packages (transport `go`; those of internal packages listed apart), with what it reaches. Libraries are never "+
			"linked: their endpoints are what the API can do, not what runs.\n", d.Name, d.Module)
	} else {
		fmt.Fprintf(&b, "# %s\n\nModule `%s`.\n", d.Name, d.Module)
	}
	var shown, internal []JourneyRef
	for _, j := range d.Journeys {
		if j.Internal {
			internal = append(internal, j)
		} else {
			shown = append(shown, j)
		}
	}
	fmt.Fprintf(&b, "\n## Journeys (%d)\n\nEach starts at an inbound endpoint; `codegraph query journey %s <key>` follows one.\n\n", len(shown), d.Name)
	for i, j := range shown {
		if i == maxRows {
			fmt.Fprintf(&b, "- … %d more\n", len(shown)-maxRows)
			break
		}
		fmt.Fprintf(&b, "- %s `%s` → `%s`%s\n", j.Transport, j.Key, j.Handler, at(j.Position))
	}
	if len(internal) > 0 {
		fmt.Fprintf(&b, "\n## Internal API (%d)\n\nThe exported functions and methods of its internal packages, which only the module "+
			"itself calls; each has a journey too (`journey %s go <key>`).\n\n", len(internal), d.Name)
		for i, j := range internal {
			if i == maxRows {
				fmt.Fprintf(&b, "- … %d more (`-json` lists them all)\n", len(internal)-maxRows)
				break
			}
			fmt.Fprintf(&b, "- `%s`%s\n", j.Key, at(j.Position))
		}
	}
	links := func(title string, ls []model.Link, other func(model.Link) string) {
		fmt.Fprintf(&b, "\n## %s (%d)\n\n", title, len(ls))
		for i, l := range ls {
			if i == maxRows {
				fmt.Fprintf(&b, "- … %d more\n", len(ls)-maxRows)
				break
			}
			fmt.Fprintf(&b, "- %s: %s `%s` (%s, %s)\n", other(l), l.Transport, l.Key, l.Kind, l.Confidence)
		}
	}
	links("Upstream: links into it", d.Upstream, func(l model.Link) string { return l.From })
	links("Downstream: links out of it", d.Downstream, func(l model.Link) string { return l.To })
	if len(d.Shares) > 0 {
		links("Shares stores with (name-only)", d.Shares, func(l model.Link) string {
			if l.From == d.Name {
				return l.To
			}
			return l.From
		})
	}
	fmt.Fprintf(&b, "\n## Endpoints no analysed service answers (%d)\n\n", len(d.Unmatched))
	for i, e := range d.Unmatched {
		if i == maxRows {
			fmt.Fprintf(&b, "- … %d more\n", len(d.Unmatched)-maxRows)
			break
		}
		reason := ""
		if e.Reason != "" {
			reason = " — " + e.Reason
		}
		if e.Partial {
			reason += " (the endpoint's other keys link)"
		}
		fmt.Fprintf(&b, "- %s %s %s `%s`%s%s\n", e.Direction, e.Transport, e.Role, e.Key, reason, at(e.Position))
	}
	if dep := d.Deployed; dep != nil {
		if len(dep.Workloads) == 0 {
			fmt.Fprintf(&b, "\n## Not deployed in %s\n\nNo %s workload was matched to this main (`deploy/deploy.md` lists the "+
				"workloads whose main could not be told).\n", dep.Environment, dep.Environment)
		} else {
			fmt.Fprintf(&b, "\n## Deployed in %s (%d workloads)\n\n", dep.Environment, len(dep.Workloads))
		}
		for _, w := range dep.Workloads {
			line := fmt.Sprintf("- %s (%s", w.Name, strings.ToLower(w.Kind))
			if w.Schedule != "" {
				line += " `" + w.Schedule + "`"
			}
			if w.Replicas != nil {
				line += fmt.Sprintf(", %d replicas", *w.Replicas)
			}
			line += ")"
			if w.Command != "" {
				line += ": `" + w.Command + "`"
			}
			if len(w.Hosts) > 0 {
				line += "; hosts " + strings.Join(w.Hosts, ", ")
			}
			if len(w.Mocks) > 0 {
				line += "; **mocked:** " + strings.Join(w.Mocks, ", ")
			}
			if len(w.Env) > 0 {
				line += fmt.Sprintf("; %d settings (`-json` lists them)", len(w.Env))
			}
			b.WriteString(line + "\n")
		}
		if a := dep.Addresses; a != nil {
			targets := func(ps []deploy.Pair, settings bool) string {
				var out []string
				for _, p := range ps {
					t := p.To
					if settings && len(p.Settings) > 0 {
						t += " (`" + strings.Join(p.Settings, "`, `") + "`)"
					}
					out = append(out, t)
				}
				return strings.Join(out, ", ")
			}
			fmt.Fprintf(&b, "\nIts direct links (gRPC, HTTP) against the addresses configured in %s:\n\n", dep.Environment)
			if len(a.Confirmed) > 0 {
				fmt.Fprintf(&b, "- **configured** (%d): %s\n", len(a.Confirmed), targets(a.Confirmed, false))
			}
			if len(a.CodeOnly) > 0 {
				fmt.Fprintf(&b, "- **in the code, no address configured** (%d): %s\n", len(a.CodeOnly), targets(a.CodeOnly, false))
			}
			var read, unread, unchecked []deploy.Pair
			for _, p := range a.ConfigOnly {
				switch {
				case !p.Checked:
					unchecked = append(unchecked, p)
				case len(p.Unread) == len(p.Settings):
					unread = append(unread, p)
				default:
					read = append(read, p)
				}
			}
			if len(unchecked) > 0 {
				fmt.Fprintf(&b, "- **configured, no link in the code** (%d; a client the analysis does not read, or an unused "+
					"setting): %s\n", len(unchecked), targets(unchecked, true))
			}
			if len(read) > 0 {
				fmt.Fprintf(&b, "- **configured, no call found** (%d; its code reads the setting, so a call the analysis misses "+
					"or a client never used): %s\n", len(read), targets(read, true))
			}
			if len(unread) > 0 {
				fmt.Fprintf(&b, "- **configured, read by nothing in its code** (%d; a stale setting, or read in a way the "+
					"analysis does not see): %s\n", len(unread), targets(unread, true))
			}
			for _, st := range a.Settled {
				fmt.Fprintf(&b, "- `%s` is served by several services: it dials **%s** (`%s`), not %s\n", st.Key, st.To,
					st.Setting, strings.Join(st.NotDialled, ", "))
			}
			for _, m := range a.Mocked {
				fmt.Fprintf(&b, "- `%s` names its link to %s: not taken here, if the setting gates it\n", m.Setting, strings.Join(m.To, ", "))
			}
		}
	}
	return b.String()
}

// JourneyMarkdown renders Journey.
func JourneyMarkdown(d *JourneyDetail, depth int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s · %s `%s`\n\nHandler `%s`%s; %d module functions; components: %s.\n",
		d.Service, d.Transport, d.Key, d.Handler, at(d.Position), d.Funcs, strings.Join(d.Components, ", "))
	if d.WholeService() {
		b.WriteString("\nA whole-service journey: it covers every method of the service, so a call listed here may " +
			"come from any of them.\n")
	}
	if len(d.Uses) > 0 {
		var mods []string
		count := map[string]int{}
		for _, u := range d.Uses {
			if count[u.Module] == 0 {
				mods = append(mods, u.Module)
			}
			count[u.Module]++
		}
		parts := make([]string, len(mods))
		for i, m := range mods {
			parts[i] = fmt.Sprintf("%s (%d)", m, count[m])
		}
		fmt.Fprintf(&b, "\nIts own code calls %d exported functions and methods of other modules: %s. `uses <module>` "+
			"lists them with their call sites (`-json` here too).\n", len(d.Uses), strings.Join(parts, ", "))
	}
	if len(d.UsedBy) > 0 {
		fmt.Fprintf(&b, "\nCalled by journeys of the run (%d):\n", len(d.UsedBy))
		for n, j := range d.UsedBy {
			if n == maxRows {
				fmt.Fprintf(&b, "- … %d more (`-json` lists them all)\n", len(d.UsedBy)-maxRows)
				break
			}
			fmt.Fprintf(&b, "- **%s** %s `%s`%s\n", j.Service, j.Transport, j.Key, at(j.Site))
		}
	}
	fmt.Fprintf(&b, "\nOutbound calls in program order, followed %d service(s) beyond this one (`-depth`). "+
		"sync `->`, async `-)`; positions are call sites, each in its own service.\n\n", depth)
	writeSteps(&b, d.Steps, 0)
	if len(d.Steps) == 0 {
		b.WriteString("_No outbound calls._\n")
	}
	if d.Truncated {
		fmt.Fprintf(&b, "\n_Cut at %d steps: ask for less depth or a narrower journey._\n", maxSteps)
	}
	return b.String()
}

func writeSteps(b *strings.Builder, steps []Step, depth int) {
	indent := strings.Repeat("  ", depth)
	for _, s := range steps {
		arrow := "->"
		if s.Kind == model.Async {
			arrow = "-)"
		}
		line := fmt.Sprintf("%s- %s %s %s `%s`", indent, arrow, s.Transport, s.Role, s.Key)
		switch {
		case s.To != "" && s.Journey != "":
			line += fmt.Sprintf(" → **%s** `%s` (`%s`%s)", s.To, s.Journey, s.Handler, at(s.HandlerAt))
		case s.To != "":
			line += fmt.Sprintf(" → **%s** (no journey for that endpoint)", s.To)
		case s.Unmatched != "":
			line += " — " + s.Unmatched
		}
		var notes []string
		if s.Confidence != "" && s.Confidence != "exact" {
			notes = append(notes, s.Confidence)
		}
		if s.Background {
			notes = append(notes, "in a goroutine")
		}
		if s.Cycle {
			notes = append(notes, "cycle: already on this path")
		}
		if s.Repeated {
			notes = append(notes, "followed above")
		}
		if s.Deeper {
			notes = append(notes, "not followed: deeper than asked")
		}
		if len(notes) > 0 {
			line += " _(" + strings.Join(notes, ", ") + ")_"
		}
		b.WriteString(line + at(s.Position) + "\n")
		writeSteps(b, s.Steps, depth+1)
	}
}

// WhoMarkdown renders Who.
func WhoMarkdown(query string, us []KeyUsers) string { return whoMarkdown(query, us, "##") }

func whoMarkdown(query string, us []KeyUsers, heading string) string {
	var b strings.Builder
	if len(us) == 0 {
		fmt.Fprintf(&b, "No endpoint has a key like `%s` (`codegraph query search` looks wider).\n", query)
		return b.String()
	}
	for _, u := range us {
		fmt.Fprintf(&b, "\n%s %s `%s`\n", heading, u.Transport, u.Key)
		uses := func(title string, list []Use) {
			if len(list) == 0 {
				return
			}
			fmt.Fprintf(&b, "\n%s:\n", title)
			for _, x := range list {
				line := fmt.Sprintf("- **%s** %s", x.Service, x.Role)
				if x.Handler != "" {
					line += fmt.Sprintf(" `%s`", x.Handler)
				}
				if x.Message != "" {
					line += fmt.Sprintf(" (message `%s`)", x.Message)
				}
				if x.Direction == string(model.Inbound) {
					line += " registered" + at(x.Position)
					if x.HandlerPosition != "" {
						line += ", handler" + at(x.HandlerPosition)
					}
				} else {
					line += at(x.Position)
				}
				if len(x.Sites) > 1 {
					line += fmt.Sprintf(" (%d call sites: %s)", len(x.Sites), strings.Join(x.Sites, ", "))
				}
				if x.Unmatched {
					switch x.Reason {
					case "":
						line += " — nothing analysed answers it"
					default:
						line += " — unmatched: " + x.Reason
					}
				}
				if len(x.Journeys) > 0 {
					line += "; journeys: " + quoteList(x.Journeys, 12)
				}
				b.WriteString(line + "\n")
			}
		}
		uses("Sent by", u.Senders)
		uses("Received by", u.Receivers)
		uses("Used by", u.Users)
	}
	return strings.TrimPrefix(b.String(), "\n")
}

// PathsMarkdown renders Paths.
func PathsMarkdown(from, to string, ps []Path) string {
	var b strings.Builder
	if len(ps) == 0 {
		fmt.Fprintf(&b, "No chain of links leads from %s to %s.\n", from, to)
		return b.String()
	}
	fmt.Fprintf(&b, "%d chain(s) from %s to %s, shortest first (each hop lists every link between the two services). "+
		"These follow links between services: a given journey may take none of them (`journey` shows what one does).\n",
		len(ps), from, to)
	for i, p := range ps {
		fmt.Fprintf(&b, "\n%d. %s", i+1, from)
		for _, hop := range p {
			var ks []string
			for _, l := range hop {
				ks = append(ks, fmt.Sprintf("%s `%s` %s", l.Transport, l.Key, l.Kind))
			}
			fmt.Fprintf(&b, " → %s [%s]", hop[0].To, strings.Join(ks, "; "))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// ImpactMarkdown renders Impact.
func ImpactMarkdown(im *Impact) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# What depends on %s `%s`\n", im.Kind, im.Target)
	if im.Kind == "service" {
		fmt.Fprintf(&b, "\n## Direct: links into it (%d)\n\n", len(im.Direct))
		for _, l := range im.Direct {
			fmt.Fprintf(&b, "- %s: %s `%s` (%s, %s)\n", l.From, l.Transport, l.Key, l.Kind, l.Confidence)
		}
		if len(im.Shares) > 0 {
			fmt.Fprintf(&b, "\n## Shares stores with (%d, name-only)\n\n", len(im.Shares))
			for _, l := range im.Shares {
				other := l.From
				if other == im.Target {
					other = l.To
				}
				fmt.Fprintf(&b, "- %s: %s `%s`\n", other, l.Transport, l.Key)
			}
		}
		b.WriteString("\n## Journeys of other services that reach it")
	} else {
		b.WriteString("\n## Direct: its endpoints\n")
		b.WriteString(whoMarkdown(im.Target, im.Users, "###"))
		b.WriteString("\n## Journeys that use or reach it")
	}
	fmt.Fprintf(&b, " (%d), nearest first\n\n", len(im.Journeys))
	b.WriteString("Distance counts the services crossed to get there (0: the journey uses it itself); the chain shows them, " +
		"ending with the link into the target. Each journey is listed once, by its nearest way in.")
	if im.Kind == "service" {
		b.WriteString(" This counts calls and messages; for an API change, `impact <gRPC service>` narrows it to the " +
			"callers of one contract (`service " + im.Target + "` lists its contracts).")
	}
	b.WriteString("\n\n")
	for i, j := range im.Journeys {
		if i == maxRows {
			fmt.Fprintf(&b, "- … %d more (`-json` lists them all)\n", len(im.Journeys)-maxRows)
			break
		}
		via := ""
		if len(j.Chain) > 0 {
			via = fmt.Sprintf(": %s (%s)", strings.Join(j.Chain, " → "), j.Via)
		}
		whole := ""
		if j.WholeService() {
			whole = " _(whole service)_"
		}
		fmt.Fprintf(&b, "- %d · **%s** %s `%s`%s%s\n", j.Distance, j.Service, j.Transport, j.Key, whole, via)
	}
	return b.String()
}

// UsesMarkdown renders Uses.
func UsesMarkdown(text string, us []SymbolUsers) string {
	var b strings.Builder
	if len(us) == 0 {
		fmt.Fprintf(&b, "Nothing analysed calls a library symbol like `%s` (a module or package path, or part of a "+
			"symbol's name; the standard library only counts when the run was made with -std).\n", text)
		return b.String()
	}
	fmt.Fprintf(&b, "Exported functions and methods of other modules matching `%s`, with the services whose code calls "+
		"them and the journeys that do (each at its first call site, in that service's repository).\n", text)
	for _, u := range us {
		fmt.Fprintf(&b, "\n## `%s` (%s)\n\n", u.Symbol, u.Module)
		if a := u.API; a != nil {
			fmt.Fprintf(&b, "Analysed in library **%s**: `journey %s %s %s`%s.\n", a.Service, a.Service, a.Transport, a.Key, at(a.Position))
		}
		if len(u.Services) > 0 {
			fmt.Fprintf(&b, "Services: %s.\n", strings.Join(u.Services, ", "))
		}
		for n, j := range u.Journeys {
			if n == maxRows {
				fmt.Fprintf(&b, "- … %d more journeys (`-json` lists them all)\n", len(u.Journeys)-maxRows)
				break
			}
			calls := ""
			if j.Calls > 1 {
				calls = fmt.Sprintf(", %d call sites", j.Calls)
			}
			fmt.Fprintf(&b, "- **%s** %s `%s`%s%s\n", j.Service, j.Transport, j.Key, at(j.Site), calls)
		}
	}
	return b.String()
}

// FoundMarkdown renders Search.
func FoundMarkdown(text string, f Found) string {
	var b strings.Builder
	if len(f.Services)+len(f.Journeys)+len(f.Keys) == 0 {
		fmt.Fprintf(&b, "Nothing matches `%s`.\n", text)
		return b.String()
	}
	if len(f.Services) > 0 {
		fmt.Fprintf(&b, "Services: %s\n", strings.Join(f.Services, ", "))
	}
	if len(f.Journeys) > 0 {
		b.WriteString("\nJourneys:\n")
		for _, j := range f.Journeys {
			fmt.Fprintf(&b, "- **%s** %s `%s` → `%s`\n", j.Service, j.Transport, j.Key, j.Handler)
		}
	}
	if len(f.Keys) > 0 {
		b.WriteString("\nKeys (`codegraph query who <key>` for their users):\n")
		for _, k := range f.Keys {
			t, key, _ := strings.Cut(k, " ")
			fmt.Fprintf(&b, "- %s `%s`\n", t, key)
		}
	}
	return b.String()
}

func at(pos string) string {
	if pos == "" {
		return ""
	}
	return " @ `" + pos + "`"
}

func repo(module string) string {
	parts := strings.Split(module, "/")
	if n := len(parts); n > 1 && strings.HasPrefix(parts[n-1], "v") && strings.Trim(parts[n-1][1:], "0123456789") == "" {
		return parts[n-2]
	}
	return parts[len(parts)-1]
}

func perTransport(m map[string]int) string {
	if len(m) == 0 {
		return "—"
	}
	var parts []string
	for _, t := range sortedKeys(m) {
		parts = append(parts, fmt.Sprintf("%d %s", m[t], t))
	}
	return strings.Join(parts, ", ")
}

func quoteList(xs []string, limit int) string {
	sort.Strings(xs)
	var out []string
	for i, x := range xs {
		if i == limit {
			out = append(out, fmt.Sprintf("… %d more", len(xs)-limit))
			break
		}
		out = append(out, "`"+x+"`")
	}
	return strings.Join(out, ", ")
}
