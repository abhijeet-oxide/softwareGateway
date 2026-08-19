package discovery

import (
	"context"

	"github.com/abhijeet-oxide/softwareGateway/internal/expand"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/vendors"
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

	// CachedManifests is how many of this package's manifest bodies are still
	// held locally, out of Artifacts.
	//
	// Reported because it is the visible edge of a design decision an operator
	// should not have to discover by surprise: the bodies are an evictable
	// cache with a budget, and a package whose bodies were swept is still fully
	// described — only a future push would have to re-read them. See
	// store.SweepManifestCache.
	CachedManifests int
	CachedBytes     int64

	// SignatureResolved is how many signature relations had their MATERIAL
	// recorded — the blob a verifier reads, and the vendor annotations that say
	// what kind of signature it is. See resolveRelationMaterial.
	SignatureResolved int
}

// InspectPackage walks a package's manifest tree and records what it finds.
//
// This is the command surface over internal/expand: the SAME function a
// transfer calls before moving bytes, so a package's contents are computed one
// way and not two, and whichever caller arrives first pays for the walk.
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
	// The TRANSFER ROOT, not the package's own manifest — the same descriptor
	// the planner walks. Where a vendor wraps the payload and its signature in
	// an index, only the wrapper reaches both, so walking the payload would
	// measure a transfer that excludes the signature and would never see the
	// signature manifest at all.
	root, err := expand.Root(pkg)
	if err != nil {
		return InspectResult{}, err
	}

	// A CONCURRENCY OF ZERO IS NOT "no limit", it is one at a time: the walker
	// clamps a non-positive value to a single worker. Every caller of this was
	// passing a source's configured ceiling straight through, and a source that
	// configures none — which is most of them — was walking a two-hundred
	// component release one manifest at a time.
	if concurrency <= 0 {
		concurrency = product.DefaultPerRegistry
	}

	out, err := expand.Ensure(ctx, packages, pkg.ID, root, client, concurrency)
	if err != nil {
		return InspectResult{}, err
	}

	res := InspectResult{
		Fetched:         out.Fetched,
		Artifacts:       len(out.Tree.Artifacts),
		AlreadyExpanded: out.FromRecord,
	}
	if out.Tree.TotalBytes != nil {
		res.TotalBytes = *out.Tree.TotalBytes
	}
	if out.Tree.BlobCount != nil {
		res.Blobs = *out.Tree.BlobCount
	}
	for _, a := range out.Tree.Artifacts {
		if a.Row.Cached {
			res.CachedManifests++
			res.CachedBytes += int64(len(a.Row.Raw))
		}
	}

	// Resolve the signature material while the tree is in hand. See
	// resolveRelationMaterial — this is the one moment the signature manifest's
	// contents are known without paying for another request.
	//
	// Best effort: a package whose size was measured must not be reported as a
	// failure because a descriptive field could not be written.
	resolved, err := resolveRelationMaterial(ctx, packages, pkg.ID, out.Tree)
	if err != nil {
		return InspectResult{}, err
	}
	res.SignatureResolved = resolved

	// Re-read the package row: the walk wrote the measured size, the blob count
	// and expanded_at onto it, and the caller's copy predates all three.
	updated, err := packages.GetPackageByID(ctx, pkg.ID)
	if err != nil {
		return InspectResult{}, err
	}
	res.Package = updated
	return res, nil
}

// resolveRelationMaterial records WHAT a package's signature is, now that its
// manifest has been fetched.
//
// Discovery can only record that a signature EXISTS: it sees the wrapper's
// descriptor of the signature manifest and stops there, because fetching that
// manifest is exactly the walk discovery defers. So the bytes a verifier
// actually reads — for NEAR, one `application/pkcs7-signature` layer — were
// recorded like any other blob and were indistinguishable from a Helm chart
// sitting beside them.
//
// The rule is vendor-neutral: a relation of role `signature` names a manifest,
// and that manifest's LAYERS are the material. That holds for a wrapper index,
// for a cosign tag, and for the referrers API, so a verifier reads one row
// rather than re-deriving a vendor's layout — which is the coupling
// internal/vendors exists to prevent.
//
// Verification itself is a later milestone. This is what it will need, captured
// at the only moment it is free.
func resolveRelationMaterial(
	ctx context.Context, packages *store.Packages, packageID int64, tree store.ExpandedTree,
) (int, error) {
	relations, err := packages.ListRelations(ctx, packageID)
	if err != nil {
		return 0, err
	}

	// Annotations live on the artifact rows rather than in the tree, and they
	// are half the point: a verifier reads the vendor's own keys off them.
	artifacts, err := packages.ListArtifacts(ctx, packageID)
	if err != nil {
		return 0, err
	}
	annotations := make(map[string]map[string]string, len(artifacts))
	for _, a := range artifacts {
		annotations[a.Digest] = a.Annotations
	}

	blobs := make(map[string][]store.BlobRef, len(tree.Artifacts))
	for _, a := range tree.Artifacts {
		blobs[a.Row.Digest] = a.Blobs
	}

	resolved := 0
	for _, rel := range relations {
		if rel.Role != string(vendors.RoleSignature) {
			continue
		}
		material := store.RelationRow{Annotations: annotations[rel.Digest]}

		// The layer, not the config: a signature manifest's config is a stub —
		// NEAR's is the two bytes `{}` — and the signature is the payload.
		for _, b := range blobs[rel.Digest] {
			if b.Kind == "layer" {
				material.BlobDigest = b.Digest
				material.BlobMediaType = b.MediaType
				material.BlobSize = b.SizeBytes
				break
			}
		}

		if err := packages.RecordRelationMaterial(
			ctx, packageID, rel.Role, rel.Digest, material); err != nil {
			return resolved, err
		}
		resolved++
	}
	return resolved, nil
}
