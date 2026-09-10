package middleware

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/abhijeet-oxide/softwareGateway/pkg/authz"
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
	// Kind and PolicyAction are the same request expressed in the policies'
	// vocabulary - `audit_event`/`view` - which is what names the permission a
	// refusal quotes. Filled by RequiredFor from PolicyFor, so the sentence a
	// caller reads and the question the engine was asked cannot disagree.
	Kind         string
	PolicyAction string
	// AnyScope permits a caller who holds the action on ANY product, rather
	// than tenant-wide.
	//
	// Set ONLY for routes whose handler narrows what it touches to
	// PermittedProducts (or, for a read, Identity.VisibleProducts). That
	// narrowing is the thing that makes the wider permission safe, so the two
	// must be changed together: marking a route AnyScope without narrowing it
	// hands a caller scoped to one product the contents of all of them.
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
	res := PolicyFor(r)
	req := Requirement{
		Action:       actionForMethod(r),
		Product:      productIn(r.URL.Path),
		Kind:         res.Kind,
		PolicyAction: res.Action,
	}

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
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/products:discover":
		// handleDiscoverAll scans PermittedProducts. A product owner asking for
		// a fleet-wide scan is asking about the fleet they hold, and refusing
		// them outright - which is what the estate-wide question does - left
		// the one control this product exists to offer disabled for the people
		// most likely to press it.
		req.AnyScope = true
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/products:checkConnectivity":
		// handleCheckConnectivity probes PermittedProducts, same rule.
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

// Authorize refuses a request the caller is not permitted to make.
//
// # Who decides
//
// The POLICY ENGINE, whenever one is configured, and it is the only decision -
// not a second opinion on top of the role ladder. Cerbos exists in this
// deployment precisely so that "who may do what" is data in
// config/access/policies, reviewable and changeable without a rebuild; asking
// it and then also consulting a hard-coded ladder would mean two answers that
// can disagree, and the one that shipped would be whichever was checked last.
//
// The ladder remains for a deployment with no engine. That is a real
// configuration - authentication without a PDP - and it is exactly the
// half-step docs/design/24 describes.
//
// # It fails closed on an engine that cannot answer
//
// An unreachable PDP means we cannot know whether this is allowed, and cannot
// know is not yes. That is a deliberate availability trade: a Cerbos outage
// refuses the API rather than opening it. Cerbos runs beside the Coordinator
// and its own health is reported separately, so the state is diagnosable.
//
// deny writes the refusal, and is passed in so this package keeps knowing
// nothing about the API's error format.
func Authorize(engine authz.Engine, deny func(http.ResponseWriter, *http.Request, string)) func(http.Handler) http.Handler {
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

			if engine != nil {
				// An anonymous deployment has authentication off and holds
				// admin; it has no roles a policy could match, and asking
				// would refuse every request on a stack that is deliberately
				// open. The engine governs identities that came from a token.
				if id.Method == "" || id.Method == "none" {
					next.ServeHTTP(w, r)
					return
				}
				allowed, permitted, err := decide(r, engine, id, req)
				switch {
				case err != nil:
					deny(w, r, "Access denied: the policy engine could not be reached, so this "+
						"request cannot be authorized. Nothing is permitted while that is true.")
				case allowed:
					next.ServeHTTP(w, withPermittedProducts(r, permitted))
				default:
					deny(w, r, Refusal(id, req))
				}
				return
			}

			// No engine: the role ladder, scoped to the caller's OWN tenant.
			// Never to the estate - a grant carries the tenant it was made in,
			// and an estate-wide question is the strictest there is (see
			// Scope.covers), so asking one here would refuse the very identity
			// that holds the role.
			if id.Can(req.Action, Scope{Tenant: id.Tenant, Product: req.Product}) {
				next.ServeHTTP(w, r)
				return
			}
			if req.AnyScope && id.CanAny(req.Action) {
				next.ServeHTTP(w, withPermittedProducts(r, ladderProducts(id, req.Action)))
				return
			}
			deny(w, r, Refusal(id, req))
		})
	}
}

