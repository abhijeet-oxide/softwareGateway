package product

import (
	"strings"
	"testing"
)

func boolp(b bool) *bool { return &b }

// Absent means off, and that is deliberate: turning this on would start traffic
// to a third system the document never mentioned.
func TestXrayIsEnabledDefaultsOff(t *testing.T) {
	if XrayIsEnabled(nil) {
		t.Error("a repository with no xrayEnabled reported Xray on")
	}
	if XrayIsEnabled(boolp(false)) {
		t.Error("xrayEnabled: false enabled Xray")
	}
	if !XrayIsEnabled(boolp(true)) {
		t.Error("xrayEnabled: true did not enable Xray")
	}
}

// The repository key is DERIVED, because the repository path already states it.
// A declared one is a second place for the same fact, and the second place is
// the one that goes stale.
func TestXrayRepositoryKeyIsDerived(t *testing.T) {
	for in, want := range map[string]string{
		"apm0014228-oci-stage":           "apm0014228-oci-stage",
		"docker-local/vendor-a/platform": "docker-local",
		"/docker-local/vendor-a":         "docker-local",
		"docker-local/":                  "docker-local",
		"  docker-remote/cfx  ":          "docker-remote",
		"":                               "",
		"/":                              "",
	} {
		if got := XrayRepositoryKey(in); got != want {
			t.Errorf("XrayRepositoryKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestJFrogTypeSpellings(t *testing.T) {
	if !RegistryArtifactory.IsJFrog() || !RegistryJFrog.IsJFrog() {
		t.Error("both spellings must resolve to the JFrog plugin")
	}
	if RegistryQuay.IsJFrog() || RegistryGeneric.IsJFrog() || RegistryType("").IsJFrog() {
		t.Error("a non-JFrog type reported itself as JFrog")
	}

	var found bool
	for _, ty := range ValidRegistryTypes {
		if ty == RegistryJFrog {
			found = true
		}
	}
	if !found {
		t.Error("jfrog is not in the closed set of registry types, so a document naming it would be rejected")
	}
}

// The document accepts two spellings and the schema knows one. A `jfrog`
// reaching repositories.registry_type would fail a CHECK constraint that this
// change deliberately does not widen.
func TestRegistryTypeCanonical(t *testing.T) {
	if got := RegistryJFrog.Canonical(); got != RegistryArtifactory {
		t.Errorf("jfrog canonicalised to %q, want artifactory", got)
	}
	for _, ty := range []RegistryType{RegistryGeneric, RegistryACR, RegistryArtifactory, RegistryQuay, ""} {
		if got := ty.Canonical(); got != ty {
			t.Errorf("%q canonicalised to %q, want itself", ty, got)
		}
	}
	for _, ty := range ValidRegistryTypes {
		if ty.Canonical() == RegistryJFrog {
			t.Errorf("%q canonicalises to a value the schema rejects", ty)
		}
	}
}

// Xray on a repository that is not JFrog is SILENTLY wrong: it parses, it
// applies, and nothing ever reads it. The operator sees a repository they
// believe reports vulnerabilities and which reports none.
func TestValidateXrayRejectsNonJFrogRepository(t *testing.T) {
	errs := validateXray("spec.sources[0]", boolp(true), "", RegistryQuay, false)
	if len(errs) == 0 {
		t.Fatal("accepted xrayEnabled on a Quay repository")
	}
	if !strings.Contains(errs[0].Message, "JFrog") {
		t.Errorf("message = %q, want it to name JFrog", errs[0].Message)
	}
	if errs := validateXray("x", boolp(true), "", RegistryJFrog, false); len(errs) != 0 {
		t.Errorf("rejected a valid JFrog repository: %v", errs)
	}
	// Off is off everywhere, and says nothing.
	if errs := validateXray("x", nil, "", RegistryQuay, false); len(errs) != 0 {
		t.Errorf("complained about a repository that never mentioned Xray: %v", errs)
	}
}

// Xray takes the repository's credential. An anonymous repository has none, and
// the resulting 403 reads like a permissions problem rather than a missing one.
func TestValidateXrayRejectsAnonymous(t *testing.T) {
	errs := validateXray("x", boolp(true), "", RegistryJFrog, true)
	if len(errs) == 0 {
		t.Fatal("accepted xray on an anonymous repository")
	}
	if !strings.Contains(errs[0].Hint, "credentialsRef") {
		t.Errorf("hint = %q, want it to point at credentialsRef", errs[0].Hint)
	}
}

func TestValidateXrayEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name     string
		enabled  *bool
		endpoint string
		want     string
	}{
		{"no scheme", boolp(true), "acme.jfrog.io", "https://"},
		{"no host", boolp(true), "https://", "host"},
		{"set but not enabled", nil, "https://acme.jfrog.io", "never be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateXray("x", tc.enabled, tc.endpoint, RegistryJFrog, false)
			if len(errs) == 0 {
				t.Fatalf("accepted %q", tc.endpoint)
			}
			if !strings.Contains(errs.Error(), tc.want) {
				t.Errorf("errors = %q, want them to mention %q", errs.Error(), tc.want)
			}
		})
	}

	if errs := validateXray("x", boolp(true), "https://acme.jfrog.io", RegistryJFrog, false); len(errs) != 0 {
		t.Errorf("rejected a valid subdomain override: %v", errs)
	}
}

func TestXrayForResolvesByRole(t *testing.T) {
	p := Product{Spec: Spec{
		Sources: []Source{{Name: "vendor", Type: RegistryJFrog, XrayEnabled: boolp(true)}},
		Targets: []Target{
			{Name: "prod", Type: RegistryQuay},
			{Name: "store", Type: RegistryArtifactory},
		},
	}}

	if on, capable := p.XrayFor(RoleSource, "vendor"); !on || !capable {
		t.Errorf("source vendor: on=%t capable=%t, want both", on, capable)
	}
	// Configured as JFrog with nothing said: capable, switched off. Not the
	// same as a Quay target, which is not capable at all.
	if on, capable := p.XrayFor(RoleTarget, "store"); on || !capable {
		t.Errorf("target store: on=%t capable=%t, want capable but off", on, capable)
	}
	if _, capable := p.XrayFor(RoleTarget, "prod"); capable {
		t.Error("a Quay target reported itself capable of Xray")
	}
	if _, capable := p.XrayFor(RoleTarget, "nonexistent"); capable {
		t.Error("an unknown repository reported itself capable of Xray")
	}
}
