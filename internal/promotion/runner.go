package promotion

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/abhijeet-oxide/softwareGateway/internal/promote"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/transfer"
)

// The runner carries out promotions the expander claimed.
//
// A tick over persisted state, not a goroutine the expander leaves behind, and
// the reason is the same one the delegated watcher gives: a Coordinator
// restarted mid-promotion must be able to finish the job in the morning. Every
// name that lands is recorded, so a resumed promotion re-issues only what it
// has left rather than the whole release.
//
// One promotion per tick. A native promotion is seconds in the case that
// matters, and a runner that drained a queue of them would hold the leader for
// as long as the slowest Artifactory in the estate.

// DestinationReader resolves a tag at one repository path of a target.
//
// A consumer-defined interface: verification needs one lookup, not the client
// factory and the credential resolution behind it. Deliberately takes a
// repository PATH as well as a target name - a bundle publishes its components
// under their own paths beneath the target's base, and a reader that could
// only see the base could not check any of them.
type DestinationReader interface {
	ResolveAt(ctx context.Context, productName, targetName, repositoryPath, tag string) (string, error)
}

// Endpoints resolves repository rows into what they name.
type Endpoints interface {
	HydrateEndpoints(ctx context.Context, ids []int64) (map[int64]store.Endpoint, error)
}

// Runner executes claimed promotions.
type Runner struct {
	service   *Service
	store     *store.Promotions
	endpoints Endpoints
	reader    DestinationReader
	owner     string
	log       *slog.Logger
}

// NewRunner builds one.
func NewRunner(
	service *Service, st *store.Promotions, endpoints Endpoints,
	reader DestinationReader, log *slog.Logger,
) *Runner {
	if log == nil {
		log = slog.Default()
	}
	return &Runner{
		service: service, store: st, endpoints: endpoints,
		reader: reader, owner: processOwner(), log: log,
	}
}

// processOwner names this process: host, pid and a random suffix.
//
// Derived here rather than passed in, so a composition root cannot forget it
// and leave every replica claiming as "coordinator" - which would make a
// stale claim indistinguishable from a live one on the deployment where that
// matters most.
//
// The suffix matters for the reason internal/security says: two Coordinators
// in a pod-per-host deployment restart onto the same host with the same pid
// often enough that host and pid alone would let a restarted process mistake
// its predecessor's claim for its own.
func processOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "coordinator"
	}
	return fmt.Sprintf("%s/%d/%s", host, os.Getpid(), uuid.NewString()[:8])
}

// Tick runs at most one promotion.
//
// Reports whether it did anything, so a controller can back off when there is
// nothing to do rather than poll at the same rate forever.
func (r *Runner) Tick(ctx context.Context) (bool, error) {
	pm, err := r.store.ClaimPromotion(ctx, r.owner)
	if errors.Is(err, store.ErrNoRecord) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	started := time.Now()
	if err := r.run(ctx, pm); err != nil {
		// A promotion that failed is SETTLED as failed rather than left for
		// the next tick. Retrying it silently forever would turn a permission
		// problem into a loop nobody is told about; a failed transfer is
		// visible, and `transfers retry` is how somebody asks again.
		r.log.ErrorContext(ctx, "promotion failed",
			"transfer", pm.TransferID, "promoter", pm.Promoter,
			"attempt", pm.Attempts, "error", err)
		if serr := r.store.Settle(ctx, pm.ID, "failed", err.Error()); serr != nil {
			return true, fmt.Errorf("record the failure of promotion %d: %w", pm.ID, serr)
		}
		return true, nil
	}

	r.log.InfoContext(ctx, "promotion complete",
		"transfer", pm.TransferID, "promoter", pm.Promoter,
		"names", pm.NamesTotal, "took", time.Since(started).Round(time.Millisecond))
	return true, r.store.Settle(ctx, pm.ID, "succeeded", "")
}

// run promotes every outstanding name, then proves it.
func (r *Runner) run(ctx context.Context, pm store.Promotion) error {
	hop, err := r.hopFor(ctx, pm)
	if err != nil {
		return err
	}

	pending, err := r.store.PendingNames(ctx, pm.ID)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		// Everything already landed - a promotion resumed after the names
		// were published but before it settled. Verification below still runs,
		// because "we recorded it" and "the registry serves it" are different
		// claims and only the second one is worth settling on.
		return r.verify(ctx, pm, hop)
	}

	bound, err := r.service.PromoterFor(hop)
	if err != nil {
		return err
	}
	if bound.Promoter == nil {
		// The configuration changed under a promotion that was already open:
		// somebody moved production to another host, or retyped the target.
		// Failing here is right and the message has to say so, because the
		// obvious reading - "JFrog is broken" - is wrong.
		return fmt.Errorf(
			"no promoter carries %s -> %s any more: %s Retry the transfer to copy it instead",
			hop.Origin, hop.Destination, bound.Verdict.Reason)
	}
	if bound.Promoter.Name() != pm.Promoter {
		return fmt.Errorf(
			"this promotion was opened by %q and %q claims it now: the configuration changed "+
				"underneath it. Retry the transfer to start again",
			pm.Promoter, bound.Promoter.Name())
	}

	// Named one at a time so a promotion interrupted half way is RESUMABLE at
	// the exact name rather than restartable at the beginning.
	for _, n := range pending {
		// The claimed hop with ONE name in it. The pair, the credentials and
		// the repository keys are whatever the claim resolved; only the name
		// narrows, which is what makes each call independently recordable.
		one := bound.Hop
		one.Names = []promote.Name{
			{Repository: n.Repository, Tag: n.Tag, Digest: n.Digest},
		}

		if _, err := bound.Promoter.Promote(ctx, one); err != nil {
			if rerr := r.store.NameFailed(ctx, pm.ID, n.Position, err.Error()); rerr != nil {
				r.log.WarnContext(ctx, "could not record which name failed",
					"promotion", pm.ID, "position", n.Position, "error", rerr)
			}
			return err
		}
		if err := r.store.NamePromoted(ctx, pm.ID, n.Position); err != nil {
			return err
		}
	}

	return r.verify(ctx, pm, hop)
}

