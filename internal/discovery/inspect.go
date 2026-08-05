package discovery

import (
	"context"
	"fmt"
	"sync"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// InspectResult reports what expanding one package found.
type InspectResult struct {
	Package store.PackageRow

	// Fetched is manifests fetched by THIS inspection. Zero on a re-inspect of
	// an already-expanded package, which is the correct and useful answer: the
	// work was already done and the registry was not troubled again.
	Fetched int
	// Artifacts and Blobs are the totals for the package once expanded.
	Artifacts int
	Blobs     int
	// TotalBytes is the transfer cost, each distinct digest counted once.
	TotalBytes int64
	// AlreadyExpanded reports that nothing needed fetching.
	AlreadyExpanded bool
}

// InspectPackage walks a package's manifest tree and records what it finds.
//
// Discovery deliberately stops at the tag's own manifest: it answers "what is
// new", and the root digest immutably determines everything beneath it, so the
// rest can be recovered whenever it is actually wanted (docs/design/07 §12).
// This is where it is wanted — and it is the SAME function M3's transfer calls
// before moving bytes, so a package's contents are computed one way and not
// two.
//
// Idempotent. The tree under a digest cannot change, so a second inspection
// fetches nothing and returns the same answer.
func InspectPackage(
	ctx context.Context,
	packages *store.Packages,
	pkg store.PackageRow,
	client registry.ManifestReader,
	concurrency int,
) (InspectResult, error) {
	root := registry.Descriptor{
		Digest:    registry.Digest(pkg.ManifestDigest),
		MediaType: pkg.MediaType,
	}
	if err := root.Digest.Validate(); err != nil {
		return InspectResult{}, fmt.Errorf("package %d has an unusable digest: %w", pkg.ID, err)
	}

	t, fetched, err := walkTree(ctx, client, root, concurrency)
	if err != nil {
		return InspectResult{}, err
	}

	res := InspectResult{
		Fetched:   fetched,
		Artifacts: len(t.Artifacts),
	}
	if t.TotalBytes != nil {
		res.TotalBytes = *t.TotalBytes
	}
	if t.BlobCount != nil {
		res.Blobs = *t.BlobCount
	}

	// Already expanded: the recorded blob count matches what the walk found and
	// nothing was fetched. Reported rather than silently doing the same work.
	res.AlreadyExpanded = fetched == 0

	if err := packages.RecordExpandedTree(ctx, pkg.ID, toStoreTree(t)); err != nil {
		return InspectResult{}, err
	}

	updated, err := packages.GetPackageByID(ctx, pkg.ID)
	if err != nil {
		return InspectResult{}, err
	}
	res.Package = updated
	return res, nil
}

// walkTree is the recursive descent discovery no longer does.
//
// Breadth-first, level by level, so parents are recorded before their children
// and each artifact's Parent — an INDEX into the slice — stays correct. A
// level's nodes are fetched in parallel; the bookkeeping that appends to the
// tree runs on one goroutine in level order.
//
// Returns the number of manifests actually fetched, which is what tells a
// caller whether it did work or found the answer already known.
func walkTree(
	ctx context.Context, src registry.ManifestReader, root registry.Descriptor, concurrency int,
) (tree, int, error) {
	if concurrency <= 0 {
		concurrency = 1
	}

	var t tree
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
			return tree{}, fetched, fmt.Errorf(
				"manifest tree exceeds %d artifacts", maxListedArtifacts)
		}
		if pending[0].depth > maxTreeDepth {
			return tree{}, fetched, fmt.Errorf(
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
				return tree{}, fetched, fmt.Errorf("fetch manifest %s: %w",
					r.q.desc.Digest.Short(), r.err)
			}
			fetched++
			children, err := appendArtifact(&t, r.q.desc, r.desc, r.raw, r.q.parent, r.q.depth)
			if err != nil {
				return tree{}, fetched, err
			}
			next = append(next, children...)
		}
		level = next
	}

	t.TotalBytes, t.BlobCount = measure(t.Artifacts)
	return t, fetched, nil
}

// toStoreTree flattens the in-memory tree into what the store writes.
func toStoreTree(t tree) store.ExpandedTree {
	out := store.ExpandedTree{Artifacts: make([]store.ExpandedArtifact, 0, len(t.Artifacts))}

	for _, a := range t.Artifacts {
		ea := store.ExpandedArtifact{
			Row: store.ArtifactRow{
				Digest:       a.Descriptor.Digest.String(),
				MediaType:    a.Descriptor.MediaType,
				ArtifactType: a.Descriptor.ArtifactType,
				SizeBytes:    a.Descriptor.Size,
				Depth:        a.Depth,
				Raw:          a.Raw,
			},
			Parent: a.Parent,
		}
		if a.Descriptor.Platform != nil {
			ea.Row.Platform = a.Descriptor.Platform.String()
		}
		if ea.Row.SizeBytes == 0 {
			ea.Row.SizeBytes = int64(len(a.Raw))
		}
		for _, b := range a.Blobs {
			ea.Blobs = append(ea.Blobs, store.BlobRef{
				Digest:    b.Descriptor.Digest.String(),
				MediaType: b.Descriptor.MediaType,
				SizeBytes: b.Descriptor.Size,
				Kind:      b.Kind,
				Ordinal:   b.Ordinal,
			})
		}
		out.Artifacts = append(out.Artifacts, ea)
	}

	out.TotalBytes = t.TotalBytes
	out.BlobCount = t.BlobCount
	return out
}
