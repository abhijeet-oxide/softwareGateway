package render_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
	"github.com/abhijeet-oxide/softwareGateway/internal/compliance/render"
)

// helmOrSkip finds a usable helm, or skips.
//
// Skipping rather than failing is deliberate: the feature is specified to work
// without helm, so CI without it must still be green - and the degradation is
// itself tested, below, on every machine.
func helmOrSkip(t *testing.T) render.Helm {
	t.Helper()
	h := render.Helm{}.WithDefaults()
	if _, err := exec.LookPath(h.Binary); err != nil {
		t.Skip("helm is not on PATH; the absent-helm behaviour is covered by TestHelmAbsentIsNeverAPass")
	}
	if _, err := h.Version(context.Background()); err != nil {
		t.Skipf("helm is present but unusable: %v", err)
	}
	return h
}

const chart = "testdata/probe-chart"

func TestRenderIsDeterministic(t *testing.T) {
	h := helmOrSkip(t)
	ctx := context.Background()

	a, err := h.Render(ctx, chart)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	b, err := h.Render(ctx, chart)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if string(a.Manifests) != string(b.Manifests) {
		t.Error("two renders of the same chart differ; every finding derived from them is unreproducible")
	}
	// The source markers must survive. They are the only reliable way to
	// attribute an object to the template it came from.
	if !contains(string(a.Manifests), compliance.SourceMarker+"probe-chart/templates/deployment.yaml") {
		t.Error("the render lost helm's # Source: markers, so findings cannot name a file")
	}
}

func TestParseAttributesObjectsToTheirTemplates(t *testing.T) {
	h := helmOrSkip(t)
	out, err := h.Render(context.Background(), chart)
	if err != nil {
		t.Fatal(err)
	}
	resources, errs := compliance.ParseManifests(out.Manifests, compliance.Address{Chart: "probe-chart"})
	if len(errs) > 0 {
		t.Fatalf("parse: %v", errs)
	}
	if len(resources) != 2 {
		t.Fatalf("want 2 resources, got %d", len(resources))
	}
	for _, r := range resources {
		if r.Address.SourceFile == "" {
			t.Errorf("%s %s has no source file", r.Kind(), r.Name())
		}
		if r.Address.Kind != r.Kind() || r.Address.Name != r.Name() {
			t.Errorf("address does not match the object: %+v", r.Address)
		}
	}
}

// The mechanism that lets a tier-1 finding block without lying.
func TestDeterminacyDistinguishesFixedFromConfigurable(t *testing.T) {
	h := helmOrSkip(t)
	ctx := context.Background()

	values, err := render.ReadValues(chart)
	if err != nil {
		t.Fatal(err)
	}
	base, err := h.Render(ctx, chart)
	if err != nil {
		t.Fatal(err)
	}
	alt, ok := render.ProbeRender(ctx, h, chart, values)
	if !ok {
		t.Fatal("the probe render failed, so determinacy would be unknown for the whole chart")
	}

	addr := compliance.Address{Chart: "probe-chart"}
	baseRes, _ := compliance.ParseManifests(base.Manifests, addr)
	altRes, _ := compliance.ParseManifests(alt, addr)
	probe := render.NewProbe(baseRes, altRes, true)

	var deployment *compliance.Resource
	for i := range baseRes {
		if baseRes[i].Kind() == "Deployment" {
			deployment = &baseRes[i]
		}
	}
	if deployment == nil {
		t.Fatal("no Deployment in the render")
	}
	subj := compliance.Subject{Resource: deployment}

	cases := []struct {
		locus string
		want  compliance.Determinacy
		why   string
	}{
		{
			"spec.template.spec.containers[0].imagePullPolicy", compliance.DeterminacyFixed,
			"the template hard-codes it, so no values file can change it and the vendor must",
		},
		{
			"spec.template.spec.containers[0].securityContext.runAsUser", compliance.DeterminacyFixed,
			"runAsUser 0 is fixed by the template - the case that has to block",
		},
		{
			"spec.replicas", compliance.DeterminacyConfigurable,
			"replicas comes from values, so a finding on it is a question for the site",
		},
		{
			"spec.template.spec.containers[0].resources.limits.memory", compliance.DeterminacyConfigurable,
			"the memory limit comes from values",
		},
		{
			"spec.template.spec.containers[0].readinessProbe", compliance.DeterminacyFixed,
			"absent from both renders: the template never emits it, which is what a missing-field finding needs in order to block",
		},
	}
	for _, c := range cases {
		if got := probe.Determinacy(subj, c.locus); got != c.want {
			t.Errorf("%s: determinacy = %s, want %s (%s)", c.locus, got, c.want, c.why)
		}
	}
}

