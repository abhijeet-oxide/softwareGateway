package cel

import (
	"encoding/base64"
	"math"
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
	"app.kubernetes.io/version": true,
	"helm.sh/chart":             true,
	"pod-template-hash":         true,
	"controller-revision-hash":  true,
}

var unstableLabelFragments = []string{"build", "commit", "sha", "revision", "timestamp"}

// stableRuntimeIdentityKeys are labels Kubernetes puts on a pod that identify
// WHICH pod it is, and that do not change when the software is upgraded.
//
// # Why they are called out rather than left to the fragment test
//
// They are the correct, documented way to address one member of a clustered
// service: a headless Service per StatefulSet replica selects on
// `statefulset.kubernetes.io/pod-name`, and that value is `db-0` for the life
// of that replica, across every release. Treating it as release-varying
// reported the standard pattern for addressing a database member as an
// upgrade-blocking defect - on every such Service in a release, twice, from two
// different checks.
//
// The distinction that matters: a pod ORDINAL is stable, a pod TEMPLATE HASH is
// not. `pod-template-hash` and `controller-revision-hash` change on every
// rollout and stay in the unstable list above.
var stableRuntimeIdentityKeys = map[string]bool{
	"statefulset.kubernetes.io/pod-name": true,
	"apps.kubernetes.io/pod-index":       true,
	"batch.kubernetes.io/job-name":       true,
	"job-name":                           true,
}

