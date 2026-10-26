// Package settings lists the environment variables a program reads: the
// names of the configuration types it parses, and the constant names it
// gives os.Getenv, os.LookupEnv and syscall.Getenv in reachable code outside
// the standard library. Two configuration libraries are understood:
//
//   - caarlos0/env: the `env` tags, with `envPrefix` composed through nested
//     structs. A type counts when reachable code converts it (or a pointer to
//     it) to an interface, as env.Parse(&cfg) and env.ParseAs[T]() do.
//   - kelseyhightower/envconfig: every exported field of the type passed to
//     envconfig.Process(prefix, &spec) (or MustProcess). A field's `envconfig`
//     tag names it (the library always looks that name up); an untagged field
//     is named from the prefix and its name (`split_words` splits camel case),
//     and a nested struct's fields take its name as their prefix. Fields
//     tagged `ignored:"true"` are skipped.
//
// Names built at run time (a Parse option's prefix, viper's AutomaticEnv,
// os.Getenv of a variable) are not seen.
package settings

import (
	"go/constant"
	"go/types"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/zalgonoise/codegraph/internal/reach"
)

// Names returns the settings the program reads, sorted.
func Names(res *reach.Result) []string {
	out := map[string]bool{}
	roots := map[types.Type]bool{}
	for fn := range res.Reachable {
		std := fn.Pkg != nil && standard(fn.Pkg.Pkg.Path())
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				switch instr := instr.(type) {
				case *ssa.MakeInterface:
					roots[instr.X.Type()] = true
				case ssa.CallInstruction:
					if !std { // the standard library's own (ZONEINFO, GODEBUG, …) are not the service's
						getenv(instr.Common(), out)
						envconfigSpec(instr.Common(), out)
					}
				}
			}
		}
	}
	for t := range roots {
		tags(t, "", map[types.Type]bool{}, out)
	}
	names := make([]string, 0, len(out))
	for n := range out {
		if n != "" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

// getenv adds the constant name a call to os.Getenv, os.LookupEnv or
// syscall.Getenv reads.
func getenv(common *ssa.CallCommon, out map[string]bool) {
	callee := common.StaticCallee()
	if callee == nil || callee.Pkg == nil || len(common.Args) == 0 {
		return
	}
	switch callee.Pkg.Pkg.Path() + "." + callee.Name() {
	case "os.Getenv", "os.LookupEnv", "syscall.Getenv":
		if c, ok := common.Args[0].(*ssa.Const); ok && c.Value != nil && c.Value.Kind() == constant.String {
			out[constant.StringVal(c.Value)] = true
		}
	}
}

// tags adds the `env` names of a struct type's fields, through pointers and
// nested structs (as caarlos0/env parses them), with prefix composed from
// `envPrefix`. onPath guards against recursive types.
func tags(t types.Type, prefix string, onPath map[types.Type]bool, out map[string]bool) {
	if p, ok := t.Underlying().(*types.Pointer); ok {
		t = p.Elem()
	}
	st, ok := t.Underlying().(*types.Struct)
	if !ok || onPath[t] {
		return
	}
	onPath[t] = true
	defer delete(onPath, t)
	for i := 0; i < st.NumFields(); i++ {
		tag := reflect.StructTag(st.Tag(i))
		name, _, _ := strings.Cut(tag.Get("env"), ",")
		if name != "" && name != "-" {
			out[prefix+name] = true
			continue
		}
		if name != "-" {
			tags(st.Field(i).Type(), prefix+tag.Get("envPrefix"), onPath, out)
		}
	}
}

// envconfigSpec adds the names kelseyhightower/envconfig reads for the spec
// passed to Process or MustProcess, with their constant prefix.
func envconfigSpec(common *ssa.CallCommon, out map[string]bool) {
	callee := common.StaticCallee()
	if callee == nil || callee.Pkg == nil || len(common.Args) < 2 || callee.Pkg.Pkg.Path() != envconfigPkg {
		return
	}
	if name := callee.Name(); name != "Process" && name != "MustProcess" {
		return
	}
	prefix := ""
	if c, ok := common.Args[0].(*ssa.Const); ok && c.Value != nil && c.Value.Kind() == constant.String {
		prefix = constant.StringVal(c.Value)
	}
	spec := common.Args[1]
	if mi, ok := spec.(*ssa.MakeInterface); ok {
		spec = mi.X
	}
	envconfigNames(spec.Type(), prefix, map[types.Type]bool{}, out)
}

const envconfigPkg = "github.com/kelseyhightower/envconfig"

// envconfigNames adds the names envconfig gives a spec's fields, as its
// gatherInfo does: the key is the field name (split at camel case with
// `split_words`), or its `envconfig` tag, after the prefix and an underscore,
// uppercased; a nested struct's fields take its key as their prefix (its
// embedded fields keep the prefix). A tagged field is also looked up by its
// tag alone, the name listed here.
func envconfigNames(t types.Type, prefix string, onPath map[types.Type]bool, out map[string]bool) {
	if p, ok := t.Underlying().(*types.Pointer); ok {
		t = p.Elem()
	}
	st, ok := t.Underlying().(*types.Struct)
	if !ok || onPath[t] {
		return
	}
	onPath[t] = true
	defer delete(onPath, t)
	for i := 0; i < st.NumFields(); i++ {
		f := st.Field(i)
		tag := reflect.StructTag(st.Tag(i))
		if !f.Exported() || strings.EqualFold(tag.Get("ignored"), "true") {
			continue
		}
		alt := strings.ToUpper(tag.Get("envconfig"))
		key := f.Name()
		if strings.EqualFold(tag.Get("split_words"), "true") {
			key = splitWords(key)
		}
		if alt != "" {
			key = alt
		}
		if prefix != "" {
			key = prefix + "_" + key
		}
		key = strings.ToUpper(key)
		ft := f.Type()
		if p, ok := ft.Underlying().(*types.Pointer); ok {
			ft = p.Elem()
		}
		if _, isStruct := ft.Underlying().(*types.Struct); isStruct && !decodes(ft) {
			inner := prefix
			if !f.Anonymous() {
				inner = key
			}
			envconfigNames(ft, inner, onPath, out)
			continue
		}
		if alt != "" {
			out[alt] = true
			continue
		}
		out[key] = true
	}
}

var (
	wordsRe   = regexp.MustCompile("([^A-Z]+|[A-Z]+[^A-Z]+|[A-Z]+)")
	acronymRe = regexp.MustCompile("([A-Z]+)([A-Z][^A-Z]+)")
)

// splitWords splits a camel-case field name into words joined by "_", as
// envconfig's split_words does (HTTPPort: HTTP_Port).
func splitWords(name string) string {
	var words []string
	for _, w := range wordsRe.FindAllStringSubmatch(name, -1) {
		if m := acronymRe.FindStringSubmatch(w[0]); len(m) == 3 {
			words = append(words, m[1], m[2])
			continue
		}
		words = append(words, w[0])
	}
	return strings.Join(words, "_")
}

// decodes reports whether a struct type decodes itself from a string (as
// envconfig's Decoder, Setter, TextUnmarshaler and BinaryUnmarshaler do), so
// it is one setting rather than a nested spec.
func decodes(t types.Type) bool {
	ms := types.NewMethodSet(types.NewPointer(t))
	for _, name := range []string{"Decode", "Set", "UnmarshalText", "UnmarshalBinary"} {
		if ms.Lookup(nil, name) != nil {
			return true
		}
	}
	return false
}

// standard reports whether a package is in the standard library (no dot in
// its first path element).
func standard(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}