// verify reads the destination back.
//
// A promotion is settled on what the REGISTRY SERVES, never on what it
// answered. The same argument the delegated watcher makes: a 200 says
// Artifactory did something, not that it did this - and the one failure worth
// catching here is the one where a release is reported promoted and the tag
// resolves to the previous version, which nobody would notice until a cluster
// pulled it.
//
// The ROOT is verified always. Verifying every name would be a HEAD per
// component - two hundred round trips to re-prove what the same call already
// reported - and the components are reached through the root, so a root that
// resolves correctly is the evidence that matters. What a promotion cannot
// prove about the tree, the destination verification stage proves for every
// transfer alike.
func (r *Runner) verify(ctx context.Context, pm store.Promotion, hop transfer.PromotionHop) error {
	if r.reader == nil || len(hop.Names) == 0 {
		return nil
	}
	root := hop.Names[0]
	if root.Digest == "" {
		return nil
	}

	got, err := r.reader.ResolveAt(ctx, hop.ProductName, hop.Destination, root.Repository, root.Tag)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			return fmt.Errorf(
				"%s reported success, but %s:%s is not present at %s",
				pm.Promoter, displayPath(root.Repository), root.Tag, hop.Destination)
		}
		// Unreachable rather than wrong. The promotion is not settled either
		// way; the next tick picks it up, finds no names left to publish, and
		// verifies again.
		return fmt.Errorf("could not read %s back from %s: %w",
			root.Tag, hop.Destination, err)
	}

	if !strings.EqualFold(got, root.Digest) {
		return fmt.Errorf(
			"%s:%s at %s resolves to %s, not %s. Something else wrote that tag; "+
				"this release was not promoted",
			displayPath(root.Repository), root.Tag, hop.Destination, got, root.Digest)
	}
	return nil
}

// hopFor rebuilds a promotion's hop from what was recorded.
//
// The endpoints come from the transfer's own repository rows rather than from
// current configuration, for the reason transfer/request.go states: a
// request's intent is durable, and re-deriving a destination later turns
// "promote to production" into "promote to whatever production means today".
func (r *Runner) hopFor(ctx context.Context, pm store.Promotion) (transfer.PromotionHop, error) {
	endpoints, err := r.endpoints.HydrateEndpoints(ctx,
		[]int64{pm.OriginRepoID, pm.DestinationRepoID})
	if err != nil {
		return transfer.PromotionHop{}, err
	}
	origin, ok := endpoints[pm.OriginRepoID]
	if !ok {
		return transfer.PromotionHop{}, fmt.Errorf(
			"origin repository %d is no longer in the catalog", pm.OriginRepoID)
	}
	destination, ok := endpoints[pm.DestinationRepoID]
	if !ok {
		return transfer.PromotionHop{}, fmt.Errorf(
			"destination repository %d is no longer in the catalog", pm.DestinationRepoID)
	}

	names, err := r.allNames(ctx, pm.ID)
	if err != nil {
		return transfer.PromotionHop{}, err
	}

	return transfer.PromotionHop{
		TransferID:     pm.TransferID,
		ProductName:    pm.ProductName,
		Origin:         configuredName(origin.Name),
		Destination:    configuredName(destination.Name),
		Package:        pm.PackageTag,
		ManifestDigest: pm.PackageDigest,
		Names:          names,
	}, nil
}

// allNames is every name of a promotion, published or not.
//
// The whole set rather than the outstanding one, because the hop is what the
// promoter CLAIMS against - "these two targets are repositories of one
// Artifactory" is a fact about the pair, and a claim asked with one name would
// be asked with different evidence on a resumed promotion than on a fresh one.
func (r *Runner) allNames(ctx context.Context, promotionID int64) ([]transfer.PromotionName, error) {
	rows, err := r.store.AllNames(ctx, promotionID)
	if err != nil {
		return nil, err
	}
	out := make([]transfer.PromotionName, 0, len(rows))
	for _, n := range rows {
		out = append(out, transfer.PromotionName{
			Repository: n.Repository, Tag: n.Tag, Digest: n.Digest,
		})
	}
	return out, nil
}

// displayPath renders a relative repository for a message.
func displayPath(p string) string {
	if p == "" {
		return "the release"
	}
	return p
}

// configuredName recovers which configured target a repository row belongs to.
//
// One configured entry can own several rows - "<configured>/<path>" per path
// beneath it - and the configured name is everything before the first slash. A
// configured name may not itself contain one, which is what makes the split
// unambiguous rather than a guess. Same derivation as regclient's, and for the
// same reason: this is how a row resolves back to the credential it uses.
func configuredName(rowName string) string {
	if i := strings.Index(rowName, "/"); i > 0 {
		return rowName[:i]
	}
	return rowName
}
