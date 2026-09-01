package cel

import (
	"regexp"
	"strings"
)

// Heuristics: the checks that cannot be exact, kept honest.
//
// # Why these are here and not in a check
//
// Two of the source catalogue's requirements - "selectors use stable labels"
// and "no credential-shaped material in configuration" - are not decidable.
// They are patterns, they have false positives, and the catalogue says so: both
// are lowered from block to warn for exactly that reason.
//
// What a shared implementation buys is a single, reviewable false-positive
// budget. Written per check, the same heuristic drifts into five spellings and
// nobody can answer "what does this tool consider a credential?" - which is the
// first question a vendor asks when it is wrong about one.

// unstableLabelKeys are label keys whose value changes between releases of the
// same software.
//
// A selector matching on one of these is the defect MTA-03 exists for:
// Deployment.spec.selector is immutable after creation, so a chart whose
// selector contains the app version cannot be upgraded at all - only deleted
// and recreated, which for a StatefulSet means losing its volumes.
var unstableLabelKeys = map[string]bool{
	"app.kubernetes.io/version":          true,
	"helm.sh/chart":                      true,
	"pod-template-hash":                  true,
	"controller-revision-hash":           true,
	"statefulset.kubernetes.io/pod-name": true,
}

var unstableLabelFragments = []string{"build", "commit", "sha", "revision", "timestamp"}

// IsUnstableLabelKey reports whether a key's value is expected to change
// between releases.
func IsUnstableLabelKey(key string) bool {
	if unstableLabelKeys[key] {
		return true
	}
	lower := strings.ToLower(key)
	// Compare the key's own name, not its prefix: a domain called
	// "build.acme.example" is an organization's namespace, not a build ID, and
	// flagging every label under it would make this useless there.
	if i := strings.IndexByte(lower, '/'); i >= 0 {
		lower = lower[i+1:]
	}
	for _, f := range unstableLabelFragments {
		if strings.Contains(lower, f) {
			return true
		}
	}
	return false
}

