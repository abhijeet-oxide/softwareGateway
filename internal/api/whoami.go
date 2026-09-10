package api

import (
	"net/http"

	"github.com/abhijeet-oxide/softwareGateway/internal/api/middleware"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// GET /api/v1/whoami - who is calling, and what may they do.
//
// # This is not authentication
//
// It reports whatever the installed Authenticator established, which today is
// the AnonymousAuthenticator's admin-for-everyone. `authenticated` is false and
// says so plainly, so a client shows "Authentication is not enabled" rather
// than implying a session that does not exist.
//
// # Why it exists before there is anything to report
//
// So that no client hardcodes a role model. A UI that decides for itself which
// controls an operator may use has to be edited everywhere the day real roles
// arrive. A UI that renders what this endpoint reports does not - the resolver
// behind it changes, and every page keeps working.
//
// Registered unconditionally: a caller must always be able to find out that
// they are anonymous.

func (s *Server) handleWhoAmI(w http.ResponseWriter, r *http.Request) {
	id := middleware.IdentityFrom(r.Context())

	out := v1.WhoAmIResponse{
		Subject:            id.Subject,
		Name:               id.Name,
		Email:              id.Email,
		Method:             id.Method,
		Authenticated:      id.Method != "" && id.Method != "none",
		Tenant:             id.Tenant,
		Products:           id.VisibleProducts(),
		ProductRoles:       id.ProductRoles,
		Member:             id.IsMember(),
		Permissions:        permissionsFor(id),
		ProductPermissions: productPermissionsFor(id),
		Access:             s.accessFor(r, id),
		Features: v1.Features{
			FileDownloads: s.deps.FileDownloadsEnabled,
		},
	}
	for _, role := range id.Roles {
		out.Roles = append(out.Roles, string(role))
	}
	WriteJSON(w, r, http.StatusOK, out)
}

// permissionsFor lists the actions this identity may take TENANT-WIDE.
//
// Tenant-wide, not estate-wide: an empty scope is the strictest question there
// is (scope.go), which a grant carrying a tenant deliberately cannot answer -
// asking it here once reported nothing for a user holding org-admin over
// everything they could see, and the interface disabled every control for the
// most privileged person in the system.
//
// And tenant-wide rather than a union across every scope, which is the other
// way to get this wrong. A caller who reads product A and owns product B holds
// four verbs and two products; flattened into one list they read as four verbs
// on both products, and the interface offers actions on A that the server then
// refuses. Where a verb applies to one product, it belongs in
// productPermissionsFor - and the two are read together, exactly as
// Scope.covers reads them.
//
// `*` means UNRESTRICTED and is decided here, tenant-wide, because that is
// what a client short-circuits on before it narrows by product.
func permissionsFor(id middleware.Identity) []string {
	out := make([]string, 0, len(reportedActions))
	for _, a := range reportedActions {
		if id.Can(a, middleware.Scope{Tenant: id.Tenant}) {
			out = append(out, string(a))
		}
	}
	if len(out) == len(reportedActions) {
		return []string{"*"}
	}
	return out
}

// productPermissionsFor lists the actions this identity may take on each
// product it holds anything on.
//
// Tenant-wide permissions are not copied in. They already cover every product,
// including products that do not exist yet, and copying them into a map keyed
// by today's products would quietly turn "covers everything" into "covers
// these" - which is the tier boundary this whole model exists to keep.
func productPermissionsFor(id middleware.Identity) map[string][]string {
	var out map[string][]string
	for _, product := range id.ProductNames() {
		var verbs []string
		for _, a := range reportedActions {
			if id.Can(a, middleware.Scope{Tenant: id.Tenant, Product: product}) {
				verbs = append(verbs, string(a))
			}
		}
		if len(verbs) == 0 {
			continue
		}
		if out == nil {
			out = map[string][]string{}
		}
		out[product] = verbs
	}
	return out
}

// accessFor resolves the caller's whole permission set, in the vocabulary the
// policies are written in.
//
// # Why the interface is told this and not left to work it out
//
// Because the alternative is the role model reimplemented in the browser, and
// it drifts the first time a policy changes - as a control offered and then
// refused, or hidden from somebody entitled to it. Publishing the answer from
// the same authority that enforces it is what makes the two agree by
// construction. See middleware.ResolveAccess.
//
// # Why it is on /whoami and not an endpoint of its own
//
// Because it is answered for the caller about the caller, it is read exactly
// once per session at the same moment as the rest of the identity, and a
// second always-allowed endpoint would be a second round trip before anything
// can be drawn. It is the same document: who you are, and what that gets you.
func (s *Server) accessFor(r *http.Request, id middleware.Identity) v1.AccessSet {
	// ANONYMOUS TAKES THE LADDER, not the engine, and this mirrors Authorize
	// exactly. A deployment with authentication off holds admin and has no
	// roles a policy could match, so asking the engine would report an empty
	// permission set for a caller the API lets do everything - an interface
	// with every control hidden in front of an API that refuses nothing.
	engine := s.deps.Engine
	if id.Method == "" || id.Method == "none" {
		engine = nil
	}
	set := middleware.ResolveAccess(r.Context(), engine, id)
	out := v1.AccessSet{
		Global:      set.Global,
		ByProduct:   set.ByProduct,
		Unavailable: set.Unavailable,
	}
	if out.Global == nil {
		out.Global = []string{}
	}
	return out
}

// The actions a client is told about. ActionWork is not among them: it is the
// data plane's, held by no person, and a worker does not render a screen.
var reportedActions = []middleware.Action{
	middleware.ActionRead,
	middleware.ActionOperate,
	middleware.ActionApply,
	middleware.ActionAdmin,
}
