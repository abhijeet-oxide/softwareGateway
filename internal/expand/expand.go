// Package expand turns a DISCOVERED package into a fully known one.
//
// # The two states a package can be in
//
// Discovery is deliberately light. It fetches a tag's own manifest and records
// what that manifest LISTS — each child's digest, media type, size and platform,
// straight out of the index's own descriptors — without fetching any of them.
// That answers "what is new" in two requests per tag, where walking would have
// cost one per artifact, and a first scan of a real vendor catalogue would
// otherwise run into five figures of requests.
//
// The cost is that such a package's transfer size is unknown: an index states
// the size of each child MANIFEST, not of the layers underneath it. Nothing is
// LOST by deferring, because the root digest immutably determines the entire
// tree, so it can be walked exactly, at any time, from a digest already held.
//
// # Why this is one function and not two
//
// Two callers want the walk, for different reasons and at different times.
// `packages inspect` wants it because a person asked how big something is
// before deciding to move it. The transfer planner wants it because it cannot
// create jobs without knowing what the jobs are.
//
// They must produce the SAME tree, and whichever arrives first must pay for it
// once. That was true by convention when each had its own copy of the logic,
// which is exactly the arrangement that stops being true the first time one of
// them is edited. So it is one function, and both call it.
//
// The record it writes is a CACHE and can never be stale: content addressing is
// what makes reuse safe here, and would not make it safe for anything mutable.
package expand

import (
	"context"
	"fmt"

	"github.com/abhijeet-oxide/softwareGateway/internal/oci"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// Result is one expansion.
type Result struct {
	// Tree is the package's complete contents, with database row IDs resolved —
	// a manifest job references its artifact row, so the caller needs them.
	Tree store.ExpandedTree

	// Fetched is manifests pulled from the registry by THIS call. Zero when the
	// record already held the tree, which is the useful answer rather than a
	// missing one: it says the registry was not troubled.
	Fetched int
	// FromRecord reports that nothing was fetched.
	FromRecord bool
}

// Ensure returns a package's complete tree, walking the registry only when the
// record does not already hold it.
//
// src may be nil for a caller that is only willing to use what is recorded; it
// then fails rather than reaching the network, which is what a follower replica
// needs.
func Ensure(
	ctx context.Context,
	packages *store.Packages,
	packageID int64,
	root registry.Descriptor,
	src registry.ManifestReader,
	concurrency int,
) (Result, error) {
	// THE RECORD FIRST. A tree already walked can never be stale, so this is
	// not an optimisation with a correctness caveat — it is the primary path,
	// and the walk is the fallback.
	//
	// Completeness is "every artifact was fetched", not "every artifact's bytes
	// are still cached": the manifest bodies are evictable and a package whose
	// bodies were reclaimed is still fully known. See store migration 00007.
	recorded, complete, err := packages.ReadExpandedTree(ctx, packageID)
	if err != nil {
		return Result{}, err
	}
	if complete {
		return Result{Tree: recorded, FromRecord: true}, nil
	}

	if src == nil {
		return Result{}, fmt.Errorf(
			"package %d has not been expanded and no source client was supplied", packageID)
	}

	t, fetched, err := oci.Walk(ctx, src, root, concurrency)
	if err != nil {
		return Result{}, fmt.Errorf("walk package %d: %w", packageID, err)
	}

	if err := packages.RecordExpandedTree(ctx, packageID, ToStoreTree(t)); err != nil {
		return Result{}, err
	}

	// Re-read rather than returning the in-memory tree, so the artifacts carry
	// their database IDs and so both branches of this function return a value
	// assembled the same way. A caller must not be able to tell which path it
	// took except by looking at Fetched.
	stored, _, err := packages.ReadExpandedTree(ctx, packageID)
	if err != nil {
		return Result{}, err
	}
	return Result{Tree: stored, Fetched: fetched}, nil
}

// ToStoreTree flattens the in-memory oci.Tree into what the store writes.
//
// Exported and shared because discovery and the transfer planner both had a
// byte-identical copy of it, which is one edit away from two subtly different
// answers to "how big is this package".
func ToStoreTree(t oci.Tree) store.ExpandedTree {
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
