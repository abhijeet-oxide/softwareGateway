package cel

import (
	"fmt"
	"strconv"
	"strings"
)

// Kubernetes semantics that every check needs and no check should implement.
//
// Each function here corresponds to a specific way the sixteen inherited
// policies were wrong, and each is unit-tested against the behaviour the API
// server actually has. They are exposed to expressions so a YAML pack can do
// cross-resource work without re-deriving any of it.

// ParseQuantity reads a Kubernetes resource quantity as a number of base units.
//
// # Why this is not strconv.ParseFloat
//
// "250m" is a quarter of a CPU, "1Gi" is 1073741824 bytes and "1G" is
// 1000000000, and the difference between the last two is 7% of a memory limit.
// A check comparing quantities as strings - or parsing them with ParseFloat and
// getting 1 for both - decides real limits wrongly, and does it silently.
//
// Binary suffixes (Ki, Mi, Gi, Ti, Pi, Ei) are powers of 1024. Decimal suffixes
// (n, u, m, k, M, G, T, P, E) are powers of 1000, with the lowercase k because
// that is what Kubernetes accepts and "K" is what it rejects.
func ParseQuantity(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty quantity")
	}
	// Scientific notation is legal and has no suffix: "1.5e3".
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		return v, nil
	}

	i := len(s)
	for i > 0 {
		c := s[i-1]
		if (c >= '0' && c <= '9') || c == '.' {
			break
		}
		i--
	}
	num, suffix := s[:i], s[i:]
	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a Kubernetes quantity", s)
	}

	mult, ok := quantitySuffixes[suffix]
	if !ok {
		return 0, fmt.Errorf("%q has an unrecognized quantity suffix %q", s, suffix)
	}
	return v * mult, nil
}

var quantitySuffixes = map[string]float64{
	"":  1,
	"n": 1e-9, "u": 1e-6, "m": 1e-3,
	"k": 1e3, "M": 1e6, "G": 1e9, "T": 1e12, "P": 1e15, "E": 1e18,
	"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30,
	"Ti": 1 << 40, "Pi": 1 << 50, "Ei": 1 << 60,
}

// ImageRef is an image reference taken apart.
type ImageRef struct {
	Registry   string
	Repository string
	Tag        string
	Digest     string
	// Original is what the manifest actually said, so a finding quotes the
	// reference the vendor wrote rather than a normalized form they will not
	// recognize.
	Original string
}

// HasDigest reports whether the reference pins content rather than a moving
// tag. This is the whole point of SUP-01: `:latest` and `:v2.1` both re-resolve
// later, and a release whose images are not pinned is not the release that was
// tested.
func (r ImageRef) HasDigest() bool { return r.Digest != "" }

// Map is the reference as an expression sees it.
func (r ImageRef) Map() map[string]any {
	return map[string]any{
		"registry":   r.Registry,
		"repository": r.Repository,
		"tag":        r.Tag,
		"digest":     r.Digest,
		"hasDigest":  r.HasDigest(),
		"original":   r.Original,
	}
}

// ParseImageRef splits an image reference into its parts.
//
// # Why the port is the hard case
//
// `registry.example.com:5000/team/app:v1` contains two colons, and the naive
// split-on-last-colon gives a tag of "v1" - correct - while split-on-first
// gives a registry of "registry.example.com" and a repository beginning
// "5000/". The rule that actually works is the one the OCI reference grammar
// uses: the registry is the first path segment only if it contains a dot, a
// colon, or is exactly "localhost". Otherwise there is no registry and the
// whole thing is a repository on Docker Hub.
//
// A check that gets this wrong reports every image from a port-bearing internal
// registry as coming from an unapproved one, which is every image in a
// disconnected environment.
func ParseImageRef(s string) ImageRef {
	ref := ImageRef{Original: s}
	rest := s

	// A digest is unambiguous: it is introduced by "@" and runs to the end.
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		ref.Digest = rest[at+1:]
		rest = rest[:at]
	}

	// Split the registry off the front, by the OCI rule.
	if slash := strings.Index(rest, "/"); slash >= 0 {
		head := rest[:slash]
		if strings.ContainsAny(head, ".:") || head == "localhost" {
			ref.Registry = head
			rest = rest[slash+1:]
		}
	}

	// What remains is repository[:tag]. A colon after the last slash is a tag;
	// one before it belonged to a registry port and is already gone.
	if colon := strings.LastIndex(rest, ":"); colon >= 0 && !strings.Contains(rest[colon:], "/") {
		ref.Tag = rest[colon+1:]
		rest = rest[:colon]
	}
	ref.Repository = rest
	return ref
}

