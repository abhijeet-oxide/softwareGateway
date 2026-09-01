package render

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"

	"sigs.k8s.io/yaml"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
)

// Determinacy by differential rendering.
//
// # The problem this solves
//
// Tier-1 checking judges charts without the site's values file. A finding on a
// value the operator will override anyway is noise, and enough of it is how a
// checking tool becomes something people close. But refusing to report anything
// overridable would leave almost nothing to report, because a Helm chart's
// whole purpose is that its values are overridable.
//
// # The mechanism
//
// Render the chart twice: once with its own values, once with every scalar in
// those values perturbed. Then compare the two renders at the exact field a
// finding is about.
//
//	the value moved      the template took it from values     configurable
//	the value did not    the template fixes it                fixed
//	the object vanished  its existence depends on values      configurable
//	we could not tell    say so                               unknown
//
// A `fixed` failure is the vendor's to fix and can block. A `configurable` one
// is a question for whoever writes the site values. Reporting them
// identically - which is all a tool without this can do - is what makes tier-1
// checking either useless or dishonest.
//
// # Why perturbation and not static template analysis
//
// Deciding which template expressions reach which output field requires
// evaluating Go templates with sprig, which is the thing helm already does. The
// probe uses the real renderer, so it is right about `default`, about nested
// `with` blocks, and about a value that reaches output through three helpers -
// none of which a static analysis would follow.

// Sentinels chosen to be recognisable in output and unlikely to collide with a
// real value. They are never compared against; what matters is only whether the
// rendered output changed.
const (
	sentinelString = "sgw-probe-9f2c"
	sentinelSuffix = "-sgw-probe"
)

// Probe holds two renders of one release and answers determinacy questions
// about them.
type Probe struct {
	baseline  map[string]map[string]any
	perturbed map[string]map[string]any
	// usable is false when the second render could not be produced. Every
	// answer is then `unknown`, which is the honest one: guessing `fixed`
	// invents vendor defects and guessing `configurable` excuses real ones.
	usable bool
}

var _ compliance.Determiner = (*Probe)(nil)

// NewProbe builds a probe from a baseline resource set and a perturbed one.
func NewProbe(baseline, perturbed []compliance.Resource, usable bool) *Probe {
	return &Probe{
		baseline:  indexByIdentity(baseline),
		perturbed: indexByIdentity(perturbed),
		usable:    usable,
	}
}

// Unusable returns a probe that answers `unknown` to everything - what a run
// with no second render is honestly able to say.
func Unusable() *Probe { return &Probe{usable: false} }

// Determinacy reports how firmly the value at locus is established.
func (p *Probe) Determinacy(subj compliance.Subject, locus string) compliance.Determinacy {
	if p == nil || !p.usable {
		return compliance.DeterminacyUnknown
	}
	if subj.Resource == nil {
		return compliance.DeterminacyNA
	}
	key := identityOf(subj.Resource)

	base, ok := p.baseline[key]
	if !ok {
		return compliance.DeterminacyUnknown
	}
	alt, ok := p.perturbed[key]
	if !ok {
		// The object is in one render and not the other, so whether it exists
		// at all depends on values. Nothing about it can be called fixed.
		return compliance.DeterminacyConfigurable
	}
	if locus == "" {
		return compliance.DeterminacyUnknown
	}

	baseVal, baseFound := compliance.Lookup(base, locus)
	altVal, altFound := compliance.Lookup(alt, locus)

	switch {
	case baseFound != altFound:
		// Present in one render and absent in the other: the values file
		// decides whether the field is set at all.
		return compliance.DeterminacyConfigurable
	case !baseFound && !altFound:
		// Absent from both. Perturbing the chart's own values did not make the
		// template emit it, so the template never emits it - which is exactly
		// what a "required field is missing" finding needs in order to block.
		return compliance.DeterminacyFixed
	case reflect.DeepEqual(baseVal, altVal):
		return compliance.DeterminacyFixed
	default:
		return compliance.DeterminacyConfigurable
	}
}

