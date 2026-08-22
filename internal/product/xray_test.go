package product

import (
	"strings"
	"testing"
	"time"
)

func boolp(b bool) *bool { return &b }

func TestXrayIsEnabledDefaultsOff(t *testing.T) {
	// Absent means off, and that is deliberate: turning this on would start
	// traffic to a third system the document never mentioned.
	var nilBlock *Xray
	if nilBlock.IsEnabled() {
		t.Error("a product with no xray block reported Xray enabled")
	}
	if (&Xray{}).IsEnabled() {
		t.Error("an xray block with no `enabled` reported Xray enabled")
	}
	if !(&Xray{Enabled: boolp(true)}).IsEnabled() {
		t.Error("xray.enabled: true did not enable Xray")
	}
	if (&Xray{Enabled: boolp(false)}).IsEnabled() {
		t.Error("xray.enabled: false enabled Xray")
	}
}

func TestXrayDefaultsAreNilSafe(t *testing.T) {
	var x *Xray
	if got := x.ConcurrencyOrDefault(); got != DefaultXrayConcurrency {
		t.Errorf("concurrency = %d, want %d", got, DefaultXrayConcurrency)
	}
	if got := x.BatchSizeOrDefault(); got != DefaultXrayBatchSize {
		t.Errorf("batchSize = %d, want %d", got, DefaultXrayBatchSize)
	}
	if got := x.TimeoutOrDefault(); got != DefaultXrayTimeout {
		t.Errorf("timeout = %s, want %s", got, DefaultXrayTimeout)
	}
	if got := x.DetailTTLOrDefault(); got != DefaultXrayDetailTTL {
		t.Errorf("detailTtl = %s, want %s", got, DefaultXrayDetailTTL)
	}
	if got := x.SummaryTTLOrDefault(); got != DefaultXraySummaryTTL {
		t.Errorf("summaryTtl = %s, want %s", got, DefaultXraySummaryTTL)
	}

	set := &Xray{Concurrency: 3, BatchSize: 10, Timeout: Duration(time.Second), DetailTTL: Duration(time.Minute)}
	if set.ConcurrencyOrDefault() != 3 || set.BatchSizeOrDefault() != 10 {
		t.Error("explicit values were overridden by defaults")
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

// An xray block on a repository that is not JFrog is SILENTLY wrong: it parses,
// it applies, and nothing ever reads it. The operator sees a repository they
// believe reports vulnerabilities and which reports none.
func TestValidateXrayRejectsNonJFrogRepository(t *testing.T) {
	errs := validateXray("spec.sources[0].xray", &Xray{Enabled: boolp(true)}, RegistryQuay, false)
	if len(errs) == 0 {
		t.Fatal("accepted an xray block on a Quay repository")
	}
	if !strings.Contains(errs[0].Message, "JFrog") {
		t.Errorf("message = %q, want it to name JFrog", errs[0].Message)
	}
	if errs := validateXray("x", &Xray{Enabled: boolp(true)}, RegistryJFrog, false); len(errs) != 0 {
		t.Errorf("rejected a valid JFrog xray block: %v", errs)
	}
}

// Xray takes the repository's credential. An anonymous repository has none, and
// the resulting 403 reads like a permissions problem rather than a missing one.
func TestValidateXrayRejectsAnonymous(t *testing.T) {
	errs := validateXray("x", &Xray{Enabled: boolp(true)}, RegistryJFrog, true)
	if len(errs) == 0 {
		t.Fatal("accepted xray on an anonymous repository")
	}
	if !strings.Contains(errs[0].Hint, "credentialsRef") {
		t.Errorf("hint = %q, want it to point at credentialsRef", errs[0].Hint)
	}
}

func TestValidateXrayBounds(t *testing.T) {
	for _, tc := range []struct {
		name string
		x    *Xray
		want string
	}{
		{"endpoint without a scheme", &Xray{Endpoint: "acme.jfrog.io"}, "https://"},
		{"concurrency over the ceiling", &Xray{Concurrency: MaxXrayConcurrency + 1}, "maximum"},
		{"batch size over the ceiling", &Xray{BatchSize: MaxXrayBatchSize + 1}, "maximum"},
		{"detail retention over the ceiling", &Xray{DetailTTL: Duration(MaxXrayDetailTTL + time.Hour)}, "maximum"},
		{"negative concurrency", &Xray{Concurrency: -1}, "negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateXray("x", tc.x, RegistryJFrog, false)
			if len(errs) == 0 {
				t.Fatalf("accepted %+v", tc.x)
			}
			joined := errs.Error()
			if !strings.Contains(joined, tc.want) {
				t.Errorf("errors = %q, want them to mention %q", joined, tc.want)
			}
		})
	}

	// A valid block passes every bound.
	ok := &Xray{
		Enabled: boolp(true), Endpoint: "https://acme.jfrog.io", RepositoryKey: "docker-local",
		Concurrency: 6, BatchSize: 50, Timeout: Duration(time.Minute),
		DetailTTL: Duration(15 * time.Minute), SummaryTTL: Duration(6 * time.Hour),
	}
	if errs := validateXray("x", ok, RegistryJFrog, false); len(errs) != 0 {
		t.Errorf("rejected a valid block: %v", errs)
	}
}

func TestXrayForResolvesByRole(t *testing.T) {
	on := &Xray{Enabled: boolp(true)}
	p := Product{Spec: Spec{
		Sources: []Source{{Name: "vendor", Type: RegistryJFrog, Xray: on}},
		Targets: []Target{
			{Name: "prod", Type: RegistryQuay},
			{Name: "store", Type: RegistryArtifactory},
		},
	}}

	if x, ok := p.XrayFor(RoleSource, "vendor"); !ok || !x.IsEnabled() {
		t.Errorf("source vendor: xray = %v ok = %t, want enabled", x, ok)
	}
	// Configured as JFrog with no xray block: capable, switched off. Not the
	// same as a Quay target, which is not capable at all.
	if x, ok := p.XrayFor(RoleTarget, "store"); !ok || x.IsEnabled() {
		t.Errorf("target store: xray = %v ok = %t, want capable but disabled", x, ok)
	}
	if _, ok := p.XrayFor(RoleTarget, "prod"); ok {
		t.Error("a Quay target reported itself capable of Xray")
	}
	if _, ok := p.XrayFor(RoleTarget, "nonexistent"); ok {
		t.Error("an unknown repository reported itself capable of Xray")
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
	// Everything persisted must be a value the schema admits.
	for _, ty := range ValidRegistryTypes {
		c := ty.Canonical()
		if c == RegistryJFrog {
			t.Errorf("%q canonicalises to a value the schema rejects", ty)
		}
	}
}
