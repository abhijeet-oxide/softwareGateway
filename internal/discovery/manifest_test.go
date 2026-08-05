package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
)

// fakeManifests serves a fixed manifest graph and records concurrency.
type fakeManifests struct {
	bodies map[registry.Digest][]byte

	mu      sync.Mutex
	current int
	peak    int
	calls   int
	release chan struct{}
	once    sync.Once
	// want is how many concurrent callers the gate waits for.
	want int
}

func (f *fakeManifests) ResolveTag(context.Context, string) (registry.Descriptor, error) {
	return registry.Descriptor{}, fmt.Errorf("ResolveTag is not used by fetchTree")
}

func (f *fakeManifests) FetchManifest(_ context.Context, ref string) (registry.Descriptor, []byte, error) {
	f.mu.Lock()
	f.calls++
	f.current++
	if f.current > f.peak {
		f.peak = f.current
	}
	current := f.current
	f.mu.Unlock()

	// Hold until enough callers are inside, so peak concurrency is a property
	// of the code rather than of the scheduler's timing.
	//
	// The root is fetched alone — a level of one — so the gate must not wait for
	// company that cannot arrive. The timeout is what turns "sequential" into a
	// readable assertion failure rather than a hang.
	if f.release != nil {
		if current >= f.want {
			f.once.Do(func() { close(f.release) })
		}
		select {
		case <-f.release:
		case <-time.After(300 * time.Millisecond):
			f.once.Do(func() { close(f.release) })
		}
	}

	f.mu.Lock()
	f.current--
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

// TestParallelTreeWalkMatchesSequential is the property that makes the
// parallel walk safe to ship: the artifact list, its order, and every parent
// index into it must be byte-identical to what one-at-a-time produced.
//
// Order is not cosmetic here — Artifact.Parent is an INDEX into the slice, so a
// reordering silently reparents artifacts.
func TestParallelTreeWalkMatchesSequential(t *testing.T) {
	root, bodies := indexGraph(t, 12)

	sequential, err := fetchTree(t.Context(), &fakeManifests{bodies: bodies}, root, 1)
	if err != nil {
		t.Fatalf("sequential: %v", err)
	}
	parallel, err := fetchTree(t.Context(), &fakeManifests{bodies: bodies}, root, 8)
	if err != nil {
		t.Fatalf("parallel: %v", err)
	}

	if !reflect.DeepEqual(sequential, parallel) {
		t.Errorf("parallel walk produced a different tree\n sequential: %d artifacts\n parallel:   %d artifacts",
			len(sequential.Artifacts), len(parallel.Artifacts))
		for i := range sequential.Artifacts {
			if i >= len(parallel.Artifacts) {
				break
			}
			a, b := sequential.Artifacts[i], parallel.Artifacts[i]
			if a.Descriptor.Digest != b.Descriptor.Digest || a.Parent != b.Parent || a.Depth != b.Depth {
				t.Errorf("  artifact %d: %s parent=%d depth=%d vs %s parent=%d depth=%d",
					i, a.Descriptor.Digest.Short(), a.Parent, a.Depth, b.Descriptor.Digest.Short(), b.Parent, b.Depth)
			}
		}
	}

	if len(parallel.Artifacts) != 13 {
		t.Errorf("got %d artifacts, want 13 (one index plus twelve children)", len(parallel.Artifacts))
	}
}

// TestTreeSiblingsAreFetchedConcurrently: a bundle whose index references sixty
// artifacts used to cost sixty SEQUENTIAL round trips inside one tag — minutes
// during which the tag counter did not move while every request succeeded.
func TestTreeSiblingsAreFetchedConcurrently(t *testing.T) {
	root, bodies := indexGraph(t, 8)

	f := &fakeManifests{bodies: bodies, release: make(chan struct{}), want: 4}
	if _, err := fetchTree(t.Context(), f, root, 4); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	f.mu.Lock()
	peak, calls := f.peak, f.calls
	f.mu.Unlock()

	if peak < 2 {
		t.Errorf("peak concurrency was %d; the siblings of an index are still "+
			"being fetched one at a time", peak)
	}
	if calls != 9 {
		t.Errorf("made %d fetches, want 9 (the index plus eight children)", calls)
	}
}

// A diamond — two parents referencing one child — must cost one fetch, not two.
func TestRepeatedChildIsFetchedOnce(t *testing.T) {
	leaf := map[string]any{
		"mediaType": registry.MediaTypeOCIManifest,
		"layers":    []map[string]any{{"digest": registry.NewDigestFromBytes([]byte("l")).String(), "size": 1}},
	}
	leafRaw, err := json.Marshal(leaf)
	if err != nil {
		t.Fatal(err)
	}
	leafDigest := registry.NewDigestFromBytes(leafRaw)
	child := registry.Descriptor{Digest: leafDigest, MediaType: registry.MediaTypeOCIManifest}

	idx := map[string]any{
		"mediaType": registry.MediaTypeOCIIndex,
		"manifests": []registry.Descriptor{child, child},
	}
	idxRaw, err := json.Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	idxDigest := registry.NewDigestFromBytes(idxRaw)

	f := &fakeManifests{bodies: map[registry.Digest][]byte{
		idxDigest: idxRaw, leafDigest: leafRaw,
	}}
	tr, err := fetchTree(t.Context(), f, registry.Descriptor{
		Digest: idxDigest, MediaType: registry.MediaTypeOCIIndex,
	}, 4)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if f.calls != 2 {
		t.Errorf("made %d fetches, want 2: a repeated child must be fetched once", f.calls)
	}
	if len(tr.Artifacts) != 2 {
		t.Errorf("got %d artifacts, want 2", len(tr.Artifacts))
	}
}
