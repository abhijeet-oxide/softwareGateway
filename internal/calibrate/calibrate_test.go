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

	samples, chosen, err := collectSamples(t.Context(), cfg, nil)
	if err != nil {
		t.Fatalf("collect samples: %v", err)
	}
	if chosen != "vendor/platform" {
		t.Errorf("sampled %q, want vendor/platform", chosen)
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

// A REPOSITORY OF SMALL BLOBS IS A POOR SAMPLE AND A USABLE ONE.
//
// Refusing to measure it - which is what this used to do - leaves somebody
// looking at "no blob of at least 256 KiB" for a repository they can see is
// full of layers, with no way to act on a threshold they cannot see. Measure
// it, and let the report carry the caveat.
func TestATinyRepositoryIsMeasuredRatherThanRefused(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)
	reg.AddImage("vendor/tiny", "1.0.0", fakeregistry.NewLayer("small"))

	samples, chosen, err := collectSamples(t.Context(), plainConfig(reg.Host(), "vendor/tiny"), nil)
	if err != nil {
		t.Fatalf("a repository with blobs in it was refused: %v", err)
	}
	if chosen != "vendor/tiny" || len(samples) == 0 {
		t.Fatalf("samples = %d from %q", len(samples), chosen)
	}
	// And it is under the floor, which is what makes the caveat necessary.
	if samples[0].size >= minSampleBytes {
		t.Fatal("the fixture is not small; the test would prove nothing")
	}
}

// A repository with real content anywhere beats one with only stubs, even
// though both are measurable.
func TestBigBlobsWinOverSmallOnes(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)
	reg.AddImage("vendor/tiny", "1.0.0", fakeregistry.NewLayer("small"))
	reg.AddImage("vendor/real", "1.0.0",
		fakeregistry.NewLayer(strings.Repeat("r", 500<<10)))

	_, chosen, err := collectSamples(t.Context(), plainConfig(reg.Host(), "vendor/tiny"),
		[]string{"vendor/tiny", "vendor/real"})
	if err != nil {
		t.Fatal(err)
	}
	if chosen != "vendor/real" {
		t.Errorf("measured %q, want the repository over the size floor", chosen)
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

// A SOURCE SPANNING MANY REPOSITORIES MUST NOT BE JUDGED BY THE FIRST ONE.
//
// The first declared repository is as likely as not to be a stub. A real run
// measured `cfx-5000-product/aaa` - one tag, nothing over 256 KiB - and
// reported the entire source as unmeasurable while the repositories holding
// sixty gigabytes sat next to it in the same list.
func TestSamplingWalksPastAnEmptyRepository(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)

	// Exactly the shape that broke: an alphabetically-first repository with one
	// tiny tag, and the real content further down the list.
	reg.AddImage("cfx-5000-product/aaa", "0.0.1", fakeregistry.NewLayer("stub"))
	reg.AddImage("cfx-5000-product/admin", "25.7",
		fakeregistry.NewLayer(strings.Repeat("m", 600<<10)))

	cfg := plainConfig(reg.Host(), "cfx-5000-product/aaa")
	candidates := []string{"cfx-5000-product/aaa", "cfx-5000-product/admin"}

	samples, chosen, err := collectSamples(t.Context(), cfg, candidates)
	if err != nil {
		t.Fatalf("the search gave up on the first repository: %v", err)
	}
	if chosen != "cfx-5000-product/admin" {
		t.Errorf("measured %q, want the repository with real content", chosen)
	}
	if len(samples) == 0 {
		t.Error("no samples returned")
	}
}

// When there is genuinely nothing to read, the error names what was tried and
// what to do about it - rather than one repository nobody chose.
func TestSamplingSaysWhatItTriedWhenItFindsNothing(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)

	_, _, err := collectSamples(t.Context(), plainConfig(reg.Host(), "a"),
		[]string{"empty/one", "empty/two"})
	if err == nil {
		t.Fatal("a source with no content at all was accepted as measurable")
	}
	if !strings.Contains(err.Error(), "2 repositories") {
		t.Errorf("error = %q, want it to say how many were tried", err)
	}
	if !strings.Contains(err.Error(), "--source-repository") {
		t.Errorf("error = %q, want it to name the flag that fixes it", err)
	}
}

