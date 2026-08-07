package discovery

import (
	"context"
	"fmt"

	"github.com/abhijeet-oxide/softwareGateway/internal/oci"
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

// InspectPackage walks a package's manifest oci.Tree and records what it finds.
//
// Discovery deliberately stops at the tag's own manifest: it answers "what is
// new", and the root digest immutably determines everything beneath it, so the
// rest can be recovered whenever it is actually wanted (docs/design/07 §12).
// This is where it is wanted — and it is the SAME function M3's transfer calls
// before moving bytes, so a package's contents are computed one way and not
// two.
//
// Idempotent. The oci.Tree under a digest cannot change, so a second inspection
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

	// THE RECORD FIRST. The tree under a digest is immutable, so a tree already
	// walked can never be stale — content addressing is what makes this cache
	// safe, and would not make it safe for anything mutable.
	//
	// This used to walk unconditionally and set AlreadyExpanded from a count
	// that could never be zero, so the function claimed idempotence while
	// re-fetching every manifest each time. Measured: three registry calls on
	// the second inspection of a three-manifest package.
	recorded, complete, err := packages.ReadExpandedTree(ctx, pkg.ID)
	if err != nil {
		return InspectResult{}, err
	}
	if complete {
		return inspectResultFrom(ctx, packages, pkg.ID, recorded, 0, true)
	}

	t, fetched, err := oci.Walk(ctx, client, root, concurrency)
	if err != nil {
		return InspectResult{}, err
	}

	expanded := toStoreTree(t)
	if err := packages.RecordExpandedTree(ctx, pkg.ID, expanded); err != nil {
		return InspectResult{}, err
	}
	return inspectResultFrom(ctx, packages, pkg.ID, expanded, fetched, false)
}

// inspectResultFrom builds the answer from a tree, however it was obtained.
//
// One shape whether the tree came from the registry or from the record, so a
// caller cannot tell — and cannot come to depend on — which happened. Fetched
// is the honest difference: it is the count of manifests THIS call pulled, and
// zero is the useful answer rather than a missing one.
func inspectResultFrom(
	ctx context.Context, packages *store.Packages, packageID int64,
	t store.ExpandedTree, fetched int, cached bool,
) (InspectResult, error) {
	res := InspectResult{
		Fetched:         fetched,
		Artifacts:       len(t.Artifacts),
		AlreadyExpanded: cached,
	}
	if t.TotalBytes != nil {
		res.TotalBytes = *t.TotalBytes
	}
	if t.BlobCount != nil {
		res.Blobs = *t.BlobCount
	}

	updated, err := packages.GetPackageByID(ctx, packageID)
	if err != nil {
		return InspectResult{}, err
	}
	res.Package = updated
	return res, nil
}

// toStoreTree flattens the in-memory oci.Tree into what the store writes.
func toStoreTree(t oci.Tree) store.ExpandedTree {
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
