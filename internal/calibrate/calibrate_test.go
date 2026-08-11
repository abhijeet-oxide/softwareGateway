package calibrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/test/fakeregistry"
)

// What a calibration must be true about, in order of how much it would cost to
// be wrong: it must not damage the target, it must measure something real, and
// its advice must follow from what it measured.

// THE PROBE MUST LEAVE THE TARGET EXACTLY AS IT FOUND IT.
//
// This is the property the whole write probe is designed around, and the only
// one whose violation would be somebody else's production registry full of
// junk. Bytes go up the wire; nothing is committed; the session is cancelled.
func TestWriteProbeMovesRealBytesAndCommitsNothing(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)

	cfg := plainConfig(reg.Host(), "dest/lab")
	if err := checkUploadSupported(t.Context(), cfg); err != nil {
		t.Fatalf("upload session not supported by the fake: %v", err)
	}

	res := probeWrite(t.Context(), withConnections(cfg, 2), 2, 500*time.Millisecond)

	if res.Bytes <= 0 {
		t.Fatalf("no bytes pushed: %+v", res)
	}
	if res.Rate <= 0 {
		t.Errorf("rate = %v, want a positive throughput", res.Rate)
	}
	if got := reg.UploadedBlobs.Load(); got != 0 {
		t.Errorf("%d blob(s) were COMMITTED to the target; the probe must commit none", got)
	}
	if got := reg.CancelledUploads.Load(); got == 0 {
		t.Error("no upload session was cancelled, so the probe left sessions open")
	}
}

// A cancelled session is cancelled even when the budget expires mid-chunk,
// which is the normal way a level ends.
func TestWriteProbeCancelsAfterItsDeadline(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)

	cfg := plainConfig(reg.Host(), "dest/lab")
	probeWrite(t.Context(), withConnections(cfg, 3), 3, 200*time.Millisecond)

	if got := reg.CancelledUploads.Load(); got < 3 {
		t.Errorf("%d session(s) cancelled for 3 streams; every stream must clean up after "+
			"itself even when the deadline is what stopped it", got)
	}
}

// The read probe must read the SOURCE's real content, and must scale its work
// with the level it is given.
func TestReadProbeReadsRealBlobs(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)
	reg.AddImage("vendor/platform", "1.0.0",
		fakeregistry.NewLayer(strings.Repeat("a", 400<<10)),
		fakeregistry.NewLayer(strings.Repeat("b", 400<<10)))

	cfg := plainConfig(reg.Host(), "vendor/platform")

	samples, err := collectSamples(t.Context(), cfg, "")
	if err != nil {
		t.Fatalf("collect samples: %v", err)
	}
	if len(samples) < 2 {
		t.Fatalf("found %d sample(s); both layers are over the minimum size", len(samples))
	}

	res := probeRead(t.Context(), withConnections(cfg, 2), samples, 2, 300*time.Millisecond)
	if res.Bytes <= 0 || res.Requests == 0 {
		t.Fatalf("read probe moved nothing: %+v", res)
	}
	if res.Errors > 0 {
		t.Errorf("%d error(s) against a healthy registry: %s", res.Errors, res.FirstError)
	}
	if reg.ServedBlobs.Load() == 0 {
		t.Error("the registry served no blob, so the probe measured something else")
	}
}

// A config blob is a couple of hundred bytes and measures the round trip, not
// the bandwidth. Sampling one would make every calibration of a small
// repository report a throughput a hundred times below the truth.
func TestSamplesSkipBlobsTooSmallToMeasure(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)
	reg.AddImage("vendor/tiny", "1.0.0", fakeregistry.NewLayer("small"))

	_, err := collectSamples(t.Context(), plainConfig(reg.Host(), "vendor/tiny"), "")
	if err == nil {
		t.Fatal("a repository of tiny blobs was accepted as a throughput sample")
	}
	if !strings.Contains(err.Error(), "no blob of at least") {
		t.Errorf("error = %q, want it to name the size floor", err)
	}
}

// End to end: real configuration, real registries, a report with advice in it.
func TestRunProducesAReportWithEvidence(t *testing.T) {
	src := fakeregistry.New()
	t.Cleanup(src.Close)
	src.AddImage("vendor/platform", "1.0.0",
		fakeregistry.NewLayer(strings.Repeat("x", 512<<10)),
		fakeregistry.NewLayer(strings.Repeat("y", 512<<10)))

	dst := fakeregistry.New()
	t.Cleanup(dst.Close)

	p := loadProduct(t, `
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata:
  name: test-product
spec:
  sources:
    - name: vendor
      registry: `+src.Host()+`
      repository: vendor/platform
      anonymous: true
  targets:
    - name: lab
      registry: `+dst.Host()+`
      repository: dest/lab
      anonymous: true
      default: true
`)

	rep, err := NewCalibrator(product.NewSecretResolver(t.TempDir())).
		Run(t.Context(), p, Options{
			Levels: []int{1, 2},
			Budget: 300 * time.Millisecond,
			Write:  true,
		})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if !rep.Source.Measured() {
		t.Fatalf("source not measured: %s", rep.Source.Skipped)
	}
	if !rep.Target.Measured() {
		t.Fatalf("target not measured: %s", rep.Target.Skipped)
	}
	if rep.Source.Knee == 0 {
		t.Error("no knee identified, so there is nothing to configure")
	}
	if dst.UploadedBlobs.Load() != 0 {
		t.Errorf("the run committed %d blob(s) to the target", dst.UploadedBlobs.Load())
	}

	// EVERY suggestion carries its measurement. Advice without a number is the
	// guesswork this package replaces, and a list that quietly grew one is a
	// regression worth failing for.
	if len(rep.Suggestions) == 0 {
		t.Fatal("a completed run produced no suggestions at all")
	}
	for _, s := range rep.Suggestions {
		if strings.TrimSpace(s.Evidence) == "" {
			t.Errorf("suggestion %q (%s) has no evidence behind it", s.Setting, s.Scope)
		}
		if s.Suggested == "" {
			t.Errorf("suggestion %q says nothing to do", s.Setting)
		}
	}

	// And the report says where it was measured, because that is the caveat
	// that decides whether any of it applies.
	if rep.MeasuredFrom == "" {
		t.Error("the report does not say which host measured it")
	}
}

// A source the product does not have is a mistake to report, not a default to
// silently substitute.
func TestRunRefusesASourceTheProductDoesNotHave(t *testing.T) {
	p := loadProduct(t, `
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata:
  name: test-product
spec:
  sources:
    - name: vendor
      registry: example.invalid
      repository: vendor/platform
      anonymous: true
  targets:
    - name: lab
      registry: example.invalid
      repository: dest/lab
      anonymous: true
`)

	_, err := NewCalibrator(product.NewSecretResolver(t.TempDir())).
		Run(t.Context(), p, Options{Source: "typo", Budget: time.Second})
	if err == nil {
		t.Fatal("an unknown source was accepted")
	}
	if !strings.Contains(err.Error(), "vendor") {
		t.Errorf("error = %q, want it to name the sources that DO exist", err)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func plainConfig(host, repository string) registry.ClientConfig {
	return registry.ClientConfig{
		Registry:       host,
		Repository:     repository,
		PlainHTTP:      true,
		MaxConnections: 4,
		ConnectTimeout: 2 * time.Second,
	}
}

func loadProduct(t *testing.T, doc string) *product.Product {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.yaml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := product.NewLoader(dir, product.NewSecretResolver(t.TempDir())).Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(res.Invalid) > 0 {
		t.Fatalf("document invalid: %v", res.Invalid[0].Err)
	}
	return res.Valid[0]
}
