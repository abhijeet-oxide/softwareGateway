package cel

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
)

// The declarative shorthand, compiled to CEL.
//
// # Why a shorthand exists at all
//
// Most of the baseline is "field X of kind Y must satisfy Z", and writing that
// a hundred times as an expression is a hundred chances to get absent-field
// handling wrong, and a hundred different spellings of the same requirement in
// the vendor's report. The shorthand gives one spelling, and it
// derives the observed value, the locus and the message from the form - which
// is how the report gets a useful "expected" column without every author
// remembering to write one.
//
// # Why it compiles to CEL rather than being interpreted separately
//
// Two evaluators means two semantics, and the day they disagree about whether
// `resources: {}` satisfies "resources.limits is required" is the day a check
// means one thing in the catalogue and another in the run. There is one
// evaluator. The shorthand is a source-to-source transform, and its output is
// visible in the API so an author can see exactly what their YAML became.

// compiledAssert is the CEL source the shorthand produced, plus what the engine
// needs to describe a failure.
type compiledAssert struct {
	// Source is the condition. True means compliant.
	Source string
	// Terms are the individual assertions, in declaration order, each with the
	// source that tests it. On failure the engine finds the first term that is
	// false, which is what lets a check with four required paths say which one
	// is missing rather than "required paths not satisfied".
	Terms []assertTerm
	// Expected is the human form of the whole requirement.
	Expected string
	// Locus is the field to report when no term is more specific.
	Locus string
}

type assertTerm struct {
	Source   string
	Locus    string
	Expected string
	// Observed is a CEL expression yielding the value that decided this term.
	Observed string
}

// buildAssert turns a declared assertion into CEL.
func buildAssert(a compliance.Assert) (compiledAssert, error) {
	var out compiledAssert

	for _, p := range sortedCopy(a.Required) {
		out.Terms = append(out.Terms, assertTerm{
			Source:   fmt.Sprintf("present(self, %s)", quote(p)),
			Locus:    p,
			Expected: "set to a non-empty value",
			Observed: fmt.Sprintf("present(self, %s) ? text(self, %s) : \"(absent)\"", quote(p), quote(p)),
		})
	}
	for _, p := range sortedCopy(a.Forbidden) {
		out.Terms = append(out.Terms, assertTerm{
			Source:   fmt.Sprintf("!present(self, %s)", quote(p)),
			Locus:    p,
			Expected: "not set",
			Observed: observedOr(p),
		})
	}
	for _, p := range sortedKeys(a.Equals) {
		lit, err := literal(a.Equals[p])
		if err != nil {
			return out, fmt.Errorf("equals[%s]: %w", p, err)
		}
		out.Terms = append(out.Terms, assertTerm{
			Source:   fmt.Sprintf("value(self, %s) == %s", quote(p), lit),
			Locus:    p,
			Expected: display(a.Equals[p]),
			Observed: observedOr(p),
		})
	}
	for _, p := range sortedKeys(a.OneOf) {
		vals := a.OneOf[p]
		lits := make([]string, 0, len(vals))
		shown := make([]string, 0, len(vals))
		for _, v := range vals {
			lit, err := literal(v)
			if err != nil {
				return out, fmt.Errorf("oneOf[%s]: %w", p, err)
			}
			lits = append(lits, lit)
			shown = append(shown, display(v))
		}
		if len(lits) == 0 {
			return out, fmt.Errorf("oneOf[%s] lists no values, so nothing can satisfy it", p)
		}
		out.Terms = append(out.Terms, assertTerm{
			Source:   fmt.Sprintf("[%s].exists(__v, __v == value(self, %s))", strings.Join(lits, ", "), quote(p)),
			Locus:    p,
			Expected: "one of " + strings.Join(shown, ", "),
			Observed: observedOr(p),
		})
	}
	for _, p := range sortedKeys(a.Matches) {
		out.Terms = append(out.Terms, assertTerm{
			Source:   fmt.Sprintf("text(self, %s).matches(%s)", quote(p), quote(a.Matches[p])),
			Locus:    p,
			Expected: "matching " + a.Matches[p],
			Observed: observedOr(p),
		})
	}
	for _, pair := range a.EqualPaths {
		// Absent on both sides is not equality here. Two paths that are both
		// missing are not "the same value" in any sense a vendor would accept -
		// a container with neither a request nor a limit has not pinned
		// anything, and reporting it compliant is the failure this form exists
		// to catch.
		out.Terms = append(out.Terms, assertTerm{
			Source: fmt.Sprintf("present(self, %s) && present(self, %s) && text(self, %s) == text(self, %s)",
				quote(pair.A), quote(pair.B), quote(pair.A), quote(pair.B)),
			Locus:    pair.A,
			Expected: fmt.Sprintf("equal to %s, and both set", pair.B),
			Observed: fmt.Sprintf("text(self, %s) + \" vs \" + text(self, %s)", quote(pair.A), quote(pair.B)),
		})
	}
	for _, p := range sortedKeys(a.Numeric) {
		b := a.Numeric[p]
		if b.Min == nil && b.Max == nil {
			return out, fmt.Errorf("numeric[%s] declares neither min nor max", p)
		}
		conds := []string{fmt.Sprintf("present(self, %s)", quote(p))}
		var want []string
		if b.Min != nil {
			conds = append(conds, fmt.Sprintf("quantity(text(self, %s)) >= %s", quote(p), formatFloat(*b.Min)))
			want = append(want, ">= "+formatFloat(*b.Min))
		}
		if b.Max != nil {
			conds = append(conds, fmt.Sprintf("quantity(text(self, %s)) <= %s", quote(p), formatFloat(*b.Max)))
			want = append(want, "<= "+formatFloat(*b.Max))
		}
		out.Terms = append(out.Terms, assertTerm{
			Source:   strings.Join(conds, " && "),
			Locus:    p,
			Expected: strings.Join(want, " and "),
			Observed: observedOr(p),
		})
	}
	if a.Expr != "" {
		out.Terms = append(out.Terms, assertTerm{
			Source:   "(" + a.Expr + ")",
			Locus:    a.Locus,
			Expected: a.Expected,
			Observed: a.Observed,
		})
	}

	if len(out.Terms) == 0 {
		return out, fmt.Errorf("assert declares nothing, so it would pass every subject it applies to")
	}

	parts := make([]string, 0, len(out.Terms))
	expected := make([]string, 0, len(out.Terms))
	for _, t := range out.Terms {
		parts = append(parts, "("+t.Source+")")
		if t.Expected != "" {
			e := t.Expected
			if t.Locus != "" {
				e = t.Locus + " " + e
			}
			expected = append(expected, e)
		}
	}
	// AND, never OR. A check whose parts are alternatives is two checks, and a
	// vendor can act on two checks.
	out.Source = strings.Join(parts, " && ")
	out.Expected = strings.Join(expected, "; ")
	if a.Expected != "" {
		out.Expected = a.Expected
	}
	out.Locus = a.Locus
	if out.Locus == "" && len(out.Terms) == 1 {
		out.Locus = out.Terms[0].Locus
	}
	return out, nil
}

