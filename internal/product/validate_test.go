package product

import (
	"fmt"
	"strings"
	"testing"
)

// valid returns a minimal product that passes validation, so each test can
// break exactly one thing.
func valid() *Product {
	prio := 100
	return &Product{
		APIVersion: APIVersion,
		Kind:       Kind,
		Metadata:   Metadata{Name: "vendor-a-platform"},
		Spec: Spec{
			Sources: []Source{{
				Name: "primary", Registry: "registry.vendor-a.example.com",
				Repository: "platform/suite", Anonymous: true,
			}},
			Targets: []Target{{
				Name: "lab", Registry: "internal.azurecr.io",
				Repository: "vendor-a/platform", Anonymous: true, Default: true,
			}},
			AutoDownload: AutoDownload{
				Enabled: true,
				Rules: []Rule{{
					Name: "ga", TagPattern: `^v\d+\.\d+\.\d+$`,
					Targets: []string{"lab"}, Priority: &prio,
				}},
			},
		},
	}
}

func fieldsOf(t *testing.T, err error) []string {
	t.Helper()
	if err == nil {
		return nil
	}
	errs, ok := AsErrors(err)
	if !ok {
		t.Fatalf("expected product.Errors, got %T: %v", err, err)
	}
	out := make([]string, len(errs))
	for i, e := range errs {
		out[i] = e.Field
	}
	return out
}

func requireField(t *testing.T, err error, field string) Error {
	t.Helper()
	errs, ok := AsErrors(err)
	if !ok {
		t.Fatalf("expected product.Errors, got %T: %v", err, err)
	}
	for _, e := range errs {
		if e.Field == field {
			return e
		}
	}
	t.Fatalf("expected an error on %q, got %v", field, fieldsOf(t, err))
	return Error{}
}

