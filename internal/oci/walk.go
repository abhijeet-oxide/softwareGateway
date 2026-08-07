package oci

import (
	"context"
	"fmt"
	"sync"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
)

// Walk traverses a manifest tree from a root, fetching every manifest.
//
// The recursive descent discovery deliberately does NOT do — it records what a
// tag's own manifest lists and stops. This is the walk a TRANSFER needs, and
// the walk `packages describe --expand` performs, which is why it lives here
// rather than in either of them.
//
// Breadth-first, level by level, so parents are recorded before their children
// and each artifact's Parent — an INDEX into the slice — stays correct. A
// level's nodes are fetched in parallel; the bookkeeping that appends to the
// tree runs on one goroutine in level order.
//
// Returns the number of manifests actually fetched, which is what tells a
// caller whether it did work or found the answer already known.
func Walk(
	ctx context.Context, src registry.ManifestReader, root registry.Descriptor, concurrency int,
) (Tree, int, error) {
	if concurrency <= 0 {
		concurrency = 1
	}

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
			return Tree{}, fetched, fmt.Errorf(
				"manifest tree exceeds %d artifacts", maxListedArtifacts)
		}
		if pending[0].depth > maxTreeDepth {
			return Tree{}, fetched, fmt.Errorf(
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
		var wg sync.WaitGroup
		for i, q := range pending {
			wg.Add(1)
			go func(i int, q queuedChild) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				desc, raw, err := src.FetchManifest(ctx, q.desc.Digest.String())
				results[i] = result{q: q, desc: desc, raw: raw, err: err}
			}(i, q)
		}
		wg.Wait()

		var next []queuedChild
		for _, r := range results {
			if r.err != nil {
				return Tree{}, fetched, fmt.Errorf("fetch manifest %s: %w",
					r.q.desc.Digest.Short(), r.err)
			}
			fetched++
			children, err := appendArtifact(&t, r.q.desc, r.desc, r.raw, r.q.parent, r.q.depth)
			if err != nil {
				return Tree{}, fetched, err
			}
			next = append(next, children...)
		}
		level = next
	}

	t.TotalBytes, t.BlobCount = measure(t.Artifacts)
	return t, fetched, nil
}
