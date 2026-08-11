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

// Root is the descriptor a package's tree is walked from, which is NOT always
// the package's own manifest.
//
// Where a vendor bundles the payload with its signature under a wrapper index,
// only the wrapper reaches both — so walking the payload alone would move the
// bytes and LEAVE THE SIGNATURE BEHIND. The layout plugin recorded the wrapper
// at discovery; this is where that decision is used.
//
// It lives here, beside Ensure, because inspect and the planner MUST agree
// about it. They did not: the planner honoured the transfer root while inspect
// walked the payload, so `packages inspect` reported a transfer size that
// excluded the signature and recorded a tree that was not the tree a transfer
// plans from — while saying, in as many words, that those were the numbers a
// transfer would move.
func Root(pkg store.PackageRow) (registry.Descriptor, error) {
	dgst := registry.Digest(pkg.ManifestDigest)
	mediaType := pkg.MediaType

	if pkg.TransferRootDigest != "" {
		dgst = registry.Digest(pkg.TransferRootDigest)
		// The wrapper's media type is not recorded separately; an index is the
		// only shape a wrapper can be, and the walk re-reads it from the
		// response anyway.
		mediaType = registry.MediaTypeOCIIndex
	}

	if err := dgst.Validate(); err != nil {
		return registry.Descriptor{}, fmt.Errorf("package %d has an unusable root digest: %w", pkg.ID, err)
	}
	return registry.Descriptor{Digest: dgst, MediaType: mediaType}, nil
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
	// Complete, AND rooted at what was asked for. The second half is not
	// pedantry: discovery records the tree of the tag it fetched — the payload —
	// while a transfer walks the wrapper that reaches the payload AND its
	// signature. Judging completeness on its own let a payload-rooted tree
	// satisfy a request for the wrapper, so the signature manifest was never
	// fetched, its size never counted, and its contents never recorded.
	if complete && rootedAt(recorded, root) {
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

// rootedAt reports whether a recorded tree is the tree under this descriptor.
//
// The root is the depth-0 artifact. A tree recorded from a different root may be
// complete and still be the wrong tree.
func rootedAt(t store.ExpandedTree, root registry.Descriptor) bool {
	for _, a := range t.Artifacts {
		if a.Row.Depth == 0 {
			return a.Row.Digest == root.Digest.String()
		}
	}
	return false
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
				// Annotations are what a bundle uses to say what each of its
				// components is CALLED — the reserved
				// org.opencontainers.image.ref.name, merged by the walker from
				// the referencing descriptor and the manifest's own. Dropping
				// them here made every component of a bundle anonymous by the
				// time the planner read the tree back, so all of them landed
				// in one flat destination with no tag.
				Annotations: a.Descriptor.Annotations,
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
