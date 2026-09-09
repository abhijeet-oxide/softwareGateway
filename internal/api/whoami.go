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
		Subject:       id.Subject,
		Name:          id.Name,
		Email:         id.Email,
		Method:        id.Method,
		Authenticated: id.Method != "" && id.Method != "none",
		Tenant:        id.Tenant,
		Products:      id.VisibleProducts(),
		ProductRoles:  id.ProductRoles,
		Permissions:   permissionsFor(id),
		Features: v1.Features{
			FileDownloads: s.deps.FileDownloadsEnabled,
		},
	}
	for _, role := range id.Roles {
		out.Roles = append(out.Roles, string(role))
	}
	WriteJSON(w, r, http.StatusOK, out)
}

// permissionsFor lists the actions this identity may take ANYWHERE, leaving
// the client to narrow them by product.
//
// This is a list of verbs, not of doors. `Products` beside it says where they
// apply, and the client pairs the two exactly as the server does - an action
// with no product named is the estate-wide question, which a caller holding
// one product cannot answer (scope.go: "a narrow grant cannot answer a
// question that names nothing").
//
// Asking the narrow question HERE has now produced the same bug twice, in the
// one place where the answer is not a refused request but a locked door:
//
//   - with an EMPTY scope it reported nothing for a user holding org-admin
//     over everything they could see, and the UI disabled every control for the
//     most privileged person in the system.
//   - with the TENANT scope it reported nothing for a caller whose grants are
//     all product-scoped. Empty permissions is precisely what the SPA reads as
//     "this account is not enabled", so somebody granted product-owner on one
//     product - correctly, deliberately, and the only grant they need - signed
//     in and was shown a door with their own address on it. Adding any `org-`
//     role appeared to fix it and fixed it by making them tenant-wide.
//
// So the question asked is the one the answer is used for: CanAny, the same
// primitive a self-filtering listing uses.
//
// `*` still means UNRESTRICTED and is decided tenant-wide, because the client
// short-circuits on it before narrowing by product. A product-owner holds all
// four actions on their product and must not collapse to the same answer as an
// org-admin.
func permissionsFor(id middleware.Identity) []string {
	all := []middleware.Action{
		middleware.ActionRead,
		middleware.ActionOperate,
		middleware.ActionApply,
		middleware.ActionAdmin,
	}

	wide := 0
	for _, a := range all {
		if id.Can(a, middleware.Scope{Tenant: id.Tenant}) {
			wide++
		}
	}
	if wide == len(all) {
		return []string{"*"}
	}

	out := make([]string, 0, len(all))
	for _, a := range all {
		if id.CanAny(a) {
			out = append(out, string(a))
		}
	}
	return out
}
