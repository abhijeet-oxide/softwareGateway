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
	"database/sql"
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
	// Blobs and Manifests break that down. Blobs is the interesting number -
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

	// Dependencies is how many edges were written - how many "this manifest
	// needs that blob" facts the transfer will be scheduled against.
	//
	// Worth reporting because it is the only evidence that per-artifact
	// readiness is actually wired up. A plan with manifests and zero
	// dependencies is one where every manifest will be pushed immediately,
	// which is the failure mode this replaced wave gating to avoid.
	Dependencies int

	// Walked reports that the planner had to read the registry because the
	// package had not been expanded. Zero means the record already held the
	// tree - see store.ReadExpandedTree.
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
	// SourceRepoID is the catalog row the package was discovered in. The job
	// carries row IDs rather than names, so a worker resolving one needs no
	// configuration of its own.
	SourceRepoID int64
	// TargetRepoID is the target's own catalog row - the destination the
	// operator configured. It is the FALLBACK, not the answer: a bundle's
	// components each land in their own repository, resolved from the source's
	// structure by ResolveLayout, and each of those gets a catalog row of its
	// own.
	TargetRepoID int64

	// ProductID owns any destination repository row this plan has to create.
	ProductID int64
	// TargetName is the configured target's name, carried onto every
	// destination row so the worker resolves the same credential for all of
	// them.
	TargetName string
	// TargetRegistry is the destination registry host.
	TargetRegistry string
	// TargetRegistryType selects the backend for destinations created here.
	TargetRegistryType string
	// TargetBasePath is the target's configured `repository`, used as a PREFIX
	// under which the source structure is reproduced. Empty mirrors the source
	// paths at the destination registry's root.
	TargetBasePath string
	// SourceRepository is the path the package was discovered in - what every
	// destination path is derived from.
	SourceRepository string

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
//     it is not - and recording it either way, so the next caller is free.
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

	root, err := expand.Root(req.Package)
	if err != nil {
		return Plan{}, err
	}

	tree, walked, err := p.treeFor(ctx, req, root)
	if err != nil {
		return Plan{}, err
	}

	plan := Plan{TransferID: req.TransferID, Walked: walked}

	// WHERE each artifact lands, derived from the source's own structure.
	//
	// A bundle is not one artifact in one repository: its components carry
	// their own repository paths and tags in `org.opencontainers.image.ref.name`,
	// and copying them into one flat destination would preserve every byte
	// while destroying every name.
	layout := ResolveLayout(tree, LayoutOptions{
		BasePath:         req.TargetBasePath,
		SourceRepository: req.SourceRepository,
		RootTags:         rootTags(req),
	})

	tx, err := p.packages.DB().BeginTx(ctx, nil)
	if err != nil {
		return Plan{}, fmt.Errorf("begin plan transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Every distinct destination path gets a catalog row, so placements,
	// deduplication and cross-repository mount all keep working unchanged -
	// they are all keyed by repository ID, and a destination without a row
	// would be invisible to each of them.
	destinations, err := p.ensureDestinations(ctx, tx, req, layout)
	if err != nil {
		return Plan{}, err
	}

	// Blobs first, because a manifest's STATE depends on them.
	//
	// This used to run the other way round, and the order was arbitrary then:
	// every manifest was created `blocked` regardless. It is not arbitrary now.
	// A manifest is created runnable when it has nothing to wait for - which is
	// the ordinary outcome for the second transfer of a product line, where
	// every blob is already at the destination - and answering that needs the
	// blob set to be known first.
	//
	// One job per (digest, destination repository). The pair matters: a
	// registry stores blobs per repository, so a base layer shared by two
	// components that land in different destinations has to be pushed to both.
	// Within one destination it is still exactly one job, which is what makes a
	// fat index share its base layer rather than move it per platform.
	blobs, err := p.blobTargets(ctx, tx, tree, layout, destinations)
	if err != nil {
		return Plan{}, err
	}

	// jobIDs maps a (kind, repository, digest) coordinate onto the row that
	// will move it, so the dependency edges can be written by name rather than
	// by hoping two loops stay in step.
	jobIDs := make(map[jobKey]int64, len(blobs)+len(tree.Artifacts))

	for _, b := range blobs {
		if b.placed {
			// Already at that destination and within its TTL. No job at all -
			// not a job that will be skipped, which would still cost a lease, a
			// round trip and a row. Deliberately absent from jobIDs: a manifest
			// must not wait on work that does not exist.
			plan.SkippedBlobs++
			plan.DedupeSkippedBytes += b.ref.SizeBytes
			continue
		}

		id, created, err := p.packages.InsertJob(ctx, tx, store.JobRow{
			TransferID:       req.TransferID,
			Kind:             "blob",
			Digest:           b.ref.Digest,
			SizeBytes:        b.ref.SizeBytes,
			MediaType:        b.ref.MediaType,
			SourceRepoID:     req.SourceRepoID,
			TargetRepoID:     b.targetID,
			TargetRepository: b.repository,
			// Wave 0 still: blobs have nothing to wait for, and the wave column
			// remains what the dequeue and the progress rollup order by.
			Wave:     0,
			State:    "pending",
			SiteRank: b.siteRank,
			Priority: req.Priority,
		})
		if err != nil {
			return Plan{}, err
		}
		jobIDs[jobKey{kind: "blob", repository: b.repository, digest: b.ref.Digest}] = id
		if created {
			plan.Jobs++
			plan.Blobs++
			plan.PlannedBytes += b.ref.SizeBytes
		}
	}

	// Manifests, deepest first: an artifact's wave is 1 + its distance from the
	// deepest leaf, so a child manifest is pushed before the index naming it and
	// the registry never sees a reference to something absent.
	//
	// The wave still orders. What it no longer does is GATE: each manifest
	// carries explicit edges to the blobs and child manifests it references, and
	// is promoted the moment those are done rather than when its whole wave is.
	maxDepth := 0
	for _, a := range tree.Artifacts {
		if a.Row.Depth > maxDepth {
			maxDepth = a.Row.Depth
		}
	}

	var edges []store.JobEdge
	children := childIndex(tree)

	// BACKWARDS through the tree, because the tree is ordered parents before
	// children and an edge has to name a row that exists. A parent depends on
	// its children, so the children's jobs must be inserted first - which is
	// the reverse of the order that makes inheritance work in ResolveLayout,
	// and the reason this loop cannot simply be merged with that one.
	for i := len(tree.Artifacts) - 1; i >= 0; i-- {
		a := tree.Artifacts[i]
		wave := maxDepth - a.Row.Depth + 1
		if wave > plan.MaxWave {
			plan.MaxWave = wave
		}

		// One job per SITE. A component that also answers to a name of its
		// own is pushed twice - once into the repository that keeps the bundle
		// resolvable, once under its own name - because a registry resolves a
		// manifest only within the repository it was pushed to.
		for rank, site := range layout[a.Row.Digest].Sites {
			targetID, ok := destinations[site.Repository]
			if !ok {
				return Plan{}, fmt.Errorf("no destination row for %q", site.Repository)
			}

			needs := p.dependenciesOf(tree, i, children, site.Repository, jobIDs)

			// BLOCKED when it has something to wait for, PENDING when it does
			// not. This is invariant I1 stated per manifest: a tag never
			// appears at the destination until everything under it is present,
			// so an interrupted transfer leaves a consumer seeing the old tag
			// or the complete new one - never a half-written one. A manifest
			// with no outstanding content satisfies that on creation, and
			// blocking it would be waiting for an event that cannot come.
			state := "blocked"
			if len(needs) == 0 {
				state = "pending"
			}

			id, created, err := p.packages.InsertJob(ctx, tx, store.JobRow{
				TransferID:       req.TransferID,
				Kind:             "manifest",
				Digest:           a.Row.Digest,
				SizeBytes:        a.Row.SizeBytes,
				MediaType:        a.Row.MediaType,
				ArtifactID:       &a.Row.ID,
				SourceRepoID:     req.SourceRepoID,
				TargetRepoID:     targetID,
				TargetRepository: site.Repository,
				TargetTags:       site.Tags,
				Wave:             wave,
				State:            state,
				SiteRank:         rank,
				Priority:         req.Priority,
			})
			if err != nil {
				return Plan{}, err
			}
			jobIDs[jobKey{kind: "manifest", repository: site.Repository, digest: a.Row.Digest}] = id

			for _, dep := range needs {
				edges = append(edges, store.JobEdge{JobID: id, DependsOnID: dep})
			}

			if created {
				plan.Jobs++
				plan.Manifests++
				plan.PlannedBytes += a.Row.SizeBytes
			}
		}
	}

	if err := p.packages.AddJobDependencies(ctx, tx, edges); err != nil {
		return Plan{}, err
	}
	plan.Dependencies = len(edges)

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
		"dependencies", plan.Dependencies,
		"bytes", plan.PlannedBytes,
		"dedupeSkippedBytes", plan.DedupeSkippedBytes,
		"maxWave", plan.MaxWave,
		"walked", plan.Walked,
	)
	return plan, nil
}