func TestValidProductPasses(t *testing.T) {
	if err := valid().Validate(nil); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The three headline error classes from docs/design/13 section 9.
// These are the reason `transferctl config validate` exists.
// ---------------------------------------------------------------------------

func TestCatchesNonCompilingTagPattern(t *testing.T) {
	p := valid()
	p.Spec.AutoDownload.Rules[0].TagPattern = `^v(\d+\.\d+`

	e := requireField(t, p.Validate(nil), "spec.autoDownload.rules[0].tagPattern")
	if !strings.Contains(e.Message, "missing closing )") {
		t.Fatalf("message should name the actual regexp fault, got %q", e.Message)
	}
	// The message renders the pattern with %q, so backslashes appear escaped.
	if !strings.Contains(e.Message, fmt.Sprintf("%q", `^v(\d+\.\d+`)) {
		t.Fatalf("message should quote the offending pattern, got %q", e.Message)
	}
}

func TestCatchesRuleNamingUndeclaredTarget(t *testing.T) {
	p := valid()
	p.Spec.AutoDownload.Rules[0].Targets = []string{"prod"}

	e := requireField(t, p.Validate(nil), "spec.autoDownload.rules[0].targets[0]")
	if !strings.Contains(e.Message, `"prod" is not a declared target`) {
		t.Fatalf("got %q", e.Message)
	}
	if e.Hint == "" {
		t.Fatal("this error should tell the reader how to fix it")
	}
}

func TestCatchesKeylessWithoutCertificateIdentity(t *testing.T) {
	// The most important validation in the system: a keyless policy with no
	// identity constraint is syntactically fine and semantically useless.
	p := valid()
	p.Spec.Verification = Verification{
		Enabled: true, Policy: PolicyEnforce, AtDestination: true,
		Cosign: Cosign{
			Mode:    CosignKeylessMode,
			Keyless: &CosignKeyless{CertificateOidcIssuer: "https://token.actions.githubusercontent.com"},
		},
	}

	e := requireField(t, p.Validate(nil), "spec.verification.cosign.keyless.certificateIdentity")
	if !strings.Contains(e.Hint, "any valid Sigstore signature would be accepted") {
		t.Fatalf("the hint must explain WHY this is dangerous, got %q", e.Hint)
	}
}

func TestKeylessWithBothConstraintsPasses(t *testing.T) {
	p := valid()
	p.Spec.Verification = Verification{
		Enabled: true, Policy: PolicyEnforce, AtDestination: true,
		Cosign: Cosign{Mode: CosignKeylessMode, Keyless: &CosignKeyless{
			CertificateIdentity:   "https://github.com/vendor-a/platform/.github/workflows/release.yaml@refs/heads/main",
			CertificateOidcIssuer: "https://token.actions.githubusercontent.com",
		}},
	}
	if err := p.Validate(nil); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
}

func TestCatchesMissingOidcIssuer(t *testing.T) {
	p := valid()
	p.Spec.Verification = Verification{
		Enabled: true, Policy: PolicyEnforce, AtSource: true,
		Cosign: Cosign{Mode: CosignKeylessMode, Keyless: &CosignKeyless{
			CertificateIdentity: "https://github.com/vendor-a/platform/.github/workflows/release.yaml@refs/heads/main",
		}},
	}
	requireField(t, p.Validate(nil), "spec.verification.cosign.keyless.certificateOidcIssuer")
}

// ---------------------------------------------------------------------------
// Structural and cross-field rules.
// ---------------------------------------------------------------------------

func TestReportsAllErrorsAtOnce(t *testing.T) {
	// Fixing errors one CI round-trip at a time is a poor experience.
	p := valid()
	p.Metadata.Name = "Not Valid"
	p.Spec.AutoDownload.Rules[0].TagPattern = `^v(`
	p.Spec.AutoDownload.Rules[0].Targets = []string{"nope"}

	got := fieldsOf(t, p.Validate(nil))
	if len(got) < 3 {
		t.Fatalf("expected at least 3 errors, got %d: %v", len(got), got)
	}
}

func TestRejectsPromotionOnlyAsAutoDownloadTarget(t *testing.T) {
	p := valid()
	p.Spec.Targets = append(p.Spec.Targets, Target{
		Name: "production", Registry: "internal.azurecr.io",
		Repository: "vendor-a/platform-prod", Anonymous: true, PromotionOnly: true,
	})
	p.Spec.AutoDownload.Rules[0].Targets = []string{"production"}

	e := requireField(t, p.Validate(nil), "spec.autoDownload.rules[0].targets[0]")
	if !strings.Contains(e.Message, "promotionOnly") {
		t.Fatalf("got %q", e.Message)
	}
}

func TestRejectsDefaultAndPromotionOnly(t *testing.T) {
	p := valid()
	p.Spec.Targets[0].PromotionOnly = true
	requireField(t, p.Validate(nil), "spec.targets[0]")
}

func TestRejectsTwoDefaultTargets(t *testing.T) {
	p := valid()
	p.Spec.Targets = append(p.Spec.Targets, Target{
		Name: "staging", Registry: "internal.azurecr.io",
		Repository: "vendor-a/platform-stg", Anonymous: true, Default: true,
	})
	requireField(t, p.Validate(nil), "spec.targets")
}

func TestRejectsDuplicateRepositoryNames(t *testing.T) {
	p := valid()
	p.Spec.Targets = append(p.Spec.Targets, Target{
		Name: "lab", Registry: "other.example.com",
		Repository: "x/y", Anonymous: true,
	})
	requireField(t, p.Validate(nil), "spec.targets[1].name")
}

func TestRejectsRegistryWithScheme(t *testing.T) {
	p := valid()
	p.Spec.Sources[0].Registry = "https://registry.vendor-a.example.com"
	e := requireField(t, p.Validate(nil), "spec.sources[0].registry")
	if !strings.Contains(e.Message, "scheme") {
		t.Fatalf("got %q", e.Message)
	}
}

func TestRejectsRepositoryWithTag(t *testing.T) {
	p := valid()
	p.Spec.Sources[0].Repository = "platform/suite:v1"
	requireField(t, p.Validate(nil), "spec.sources[0].repository")
}

func TestRejectsAnonymousWithCredentials(t *testing.T) {
	p := valid()
	p.Spec.Sources[0].CredentialsRef = &CredentialsRef{SecretName: "creds"}
	requireField(t, p.Validate(nil), "spec.sources[0]")
}

func TestRequiresCredentialsWhenNotAnonymous(t *testing.T) {
	p := valid()
	p.Spec.Sources[0].Anonymous = false
	requireField(t, p.Validate(nil), "spec.sources[0].credentialsRef")
}

func TestRejectsUnknownRegistryType(t *testing.T) {
	p := valid()
	p.Spec.Targets[0].Type = "harbor"
	e := requireField(t, p.Validate(nil), "spec.targets[0].type")
	if !strings.Contains(e.Message, "generic") {
		t.Fatalf("message should list the valid types, got %q", e.Message)
	}
}

func TestRejectsBurstBelowRate(t *testing.T) {
	p := valid()
	p.Spec.Targets[0].RateLimits = RateLimits{RequestsPerSecond: 100, Burst: 10}
	e := requireField(t, p.Validate(nil), "spec.targets[0].rateLimits.burst")
	if e.Hint == "" {
		t.Fatal("should explain that this throttles below the configured rate")
	}
}

func TestRejectsVerificationThatWouldNeverRun(t *testing.T) {
	p := valid()
	p.Spec.Verification = Verification{
		Enabled: true, Policy: PolicyWarn,
		Cosign: Cosign{Mode: CosignKeyMode, Key: &CosignKey{PublicKeyRef: &SecretRef{SecretName: "k", Key: "cosign.pub"}}},
	}
	e := requireField(t, p.Validate(nil), "spec.verification")
	if !strings.Contains(e.Hint, "would never run") {
		t.Fatalf("got hint %q", e.Hint)
	}
}

func TestRejectsUnknownNotificationEvent(t *testing.T) {
	p := valid()
	p.Spec.Notifications = Notifications{
		Enabled: true,
		Channels: []Channel{{
			Name: "team", Type: ChannelEmail,
			Email: &EmailChannel{Recipients: []string{"a@example.com"}},
		}},
		Subscriptions: []Subscription{{Events: []string{"PackageExploded"}, Channels: []string{"team"}}},
	}
	e := requireField(t, p.Validate(nil), "spec.notifications.subscriptions[0].events[0]")
	if !strings.Contains(e.Hint, "PackageDiscovered") {
		t.Fatalf("hint should list the known events, got %q", e.Hint)
	}
}

func TestRejectsSubscriptionToUndeclaredChannel(t *testing.T) {
	p := valid()
	p.Spec.Notifications = Notifications{
		Enabled:       true,
		Channels:      []Channel{{Name: "team", Type: ChannelEmail, Email: &EmailChannel{Recipients: []string{"a@example.com"}}}},
		Subscriptions: []Subscription{{Events: []string{"TransferFailed"}, Channels: []string{"pager"}}},
	}
	requireField(t, p.Validate(nil), "spec.notifications.subscriptions[0].channels[0]")
}

func TestRequiresTeamsWebhookRef(t *testing.T) {
	p := valid()
	p.Spec.Notifications = Notifications{
		Enabled:  true,
		Channels: []Channel{{Name: "team", Type: ChannelTeams}},
	}
	e := requireField(t, p.Validate(nil), "spec.notifications.channels[0].teams.webhookUrlRef")
	if !strings.Contains(e.Hint, "Power Automate") {
		t.Fatalf("hint should name the supported mechanism, got %q", e.Hint)
	}
}

func TestRejectsWrongAPIVersionAndKind(t *testing.T) {
	p := valid()
	p.APIVersion = "v1"
	p.Kind = "ConfigMap"
	got := fieldsOf(t, p.Validate(nil))
	want := map[string]bool{"apiVersion": true, "kind": true}
	for _, f := range got {
		delete(want, f)
	}
	if len(want) != 0 {
		t.Fatalf("missing errors for %v; got %v", want, got)
	}
}

func TestErrorMessageIncludesHint(t *testing.T) {
	e := Error{Field: "spec.x", Message: "bad", Hint: "because reasons"}
	if got := e.Error(); got != "spec.x: bad — because reasons" {
		t.Fatalf("got %q", got)
	}
	e.Hint = ""
	if got := e.Error(); got != "spec.x: bad" {
		t.Fatalf("got %q", got)
	}
}

func TestRuleWithoutTargetsNeedsDefault(t *testing.T) {
	p := valid()
	p.Spec.Targets[0].Default = false
	p.Spec.Targets = append(p.Spec.Targets, Target{
		Name: "staging", Registry: "internal.azurecr.io",
		Repository: "vendor-a/platform-stg", Anonymous: true,
	})
	p.Spec.AutoDownload.Rules[0].Targets = nil

	e := requireField(t, p.Validate(nil), "spec.autoDownload.rules[0].targets")
	if !strings.Contains(e.Hint, "no default target") {
		t.Fatalf("got hint %q", e.Hint)
	}
}

func TestPriorityRange(t *testing.T) {
	for _, bad := range []int{-1, 1001} {
		p := valid()
		p.Spec.AutoDownload.Rules[0].Priority = &bad
		t.Run(fmt.Sprint(bad), func(t *testing.T) {
			requireField(t, p.Validate(nil), "spec.autoDownload.rules[0].priority")
		})
	}
}
