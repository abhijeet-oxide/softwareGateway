package transfer_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/test/fakeregistry"
)

// A whole bundle, copied with its structure and its names.
//
// This is the requirement the rest of the transfer path exists to serve: an
// ORB is an index over container images, Helm charts and generic artifacts,
// and each of those has its own repository path and its own tag at the source.
// Copying the bytes is not enough — a consumer who could pull
// `orbs/CFX-5000-k8s/nginx:1.2.3` from the vendor must be able to pull the same
// thing from us.
//
// NOTHING HERE IS HARD-CODED INTO THE PLANNER. The structure is discovered
// from the manifests: an index's children, and the reserved
// `org.opencontainers.image.ref.name` annotation on each. A vendor bundling
// different component types, or naming them differently, needs no code change
// — which is exactly why the generic artifacts below carry made-up layer
// media types and a media type nothing in this repository knows about.

// orbComponent is one thing an ORB contains.
type orbComponent struct {
	// refName is what the bundle calls it — the annotation everything else is
	// derived from.
	refName string
	// destination and tag are what must appear at the target.
	destination string
	tag         string
	digest      string
}

func TestORBKeepsItsStructureAndNames(t *testing.T) {
	s := newSlice(t)

	pkg, components := seedORB(t, s, "orb_23.8.1076")
	s.plan(pkg, "orb-transfer")

	got := s.drain("worker-1", 4)
	if got.Failed != 0 {
		t.Fatalf("%d jobs failed, last error: %v", got.Failed, got.LastError)
	}
	if state := s.transferState("orb-transfer"); state != "succeeded" {
		t.Fatalf("transfer is %q, want succeeded", state)
	}

	// The ORB itself, under the vendor's own path nested beneath the target's
	// configured prefix, carrying the tag a person asked for.
	assertTagged(t, s, targetPath, "orb_23.8.1076", pkg.ManifestDigest)

	// Every component, where the bundle said it belongs.
	for _, c := range components {
		assertTagged(t, s, c.destination, c.tag, c.digest)
	}
}

// A component's own manifest must arrive byte-identical, because the paths
// inside a generic artifact — `CONFIGURATION/example_parameters.json` and the
// rest — live in its layer annotations, not anywhere this tool models. Copying
// the bytes IS how that directory structure is preserved, and re-serializing
// would change the digest and break every signature over it.
func TestComponentManifestsArriveByteIdentical(t *testing.T) {
	s := newSlice(t)

	pkg, components := seedORB(t, s, "orb_23.8.1076")
	s.plan(pkg, "orb-verbatim")

	if got := s.drain("worker-1", 4); got.Failed != 0 {
		t.Fatalf("%d jobs failed, last error: %v", got.Failed, got.LastError)
	}

	for _, c := range components {
		want, ok := s.src.ManifestBytes(sourcePath, c.digest)
		if !ok {
			t.Fatalf("%s is missing from the source", c.refName)
		}
		got, ok := s.dst.ManifestBytes(c.destination, c.digest)
		if !ok {
			t.Fatalf("%s is missing from the destination at %s", c.refName, c.destination)
		}
		if string(got) != string(want) {
			t.Errorf("%s was rewritten in transit:\n source: %s\n target: %s",
				c.refName, want, got)
		}
	}

	// The layer titles are the observable end of it: a `generic_custo` whose
	// DOCUMENTATION/ and CONFIGURATION/ entries did not survive would be a
	// bundle that copied and arrived useless.
	assertLayerTitles(t, s, components)
}

func assertLayerTitles(t *testing.T, s *slice, components []orbComponent) {
	t.Helper()

	var custo orbComponent
	for _, c := range components {
		if c.tag == "generic_custo_23.8.1076" {
			custo = c
		}
	}
	if custo.digest == "" {
		t.Fatal("the test did not seed a generic_custo component")
	}

	raw, ok := s.dst.ManifestBytes(custo.destination, custo.digest)
	if !ok {
		t.Fatalf("generic_custo is missing from %s", custo.destination)
	}

	var body struct {
		Layers []struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}

	var titles []string
	for _, l := range body.Layers {
		titles = append(titles, l.Annotations["org.opencontainers.image.title"])
	}
	slices.Sort(titles)

	want := []string{
		"CONFIGURATION/example_input.json",
		"CONFIGURATION/example_parameters.json",
		"DOCUMENTATION/readme",
		"METADATA/manifest.yaml",
	}
	if !slices.Equal(titles, want) {
		t.Errorf("generic_custo arrived with layer titles %v, want %v", titles, want)
	}
}

