package middleware

import (
	"context"
	"fmt"
	"net/http"

	"github.com/abhijeet-oxide/softwareGateway/pkg/authz"
)

// OIDCAuthenticator is the real authenticator that replaces
// AnonymousAuthenticator. See docs/design/24-identity-and-access.md.
//
// It is a thin adapter on purpose. All the verification lives in pkg/authz,
// which is shared with the other tools on the platform; this file only maps
// that package's Identity onto the one every handler here already reads, so
// switching authentication on changes no handler and no route.
type OIDCAuthenticator struct {
	Verifier *authz.Verifier
	// Engine decides permissions. When nil, permission checks fall back to the
	// role ladder below, which lets authentication ship before policy does.
	Engine authz.Engine
}

// Authenticate validates the bearer token and maps the result onto Identity.
func (a OIDCAuthenticator) Authenticate(r *http.Request) (Identity, error) {
	raw := authz.BearerToken(r)
	if raw == "" {
		return Identity{}, fmt.Errorf("no bearer token")
	}
	id, err := a.Verifier.Verify(r.Context(), raw)
	if err != nil {
		return Identity{}, err
	}
	return fromAuthz(id), nil
}

// fromAuthz maps the shared identity onto this package's.
//
// The two role tiers become Grants, which is what Can already consults:
//
//   - an org-tier role produces a grant with an EMPTY Product, which
//     Scope.covers treats as a wildcard - so it covers products that do not
//     exist yet. That is the whole point of the tier.
//   - a product-tier role produces a grant naming that product.
func fromAuthz(a authz.Identity) Identity {
	out := Identity{
		Subject: a.Subject,
		Name:    a.Name,
		Email:   a.Email,
		Tenant:  a.Tenant,
		Method:  "oidc",
	}
	for _, role := range a.OrgRoles {
		for _, act := range actionsFor(role) {
			out.Grants = append(out.Grants, Grant{
				Action: act,
				Scope:  Scope{Tenant: a.Tenant},
			})
		}
		out.Roles = append(out.Roles, Role(role))
	}
	for product, roles := range a.Products {
		if out.ProductRoles == nil {
			out.ProductRoles = map[string][]string{}
		}
		out.ProductRoles[product] = append(out.ProductRoles[product], roles...)
		for _, role := range roles {
			for _, act := range actionsFor(role) {
				out.Grants = append(out.Grants, Grant{
					Action: act,
					Scope:  Scope{Tenant: a.Tenant, Product: product},
				})
			}
		}
	}
	return out
}

// actionsFor maps a role name onto the actions it implies.
//
// This is the FALLBACK ladder, used when no policy engine is configured. When
// Cerbos is wired the engine is authoritative and this only decides what the
// UI is told it may attempt. Keep the two in step: the suffix of a role name
// is the level, which is why the names were chosen that way.
func actionsFor(role string) []Action {
	switch suffix(role) {
	case "admin", "owner":
		return []Action{ActionRead, ActionOperate, ActionApply, ActionAdmin, ActionWork}
	// NOT ActionApply. deploy/cerbos/policies/download.yaml puts `apply`
	// alongside `promote` and grants both to org_wide_admin and product_owner
	// only, and docs/design/24 section 5.2 describes org-operator in the same
	// terms. This ladder disagreed with both, which cost nothing while nothing
	// consulted it and would have handed every operator the one action that
	// writes into somebody else's registry the moment something did.
	case "operator":
		return []Action{ActionRead, ActionOperate}
	case "security", "reader", "viewer":
		return []Action{ActionRead}
	// The data plane. ONE action and deliberately not ActionRead as well: a
	// worker is told everything it needs in the lease response, so a credential
	// that could also list products, read the audit trail or enumerate
	// transfers would be carrying reach it has no use for. See
	// middleware/workload.go for the fence this makes enforceable.
	case "worker":
		return []Action{ActionWork}
	default:
		return nil
	}
}

// suffix takes the level off a role name: "org-security" -> "security",
// "product-owner" -> "owner".
func suffix(role string) string {
	for i := len(role) - 1; i >= 0; i-- {
		if role[i] == '-' {
			return role[i+1:]
		}
	}
	return role
}

// NewOIDCAuthenticator builds the authenticator from configuration. It fails
// fast: a service that starts with broken auth configuration looks healthy
// while rejecting everyone.
func NewOIDCAuthenticator(ctx context.Context, issuer, discoveryURL, hostHeader, audience, cerbosAddr string, skipIssuer bool) (OIDCAuthenticator, error) {
	v, err := authz.NewVerifier(ctx, authz.Config{
		Issuer:          issuer,
		DiscoveryURL:    discoveryURL,
		HostHeader:      hostHeader,
		Audience:        audience,
		SkipIssuerCheck: skipIssuer,
	})
	if err != nil {
		return OIDCAuthenticator{}, err
	}
	a := OIDCAuthenticator{Verifier: v}
	if cerbosAddr != "" {
		a.Engine = authz.NewCerbos(cerbosAddr)
	}
	return a, nil
}