// IsUnstableLabelKey reports whether a key's value is expected to change
// between releases of the same software.
func IsUnstableLabelKey(key string) bool {
	if stableRuntimeIdentityKeys[key] {
		return false
	}
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

// runtimeSuppliedLabelKeys are labels no chart writes and every pod has: the
// controller adds them when it creates the pod.
//
// # Why a check has to know about them
//
// A check asking "does this Service select a pod that exists?" compares the
// selector against the labels it can see, which are the ones in the rendered
// chart. A selector key the platform supplies at pod creation is not in the
// chart and never will be, so a naive comparison concludes the Service selects
// nothing - and reports a 100% error rate against a Service that is completely
// correct. Every such finding is false, and there is one for every per-replica
// Service in a clustered database.
var runtimeSuppliedLabelKeys = map[string]bool{
	"statefulset.kubernetes.io/pod-name": true,
	"apps.kubernetes.io/pod-index":       true,
	"controller-revision-hash":           true,
	"pod-template-hash":                  true,
	"pod-template-generation":            true,
	"batch.kubernetes.io/job-name":       true,
	"batch.kubernetes.io/controller-uid": true,
	"job-name":                           true,
	"controller-uid":                     true,
}

// IsRuntimeSuppliedLabelKey reports whether Kubernetes adds this label itself
// when it creates the pod, so its absence from a rendered chart says nothing.
func IsRuntimeSuppliedLabelKey(key string) bool { return runtimeSuppliedLabelKeys[key] }

// builtinAPIGroups are the API groups a cluster serves without anybody
// installing a CustomResourceDefinition for them.
//
// # Why this list exists
//
// The check that asks "does this release ship the definition for the custom
// resource types it uses?" has to know what a custom resource IS. Everything
// with a slash in its apiVersion looks like one to a naive test, so the check
// reported that a release must ship a definition for `PodDisruptionBudget` -
// a built-in type that can never have one, and for which the finding has no
// possible fix. The list is here rather than spelled as twelve startsWith
// clauses in the check, because it is a fact about Kubernetes and OpenShift and
// it should have exactly one spelling.
var builtinAPIGroups = map[string]bool{
	"":                                  true,
	"apps":                              true,
	"batch":                             true,
	"policy":                            true,
	"autoscaling":                       true,
	"networking.k8s.io":                 true,
	"rbac.authorization.k8s.io":         true,
	"apiextensions.k8s.io":              true,
	"apiregistration.k8s.io":            true,
	"admissionregistration.k8s.io":      true,
	"authentication.k8s.io":             true,
	"authorization.k8s.io":              true,
	"certificates.k8s.io":               true,
	"coordination.k8s.io":               true,
	"discovery.k8s.io":                  true,
	"events.k8s.io":                     true,
	"flowcontrol.apiserver.k8s.io":      true,
	"node.k8s.io":                       true,
	"scheduling.k8s.io":                 true,
	"storage.k8s.io":                    true,
	"resource.k8s.io":                   true,
	"route.openshift.io":                true,
	"security.openshift.io":             true,
	"apps.openshift.io":                 true,
	"image.openshift.io":                true,
	"build.openshift.io":                true,
	"template.openshift.io":             true,
	"project.openshift.io":              true,
	"config.openshift.io":               true,
	"operator.openshift.io":             true,
	"machineconfiguration.openshift.io": true,
	"monitoring.coreos.com":             true,
	"k8s.cni.cncf.io":                   true,
}

// IsBuiltinAPIGroup reports whether an apiVersion belongs to a group the
// cluster serves itself. It accepts either a bare group or a full apiVersion,
// because a check reads whichever the object carries.
func IsBuiltinAPIGroup(apiVersion string) bool {
	i := strings.IndexByte(apiVersion, '/')
	if i < 0 {
		// No group at all - "v1" - which is the core group, and is as built in
		// as anything gets.
		return true
	}
	return builtinAPIGroups[apiVersion[:i]]
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

// Credential detection.
//
// # Why this is value-driven and not name-driven
//
// It used to ask one question: does the KEY look like it holds a password? On a
// real chart that question produced four findings and every one of them was
// wrong - a retry counter called SECRET_FETCH_RETRYCOUNT, a cache lifetime
// called TOKEN_CACHE_TTL_SECONDS, a minimum length called PASSWORD_MIN_LENGTH,
// a file path called KEYSTORE_PATH. None of them is a credential and all four
// are the ordinary way to name a configuration parameter about credentials.
//
// The same run missed a database password, a signed token, a cloud access key
// and a connection string with the password inline, because none of their keys
// matched the pattern. The check written to find credentials found only things
// that were not credentials and none of the things that were, which is the
// worst outcome available to it.
//
// So the order is inverted. The value is examined first, and a value with a
// recognisable credential SHAPE is a finding whatever it is called. The key
// name is used only as corroboration for the one case where the shape is not
// conclusive: an opaque string in a field named for a secret.
//
// # The false-positive budget, stated
//
// Shape signals (§1) are exact: a PEM private-key header, a JWT, an AWS key id,
// a URI with an inline password, an Authorization header. A false positive
// there means somebody wrote a real credential into a comment.
//
// The corroborated signal (§3) is a heuristic and carries the residual budget.
// It needs BOTH a key named for a credential, after the operational-parameter
// exclusions, AND a value that is not a number, a duration, a path, a hostname,
// a URL, a class name or a boolean. Raw entropy is deliberately not a signal on
// its own: it flags every checksum, UUID and base64-encoded certificate in a
// chart, and a check that is wrong three times in a row is a check nobody reads
// again.

// credentialKeyPattern matches a key whose value is expected to be a secret.
var credentialKeyPattern = regexp.MustCompile(
	`(?i)(^|[._\-])(passwd|password|secret|token|apikey|api[_\-]?key|access[_\-]?key|secret[_\-]?key|private[_\-]?key|credential|auth|bearer|session[_\-]?key)([._\-]|$)`)

// keyIsNotACredential are the near-misses that name a credential rather than
// carrying one.
//
// Each is a real field name from a real chart. Without them the check reports
// every OIDC issuer URL and every secretKeyRef as a leaked credential, and
// after the third of those nobody reads the report - which is the actual
// failure mode a compliance tool has.
var keyIsNotACredential = regexp.MustCompile(
	`(?i)(secretname|secretref|secretkeyref|passwordkey|tokenkey|authurl|authorizationurl|authmode|authtype|authenticationmethod|secretpath|tokenpath|keyalgorithm|keysize|passwordpolicy|tokenexpiry|tokenttl|secretengine|existingsecret|authproxy)`)

// operationalParameterKey matches a key whose NAME ends in a unit, a count, a
// location or a setting - a parameter ABOUT a credential rather than one.
//
// Every entry here is a key that produced a false positive on a real chart, or
// is the same shape as one that did.
var operationalParameterKey = regexp.MustCompile(
	`(?i)(^|[._\-])(count|retries|retrycount|attempts|interval|intervals|sec|secs|second|seconds|ms|millis|milliseconds|minutes|hours|days|timeout|ttl|deadline|enabled|disabled|required|optional|path|paths|file|files|filename|dir|directory|url|uri|endpoint|host|hostname|address|port|class|classname|provider|algorithm|cipher|mode|format|encoding|policy|strategy|type|kind|version|length|minlength|maxlength|size|limit|expiry|expiration|rotation|issuer|audience|realm|scheme|method|source|store|backend|namespace|prefix|suffix|header|field|label|id)$`)

var (
	pemPrivateKey  = regexp.MustCompile(`-----BEGIN (?:[A-Z ]+ )?PRIVATE KEY-----`)
	jsonWebToken   = regexp.MustCompile(`^eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{4,}$`)
	cloudAccessKey = regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA)[0-9A-Z]{16}\b`)
	uriWithSecret  = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.\-]*://[^/\s:@]+:[^/\s:@]+@`)
	authHeader     = regexp.MustCompile(`(?i)^(?:bearer|basic)\s+[A-Za-z0-9+/=_\-.]{16,}$`)
	vendorToken    = regexp.MustCompile(`^(?:gh[pousr]_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{20,}|xox[baprs]-[A-Za-z0-9-]{10,}|sk-[A-Za-z0-9]{20,}|glpat-[A-Za-z0-9_\-]{16,}|AIza[0-9A-Za-z_\-]{30,})$`)

	numericValue  = regexp.MustCompile(`^[-+]?[0-9]+(?:\.[0-9]+)?$`)
	durationValue = regexp.MustCompile(`(?i)^[0-9]+(?:\.[0-9]+)?(?:ns|us|ms|s|m|h|d|Ki|Mi|Gi|Ti|k|M|G|T)?$`)
	pathValue     = regexp.MustCompile(`^(?:/|\./|\.\./|[A-Za-z]:\\)[^\s]*$`)
	hostOrURL     = regexp.MustCompile(`(?i)^(?:[a-z][a-z0-9+.\-]*://)?[a-z0-9]([a-z0-9.\-]*[a-z0-9])?(?::[0-9]{1,5})?(?:/[^\s]*)?$`)
	classNameLike = regexp.MustCompile(`^[a-z][a-z0-9]*(?:\.[A-Za-z][A-Za-z0-9_]*){2,}$`)
	uuidValue     = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	digestValue   = regexp.MustCompile(`(?i)^(?:sha(?:1|256|512)[:=])?[0-9a-f]{32,128}$`)
)