// A whole object whose existence is a values flag is configurable, not fixed.
// Perturbing only strings would leave every conditional taking the same branch
// and report this as fixed.
func TestObjectExistenceIsConfigurable(t *testing.T) {
	h := helmOrSkip(t)
	ctx := context.Background()
	values, err := render.ReadValues(chart)
	if err != nil {
		t.Fatal(err)
	}
	base, _ := h.Render(ctx, chart)
	alt, ok := render.ProbeRender(ctx, h, chart, values)
	if !ok {
		t.Fatal("probe render failed")
	}

	addr := compliance.Address{Chart: "probe-chart"}
	baseRes, _ := compliance.ParseManifests(base.Manifests, addr)
	altRes, _ := compliance.ParseManifests(alt, addr)

	var monitor *compliance.Resource
	for i := range baseRes {
		if baseRes[i].Kind() == "ServiceMonitor" {
			monitor = &baseRes[i]
		}
	}
	if monitor == nil {
		t.Fatal("the ServiceMonitor is not in the baseline render")
	}
	for _, r := range altRes {
		if r.Kind() == "ServiceMonitor" {
			t.Fatal("metrics.enabled was not flipped, so the probe is not exercising conditional blocks")
		}
	}

	probe := render.NewProbe(baseRes, altRes, true)
	if got := probe.Determinacy(compliance.Subject{Resource: monitor}, "spec.endpoints"); got != compliance.DeterminacyConfigurable {
		t.Errorf("determinacy = %s, want configurable: whether this object exists at all is a values flag", got)
	}
}

// A chart that was not probed costs ITS OWN findings their ownership and
// nothing else.
//
// # Why this is the test that matters most about the probe
//
// Determinacy used to be all-or-nothing per release: one chart whose second
// render did not happen made the whole run answer `unknown`. On a ninety-five
// chart orb one such chart is a certainty, so every finding read "Ownership not
// established" and the split between the vendor's defect and the site's
// decision - the first split anybody makes triaging a report - was on no screen
// anywhere.
//
// The guarantee that made it all-or-nothing is still enforced, and it is the
// other half of this test: an object in the baseline index and absent from the
// perturbed one reads as "its existence depends on values", so a chart must
// contribute to BOTH indexes or to NEITHER. Feeding one render of an unprobed
// chart would report every field of it as `configurable`, which invents an
// excuse for a real defect.
func TestUnprobedChartsCostOnlyTheirOwnDeterminacy(t *testing.T) {
	fixed := compliance.Resource{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "probed"},
		"spec":     map[string]any{"replicas": float64(3)},
	}}
	// Same object in both renders, so its field is the chart's own decision.
	probe := render.NewProbe(
		[]compliance.Resource{fixed},
		[]compliance.Resource{fixed},
		true,
	)
	if got := probe.Determinacy(compliance.Subject{Resource: &fixed}, "spec.replicas"); got != compliance.DeterminacyFixed {
		t.Errorf("a probed chart's field = %s, want fixed", got)
	}

	// A chart the run could not render twice is in NEITHER index. It must read
	// `unknown` - not `configurable`, which is what feeding only its baseline
	// render into the probe would produce, and which would excuse a real defect.
	unprobed := compliance.Resource{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "unprobed"},
		"spec":     map[string]any{"replicas": float64(1)},
	}}
	if got := probe.Determinacy(compliance.Subject{Resource: &unprobed}, "spec.replicas"); got != compliance.DeterminacyUnknown {
		t.Errorf("an unprobed chart's field = %s, want unknown", got)
	}
}