// THE SHAPE THAT BROKE IT: an index of indexes.
//
// A bundle's root lists its components, each component is a multi-platform
// index, and the layers live a level below THAT. A search that descends once
// lands on a component index, finds no layers because an index has none, and
// reports a repository holding gigabytes as having nothing worth measuring -
// which is exactly what happened against a real ORB, with and without
// --source-repository, while `packages inspect` listed every layer.
func TestBlobsAreFoundUnderAnIndexOfIndexes(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)

	// Level 3: the image that actually holds layers.
	image := reg.AddImage("orbs/bundle", "component-linux-amd64",
		fakeregistry.NewLayer(strings.Repeat("z", 700<<10)))
	// Level 2: a multi-platform index over it.
	component := reg.AddIndex("orbs/bundle", "component",
		[]string{image}, []string{"linux/amd64"})
	// Level 1: the bundle, listing the component index.
	reg.AddIndex("orbs/bundle", "orb_25.7", []string{component}, []string{""})

	// Only the bundle is TAGGED, which is the real shape: a vendor publishes
	// the release, not each manifest under it. Without this the search reaches
	// the layers by opening the leaf's own tag and never descends at all -
	// which is how the first version of this test passed against code that
	// could not descend.
	reg.RemoveTag("orbs/bundle", "component-linux-amd64")
	reg.RemoveTag("orbs/bundle", "component")

	samples, _, err := collectSamples(t.Context(), plainConfig(reg.Host(), "orbs/bundle"), nil)
	if err != nil {
		t.Fatalf("no blob found under a nested index: %v", err)
	}
	if samples[0].size < minSampleBytes {
		t.Errorf("largest sample is %d bytes; the 700 KiB layer two levels down was missed",
			samples[0].size)
	}
}

// Registries serve tags lexically, so the FIRST one is the oldest spelling and
// routinely the smallest. The newest is the better sample and the one a
// transfer is more likely to be moving.
func TestNewerTagsAreOpenedFirst(t *testing.T) {
	got := preferredTags([]string{"1.0.0", "25.6", "25.7"})
	if got[0] != "25.7" {
		t.Errorf("first tag opened = %q, want the last one listed", got[0])
	}
	if len(got) != 3 {
		t.Errorf("tags dropped: %v", got)
	}
}

// A SIGNATURE IS THE WORST POSSIBLE SAMPLE AND SORTS FIRST.
//
// `signature_orb_25.7` sorts after `orb_25.7`, and cosign's `sha256-….sig`
// sorts after everything - so opening newest-first walks straight into a
// handful of three-kilobyte PKCS#7 blobs and concludes the repository has
// nothing worth measuring. They go last, and are still opened, because a
// repository holding only signatures is still measurable.
func TestSignatureTagsGoToTheBackOfTheQueue(t *testing.T) {
	got := preferredTags([]string{
		"orb_25.6", "orb_25.7", "signature_orb_25.7", "signed_orb_25.7",
		"sha256-abc.sig", "sha256-abc.att",
	})

	if got[0] != "orb_25.7" {
		t.Errorf("first tag opened = %q, want the newest release", got[0])
	}
	for _, accessory := range []string{"signature_orb_25.7", "sha256-abc.sig"} {
		if position(got, accessory) < position(got, "orb_25.6") {
			t.Errorf("%q is opened before a release tag: %v", accessory, got)
		}
	}
	// Nothing is dropped: a repository of nothing but signatures still gets
	// measured, with the caveat that its blobs are tiny.
	if len(got) != 6 {
		t.Errorf("tags dropped: %v", got)
	}
}

func position(tags []string, want string) int {
	for i, t := range tags {
		if t == want {
			return i
		}
	}
	return len(tags)
}

