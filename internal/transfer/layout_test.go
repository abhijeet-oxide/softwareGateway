package transfer

import (
	"slices"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// tree builds an ExpandedTree from a compact description.
//
// Each entry is digest, parent index, and the ref.name annotation - the three
// things layout resolution reads. Nothing else matters to it.
func tree(entries ...artifactSpec) store.ExpandedTree {
	out := store.ExpandedTree{Artifacts: make([]store.ExpandedArtifact, 0, len(entries))}
	for _, e := range entries {
		a := store.ExpandedArtifact{
			Row:    store.ArtifactRow{Digest: e.digest},
			Parent: e.parent,
		}
		if e.refName != "" {
			a.Row.Annotations = map[string]string{registry.AnnotationRefName: e.refName}
		}
		out.Artifacts = append(out.Artifacts, a)
	}
	return out
}

type artifactSpec struct {
	digest  string
	parent  int
	refName string
}

// A bundle's components land where the vendor called them, not in one flat
// heap. This is the whole point of layout resolution.
func TestComponentsKeepTheirRepositoriesAndTags(t *testing.T) {
	got := ResolveLayout(tree(
		artifactSpec{digest: "sha256:root", parent: -1},
		artifactSpec{digest: "sha256:nginx", parent: 0,
			refName: "orbs/CFX-5000-k8s/nginx:1.2.3"},
		artifactSpec{digest: "sha256:chart", parent: 0,
			refName: "orbs/CFX-5000-k8s/charts/cfx:23.8.1076"},
		artifactSpec{digest: "sha256:custo", parent: 0,
			refName: "orbs/CFX-5000-k8s:generic_custo_23.8.1076"},
	), LayoutOptions{
		BasePath:         "nokia-lab",
		SourceRepository: "orbs/CFX-5000-k8s",
		RootTags:         []string{"orb_23.8.1076"},
	})

	// The bundle stays whole: every component is in the ORB's own repository,
	// because an index may only reference what its repository serves. A
	// component named elsewhere is ALSO published there, under its own name.
	assertPlacements(t, got, map[string]Placement{
		"sha256:root": {Sites: []Site{
			{Repository: "nokia-lab/orbs/CFX-5000-k8s", Tags: []string{"orb_23.8.1076"}},
		}},
		"sha256:nginx": {Sites: []Site{
			{Repository: "nokia-lab/orbs/CFX-5000-k8s"},
			{Repository: "nokia-lab/orbs/CFX-5000-k8s/nginx", Tags: []string{"1.2.3"}},
		}},
		"sha256:chart": {Sites: []Site{
			{Repository: "nokia-lab/orbs/CFX-5000-k8s"},
			{Repository: "nokia-lab/orbs/CFX-5000-k8s/charts/cfx", Tags: []string{"23.8.1076"}},
		}},
		// Named within the ORB's own repository, so one site and the tag
		// belongs right there.
		"sha256:custo": {Sites: []Site{
			{Repository: "nokia-lab/orbs/CFX-5000-k8s", Tags: []string{"generic_custo_23.8.1076"}},
		}},
	})
}

// No base path means the destination mirrors the source exactly, so a consumer
// changes the hostname and nothing else.
func TestNoBasePathMirrorsTheSourceExactly(t *testing.T) {
	got := ResolveLayout(tree(
		artifactSpec{digest: "sha256:root", parent: -1},
		artifactSpec{digest: "sha256:nginx", parent: 0, refName: "orbs/CFX-5000-k8s/nginx:1.2.3"},
	), LayoutOptions{
		SourceRepository: "orbs/CFX-5000-k8s",
		RootTags:         []string{"orb_23.8.1076"},
	})

	assertPlacements(t, got, map[string]Placement{
		"sha256:root": {Sites: []Site{
			{Repository: "orbs/CFX-5000-k8s", Tags: []string{"orb_23.8.1076"}},
		}},
		"sha256:nginx": {Sites: []Site{
			{Repository: "orbs/CFX-5000-k8s"},
			{Repository: "orbs/CFX-5000-k8s/nginx", Tags: []string{"1.2.3"}},
		}},
	})
}

// An ordinary multi-platform image annotates nothing. Its children must follow
// the index rather than scatter, which is what inheritance is for.
func TestUnannotatedChildrenFollowTheirParent(t *testing.T) {
	got := ResolveLayout(tree(
		artifactSpec{digest: "sha256:index", parent: -1},
		artifactSpec{digest: "sha256:amd64", parent: 0},
		artifactSpec{digest: "sha256:arm64", parent: 0},
	), LayoutOptions{
		BasePath:         "lab",
		SourceRepository: "vendor/suite",
		RootTags:         []string{"v1.0.0"},
	})

	for _, digest := range []string{"sha256:index", "sha256:amd64", "sha256:arm64"} {
		p := got[digest]
		if len(p.Sites) != 1 {
			t.Errorf("%s landed in %d places, want 1", digest, len(p.Sites))
			continue
		}
		if p.Primary().Repository != "lab/vendor/suite" {
			t.Errorf("%s landed in %q, want lab/vendor/suite", digest, p.Primary().Repository)
		}
	}
	// Only the index is named. A platform manifest carrying its own tag would
	// be wrong: the tag belongs to the index that selects between them.
	if tags := got["sha256:amd64"].Primary().Tags; len(tags) != 0 {
		t.Errorf("platform manifest got tags %v, want none", tags)
	}
}

// A component nested below a named parent inherits that parent's destination,
// not the package's. Without this, anything two levels deep under a relocated
// component would land back at the bundle's own path.
func TestInheritanceFollowsTheNearestNamedAncestor(t *testing.T) {
	got := ResolveLayout(tree(
		artifactSpec{digest: "sha256:root", parent: -1},
		artifactSpec{digest: "sha256:sub", parent: 0, refName: "orbs/other-product:sub_1.0"},
		artifactSpec{digest: "sha256:leaf", parent: 1},
	), LayoutOptions{
		BasePath:         "lab",
		SourceRepository: "orbs/CFX-5000-k8s",
	})

	// The leaf stays inside the bundle's own repository - that is where its
	// parent physically lives, whatever the parent is CALLED elsewhere.
	if repo := got["sha256:leaf"].Primary().Repository; repo != "lab/orbs/CFX-5000-k8s" {
		t.Errorf("leaf lands in %q, want lab/orbs/CFX-5000-k8s", repo)
	}
	// The named parent is published under its own name as well as staying put.
	sub := got["sha256:sub"].Sites
	if len(sub) != 2 || sub[1].Repository != "lab/orbs/other-product" {
		t.Errorf("named component sites = %+v, want a second at lab/orbs/other-product", sub)
	}
}

// One artifact referenced twice keeps every name it was given. Dropping one
// would leave a tag at the source with no counterpart at the destination.
func TestSharedArtifactKeepsEveryTag(t *testing.T) {
	got := ResolveLayout(tree(
		artifactSpec{digest: "sha256:root", parent: -1},
		artifactSpec{digest: "sha256:base", parent: 0, refName: "orbs/p:base_1.0"},
		artifactSpec{digest: "sha256:base", parent: 0, refName: "orbs/p:base_latest"},
	), LayoutOptions{SourceRepository: "orbs/p"})

	tags := got["sha256:base"].Primary().Tags
	slices.Sort(tags)
	if !slices.Equal(tags, []string{"base_1.0", "base_latest"}) {
		t.Errorf("shared artifact kept tags %v, want both base_1.0 and base_latest", tags)
	}
}

// The reference annotation is free text written by whoever published the
// artifact. Each shape it legitimately takes has to be read correctly, and
// anything that is not a name at all has to be ignored rather than turned
// into a repository at the destination.
func TestParseRefName(t *testing.T) {
	for _, tc := range []struct {
		in       string
		wantRepo string
		wantTag  string
	}{
		{"orbs/CFX-5000-k8s:orb_23.8.1076", "orbs/CFX-5000-k8s", "orb_23.8.1076"},
		{"orbs/CFX-5000-k8s", "orbs/CFX-5000-k8s", ""},
		{"orb_23.8.1076", "", "orb_23.8.1076"},
		{"registry.example.com/orbs/p:v1", "registry.example.com/orbs/p", "v1"},
		{"  orbs/p:v1  ", "orbs/p", "v1"},
		{"/orbs/p/:v1", "orbs/p", "v1"},
		{"", "", ""},
		// A digest is not a name. Parsed as repo:tag it would create a
		// repository literally called `sha256` at the destination.
		{"sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", "", ""},
		// A colon with a path after it is not a tag: tags cannot contain `/`.
		{"orbs/p:weird/value", "orbs/p:weird/value", ""},
		{"orbs/p:", "orbs/p:", ""},
	} {
		got := parseRefName(tc.in)
		if got.repository != tc.wantRepo || got.tag != tc.wantTag {
			t.Errorf("parseRefName(%q) = {%q, %q}, want {%q, %q}",
				tc.in, got.repository, got.tag, tc.wantRepo, tc.wantTag)
		}
	}
}

func assertPlacements(t *testing.T, got, want map[string]Placement) {
	t.Helper()

	for digest, w := range want {
		g, ok := got[digest]
		if !ok {
			t.Errorf("%s has no placement", digest)
			continue
		}
		if len(g.Sites) != len(w.Sites) {
			t.Errorf("%s lands in %d places (%+v), want %d (%+v)",
				digest, len(g.Sites), g.Sites, len(w.Sites), w.Sites)
			continue
		}
		for i := range w.Sites {
			if g.Sites[i].Repository != w.Sites[i].Repository {
				t.Errorf("%s site %d is %q, want %q",
					digest, i, g.Sites[i].Repository, w.Sites[i].Repository)
			}
			if !slices.Equal(g.Sites[i].Tags, w.Sites[i].Tags) {
				t.Errorf("%s site %d tagged %v, want %v",
					digest, i, g.Sites[i].Tags, w.Sites[i].Tags)
			}
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d placements, want %d", len(got), len(want))
	}
}

// A vendor's ref.name annotation need not agree letter for letter with the
// repository the artifact actually lives in. Observed in production: content in
// `orbs/cfx-5000-k8s-215952-edgenac-…` annotated
// `orbs/CFX-5000-k8s-215952-edgeNAC-…:orb_25.7_…`. The same repository, spelled
// twice.
//
// Compared byte for byte, that is two repositories - so the destination grew
// two sibling folders with the same name and different capitals, half the
// bundle in each, and the mixed-case one returned 401 on its tag because the
// OCI grammar for a repository name is lowercase-only and the registry scoped
// its token to the name it normalised.
func TestAnAnnotationThatRespellsItsOwnRepositoryDoesNotCreateASecondOne(t *testing.T) {
	tree := store.ExpandedTree{Artifacts: []store.ExpandedArtifact{
		{Row: store.ArtifactRow{ID: 1, Digest: "sha256:root"}, Parent: -1},
		{
			Row: store.ArtifactRow{ID: 2, Digest: "sha256:custo", Annotations: map[string]string{
				// Same repository as the container, in different capitals.
				registry.AnnotationRefName: "orbs/CFX-5000-K8s:generic_custo_23.8.1076",
			}},
			Parent: 0,
		},
	}}

	got := ResolveLayout(tree, LayoutOptions{
		SourceRepository: "orbs/cfx-5000-k8s",
		RootTags:         []string{"orb_23.8.1076"},
	})

	custo := got["sha256:custo"]
	if len(custo.Sites) != 1 {
		t.Fatalf("the component landed in %d sites: %+v; a respelling of its own "+
			"repository must not create a second one", len(custo.Sites), custo.Sites)
	}
	if custo.Sites[0].Repository != "orbs/cfx-5000-k8s" {
		t.Errorf("site = %q, want the container's own spelling", custo.Sites[0].Repository)
	}
	// And the TAG survives untouched: the grammar for a tag permits uppercase,
	// so lowercasing one would publish a reference the vendor never did.
	if len(custo.Sites[0].Tags) != 1 || custo.Sites[0].Tags[0] != "generic_custo_23.8.1076" {
		t.Errorf("tags = %v, want the vendor's own tag", custo.Sites[0].Tags)
	}
}

// A wrapper's payload keeps its own ref.name, and that name is one of the
// wrapper's own RootTags — NEAR's `signed_orb_X` bundles `orb_X` so one tag
// reaches both the payload and the signature. Without the fix the payload
// child claimed `orb_X` a second time, and the destination saw two manifests
// racing to own the same tag.
func TestAWrapperAndItsPayloadDoNotCompeteForTheSameTag(t *testing.T) {
	tree := store.ExpandedTree{Artifacts: []store.ExpandedArtifact{
		// The wrapper: the transfer root, tagged with both names by RootTags.
		{Row: store.ArtifactRow{ID: 1, Digest: "sha256:wrapper"}, Parent: -1},
		{
			Row: store.ArtifactRow{ID: 2, Digest: "sha256:payload", Annotations: map[string]string{
				registry.AnnotationRefName: "orbs/cfx-5000-k8s:orb_25.7_mp2604_2131",
			}},
			Parent: 0,
		},
		{
			Row: store.ArtifactRow{ID: 3, Digest: "sha256:signature", Annotations: map[string]string{
				registry.AnnotationRefName: "orbs/cfx-5000-k8s:signature_orb_25.7_mp2604_2131",
			}},
			Parent: 0,
		},
	}}

	got := ResolveLayout(tree, LayoutOptions{
		SourceRepository: "orbs/cfx-5000-k8s",
		RootTags:         []string{"orb_25.7_mp2604_2131", "signed_orb_25.7_mp2604_2131"},
	})

	wrapper := got["sha256:wrapper"]
	if want := []string{"orb_25.7_mp2604_2131", "signed_orb_25.7_mp2604_2131"}; !slices.Equal(wrapper.Sites[0].Tags, want) {
		t.Errorf("wrapper tags = %v, want %v", wrapper.Sites[0].Tags, want)
	}

	// The payload shares the wrapper's repository and the wrapper's own name,
	// so it must carry NO tag of its own: the wrapper already answers to it.
	payload := got["sha256:payload"]
	if len(payload.Sites) != 1 || len(payload.Sites[0].Tags) != 0 {
		t.Errorf("payload sites = %+v, want one untagged site", payload.Sites)
	}

	// The signature's own name is not a root tag, so it keeps it.
	signature := got["sha256:signature"]
	if want := []string{"signature_orb_25.7_mp2604_2131"}; !slices.Equal(signature.Sites[0].Tags, want) {
		t.Errorf("signature tags = %v, want %v", signature.Sites[0].Tags, want)
	}
}

// The contract that must NOT change: a vendor genuinely serving a mixed-case
// repository is mirrored to a destination of the same name. Reading is
// verbatim, and so is the path derived from it.
func TestAMixedCaseSourceRepositoryIsStillMirroredVerbatim(t *testing.T) {
	tree := store.ExpandedTree{Artifacts: []store.ExpandedArtifact{
		{Row: store.ArtifactRow{ID: 1, Digest: "sha256:root"}, Parent: -1},
	}}

	got := ResolveLayout(tree, LayoutOptions{
		SourceRepository: "orbs/CFX-5000-k8s",
		RootTags:         []string{"v1"},
	})
	if r := got["sha256:root"].Sites[0].Repository; r != "orbs/CFX-5000-k8s" {
		t.Errorf("site = %q, want the source's own spelling mirrored", r)
	}
}

// Two genuinely different repositories are still two, whatever their capitals.
func TestADifferentRepositoryStillGetsItsOwnSite(t *testing.T) {
	tree := store.ExpandedTree{Artifacts: []store.ExpandedArtifact{
		{Row: store.ArtifactRow{ID: 1, Digest: "sha256:root"}, Parent: -1},
		{
			Row: store.ArtifactRow{ID: 2, Digest: "sha256:nginx", Annotations: map[string]string{
				registry.AnnotationRefName: "orbs/cfx-5000-k8s/nginx:1.2.3",
			}},
			Parent: 0,
		},
	}}

	got := ResolveLayout(tree, LayoutOptions{
		SourceRepository: "orbs/cfx-5000-k8s",
	})
	if n := len(got["sha256:nginx"].Sites); n != 2 {
		t.Errorf("the component landed in %d sites, want its container and its own name", n)
	}
}
