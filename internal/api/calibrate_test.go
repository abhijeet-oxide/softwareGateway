package api

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/calibrate"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
	"github.com/abhijeet-oxide/softwareGateway/test/fakeregistry"
)

// The whole path, once: product configuration to resolved endpoints to real
// probes against real registries to a rendered report, over HTTP.
//
// The unit tests either side of this one are more precise about the arithmetic;
// this is the one that would catch a route that is registered but not wired, a
// DTO field that never gets populated, or a probe that cannot see the
// credentials the product declares.
func TestCalibrateEndToEndThroughTheAPI(t *testing.T) {
	src := fakeregistry.New()
	t.Cleanup(src.Close)
	src.AddImage("vendor/platform", "1.0.0",
		fakeregistry.NewLayer(strings.Repeat("x", 900<<10)),
		fakeregistry.NewLayer(strings.Repeat("y", 900<<10)))
	dst := fakeregistry.New()
	t.Cleanup(dst.Close)

	dir := t.TempDir()
	doc := `
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata:
  name: cfx-5000-product
spec:
  sources:
    - name: near
      registry: ` + src.Host() + `
      repository: vendor/platform
      anonymous: true
  targets:
    - name: jfrog-lab
      registry: ` + dst.Host() + `
      repository: apm-oci-stage
      anonymous: true
      default: true
`
	if err := os.WriteFile(filepath.Join(dir, "p.yaml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	secrets := product.NewSecretResolver(t.TempDir())
	res, err := product.NewLoader(dir, secrets).
		WithConcurrency(product.Concurrency{PerRegistry: 32}).Load()
	if err != nil || len(res.Invalid) > 0 {
		t.Fatalf("load: %v %v", err, res.Invalid)
	}
	reg := product.NewRegistry()
	reg.Swap(res)

	srv := NewServer(Deps{Products: reg, Calibrator: calibrate.NewCalibrator(secrets)})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	client := v1.NewClient(ts.URL)
	resp, err := client.Calibrate(t.Context(), "cfx-5000-product", v1.CalibrateRequest{
		Levels: []int{1, 2}, BudgetSeconds: 0.2,
		// 30 GiB, so the projection is exercised too.
		BundleBytes: "32212254720",
	})
	if err != nil {
		t.Fatalf("calibrate: %v", err)
	}

	if resp.Source.Skipped != "" {
		t.Fatalf("source not measured: %s", resp.Source.Skipped)
	}
	if resp.Target.Skipped != "" {
		t.Fatalf("target not measured: %s", resp.Target.Skipped)
	}
	if len(resp.Source.Levels) == 0 || resp.Source.Levels[0].Rate <= 0 {
		t.Errorf("source levels carry no rate: %+v", resp.Source.Levels)
	}
	if resp.Source.Knee == 0 || resp.Target.Knee == 0 {
		t.Errorf("knees not reported: source %d, target %d", resp.Source.Knee, resp.Target.Knee)
	}
	if resp.MeasuredFrom == "" {
		t.Error("the report does not say which host measured it")
	}
	if len(resp.Suggestions) == 0 {
		t.Fatal("no suggestions returned")
	}

	// The property that matters most, asserted at the far end of the whole
	// stack rather than only at the probe: a calibration must not put anything
	// in the target registry.
	if got := dst.UploadedBlobs.Load(); got != 0 {
		t.Errorf("%d blob(s) were committed to the target", got)
	}
	if dst.CancelledUploads.Load() == 0 {
		t.Error("no upload session was cancelled, so the probe left sessions open")
	}

	var projected bool
	for _, s := range resp.Suggestions {
		if strings.Contains(s.Suggested, "would take about") {
			projected = true
		}
		if strings.TrimSpace(s.Evidence) == "" {
			t.Errorf("suggestion %q carries no evidence", s.Setting)
		}
	}
	if !projected {
		t.Error("bundleBytes was given but no projection was made from it")
	}
}
