package product

import (
	"strings"
	"testing"
)

// verifying returns a COMPLETE destination-verification block. Complete
// matters: validation rejects `enabled: true` without a policy and a cosign
// mode, and these tests are about documents that are valid.
func verifying(atDestination bool) Verification {
	v := Verification{
		Enabled: true, Policy: PolicyEnforce,
		Cosign: Cosign{Mode: CosignKeylessMode, Keyless: &CosignKeyless{
			CertificateIdentity:   "https://github.com/vendor/platform/.github/workflows/release.yaml@refs/heads/main",
			CertificateOidcIssuer: "https://token.actions.githubusercontent.com",
		}},
	}
	if atDestination {
		v.AtDestination = true
	} else {
		v.AtSource = true
	}
	return v
}

func warningOn(t *testing.T, p *Product, field string) Warning {
	t.Helper()
	if err := p.Validate(nil); err != nil {
		t.Fatalf("fixture must be VALID for a warning to be the point, got: %v", err)
	}
	for _, w := range p.computeWarnings() {
		if w.Field == field {
			return w
		}
	}
	var got []string
	for _, w := range p.computeWarnings() {
		got = append(got, w.Field)
	}
	t.Fatalf("expected a warning on %q, got %v", field, got)
	return Warning{}
}

func noWarningOn(t *testing.T, p *Product, field string) {
	t.Helper()
	for _, w := range p.computeWarnings() {
		if w.Field == field {
			t.Fatalf("unexpected warning on %q: %s", field, w.Message)
		}
	}
}

// The trap this whole defence exists for: a mirror rule that copies every
// image and not one signature. The document is VALID - that is the problem.
func TestWarnsWhenMirrorGlobExcludesSignatures(t *testing.T) {
	p := withQuayChain()
	p.Spec.Targets[1].Replication.Mirror.Tags = []string{"v3.*"}
	p.Spec.Verification = verifying(true)

	w := warningOn(t, p, "spec.targets[1].replication.mirror.tags")
	if !strings.Contains(w.Hint, "sha256-") {
		t.Fatalf("hint must show the tag form that is being missed, got %q", w.Hint)
	}
	if !strings.Contains(w.Hint, "report success") {
		t.Fatalf("hint must say the sync SUCCEEDS - that is what makes it expensive, got %q", w.Hint)
	}
}

func TestNoSignatureWarningWhenSignaturesAreMirrored(t *testing.T) {
	p := withQuayChain() // tags already carry 'sha256-*.sig'
	p.Spec.Verification = verifying(true)

	noWarningOn(t, p, "spec.targets[1].replication.mirror.tags")
}

func TestNoSignatureWarningWhenWildcardMirrorsEverything(t *testing.T) {
	p := withQuayChain()
	p.Spec.Targets[1].Replication.Mirror.Tags = []string{"*"}
	p.Spec.Verification = verifying(true)

	noWarningOn(t, p, "spec.targets[1].replication.mirror.tags")
}

// A product that does not verify at the destination has no reason to mirror
// signatures, so warning would be noise - and noise is how warnings get
// switched off.
func TestNoSignatureWarningWithoutDestinationVerification(t *testing.T) {
	p := withQuayChain()
	p.Spec.Targets[1].Replication.Mirror.Tags = []string{"v3.*"}
	p.Spec.Verification = verifying(false)

	noWarningOn(t, p, "spec.targets[1].replication.mirror.tags")
}

func TestNoSignatureWarningWhenSignaturesAreNotTransferred(t *testing.T) {
	p := withQuayChain()
	p.Spec.Targets[1].Replication.Mirror.Tags = []string{"v3.*"}
	off := false
	v := verifying(true)
	v.TransferSignatures = &off
	p.Spec.Verification = v

	noWarningOn(t, p, "spec.targets[1].replication.mirror.tags")
}

// A target-level verification block overrides the product's, so the warning
// has to be computed against the EFFECTIVE settings rather than the product's.
func TestSignatureWarningRespectsTargetOverride(t *testing.T) {
	p := withQuayChain()
	p.Spec.Targets[1].Replication.Mirror.Tags = []string{"v3.*"}
	p.Spec.Verification = verifying(false)
	dest := verifying(true)
	p.Spec.Targets[1].Verification = &dest

	warningOn(t, p, "spec.targets[1].replication.mirror.tags")
}

// Two copy targets both pull the full package from the vendor. Legitimate for
// a DR registry, usually an accident on a metered link, and invisible either
// way - nothing in the document says "this costs twice".
func TestWarnsAboutRedundantVendorEgress(t *testing.T) {
	p := valid()
	p.Spec.Targets = append(p.Spec.Targets, Target{
		Name: "ocp", Registry: "quay.apps.ocp.example.com",
		Repository: "platform/vendor-a", Type: RegistryQuay, Anonymous: true,
	})

	w := warningOn(t, p, "spec.targets")
	if !strings.Contains(w.Hint, "mirror.from") {
		t.Fatalf("hint must name the fix, got %q", w.Hint)
	}
}

func TestNoEgressWarningOnceQuayMirrors(t *testing.T) {
	noWarningOn(t, withQuayChain(), "spec.targets")
}

func TestWarningsAreNotErrors(t *testing.T) {
	p := withQuayChain()
	p.Spec.Targets[1].Replication.Mirror.Tags = []string{"v3.*"}
	p.Spec.Verification = verifying(true)

	if err := p.Validate(nil); err != nil {
		t.Fatalf("a warned document must still load: %v", err)
	}
}
