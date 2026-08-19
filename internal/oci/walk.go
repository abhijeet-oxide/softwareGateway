package oci

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
)

// Walk traverses a manifest tree from a root, fetching every manifest.
//
// The recursive descent discovery deliberately does NOT do - it records what a
// tag's own manifest lists and stops. This is the walk a TRANSFER needs, and
// the walk `packages describe --expand` performs, which is why it lives here
// rather than in either of them.
//
// Breadth-first, level by level, so parents are recorded before their children
// and each artifact's Parent - an INDEX into the slice - stays correct. A
// level's nodes are fetched in parallel; the bookkeeping that appends to the
// tree runs on one goroutine in level order.
//
// Returns the number of manifests actually fetched, which is what tells a
// caller whether it did work or found the answer already known.
func Walk(
	ctx context.Context, src registry.ManifestReader, root registry.Descriptor, concurrency int,
) (Tree, int, error) {
	t, _, fetched, err := walk(ctx, src, root, concurrency, false, nil)
	return t, fetched, err
}

// Missing is a manifest an index references and the registry would not serve.
type Missing struct {
	// Descriptor is what the REFERENCING index said about it - including the
	// `org.opencontainers.image.ref.name` annotation, which is what makes a
	// missing component identifiable rather than merely a digest.
	Descriptor registry.Descriptor
	Parent     int
	Depth      int
	Err        error
}

// WalkPartial is Walk that RECORDS an unreachable child instead of failing.
//
// A transfer that stopped part-way leaves exactly this: an index naming
// children the destination does not have. For a transfer that is a fatal
// inconsistency, which is why Walk refuses it - but for a comparison it is the
// FINDING, and a walk that aborted on the first missing component could not
// report the other nineteen.
//
// The referencing descriptor is kept for each one, so a missing component is
// still identifiable by the name its parent gave it.
func WalkPartial(
	ctx context.Context, src registry.ManifestReader, root registry.Descriptor, concurrency int,
) (Tree, []Missing, int, error) {
	return walk(ctx, src, root, concurrency, true, nil)
}

// Progress is how far a walk has got, reported while it is still going.
//
// # Why a walk can say how far it is and not how far there is to go
//
// The tree is DISCOVERED by walking it: a level's children are only known once
// that level has been read. So Total is what is known so far - the current
// level's population plus everything read before it - and it grows as the walk
// opens each level. That is honest, and it is very nearly a real denominator
// for the shape this walks in practice: an orb is a root index and then one
// wide level of components, so the total settles after the first fetch and the
// bar afterwards means what it looks like it means.
//
// A caller that wants a percentage should treat a Total that is still growing
// as an estimate, and say so.
type Progress struct {
	// Fetched is how many manifests have been read.
	Fetched int
	// Known is Fetched plus what is queued and not yet read.
	Known int
}

// ProgressFunc is called as a walk proceeds. It runs on the fetching
// goroutines, so an implementation must be cheap and safe to call
// concurrently - the walk holds no lock while calling it.
type ProgressFunc func(Progress)

// WalkPartialProgress is WalkPartial that reports as it goes.
//
// A separate entry point rather than a parameter on WalkPartial, because every
// existing caller wants the answer and not the commentary, and a nil callback
// argument at four call sites is four places to read past.
func WalkPartialProgress(
	ctx context.Context, src registry.ManifestReader, root registry.Descriptor,
	concurrency int, onProgress ProgressFunc,
) (Tree, []Missing, int, error) {
	return walk(ctx, src, root, concurrency, true, onProgress)
}

func walk(
	ctx context.Context, src registry.ManifestReader, root registry.Descriptor,
	concurrency int, tolerant bool, onProgress ProgressFunc,
) (Tree, []Missing, int, error) {
	if concurrency <= 0 {
		concurrency = 1
	}
	var missing []Missing

	var t Tree
	level := []queuedChild{{desc: root, parent: -1, depth: 0}}
	seen := map[registry.Digest]bool{}
	fetched := 0

	for len(level) > 0 {
		pending := make([]queuedChild, 0, len(level))
		for _, q := range level {
			if seen[q.desc.Digest] {
				continue
			}
			seen[q.desc.Digest] = true
			pending = append(pending, q)
		}
		if len(pending) == 0 {
			break
		}
		if len(t.Artifacts)+len(pending) > maxListedArtifacts {
			return Tree{}, missing, fetched, fmt.Errorf(
				"manifest tree exceeds %d artifacts", maxListedArtifacts)
		}
		if pending[0].depth > maxTreeDepth {
			return Tree{}, missing, fetched, fmt.Errorf(
				"manifest tree deeper than %d levels", maxTreeDepth)
		}

		type result struct {
			q    queuedChild
			desc registry.Descriptor
			raw  []byte
			err  error
		}
		results := make([]result, len(pending))

		sem := make(chan struct{}, concurrency)
		var (
			wg   sync.WaitGroup
			done atomic.Int64
		)
		// The denominator is what is KNOWN, not what exists: this level's
		// population plus everything already read. It grows as levels open,
		// which is the truth about a tree being discovered by walking it.
		known := fetched + len(pending)
		for i, q := range pending {
			wg.Add(1)
			go func(i int, q queuedChild) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				desc, raw, err := src.FetchManifest(ctx, q.desc.Digest.String())
				results[i] = result{q: q, desc: desc, raw: raw, err: err}

				if onProgress != nil {
					// Reported from the fetching goroutine and counted with an
					// atomic, so the number moves while a level is in flight
					// rather than in one step per level - which for an orb is
					// one step for the whole walk.
					onProgress(Progress{
						Fetched: fetched + int(done.Add(1)),
						Known:   known,
					})
				}
			}(i, q)
		}
		wg.Wait()

		var next []queuedChild
		for _, r := range results {
			if r.err != nil {
				// The ROOT is never tolerated: a side whose root is unreachable
				// has nothing to compare, and reporting it as an empty bundle
				// would read as "the destination has none of this" when the
				// truth is that we could not ask.
				if !tolerant || r.q.parent < 0 {
					return Tree{}, missing, fetched, fmt.Errorf("fetch manifest %s: %w",
						r.q.desc.Digest.Short(), r.err)
				}
				missing = append(missing, Missing{
					Descriptor: r.q.desc, Parent: r.q.parent, Depth: r.q.depth, Err: r.err,
				})
				continue
			}
			fetched++
			children, err := appendArtifact(&t, r.q.desc, r.desc, r.raw, r.q.parent, r.q.depth)
			if err != nil {
				if !tolerant {
					return Tree{}, missing, fetched, err
				}
				missing = append(missing, Missing{
					Descriptor: r.q.desc, Parent: r.q.parent, Depth: r.q.depth, Err: err,
				})
				continue
			}
			next = append(next, children...)
		}
		level = next
	}

	t.TotalBytes, t.BlobCount = measure(t.Artifacts)
	return t, missing, fetched, nil
}
