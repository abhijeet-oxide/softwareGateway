package compliance

import (
	"strconv"
	"strings"
)

// Path lookup over decoded YAML.
//
// # Why paths are strings and not a typed accessor
//
// A check is data. "resources.limits.memory" is written in a YAML manifest by
// somebody who is reading a Kubernetes reference page, and it has to mean there
// what it means there. The alternative - a typed accessor per kind - cannot
// address a custom resource at all, and a release ships plenty of those.
//
// # Why absence is distinguished from empty
//
// Lookup returns a found flag rather than a zero value. `replicas: 0` and no
// replicas field are different facts, and a check that cannot tell them apart
// either reports a deliberate scale-to-zero as a defect or misses a missing
// field. Every caller here is required to handle both, which is Rule 2 applied
// to field access.

// Lookup resolves a dotted path against a decoded document.
//
// Segments are map keys, except that a segment of the form name[3] indexes a
// list, and a bare integer segment does too. Keys containing dots - which
// annotations and label keys routinely do - are written in brackets:
// metadata.annotations[acme.example/quorum-size].
func Lookup(doc any, path string) (any, bool) {
	cur := doc
	for _, seg := range splitPath(path) {
		if seg == "" {
			continue
		}
		next, ok := step(cur, seg)
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

func step(cur any, seg string) (any, bool) {
	// A bracketed suffix is an index, or a quoted key.
	if i := strings.IndexByte(seg, '['); i >= 0 && strings.HasSuffix(seg, "]") {
		inner := seg[i+1 : len(seg)-1]
		base := seg[:i]
		if base != "" {
			v, ok := step(cur, base)
			if !ok {
				return nil, false
			}
			cur = v
		}
		if n, err := strconv.Atoi(inner); err == nil {
			list, ok := cur.([]any)
			if !ok || n < 0 || n >= len(list) {
				return nil, false
			}
			return list[n], true
		}
		return mapKey(cur, strings.Trim(inner, `"'`))
	}
	if n, err := strconv.Atoi(seg); err == nil {
		if list, ok := cur.([]any); ok {
			if n < 0 || n >= len(list) {
				return nil, false
			}
			return list[n], true
		}
	}
	return mapKey(cur, seg)
}

func mapKey(cur any, key string) (any, bool) {
	m, ok := cur.(map[string]any)
	if !ok {
		return nil, false
	}
	v, ok := m[key]
	return v, ok
}

// splitPath splits on dots that are not inside brackets, so a path may address
// an annotation key that contains dots.
func splitPath(path string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(path); i++ {
		switch path[i] {
		case '[':
			depth++
		case ']':
			if depth > 0 {
				depth--
			}
		case '.':
			if depth == 0 {
				out = append(out, path[start:i])
				start = i + 1
			}
		}
	}
	return append(out, path[start:])
}

// Present reports whether a path resolves to something a person would call set.
//
// nil is absent; an empty string, empty list and empty map are absent. This is
// what "required" means to somebody writing a check: `resources: {}` does not
// satisfy "resources.limits is required", and a check that accepted it would
// pass every chart that declares the field and fills in nothing - which is the
// most common way a vendor "supports" a requirement.
func Present(doc any, path string) bool {
	v, ok := Lookup(doc, path)
	if !ok || v == nil {
		return false
	}
	switch t := v.(type) {
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

// helpers used across the package for reading loosely-typed documents.

func mapAt(doc map[string]any, path ...string) (map[string]any, bool) {
	var cur any = doc
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[p]
		if !ok {
			return nil, false
		}
	}
	m, ok := cur.(map[string]any)
	return m, ok
}

func stringAt(doc map[string]any, path ...string) string {
	var cur any = doc
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[p]
	}
	s, _ := cur.(string)
	return s
}

// stringMapAt reads a map of strings, dropping entries whose value is not a
// string. Labels and annotations are string-to-string in Kubernetes; a YAML
// document that put a bool there was already invalid, and coercing it would
// make this tool accept what the API server will not.
func stringMapAt(doc map[string]any, path ...string) map[string]string {
	m, ok := mapAt(doc, path...)
	if !ok {
		return map[string]string{}
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}