var (
	hexish     = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	rfc3339ish = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}[Tt ]\d{2}:\d{2}`)
)

// LooksGenerated reports whether a label VALUE looks like it changes on every
// build.
//
// # The false-positive budget, stated
//
// A 7-to-40-character all-hex string is a commit SHA in every case anybody has
// seen, and is also a legitimate value somebody could have chosen. The check
// that uses this is a warning for that reason, and the finding quotes the value
// so a reviewer settles it in five seconds rather than trusting the tool.
//
// Deliberately NOT included: anything containing a digit, which would flag
// every version-like value and make the check worthless.
func LooksGenerated(v string) bool {
	if v == "" {
		return false
	}
	return hexish.MatchString(v) || rfc3339ish.MatchString(v)
}

// credentialKeyPattern matches a key whose value should not be a literal.
//
// Keyed on the NAME rather than the value, because a value cannot be recognized
// as a password - that is the property that makes it a good one. What can be
// recognized is somebody calling a field a password and then writing one in.
var credentialKeyPattern = regexp.MustCompile(
	`(?i)(^|[._\-])(passwd|password|secret|token|apikey|api[_\-]?key|access[_\-]?key|secret[_\-]?key|private[_\-]?key|credential|auth|bearer|session[_\-]?key)([._\-]|$)`)

// keyIsNotACredential are the near-misses that would otherwise fire constantly.
//
// Each one is a real field name from a real chart. Without them the check
// reports every OIDC issuer URL and every secretKeyRef as a leaked credential,
// and after the third of those nobody reads the report - which is the actual
// failure mode a compliance tool has.
var keyIsNotACredential = regexp.MustCompile(
	`(?i)(secretname|secretref|secretkeyref|passwordkey|tokenkey|authurl|authorizationurl|authmode|authtype|authenticationmethod|secretpath|tokenpath|keyalgorithm|keysize|passwordpolicy|tokenexpiry|tokenttl|secretengine|existingsecret|authproxy)`)

// placeholderValues are the defaults a vendor ships meaning "change this", and
// which reach production unchanged often enough that SUP-10 exists.
var placeholderValues = map[string]bool{
	"changeme": true, "change-me": true, "change_me": true,
	"admin": true, "password": true, "passw0rd": true, "p@ssw0rd": true,
	"secret": true, "letmein": true, "test123": true, "default": true,
	"root": true, "toor": true, "guest": true, "welcome": true,
	"123456": true, "12345678": true, "qwerty": true, "notsecure": true,
}

// LooksLikeCredential reports whether a key/value pair is a credential written
// in plain text.
//
// A reference to a credential is not a credential: `passwordSecretRef` names a
// Secret and is exactly what a chart SHOULD do, so flagging it would punish the
// correct pattern. An empty value is not a credential either - it is a
// placeholder for one, which is the shape a well-behaved chart ships.
func LooksLikeCredential(key, value string) bool {
	if value == "" {
		return false
	}
	if keyIsNotACredential.MatchString(key) {
		return false
	}
	if !credentialKeyPattern.MatchString(key) {
		return false
	}
	// A value that is itself a reference or a template is not a literal.
	if strings.HasPrefix(value, "$") || strings.Contains(value, "{{") {
		return false
	}
	return true
}

// IsPlaceholderCredential reports a value that is a known default.
//
// Separate from LooksLikeCredential because it is a different finding with a
// different fix: one says "you shipped a secret", the other says "you shipped
// the word 'changeme' and somebody will not".
func IsPlaceholderCredential(value string) bool {
	return placeholderValues[strings.ToLower(strings.TrimSpace(value))]
}

// extendedResourceExceptions are the resource names Kubernetes handles itself.
// Everything else in a requests or limits map is an extended resource, and
// Kubernetes requires request and limit to be equal for those - a rule NET-10
// exists because charts routinely get it wrong for SR-IOV and GPU resources.
var extendedResourceExceptions = map[string]bool{
	"cpu": true, "memory": true, "ephemeral-storage": true,
}

// IsExtendedResource reports whether a resource name is one the scheduler
// treats as an extended resource, where request must equal limit.
//
// Hugepages are included: they behave the same way and are the case that
// matters most for a CNF dataplane.
func IsExtendedResource(name string) bool {
	if extendedResourceExceptions[name] {
		return false
	}
	return name != ""
}

// operationalPaths are the URL prefixes that are unauthenticated by convention
// because they are meant to be reachable only from inside the cluster.
//
// Published through an Ingress, a metrics endpoint is an inventory of the
// deployment and a debug endpoint is usually a profiler that will dump the
// heap to anybody who asks. The list is shared so NET-06 and the observability
// checks agree about what counts as one.
var operationalPaths = []string{
	"/metrics", "/debug", "/actuator", "/admin", "/-/", "/pprof", "/env", "/heapdump",
}

// IsOperationalPath reports whether a URL path exposes an operational endpoint.
//
// A prefix match, because /actuator/health is the same surface as /actuator,
// and a chart routing the parent path has published all of it. The empty path
// is not a match: an Ingress with no path serves the application.
func IsOperationalPath(p string) bool {
	if p == "" || p == "/" {
		return false
	}
	lower := strings.ToLower(p)
	for _, op := range operationalPaths {
		if strings.HasPrefix(lower, op) {
			return true
		}
	}
	return false
}

// RuleGrants reports whether one RBAC rule grants any of `verbs` on
// `resource`.
//
// # Why this is not a string comparison in each check
//
// A rule saying `resources: ["*"]` grants secrets. A rule saying
// `verbs: ["*"]` grants list. To Kubernetes the wildcard is not a special case
// to handle - it is the ordinary meaning of the field - so a check comparing
// against the literal "secrets" reports that a role granting everything does
// not read secrets.
//
// That was the state of the RBAC checks in this pack until the fixture caught
// it, and the failure is instructive: a wildcard rule is caught by RBAC-03
// anyway, so the release is blocked either way and nobody would notice. But a
// reader seeing "RBAC-05 passed" would conclude that secrets are protected,
// and for a rule with a wildcard resource and narrow verbs that conclusion is
// wrong. A check that is right for the wrong reason is a check that will be
// wrong later.
//
// Subresources are matched by prefix in one direction only: a rule on `pods`
// does NOT grant `pods/exec` - Kubernetes treats them as separate resources -
// but a rule on `*` does.
func RuleGrants(resources, verbs []string, wantResource string, wantVerbs []string) bool {
	if !grantsResource(resources, wantResource) {
		return false
	}
	for _, want := range wantVerbs {
		for _, have := range verbs {
			if have == "*" || have == want || want == "*" {
				return true
			}
		}
	}
	return false
}

// grantsResource matches in both directions. A rule on "*" grants the resource
// asked about; asking about "*" matches any rule, which is how a check says
// "this verb, on anything at all" - impersonate is a permission regardless of
// what it names.
func grantsResource(resources []string, want string) bool {
	if want == "*" {
		return len(resources) > 0
	}
	for _, r := range resources {
		if r == "*" || r == want {
			return true
		}
	}
	return false
}

// RBACWriteVerbs are the verbs that let a subject change an object. Named here
// so RBAC-06 and any pack that needs the same list agree about what "write"
// means - bind and escalate included, because both grant permissions without
// looking like it.
var RBACWriteVerbs = []string{
	"create", "update", "patch", "delete", "deletecollection", "bind", "escalate",
}