// decide asks the policy engine about this request, and reports WHICH PRODUCTS
// it said yes for.
//
// One call in the ordinary case. The extra calls happen only for a caller who
// holds product-tier roles AND is on a route that acts across products, which
// is the one question a single check cannot express: "may you list products" is
// not "may you act on the estate", and a caller granted one product cannot
// answer the second while still needing to see the first. So the tenant-wide
// question is asked first - every org-tier caller passes there and stops - and
// only then is it re-asked once per product they actually hold.
//
// # Why the product list comes back
//
// Because the handler has to narrow, and until now it had to work out the
// narrowing for itself from the identity - a SECOND authorization decision,
// written in a different place, from different inputs, which is how a fleet
// -wide scan came to be refused to the owners of every product in the fleet.
// The middleware has already asked the engine product by product; the answer it
// got is exactly the list the handler needs, so it is passed on rather than
// recomputed. Empty means unrestricted: the tenant-wide question said yes.
func decide(r *http.Request, engine authz.Engine, id Identity, req Requirement) (bool, []string, error) {
	principal := id.principal()
	res := PolicyFor(r)

	ok, err := authz.Allowed(r.Context(), engine, principal, authz.Resource{
		Kind: res.Kind, ID: res.ID, Product: res.Product,
	}, res.Action)
	if err != nil || ok || !req.AnyScope {
		return ok, nil, err
	}

	var permitted []string
	for _, product := range principal.ProductNames() {
		ok, err := authz.Allowed(r.Context(), engine, principal, authz.Resource{
			Kind: res.Kind, ID: res.ID, Product: product,
		}, res.Action)
		if err != nil {
			return false, nil, err
		}
		if ok {
			permitted = append(permitted, product)
		}
	}
	// Sorted, because the handlers hand this to a person: a fleet-wide scan
	// that reports its products in map order reports them differently on every
	// call.
	sort.Strings(permitted)
	return len(permitted) > 0, permitted, nil
}

// ladderProducts is the same answer from the role ladder, for a deployment
// with no policy engine.
func ladderProducts(id Identity, action Action) []string {
	var out []string
	seen := map[string]bool{}
	for _, g := range id.Grants {
		if g.Action != action || g.Scope.Product == "" || seen[g.Scope.Product] {
			continue
		}
		seen[g.Scope.Product] = true
		out = append(out, g.Scope.Product)
	}
	sort.Strings(out)
	return out
}

type ctxKeyPermitted struct{}

// withPermittedProducts records the narrowing the authorization decision
// produced, for the handler to apply.
//
// Nothing is recorded when the list is empty, and that is the important half:
// empty means the caller passed the TENANT-WIDE question, which covers every
// product including ones created after they signed in. A handler that read an
// empty list as "no products" would show an org administrator nothing at all.
func withPermittedProducts(r *http.Request, products []string) *http.Request {
	if len(products) == 0 {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), ctxKeyPermitted{}, products))
}

// PermittedProducts is which products this request was authorized for.
//
// Empty means UNRESTRICTED - every product - which is what a tenant-wide role
// and an unauthenticated deployment both produce. It is the same convention
// Identity.VisibleProducts uses, so a handler that already narrows by one can
// narrow by the other without a second shape to think about.
//
// A handler on an AnyScope route MUST apply it. See Requirement.AnyScope.
func PermittedProducts(ctx context.Context) []string {
	if products, ok := ctx.Value(ctxKeyPermitted{}).([]string); ok {
		return products
	}
	return nil
}

// principal rebuilds the identity the policy engine takes.
//
// Rebuilt rather than carried, because this type is what every handler reads
// and adding a second copy of the same facts to it is how the two drift. The
// two role tiers are already here in the shape the derived roles expect: bare
// role names, and the products they were granted on.
func (i Identity) principal() authz.Identity {
	out := authz.Identity{
		Subject:  i.Subject,
		Tenant:   i.Tenant,
		Email:    i.Email,
		Name:     i.Name,
		Products: map[string][]string{},
		Method:   i.Method,
	}
	for _, r := range i.Roles {
		out.OrgRoles = append(out.OrgRoles, string(r))
	}
	for product, roles := range i.ProductRoles {
		out.Products[product] = append(out.Products[product], roles...)
	}
	return out
}

