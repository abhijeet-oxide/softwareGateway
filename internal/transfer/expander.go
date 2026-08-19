package transfer

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/vendors"
)

// The expander is the link between a request and the queue.
//
// Discovery's auto-download rules - and, later, the API - write
// `transfer_requests` rows. Nothing else turns one into work. This does: for
// each request it opens one transfer per destination, plans it, and marks the
// request expanded.
//
// It runs ON THE LEADER, as a tick. Running it on every replica would be
// harmless - every step is idempotent, by unique constraint rather than by
// application logic - but it would multiply registry walks for no benefit.
//
// See docs/design/04 §10 and docs/design/05 §3.

// Resolver supplies what the expander cannot know on its own.
//
// A consumer-defined interface: the expander needs two answers, not the
// product loader, the secret resolver and the client factory that produce them
// (docs/design/15 §6). It is also what keeps this package free of
// internal/product, so a transfer can be expanded in a test with two closures.
type Resolver interface {
	// Reader builds a client for a repository row, used for the manifest walk
	// when the package has not been expanded already.
	//
	// The row may be a SOURCE or a TARGET: a promotion reads from the target it
	// is promoting out of, and the engine has never cared about roles. The row
	// supplies the registry and the credential; the path is passed separately
	// because a promotion reads from BENEATH its target's configured prefix.
	Reader(ctx context.Context, repoID int64, repositoryPath string) (registry.ManifestReader, error)

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

	// delegation and replication are the strategy seam (delegate.go). Both nil
	// means every destination is a copy, which is what every deployment before
	// M8 was and what an estate with no Quay targets still is.
	delegation  Delegation
	replication *store.Replication

	// batch bounds one tick, so a backlog of a thousand requests does not hold
	// the leader in one loop for minutes.
	batch int
}

