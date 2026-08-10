package discovery

import (
	"context"
	"fmt"

	"github.com/abhijeet-oxide/softwareGateway/internal/expand"
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
	root := registry.Descriptor{
		Digest:    registry.Digest(pkg.ManifestDigest),
		MediaType: pkg.MediaType,
	}
	if err := root.Digest.Validate(); err != nil {
		return InspectResult{}, fmt.Errorf("package %d has an unusable digest: %w", pkg.ID, err)
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

	// Re-read the package row: the walk wrote the measured size, the blob count
	// and expanded_at onto it, and the caller's copy predates all three.
	updated, err := packages.GetPackageByID(ctx, pkg.ID)
	if err != nil {
		return InspectResult{}, err
	}
	res.Package = updated
	return res, nil
}