// Without a second render every answer is `unknown`. Guessing `fixed` invents
// vendor defects; guessing `configurable` excuses real ones.
func TestUnusableProbeSaysUnknown(t *testing.T) {
	p := render.Unusable()
	got := p.Determinacy(compliance.Subject{Resource: &compliance.Resource{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "x"},
	}}}, "spec.replicas")
	if got != compliance.DeterminacyUnknown {
		t.Errorf("determinacy = %s, want unknown", got)
	}
}

// A missing helm must produce a run that is inconclusive, never one that is
// green. A release whose charts were never rendered has not been shown to
// comply with anything.
func TestHelmAbsentIsNeverAPass(t *testing.T) {
	h := render.Helm{Binary: filepath.Join(t.TempDir(), "no-such-helm")}
	if _, err := h.Version(context.Background()); err == nil {
		t.Fatal("a missing helm reported a version")
	}
	// The verdict arithmetic is what actually protects the release: an error
	// result outranks a clean sweep of passes.
	v := compliance.Decide(compliance.Counts{Pass: 500, Error: 1})
	if v != compliance.VerdictInconclusive {
		t.Errorf("verdict with an undecidable check = %s, want inconclusive", v)
	}
}

// Perturbation has to change values without breaking the render, or the second
// render fails and determinacy is lost for the whole chart.
func TestPerturbationKeepsValuesParseable(t *testing.T) {
	cases := []struct{ in, want string }{
		{"512Mi", "513Mi"},
		{"250m", "251m"},
		{"30s", "31s"},
		// A version keeps its shape as a pre-release rather than becoming a
		// different version, so a chart that parses it still renders.
		{"1.0.0", "1.0.0-sgw-probe"},
		{"8080", "8081"},
		{"registry.acme.example/app", "registry.acme.example/app-sgw-probe"},
		{"", "sgw-probe-9f2c"},
	}
	for _, c := range cases {
		got, _ := render.PerturbValues(c.in).(string)
		if got != c.want {
			t.Errorf("PerturbValues(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// A boolean must flip: `{{- if .Values.metrics.enabled }}` is how a chart
	// decides whether an object exists at all.
	if got := render.PerturbValues(true); got != false {
		t.Errorf("PerturbValues(true) = %v, want false", got)
	}
	if got := render.PerturbValues(false); got != true {
		t.Errorf("PerturbValues(false) = %v, want true", got)
	}
	// Nesting is preserved, not flattened.
	nested := map[string]any{"a": map[string]any{"b": []any{"x", true}}}
	out, ok := render.PerturbValues(nested).(map[string]any)
	if !ok {
		t.Fatal("a map did not perturb to a map")
	}
	inner, _ := out["a"].(map[string]any)
	list, _ := inner["b"].([]any)
	if len(list) != 2 || list[0] != "x-sgw-probe" || list[1] != false {
		t.Errorf("nested perturbation = %#v", out)
	}
}

func TestReadChartMeta(t *testing.T) {
	meta, err := render.ReadChartMeta(chart)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Name != "probe-chart" || meta.Version != "1.0.0" || meta.AppVersion != "2.3.4" {
		t.Errorf("chart metadata = %+v", meta)
	}
	if !render.IsChart(chart) {
		t.Error("IsChart said no")
	}
	if render.IsChart("testdata") {
		t.Error("IsChart said yes for a directory with no Chart.yaml")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && stringIndex(haystack, needle) >= 0
}

func stringIndex(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

func writeFile(path string, body []byte) error { return os.WriteFile(path, body, 0o600) }

// THE INVARIANT THE WHOLE EVIDENCE FEATURE RESTS ON: a resource's RenderedLine
// is an offset into the document that was KEPT, not into some other rendering
// of the same chart.
//
// If these ever diverge - a document sliced per object, a stream normalised on
// the way to storage, a second render substituted for the first - every excerpt
// would point at a plausible-looking line of the wrong object, which is worse
// than showing nothing.
func TestKeptEvidenceIsWhatTheLineNumbersIndex(t *testing.T) {
	helmOrSkip(t)
	loader := render.Loader{Helm: render.Helm{}.WithDefaults(), HelmAvailable: true}

	rel, _, err := loader.Load(context.Background(), chart, compliance.Address{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rel.Rendered) != 1 {
		t.Fatalf("kept %d documents for one chart", len(rel.Rendered))
	}
	doc := rel.Rendered[0]
	if doc.Chart != "probe-chart" || doc.Truncated {
		t.Fatalf("document = %+v", doc)
	}

	lines := strings.Split(strings.TrimSuffix(string(doc.Content), "\n"), "\n")
	if doc.Lines != len(lines) || doc.Bytes != len(doc.Content) {
		t.Errorf("recorded %d lines / %d bytes, actually %d / %d",
			doc.Lines, doc.Bytes, len(lines), len(doc.Content))
	}

	for _, r := range rel.Resources {
		n := r.Address.RenderedLine
		if n <= 0 || n > len(lines) {
			t.Fatalf("%s %s is at line %d of a %d-line document",
				r.Kind(), r.Name(), n, len(lines))
		}
		// The recorded line is where the object's DOCUMENT starts, which is
		// helm's `# Source:` marker where there is one - and that is the right
		// anchor, because the marker names the template the object came from.
		// What must hold is that the object itself begins there and not
		// somewhere else: the first line of substance is its apiVersion.
		first := ""
		for _, l := range lines[n-1:] {
			t := strings.TrimSpace(l)
			if t == "" || t == "---" || strings.HasPrefix(t, "#") {
				continue
			}
			first = t
			break
		}
		if !strings.HasPrefix(first, "apiVersion:") {
			t.Errorf("%s %s claims line %d, where the document starts %q",
				r.Kind(), r.Name(), n, first)
		}
		// And a locus resolved against the kept text must land inside it.
		if at := compliance.LocusLine(doc.Content, n, "metadata.name"); at > 0 {
			if !strings.Contains(lines[at-1], r.Name()) {
				t.Errorf("%s %s: metadata.name resolved to %q", r.Kind(), r.Name(), lines[at-1])
			}
		} else {
			t.Errorf("%s %s: metadata.name did not resolve in the kept document", r.Kind(), r.Name())
		}
	}
}

// Over the budget the document is TRUNCATED and says so, rather than dropped:
// the findings in its first hundred lines are still shown, and the ones past
// the cut are told the text stops there.
func TestEvidenceBudgetTruncatesRatherThanDropping(t *testing.T) {
	helmOrSkip(t)
	loader := render.Loader{
		Helm: render.Helm{}.WithDefaults(), HelmAvailable: true,
		Evidence: render.EvidenceBudget{PerDocument: 200, PerRelease: 200},
	}
	rel, _, err := loader.Load(context.Background(), chart, compliance.Address{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rel.Rendered) != 1 {
		t.Fatalf("kept %d documents", len(rel.Rendered))
	}
	doc := rel.Rendered[0]
	if !doc.Truncated {
		t.Error("a document cut at the budget did not say so")
	}
	if doc.Bytes > 200 {
		t.Errorf("kept %d bytes against a 200-byte budget", doc.Bytes)
	}
	// Cut on a line boundary: a half-line looks like a malformed manifest and
	// would eventually be reported as a defect in the vendor's chart.
	if !strings.HasSuffix(string(doc.Content), "\n") {
		t.Errorf("the document was cut mid-line: %q", tail(string(doc.Content)))
	}
}

// A deployment that will not hold vendor manifests in its database turns the
// keeping off. Nothing else changes: the manifests are what a finding is
// DISPLAYED against, never what it is derived from.
func TestEvidenceCanBeTurnedOff(t *testing.T) {
	helmOrSkip(t)
	loader := render.Loader{
		Helm: render.Helm{}.WithDefaults(), HelmAvailable: true,
		Evidence: render.EvidenceBudget{PerDocument: -1},
	}
	rel, _, err := loader.Load(context.Background(), chart, compliance.Address{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rel.Rendered) != 0 {
		t.Fatalf("kept %d documents with evidence off", len(rel.Rendered))
	}
	if len(rel.Resources) == 0 {
		t.Fatal("turning evidence off lost the resources too")
	}
}

func tail(s string) string {
	if len(s) > 40 {
		return s[len(s)-40:]
	}
	return s
}
