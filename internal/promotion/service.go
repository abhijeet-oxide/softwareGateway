// Package promotion binds the promoter plugins to product configuration.
//
// See docs/design/22-promotion.md §6.
//
// It is the composition side of the seam in internal/transfer/promotion.go,
// and it lives here rather than in internal/transfer for the same reason
// internal/replication.Delegation does: this is the half that knows about
// products, credentials and JFrog, which is precisely what the seam exists to
// keep out of the engine.
//
// Two halves, and they are deliberately separate:
//
//   - Service answers the CLAIM. Configuration only, no network, no
//     credential, and it is asked on every promotion and by the promotion
//     dialog before anybody commits to anything.
//   - Runner does the WORK, as a leader tick over promotions the expander
//     opened. Not inline in the expander, because a promotion is a call to
//     somebody else's registry and holding the expander tick on it would stall
//     every other product's planning behind one slow Artifactory.
package promotion

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/promote"
	"github.com/abhijeet-oxide/softwareGateway/internal/regclient"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/transfer"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// Products supplies the loaded configuration a claim needs.
//
// A consumer-defined interface: one lookup, not the loader, the watcher and
// the reconciler behind it.
type Products interface {
	Get(name string) (*product.Product, bool)
}

// Service answers whether a plugin carries a hop, and builds the one that does.
type Service struct {
	products Products
	secrets  *product.SecretResolver
	log      *slog.Logger
}

// NewService builds it.
func NewService(products Products, secrets *product.SecretResolver, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{products: products, secrets: secrets, log: log}
}

// Claim implements transfer.Promotion.
//
// Configuration only. It resolves both targets, translates them into the
// plugin registry's vocabulary and asks - no request leaves the process, which
// is what makes it safe to call on every promotion and safe to call from a
// GET.
func (s *Service) Claim(ctx context.Context, hop transfer.PromotionHop) (transfer.PromotionClaim, error) {
	res, err := s.resolve(hop)
	if err != nil {
		return transfer.PromotionClaim{}, err
	}

	if res.Promoter == nil {
		return transfer.PromotionClaim{Reason: reasonFor(res)}, nil
	}
	return transfer.PromotionClaim{
		Promoter: res.Verdict.Promoter,
		Claimed:  true,
		Reason:   res.Verdict.Reason,
	}, nil
}

// Bound is a promoter and the hop it claimed, in the plugin's own vocabulary.
//
// The two travel together because a promoter is built FOR one hop - it holds
// the resolved endpoints and credentials of that pair - so handing the runner
// a promoter without the hop it belongs to would invite the two to be mixed.
type Bound struct {
	Promoter promote.Promoter
	Hop      promote.Hop
	Verdict  promote.Verdict
}

// PromoterFor returns the plugin that carries a hop, and the hop it carries.
//
// Used by the Runner, which has to end up with the same promoter the expander
// claimed with - through this one function, so a hop cannot be claimed by one
// plugin and run by another.
//
// Bound.Promoter is nil when nothing claimed, which is not an error: the
// caller decides what that means, and for the expander it means "copy it".
func (s *Service) PromoterFor(hop transfer.PromotionHop) (Bound, error) {
	res, pluginHop, err := s.resolveHop(hop)
	if err != nil {
		return Bound{}, err
	}
	if res.Promoter == nil {
		return Bound{Verdict: promote.Verdict{Reason: reasonFor(res)}}, nil
	}
	return Bound{Promoter: res.Promoter, Hop: pluginHop, Verdict: res.Verdict}, nil
}

func (s *Service) resolve(hop transfer.PromotionHop) (promote.Resolution, error) {
	res, _, err := s.resolveHop(hop)
	return res, err
}

