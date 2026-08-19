package product

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

// loadOne loads a single document through the real loader, so these tests
// exercise the same path the Coordinator does - parsing, defaulting and
// validation - rather than a hand-built struct that skips half of it.
func loadOne(t *testing.T, doc string) (*Product, error) {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.yaml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := NewLoader(dir, NewSecretResolver(t.TempDir())).Load()
	if err != nil {
		t.Fatalf("load directory: %v", err)
	}
	if len(res.Invalid) > 0 {
		return nil, res.Invalid[0].Err
	}
	if len(res.Valid) != 1 {
		t.Fatalf("expected 1 product, got %d", len(res.Valid))
	}
	return res.Valid[0], nil
}

// Absent means enabled. Every configuration written before this field existed
// means "on", and a new one will expect the same.
func TestEnabledDefaultsToTrue(t *testing.T) {
	if !(Product{}).IsEnabled() {
		t.Error("a product with no enabled field must be enabled")
	}
	if !(Source{}).IsEnabled() {
		t.Error("a source with no enabled field must be enabled")
	}
	if !(Target{}).IsEnabled() {
		t.Error("a target with no enabled field must be enabled")
	}
}

func TestExplicitlyDisabled(t *testing.T) {
	p := Product{Metadata: Metadata{Enabled: boolPtr(false)}}
	if p.IsEnabled() {
		t.Error("metadata.enabled: false must disable the product")
	}
	if (Source{Enabled: boolPtr(false)}).IsEnabled() {
		t.Error("enabled: false must disable a source")
	}
	if (Target{Enabled: boolPtr(false)}).IsEnabled() {
		t.Error("enabled: false must disable a target")
	}
}

// A disabled target must not be handed out as the default. Silently
// replicating into a destination somebody switched off would defeat the point
// of the switch.
func TestDisabledTargetIsNotTheDefault(t *testing.T) {
	p := Product{Spec: Spec{Targets: []Target{
		{Name: "lab", Default: true, Enabled: boolPtr(false)},
		{Name: "backup"},
	}}}

	got, ok := p.DefaultTarget()
	if !ok {
		t.Fatal("expected the remaining enabled target to be chosen")
	}
	if got.Name != "backup" {
		t.Errorf("expected the enabled target, got %q", got.Name)
	}
}

func TestNoEnabledTargetMeansNoDefault(t *testing.T) {
	p := Product{Spec: Spec{Targets: []Target{
		{Name: "lab", Default: true, Enabled: boolPtr(false)},
	}}}
	if _, ok := p.DefaultTarget(); ok {
		t.Error("a product whose only target is disabled has no default")
	}
}

// A disabled source or target keeps its RepositoryRef so reconciliation can
// deactivate the row rather than delete it - the packages and transfer history
// that reference it must survive.
func TestDisabledRepositoriesAreStillDeclaredButNotEnabled(t *testing.T) {
	p := Product{
		Metadata: Metadata{Name: "p"},
		Spec: Spec{
			Sources: []Source{
				{Name: "live", Registry: "r", Repository: "a/one"},
				{Name: "paused", Registry: "r", Repository: "a/two", Enabled: boolPtr(false)},
			},
			Targets: []Target{
				{Name: "off", Registry: "r", Repository: "b/one", Enabled: boolPtr(false)},
			},
		},
	}

	refs := p.Repositories()
	if len(refs) != 3 {
		t.Fatalf("every declared repository must still be returned, got %d", len(refs))
	}

	state := map[string]bool{}
	for _, r := range refs {
		state[r.Name] = r.Enabled
	}
	if !state["live"] {
		t.Error("the live source should be enabled")
	}
	if state["paused"] {
		t.Error("the paused source should not be enabled")
	}
	if state["off"] {
		t.Error("the disabled target should not be enabled")
	}
}