// observedOr renders the value at a path, or says there is none.
//
// An empty string is what a missing field renders as, and "expected == false"
// with nothing before it does not tell a vendor whether the field is absent or
// set to something else. Those need different fixes.
func observedOr(path string) string {
	return fmt.Sprintf("present(self, %s) ? text(self, %s) : \"(absent)\"", quote(path), quote(path))
}

// quote renders a Go string as a CEL string literal.
//
// The escaping is deliberate rather than delegated to %q: a path or a regexp
// written by an author reaches the compiler through this function, and a
// backslash surviving unescaped would turn a check's own data into a
// sub-expression. There is no way to inject an expression through a path.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// literal renders a YAML scalar as a CEL literal.
//
// Only scalars: a check comparing a whole map for equality is asserting
// something about a structure nobody can read from the report, and the form it
// wants is several checks or an expression.
func literal(v any) (string, error) {
	switch t := v.(type) {
	case nil:
		return "null", nil
	case string:
		return quote(t), nil
	case bool:
		return strconv.FormatBool(t), nil
	case int:
		return strconv.Itoa(t), nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case float64:
		return formatFloat(t), nil
	default:
		return "", fmt.Errorf("%T is not a scalar; compare a scalar field, or use expr", v)
	}
}

func display(v any) string {
	if s, ok := v.(string); ok {
		return strconv.Quote(s)
	}
	return fmt.Sprint(v)
}

// formatFloat writes a number CEL will read back as a double, so integer-valued
// bounds do not become integer literals and change the comparison's type.
func formatFloat(f float64) string {
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

// sortedCopy and sortedKeys make compilation deterministic.
//
// Map iteration order is random in Go, and the generated source is visible in
// the API and hashed into the bundle digest. A digest that changed on every
// restart would make "which rulebook produced this report" unanswerable, which
// is Rule 5 lost to a detail.
func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
