package transfer

import (
	"context"
	"fmt"

	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// The NATIVE PROMOTION half of the strategy seam.
//
// delegate.go answers "does this destination want the bytes pushed to it".
// This answers a different question about a different pair: "can the registry
// these two targets share do this hop better than we can". Both sit at the
// same point - where a transfer becomes work - and for the same reason
// (docs/design/18 §7): the planner still knows only about manifests and blobs,
// the engine still knows only about streams, and neither has to answer "and
// what if we are not actually moving anything?".
//
// It is separate from Delegation rather than folded into Strategy because the
// inputs differ. A delegated destination is a property of ONE target, known
// from its configuration alone. A native promotion is a property of the PAIR -
// lab and production being repositories of one Artifactory - and of the
// package, since the names to publish have to be known before anything is
// claimed. Widening Strategy to take an origin would have made every caller of
// it pass one, including the replication path that has no use for it.
//
// Optional, like delegation: nil means every promotion is a copy, which is
// what every deployment before this seam existed was, and what an estate with
// no JFrog pair still is.

// PromotionName is one name the destination must answer to when the hop is
// done. It mirrors promote.Name, restated here so this package does not import
// the plugin registry - the seam is what keeps a vendor's name out of the
// engine, and an import would put it back.
type PromotionName struct {
	// Repository is the path BENEATH each end's configured base, so the same
	// value re-bases under either.
	Repository string
	Tag        string
	Digest     string
}

// PromotionHop is one promotion as the seam sees it.
type PromotionHop struct {
	TransferID  string
	ProductName string
	// Origin and Destination are CONFIGURED TARGET NAMES. The seam's other
	// side resolves them to registries, credentials and repository keys; this
	// package does not know what any of those are.
	Origin      string
	Destination string

	Package        string
	ManifestDigest string
	Names          []PromotionName
}

// PromotionClaim is what came back.
type PromotionClaim struct {
	// Promoter is the plugin that claimed, empty when none did.
	Promoter string
	Claimed  bool
	// Reason is why, claimed or not, in words. Recorded on the transfer when a
	// promotion goes the slow way, so "why did this take forty minutes" has an
	// answer that outlives the request.
	Reason string
}

// Promotion is the native half of the seam.
//
// A consumer-defined interface, and the reason this package still has no
// knowledge of JFrog, of product configuration or of credentials: it needs one
// answer, not the plugin registry and the client factory behind it.
type Promotion interface {
	// Claim asks whether a plugin carries this hop natively.
	//
	// Called on every promotion, so it must be cheap: a configuration lookup
	// and a comparison, never a network call.
	Claim(ctx context.Context, hop PromotionHop) (PromotionClaim, error)
}

// WithPromotion attaches the native half of the seam.
//
// A builder rather than a constructor argument, for the same reason
// WithDelegation is one: it is genuinely optional, and adding it to
// NewExpander would make every existing caller and every test pass a nil to
// say "unchanged".
func (e *Expander) WithPromotion(p Promotion, st *store.Promotions) *Expander {
	e.promotion = p
	e.promotions = st
	return e
}

// relocate plans a promotion the registry carries out itself.
//
// It creates no jobs and never will. The transfer enters `promoting` and is
// settled later by whatever runs the promoter - the same shape as delegate(),
// and for the same reason: the registry is under no obligation to finish while
// this tick is running, and a promotion that completes during a Coordinator
// restart must still settle correctly afterwards.
//
// The names are recorded on the promotion row rather than re-derived at run
// time. What has to arrive at the destination is decided by the tree as it was
// when somebody asked, and re-deriving it later would let a package re-analysed
// in between silently change what a promotion means.
func (e *Expander) relocate(
	ctx context.Context, t store.PendingTransfer, claim PromotionClaim, hop PromotionHop,
) error {
	if e.promotions == nil {
		return fmt.Errorf(
			"%s claims this promotion, but no promotion store is configured", claim.Promoter)
	}

	names := make([]store.PromotionName, 0, len(hop.Names))
	for _, n := range hop.Names {
		names = append(names, store.PromotionName{
			Repository: n.Repository, Tag: n.Tag, Digest: n.Digest,
		})
	}

	if err := e.promotions.Open(ctx, t.ID, claim.Promoter, names); err != nil {
		return err
	}

	e.log.InfoContext(ctx, "promotion handed to the registry",
		"transfer", t.ID, "promoter", claim.Promoter,
		"from", hop.Origin, "to", hop.Destination, "names", len(names))
	return nil
}

// claimPromotion asks the seam, and treats an unreachable seam as "no".
//
// A failure to ASK must not fail the transfer. The fast path is an
// optimisation; the copy path is always correct, and refusing to promote at
// all because a configuration lookup errored would turn a cosmetic problem
// into an outage. The complaint is logged and the promotion proceeds the
// ordinary way, which is the same trade delegate() does not make - there,
// falling back would push bytes at a read-only mirror.
func (e *Expander) claimPromotion(ctx context.Context, hop PromotionHop) PromotionClaim {
	if e.promotion == nil {
		return PromotionClaim{}
	}
	claim, err := e.promotion.Claim(ctx, hop)
	if err != nil {
		e.log.WarnContext(ctx, "could not ask whether the registry can promote this itself",
			"transfer", hop.TransferID, "from", hop.Origin, "to", hop.Destination, "error", err)
		return PromotionClaim{Reason: "could not be determined: " + err.Error()}
	}
	return claim
}
