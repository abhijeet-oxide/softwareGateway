package api

import (
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/oci"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// The fold from media types to the words a person uses.
//
// Several media types are one kind — OCI and Docker both describe an image —
// and a reader handed both has been given the tool's internals in place of an
// answer.

func TestContentGroupsFoldMediaTypesIntoKinds(t *testing.T) {
	got := toAPIContent([]store.ContentRow{
		{MediaType: "application/vnd.oci.image.manifest.v1+json",
			Outcome: store.ContentCopied, Count: 4},
		{MediaType: "application/vnd.docker.distribution.manifest.v2+json",
			Outcome: store.ContentPresent, Count: 225},
		{MediaType: "application/vnd.oci.image.manifest.v1+json",
			ArtifactType: "application/vnd.cncf.helm.config.v1+json",
			Outcome:      store.ContentPresent, Count: 21},
		{MediaType: "application/vnd.oci.image.index.v1+json",
			Outcome: store.ContentCopied, Count: 1},
	}, nil)

	if len(got) != 3 {
		t.Fatalf("folded into %d kinds, want index, image and chart: %+v", len(got), got)
	}

	// Structural first, and fixed: a reader comparing two transfers compares
	// rows by position.
	if got[0].Kind != "index" || got[1].Kind != "image" || got[2].Kind != "chart" {
		t.Errorf("kinds came out %s, %s, %s — want index, image, chart",
			got[0].Kind, got[1].Kind, got[2].Kind)
	}

	// The two image media types are one row, and their outcomes are kept apart.
	image := got[1]
	if image.Total != 229 || image.Copied != 4 || image.Present != 225 {
		t.Errorf("images = %+v, want 229 total, 4 copied, 225 present", image)
	}
	if chart := got[2]; chart.Total != 21 || chart.Present != 21 {
		t.Errorf("charts = %+v, want 21 present", chart)
	}
}

// An outcome the store grows later must not be silently counted as copied or
// present. Anything unrecognised is work still to do.
func TestAnUnknownOutcomeCountsAsOutstanding(t *testing.T) {
	got := toAPIContent([]store.ContentRow{
		{MediaType: "application/vnd.oci.image.manifest.v1+json",
			Outcome: "something-new", Count: 3},
	}, nil)

	if len(got) != 1 || got[0].Outstanding != 3 {
		t.Fatalf("unknown outcome came out %+v, want 3 outstanding", got)
	}
	if got[0].Copied != 0 || got[0].Present != 0 {
		t.Errorf("an unknown outcome was counted as a result: %+v", got[0])
	}
}

// The breakdown of a transfer and the contents of the release it moves are the
// same set of things, counted twice. They must not be named differently.
//
// A NEAR orb's charts are plain OCI image manifests carrying an image config;
// nothing in the OCI fields distinguishes them from its images, and the only
// evidence is the annotation the vendor wrote. The release page reads it. This
// page did not, so a download of 160 images, 97 charts and 2 files reported
// itself as 260 images.
func TestContentGroupsHonourTheVendorsOwnAnnotations(t *testing.T) {
	// Stands in for the product's layout plugin, which is what supplies this
	// in the handler. Only the annotation is vendor knowledge; the fold is not.
	classify := func(mediaType, artifactType, configMediaType string, annotations map[string]string) string {
		switch annotations["com.nokia.ncd.orb.type"] {
		case "helmchart":
			return oci.KindChart
		case "generic_file":
			return oci.KindFile
		}
		return oci.Classify(mediaType, artifactType, configMediaType)
	}

	const image = "application/vnd.oci.image.manifest.v1+json"
	got := toAPIContent([]store.ContentRow{
		{MediaType: image, Outcome: store.ContentCopied, Count: 160},
		{MediaType: image, Annotations: map[string]string{"com.nokia.ncd.orb.type": "helmchart"},
			Outcome: store.ContentCopied, Count: 97},
		{MediaType: image, Annotations: map[string]string{"com.nokia.ncd.orb.type": "generic_file"},
			Outcome: store.ContentPresent, Count: 2},
	}, classify)

	byKind := map[string]v1.ContentGroup{}
	for _, g := range got {
		byKind[g.Kind] = g
	}
	if g := byKind[oci.KindImage]; g.Total != 160 {
		t.Errorf("images = %d, want 160", g.Total)
	}
	if g := byKind[oci.KindChart]; g.Total != 97 || g.Copied != 97 {
		t.Errorf("charts = %+v, want 97 copied", g)
	}
	if g := byKind[oci.KindFile]; g.Total != 2 || g.Present != 2 {
		t.Errorf("files = %+v, want 2 present", g)
	}
}