// SelectorMatches implements Kubernetes label-selector semantics in full.
//
// # Why the full semantics and not matchLabels
//
// The inherited pdb.rego compares matchLabels only. A PodDisruptionBudget
// written as
//
//	selector:
//	  matchExpressions:
//	    - {key: app, operator: In, values: [etcd]}
//
// selects the etcd pods exactly as matchLabels would, and that policy sees an
// empty matchLabels, matches nothing, and reports the workload as uncovered -
// or, worse, reports nothing at all. Both spellings are ordinary, and a check
// that understands one of them is a check that is wrong about half the estate.
//
// # Why an empty selector matches everything
//
// Because that is what it means to Kubernetes: a PodDisruptionBudget with an
// empty selector covers every pod in its namespace. Callers that need the
// opposite - a Service with no selector selects nothing - decide that before
// calling, because the two objects genuinely differ.
func SelectorMatches(selector map[string]any, labels map[string]string) bool {
	if selector == nil {
		return false
	}
	matchLabelsRaw, _ := selector["matchLabels"].(map[string]any)
	exprs, _ := selector["matchExpressions"].([]any)

	if len(matchLabelsRaw) == 0 && len(exprs) == 0 {
		return true
	}

	for k, v := range matchLabelsRaw {
		want, ok := v.(string)
		if !ok {
			return false
		}
		if labels[k] != want {
			return false
		}
	}

	for _, e := range exprs {
		m, ok := e.(map[string]any)
		if !ok {
			return false
		}
		key, _ := m["key"].(string)
		op, _ := m["operator"].(string)
		var values []string
		if raw, ok := m["values"].([]any); ok {
			for _, v := range raw {
				if s, ok := v.(string); ok {
					values = append(values, s)
				}
			}
		}
		got, present := labels[key]

		switch op {
		case "In":
			if !present || !containsStr(values, got) {
				return false
			}
		case "NotIn":
			// An absent label satisfies NotIn: the pod is not in the set.
			if present && containsStr(values, got) {
				return false
			}
		case "Exists":
			if !present {
				return false
			}
		case "DoesNotExist":
			if present {
				return false
			}
		default:
			// An operator Kubernetes does not define. The selector is invalid,
			// and treating an invalid selector as matching would silently widen
			// whatever check called us.
			return false
		}
	}
	return true
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// CompareSemver orders two versions numerically, returning -1, 0 or 1.
//
// # Why not string comparison
//
// "1.10.0" sorts before "1.9.0" as a string. An upgrade check comparing
// operator versions that way is correct for the first nine minor releases and
// wrong from the tenth - which is to say, wrong exactly when a component has
// been maintained long enough for the check to matter.
//
// Leading "v" is tolerated because charts write both. Pre-release suffixes sort
// before the release they precede, per semver, and build metadata is ignored.
// A segment that is not a number compares as a string, so a version this does
// not understand still produces a stable order rather than a panic.
func CompareSemver(a, b string) int {
	aCore, aPre := splitPrerelease(normalizeVersion(a))
	bCore, bPre := splitPrerelease(normalizeVersion(b))

	an, bn := strings.Split(aCore, "."), strings.Split(bCore, ".")
	for i := 0; i < len(an) || i < len(bn); i++ {
		if c := compareSegment(at(an, i), at(bn, i)); c != 0 {
			return c
		}
	}
	switch {
	case aPre == "" && bPre == "":
		return 0
	case aPre == "":
		// A release outranks any pre-release of the same version.
		return 1
	case bPre == "":
		return -1
	}
	return strings.Compare(aPre, bPre)
}

func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i] // build metadata does not affect precedence
	}
	return v
}

func splitPrerelease(v string) (core, pre string) {
	if i := strings.IndexByte(v, '-'); i >= 0 {
		return v[:i], v[i+1:]
	}
	return v, ""
}

func at(parts []string, i int) string {
	if i < len(parts) {
		return parts[i]
	}
	return "0"
}

func compareSegment(a, b string) int {
	ai, aerr := strconv.Atoi(a)
	bi, berr := strconv.Atoi(b)
	if aerr == nil && berr == nil {
		switch {
		case ai < bi:
			return -1
		case ai > bi:
			return 1
		default:
			return 0
		}
	}
	return strings.Compare(a, b)
}
