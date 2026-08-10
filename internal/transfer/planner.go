// Package transfer turns a package plus a destination into a set of jobs.
//
// It runs on the Coordinator, and it is the only place that decides WHAT will
// move. Nothing here touches a byte of content: the planner reads manifests,
// consults what is already at the destination, and writes rows. The worker
// moves the bytes.
//
// See docs/design/05-transfer-engine.md §3.
package transfer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/abhijeet-oxide/softwareGateway/internal/expand"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/vendors"
)

// Plan is what one planning run produced.
type Plan struct {
	TransferID string

	// Jobs is how many were created. A REPLAN creates none and is not an error:
	// planning is idempotent by construction.
	Jobs int
	// Blobs and Manifests break that down. Blobs is the interesting number —
	// it is the unit of work, of failure and of resumption.
	Blobs     int
	Manifests int

	// PlannedBytes is what a transfer will actually move: the sum of blobs that
	// got a job, each distinct digest counted once.
	PlannedBytes int64
	// DedupeSkippedBytes is what it will NOT move because the destination
	// already has it. This is the number that makes the second transfer of a
	// product line nearly free, and it is reported rather than buried.
	DedupeSkippedBytes int64
	// SkippedBlobs is how many blobs needed no job at all.
	SkippedBlobs int

	// MaxWave is the deepest wave, so the queue knows when a transfer is done.
	MaxWave int

	// Walked reports that the planner had to read the registry because the
	// package had not been expanded. Zero means the record already held the
	// tree — see store.ReadExpandedTree.
	Walked int
}

// Planner builds transfer plans.
type Planner struct {
	packages *store.Packages
	log      *slog.Logger

	// concurrency bounds the manifest walk, when one is needed at all.
	concurrency int
}

// NewPlanner builds a planner.
func NewPlanner(packages *store.Packages, concurrency int, log *slog.Logger) *Planner {
	if log == nil {
		log = slog.Default()
	}
	if concurrency <= 0 {
		concurrency = 8
	}
	return &Planner{packages: packages, log: log, concurrency: concurrency}
}

// Request is one package to move to one destination.
type Request struct {
	TransferID string
	RequestID  string

	Package store.PackageRow
	// SourceRepoID and TargetRepoID are catalog row IDs. The job carries them
	// rather than names, so a worker resolving a job needs no configuration.
	SourceRepoID int64
	TargetRepoID int64
	// TargetRepository is the destination PATH, already mapped from the source
	// by the target's `repositories` setting. The planner does not do the
	// mapping — that is configuration's job — but it records the result,
	// because a deployment working after replication depends on it.
	TargetRepository string

	Priority int

	// Source reads the origin repository, for the walk when one is needed.
	Source registry.ManifestReader

	// Related are the package's signature, SBOM and wrapper artifacts.
	//
	// Carried into the plan, because a transfer that moves the payload and
	// leaves the signature behind makes destination-side verification
	// impossible for good.
	Related []vendors.Related
}

