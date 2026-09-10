package middleware

import (
	"context"
	"sort"

	"github.com/abhijeet-oxide/softwareGateway/pkg/authz"
)

// THE PERMISSION CATALOGUE: every distinct thing a caller may be allowed to do,
// named once, in the vocabulary the policies are written in.
//
// # Why a catalogue exists at all
//
// Because an interface has to render itself from something, and the two
// alternatives are both worse.
//
// The first is for the browser to decide from ROLES: "if you hold
// product-owner, show Discover". That is the role model reimplemented in
// TypeScript, in a place nobody reviews against config/access/policies, and it
// drifts the first time a policy changes - which shows up as a button that is
// offered and then refused, or one that is hidden from somebody who is
// perfectly entitled to it. Both were happening here.
//
// The second is for the browser to guess from the coarse verbs read/operate/
// apply/admin. That is what this had, and it cannot express the estate
// boundary: `operate` is true for a product owner, and "run a fleet-wide scan"
// and "run a scan on my product" are the same word to it.
//
// So the server publishes the SAME questions it enforces, answered by the SAME
// authority - the policy engine when one is configured - and the interface
// renders what it is told. This is the ordinary shape: a permission-discovery
// endpoint whose entries are the enforcement points themselves, not a summary
// of them.
//
// # It is not a security control
//
// Nothing here decides anything. Authorize decides, on every request, from the
// same catalogue; a caller who forges a permission set in their browser gets a
// screen full of buttons that all answer 403. What this removes is the
// interface offering work the API will refuse, and hiding work it would allow.
//
// # Keeping it in step
//
// Every entry names a Kind and Action that PolicyFor already produces for some
// route, and a rule that exists in config/access/policies. Both directions are
// tested: permissions_test.go asserts that every route's (kind, action) pair
// appears here, so a route added without a permission fails the build rather
// than silently becoming invisible in the interface.

// Permission is one thing a caller may be allowed to do, named
// "<resource>.<action>" - `product.discover`, `audit_event.view`.
//
// The dotted form is what crosses the wire and what the browser branches on.
// It is stable API: renaming one is a breaking change to every client, in
// exactly the way renaming an HTTP route is.
type Permission string

const (
	PermProductView              Permission = "product.view"
	PermProductDiscover          Permission = "product.discover"
	PermProductCalibrate         Permission = "product.calibrate"
	PermProductCheckConnectivity Permission = "product.check_connectivity"

	PermPackageView    Permission = "package.view"
	PermPackageInspect Permission = "package.inspect"

	PermDownloadView    Permission = "software_download.view"
	PermDownloadRequest Permission = "software_download.request"
	PermDownloadRetry   Permission = "software_download.retry"
	PermDownloadCancel  Permission = "software_download.cancel"
	PermDownloadPromote Permission = "software_download.promote"
	PermDownloadApply   Permission = "software_download.apply"

	PermDownloadRuleView Permission = "download_rule.view"

	PermReplicationView       Permission = "replication.view"
	PermReplicationSync       Permission = "replication.sync"
	PermReplicationCancelSync Permission = "replication.cancel_sync"
	PermReplicationApply      Permission = "replication.apply"

	PermSecurityView   Permission = "security_report.view"
	PermSecurityExport Permission = "security_report.export"

	PermComplianceView   Permission = "compliance_report.view"
	PermComplianceExport Permission = "compliance_report.export"
	PermComplianceRun    Permission = "compliance_report.run"
	PermComplianceCancel Permission = "compliance_report.cancel"

	PermAuditView Permission = "audit_event.view"

	PermReportView  Permission = "report.view"
	PermWorkerView  Permission = "worker.view"
	PermPolicyView  Permission = "policy_catalogue.view"
	PermSystemView  Permission = "system.view"
	PermSystemWrite Permission = "system.write"
)

// PermissionDef is one catalogue entry.
type PermissionDef struct {
	Name Permission
	// Kind and Action are the question asked of the policy engine, and they
	// are exactly what PolicyFor produces for the routes this permission
	// governs.
	Kind   string
	Action string
	// Scoped says this permission can be held on ONE product.
	//
	// False marks an ESTATE permission - the fleet, the rollups, the rulebook,
	// the deployment itself - which has no product tier by design (see
	// config/access/policies/report.yaml). Asking about one per product would
	// invent a narrower grant than any policy can express, and an interface
	// reading that answer would offer a product owner the whole estate.
	Scoped bool
	// Title says what holding it lets somebody do, as a verb phrase, so a
	// refusal can name the thing that was refused rather than a resource kind
	// and an underscore.
	Title string
	// Fallback is the coarse action this permission implies when a deployment
	// runs with authentication but NO policy engine. It is the same ladder
	// Identity.Can uses, and it is consulted in exactly the same case
	// Authorize consults it.
	Fallback Action
}

