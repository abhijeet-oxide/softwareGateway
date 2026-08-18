package middleware

import (
	"context"
	"net/http"
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
// a config block — no route changes, no handler changes, no schema changes:
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
	Roles   []Role
	// Method records how the identity was established ("none", "oidc",
	// "kubernetes", "token") so an audit record shows the trust path.
	Method string

	// Tenant is which tenant this caller belongs to. Empty means the estate,
	// which is every deployment today. Added before tenancy exists because a
	// field is free to carry and a signature change is not — see scope.go.
	Tenant string
	// Grants are scoped permissions, consulted by Can before Roles. Empty
	// today: the anonymous identity holds admin and needs none.
	Grants []Grant
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
func Auth(a Authenticator, writeUnauthenticated func(http.ResponseWriter, *http.Request, error)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
// without the middleware — in a unit test, say — still has a usable subject
// for audit rather than writing an empty actor.
func IdentityFrom(ctx context.Context) Identity {
	if id, ok := ctx.Value(ctxKeyIdentity{}).(Identity); ok {
		return id
	}
	return Anonymous
}