func (s *Service) resolveHop(
	hop transfer.PromotionHop,
) (promote.Resolution, promote.Hop, error) {
	p, ok := s.products.Get(hop.ProductName)
	if !ok {
		return promote.Resolution{}, promote.Hop{},
			fmt.Errorf("product %q is not loaded", hop.ProductName)
	}

	origin, err := s.endpoint(p, hop.Origin)
	if err != nil {
		return promote.Resolution{}, promote.Hop{}, err
	}
	destination, err := s.endpoint(p, hop.Destination)
	if err != nil {
		return promote.Resolution{}, promote.Hop{}, err
	}

	names := make([]promote.Name, 0, len(hop.Names))
	for _, n := range hop.Names {
		names = append(names, promote.Name{Repository: n.Repository, Tag: n.Tag, Digest: n.Digest})
	}

	pluginHop := promote.Hop{
		Product:        hop.ProductName,
		Package:        hop.Package,
		ManifestDigest: hop.ManifestDigest,
		Origin:         origin.endpoint,
		Destination:    destination.endpoint,
		Names:          names,
	}

	res, err := promote.Resolve(promote.Config{
		Origin:            origin.endpoint,
		Destination:       destination.endpoint,
		OriginClient:      origin.client,
		DestinationClient: destination.client,
		Logger:            s.log,
	}, pluginHop)
	return res, pluginHop, err
}

// resolvedEnd is one target, in both vocabularies.
type resolvedEnd struct {
	endpoint promote.Endpoint
	client   registry.ClientConfig
}

// endpoint translates one configured target for the plugin registry.
//
// The client config comes from regclient.ConfigFor - the SAME translation a
// transfer gets, credential, CA bundle, proxy and timeouts included. A
// promotion path that resolved its own would reach a host by a different route
// from the one that replicates to it, which is the exact failure
// internal/regclient's package comment exists to prevent.
func (s *Service) endpoint(p *product.Product, targetName string) (resolvedEnd, error) {
	t, ok := p.Target(targetName)
	if !ok {
		return resolvedEnd{}, fmt.Errorf(
			"product %q has no target %q", p.Metadata.Name, targetName)
	}

	client, err := regclient.ConfigFor(p, s.secrets, v1.JobEndpoint{
		Product:    p.Metadata.Name,
		Name:       t.Name,
		Registry:   t.Registry,
		Repository: t.Repository,
		Type:       string(t.Type),
		Role:       "target",
	})
	if err != nil {
		return resolvedEnd{}, err
	}

	return resolvedEnd{
		endpoint: promote.Endpoint{
			Name:         t.Name,
			Registry:     t.Registry,
			Repository:   t.Repository,
			RegistryType: string(t.Type),
			Options:      options(t),
		},
		client: client,
	}, nil
}

// options carries a target's promoter-specific settings.
//
// A map rather than fields on promote.Endpoint, so adding the second plugin
// does not mean editing the first plugin's types - see the Options comment
// there. The keys are the plugin's own, which is why they are spelled as the
// configuration spells them.
func options(t product.Target) map[string]string {
	out := map[string]string{}
	if t.JFrogRepositoryKey != "" {
		out["jfrogRepositoryKey"] = t.JFrogRepositoryKey
	}
	// The platform base URL falls back to the one Xray already needed. It is
	// the same host reached for the same reason, and a second field to
	// configure it would be a second field to get wrong - and the wrong one is
	// always the one that was not updated.
	if endpoint := t.JFrogEndpoint; endpoint != "" {
		out["jfrogEndpoint"] = endpoint
	} else if t.XrayEndpoint != "" {
		out["jfrogEndpoint"] = t.XrayEndpoint
	}
	return out
}

// reasonFor renders why nothing claimed, in words an operator can act on.
func reasonFor(res promote.Resolution) string {
	if reason := res.DeclinedReason(); reason != "" {
		return reason
	}
	// No plugin registered at all. Not a fault, and the wording matters: an
	// estate with no same-registry pair should read this as "there is no
	// shortcut here", not as "something is misconfigured".
	return "no promoter handles this pair, so the content is copied"
}

var _ transfer.Promotion = (*Service)(nil)
