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
		Method:        id.Method,
		Authenticated: id.Method != "" && id.Method != "none",
		Tenant:        id.Tenant,
		Products:      id.VisibleProducts(),
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

// permissionsFor lists the actions this identity may take estate-wide.
//
// `*` rather than an enumeration when the caller may do everything, so a
// client has one thing to test for the unrestricted case instead of having to
// keep its list of actions in step with the server's.
func permissionsFor(id middleware.Identity) []string {
	all := []middleware.Action{
		middleware.ActionRead,
		middleware.ActionOperate,
		middleware.ActionApply,
		middleware.ActionAdmin,
	}

	out := make([]string, 0, len(all))
	for _, a := range all {
		if id.Can(a, middleware.Scope{}) {
			out = append(out, string(a))
		}
	}
	if len(out) == len(all) {
		return []string{"*"}
	}
	return out
}
