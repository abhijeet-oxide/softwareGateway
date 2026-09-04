package cel

import "testing"

// The credential detector's false-positive budget, as a test.
//
// The table is the specification: every "no" here is a value that was reported
// as a leaked credential by the name-driven detector this replaced, and every
// "yes" is a real credential it did not report at all. A change that flips any
// row is a change to what this tool tells a vendor about their chart, and it
// should have to be made deliberately.
func TestCredentialClass(t *testing.T) {
	cases := []struct {
		key, value string
		want       bool
		why        string
	}{
		// Parameters ABOUT credentials. Every one of these is a real field name
		// from a real chart, and every one was a false positive.
		{"SECRET_FETCH_RETRYCOUNT", "5", false, "a retry counter"},
		{"SECRET_REFRESH_INTERVAL_SEC", "300", false, "a polling interval"},
		{"TOKEN_CACHE_TTL_SECONDS", "900", false, "a cache lifetime"},
		{"PASSWORD_MIN_LENGTH", "12", false, "a policy parameter"},
		{"KEYSTORE_PATH", "/etc/tls/keystore.jks", false, "a file path"},
		{"CREDENTIAL_PROVIDER_CLASS", "com.example.vault.Provider", false, "a class name"},
		{"auth.mode", "oidc", false, "a setting, not a secret"},
		{"database.passwordSecretRef", "app-db", false, "a reference to a credential"},
		{"oidc.authorizationUrl", "https://sso.example.com/authorize", false, "a URL"},
		{"tokenExpiry", "3600", false, "a duration"},
		{"apiKeyEnabled", "true", false, "a boolean"},
		{"password", "", false, "an empty placeholder is what a good chart ships"},
		{"password", "{{ .Values.dbPassword }}", false, "an unrendered template is not a value"},
		{"password", "$(DB_PASSWORD)", false, "a substitution is a reference"},
		{"tls.crt", "-----BEGIN CERTIFICATE-----\nMIIB...", false, "a public certificate is public"},

		// Real credentials. None of these has a key the name-driven detector
		// matched, and all four are unmistakable from the value alone.
		{"CONNECTION_STRING", "postgresql://svcuser:hunter2@db.example.com:5432/appdb", true, "a password inside a URI"},
		{"CLOUD_ACCESS_KEY", "AKIAIOSFODNN7EXAMPLE", true, "a cloud access key"},
		{"API_TOKEN", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk", true, "a signed token"},
		{"tls.key", "-----BEGIN PRIVATE KEY-----\nMIIEvQIBADAN", true, "a private key"},
		{"INITIAL_LOGIN", "changeme", true, "a placeholder somebody was meant to replace"},
		{"AUTHORIZATION", "Bearer aGVsbG8td29ybGQtdG9rZW4tdmFsdWU=", true, "a ready-made header"},
		{"github", "ghp_16C7e42F292c6912E7710c838347Ae178B4a", true, "a vendor access token"},

		// Corroborated: a field named for a credential holding a literal.
		{"database.password", "hunter2", true, "a password in a field named for one"},
		{"api_key", "sk-live-8f3a9c2e1b4d", true, "an api key in a field named for one"},
	}

	for _, c := range cases {
		got := LooksLikeCredential(c.key, c.value)
		if got != c.want {
			t.Errorf("LooksLikeCredential(%q, %q) = %v, want %v - %s (class %q)",
				c.key, truncate(c.value), got, c.want, c.why, CredentialClass(c.key, c.value))
		}
	}
}

func truncate(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

func TestDecodeBase64(t *testing.T) {
	// A Secret's values are base64. A detector reading the encoded form finds
	// nothing at all, which is how a private key and a default administrator
	// credential passed through a whole run with no finding.
	if got := DecodeBase64("UzNjcjN0LVBAc3N3MHJkLVZhbHVl"); got != "S3cr3t-P@ssw0rd-Value" {
		t.Errorf("DecodeBase64 = %q", got)
	}
	// Not base64: returned unchanged rather than mangled.
	if got := DecodeBase64("plain text value"); got != "plain text value" {
		t.Errorf("DecodeBase64 of plain text = %q", got)
	}
	// Base64 that decodes to binary is not something anybody wrote by hand, and
	// printing it into a report would corrupt the report.
	if got := DecodeBase64("AAECAwQ="); got != "AAECAwQ=" {
		t.Errorf("DecodeBase64 of binary = %q, want it left alone", got)
	}
}

func TestLabelKeyClassification(t *testing.T) {
	// A pod's ordinal is the documented way to address one member of a
	// clustered service, and it is stable for the life of that member. Treating
	// it as release-varying reported the standard pattern as an
	// upgrade-blocking defect, twice, from two different checks.
	stable := []string{
		"app.kubernetes.io/name", "app.kubernetes.io/instance",
		"statefulset.kubernetes.io/pod-name", "apps.kubernetes.io/pod-index",
		"build.acme.example/owner", // a domain that contains "build" is not a build id
	}
	for _, k := range stable {
		if IsUnstableLabelKey(k) {
			t.Errorf("%s is reported as changing between releases, and it does not", k)
		}
	}
	unstable := []string{
		"app.kubernetes.io/version", "helm.sh/chart", "pod-template-hash",
		"controller-revision-hash", "example.com/git-sha", "example.com/build-id",
	}
	for _, k := range unstable {
		if !IsUnstableLabelKey(k) {
			t.Errorf("%s changes between releases and is not reported", k)
		}
	}
	// The labels a chart can never contain, because Kubernetes adds them when
	// it creates the pod. A selector on one of these cannot be matched against
	// a rendered chart, and treating that as "matches nothing" is a false
	// finding on every per-replica Service in a clustered database.
	runtime := []string{
		"statefulset.kubernetes.io/pod-name", "pod-template-hash",
		"controller-revision-hash", "batch.kubernetes.io/job-name",
	}
	for _, k := range runtime {
		if !IsRuntimeSuppliedLabelKey(k) {
			t.Errorf("%s is supplied by the platform and is not recognised as such", k)
		}
	}
	if IsRuntimeSuppliedLabelKey("app.kubernetes.io/name") {
		t.Error("a chart's own label is treated as platform-supplied")
	}
}

func TestBuiltinAPIGroup(t *testing.T) {
	// The check that asks whether a release ships the definitions for the
	// types it uses has to know what a custom type IS. Without this it asked
	// for a definition of PodDisruptionBudget - a finding with no possible fix.
	builtin := []string{"apps/v1", "policy/v1", "networking.k8s.io/v1", "v1",
		"route.openshift.io/v1", "monitoring.coreos.com/v1"}
	for _, g := range builtin {
		if !IsBuiltinAPIGroup(g) {
			t.Errorf("%s is served by the cluster itself and is treated as a custom type", g)
		}
	}
	if IsBuiltinAPIGroup("acme.example/v1") {
		t.Error("a vendor's own type is treated as built in")
	}
}

// The disruption arithmetic, which is what separates the configuration this
// organization's standard RECOMMENDS from the one that deadlocks maintenance.
//
// A check that pattern-matches on the literal `minAvailable: 1` reports the
// standard's own baseline for a two-copy service as a blocking defect - "for
// replicas=2, a common safe setting is maxUnavailable: 1 (or minAvailable: 1)" -
// and a chart author who acts on that finding makes their release worse. The
// same check, matching literals, misses `maxUnavailable: 10%` over a single
// copy, which is a genuine deadlock that reads as permissive.
func TestDisruptionsAllowed(t *testing.T) {
	cases := []struct {
		spec     map[string]any
		replicas int
		want     int
		why      string
	}{
		// The standard's own recommendation for two copies. Must be allowed.
		{map[string]any{"minAvailable": 1}, 2, 1, "minAvailable 1 of 2 spares one"},
		{map[string]any{"maxUnavailable": 1}, 2, 1, "maxUnavailable 1 spares one"},
		// The four spellings of a deadlock.
		{map[string]any{"minAvailable": 2}, 2, 0, "minAvailable equal to the copy count"},
		{map[string]any{"maxUnavailable": 0}, 3, 0, "no copy may be unavailable"},
		{map[string]any{"maxUnavailable": "0%"}, 3, 0, "the string form of the same thing"},
		{map[string]any{"minAvailable": "100%"}, 3, 0, "every copy must stay"},
		// The one the literal comparison cannot see: 10% of one copy rounds
		// DOWN to zero, so it reads as permissive and is absolute.
		{map[string]any{"maxUnavailable": "10%"}, 1, 0, "floor(1 x 10%) is zero"},
		{map[string]any{"maxUnavailable": "10%"}, 30, 3, "and is fine at thirty copies"},
		// minAvailable percentages round UP, which is where quorum-sized
		// workloads deadlock.
		{map[string]any{"minAvailable": "51%"}, 2, 0, "ceil(2 x 51%) is 2 of 2"},
		{map[string]any{"minAvailable": "50%"}, 4, 2, "ceil(4 x 50%) leaves two"},
		// A policy that protects nothing at all.
		{map[string]any{"minAvailable": 0}, 3, 3, "every copy may go at once"},
		// Neither bound stated.
		{map[string]any{}, 3, -1, "nothing to compute"},
	}
	for _, c := range cases {
		if got := disruptionsAllowed(c.spec, c.replicas); got != c.want {
			t.Errorf("disruptionsAllowed(%v, %d) = %d, want %d - %s",
				c.spec, c.replicas, got, c.want, c.why)
		}
	}
}
