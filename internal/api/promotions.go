package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/transfer"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// The promotion dialog's one question, answered on the server.
//
// See docs/design/22-promotion.md §7.
//
// "Where can this release go, and what will happen if I send it there" reads
// like a client-side join over configuration and history. It is not: the
// answer needs the promotion path, promotionOnly, which targets already hold
// the release, whether its tree has been walked, and which promoter plugin
// would claim each hop. A client assembling that would be re-implementing
// internal/transfer's resolution rules in TypeScript, and the copy that
// drifted would be the one people clicked.
//
// It is a GET and it stays one: every input is configuration or a row, nothing
// leaves the process, and a dialog that could not be opened without a side
// effect would be a dialog nobody could safely open twice.

// Promotions answers how a promotion would be carried out.
//
// A consumer-defined interface rather than *promotion.Service, so this package
// depends on the one call it makes rather than on the plugin registry, the
// credential resolution and the JFrog client behind it (docs/design/15 §6).
// Optional: without it every hop is reported as a copy, which is exactly what
// it would be.
type Promotions interface {
	Claim(ctx context.Context, hop transfer.PromotionHop) (transfer.PromotionClaim, error)
}

// handlePromotionOptions serves
// GET /api/v1/products/{product}/packages/{package}/promotionOptions.
func (s *Server) handlePromotionOptions(w http.ResponseWriter, r *http.Request) {
	productName := chi.URLParam(r, "product")
	ref := chi.URLParam(r, "package")

	if !s.productExists(w, r, productName) {
		return
	}
	p, ok := s.deps.Products.Get(productName)
	if !ok {
		NotFound(w, r, "product", productName)
		return
	}
	row, ok := s.resolvePackage(w, r, productName, ref)
	if !ok {
		return
	}

	out := v1.PromotionOptionsResponse{
		Product:  productName,
		Package:  ref,
		Tag:      row.Tag,
		Analysed: row.ExpandedAt != nil,
	}

	// WHERE IT IS, from the transfer history rather than from a field on the
	// package. A release is at a target because a transfer put it there, and
	// that is the only record that survives the target being reconfigured.
	held, inFlight := s.placement(r.Context(), row.ID)

	from, to := p.PromotionPath()
	out.Origins = origins(p, held)
	out.DefaultOrigin = defaultOrigin(p, out.Origins, from)

	// The names a promotion would publish, needed by the plugin claim. Derived
	// through the SAME function the expander uses, so the dialog cannot
	// promise a fast path the transfer then declines to take.
	names := s.promotionNames(r.Context(), row)

	out.Destinations = s.destinations(r.Context(), p, row, out.DefaultOrigin, names, held, inFlight)
	out.DefaultDestinations = defaultDestinations(p, to, out.Destinations)
	out.Promotable, out.Reason = promotable(out)

	WriteJSON(w, r, http.StatusOK, out)
}

// placement is which targets hold this release, and which are still receiving it.
//
// Keyed by CONFIGURED target name, because that is what a promotion names. A
// transfer that predates the name being recorded falls back to the resolved
// path, which will not match a configured name and correctly contributes
// nothing rather than matching the wrong target.
func (s *Server) placement(
	ctx context.Context, packageID int64,
) (held, inFlight map[string]string) {
	held, inFlight = map[string]string{}, map[string]string{}
	if s.deps.Packages == nil {
		return held, inFlight
	}

	transfers, err := s.deps.Packages.ListTransfers(ctx, store.ListTransfersFilter{
		PackageID: packageID, Limit: 100,
	})
	if err != nil {
		// Not fatal, and the degradation is honest: with no history every
		// destination reads as ABSENT, which offers a promotion that is
		// harmless to make twice.
		return held, inFlight
	}

	for _, t := range transfers {
		name := t.TargetName
		if name == "" {
			continue
		}
		switch strings.ToLower(t.State) {
		case "succeeded":
			if _, seen := held[name]; !seen {
				held[name] = t.ID
			}
		case "pending", "planning", "ready", "running", "paused", "syncing",
			"promoting", "verifying", "waiting":
			if _, seen := inFlight[name]; !seen {
				inFlight[name] = t.ID
			}
		}
	}
	return held, inFlight
}

// origins is every enabled target, marked with whether it holds the release.
//
// All of them rather than only the ones that do, because "this release is not
// in lab" is the answer somebody needs when the dialog will not let them
// promote - and a list that silently omitted lab could not say it.
func origins(p *product.Product, held map[string]string) []v1.PromotionOrigin {
	out := make([]v1.PromotionOrigin, 0, len(p.Spec.Targets))
	for _, t := range p.Spec.Targets {
		if !t.IsEnabled() {
			continue
		}
		id, holds := held[t.Name]
		out = append(out, v1.PromotionOrigin{
			Name:           t.Name,
			Environment:    t.Environment,
			Registry:       t.Registry,
			Repository:     t.Repository,
			Holds:          holds,
			LastTransferID: id,
		})
	}
	return out
}