// treeFor returns the package's full tree, walking only when it must.
//
// THE WALK HAPPENS ONCE, and it is the same walk `packages inspect` performs -
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

// ensureDestinations gives every distinct destination path a catalog row.
//
// Reusing the repositories table rather than inventing a second place to keep
// destination paths is what makes the rest of the system need no changes:
// blob_placements, the dedupe check, the concurrent-duplicate suppression in
// the dequeue and the cross-repository mount are all keyed by repository ID.
//
// Each row carries the CONFIGURED target's name, not a generated one, so the
// worker resolves the same credential for every destination a bundle spreads
// across - the credential belongs to the target, and the paths beneath it are
// the vendor's structure rather than separate configuration.
func (p *Planner) ensureDestinations(
	ctx context.Context, tx *sql.Tx, req Request, layout map[string]Placement,
) (map[string]int64, error) {
	out := make(map[string]int64)

	for _, place := range layout {
		for _, site := range place.Sites {
			if _, done := out[site.Repository]; done {
				continue
			}

			// The configured target keeps the row the operator declared; only
			// the paths a bundle's structure implies are created here.
			if site.Repository == req.TargetBasePath || site.Repository == "" {
				out[site.Repository] = req.TargetRepoID
				continue
			}

			// The row NAME follows the convention discovery already established
			// for a source covering several repositories: "<configured>/<path>".
			// The (product, role, name) unique constraint forbids reusing the
			// target's own name for every path, and the prefix is what lets the
			// worker resolve ONE credential for all of them - see
			// regclient.endpointSpec.
			id, err := p.packages.EnsureRepository(ctx, tx, req.ProductID, "target",
				req.TargetName+"/"+site.Repository, req.TargetRegistry, site.Repository,
				req.TargetRegistryType, "discovery", "")
			if err != nil {
				return nil, fmt.Errorf("register destination %q: %w", site.Repository, err)
			}
			out[site.Repository] = id
		}
	}
	return out, nil
}

