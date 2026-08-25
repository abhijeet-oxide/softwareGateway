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

	// promotion and promotions are the OTHER half of that seam
	// (promotion.go): a hop the registry the two targets share can carry out
	// itself. Both nil means every promotion is a copy, which is always
	// correct and is what an estate with no such pair still does.
	promotion  Promotion
	promotions *store.Promotions

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

	// THE PROMOTION BRANCH, and it is deliberately after the one above.
	//
	// A destination that fetches for itself has already answered the question
	// this asks: it does not want the content pushed at it AT ALL, by us or by
	// its own registry's copy endpoint. Asking here first would let a native
	// promotion write into a mirror that is meant to be read-only.
	//
	// Asked before anything is read from a registry, for the same reason the
	// delegated branch is: a claimed hop needs no manifest walk, and producing
	// one to throw it away costs a round trip per artifact.
	if t.Operation == "promote" && e.promotion != nil {
		hop, ok, err := e.promotionHop(ctx, t, pkg, origin, destination, relative)
		if err != nil {
			return 0, err
		}
		if ok {
			if claim := e.claimPromotion(ctx, hop); claim.Claimed {
				return 0, e.relocate(ctx, t, claim, hop)
			} else if claim.Reason != "" {
				// Said out loud, once, at the moment the slow path is chosen.
				// "Why did promoting to production take forty minutes when it
				// usually takes six seconds" is otherwise unanswerable after
				// the fact.
				e.log.InfoContext(ctx, "promoting by copy",
					"transfer", t.ID, "from", origin.Name, "to", destination.Name,
					"reason", claim.Reason)
			}
		}
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

// promotionHop describes a promotion in terms of the NAMES it publishes.
//
// ok is false when the hop cannot be described, and that is a fall-through
// rather than a failure: the copy path derives the same names for itself by
// walking the registry, so anything this cannot answer, that can.
//
// The one case that matters is a package nobody has analysed. Its tree is
// recorded as far as discovery got - the root manifest and a list of what it
// references, unfetched - so the names underneath it are simply not known
// here. Claiming on a partial tree would promote the root and quietly leave a
// bundle's components behind, which is worse than being slow.
func (e *Expander) promotionHop(
	ctx context.Context, t store.PendingTransfer, pkg store.PackageRow,
	origin, destination store.Endpoint, relative string,
) (PromotionHop, bool, error) {
	tree, complete, err := e.packages.ReadExpandedTree(ctx, pkg.ID)
	if err != nil {
		return PromotionHop{}, false, err
	}
	if !complete || len(tree.Artifacts) == 0 {
		e.log.InfoContext(ctx, "promoting by copy: the release has not been analysed, "+
			"so the names underneath it are not known here",
			"transfer", t.ID, "package", pkg.ID)
		return PromotionHop{}, false, nil
	}

	related, err := e.resolve.Related(ctx, t.ProductName, pkg)
	if err != nil {
		// Same trade the copy path makes: a release whose accessories could
		// not be listed still has a payload worth moving.
		e.log.WarnContext(ctx, "could not list the package's related artifacts",
			"package", pkg.ID, "error", err)
	}

	hop := PromotionHop{
		TransferID:     t.ID,
		ProductName:    t.ProductName,
		Origin:         origin.Name,
		Destination:    destination.Name,
		Package:        pkg.Tag,
		ManifestDigest: pkg.ManifestDigest,
		Names: PromotionNamesFor(tree, relative,
			rootTags(Request{Package: pkg, Related: related})),
	}
	if len(hop.Names) == 0 {
		return PromotionHop{}, false, nil
	}
	return hop, true, nil
}

// PromotionNamesFor is every TAGGED site of a tree, root first.
//
// Exported because the API answers the same question BEFORE anybody commits to
// anything - the promotion dialog says how a hop would be carried out, and an
// answer derived differently from the one the expander will reach is worse
// than no answer at all.
//
// Tagged only, and the omission is the whole difference between the two paths.
// Our engine moves CONTENT, so it has to visit every manifest and every blob
// including the ones nothing names - an index's per-platform children are
// reached by digest and carry no tag of their own. A registry relocating
// within itself already holds all of that; what it has to be told is which
// NAMES to publish, and a name is a repository and a tag.
//
// Document order, which ResolveLayout preserves from the tree, so the root -
// the name that makes the release resolvable at all - is published first and a
// promotion interrupted half way through has left a consistent prefix rather
// than an arbitrary subset.
func PromotionNamesFor(
	tree store.ExpandedTree, sourceRepository string, rootTags []string,
) []PromotionName {
	// BASE PATH EMPTY, on purpose. The sites that come back are then RELATIVE
	// to whatever prefix an end configures, which is exactly what a hop needs:
	// the same value re-bases under lab's prefix to say where to read and
	// under production's to say where to write. Passing either end's base
	// here would bake one of them into both.
	layout := ResolveLayout(tree, LayoutOptions{
		SourceRepository: sourceRepository,
		RootTags:         rootTags,
	})

	var out []PromotionName
	seen := map[string]bool{}

	for _, a := range tree.Artifacts {
		placement, ok := layout[a.Row.Digest]
		if !ok {
			continue
		}
		for _, site := range placement.Sites {
			for _, tag := range site.Tags {
				key := site.Repository + ":" + tag
				if seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, PromotionName{
					Repository: site.Repository, Tag: tag, Digest: a.Row.Digest,
				})
			}
		}
	}
	return out
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
