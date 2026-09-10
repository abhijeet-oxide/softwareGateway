package middleware

import (
	"context"
	"net/http"
	"sort"
)

// Authentication is NOT IMPLEMENTED in v1.
//
// See docs/design/09-api.md section 10. The Coordinator runs unauthenticated
// behind a NetworkPolicy. This is an accepted, documented risk:
//
//	Anyone with network reach to the Coordinator can create, cancel and
//	re-prioritize transfers, and can read the entire audit trail. The only
//	mitigating control is network isolation. Do not expose this service
//	outside the cluster, and do not add an Ingress before replacing
//	AnonymousAuthenticator with a real one.
//
// This file exists so that enabling authentication later is ONE middleware and
// a config block - no route changes, no handler changes, no schema changes:
//
//   - the middleware slot is already occupied and correctly positioned;
//   - handlers already read an Identity from the context;
//   - every mutating handler already records identity.Subject as the audit
//     actor, so audit records are attributable the moment identities are real.
//
// An audit trail retrofitted after the fact would have a year of
// unattributable history.

// Role is a coarse authorization level. docs/design/09-api.md section 10.1.
type Role string

const (
	// RoleViewer may perform every GET and no mutation.
	RoleViewer Role = "viewer"
	// RoleOperator adds create/pause/resume/cancel/retry/setPriority/verify.
	RoleOperator Role = "operator"
	// RoleAdmin adds the worker-plane routes and audit export.
	RoleAdmin Role = "admin"
)

// Identity is the authenticated caller.
type Identity struct {
	// Subject is recorded as the audit actor. "anonymous" until authentication
	// is enabled.
	Subject string

	// Name and Email are who the SUBJECT IS, and they are carried for the
	// interface rather than for any decision made here: Subject is an opaque
	// identifier that never changes, which is exactly right for an audit record
	// and unreadable on a screen. Without them the navigation showed a person
	// their own user id, which is a number.
	Name  string
	Email string

	Roles []Role
	// ProductRoles maps a product to the roles held ON that product, e.g.
	// {"software-01": ["product-owner"]}. Roles above holds the tenant-wide
	// tier only, so a caller whose access is entirely per-product had an empty
	// role list and a screen that said they held none.
	ProductRoles map[string][]string
	// Method records how the identity was established ("none", "oidc",
	// "kubernetes", "token") so an audit record shows the trust path.
	Method string

	// Tenant is which tenant this caller belongs to. Empty means the estate,
	// which is every deployment today. Added before tenancy exists because a
	// field is free to carry and a signature change is not - see scope.go.
	Tenant string
	// Grants are scoped permissions, consulted by Can before Roles. Empty
	// today: the anonymous identity holds admin and needs none.
	Grants []Grant
}

// IsMember reports whether this account has been PROVISIONED in this tenant,
// which is not the same question as whether it may do anything.
//
// Being able to sign in proves only that the identity provider recognised
// somebody. With a corporate directory federated that is every employee, and
// it was the whole of the hole this package's authorization was written to
// close: an account nobody had provisioned arrived holding no roles, and so
// did a colleague who had been provisioned and not yet given a product. One is
// a stranger and one is waiting on an administrator; treating them alike means
// telling the second that their account does not exist.
//
// Membership is holding ANY role, of either tier. That is deliberately not "a
// role named org-member": the baseline role in config/access/roles.yaml is the
// MECHANISM that gives a product-less person their one role, and pinning its
// name in Go would make a rename here a lockout there. Anybody holding a real
// role is a member by having it.
//
// It grants nothing. Every permission question is still Can, against a scope.
func (i Identity) IsMember() bool {
	return len(i.Roles) > 0 || len(i.ProductRoles) > 0
}

// ProductNames lists the products this caller holds any role on.
//
// The keys of ProductRoles rather than a walk over Grants: this is "which
// products is this caller scoped to", which is a fact about their roles, and
// deriving it from grants would answer "which products did some action land a
// grant for" - the same list today and not the same question.
func (i Identity) ProductNames() []string {
	out := make([]string, 0, len(i.ProductRoles))
	for product := range i.ProductRoles {
		out = append(out, product)
	}
	sort.Strings(out)
	return out
}

// HasRole reports whether the identity holds a role. Admin implies operator
// implies viewer, so callers test the minimum they need.
//
// PREFER Can: a role check cannot express "operator, on this product", so a
// handler written against this one has to be rewritten when grants become
// scoped. This stays because Can is built on it.
func (i Identity) HasRole(want Role) bool {
	for _, r := range i.Roles {
		if r == want {
			return true
		}
		if r == RoleAdmin {
			return true
		}
		if r == RoleOperator && want == RoleViewer {
			return true
		}
	}
	return false
}

// Anonymous is the identity used while authentication is disabled.
var Anonymous = Identity{
	Subject: "anonymous",
	Roles:   []Role{RoleAdmin},
	Method:  "none",
}

type ctxKeyIdentity struct{}

// Authenticator establishes the caller's identity.
//
// Replacing the implementation is the whole change needed to switch on auth:
// an OIDC validator for humans and a Kubernetes TokenReview for in-cluster
// callers, per docs/design/09-api.md section 10.2.
type Authenticator interface {
	Authenticate(r *http.Request) (Identity, error)
}

// AnonymousAuthenticator grants admin to every caller.
//
// It is deliberately a named type rather than an inline closure so that it is
// greppable, and so that a deployment can assert at startup which
// authenticator is installed.
type AnonymousAuthenticator struct{}

func (AnonymousAuthenticator) Authenticate(*http.Request) (Identity, error) {
	return Anonymous, nil
}

// Auth installs the authenticator and puts the resulting Identity in the
// request context.
//
// public reports paths that must answer WITHOUT credentials. Pass nil for none.
// Liveness and readiness probes belong here and the reason is not convenience:
// a probe that requires a token gets 401, the orchestrator reads that as "not
// ready", and every replica restart-loops while the service is perfectly
// healthy. The same applies to the metrics endpoint, which is scraped by
// infrastructure that holds no identity.
func Auth(a Authenticator, writeUnauthenticated func(http.ResponseWriter, *http.Request, error), public func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if public != nil && public(r) {
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyIdentity{}, Anonymous)))
				return
			}
			id, err := a.Authenticate(r)
			if err != nil {
				writeUnauthenticated(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyIdentity{}, id)))
		})
	}
}

// IdentityFrom returns the caller's identity.
//
// Falls back to Anonymous rather than the zero value so that a handler reached
// without the middleware - in a unit test, say - still has a usable subject
// for audit rather than writing an empty actor.
func IdentityFrom(ctx context.Context) Identity {
	if id, ok := ctx.Value(ctxKeyIdentity{}).(Identity); ok {
		return id
	}
	return Anonymous
}

// PublicPaths reports the endpoints that must answer without credentials:
// probes and metrics. Everything else is authenticated.
func PublicPaths(r *http.Request) bool {
	switch r.URL.Path {
	case "/healthz", "/readyz", "/livez", "/metrics":
		return true
	}
	return false
}