// Plan turns a request into jobs.
//
// The shape, and why each step is where it is:
//
//  1. Resolve the ROOT. Not always the package's own manifest: where a vendor
//     bundles the payload with its signature under a wrapper index, only the
//     wrapper reaches both.
//  2. Get the tree. From the RECORD when it is there, from the registry when
//     it is not — and recording it either way, so the next caller is free.
//  3. Classify every distinct blob against blob_placements. A database lookup,
//     not a network call: that is what makes planning a thousand-blob package
//     fast, and the registry HEAD is deferred to the worker where it is
//     parallel and where a stale record is caught anyway.
//  4. Assign waves from artifact depth, so blobs land before the manifests
//     that reference them and the top-level index lands last.
//  5. Insert. Wave 0 pending, the rest blocked.
//
// IDEMPOTENT. A Coordinator that dies mid-plan leaves a partial job set; on
// restart the transfer is still `planning` and this runs again, with ON
// CONFLICT DO NOTHING making the rows that exist free.
func (p *Planner) Plan(ctx context.Context, req Request) (Plan, error) {
	if req.TransferID == "" {
		return Plan{}, errors.New("plan: transfer ID is required")
	}

	root, err := rootDescriptor(req.Package)
	if err != nil {
		return Plan{}, err
	}

	tree, walked, err := p.treeFor(ctx, req, root)
	if err != nil {
		return Plan{}, err
	}

	plan := Plan{TransferID: req.TransferID, Walked: walked}

	// Distinct blobs, classified once. A package that references the same base
	// layer from five images yields ONE blob and therefore one job — which is
	// the whole reason the unit of work is a blob and not an image.
	blobs := distinctBlobs(tree)

	placed, err := p.packages.PlacedDigests(ctx, req.TargetRepoID, digestsOf(blobs))
	if err != nil {
		return Plan{}, err
	}

	tx, err := p.packages.DB().BeginTx(ctx, nil)
	if err != nil {
		return Plan{}, fmt.Errorf("begin plan transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, b := range blobs {
		if placed[b.Digest] {
			// Already at the destination and within its TTL. No job at all —
			// not a job that will be skipped, which would still cost a lease, a
			// round trip and a row.
			plan.SkippedBlobs++
			plan.DedupeSkippedBytes += b.SizeBytes
			continue
		}

		created, err := p.packages.InsertJob(ctx, tx, store.JobRow{
			TransferID:   req.TransferID,
			Kind:         "blob",
			Digest:       b.Digest,
			SizeBytes:    b.SizeBytes,
			MediaType:    b.MediaType,
			SourceRepoID: req.SourceRepoID,
			TargetRepoID: req.TargetRepoID,
			// Wave 0: everything a manifest could reference must exist first.
			Wave:     0,
			State:    "pending",
			Priority: req.Priority,
		})
		if err != nil {
			return Plan{}, err
		}
		if created {
			plan.Jobs++
			plan.Blobs++
			plan.PlannedBytes += b.SizeBytes
		}
	}

	// Manifests, deepest first. An artifact's wave is 1 + its distance from the
	// deepest leaf, so a child manifest is pushed before the index naming it
	// and the registry never sees a reference to something absent.
	maxDepth := 0
	for _, a := range tree.Artifacts {
		if a.Row.Depth > maxDepth {
			maxDepth = a.Row.Depth
		}
	}

	for _, a := range tree.Artifacts {
		wave := maxDepth - a.Row.Depth + 1
		if wave > plan.MaxWave {
			plan.MaxWave = wave
		}

		created, err := p.packages.InsertJob(ctx, tx, store.JobRow{
			TransferID:   req.TransferID,
			Kind:         "manifest",
			Digest:       a.Row.Digest,
			SizeBytes:    a.Row.SizeBytes,
			MediaType:    a.Row.MediaType,
			ArtifactID:   &a.Row.ID,
			SourceRepoID: req.SourceRepoID,
			TargetRepoID: req.TargetRepoID,
			Wave:         wave,
			// BLOCKED until its wave opens. This is what invariant I1 rests on:
			// a tag never appears at the destination until everything under it
			// is present, so an interrupted transfer leaves a consumer seeing
			// the old tag or the complete new one — never a half-written one.
			State:    "blocked",
			Priority: req.Priority,
		})
		if err != nil {
			return Plan{}, err
		}
		if created {
			plan.Jobs++
			plan.Manifests++
			plan.PlannedBytes += a.Row.SizeBytes
		}
	}

	if err := p.packages.RecordPlan(ctx, tx, req.TransferID, store.PlanTotals{
		JobCount:           plan.Jobs,
		PlannedBytes:       plan.PlannedBytes,
		DedupeSkippedBytes: plan.DedupeSkippedBytes,
		MaxWave:            plan.MaxWave,
	}); err != nil {
		return Plan{}, err
	}

	if err := tx.Commit(); err != nil {
		return Plan{}, fmt.Errorf("commit plan: %w", err)
	}

	p.log.InfoContext(ctx, "planned transfer",
		"transfer", req.TransferID,
		"package", req.Package.Tag,
		"root", root.Digest.Short(),
		"jobs", plan.Jobs,
		"blobs", plan.Blobs,
		"manifests", plan.Manifests,
		"bytes", plan.PlannedBytes,
		"dedupeSkippedBytes", plan.DedupeSkippedBytes,
		"maxWave", plan.MaxWave,
		"walked", plan.Walked,
	)
	return plan, nil
}

// treeFor returns the package's full tree, walking only when it must.
//
// THE WALK HAPPENS ONCE, and it is the same walk `packages inspect` performs —
// literally, via internal/expand. If somebody already inspected this package
// the record holds the tree and the registry is not touched; if not, this walks
// and RECORDS it, so a later inspect is free and the sizes an estimate needs
// are present either way.
//
// It also marks the package's cached manifest bodies as recently used. That is
// what makes the cache's LRU eviction rank by USE rather than by curiosity: a
// transfer is the only thing that will actually push these bytes, so a transfer
// touching them is the signal worth keeping them for.
func (p *Planner) treeFor(
	ctx context.Context, req Request, root registry.Descriptor,
) (store.ExpandedTree, int, error) {
	out, err := expand.Ensure(ctx, p.packages, req.Package.ID, root, req.Source, p.concurrency)
	if err != nil {
		return store.ExpandedTree{}, 0, err
	}

	// Best effort: a failure to bump an LRU stamp costs a re-fetch at worst and
	// must not fail a plan that is otherwise complete.
	if err := p.packages.TouchManifestCache(ctx, req.Package.ID); err != nil {
		p.log.WarnContext(ctx, "could not refresh the manifest cache stamp",
			"package", req.Package.ID, "error", err)
	}
	return out.Tree, out.Fetched, nil
}

// rootDescriptor is what the planner walks, which is not always the package's
// own manifest.
//
// Where a vendor bundles the payload with its signature under a wrapper index,
// only the wrapper reaches both — so planning from the payload alone would move
// the bytes and LEAVE THE SIGNATURE BEHIND. The layout plugin recorded the
// wrapper at discovery; this is where that decision is used.
func rootDescriptor(pkg store.PackageRow) (registry.Descriptor, error) {
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

// distinctBlobs collects every blob the tree references, each digest once.
//
// DISTINCT is the point. A fat index whose platforms share a base layer must
// transfer that layer once, not once per platform — and a plan that counted it
// per reference would overstate the transfer, sometimes several times over, in
// every number an operator reads.
func distinctBlobs(t store.ExpandedTree) []store.BlobRef {
	seen := map[string]bool{}
	out := make([]store.BlobRef, 0, len(t.Artifacts))

	for _, a := range t.Artifacts {
		for _, b := range a.Blobs {
			if seen[b.Digest] {
				continue
			}
			seen[b.Digest] = true
			out = append(out, b)
		}
	}
	return out
}

func digestsOf(blobs []store.BlobRef) []string {
	out := make([]string, 0, len(blobs))
	for _, b := range blobs {
		out = append(out, b.Digest)
	}
	return out
}
