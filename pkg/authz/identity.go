// Package authz is the shared authentication and authorization middleware for
// every tool on the platform. It is deliberately self-contained: import it,
// give it an issuer and a Cerbos address, and every handler behind it knows
// who the caller is and what they may do.
//
// It is a SEPARATE package from any one product's internals so that Configer
// and Software Gateway run the same code rather than two careful copies of the
// same intentions - the same rule that keeps uikit/ byte-identical between the
// two front ends.
//
// See docs/design/24-identity-and-access.md.
package authz

import (
	"context"
	"strings"
)

// Identity is the authenticated caller, decoded from the token.
//
// Roles arrive in two tiers and the difference is the whole model:
//
//   - OrgRoles name no product and therefore cover every product in the
//     tenant, including ones created after the token was issued.
//   - Products maps a product name to the roles held ON that product.
type Identity struct {
	// Subject is the stable user id (ZITADEL's `sub`). Recorded as the audit
	// actor. Never an e-mail or a username: those get renamed, this does not.
	Subject string
	// Tenant is the ZITADEL organization the caller belongs to.
	Tenant string
	// OrgRoles are tenant-wide, e.g. org-security.
	OrgRoles []string
	// Products maps product name to the roles held on it, e.g.
	// {"software-01": ["product-owner"]}.
	Products map[string][]string

	Email string
	Name  string
	// Method records how the identity was established, for audit: "oidc",
	// "anonymous".
	Method string
}

// Anonymous is the identity used when authentication is disabled.
var Anonymous = Identity{Subject: "anonymous", Method: "anonymous"}

// Authenticated reports whether a real identity was established.
func (i Identity) Authenticated() bool {
	return i.Method != "" && i.Method != "anonymous"
}

// AllRoles is every role the caller holds, org tier first. This is what a
// policy engine is given: the engine decides what each role may do, and the
// scope conditions are carried by ProductNames and Tenant.
func (i Identity) AllRoles() []string {
	out := make([]string, 0, len(i.OrgRoles)+len(i.Products))
	out = append(out, i.OrgRoles...)
	seen := map[string]bool{}
	for _, roles := range i.Products {
		for _, r := range roles {
			if !seen[r] {
				seen[r] = true
				out = append(out, r)
			}
		}
	}
	return out
}

// ProductNames lists the products the caller holds any product-tier role on.
//
// An org-tier role grants nothing here on purpose: it is not a list of
// products, it is the absence of a product restriction, and a policy that
// checks membership of this list would silently stop covering products created
// later. That is the failure this whole two-tier model exists to prevent.
func (i Identity) ProductNames() []string {
	out := make([]string, 0, len(i.Products))
	for p := range i.Products {
		out = append(out, p)
	}
	return out
}

// HasOrgRole reports whether the caller holds a tenant-wide role.
func (i Identity) HasOrgRole(role string) bool {
	for _, r := range i.OrgRoles {
		if r == role {
			return true
		}
	}
	return false
}

// RolesOn lists the roles held on one product, org-tier roles included,
// because an org-tier role applies to every product by definition.
func (i Identity) RolesOn(product string) []string {
	out := append([]string(nil), i.OrgRoles...)
	return append(out, i.Products[product]...)
}

type ctxKey struct{}

// NewContext carries an identity through the request.
func NewContext(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the caller's identity.
//
// Falls back to Anonymous rather than the zero value so a handler reached
// without the middleware - in a unit test, say - still has a usable subject
// for audit rather than writing an empty actor.
func FromContext(ctx context.Context) Identity {
	if id, ok := ctx.Value(ctxKey{}).(Identity); ok {
		return id
	}
	return Anonymous
}

// splitProductRole decodes a product-tier role key.
//
// Product role keys are namespaced as "<product>:<role>" (software-01:product-owner)
// and this is not cosmetic. ZITADEL emits roles under a claim keyed by PROJECT
// ID - an opaque number - so a token alone cannot say which product a bare
// `product-owner` refers to. Resolving that would mean either shipping a
// project-id map into every service or giving every service a ZITADEL
// credential to look one up. Putting the product in the role key makes the
// token self-describing, and the middleware stateless.
func splitProductRole(key string) (product, role string, ok bool) {
	i := strings.Index(key, ":")
	if i <= 0 || i == len(key)-1 {
		return "", "", false
	}
	return key[:i], key[i+1:], true
}