// wordValues are single words that are settings, not secrets. A field called
// `auth` set to `none` is a configuration choice, and reporting it as a leaked
// credential is the kind of finding that gets a whole category switched off.
var wordValues = map[string]bool{
	"true": true, "false": true, "yes": true, "no": true, "on": true, "off": true,
	"none": true, "null": true, "nil": true, "enabled": true, "disabled": true,
	"auto": true, "always": true, "never": true, "required": true, "optional": true,
	"debug": true, "info": true, "warn": true, "warning": true, "error": true,
	"basic": true, "bearer": true, "digest": true, "oauth": true, "oauth2": true,
	"oidc": true, "ldap": true, "saml": true, "jwt": true, "mtls": true, "tls": true,
	"kubernetes": true, "vault": true, "aws": true, "azure": true, "gcp": true,
}

// SecretMaterialClass classifies the contents of one Secret value.
//
// # Why a Secret needs its own rule
//
// The check that reports credentials travelling inside a chart fired on the
// PRESENCE of inline data. On a real release that was 56 findings of which 14
// were genuine: the rest were usernames, hostnames, object names, and - worst -
// configuration templates whose credential fields are deliberately EMPTY,
// waiting to be filled in at install time. Rating those Critical tells a team
// to fix something that is already correct.
//
// So the contents decide, in three tiers:
//
//	private key   - unambiguous, and the one credential that cannot be contained
//	                after exposure
//	credential    - a value that is credential-shaped, or a credential-named
//	                field holding a non-empty literal
//	inline data   - something is shipped in the chart and it is not recognisably
//	                a credential. Worth recording, not worth blocking a release
//
// An empty value returns "" at every tier, including the field-inside-a-file
// case: `password =` with nothing after it is a placeholder, which is the shape
// a well-behaved chart ships.
func SecretMaterialClass(key, value string) string {
	v := strings.TrimSpace(value)
	if v == "" {
		return ""
	}
	if pemPrivateKey.MatchString(v) {
		return "a private key"
	}
	// A properties or YAML fragment shipped as one value. Classify the fields
	// inside it rather than the blob: this is the shape that produced ten of
	// the twelve false positives, because the blob is long and opaque and every
	// credential field in it is empty.
	if inner := embeddedFieldClass(v); inner != "" {
		return inner
	}
	if strings.ContainsAny(v, "\n") {
		// Multi-line and nothing inside it looked like a credential. Reporting
		// the blob itself would be reporting its length.
		return ""
	}
	if c := CredentialClass(key, v); c != "" {
		return c
	}
	return ""
}

