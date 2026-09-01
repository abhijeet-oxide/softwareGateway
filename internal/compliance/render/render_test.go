package render_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
