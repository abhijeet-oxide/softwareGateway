package render_test

import (
	"context"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
	"github.com/abhijeet-oxide/softwareGateway/internal/compliance/baseline"
	celc "github.com/abhijeet-oxide/softwareGateway/internal/compliance/cel"
	"github.com/abhijeet-oxide/softwareGateway/internal/compliance/render"
)

// The whole path: a chart directory becomes a run.
//
// This is the test that would catch a break anywhere between helm's stdout and
// a finding a vendor reads - the parse, the addressing, the applicability, the
// determinacy join, and the verdict arithmetic. The unit tests each cover one
// link; only this one covers the chain.
func TestDirectoryToRun(t *testing.T) {
	h := helmOrSkip(t)
	ctx := context.Background()

	cat := shippedCatalog(t)
	version, _ := h.Version(ctx)
	loader := render.Loader{Helm: h, Probe: true, HelmAvailable: true, HelmVersion: version}

	rel, probe, err := loader.Load(ctx, chart, compliance.Address{Product: "probe", Release: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	rel.Config = map[string]any{"approvedRegistries": []any{"registry.acme.example"}}

	eng := &compliance.Engine{Catalog: cat, Determiner: probe}
	run, err := compliance.Execute(ctx, eng, rel, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}

	if run.Verdict != compliance.VerdictFail {
		t.Errorf("verdict = %s, want fail: this chart runs as root with no resource requests", run.Verdict)
	}
	if run.Counts.Blocking == 0 {
		t.Error("no blocking findings on a chart that runs as root")
	}
	if run.BundleDigest == "" || run.HelmVersion == "" || run.KubeVersion == "" {
		t.Error("the run does not record what produced it, so no finding in it is reproducible")
	}

	// The two determinacies must both appear, and on the right findings. This
	// is the distinction the whole probe exists for: one is the vendor's to
	// fix and one is a question for the site's values file.
	seen := map[string]compliance.Determinacy{}
	for _, r := range run.Results {
		if r.Outcome == compliance.OutcomeFail {
			seen[r.CheckID] = r.Determinacy
		}
	}
	if got := seen["SEC-01"]; got != compliance.DeterminacyFixed {
		t.Errorf("SEC-01 determinacy = %s, want fixed: runAsUser 0 is hard-coded in the template", got)
	}
	if got := seen["SUP-01"]; got != compliance.DeterminacyConfigurable {
		t.Errorf("SUP-01 determinacy = %s, want configurable: the image tag comes from values", got)
	}

	// Every finding must be addressed well enough to act on without this tool.
	for _, r := range run.Results {
		if r.Outcome != compliance.OutcomeFail {
			continue
		}
		if r.Address.Chart == "" || r.Address.SourceFile == "" || r.Address.Kind == "" || r.Address.Name == "" {
			t.Errorf("%s is not addressed to a resource a vendor can open: %+v", r.CheckID, r.Address)
		}
		if r.Message == "" {
			t.Errorf("%s has no message", r.CheckID)
		}
		if r.Remediation == "" {
			t.Errorf("%s tells a vendor they are wrong and not what to do", r.CheckID)
		}
	}
}

// Without helm the run is inconclusive, the charts are named, and nothing is a
// pass. This is the degradation the feature is specified to have, and it runs
// on every machine.
func TestWithoutHelmTheRunIsInconclusive(t *testing.T) {
	ctx := context.Background()
	cat := shippedCatalog(t)

	loader := render.Loader{
		Helm:          render.Helm{Binary: "definitely-not-helm"}.WithDefaults(),
		HelmAvailable: false,
	}
	rel, probe, err := loader.Load(ctx, chart, compliance.Address{Product: "probe", Release: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rel.Resources) != 0 {
		t.Fatalf("charts were rendered without helm: %d resources", len(rel.Resources))
	}
	if len(rel.Charts) != 1 || rel.Charts[0].RenderStatus != compliance.RenderSkipped {
		t.Fatalf("the chart's status does not record why it was not rendered: %+v", rel.Charts)
	}

	eng := &compliance.Engine{Catalog: cat, Determiner: probe}
	run, err := compliance.Execute(ctx, eng, rel, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}

	if run.Verdict != compliance.VerdictInconclusive {
		t.Fatalf("verdict = %s, want inconclusive; a release whose charts were never rendered "+
			"must never read as compliant", run.Verdict)
	}
	if run.Counts.Error != cat.Len() {
		t.Errorf("%d error results for %d checks: every check must say it could not be decided, "+
			"or the ones that vanish look like passes", run.Counts.Error, cat.Len())
	}
	if run.Counts.Pass != 0 {
		t.Errorf("%d checks passed against a chart that was never rendered", run.Counts.Pass)
	}
	for _, r := range run.Results {
		if r.Outcome == compliance.OutcomeError && r.Error == "" {
			t.Errorf("%s could not be decided and does not say why", r.CheckID)
		}
	}
}

func shippedCatalog(t *testing.T) *compliance.Catalog {
	t.Helper()
	comp, err := celc.NewCompiler()
	if err != nil {
		t.Fatal(err)
	}
	files, err := baseline.Files()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, body := range files {
		if err := writeFile(dir+"/"+name, body); err != nil {
			t.Fatal(err)
		}
	}
	cat, err := (&compliance.Loader{Compiler: comp}).Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range cat.Packs() {
		if !p.OK() {
			t.Fatalf("shipped pack %s did not load: %v", p.Name, p.Errors)
		}
	}
	return cat
}