// jobKey identifies the one job that moves a given digest to a given
// destination repository - which is exactly the job table's uniqueness key,
// minus the transfer, because a plan is scoped to one.
type jobKey struct {
	kind       string
	repository string
	digest     string
}

// dependenciesOf lists the jobs an artifact's manifest must wait for at one
// site.
//
// Two kinds, and both are required by the distribution spec rather than by
// preference: a manifest push is rejected BLOB_UNKNOWN if a layer it names is
// absent from the repository, and MANIFEST_UNKNOWN if a child index is. So the
// edge set is not a scheduling heuristic - it is the registry's own
// precondition, written down.
//
// A dependency with no job is not an edge. The usual reason is a blob already
// at the destination, which the planner drops entirely rather than queueing as
// a skip; waiting on it would be waiting for a completion that never arrives.
//
// # The one asymmetry worth knowing about
//
// Blobs follow their manifest to EVERY site, but child manifests do not: a
// child inherits its parent's CONTAINER repository, so an artifact published
// additionally under its own name gets its layers there and not its children.
// Where that happens there is simply no child job at the second site to depend
// on, and this reports the dependencies that exist rather than inventing one.
// The placement question is ResolveLayout's, and this does not second-guess it.
func (p *Planner) dependenciesOf(
	tree store.ExpandedTree, index int, children [][]int, repository string, jobIDs map[jobKey]int64,
) []int64 {
	var out []int64
	seen := map[int64]bool{}

	add := func(kind, digest string) {
		id, ok := jobIDs[jobKey{kind: kind, repository: repository, digest: digest}]
		if !ok || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}

	for _, b := range tree.Artifacts[index].Blobs {
		add("blob", b.Digest)
	}
	for _, child := range children[index] {
		add("manifest", tree.Artifacts[child].Row.Digest)
	}
	return out
}

