package middleware

// Scoped authorization, defined BEFORE anything calls it.
//
// # Why this exists now
//
// `HasRole` has no callers. That is the whole reason this file is worth
// writing today rather than when authentication ships: the moment a handler
// starts asking "may this caller do this", it either asks a question that can
// carry a tenant and a product or it asks one that cannot. The flat question
// is cheaper to write and has to be unpicked in every handler later.
//
// So the shape lands first, backed today by the same permissive answer the
// AnonymousAuthenticator already gives. When identities become real, the
// resolver behind Can changes and its callers do not.
//
// See docs/design/09 §10 for the authentication seam this completes, and
// docs/design/19 §4 for why configuration is never what a caller is granted
// against — products come from Git, and authorization decides who may LOOK at
// them and REQUEST work on them.

// Action is a verb a caller may or may not be permitted.
//
// Deliberately coarse and named for what a user does, not for the route that
// implements it: "download this release" is one permission whether it takes
// one API call or four.
type Action string

const (
	// ActionRead covers every GET. The audit trail included — it is the most
	// sensitive read in the system and is exactly what a viewer role is for.
	ActionRead Action = "read"
	// ActionOperate covers requesting work: downloads, promotions, discovery,
	// retry, pause, resume, stop.
	ActionOperate Action = "operate"
	// ActionApply covers pushing configuration into a third-party registry —
	// the replication apply. Separate from operate because it changes
	// something outside this system that outlives the request.
	ActionApply Action = "apply"
	// ActionAdmin covers the worker plane and anything estate-wide.
	ActionAdmin Action = "admin"
)

// Scope is what an action is being attempted ON.
//
// Both fields empty means estate-wide — "list every product". A permission
// check against an empty scope is therefore the STRICTEST question, not the
// most permissive one, and a future resolver must treat it that way.
type Scope struct {
	Tenant  string
	Product string
}

// Grant is one permission a caller holds, over one scope.
//
// Empty Tenant or Product means "all of them", which is what makes a
// single-tenant deployment expressible without inventing a tenant name for it.
type Grant struct {
	Action Action
	Scope  Scope
}

// Can reports whether the identity may perform an action within a scope.
//
// Handlers call THIS, never HasRole. The difference is the whole point: a
// grant can be narrower than the estate, and a check that cannot express a
// product cannot be narrowed later without editing every caller.
//
// Today the answer comes from the identity's roles, so behaviour is identical
// to the flat model and no deployment changes. When grants become real, they
// are consulted first and roles remain the fallback for a caller that has
// none.
func (i Identity) Can(action Action, scope Scope) bool {
	for _, g := range i.Grants {
		if g.Action == action && g.Scope.covers(scope) {
			return true
		}
	}
	return i.HasRole(roleFor(action))
}

// covers reports whether a grant's scope includes the scope being asked about.
//
// An empty field on the GRANT is a wildcard. An empty field on the REQUEST is
// an estate-wide question, which only an estate-wide grant answers — so a
// caller granted one product cannot list the estate by asking about no product
// in particular.
func (g Scope) covers(want Scope) bool {
	if g.Tenant != "" && g.Tenant != want.Tenant {
		return false
	}
	if g.Product != "" && g.Product != want.Product {
		return false
	}
	if g.Tenant == "" && g.Product == "" {
		return true
	}
	// A narrow grant cannot answer a question that names nothing.
	return want.Tenant != "" || want.Product != ""
}

// roleFor maps an action onto the coarse role that implies it today.
func roleFor(action Action) Role {
	switch action {
	case ActionRead:
		return RoleViewer
	case ActionOperate, ActionApply:
		return RoleOperator
	default:
		return RoleAdmin
	}
}

// VisibleProducts is the product names this identity may see, for the store
// filters that take one.
//
// Empty means unrestricted, which is the convention every scoped store filter
// already uses — so an unauthenticated deployment passes nil and every query
// stays exactly as it is today.
func (i Identity) VisibleProducts() []string {
	if i.Can(ActionRead, Scope{}) {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, g := range i.Grants {
		if g.Action != ActionRead || g.Scope.Product == "" || seen[g.Scope.Product] {
			continue
		}
		seen[g.Scope.Product] = true
		out = append(out, g.Scope.Product)
	}
	return out
}