// defaultOrigin is where a promotion with no --from would read from.
//
// It MIRRORS transfer.promotionOrigin step for step, and that is the whole
// requirement: a dialog that pre-selected a different origin from the one the
// request will resolve would be a dialog that lies about what pressing the
// button does. The rules, in order:
//
//  1. The promotion path's source environment, when it names exactly one
//     target. That is what the operator wrote down.
//  2. Otherwise the one candidate that actually HOLDS the release. `lab-eu`
//     and `lab-us` are indistinguishable as configuration and completely
//     distinguishable as history.
//  3. Otherwise nothing, and the dialog asks. A release in two labs is two
//     different promotions, and picking one would be a guess.
func defaultOrigin(p *product.Product, origins []v1.PromotionOrigin, fromEnvironment string) string {
	var candidates []v1.PromotionOrigin

	if anyEnvironment(p) {
		for _, o := range origins {
			if o.Environment == fromEnvironment {
				candidates = append(candidates, o)
			}
		}
		if len(candidates) == 1 {
			return candidates[0].Name
		}
		if len(candidates) == 0 {
			candidates = origins
		}
	} else {
		// No environments anywhere: a promotion moves OUT of a target that
		// accepts replication, so the ordinary ones are the candidates.
		byName := map[string]bool{}
		for _, t := range p.Spec.Targets {
			if !t.PromotionOnly {
				byName[t.Name] = true
			}
		}
		for _, o := range origins {
			if byName[o.Name] {
				candidates = append(candidates, o)
			}
		}
		if len(candidates) == 1 {
			return candidates[0].Name
		}
	}

	var holding []string
	for _, c := range candidates {
		if c.Holds {
			holding = append(holding, c.Name)
		}
	}
	if len(holding) == 1 {
		return holding[0]
	}
	return ""
}

// destinations is every target this release could be promoted into.
func (s *Server) destinations(
	ctx context.Context, p *product.Product, row store.PackageRow,
	origin string, names []transfer.PromotionName,
	held, inFlight map[string]string,
) []v1.PromotionDestination {
	out := make([]v1.PromotionDestination, 0, len(p.Spec.Targets))

	for _, t := range p.Spec.Targets {
		d := v1.PromotionDestination{
			Name:          t.Name,
			Environment:   t.Environment,
			Registry:      t.Registry,
			Repository:    t.Repository,
			PromotionOnly: t.PromotionOnly,
			Default:       t.Default,
			State:         "ABSENT",
			Method:        v1.PromotionCopy,
		}

		switch {
		case !t.IsEnabled():
			d.Unavailable = "this target is disabled"
		case t.Name == origin:
			d.Unavailable = "this is where the release is being promoted from"
		}

		if id, ok := inFlight[t.Name]; ok {
			d.State, d.TransferID = "IN_FLIGHT", id
		} else if id, ok := held[t.Name]; ok {
			d.State, d.TransferID = "PRESENT", id
		}

		d.Method, d.MethodReason = s.method(ctx, p, row, origin, t.Name, names, d.Unavailable)
		out = append(out, d)
	}
	return out
}

// method asks the promoter plugins how this hop would be carried out.
//
// Every branch produces a SENTENCE, because the interesting case is the one
// where the fast path is unavailable and three of the reasons are
// configuration mistakes worth fixing. "COPY" on its own tells nobody whether
// their two targets are on different hosts, typed wrong, or simply not
// analysed yet.
func (s *Server) method(
	ctx context.Context, p *product.Product, row store.PackageRow,
	origin, destination string, names []transfer.PromotionName, unavailable string,
) (v1.PromotionMethod, string) {
	switch {
	case unavailable != "":
		return v1.PromotionCopy, ""
	case s.deps.Promotions == nil:
		return v1.PromotionCopy, "no promoter is configured, so the content is copied"
	case origin == "":
		return v1.PromotionCopy, "the origin is not decided yet"
	case len(names) == 0:
		// The same gate the expander applies. Said in the terms a reader can
		// act on: analysing is a button on the release, and pressing it makes
		// the fast path available.
		return v1.PromotionCopy,
			"this release has not been analysed, so the names underneath it are not known yet - " +
				"analyse it and a JFrog pair can relocate it instead of copying it"
	}

	claim, err := s.deps.Promotions.Claim(ctx, transfer.PromotionHop{
		ProductName:    p.Metadata.Name,
		Origin:         origin,
		Destination:    destination,
		Package:        row.Tag,
		ManifestDigest: row.ManifestDigest,
		Names:          names,
	})
	if err != nil {
		// A dialog must open. Reporting the copy path with the complaint
		// attached is right in a way a 500 is not: the copy path is what would
		// actually happen, and it is always correct.
		return v1.PromotionCopy, "could not be determined: " + err.Error()
	}
	if claim.Claimed {
		return v1.PromotionRelocate, claim.Reason
	}
	return v1.PromotionCopy, claim.Reason
}