// Catalogue is every permission, in the order an interface would read them.
var Catalogue = []PermissionDef{
	{PermProductView, "product", "view", true, "view products", ActionRead},
	{PermProductDiscover, "product", "discover", true, "run discovery", ActionOperate},
	{PermProductCalibrate, "product", "calibrate", true, "measure a transfer path", ActionOperate},
	{PermProductCheckConnectivity, "product", "check_connectivity", true, "probe a registry", ActionOperate},

	{PermPackageView, "package", "view", true, "view releases", ActionRead},
	{PermPackageInspect, "package", "inspect", true, "inspect a release", ActionOperate},

	{PermDownloadView, "software_download", "view", true, "view downloads", ActionRead},
	{PermDownloadRequest, "software_download", "request", true, "request a download", ActionOperate},
	{PermDownloadRetry, "software_download", "retry", true, "retry a download", ActionOperate},
	{PermDownloadCancel, "software_download", "cancel", true, "pause, resume or stop a download", ActionOperate},
	{PermDownloadPromote, "software_download", "promote", true, "promote a release", ActionApply},
	{PermDownloadApply, "software_download", "apply", true, "apply a download to a registry", ActionApply},

	{PermDownloadRuleView, "download_rule", "view", true, "view automatic download rules", ActionRead},

	{PermReplicationView, "replication", "view", true, "view mirror configuration", ActionRead},
	{PermReplicationSync, "replication", "sync", true, "sync a mirror", ActionOperate},
	{PermReplicationCancelSync, "replication", "cancel_sync", true, "stop a mirror sync", ActionOperate},
	{PermReplicationApply, "replication", "apply", true, "apply mirror configuration", ActionApply},

	{PermSecurityView, "security_report", "view", true, "view security findings", ActionRead},
	{PermSecurityExport, "security_report", "export", true, "export security findings", ActionRead},

	{PermComplianceView, "compliance_report", "view", true, "view compliance verdicts", ActionRead},
	{PermComplianceExport, "compliance_report", "export", true, "export compliance verdicts", ActionRead},
	{PermComplianceRun, "compliance_report", "run", true, "run a compliance check", ActionOperate},
	{PermComplianceCancel, "compliance_report", "cancel", true, "stop a compliance check", ActionOperate},

	// The audit trail is the one estate resource that CAN be narrowed: the
	// handler filters it by product, so a product reader sees their own
	// products' events and nobody else's. See config/access/policies/audit.yaml.
	{PermAuditView, "audit_event", "view", true, "view the audit trail", ActionRead},

	// The estate. No product tier, deliberately - see PermissionDef.Scoped.
	{PermReportView, "report", "view", false, "view reports", ActionRead},
	{PermWorkerView, "worker", "view", false, "view the worker fleet", ActionRead},
	{PermPolicyView, "policy_catalogue", "view", false, "view the policy catalogue", ActionRead},
	{PermSystemView, "system", "view", false, "view deployment settings", ActionRead},
	{PermSystemWrite, "system", "write", false, "change the deployment", ActionAdmin},
}

// byQuestion indexes the catalogue by the question a route asks.
var byQuestion = func() map[[2]string]PermissionDef {
	out := make(map[[2]string]PermissionDef, len(Catalogue))
	for _, def := range Catalogue {
		out[[2]string{def.Kind, def.Action}] = def
	}
	return out
}()

// PermissionFor names the permission a route needs, which is what a refusal
// quotes and what an interface hides the control on.
//
// The second return is false for a (kind, action) pair no catalogue entry
// covers. That is a route added without a permission, and the caller says so
// in general terms rather than inventing a name - the test in
// permissions_test.go is what stops it reaching a deployment.
func PermissionFor(kind, action string) (PermissionDef, bool) {
	def, ok := byQuestion[[2]string{kind, action}]
	return def, ok
}

// AccessSet is what a caller may do, as the interface reads it.
//
// TWO LISTS, NOT ONE, and the split is the same one Scope.covers makes: a
// tenant-wide permission covers every product including ones created
// tomorrow, and a product permission covers the product it names. Flattened
// into a single list they read as every verb on every product, which is how an
// interface ends up offering a promotion on a product somebody may only read.
type AccessSet struct {
	// Global is held tenant-wide: over every product, and over the estate
	// resources that have no product.
	Global []string `json:"global"`
	// ByProduct is held on ONE product, keyed by product name. Global
	// permissions are not repeated into it.
	ByProduct map[string][]string `json:"byProduct,omitempty"`
	// Unavailable says the policy engine could not be reached, so this set is
	// EMPTY because nothing could be resolved rather than because nothing is
	// held.
	//
	// Stated rather than left to be inferred: an interface that cannot tell
	// the two apart shows a person with every permission a screen saying they
	// have none, and the fix for that is a sentence about the policy engine,
	// not about their account.
	Unavailable bool `json:"unavailable,omitempty"`
}