// Refusal is what the caller is told, and the three cases are worth telling
// apart.
//
// "Nobody has provisioned you", "you have been provisioned and given nothing
// yet" and "you hold the wrong roles for this" are three different problems.
// The first two have an answer the person can act on and the third mostly does
// not, and the first two are by far the likelier: an account created by the
// identity provider at a first sign-in, and an account added to
// config/users/users.yaml before anybody decided which products it should
// reach, are the two shapes this gate was written for.
//
// # The register
//
// Every one of these opens with "Access denied", names the PERMISSION that was
// required, and stops. That is the form an operator already knows from every
// other system they administer - a subject, a permission, a resource - and it
// is what makes a refusal quotable into a ticket without a translation step.
// The sentences these replaced described the request instead ("This account
// may not read this"), which named neither what was needed nor how to get it.
func Refusal(id Identity, req Requirement) string {
	if !id.IsMember() {
		return "Access denied: this account is not provisioned in this tenant and holds no " +
			"roles, so it may not read or change anything here. Roles are granted in the " +
			"identity provider; ask an administrator to grant one. A grant takes effect when " +
			"this session's access token is next renewed, within its lifetime; signing out " +
			"and in again applies it immediately."
	}
	// Provisioned, and holding nothing that reaches anything: the baseline
	// role and no other. Saying "you lack a permission" to them describes the
	// request rather than their situation, and their situation is the answer.
	if len(id.Grants) == 0 {
		return "Access denied: this account is provisioned but has not been granted access to " +
			"any product. Ask an administrator for access to the products you need. A grant " +
			"takes effect when this session's access token is next renewed, within its " +
			"lifetime; signing out and in again applies it immediately."
	}
	return "Access denied: this account does not have the " + requiredPermission(req) +
		" permission" + onWhat(req) + "."
}

// RefusalFor is the same sentence for a route, when the caller has the request
// rather than the requirement in hand.
func RefusalFor(id Identity, r *http.Request) string {
	return Refusal(id, RequiredFor(r))
}

// requiredPermission names the permission in the catalogue's vocabulary, which
// is the same string the interface hides the control on and the same string an
// administrator greps config/access/policies for.
//
// A route no catalogue entry covers falls back to the coarse action. That is a
// route added without a permission - permissions_test.go fails the build on
// one - and a refusal is not the place to invent a name for it.
func requiredPermission(req Requirement) string {
	if def, ok := PermissionFor(req.Kind, req.PolicyAction); ok {
		return string(def.Name)
	}
	return string(req.Action)
}

// onWhat names the resource the permission was needed on.
func onWhat(req Requirement) string {
	if req.Product != "" {
		return " on product " + strconv.Quote(req.Product)
	}
	return " for this resource"
}

// Permits answers ONE permission question with the same authority Authorize
// uses, for a handler that could not be asked earlier.
//
// # Why a handler ever asks at all
//
// Because a middleware cannot read a request body without consuming it. A
// download names its product in the BODY, so the gate in front of that route
// can only ask the weaker "may you operate on anything" - which is what lets a
// caller scoped to one product request the one thing this system is for - and
// the real question waits until the body is decoded. Skipping it there would
// let that caller start a transfer on any product in the estate.
//
// # Why it takes the engine
//
// So the second question is decided by whoever decided the first. A handler
// that consulted the role ladder while the gate consulted Cerbos would enforce
// a policy nobody wrote, and would do it only on the routes that ask twice.
// Nil engine takes the ladder, exactly as Authorize does.
//
// It fails closed: an engine that cannot answer is not a yes.
func Permits(ctx context.Context, engine authz.Engine, id Identity, req Requirement) (bool, error) {
	if engine == nil || id.Method == "" || id.Method == "none" {
		return id.Can(req.Action, Scope{Tenant: id.Tenant, Product: req.Product}), nil
	}
	return authz.Allowed(ctx, engine, id.principal(), authz.Resource{
		Kind: req.Kind, ID: "*", Product: req.Product,
	}, req.PolicyAction)
}
