package oci

import "testing"

// What an artifact is, from the fields that describe it.
//
// The case that matters is the third argument. A Helm chart and a container
// image are the SAME manifest media type with no artifactType between them, and
// a classifier reading only the first two fields calls both of them images -
// which it did, 97 times, on an orb whose vendor catalogue lists 157 images and
// 97 charts.

func TestClassifyNamesWhatTheArtifactSaysItIs(t *testing.T) {
	const (
		ociManifest = "application/vnd.oci.image.manifest.v1+json"
		ociIndex    = "application/vnd.oci.image.index.v1+json"
		dockerList  = "application/vnd.docker.distribution.manifest.list.v2+json"
	)

	for _, tc := range []struct {
		name                                 string
		mediaType, artifactType, configMedia string
		want                                 string
	}{
		{
			name:      "a chart declares itself in its config, as Helm has always published",
			mediaType: ociManifest, configMedia: "application/vnd.cncf.helm.config.v1+json",
			want: KindChart,
		},
		{
			name:      "an image has the same manifest type and an image config",
			mediaType: ociManifest, configMedia: "application/vnd.oci.image.config.v1+json",
			want: KindImage,
		},
		{
			name:      "a docker image, whose config is spelled differently",
			mediaType: ociManifest, configMedia: "application/vnd.docker.container.image.v1+json",
			want: KindImage,
		},
		{
			// artifactType is the specification's own field for this, so where a
			// publisher does set it, it wins over anything inferred.
			name:      "an OCI 1.1 chart that does declare an artifactType",
			mediaType: ociManifest, artifactType: "application/vnd.cncf.helm.config.v1+json",
			configMedia: "application/vnd.oci.image.config.v1+json",
			want:        KindChart,
		},
		{
			name: "a signature", mediaType: ociManifest,
			artifactType: "application/vnd.nokia.ncd.generic_signature",
			want:         KindSignature,
		},
		{
			name: "a generic file artifact", mediaType: ociManifest,
			configMedia: "application/vnd.nokia.ncd.generic_custo.config.v1+json",
			want:        KindFile,
		},
		{name: "an index", mediaType: ociIndex, want: KindIndex},
		{name: "a docker manifest list", mediaType: dockerList, want: KindIndex},
		{
			// An index has no config, and must not be misread as anything else
			// on account of not having one.
			name: "an index with an artifactType", mediaType: ociIndex,
			artifactType: "application/vnd.cncf.helm.config.v1+json", want: KindIndex,
		},
		{
			name: "content that declares nothing about itself", want: KindArtifact,
		},
		{
			// A manifest with no config and no artifactType. Nothing says it is
			// anything but an image, and guessing otherwise would be worse.
			name: "a manifest with neither", mediaType: ociManifest, want: KindImage,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.mediaType, tc.artifactType, tc.configMedia); got != tc.want {
				t.Errorf("Classify(%q, %q, %q) = %q, want %q",
					tc.mediaType, tc.artifactType, tc.configMedia, got, tc.want)
			}
		})
	}
}

// The order kinds read in is fixed, so a reader comparing two transfers can
// compare rows by position.
func TestKindsSortStructuralFirst(t *testing.T) {
	if RankOf(KindIndex) >= RankOf(KindImage) {
		t.Error("indexes do not sort before images")
	}
	if RankOf(KindImage) >= RankOf(KindChart) {
		t.Error("images do not sort before charts")
	}
	// An unknown kind sorts last rather than in the middle of the table.
	if RankOf("something-new") < RankOf(KindArtifact) {
		t.Error("an unrecognised kind sorted before a known one")
	}
}
