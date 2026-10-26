package model

import (
	"path"
	"regexp"
	"strings"
)

// A library's API (Manifest.Library) is keyed by package and member:
// "fts.New", "fts.Index.Search", "fts/log.Logger.Info". Code of other modules
// records the library symbols it calls (Journey.Uses) by package path and
// label; APICalls pairs those calls with the journeys of the analysed
// libraries' symbols.

// APIQualifier names a package of a module in API keys: the module's base
// name (without a major version suffix) for its root package, followed by the
// package's path below the module ("fts", "fts/log"); a package outside the
// module keeps its path.
func APIQualifier(pkg, module string) string {
	name := path.Base(module)
	if majorVersion.MatchString(name) && path.Dir(module) != "." {
		name = path.Base(path.Dir(module))
	}
	switch {
	case pkg == module:
		return name
	case module != "" && strings.HasPrefix(pkg, module+"/"):
		return name + strings.TrimPrefix(pkg, module)
	}
	return pkg
}

var majorVersion = regexp.MustCompile(`^v[0-9]+$`)

// APIMember turns a function's label into the member of its package it
// names, without type arguments or the receiver's pointer: "fts.New" → "New",
// "(*fts.Index[K, V]).Search" → "Index.Search",
// "fts.Indexer[string, string].Search" → "Indexer.Search".
func APIMember(label string) string {
	var b strings.Builder
	depth := 0
	for _, r := range label {
		switch {
		case r == '[':
			depth++
		case r == ']':
			depth--
		case depth > 0, r == '(', r == ')', r == '*':
		default:
			b.WriteRune(r)
		}
	}
	s := b.String()
	if _, member, ok := strings.Cut(s, "."); ok {
		return member
	}
	return s
}

// APIKey is the key of a library's API journey for a symbol of package pkg
// (an import path below module) with the given label.
func APIKey(pkg, module, label string) string {
	return APIQualifier(pkg, module) + "." + APIMember(label)
}

// JourneyAt locates a journey: ms[Manifest].Journeys[Journey].
type JourneyAt struct{ Manifest, Journey int }

// APICall is a journey whose code calls an exported symbol of a library the
// run analysed, and that symbol's journey in the library.
type APICall struct {
	Caller, API JourneyAt
	Use         SymbolUse
}

// APIIndex finds the journeys of the analysed libraries' exported symbols.
type APIIndex struct {
	modules []string             // the libraries' modules
	byKey   map[string]JourneyAt // package|key -> the symbol's journey
}

// NewAPIIndex indexes the API journeys of the libraries among ms; it is nil
// when there is none.
func NewAPIIndex(ms []*Manifest) *APIIndex {
	var x *APIIndex
	for mi, m := range ms {
		if !m.Library {
			continue
		}
		if x == nil {
			x = &APIIndex{byKey: map[string]JourneyAt{}}
		}
		x.modules = append(x.modules, m.Module)
		pkgOf := map[string]string{}
		for _, ep := range m.Endpoints {
			pkgOf[ep.ID] = ep.Component
		}
		for ji, j := range m.Journeys {
			if j.Transport == APITransport {
				x.byKey[pkgOf[j.Entrypoint]+"|"+j.Key] = JourneyAt{mi, ji}
			}
		}
	}
	return x
}

// Find returns the journey of the symbol (a label, as Journey.Uses records
// it) of package pkg, when the run analysed its library.
func (x *APIIndex) Find(pkg, symbol string) (JourneyAt, bool) {
	if x == nil || pkg == "" {
		return JourneyAt{}, false
	}
	for _, module := range x.modules {
		if pkg != module && !strings.HasPrefix(pkg, module+"/") {
			continue
		}
		if at, ok := x.byKey[pkg+"|"+APIKey(pkg, module, symbol)]; ok {
			return at, true
		}
	}
	return JourneyAt{}, false
}

// APICalls lists the calls of journeys into the exported symbols of the
// analysed libraries among ms, in manifest and journey order.
func APICalls(ms []*Manifest) []APICall {
	x := NewAPIIndex(ms)
	if x == nil {
		return nil
	}
	var out []APICall
	for mi, m := range ms {
		for ji, j := range m.Journeys {
			for _, u := range j.Uses {
				if at, ok := x.Find(u.Package, u.Symbol); ok && at != (JourneyAt{mi, ji}) {
					out = append(out, APICall{Caller: JourneyAt{mi, ji}, API: at, Use: u})
				}
			}
		}
	}
	return out
}