// WithDelegation attaches the delegated half of the strategy seam.
//
// A builder rather than a constructor argument, because it is genuinely
// optional and adding it to NewExpander would make every existing caller and
// every test pass two nils to say "unchanged".
func (e *Expander) WithDelegation(d Delegation, st *store.Replication) *Expander {
	e.delegation = d
	e.replication = st
	return e
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

// Expand plans one batch of unplanned transfers.
func (e *Expander) Expand(ctx context.Context) (ExpandResult, error) {
	var res ExpandResult

	pending, err := e.packages.PendingTransfers(ctx, e.batch)
	if err != nil {
		return res, err
	}

	requests := map[string]bool{}
	for _, t := range pending {
		res.Transfers++
		requests[t.RequestID] = true

		jobs, err := e.plan(ctx, t)
		if err != nil {
			// One transfer's failure must not stop the rest. An unreachable
			// origin, a package whose tree cannot be walked - each is local to
			// that transfer, and stopping the tick would let one broken
			// product stall every other product's work.
			res.Failed++
			e.log.ErrorContext(ctx, "could not plan transfer",
				"transfer", t.ID, "product", t.ProductName, "error", err)
			if err := e.packages.FailTransfer(ctx, t.ID, err.Error()); err != nil {
				e.log.ErrorContext(ctx, "could not record the planning failure",
					"transfer", t.ID, "error", err)
			}
			continue
		}
		res.Jobs += jobs
	}

	// A request is expanded once every transfer it opened has been planned.
	// Recorded separately because one request can produce several, and the
	// request is not done until the last of them is.
	for id := range requests {
		res.Requests++
		if err := e.packages.SettleRequest(ctx, id); err != nil {
			e.log.WarnContext(ctx, "could not settle the request", "request", id, "error", err)
		}
	}
	return res, nil
}

// plan turns one open transfer into jobs.
//
// The transfer row already names its origin and destination - recorded when
// the request was made - so nothing here consults configuration to decide
// WHERE. It decides only what has to move, which is what planning is.
func (e *Expander) plan(ctx context.Context, t store.PendingTransfer) (int, error) {
	pkg, err := e.packages.GetPackageByID(ctx, t.PackageID)
	if err != nil {
		return 0, err
	}

	endpoints, err := e.packages.HydrateEndpoints(ctx, []int64{t.SourceRepoID, t.TargetRepoID})
	if err != nil {
		return 0, err
	}
	origin, ok := endpoints[t.SourceRepoID]
	if !ok {
		return 0, fmt.Errorf("origin repository %d is not in the catalog", t.SourceRepoID)
	}
	destination, ok := endpoints[t.TargetRepoID]
	if !ok {
		return 0, fmt.Errorf("destination repository %d is not in the catalog", t.TargetRepoID)
	}

	// The vendor's own repository path is the canonical relative layout, and
	// it survives every hop: a promotion reproduces the same structure under
	// its destination's prefix that replication put under lab's.
	relative, err := e.relativePath(ctx, pkg, origin)
	if err != nil {
		return 0, err
	}

	// Where the bytes actually are. For a replication that is the vendor's
	// repository. For a promotion it is that path NESTED under the origin
	// target's prefix, because that is where the previous hop put it - reading
	// the prefix itself would find an empty repository.
	readPath := relative
	if t.Operation == "promote" {
		readPath = joinPath(origin.Repository, relative)
	}

	// THE STRATEGY BRANCH.
	//
	// Asked before anything is read from a registry, because a delegated
	// destination needs none of it: no manifest walk, no related-artifact
	// lookup, no plan. Asking later would cost a vendor round trip to produce
	// a plan that is then thrown away.
	if strategy, err := e.strategyFor(ctx, t.ProductName, destination.Name); err != nil {
		return 0, err
	} else if strategy != StrategyCopy {
		return 0, e.delegate(ctx, t, strategy, destination.Name, pkg)
	}

	originRepoID, err := e.ensureOrigin(ctx, t, origin, readPath)
	if err != nil {
		return 0, err
	}

	reader, err := e.resolve.Reader(ctx, t.SourceRepoID, readPath)
	if err != nil {
		return 0, err
	}

	related, err := e.resolve.Related(ctx, t.ProductName, pkg)
	if err != nil {
		// Not fatal. A package whose accessories could not be listed still has
		// a payload worth moving, and failing the whole transfer over a
		// signature lookup would be the wrong trade - the signature's absence
		// is recorded on the package and visible there.
		e.log.WarnContext(ctx, "could not list the package's related artifacts",
			"package", pkg.ID, "error", err)
	}

	plan, err := e.planner.Plan(ctx, Request{
		TransferID:   t.ID,
		RequestID:    t.RequestID,
		Package:      pkg,
		SourceRepoID: originRepoID,
		TargetRepoID: t.TargetRepoID,

		ProductID:          t.ProductID,
		TargetName:         destination.Name,
		TargetRegistry:     destination.Registry,
		TargetRegistryType: destination.RegistryType,
		TargetBasePath:     destination.Repository,
		SourceRepository:   relative,

		Priority: t.Priority,
		Source:   reader,
		Related:  related,
	})
	if err != nil {
		return 0, err
	}
	return plan.Jobs, nil
}

// strategyFor asks how content reaches a destination. No delegation
// configured means copy, which is the answer for every target in an estate
// with no delegated ones.
func (e *Expander) strategyFor(ctx context.Context, productName, targetName string) (Strategy, error) {
	if e.delegation == nil {
		return StrategyCopy, nil
	}
	return e.delegation.Strategy(ctx, productName, targetName)
}

// relativePath is the vendor repository a package was published in.
//
// Taken from the PACKAGE's own source row rather than from the transfer's
// origin, and that distinction is the whole of how promotion preserves layout:
// the origin of a promotion is a target whose path is a prefix we added, while
// the package still knows the path the vendor used. Reproducing the vendor's
// path under each destination's prefix is what makes lab and production hold
// the same structure rather than production holding lab's prefix twice.
func (e *Expander) relativePath(
	ctx context.Context, pkg store.PackageRow, origin store.Endpoint,
) (string, error) {
	endpoints, err := e.packages.HydrateEndpoints(ctx, []int64{pkg.SourceRepoID})
	if err != nil {
		return "", err
	}
	if src, ok := endpoints[pkg.SourceRepoID]; ok && src.Repository != "" {
		return src.Repository, nil
	}
	// The vendor source has been removed from configuration. The origin's own
	// path is the best remaining answer, and for a replication it is exactly
	// right.
	return origin.Repository, nil
}

// ensureOrigin gives the path being read from a catalog row.
//
// A replication reads from the vendor repository, which already has one. A
// promotion reads from a path beneath its target's prefix, which does not -
// and jobs carry repository IDs rather than paths, so one has to exist before
// a job can name it.
func (e *Expander) ensureOrigin(
	ctx context.Context, t store.PendingTransfer, origin store.Endpoint, readPath string,
) (int64, error) {
	if readPath == origin.Repository {
		return t.SourceRepoID, nil
	}

	tx, err := e.packages.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin origin registration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Named "<configured>/<path>" like every other row one configured entry
	// owns, so the worker resolves the same credential for it - see
	// regclient.endpointSpec.
	id, err := e.packages.EnsureRepository(ctx, tx, t.ProductID, origin.Role,
		origin.Name+"/"+readPath, origin.Registry, readPath,
		origin.RegistryType, "discovery", "")
	if err != nil {
		return 0, fmt.Errorf("register origin %q: %w", readPath, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit origin registration: %w", err)
	}
	return id, nil
}

// joinPath nests one repository path under another.
func joinPath(base, rest string) string {
	base = strings.Trim(strings.TrimSpace(base), "/")
	rest = strings.Trim(strings.TrimSpace(rest), "/")
	switch {
	case base == "":
		return rest
	case rest == "":
		return base
	default:
		return base + "/" + rest
	}
}