// promotionNames is what a promotion of this release would publish.
//
// Empty when the release has not been analysed, which is not a failure: it is
// the state discovery leaves behind, and it is exactly the state in which the
// expander declines to claim.
func (s *Server) promotionNames(
	ctx context.Context, row store.PackageRow,
) []transfer.PromotionName {
	if s.deps.Packages == nil || row.ExpandedAt == nil {
		return nil
	}
	tree, complete, err := s.deps.Packages.ReadExpandedTree(ctx, row.ID)
	if err != nil || !complete || len(tree.Artifacts) == 0 {
		return nil
	}

	relative := row.SourceRepository
	endpoints, err := s.deps.Packages.HydrateEndpoints(ctx, []int64{row.SourceRepoID})
	if err == nil {
		if src, ok := endpoints[row.SourceRepoID]; ok && src.Repository != "" {
			relative = src.Repository
		}
	}
	return transfer.PromotionNamesFor(tree, relative, []string{row.Tag})
}

// defaultDestinations is what a dialog pre-selects.
//
// The promotion path's destination environment, and only when it resolves to
// exactly ONE target. Several is not a default: `production-eu` and
// `production-us` are two promotions, and pre-ticking both would make sending
// a release to a region nobody asked about the path of least resistance. The
// dialog asks instead.
func defaultDestinations(
	p *product.Product, toEnvironment string, destinations []v1.PromotionDestination,
) []string {
	var inEnvironment []string
	for _, d := range destinations {
		if d.Unavailable != "" || d.State != "ABSENT" {
			continue
		}
		if d.Environment == toEnvironment {
			inEnvironment = append(inEnvironment, d.Name)
		}
	}
	if len(inEnvironment) == 1 {
		return inEnvironment
	}

	// No environments declared anywhere. A promotion goes INTO a target that
	// refuses direct replication, so one promotionOnly target is unambiguous
	// with no configuration at all - the same deduction
	// transfer.selectTargets makes, and it must stay the same one or the
	// dialog would pre-select something the requester then refuses.
	if !anyEnvironment(p) {
		var only []string
		for _, d := range destinations {
			if d.PromotionOnly && d.Unavailable == "" && d.State == "ABSENT" {
				only = append(only, d.Name)
			}
		}
		if len(only) == 1 {
			return only
		}
	}
	return nil
}

func anyEnvironment(p *product.Product) bool {
	for _, t := range p.Spec.Targets {
		if t.Environment != "" {
			return true
		}
	}
	return false
}

// promotable says whether there is anything to offer, and why not.
//
// The reasons are separate because the fixes are: a release nowhere yet needs
// downloading, a product with one target needs configuring, and a release
// already everywhere needs nothing at all. Collapsing them into "cannot
// promote" would send all three readers to the wrong place.
func promotable(out v1.PromotionOptionsResponse) (bool, string) {
	var open, blocked int
	for _, d := range out.Destinations {
		if d.Unavailable != "" {
			continue
		}
		if d.State == "ABSENT" {
			open++
		} else {
			blocked++
		}
	}

	switch {
	case out.DefaultOrigin == "" && !anyHolds(out.Origins):
		return false, "this release has not been downloaded to any target yet, " +
			"so there is nothing to promote"
	case out.DefaultOrigin == "":
		return true, "several targets hold this release; choose which one to promote from"
	case open > 0:
		return true, ""
	case blocked > 0:
		return false, "every other target already holds this release"
	default:
		return false, "this product has no other target to promote into"
	}
}

func anyHolds(origins []v1.PromotionOrigin) bool {
	for _, o := range origins {
		if o.Holds {
			return true
		}
	}
	return false
}

// promotionProgressDTO renders a transfer's promotion.
func promotionProgressDTO(pm store.Promotion) *v1.PromotionProgress {
	return &v1.PromotionProgress{
		Promoter:    pm.Promoter,
		State:       strings.ToUpper(pm.State),
		NamesTotal:  pm.NamesTotal,
		NamesDone:   pm.NamesDone,
		Attempts:    pm.Attempts,
		LastError:   pm.LastError,
		RequestedAt: pm.RequestedAt.String,
		StartedAt:   pm.StartedAt.String,
		FinishedAt:  pm.FinishedAt.String,
	}
}
