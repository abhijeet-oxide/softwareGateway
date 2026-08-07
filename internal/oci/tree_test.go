package oci

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
)

// fakeManifests serves a fixed manifest graph and counts requests.
type fakeManifests struct {
	bodies map[registry.Digest][]byte

	mu    sync.Mutex
	calls int
}

func (f *fakeManifests) ResolveTag(context.Context, string) (registry.Descriptor, error) {
	return registry.Descriptor{}, fmt.Errorf("ResolveTag is not used by fetchTree")
}

func (f *fakeManifests) FetchManifest(_ context.Context, ref string) (registry.Descriptor, []byte, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()

	raw, ok := f.bodies[registry.Digest(ref)]
	if !ok {
		return registry.Descriptor{}, nil, fmt.Errorf("no such manifest %s", ref)
	}
	return registry.Descriptor{
		Digest:    registry.Digest(ref),
		Size:      int64(len(raw)),
		MediaType: registry.MediaTypeOCIManifest,
	}, raw, nil
}

// indexGraph builds an index with n children, each a leaf manifest.
func indexGraph(t *testing.T, n int) (registry.Descriptor, map[registry.Digest][]byte) {
	t.Helper()

	bodies := map[registry.Digest][]byte{}
	children := make([]registry.Descriptor, 0, n)

	for i := range n {
		leaf := map[string]any{
			"mediaType": registry.MediaTypeOCIManifest,
			"config":    map[string]any{"digest": registry.NewDigestFromBytes([]byte(fmt.Sprintf("cfg-%d", i))).String(), "size": 1},
			"layers": []map[string]any{
				{"digest": registry.NewDigestFromBytes([]byte(fmt.Sprintf("layer-%d", i))).String(), "size": 2},
			},
		}
		raw, err := json.Marshal(leaf)
		if err != nil {
			t.Fatal(err)
		}
		d := registry.NewDigestFromBytes(raw)
		bodies[d] = raw
		children = append(children, registry.Descriptor{
			Digest: d, Size: int64(len(raw)), MediaType: registry.MediaTypeOCIManifest,
		})
	}

	idx := map[string]any{"mediaType": registry.MediaTypeOCIIndex, "manifests": children}
	raw, err := json.Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	root := registry.NewDigestFromBytes(raw)
	bodies[root] = raw

	return registry.Descriptor{
		Digest: root, Size: int64(len(raw)), MediaType: registry.MediaTypeOCIIndex,
	}, bodies
}

// TestOnlyTheTagsOwnManifestIsFetched is the property the design now rests on:
// one request per newly discovered tag, whatever the package contains.
//
// The walk used to recurse, fetching every child of every index. Deferring
// loses nothing — the root digest immutably determines the whole Tree, so it
// can be walked exactly, at any time, from a digest we already hold — and it
// takes a bundle with sixty artifacts from sixty-one requests to one.
func TestOnlyTheTagsOwnManifestIsFetched(t *testing.T) {
	root, bodies := indexGraph(t, 60)

	f := &fakeManifests{bodies: bodies}
	tr, err := FetchRoot(t.Context(), f, root)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if f.calls != 1 {
		t.Errorf("made %d requests, want exactly 1: the index lists its children, "+
			"so fetching them is work we do not need to do", f.calls)
	}
	if len(tr.Artifacts) != 61 {
		t.Errorf("recorded %d artifacts, want 61 — the index plus the sixty it lists",
			len(tr.Artifacts))
	}
}

