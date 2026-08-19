package oci

import (
	"strings"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
)

// What an artifact IS, in the words somebody uses about it.
//
// # Why this is one function rather than one per caller
//
// Two places answer this question and they must not answer it differently: a
// comparison saying `chart` where a transfer summary says `image` describes one
// registry as two, and the reader has no way to tell which is wrong. It lives
// here because both callers already depend on this package's vocabulary, and
// because the answer is derived from OCI fields alone - no vendor, no
// configuration, and nothing about where the artifact came from.
//
// # Why the media type leads
//
// It is what the specification defines and what the registry actually serves.
// `artifactType` is consulted second, for the OCI 1.1 artifacts that carry one,
// and never a repository NAME: a path called `charts/` holding an image is a
// mislabelled path, not a chart, and a classifier that believed the name would
// report the mislabelling as fact.
//
// # Why the CONFIG media type is consulted, and why it is not optional
//
// A Helm chart is an ordinary image manifest whose CONFIG says what it is:
//
//	mediaType    application/vnd.oci.image.manifest.v1+json
//	artifactType (absent)
//	config       application/vnd.cncf.helm.config.v1+json
//
// `artifactType` arrived in OCI 1.1, and the specification says that where it
// is absent the config's media type takes its place. Helm predates it and still
// publishes exactly this shape, so a classifier reading only the top two fields
// sees an image manifest with nothing to distinguish it and says `image`.
//
// It said it 97 times on one orb. The vendor's own catalogue listed `157 CNF
// Images, 97 Helm Charts`; this reported 257 images, which is not a rounding
// difference but a whole category the operator could not see.

// Kinds, as a person says them. A bounded set, so it is safe as a map key, a
// column heading and a metric label.
const (
	KindIndex     = "index"
	KindImage     = "image"
	KindChart     = "chart"
	KindFile      = "file"
	KindSignature = "signature"
	// KindArtifact is content that declares nothing about itself. Deliberately
	// not folded into `image`: a guess recorded as a fact is worse than an
	// honest "something is here and it did not say what".
	KindArtifact = "artifact"
)

// Classify names what an artifact is from the fields that describe it.
//
// configMediaType is the media type of the manifest's config blob, and may be
// empty for an index - which has none - or for an artifact nothing has fetched.
// Any of the three may be empty, and that is a real state rather than a
// caller's mistake: content older than OCI 1.1 has no `artifactType`, and a
// descriptor written by something careless has none of them.
func Classify(mediaType, artifactType, configMediaType string) string {
	// The specification's own fallback: where a manifest declares no
	// artifactType, its config's media type IS the artifact type. Applied here
	// rather than at each test below, so every rule sees the same answer to
	// "what does this claim to be".
	if artifactType == "" {
		artifactType = configMediaType
	}

	switch {
	case registry.IsIndex(mediaType):
		return KindIndex
	case strings.Contains(artifactType, "helm"), strings.Contains(mediaType, "helm"):
		return KindChart
	case strings.Contains(artifactType, "signature"), strings.Contains(artifactType, "sig"):
		return KindSignature
	case strings.Contains(artifactType, "generic"):
		return KindFile
	case mediaType == "":
		return KindArtifact
	default:
		return KindImage
	}
}

// KindOrder is the order these read in, most structural first.
//
// Fixed rather than alphabetical or by count: a reader comparing two transfers
// compares rows by position, and a table whose rows reorder when a count changes
// makes that impossible.
var KindOrder = []string{
	KindIndex, KindImage, KindChart, KindFile, KindSignature, KindArtifact,
}

// RankOf is where a kind sorts, for callers assembling a table.
func RankOf(kind string) int {
	for i, k := range KindOrder {
		if k == kind {
			return i
		}
	}
	return len(KindOrder)
}
