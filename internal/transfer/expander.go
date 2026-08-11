package transfer

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/vendors"
)

// The expander is the link between a request and the queue.
//
// Discovery's auto-download rules — and, later, the API — write
// `transfer_requests` rows. Nothing else turns one into work. This does: for
// each request it opens one transfer per destination, plans it, and marks the
// request expanded.
//
// It runs ON THE LEADER, as a tick. Running it on every replica would be
// harmless — every step is idempotent, by unique constraint rather than by
// application logic — but it would multiply registry walks for no benefit.
//
// See docs/design/04 §10 and docs/design/05 §3.

// Destination is one place a request expands into.
//
// Resolved from configuration by the caller, not read from the database: a
// target is a configured place, and the set of them can change between a
// request being written and being expanded. Expanding against current
// configuration is what makes "planning happens at execution time" true
// (docs/design/04 §10).
type Destination struct {
	// RepositoryID is the catalog row for this target as configured.
	RepositoryID int64
	// Name is the configured target name. It travels onto every destination
	// row a bundle's structure implies, so one credential serves all of them.
	Name string
	// Repository is the configured destination path, used as a PREFIX beneath
	// which the source's structure is reproduced. Empty mirrors the source
	// paths at the destination registry's root.
	Repository string
	// Registry is the destination host, and Type selects its backend. Both are
	// needed to register the repositories a bundle spreads across.
	Registry string
	Type     string
}

// Resolver supplies what the expander cannot know on its own.
//
// A consumer-defined interface: the expander needs three answers, not the
// product loader, the secret resolver and the client factory that produce them
// (docs/design/15 §6). It is also what keeps this package free of
// internal/product, so a request can be expanded in a test with three closures.
type Resolver interface {
	// Destinations returns where a request should go. An empty result is not
	// an error — a product with no enabled targets has nowhere to replicate
	// to, which is a configuration state rather than a failure.
	Destinations(ctx context.Context, productName string, operation string) ([]Destination, error)

	// SourceReader builds a client for the request's source repository, used
	// for the manifest walk when the package has not been expanded already.
	SourceReader(ctx context.Context, productName string, repoID int64) (registry.ManifestReader, error)

	// Related returns the package's signature, SBOM and wrapper artifacts, so
	// a transfer that moves the payload does not leave the signature behind.
	Related(ctx context.Context, productName string, pkg store.PackageRow) ([]vendors.Related, error)
}

// Expander turns pending requests into planned transfers.
type Expander struct {
	packages *store.Packages
	planner  *Planner
	resolve  Resolver
	log      *slog.Logger

	// batch bounds one tick, so a backlog of a thousand requests does not hold
	// the leader in one loop for minutes.
	batch int
}

// NewExpander builds an expander.
func NewExpander(
	packages *store.Packages, planner *Planner, resolve Resolver, batch int, log *slog.Logger,
) *Expander {
	if log == nil {
		log = slog.Default()
	}
	if batch <= 0 {
		batch = 25
	}
	return &Expander{packages: packages, planner: planner, resolve: resolve, batch: batch, log: log}
}

// ExpandResult reports one tick's work.
type ExpandResult struct {
	Requests  int
	Transfers int
	Jobs      int
	Failed    int
}

// Expand processes one batch of pending requests.
func (e *Expander) Expand(ctx context.Context) (ExpandResult, error) {
	var res ExpandResult

	pending, err := e.packages.PendingRequests(ctx, e.batch)
	if err != nil {
		return res, err
	}

	for _, req := range pending {
		res.Requests++

		transfers, jobs, err := e.expandOne(ctx, req)
		if err != nil {
			// One request's failure must not stop the rest. A missing target,
			// an unreachable source, a package whose tree cannot be walked —
			// each is local to that request, and stopping the tick would let
			// one broken product stall every other product's transfers.
			res.Failed++
			e.log.ErrorContext(ctx, "could not expand transfer request",
				"request", req.ID, "product", req.ProductName, "error", err)
			if err := e.packages.SetRequestState(ctx, req.ID, "failed"); err != nil {
				e.log.ErrorContext(ctx, "could not mark the request failed",
					"request", req.ID, "error", err)
			}
			continue
		}
		res.Transfers += transfers
		res.Jobs += jobs
	}
	return res, nil
}

