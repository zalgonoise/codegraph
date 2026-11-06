package options

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// KeyChecker is a type whose YAML form CheckKeys cannot walk by its fields
// alone (a pipeline step: a name, or a name with its settings).
type KeyChecker interface {
	CheckKeys(n *yaml.Node, warn func(n *yaml.Node, msg string))
}

// DecodeYAML decodes a YAML document into v through its json names, so a file
// uses the same keys as the JSON codegraph writes. A key v has no field for is
// a warning (with a suggestion when one is close), not an error: a file written
// for a newer codegraph still works. A value of the wrong type is an error.
func DecodeYAML(data []byte, file string, v any) (warnings []string, err error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if len(root.Content) == 0 {
		return nil, nil
	}
	doc := root.Content[0]
	CheckKeys(doc, reflect.TypeOf(v), func(n *yaml.Node, msg string) {
		warnings = append(warnings, fmt.Sprintf("%s:%d: %s", file, n.Line, msg))
	})
	var generic any
	if err := doc.Decode(&generic); err != nil {
		return warnings, fmt.Errorf("%s: %w", file, err)
	}
	js, err := json.Marshal(generic)
	if err != nil {
		return warnings, fmt.Errorf("%s: %w", file, err)
	}
	if err := json.Unmarshal(js, v); err != nil {
		return warnings, fmt.Errorf("%s: %w", file, err)
	}
	return warnings, nil
}

var keyChecker = reflect.TypeOf((*KeyChecker)(nil)).Elem()

// CheckKeys reports, through warn, every mapping key of n that type t has no
// field for, in nested structs, lists and maps too.
func CheckKeys(n *yaml.Node, t reflect.Type, warn func(n *yaml.Node, msg string)) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if reflect.PointerTo(t).Implements(keyChecker) {
		reflect.New(t).Interface().(KeyChecker).CheckKeys(n, warn)
		return
	}
	switch t.Kind() {
	case reflect.Struct:
		if n.Kind != yaml.MappingNode {
			return
		}
		fields := map[string]reflect.Type{}
		structFields(t, fields)
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			ft, ok := fields[key.Value]
			if !ok {
				warn(key, unknownKey(key.Value, fields))
				continue
			}
			CheckKeys(val, ft, warn)
		}
	case reflect.Slice:
		if n.Kind == yaml.SequenceNode {
			for _, e := range n.Content {
				CheckKeys(e, t.Elem(), warn)
			}
		}
	case reflect.Map:
		if n.Kind == yaml.MappingNode {
			for i := 1; i < len(n.Content); i += 2 {
				CheckKeys(n.Content[i], t.Elem(), warn)
			}
		}
	}
}

// structFields maps t's json names to their types, with the fields of
// embedded structs promoted as encoding/json does.
func structFields(t reflect.Type, out map[string]reflect.Type) {
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if f.Anonymous && tag == "" {
			structFields(f.Type, out)
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "-" || !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
}

// unknownKey words the warning for key, suggesting the closest known key.
func unknownKey(key string, fields map[string]reflect.Type) string {
	known := make([]string, 0, len(fields))
	for k := range fields {
		known = append(known, k)
	}
	sort.Strings(known)
	if s := Suggest(key, known); s != "" {
		return fmt.Sprintf("unknown key %q, ignored (did you mean %q?)", key, s)
	}
	return fmt.Sprintf("unknown key %q, ignored", key)
}

// Suggest returns the name of names closest to s: the same but for case,
// dashes and underscores (repo-rules for repoRules), else at most two edits
// away; "" when none is.
func Suggest(s string, names []string) string {
	norm := func(x string) string {
		return strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(x))
	}
	best, bestD := "", 3
	for _, n := range names {
		if norm(n) == norm(s) {
			return n
		}
		if d := distance(strings.ToLower(s), strings.ToLower(n)); d < bestD {
			best, bestD = n, d
		}
	}
	return best
}

// distance is the Levenshtein distance between a and b.
func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}