// embeddedFieldClass reads `key = value` and `key: value` lines out of a value
// that is really a configuration file, and classifies those.
//
// Returns "" when every credential-named field in it is empty, which is the
// case a check that looks only at the whole blob gets wrong.
func embeddedFieldClass(v string) string {
	if !strings.Contains(v, "\n") && !strings.Contains(v, "=") {
		return ""
	}
	found := false
	for _, line := range strings.Split(v, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		k, val, ok := cutField(line)
		if !ok {
			continue
		}
		found = true
		if c := CredentialClass(k, strings.TrimSpace(val)); c != "" {
			return c
		}
	}
	if found {
		// It parsed as a configuration file and nothing in it is a credential.
		// Saying so is what stops the blob being reported for its size.
		return ""
	}
	return ""
}

// cutField splits one configuration line into its key and value, on either
// separator, taking whichever comes first.
func cutField(line string) (string, string, bool) {
	eq := strings.Index(line, "=")
	co := strings.Index(line, ":")
	switch {
	case eq < 0 && co < 0:
		return "", "", false
	case eq >= 0 && (co < 0 || eq < co):
		k, v, _ := strings.Cut(line, "=")
		return strings.TrimSpace(k), v, true
	default:
		k, v, _ := strings.Cut(line, ":")
		return strings.TrimSpace(k), v, true
	}
}