// The children must be recorded from the index's own descriptors, marked as
// listed rather than fetched. A row with bytes was verified against its digest;
// a row without has the vendor's word for it, and conflating them would make
// the record claim more than it knows.
func TestListedChildrenCarryNoBytes(t *testing.T) {
	root, bodies := indexGraph(t, 4)

	tr, err := FetchRoot(t.Context(), &fakeManifests{bodies: bodies}, root)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if tr.Artifacts[0].Raw == nil {
		t.Error("the root manifest was fetched and must carry its bytes")
	}
	if tr.Artifacts[0].Parent != -1 || tr.Artifacts[0].Depth != 0 {
		t.Errorf("root has parent=%d depth=%d, want -1 and 0",
			tr.Artifacts[0].Parent, tr.Artifacts[0].Depth)
	}

	for i, a := range tr.Artifacts[1:] {
		if a.Raw != nil {
			t.Errorf("child %d carries bytes; it was never fetched", i)
		}
		if a.Parent != 0 || a.Depth != 1 {
			t.Errorf("child %d has parent=%d depth=%d, want 0 and 1", i, a.Parent, a.Depth)
		}
		if a.Descriptor.Digest == "" {
			t.Errorf("child %d has no digest; the index states one", i)
		}
	}
}

// A size we cannot know must be reported as unknown, not as zero. The index
// states the size of each child MANIFEST, not of the layers underneath it, so
// summing what we hold would understate a bundle by nearly all of it.
func TestIndexPackageReportsUnknownSize(t *testing.T) {
	root, bodies := indexGraph(t, 3)

	tr, err := FetchRoot(t.Context(), &fakeManifests{bodies: bodies}, root)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if tr.TotalBytes != nil {
		t.Errorf("TotalBytes = %d, want nil: the layer bytes were never fetched", *tr.TotalBytes)
	}
	if tr.BlobCount != nil {
		t.Errorf("BlobCount = %d, want nil", *tr.BlobCount)
	}
}

// A plain image manifest IS fully measurable from the one fetch: its config and
// layer descriptors are inside the bytes we already have.
func TestSingleManifestPackageIsMeasured(t *testing.T) {
	leaf := map[string]any{
		"mediaType": registry.MediaTypeOCIManifest,
		"config":    map[string]any{"digest": registry.NewDigestFromBytes([]byte("cfg")).String(), "size": 7},
		"layers": []map[string]any{
			{"digest": registry.NewDigestFromBytes([]byte("l1")).String(), "size": 100},
			{"digest": registry.NewDigestFromBytes([]byte("l2")).String(), "size": 200},
		},
	}
	raw, err := json.Marshal(leaf)
	if err != nil {
		t.Fatal(err)
	}
	d := registry.NewDigestFromBytes(raw)

	f := &fakeManifests{bodies: map[registry.Digest][]byte{d: raw}}
	tr, err := FetchRoot(t.Context(), f, registry.Descriptor{
		Digest: d, MediaType: registry.MediaTypeOCIManifest,
	})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if tr.TotalBytes == nil {
		t.Fatal("a single manifest is fully measurable from its own bytes")
	}
	want := int64(7 + 100 + 200 + len(raw))
	if *tr.TotalBytes != want {
		t.Errorf("TotalBytes = %d, want %d (config + layers + the manifest itself)",
			*tr.TotalBytes, want)
	}
	if tr.BlobCount == nil || *tr.BlobCount != 3 {
		t.Errorf("BlobCount = %v, want 3", tr.BlobCount)
	}
}

// An index listing the same child twice must record it once.
func TestRepeatedChildIsRecordedOnce(t *testing.T) {
	leaf := map[string]any{"mediaType": registry.MediaTypeOCIManifest}
	leafRaw, err := json.Marshal(leaf)
	if err != nil {
		t.Fatal(err)
	}
	child := registry.Descriptor{
		Digest: registry.NewDigestFromBytes(leafRaw), MediaType: registry.MediaTypeOCIManifest,
	}

	idx := map[string]any{
		"mediaType": registry.MediaTypeOCIIndex,
		"manifests": []registry.Descriptor{child, child},
	}
	idxRaw, err := json.Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	idxDigest := registry.NewDigestFromBytes(idxRaw)

	tr, err := FetchRoot(t.Context(),
		&fakeManifests{bodies: map[registry.Digest][]byte{idxDigest: idxRaw}},
		registry.Descriptor{Digest: idxDigest, MediaType: registry.MediaTypeOCIIndex})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(tr.Artifacts) != 2 {
		t.Errorf("recorded %d artifacts, want 2: a repeated child is one artifact",
			len(tr.Artifacts))
	}
}