// ---------------------------------------------------------------------------

// seedORB publishes a bundle shaped like the real thing.
//
// Every component lives in the ORB's own repository, which is not a
// simplification: an index can only reference children the registry will serve
// from the index's repository, so a bundle's parts are necessarily co-located.
// `ref.name` says what each is CALLED, and that is what the destination
// reproduces.
func seedORB(t *testing.T, s *slice, tag string) (store.PackageRow, []orbComponent) {
	t.Helper()

	// A container image and a Helm chart: ordinary OCI artifacts that happen
	// to belong to a bundle.
	shared := fakeregistry.NewLayer("a base layer both images share")
	nginx := s.src.AddImage(sourcePath, "", shared, fakeregistry.NewLayer("nginx payload"))
	chartLayer := fakeregistry.NewLayer("chart tarball")
	chartLayer.MediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
	chart := s.src.AddImage(sourcePath, "", chartLayer)

	// The generic artifacts. Their layers are ordinary blobs whose PATHS live
	// in an annotation — nothing in this tool parses or models them, which is
	// the point: they survive because the manifest is copied verbatim.
	custo := s.addGeneric(sourcePath, map[string]string{
		"DOCUMENTATION/readme":                  "how to deploy this release",
		"CONFIGURATION/example_parameters.json": `{"replicas":3}`,
		"CONFIGURATION/example_input.json":      `{"site":"example"}`,
		"METADATA/manifest.yaml":                "version: 23.8.1076",
	})
	system := s.addGeneric(sourcePath, map[string]string{
		"CONFIGURATION/system.json": `{"profile":"production"}`,
	})

	components := []orbComponent{
		{refName: sourcePath + "/nginx:1.2.3",
			destination: targetBase + "/" + sourcePath + "/nginx", tag: "1.2.3", digest: nginx},
		{refName: sourcePath + "/charts/cfx:23.8.1076",
			destination: targetBase + "/" + sourcePath + "/charts/cfx", tag: "23.8.1076", digest: chart},
		{refName: sourcePath + ":generic_custo_23.8.1076",
			destination: targetPath, tag: "generic_custo_23.8.1076", digest: custo},
		{refName: sourcePath + ":generic_system_23.8.1076",
			destination: targetPath, tag: "generic_system_23.8.1076", digest: system},
	}

	// The ORB index, naming each child with the reserved OCI annotation. This
	// is the only place the structure is stated, and the only place the
	// planner reads it from.
	children := make([]map[string]any, 0, len(components))
	for _, c := range components {
		children = append(children, map[string]any{
			"mediaType":   registry.MediaTypeOCIManifest,
			"digest":      c.digest,
			"size":        100,
			"annotations": map[string]string{registry.AnnotationRefName: c.refName},
		})
	}
	raw, err := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": registry.MediaTypeOCIIndex, "manifests": children,
	})
	if err != nil {
		t.Fatal(err)
	}
	root := s.src.AddManifest(sourcePath, tag, raw, registry.MediaTypeOCIIndex)

	return s.recordPackage(tag, root, len(components)+1), components
}

// assertTagged checks that a destination repository resolves a tag to a digest.
func assertTagged(t *testing.T, s *slice, repository, tag, digest string) {
	t.Helper()

	got := s.dst.TagDigest(repository, tag)
	switch {
	case got == "":
		t.Errorf("%s:%s does not exist at the destination", repository, tag)
	case got != digest:
		t.Errorf("%s:%s resolves to %s, want %s", repository, tag, got, digest)
	}
}
