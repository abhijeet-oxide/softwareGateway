package source_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
	"github.com/abhijeet-oxide/softwareGateway/internal/compliance/source"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/vendors"
	"github.com/abhijeet-oxide/softwareGateway/internal/vendors/near"
)

// The bug this file exists for.
//
// A real NEAR orb of 95 Helm charts reported "this release ships no Helm
// charts". Its charts are plain OCI image manifests carrying an ordinary image
// config - media type, artifact type and config media type are IDENTICAL to its
// images - and the only evidence anywhere is the annotation the vendor wrote.
// An index's children do not even have the config media type recorded, because
// discovery reads what the index LISTED without fetching each child.
//
// Selecting charts by the Helm config media type therefore matched nothing, and
// the release's own page counted the charts correctly one tab away. This is the
// same failure that once reported an orb of 157 images and 97 charts as 254
// images, which is why the fix is to use the product's own classifier rather
// than to write a better query.

// orbCandidates is what ChartCandidates returns for a NEAR orb: every artifact
// looks identical on the OCI fields.
func orbCandidates() []store.ChartCandidate {
	const (
		manifest = "application/vnd.oci.image.manifest.v1+json"
		imgCfg   = "application/vnd.oci.image.config.v1+json"
	)
	return []store.ChartCandidate{
		{
			ArtifactID: 1, Digest: "sha256:c1", LayerDigest: "sha256:l1", LayerCount: 1,
			MediaType: manifest, ConfigMediaType: "", // an index child: not recorded
			Annotations: map[string]string{
				"com.nokia.ncd.orb.type":            "helmchart",
				"org.opencontainers.image.ref.name": "orbs/cfx/charts/amf",
			},
		},
		{
			ArtifactID: 2, Digest: "sha256:i1", LayerDigest: "sha256:l2", LayerCount: 8,
			MediaType: manifest, ConfigMediaType: imgCfg,
			Annotations: map[string]string{
				"com.nokia.ncd.orb.type":            "cnfimage",
				"org.opencontainers.image.ref.name": "orbs/cfx/images/amf",
			},
		},
		{
			ArtifactID: 3, Digest: "sha256:c2", LayerDigest: "sha256:l3", LayerCount: 1,
			MediaType: manifest, ConfigMediaType: imgCfg,
			Annotations: map[string]string{
				"com.nokia.ncd.orb.type": "helmchart",
			},
		},
	}
}

type stubLookup struct{ candidates []store.ChartCandidate }

func (s stubLookup) GetPackageByID(context.Context, int64) (store.PackageRow, error) {
	return store.PackageRow{Tag: "orb_24.7.3099"}, nil
}
func (s stubLookup) ChartCandidates(context.Context, int64) ([]store.ChartCandidate, error) {
	return s.candidates, nil
}

func nearClassifier(string) vendors.Classifier {
	reg := vendors.NewRegistry()
	near.Register(reg)
	return vendors.ClassifierFor(reg, []string{near.Name})
}

// The regression: a NEAR orb's charts must be found.
func TestNEAROrbChartsAreFound(t *testing.T) {
	p := &source.Preparer{
		Packages: stubLookup{candidates: orbCandidates()},
		Classify: nearClassifier,
		Fetcher:  source.Fetcher{Blobs: blobs{}},
	}

	// Prepare fails at the fetch, because the stub serves no blobs. What
	// matters is WHICH failure: reaching the fetch at all means the charts were
	// recognised, and ErrNoCharts means they were not.
	_, _, cleanup, err := p.Prepare(context.Background(),
		compliance.Request{Product: "cfx", Release: "orb_24.7.3099"},
		func(compliance.Stage, int, int, string) {})
	if cleanup != nil {
		cleanup()
	}
	if err != nil && strings.Contains(err.Error(), "ships no Helm charts") {
		t.Fatalf("a NEAR orb's charts were not recognised: %v", err)
	}
}

