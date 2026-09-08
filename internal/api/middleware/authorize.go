package middleware

import (
	"net/http"
	"net/url"
	"strings"
)

// Authorization: what an authenticated caller may actually reach.
//
// # The hole this closes
//
// Authentication and authorization are two questions, and until this file
// existed only the first was asked. Every route was reachable by anybody
// holding a valid token, whatever roles they held or did not hold. With an
// identity provider federating a corporate directory that is not a narrow gap:
// ANY account in the directory could sign in and read the whole estate - every
// product, every transfer, and the audit trail, which is the most sensitive
// read in the system. The person who found it had signed in through Microsoft,
// been provisioned nothing at all, and could see everything.
//
// The machinery was all present and nothing called it: Identity.Can, scoped
// grants, a policy engine constructed at startup and never consulted. A
// permission model that no handler asks is documentation.
//
// # Why it is one middleware and not eighty handler edits
//
// A check inside each handler is the same decision written eighty times, and
// the failure mode is a route added later with the check left out - which is
// silent, and is exactly how this gap would come back. Here the DEFAULT
// decides, so a new route is governed the moment it is registered and the only
// way to weaken it is to edit this file, where it is visible.
//
// It is also the same shape as the other two route predicates in this package,
// PublicPaths and WorkerPlane. One greppable function per question.
//
// # It fails closed
//
// There is no "unknown route" branch that permits. A path this file does not
// recognise gets the ordinary rule for its method, which is read for a GET and
// operate for anything that writes; a caller holding neither is refused. The
// only routes that pass unconditionally are the two named in AlwaysAllowed, and
// they are named there with the reason.

// Requirement is what a request needs in order to proceed.
type Requirement struct {
	Action Action
	// Product is the product named in the path, empty for a route that names
	// none.
	Product string
	// AnyScope permits a caller who holds the action on ANY product, rather
	// than tenant-wide.
	//
	// Set ONLY for routes whose handler filters its own results by
	// Identity.VisibleProducts. That filtering is the thing that makes the
	// wider permission safe, so the two must be changed together: marking a
	// route AnyScope without filtering it hands a caller scoped to one product
	// the contents of all of them.
	AnyScope bool
}

// AlwaysAllowed reports the routes any authenticated caller may reach,
// whatever they hold.
//
// Both of them exist so the application can EXPLAIN a refusal. The SPA probes
// /system/version before it renders anything at all and reads /whoami to learn
// what it may offer; gate either and a person with no roles gets a
// service-unavailable screen or a blank one, instead of a page telling them
// their account has no roles and who to ask. A security control whose effect is
// that nobody can be told why they were refused produces a support ticket
// rather than a fix.
//
// Neither carries anything worth withholding: build metadata, and a
// description of the caller's own permissions.
func AlwaysAllowed(r *http.Request) bool {
	switch r.URL.Path {
	case "/api/v1/whoami", "/api/v1/system/version":
		return true
	}
	return false
}

// RequiredFor states what a request needs.
func RequiredFor(r *http.Request) Requirement {
	req := Requirement{Action: actionForMethod(r), Product: productIn(r.URL.Path)}

	// Pushing configuration into somebody else's registry outlives the
	// request, which is why it is its own action rather than a corner of
	// operate. See ActionApply.
	if strings.HasSuffix(r.URL.Path, ":apply") {
		req.Action = ActionApply
	}

	// The routes that filter themselves. Keep this list and the filtering in
	// those handlers in step - see Requirement.AnyScope.
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/products":
		// handleListProducts filters by VisibleProducts.
		req.AnyScope = true
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/auditEvents":
		// auditProducts filters by VisibleProducts.
		req.AnyScope = true
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/transfers":
		// The product is in the BODY, which a middleware cannot read without
		// consuming it, so handleCreateTransfer re-asks with the product it
		// decoded. Without this a caller scoped to one product could not
		// request the one thing this system is for.
		req.AnyScope = true
	}
	return req
}

// actionForMethod is the default, and it is where fail-closed lives: a route
// nobody thought about still needs read to look and operate to change.
func actionForMethod(r *http.Request) Action {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return ActionRead
	default:
		return ActionOperate
	}
}

// productIn pulls the product out of /api/v1/products/{product}/...
//
// Read from the PATH rather than from chi's route parameters because this runs
// before routing: chi has not matched a pattern yet, so there is nothing to
// ask it for.
func productIn(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 4 || parts[0] != "api" || parts[1] != "v1" || parts[2] != "products" {
		return ""
	}
	seg := parts[3]
	// An AIP-136 custom method is a colon and a verb on the end of the
	// resource: `software-01:calibrate` names the product software-01. Split on
	// the LAST colon, so a name that somehow contains one keeps everything but
	// the verb.
	if i := strings.LastIndex(seg, ":"); i > 0 {
		seg = seg[:i]
	}
	if unescaped, err := url.PathUnescape(seg); err == nil {
		return unescaped
	}
	return seg
}

// Authorize refuses a request the caller's grants do not cover.
//
// deny writes the refusal, and is passed in so this package keeps knowing
// nothing about the API's error format.
func Authorize(deny func(http.ResponseWriter, *http.Request, string)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The data plane is governed by Confine, which has already run and
			// asked a different question of a different kind of caller.
			if WorkerPlane(r) || AlwaysAllowed(r) {
				next.ServeHTTP(w, r)
				return
			}
			id := IdentityFrom(r.Context())
			req := RequiredFor(r)

			// Scoped to the caller's OWN tenant, never to the estate: a grant
			// carries the tenant it was made in, and an estate-wide question is
			// the strictest there is (see Scope.covers), so asking one here
			// would refuse the very identity that holds the role.
			if id.Can(req.Action, Scope{Tenant: id.Tenant, Product: req.Product}) {
				next.ServeHTTP(w, r)
				return
			}
			if req.AnyScope && id.CanAny(req.Action) {
				next.ServeHTTP(w, r)
				return
			}
			deny(w, r, Refusal(id, req))
		})
	}
}

// Refusal is what the caller is told, and the two cases are worth telling
// apart.
//
// "You hold no roles" is a different problem from "you hold the wrong ones",
// and only the first has an answer the person can act on themselves. It is
// also by far the likelier one: an account provisioned by nobody, created by
// the identity provider at a first sign-in, is the shape this whole gate was
// written for.
func Refusal(id Identity, req Requirement) string {
	if len(id.Grants) == 0 && len(id.Roles) == 0 {
		return "This account holds no roles, so it may not read or change anything here. " +
			"Roles are granted in the identity provider; ask an administrator to grant one, " +
			"then sign out and in again."
	}
	what := "this"
	if req.Product != "" {
		what = "the product " + req.Product
	}
	return "This account may not " + string(req.Action) + " " + what + "."
}