// indexByIdentity keys resources the way the two renders can be aligned.
//
// Namespace is deliberately excluded from the key: a chart that templates
// .Release.Namespace into metadata.namespace would produce a different
// namespace in the perturbed render, and the two copies of one object would
// stop being comparable - reporting every field as configurable.
func indexByIdentity(rs []compliance.Resource) map[string]map[string]any {
	out := make(map[string]map[string]any, len(rs))
	for i := range rs {
		out[identityOf(&rs[i])] = rs[i].Object
	}
	return out
}

func identityOf(r *compliance.Resource) string {
	return r.APIVersion() + "|" + r.Kind() + "|" + r.Name()
}

// PerturbValues returns a copy of a chart's values with every scalar changed.
//
// # Why every type is perturbed differently
//
// A string becomes a sentinel, a number changes, and a BOOLEAN FLIPS - the last
// being the one that matters most, because `{{- if .Values.metrics.enabled }}`
// is how a chart decides whether an object exists at all. Perturbing only
// strings would leave every conditional block taking the same branch, and the
// probe would report "fixed" for a whole object whose existence is a values
// flag.
//
// Nulls become the sentinel for the same reason: a null value with a `default`
// helper behind it is a value the operator can supply.
func PerturbValues(v any) any {
	switch t := v.(type) {
	case nil:
		return sentinelString
	case string:
		// Keep it a plausible string: a chart that parses a value as a
		// duration or a URL would fail to render on a sentinel that is not
		// one, and a failed second render turns every answer into `unknown`.
		return perturbString(t)
	case bool:
		return !t
	case float64:
		return t + 1
	case int:
		return t + 1
	case int64:
		return t + 1
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = PerturbValues(t[i])
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = PerturbValues(val)
		}
		return out
	default:
		return v
	}
}

// perturbString changes a string while keeping its shape, so a chart that
// parses its own values still renders.
//
// The shapes that matter are the ones charts routinely parse: a quantity, a
// duration, an image reference, a URL, a number in a string. Producing garbage
// for these would break the second render, and a broken second render costs
// determinacy for the whole chart rather than for one field.
func perturbString(s string) string {
	if s == "" {
		return sentinelString
	}
	// A pure number in a string: keep it numeric.
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return strconv.FormatFloat(n+1, 'f', -1, 64)
	}
	// A quantity, a duration, a port - anything ending in units. Change the
	// leading number and keep the suffix.
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	if i > 0 {
		if n, err := strconv.ParseFloat(s[:i], 64); err == nil {
			return strconv.FormatFloat(n+1, 'f', -1, 64) + s[i:]
		}
	}
	// Anything else: append rather than replace, so a URL stays a URL, an
	// image reference stays parseable, and a boolean-ish string stays itself.
	return s + sentinelSuffix
}

// WritePerturbedValues renders a perturbed copy of a chart's values into a file
// helm can be pointed at.
func WritePerturbedValues(dir string, values map[string]any) (string, error) {
	perturbed, _ := PerturbValues(values).(map[string]any)
	b, err := yaml.Marshal(perturbed)
	if err != nil {
		return "", fmt.Errorf("serializing probe values: %w", err)
	}
	path := filepath.Join(dir, "sgw-probe-values.yaml")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return "", fmt.Errorf("writing probe values: %w", err)
	}
	return path, nil
}

// ProbeRender produces the second render for a chart.
//
// A failure is not an error the caller has to handle: it means determinacy is
// unknown for that chart, which is a legitimate answer and is reported as one.
// The alternative - failing the whole run because a perturbed render did not
// work - would let one chart's oddity remove every finding in the release.
func ProbeRender(ctx context.Context, h Helm, chartDir string, values map[string]any) ([]byte, bool) {
	tmp, err := os.MkdirTemp("", "sgw-probe-")
	if err != nil {
		return nil, false
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	path, err := WritePerturbedValues(tmp, values)
	if err != nil {
		return nil, false
	}
	out, err := h.Render(ctx, chartDir, path)
	if err != nil {
		return nil, false
	}
	return out.Manifests, true
}