// ResolveAccess computes the caller's whole permission set.
//
// # Who answers
//
// The same authority that enforces: the policy engine when one is configured,
// the role ladder when one is not, asked the same way Authorize asks. That is
// the only property that makes this worth publishing - a second implementation
// of "what may this person do" would be wrong the first time a policy changed,
// and wrong in the direction of offering work the API refuses.
//
// # What is asked
//
// Two passes. The tenant-wide question first, once for every permission: it is
// what an org-tier role answers, and it is the answer that covers products
// that do not exist yet. Then one pass per product the caller holds a
// product-tier role on, for the scoped permissions only, with the tenant-wide
// answers subtracted - so ByProduct carries what is held ON that product and
// nothing that was already true everywhere.
//
// It is one round trip to the engine per pass, not one per permission. See
// authz.CheckMany.
func ResolveAccess(ctx context.Context, engine authz.Engine, id Identity) AccessSet {
	if engine == nil {
		return resolveFromLadder(id)
	}
	principal := id.principal()

	global, err := checkAll(ctx, engine, principal, "", Catalogue)
	if err != nil {
		// Fails closed, and says so. An engine that cannot answer means the
		// API is refusing everything anyway (see Authorize), so an interface
		// that offered controls here would offer nothing but 403s.
		return AccessSet{Global: []string{}, Unavailable: true}
	}

	scoped := make([]PermissionDef, 0, len(Catalogue))
	for _, def := range Catalogue {
		if def.Scoped && !contains(global, string(def.Name)) {
			scoped = append(scoped, def)
		}
	}

	byProduct := map[string][]string{}
	for _, product := range id.ProductNames() {
		held, err := checkAll(ctx, engine, principal, product, scoped)
		if err != nil {
			return AccessSet{Global: []string{}, Unavailable: true}
		}
		if len(held) > 0 {
			byProduct[product] = held
		}
	}
	if len(byProduct) == 0 {
		byProduct = nil
	}
	return AccessSet{Global: global, ByProduct: byProduct}
}

// checkAll asks the engine about every permission in one exchange and returns
// the ones it allowed, sorted so the answer is stable between calls.
//
// # One entry per RESOURCE, not one per permission
//
// Cerbos answers a list of resources, each with a list of actions, and the four
// `product.*` permissions are four actions on ONE resource. Sending them as
// four identical resource entries asks the same question four times, and asks a
// PDP whose default request limit is fifty resources to hold a list that grows
// with the catalogue rather than with the number of things in it. Grouped, the
// whole catalogue is thirteen entries whatever it grows to.
func checkAll(
	ctx context.Context, engine authz.Engine, principal authz.Identity,
	product string, defs []PermissionDef,
) ([]string, error) {
	if len(defs) == 0 {
		return []string{}, nil
	}

	// Grouped, in first-seen order so the request is deterministic and a
	// captured exchange diffs cleanly between runs.
	var kinds []string
	actions := map[string][]string{}
	for _, def := range defs {
		if _, seen := actions[def.Kind]; !seen {
			kinds = append(kinds, def.Kind)
		}
		actions[def.Kind] = append(actions[def.Kind], def.Action)
	}

	queries := make([]authz.Query, 0, len(kinds))
	for _, kind := range kinds {
		queries = append(queries, authz.Query{
			Resource: authz.Resource{Kind: kind, ID: "*", Product: product},
			Actions:  actions[kind],
		})
	}
	answers, err := authz.CheckMany(ctx, engine, principal, queries)
	if err != nil {
		return nil, err
	}

	allowed := map[string]map[string]bool{}
	for i, kind := range kinds {
		if i < len(answers) {
			allowed[kind] = answers[i]
		}
	}
	held := make([]string, 0, len(defs))
	for _, def := range defs {
		if allowed[def.Kind][def.Action] {
			held = append(held, string(def.Name))
		}
	}
	sort.Strings(held)
	return held, nil
}

// resolveFromLadder answers from the coarse role ladder, for a deployment
// running with authentication and no policy engine.
//
// Deliberately the SAME fallback Authorize takes in that configuration, down
// to the scope each question is asked with. An interface driven by a different
// approximation of the ladder than the one the server enforces would disagree
// with it exactly where it matters.
func resolveFromLadder(id Identity) AccessSet {
	global := make([]string, 0, len(Catalogue))
	for _, def := range Catalogue {
		if id.Can(def.Fallback, Scope{Tenant: id.Tenant}) {
			global = append(global, string(def.Name))
		}
	}
	sort.Strings(global)

	byProduct := map[string][]string{}
	for _, product := range id.ProductNames() {
		var held []string
		for _, def := range Catalogue {
			if !def.Scoped || contains(global, string(def.Name)) {
				continue
			}
			if id.Can(def.Fallback, Scope{Tenant: id.Tenant, Product: product}) {
				held = append(held, string(def.Name))
			}
		}
		if len(held) > 0 {
			sort.Strings(held)
			byProduct[product] = held
		}
	}
	if len(byProduct) == 0 {
		byProduct = nil
	}
	return AccessSet{Global: global, ByProduct: byProduct}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