// Disabling the PRODUCT disables everything under it, without each repository
// having to say so.
func TestDisabledProductDisablesItsRepositories(t *testing.T) {
	p := Product{
		Metadata: Metadata{Name: "p", Enabled: boolPtr(false)},
		Spec: Spec{
			Sources: []Source{{Name: "s", Registry: "r", Repository: "a/one"}},
			Targets: []Target{{Name: "t", Registry: "r", Repository: "b/one"}},
		},
	}

	for _, r := range p.Repositories() {
		if r.Enabled {
			t.Errorf("%q should be disabled because its product is", r.Name)
		}
	}
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

// A disabled product is validated exactly like an enabled one. The whole point
// of disabling rather than deleting is that re-enabling is safe, which it is
// not if errors went unreported while it was off.
func TestDisabledProductIsStillValidated(t *testing.T) {
	doc := `
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata:
  name: paused
  enabled: false
spec:
  sources:
    - name: vendor
      registry: registry.example.com
      repository: a/b
      anonymous: true
      discovery:
        tagFilters:
          include: ["^v("]
  targets:
    - name: internal
      registry: internal.example.com
      repository: mirror/x
      anonymous: true
      default: true
`
	_, errs := loadOne(t, doc)
	if errs == nil {
		t.Fatal("a broken pattern must be reported even in a disabled product")
	}
	if !strings.Contains(errs.Error(), "invalid regexp") {
		t.Errorf("expected the pattern error, got: %v", errs)
	}
}

// Every source disabled while the product is on discovers nothing while
// looking active - the exact silent failure disabling is meant to make visible.
func TestEverySourceDisabledIsRejected(t *testing.T) {
	doc := `
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata: {name: p}
spec:
  sources:
    - name: vendor
      registry: registry.example.com
      repository: a/b
      anonymous: true
      enabled: false
  targets:
    - name: internal
      registry: internal.example.com
      repository: mirror/x
      anonymous: true
      default: true
`
	_, errs := loadOne(t, doc)
	if errs == nil {
		t.Fatal("expected every-source-disabled to be rejected")
	}
	if !strings.Contains(errs.Error(), "disable the product itself") {
		t.Errorf("the error should name the right fix, got: %v", errs)
	}
}

// But disabling the product AND its sources is coherent, so it must pass.
func TestDisabledProductWithDisabledSourcesIsFine(t *testing.T) {
	doc := `
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata:
  name: p
  enabled: false
spec:
  sources:
    - name: vendor
      registry: registry.example.com
      repository: a/b
      anonymous: true
      enabled: false
  targets:
    - name: internal
      registry: internal.example.com
      repository: mirror/x
      anonymous: true
      default: true
`
	if _, errs := loadOne(t, doc); errs != nil {
		t.Errorf("a fully disabled product is coherent, got: %v", errs)
	}
}

// A rule pointing at a disabled target fails the first time a package matches
// it - potentially weeks later. Caught at load instead.
func TestRuleTargetingADisabledTargetIsRejected(t *testing.T) {
	doc := `
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata: {name: p}
spec:
  sources:
    - name: vendor
      registry: registry.example.com
      repository: a/b
      anonymous: true
  targets:
    - name: lab
      registry: internal.example.com
      repository: mirror/lab
      anonymous: true
      default: true
    - name: decommissioned
      registry: internal.example.com
      repository: mirror/old
      anonymous: true
      enabled: false
  autoDownload:
    enabled: true
    rules:
      - name: releases
        tagPattern: '^v'
        targets: [decommissioned]
`
	_, errs := loadOne(t, doc)
	if errs == nil {
		t.Fatal("a rule pointing at a disabled target must be rejected")
	}
	if !strings.Contains(errs.Error(), "is disabled") {
		t.Errorf("expected the error to say so, got: %v", errs)
	}
}

func TestAutoDownloadWithEveryTargetDisabledIsRejected(t *testing.T) {
	doc := `
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata: {name: p}
spec:
  sources:
    - name: vendor
      registry: registry.example.com
      repository: a/b
      anonymous: true
  targets:
    - name: lab
      registry: internal.example.com
      repository: mirror/lab
      anonymous: true
      default: true
      enabled: false
  autoDownload:
    enabled: true
    rules:
      - name: releases
        tagPattern: '^v'
`
	_, errs := loadOne(t, doc)
	if errs == nil {
		t.Fatal("expected autoDownload with no enabled target to be rejected")
	}
	if !strings.Contains(errs.Error(), "nowhere to send") {
		t.Errorf("the error should explain the consequence, got: %v", errs)
	}
}

// ---------------------------------------------------------------------------
// Per-repository verification
// ---------------------------------------------------------------------------

// Two vendors do not share a signing identity, so `cosign` replaces wholesale
// rather than merging - a trust configuration assembled from two documents is
// one nobody can audit.
func TestVerificationCosignReplacesWholesale(t *testing.T) {
	p := Product{Spec: Spec{Verification: Verification{
		Enabled: true, Policy: PolicyEnforce, AtSource: true,
		Cosign: Cosign{
			Mode: CosignKeylessMode,
			Keyless: &CosignKeyless{
				CertificateIdentity:   "product-identity",
				CertificateOidcIssuer: "https://product-issuer",
			},
		},
	}}}

	override := &Verification{
		Enabled: true, AtSource: true,
		Cosign: Cosign{
			Mode: CosignKeyMode,
			Key:  &CosignKey{PublicKeyRef: &SecretRef{SecretName: "vendor-b-key"}},
		},
	}

	got := p.VerificationFor(override)

	if got.Cosign.Mode != CosignKeyMode {
		t.Fatalf("expected the override's mode, got %q", got.Cosign.Mode)
	}
	// The critical assertion: nothing from the product's keyless block leaks
	// into a key-mode configuration.
	if got.Cosign.Keyless != nil {
		t.Error("the product's keyless identity must not survive a key-mode override")
	}
}

// Scalars inherit, so a product states its policy once and a repository
// overrides only what differs.
func TestVerificationScalarsInherit(t *testing.T) {
	p := Product{Spec: Spec{Verification: Verification{
		Enabled: true, Policy: PolicyEnforce, AtSource: true,
		TransferSignatures: boolPtr(true),
	}}}

	got := p.VerificationFor(&Verification{Enabled: true, AtSource: true, Policy: PolicyWarn})

	if got.Policy != PolicyWarn {
		t.Errorf("the override's policy should win, got %q", got.Policy)
	}
	if got.TransferSignatures == nil || !*got.TransferSignatures {
		t.Error("an unset field should inherit from the product")
	}
}

func TestNoOverrideReturnsProductVerification(t *testing.T) {
	p := Product{Spec: Spec{Verification: Verification{Enabled: true, Policy: PolicyEnforce}}}
	if got := p.VerificationFor(nil); got.Policy != PolicyEnforce {
		t.Errorf("expected the product's settings unchanged, got %q", got.Policy)
	}
}

// The keyless-identity rule is the one that stops a trust configuration that
// looks secure and is not. It must apply to per-repository blocks too.
func TestPerSourceKeylessWithoutIdentityIsRejected(t *testing.T) {
	doc := `
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata: {name: p}
spec:
  sources:
    - name: vendor
      registry: registry.example.com
      repository: a/b
      anonymous: true
      verification:
        enabled: true
        policy: enforce
        atSource: true
        cosign:
          mode: keyless
          keyless:
            # An issuer but no identity - the configuration that looks secure
            # and accepts a signature from anyone that issuer has ever vouched
            # for.
            certificateOidcIssuer: https://token.actions.githubusercontent.com
  targets:
    - name: internal
      registry: internal.example.com
      repository: mirror/x
      anonymous: true
      default: true
`
	_, errs := loadOne(t, doc)
	if errs == nil {
		t.Fatal("keyless without certificateIdentity must be rejected at source level too")
	}
	if !strings.Contains(errs.Error(), "certificateIdentity") {
		t.Errorf("expected the identity error, got: %v", errs)
	}
	if !strings.Contains(errs.Error(), "sources[0].verification") {
		t.Errorf("the error must be located at the source, got: %v", errs)
	}
}

// ---------------------------------------------------------------------------
// Proxy
// ---------------------------------------------------------------------------

func TestProxyDirectIsAccepted(t *testing.T) {
	doc := `
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata: {name: p}
spec:
  network:
    proxy:
      httpsProxy: http://proxy.example.com:3128
  sources:
    - name: vendor
      registry: registry.example.com
      repository: a/b
      anonymous: true
      network:
        proxy:
          direct: true
  targets:
    - name: internal
      registry: internal.example.com
      repository: mirror/x
      anonymous: true
      default: true
`
	p, errs := loadOne(t, doc)
	if errs != nil {
		t.Fatalf("proxy.direct must be accepted: %v", errs)
	}
	src, _ := p.Source("vendor")
	if src.Network == nil || src.Network.Proxy == nil || !src.Network.Proxy.Direct {
		t.Error("expected the source to carry proxy.direct")
	}
}