// CredentialShape is the conclusive half of the detector: what a value IS,
// with no reference to what the field is called.
//
// # Why the two halves are separable
//
// A PEM private key is a private key wherever it appears. An opaque string in a
// field named for a credential is an inference, and a good one - but it is
// still an inference, and on a real release it was wrong: three environment
// variables named APP_*_SECRET held "session-store-v1" and similar, which
// follow the naming convention of the other objects in that release rather than
// the shape of credential material.
//
// Reporting both at one severity, with confidence "confirmed", puts a guess
// behind the tool's strongest claim. Separating them lets the shape-confirmed
// case block a release and the inferred case ask a question.
func CredentialShape(value string) string {
	v := strings.TrimSpace(value)
	if v == "" {
		return ""
	}
	if strings.HasPrefix(v, "$") || strings.Contains(v, "{{") || strings.Contains(v, "$(") {
		return ""
	}
	if strings.Contains(v, "-----BEGIN CERTIFICATE-----") ||
		strings.Contains(v, "-----BEGIN PUBLIC KEY-----") {
		return ""
	}
	switch {
	case pemPrivateKey.MatchString(v):
		return "a private key"
	case jsonWebToken.MatchString(v):
		return "a signed token (JWT)"
	case cloudAccessKey.MatchString(v):
		return "a cloud access key"
	case uriWithSecret.MatchString(v):
		return "a connection string with the password written into it"
	case authHeader.MatchString(v):
		return "a ready-made authorization header"
	case vendorToken.MatchString(v):
		return "an API access token"
	case obviousPlaceholders[strings.ToLower(v)]:
		return "a placeholder credential somebody was meant to replace"
	}
	return ""
}

// CredentialClass names what a value looks like, or returns "" when it does not
// look like a credential at all.
//
// The class is printed in the finding INSTEAD of the value. A compliance report
// is itself a distributable artifact - it goes into a ticket, an email and a
// shared drive - so a report that quotes the password it found has copied the
// exposure rather than described it.
func CredentialClass(key, value string) string {
	v := strings.TrimSpace(value)
	if v == "" {
		return ""
	}
	// A reference to a credential is the correct pattern, not a finding. A
	// template fragment is a reference that has not been rendered yet.
	if strings.HasPrefix(v, "$") || strings.Contains(v, "{{") || strings.Contains(v, "$(") {
		return ""
	}
	// A public certificate is meant to be public. It is long, base64 and
	// alarming-looking, and it is the single most common false positive in any
	// entropy-based detector.
	if strings.Contains(v, "-----BEGIN CERTIFICATE-----") ||
		strings.Contains(v, "-----BEGIN PUBLIC KEY-----") {
		return ""
	}

	// 1. Shape signals. These are conclusive whatever the key is called, and
	//    they are the half the name-driven detector missed entirely.
	switch {
	case pemPrivateKey.MatchString(v):
		return "a private key"
	case jsonWebToken.MatchString(v):
		return "a signed token (JWT)"
	case cloudAccessKey.MatchString(v):
		return "a cloud access key"
	case uriWithSecret.MatchString(v):
		return "a connection string with the password written into it"
	case authHeader.MatchString(v):
		return "a ready-made authorization header"
	case vendorToken.MatchString(v):
		return "an API access token"
	case obviousPlaceholders[strings.ToLower(v)]:
		return "a placeholder credential somebody was meant to replace"
	}

	// 2. Exclusions, applied before the key name is trusted at all.
	if keyIsNotACredential.MatchString(key) || operationalParameterKey.MatchString(key) {
		return ""
	}
	if !credentialKeyPattern.MatchString(key) {
		return ""
	}
	if isNotSecretShaped(v) {
		return ""
	}

	// 3. Corroborated signal: a field named for a credential, holding a literal
	//    that is not a number, a duration, a path, a host, a class or a word.
	if IsPlaceholderCredential(v) {
		return "a well-known default credential"
	}
	if len(v) >= 16 && shannonEntropy(v) >= 3.5 {
		return "an opaque high-entropy value in a field named for a credential"
	}
	return "a literal value in a field named for a credential"
}

// isNotSecretShaped reports values that cannot be the credential their key is
// named for. A password of "8080" is a port; a password of "/etc/keys/db" is
// where the password lives.
func isNotSecretShaped(v string) bool {
	switch {
	case numericValue.MatchString(v), durationValue.MatchString(v):
		return true
	case pathValue.MatchString(v):
		return true
	case classNameLike.MatchString(v):
		return true
	case uuidValue.MatchString(v), digestValue.MatchString(v):
		return true
	case wordValues[strings.ToLower(v)]:
		return true
	case !strings.ContainsAny(v, " \t") && hostOrURL.MatchString(v) && strings.Contains(v, "."):
		// A bare hostname or a URL with no credentials in it. The "." keeps
		// this from swallowing every short opaque string.
		return true
	}
	return false
}