// THE WRITE PROBE MUST GO WHERE A TRANSFER WRITES.
//
// A target's configured repository is a PREFIX. Opening an upload session
// against it comes back 404 from a registry that is working perfectly, which
// reads as a broken target rather than as a probe addressing a path that is not
// an image repository.
func TestWriteProbeUsesAPathATransferWouldWrite(t *testing.T) {
	target := endpoint{
		basePath: "apm0014228-oci-stage",
		cfg:      registry.ClientConfig{Repository: "apm0014228-oci-stage"},
	}

	got := writeProbePath(target, "cfx-5000-product/admin")
	if want := "apm0014228-oci-stage/cfx-5000-product/admin"; got != want {
		t.Errorf("write probe path = %q, want %q", got, want)
	}

	// A target with no prefix mirrors the source path at the registry root,
	// which is what the planner does with an empty base.
	rootTarget := endpoint{cfg: registry.ClientConfig{Repository: ""}}
	if got := writeProbePath(rootTarget, "orbs/thing"); got != "orbs/thing" {
		t.Errorf("write probe path = %q, want the mirrored source path", got)
	}
}

// A LEVEL THAT MEASURED NOTHING MUST SAY SO.
//
// A row of dashes with no explanation anywhere is what a five-second budget
// produced against a path with a 900ms round trip: the first request of every
// level was still in flight when the level ended, the deadline path returned
// silently, and the whole table read as a broken probe rather than as a budget
// too short for the link.
func TestALevelThatFinishesNothingExplainsItself(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)
	reg.AddImage("vendor/platform", "1.0.0",
		fakeregistry.NewLayer(strings.Repeat("a", 400<<10)))

	cfg := plainConfig(reg.Host(), "vendor/platform")
	samples, _, err := collectSamples(t.Context(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	// A budget no request can fit in, whatever the link.
	res := probeRead(t.Context(), withConnections(cfg, 1), samples, 1, time.Nanosecond)
	if res.Bytes != 0 || res.Requests != 0 {
		t.Skip("the fake answered inside a nanosecond; nothing to assert")
	}
	if res.FirstError == "" {
		t.Fatal("an empty level reported no reason at all")
	}
	if !strings.Contains(res.FirstError, "--budget") {
		t.Errorf("reason %q does not name the flag that fixes it", res.FirstError)
	}
}

// The write half has the same failure and the same fix.
func TestAnEmptyWriteLevelExplainsItself(t *testing.T) {
	reg := fakeregistry.New()
	t.Cleanup(reg.Close)

	res := probeWrite(t.Context(), withConnections(plainConfig(reg.Host(), "dest/lab"), 1),
		1, time.Nanosecond)
	if res.Requests != 0 {
		t.Skip("the fake accepted a chunk inside a nanosecond")
	}
	if res.FirstError == "" {
		t.Error("an empty write level reported no reason at all")
	}
	// And the session it opened for the measurement is still cancelled.
	if reg.CancelledUploads.Load() == 0 {
		t.Error("a level that measured nothing left its upload session open")
	}
}

// THE BUDGET FOLLOWS THE LINK.
//
// A level has to outlast a few round trips or it measures handshakes. Against a
// registry 900ms away the five-second default is five round trips, which is how
// a whole sweep came back empty.
func TestTheBudgetGrowsWithTheRoundTrip(t *testing.T) {
	base := Options{Budget: 5 * time.Second}

	// A fast path is left alone: it settles its answer in the first second and
	// should not be made to wait.
	got, notes := withBudgetFor(base, 2*time.Millisecond, "source", nil)
	if got.Budget != base.Budget {
		t.Errorf("budget = %s on a 2ms link, want it unchanged", got.Budget)
	}
	if len(notes) != 0 {
		t.Errorf("a note was added for a fast link: %v", notes)
	}

	// A 900ms link wants nine seconds and gets them.
	got, notes = withBudgetFor(base, 900*time.Millisecond, "source", nil)
	if got.Budget != 9*time.Second {
		t.Errorf("budget = %s on a 900ms link, want 9s", got.Budget)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "round trip") {
		t.Errorf("notes = %v, want one explaining the change", notes)
	}

	// And it is capped, because a client is waiting on a timeout derived from
	// the budget it asked for.
	got, _ = withBudgetFor(base, 10*time.Second, "source", nil)
	if got.Budget != 15*time.Second {
		t.Errorf("budget = %s on a very slow link, want it capped at 3x", got.Budget)
	}
}