// childIndex inverts the parent pointers once, so building the edge set is
// linear in the tree rather than quadratic. A bundle of two thousand artifacts
// makes that the difference between a plan and a pause.
func childIndex(tree store.ExpandedTree) [][]int {
	out := make([][]int, len(tree.Artifacts))
	for i, a := range tree.Artifacts {
		if a.Parent >= 0 && a.Parent < len(out) {
			out[a.Parent] = append(out[a.Parent], i)
		}
	}
	return out
}

// blobTarget is one blob bound for one destination repository.
type blobTarget struct {
	ref        store.BlobRef
	repository string
	targetID   int64
	placed     bool
	// siteRank is 0 when some artifact places this blob here to keep a bundle
	// resolvable, and 1 when it is only here because a component is also
	// published under its own name. The dequeue orders by it so the second copy
	// always runs after the first and can mount rather than stream.
	siteRank int
}

// blobTargets pairs every blob with the destinations its manifests land in.
//
// A blob follows the manifest that references it: the registry has no notion of
// a blob existing independently of a repository, so "where does this layer go"
// is answered entirely by "where does the manifest naming it go".
//
// DISTINCT per (digest, destination). A fat index whose platforms share a base
// layer moves it once when they land together, and once per destination when
// they do not - which is not duplication, it is the registry's storage model.
func (p *Planner) blobTargets(
	ctx context.Context,
	tx *sql.Tx,
	tree store.ExpandedTree,
	layout map[string]Placement,
	destinations map[string]int64,
) ([]blobTarget, error) {
	seen := map[string]bool{}
	byDestination := map[string][]store.BlobRef{}
	// rankOf keeps the LOWEST rank any artifact gave this (repository, digest)
	// pair. A blob that some artifact needs in its container repository is a
	// rank-0 job even if another artifact also wants it under a published name:
	// the copy that keeps a bundle resolvable is the one that must not wait.
	rankOf := map[string]int{}
	var order []string

	for _, a := range tree.Artifacts {
		// A blob follows its manifest to EVERY site: a registry stores blobs
		// per repository, so a component published under two names needs its
		// layers under both or the second manifest push fails BLOB_UNKNOWN.
		for rank, site := range layout[a.Row.Digest].Sites {
			for _, b := range a.Blobs {
				key := site.Repository + "|" + b.Digest
				if seen[key] {
					if rank < rankOf[key] {
						rankOf[key] = rank
					}
					continue
				}
				seen[key] = true
				rankOf[key] = rank
				if _, known := byDestination[site.Repository]; !known {
					order = append(order, site.Repository)
				}
				byDestination[site.Repository] = append(byDestination[site.Repository], b)
			}
		}
	}

	var out []blobTarget
	for _, repo := range order {
		refs := byDestination[repo]
		targetID, ok := destinations[repo]
		if !ok {
			return nil, fmt.Errorf("no destination row for %q", repo)
		}

		// One placement lookup per destination rather than per blob: a
		// thousand-blob bundle spread over five repositories costs five
		// queries, which is what keeps planning a database operation rather
		// than a network one.
		placed, err := p.packages.PlacedDigestsTx(ctx, tx, targetID, digestsOf(refs))
		if err != nil {
			return nil, err
		}

		for _, b := range refs {
			out = append(out, blobTarget{
				ref:        b,
				repository: repo,
				targetID:   targetID,
				placed:     placed[b.Digest],
				siteRank:   rankOf[repo+"|"+b.Digest],
			})
		}
	}
	return out, nil
}

// rootTags is what the package's own root manifest must be called.
//
// The tag a person asked for, plus the wrapper tag where a vendor bundles the
// payload with its signature under one: the transfer root IS the wrapper, and
// a consumer expecting the vendor's layout must still find it by that name
// after replication.
func rootTags(req Request) []string {
	tags := []string{req.Package.Tag}
	for _, rel := range req.Related {
		if rel.Role == vendors.RoleWrapper && rel.Tag != "" {
			tags = append(tags, rel.Tag)
		}
	}
	return tags
}

func digestsOf(blobs []store.BlobRef) []string {
	out := make([]string, 0, len(blobs))
	for _, b := range blobs {
		out = append(out, b.Digest)
	}
	return out
}