// The OCI-conformant shape must still work, and an image must still be
// excluded whichever way it is labelled.
func TestClassificationSelectsOnlyCharts(t *testing.T) {
	const (
		manifest = "application/vnd.oci.image.manifest.v1+json"
		helmCfg  = "application/vnd.cncf.helm.config.v1+json"
		imgCfg   = "application/vnd.oci.image.config.v1+json"
	)
	candidates := []store.ChartCandidate{
		// A conformant chart: config media type says so.
		{ArtifactID: 1, Digest: "sha256:a", LayerDigest: "sha256:la", LayerCount: 1,
			MediaType: manifest, ConfigMediaType: helmCfg},
		// An ordinary image.
		{ArtifactID: 2, Digest: "sha256:b", LayerDigest: "sha256:lb", LayerCount: 12,
			MediaType: manifest, ConfigMediaType: imgCfg},
	}

	p := &source.Preparer{
		Packages: stubLookup{candidates: candidates},
		Fetcher:  source.Fetcher{Blobs: blobs{}},
	}
	_, _, cleanup, err := p.Prepare(context.Background(),
		compliance.Request{Product: "conformant"},
		func(compliance.Stage, int, int, string) {})
	if cleanup != nil {
		cleanup()
	}
	if err != nil && strings.Contains(err.Error(), "ships no Helm charts") {
		t.Fatalf("a conformant chart was not recognised: %v", err)
	}
}

// A release of images only is a legitimate thing to say - and the message has
// to say how it got there, or the next time it is wrong nobody can tell.
func TestNoChartsSaysHowManyItLookedAt(t *testing.T) {
	const manifest = "application/vnd.oci.image.manifest.v1+json"
	candidates := []store.ChartCandidate{
		{ArtifactID: 1, Digest: "sha256:b", LayerDigest: "sha256:lb", LayerCount: 12,
			MediaType: manifest, ConfigMediaType: "application/vnd.oci.image.config.v1+json"},
	}
	p := &source.Preparer{Packages: stubLookup{candidates: candidates}}
	_, _, _, err := p.Prepare(context.Background(), compliance.Request{Product: "images-only"},
		func(compliance.Stage, int, int, string) {})
	if err == nil {
		t.Fatal("an image-only release was accepted as checkable")
	}
	if !strings.Contains(err.Error(), "0 of 1") {
		t.Errorf("the message does not say how it decided: %v", err)
	}

	// And a release nobody has analysed is a different problem with a
	// different fix, so it is a different error - not "no Helm charts", which
	// would send somebody looking at the vendor's packaging for a problem that
	// is on this side.
	p = &source.Preparer{Packages: stubLookup{}}
	_, _, _, err = p.Prepare(context.Background(), compliance.Request{Product: "unanalysed"},
		func(compliance.Stage, int, int, string) {})
	if !errors.Is(err, source.ErrNotAnalysed) {
		t.Errorf("an unanalysed release reports %v, want ErrNotAnalysed", err)
	}
	if errors.Is(err, source.ErrNoCharts) {
		t.Error("an unanalysed release is reported as shipping no charts")
	}
}

// A chart-classified artifact with many layers is not the shape the unpacker
// understands. Unpacking the first of twelve would report on a fraction of what
// shipped while looking complete.
func TestMultiLayerChartIsSkippedNotGuessed(t *testing.T) {
	const manifest = "application/vnd.oci.image.manifest.v1+json"
	candidates := []store.ChartCandidate{
		{ArtifactID: 1, Digest: "sha256:m", LayerDigest: "sha256:lm", LayerCount: 12,
			MediaType: manifest, Ref: "orbs/cfx/charts/weird",
			Annotations: map[string]string{"com.nokia.ncd.orb.type": "helmchart"}},
	}
	p := &source.Preparer{
		Packages: stubLookup{candidates: candidates},
		Classify: nearClassifier,
	}
	_, _, _, err := p.Prepare(context.Background(), compliance.Request{Product: "cfx"},
		func(compliance.Stage, int, int, string) {})
	if err == nil || !strings.Contains(err.Error(), "ships no Helm charts") {
		t.Fatalf("a twelve-layer chart was unpacked anyway: %v", err)
	}
}