// shannonEntropy is bits per character, used only to sharpen the wording of a
// finding that already matched on its key name. It never promotes a value to a
// finding on its own - see the budget note above.
func shannonEntropy(s string) float64 {
	if s == "" {
		return 0
	}
	var counts [256]float64
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	n := float64(len(s))
	var h float64
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := c / n
		h -= p * math.Log2(p)
	}
	return h
}

// LooksLikeCredential reports whether a key/value pair is a credential written
// where anybody who can read the object can read it.
func LooksLikeCredential(key, value string) bool { return CredentialClass(key, value) != "" }

// DecodeBase64 returns the decoded form of a base64 value, or the value
// unchanged when it is not base64.
//
// # Why every credential check needs it
//
// A Secret stores its values base64-encoded. That is an encoding, not
// protection - `base64 -d` is the whole attack - but a detector reading the
// encoded form sees an opaque blob and finds nothing. In the run that produced
// this rewrite, a private key, a database password and a default administrator
// credential were all plainly visible after a single decode, and all three were
// reported as no finding at all.
func DecodeBase64(v string) string {
	t := strings.TrimSpace(v)
	if len(t) < 4 {
		return v
	}
	dec, err := base64.StdEncoding.DecodeString(t)
	if err != nil {
		if dec, err = base64.RawStdEncoding.DecodeString(t); err != nil {
			return v
		}
	}
	// Binary is not a credential anybody wrote by hand, and printing it into a
	// finding would corrupt the report.
	for _, b := range dec {
		if b < 0x09 || (b > 0x0d && b < 0x20) || b == 0x7f {
			return v
		}
	}
	return string(dec)
}

// obviousPlaceholders are values that cannot be anything but a credential
// somebody meant to replace. They are a SHAPE signal - reported wherever they
// appear, whatever the field is called - because "changeme" in a field named
// `initialLogin` is exactly as dangerous as "changeme" in a field named
// `password`, and only one of those two spellings would ever be caught by a
// check that trusts the field name.
//
// Kept deliberately narrow. The ambiguous defaults below - admin, secret, root,
// default, test - are ordinary values for a role, a tier or an environment, so
// they stay gated behind a field named for a credential.
var obviousPlaceholders = map[string]bool{
	"changeme": true, "change-me": true, "change_me": true, "changeit": true,
	"passw0rd": true, "p@ssw0rd": true, "password123": true, "letmein": true,
	"test123": true, "qwerty": true, "123456": true, "12345678": true,
	"notsecure": true, "insecure": true, "hunter2": true,
}

// placeholderValues are the defaults a vendor ships meaning "change this", and
// which reach production unchanged often enough that the source catalogue names
// them specifically (SUP-10). These are only a finding in a field that is
// already named for a credential.
var placeholderValues = map[string]bool{
	"changeme": true, "change-me": true, "change_me": true, "changeit": true,
	"admin": true, "administrator": true, "password": true, "passw0rd": true,
	"p@ssw0rd": true, "password123": true, "secret": true, "letmein": true,
	"test": true, "test123": true, "default": true, "example": true,
	"root": true, "toor": true, "guest": true, "welcome": true, "insecure": true,
	"123456": true, "12345678": true, "qwerty": true, "notsecure": true,
}

// IsPlaceholderCredential reports a value that is a known default.
//
// Separate from LooksLikeCredential because it is a different finding with a
// different fix: one says "you shipped a secret", the other says "you shipped
// the word 'changeme' and somebody will not".
func IsPlaceholderCredential(value string) bool {
	v := strings.ToLower(strings.TrimSpace(value))
	return placeholderValues[v] || obviousPlaceholders[v]
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
