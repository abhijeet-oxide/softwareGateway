package middleware

import (
	"net/http"
	"strings"
)

// What each route IS, in the vocabulary the policies are written in.
//
// # Why a table and not a check per handler
//
// The same argument as authorize.go: eighty checks are eighty chances to leave
// one out, silently. This is one function, and a route it does not name still
// gets a decision - the fallback at the bottom - rather than a pass.
//
// # Why these names
//
// They are the resource kinds in config/access/policies, which is where the
// decision is actually made. A kind here with no policy there is refused by
// Cerbos, because a policy that does not exist allows nothing; that is the
// correct direction to fail, and `make` will not catch it, so the two are kept
// in step by the test that walks every route in the router.

// PolicyResource is a route expressed as a policy question.
type PolicyResource struct {
	// Kind matches a Cerbos resourcePolicy, e.g. "software_download".
	Kind string
	// Action matches an action in that policy, e.g. "request".
	Action string
	// Product is the product named in the path, empty for an estate-wide
	// route. It becomes R.attr.product, which is what the product tier's
	// derived roles condition on.
	Product string
	// ID names the object, "*" for a collection.
	ID string
}

// PolicyFor turns a request into the question to ask the policy engine.
//
// The ORDER of these cases is the specification: the first match wins, so the
// narrow paths come before the broad ones. A `:verb` suffix is matched
// explicitly rather than by "is this a POST", because two verbs on one path
// can sit either side of a permission boundary - `replication:sync` moves
// content under configuration that is already there, `replication:apply`
// writes the configuration.
func PolicyFor(r *http.Request) PolicyResource {
	p := r.URL.Path
	product := productIn(p)
	write := r.Method != http.MethodGet && r.Method != http.MethodHead
	res := PolicyResource{Product: product, ID: "*"}

	verb := ""
	if i := strings.LastIndex(p, ":"); i > 0 {
		verb = p[i+1:]
	}

	switch {
	// ---- the estate: no product, and no product tier ----
	case strings.HasPrefix(p, "/api/v1/auditEvents"):
		res.Kind, res.Action = "audit_event", "view"
	case strings.HasPrefix(p, "/api/v1/reports"):
		res.Kind, res.Action = "report", "view"
	case strings.HasPrefix(p, "/api/v1/workers"):
		res.Kind, res.Action = "worker", "view"
	case strings.HasPrefix(p, "/api/v1/policies"):
		res.Kind, res.Action = "policy_catalogue", "view"
	// Discovery for the whole estate. `product`/`view` rather than a kind of
	// its own, because that is what the per-product route already asks and the
	// two return the same rows: a caller who may read a product's discovery
	// status may read it in a listing as well. Named here rather than left to
	// the fallback, which would judge it a tenant-wide `system` read and refuse
	// every product owner the Overview.
	case strings.HasPrefix(p, "/api/v1/discovery"):
		res.Kind, res.Action = "product", "view"
	// Releases across every product, which is what the Packages listing shows.
	// The same `package`/`view` the per-product route asks, for the same rows;
	// the handler narrows them to VisibleProducts, which is what makes the
	// wider door safe. Before this route existed the listing asked once per
	// product and joined the answers in the browser.
	case strings.HasPrefix(p, "/api/v1/packages"):
		res.Kind, res.Action = "package", "view"
	case strings.HasPrefix(p, "/api/v1/system"):
		res.Kind, res.Action = "system", "view"

	// ---- downloads, which the domain calls transfers ----
	case strings.HasPrefix(p, "/api/v1/transfers"):
		res.Kind = "software_download"
		switch {
		case verb == "retry":
			res.Action = "retry"
		case p == "/api/v1/transfers" && write:
			res.Action = "request"
		case write:
			// The custom methods on one transfer: pause, resume, cancel,
			// verify. All of them stop or re-drive work somebody asked for.
			res.Action = "cancel"
		default:
			res.Action = "view"
		}

	// ---- everything under a product ----
	case strings.HasPrefix(p, "/api/v1/products"):
		switch {
		// `/targets/` as well as `/replication`: a target's SYNCS live at
		// /targets/{t}/syncs and carry no "replication" segment at all, so
		// matching the word alone judged reading a mirror's history as an
		// ordinary product read.
		case strings.Contains(p, "/replication"), strings.Contains(p, "/targets/"):
			res.Kind = "replication"
			switch verb {
			case "apply":
				res.Action = "apply"
			case "sync":
				res.Action = "sync"
			case "cancelSync":
				res.Action = "cancel_sync"
			default:
				res.Action = "view"
			}
		case strings.Contains(p, "/security"):
			res.Kind, res.Action = "security_report", "view"
			if strings.HasSuffix(p, "/export") {
				res.Action = "export"
			}
		case strings.Contains(p, "/compliance"):
			res.Kind, res.Action = "compliance_report", "view"
			switch {
			case verb == "run":
				res.Action = "run"
			case verb == "cancel":
				res.Action = "cancel"
			case strings.HasSuffix(p, "/export"):
				res.Action = "export"
			}
		case strings.Contains(p, "/autoDownloadRules"):
			res.Kind, res.Action = "download_rule", "view"
		case strings.Contains(p, "/downloads"):
			res.Kind, res.Action = "software_download", "view"
			if write {
				res.Action = "request"
			}
		case strings.Contains(p, "/packages"):
			res.Kind, res.Action = "package", "view"
			switch {
			case verb == "discover":
				// Scanning a product's sources is a property of the PRODUCT,
				// not of any package: it is what finds them.
				res.Kind, res.Action = "product", "discover"
			case write:
				res.Action = "inspect"
			}
		default:
			res.Kind, res.Action = "product", "view"
			switch verb {
			case "discover":
				res.Action = "discover"
			case "calibrate":
				res.Action = "calibrate"
			case "checkConnectivity":
				res.Action = "check_connectivity"
			}
		}

	// ---- a comparison in flight, keyed by its own token ----
	case strings.HasPrefix(p, "/api/v1/comparisons"):
		res.Kind, res.Action = "package", "view"

	// ---- anything this table has never heard of ----
	//
	// Not a pass. `system` has one rule, allowing the tenant-wide reader tier
	// and nobody else, so an unmapped read is refused to a product-scoped
	// caller and an unmapped WRITE is refused to everybody - including an
	// administrator, loudly, which is the failure that gets a new route added
	// here rather than a permission quietly granted.
	default:
		res.Kind, res.Action = "system", "view"
		if write {
			res.Action = "write"
		}
	}
	return res
}