// expandOne opens and plans every transfer for one request.
func (e *Expander) expandOne(ctx context.Context, req store.PendingRequest) (int, int, error) {
	destinations, err := e.resolve.Destinations(ctx, req.ProductName, req.Operation)
	if err != nil {
		return 0, 0, err
	}
	if len(destinations) == 0 {
		return 0, 0, fmt.Errorf("product %q has no enabled target for a %s request",
			req.ProductName, req.Operation)
	}

	pkg, err := e.packages.GetPackageByID(ctx, req.PackageID)
	if err != nil {
		return 0, 0, err
	}

	source, err := e.resolve.SourceReader(ctx, req.ProductName, req.SourceRepoID)
	if err != nil {
		return 0, 0, err
	}

	related, err := e.resolve.Related(ctx, req.ProductName, pkg)
	if err != nil {
		// Not fatal. A package whose accessories could not be listed still has
		// a payload worth moving, and failing the whole transfer over a
		// signature lookup would be the wrong trade — the signature's absence
		// is recorded on the package and visible there.
		e.log.WarnContext(ctx, "could not list the package's related artifacts",
			"package", pkg.ID, "error", err)
	}

	// The path the package was discovered in: every destination path is
	// derived from it, so a bundle's structure is reproduced relative to where
	// the vendor published it rather than invented.
	sourceRepository, err := e.sourceRepositoryPath(ctx, req.SourceRepoID)
	if err != nil {
		return 0, 0, err
	}

	var transfers, jobs int
	for _, dst := range destinations {
		n, err := e.expandTo(ctx, req, pkg, source, sourceRepository, related, dst)
		if err != nil {
			return transfers, jobs, err
		}
		transfers++
		jobs += n
	}

	if err := e.packages.SetRequestState(ctx, req.ID, "expanded"); err != nil {
		return transfers, jobs, err
	}
	return transfers, jobs, nil
}

// expandTo opens one transfer and plans it.
//
// The transfer row is created FIRST and separately from the plan, so a
// Coordinator that dies between the two leaves a transfer in `planning` that
// the next tick finds and re-plans. Planning is idempotent by unique
// constraint, so the re-plan costs nothing for the jobs that already exist.
func (e *Expander) expandTo(
	ctx context.Context,
	req store.PendingRequest,
	pkg store.PackageRow,
	source registry.ManifestReader,
	sourceRepository string,
	related []vendors.Related,
	dst Destination,
) (int, error) {
	transferID, err := e.openTransfer(ctx, req, dst)
	if err != nil {
		return 0, err
	}

	plan, err := e.planner.Plan(ctx, Request{
		TransferID:   transferID,
		RequestID:    req.ID,
		Package:      pkg,
		SourceRepoID: req.SourceRepoID,
		TargetRepoID: dst.RepositoryID,

		ProductID:          req.ProductID,
		TargetName:         dst.Name,
		TargetRegistry:     dst.Registry,
		TargetRegistryType: dst.Type,
		TargetBasePath:     dst.Repository,
		SourceRepository:   sourceRepository,

		Priority: req.Priority,
		Source:   source,
		Related:  related,
	})
	if err != nil {
		// A transfer that cannot be planned must not sit in `planning`
		// forever: the next tick would re-plan it, fail the same way, and do
		// so until somebody reads the logs. Naming the reason on the row is
		// what makes `transfers describe` able to answer "why is this stuck".
		if markErr := e.packages.FailTransfer(ctx, transferID, err.Error()); markErr != nil {
			e.log.ErrorContext(ctx, "could not record the planning failure",
				"transfer", transferID, "error", markErr)
		}
		return 0, fmt.Errorf("plan transfer to %s: %w", dst.Name, err)
	}
	return plan.Jobs, nil
}

// openTransfer creates the transfer row, or finds the one already there.
//
// UNIQUE (request_id, target_repo_id) is what makes this idempotent: a request
// expanded twice produces one transfer per target, not two. The conflict path
// is the normal path on a retry, not an error.
func (e *Expander) openTransfer(
	ctx context.Context, req store.PendingRequest, dst Destination,
) (string, error) {
	tx, err := e.packages.DB().BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin transfer creation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	id := uuid.NewString()
	created, err := e.packages.CreateTransfer(ctx, tx, store.TransferRow{
		ID:           id,
		RequestID:    req.ID,
		PackageID:    req.PackageID,
		SourceRepoID: req.SourceRepoID,
		TargetRepoID: dst.RepositoryID,
		Priority:     req.Priority,
	})
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit transfer creation: %w", err)
	}

	if created {
		return id, nil
	}

	// Already there from an earlier attempt. Use the existing row: creating a
	// second would violate the unique constraint and, worse, would double the
	// work if it somehow did not.
	existing, err := e.packages.TransferIDFor(ctx, req.ID, dst.RepositoryID)
	if err != nil {
		return "", err
	}
	return existing, nil
}

// sourceRepositoryPath reads the path a package was discovered in.
//
// Read from the catalog rather than carried on the request, because the
// request names a repository ROW and the path is that row's business — and
// because a source whose path was edited in configuration must expand against
// what the catalog now says rather than what a request recorded earlier.
func (e *Expander) sourceRepositoryPath(ctx context.Context, repoID int64) (string, error) {
	endpoints, err := e.packages.HydrateEndpoints(ctx, []int64{repoID})
	if err != nil {
		return "", err
	}
	ep, ok := endpoints[repoID]
	if !ok {
		return "", fmt.Errorf("source repository %d is not in the catalog", repoID)
	}
	return ep.Repository, nil
}
