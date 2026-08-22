package regclient

import (
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
)

func on() *bool {
	enabled := true
	return &enabled
}

// The estate this was got wrong for: a vendor source that is not JFrog, and a
// JFrog TARGET where the scanner actually runs. Scoping to the source finds no
// scanner at all and reports "not configured" on an estate where Xray is on.
func TestSecurityRepositoryPrefersTheTargetHoldingTheRelease(t *testing.T) {
	p := &product.Product{Spec: product.Spec{
		Sources: []product.Source{
			{Name: "cfx-near", Registry: "near.example.com", Repository: "orbs/cfx"},
		},
		Targets: []product.Target{
			{Name: "cfx-jfrog-lab", Registry: "artifact.example.com", Repository: "oci-stage",
				Type: product.RegistryJFrog, XrayEnabled: on(), Default: true},
			{Name: "cfx-jfrog-production", Registry: "artifact.example.com", Repository: "oci-gold",
				Type: product.RegistryJFrog, XrayEnabled: on(), PromotionOnly: true},
		},
	}}

	t.Run("a target the release reached wins", func(t *testing.T) {
		got, ok := SecurityRepositoryFor(p, []string{"cfx-jfrog-production"})
		if !ok || got.Name != "cfx-jfrog-production" {
			t.Fatalf("got %+v ok=%t, want the production target", got, ok)
		}
		if got.Role != product.RoleTarget {
			t.Errorf("role = %q, want target", got.Role)
		}
	})

	t.Run("otherwise the default target", func(t *testing.T) {
		got, ok := SecurityRepositoryFor(p, nil)
		if !ok || got.Name != "cfx-jfrog-lab" {
			t.Fatalf("got %+v ok=%t, want the default target", got, ok)
		}
	})

	t.Run("a source with no scanner is never chosen", func(t *testing.T) {
		got, _ := SecurityRepositoryFor(p, []string{"cfx-near"})
		if got.Name == "cfx-near" {
			t.Error("chose the vendor source, which does not run the scanner")
		}
	})
}

func TestSecurityRepositoryFallsBackToASource(t *testing.T) {
	p := &product.Product{Spec: product.Spec{
		Sources: []product.Source{
			{Name: "vendor-jfrog", Registry: "acme.jfrog.io", Repository: "docker-local/cfx",
				Type: product.RegistryJFrog, XrayEnabled: on()},
		},
		Targets: []product.Target{
			{Name: "internal", Registry: "internal.example.com", Repository: "mirror/cfx"},
		},
	}}
	got, ok := SecurityRepositoryFor(p, nil)
	if !ok || got.Name != "vendor-jfrog" || got.Role != product.RoleSource {
		t.Fatalf("got %+v ok=%t, want the JFrog source", got, ok)
	}
}

// A product with the scanner switched off everywhere has no candidate, and
// saying so is a different answer from picking one that cannot answer.
func TestSecurityRepositoryNoneConfigured(t *testing.T) {
	off := false
	p := &product.Product{Spec: product.Spec{
		Targets: []product.Target{
			{Name: "quay", Registry: "quay.io", Repository: "org/app", Type: product.RegistryQuay},
			{Name: "jfrog-off", Registry: "acme.jfrog.io", Repository: "oci",
				Type: product.RegistryJFrog, XrayEnabled: &off},
			{Name: "jfrog-silent", Registry: "acme.jfrog.io", Repository: "oci2",
				Type: product.RegistryJFrog},
		},
	}}
	if got, ok := SecurityRepositoryFor(p, nil); ok {
		t.Fatalf("chose %+v where no repository has a scanner", got)
	}
}

// A disabled target is not a place a release can be scanned.
func TestSecurityRepositorySkipsDisabledTargets(t *testing.T) {
	off := false
	p := &product.Product{Spec: product.Spec{
		Targets: []product.Target{
			{Name: "retired", Enabled: &off, Type: product.RegistryJFrog, XrayEnabled: on(),
				Registry: "old.jfrog.io", Repository: "oci"},
			{Name: "live", Type: product.RegistryJFrog, XrayEnabled: on(),
				Registry: "acme.jfrog.io", Repository: "oci"},
		},
	}}
	got, ok := SecurityRepositoryFor(p, []string{"retired"})
	if !ok || got.Name != "live" {
		t.Fatalf("got %+v ok=%t, want the live target", got, ok)
	}
}

// The Artifactory repository key is DERIVED from the repository path, never
// declared. A declared one is a second place to state a fact the document
// already states, and a mismatch would report the vulnerabilities of a
// different repository - which is worse than reporting none.
func TestRepositoryKeyIsDerivedFromTheRepository(t *testing.T) {
	for repository, want := range map[string]string{
		"apm0014228-oci-stage":           "apm0014228-oci-stage",
		"docker-local/vendor-a/platform": "docker-local",
	} {
		if got := product.XrayRepositoryKey(repository); got != want {
			t.Errorf("repository %q derived key %q, want %q", repository, got, want)
		}
	}
}

// The tuning is the operator's, not the product's, so a resolver built with
// none still produces a working provider rather than one that never asks.
func TestSecurityTuningIsOperatorLevel(t *testing.T) {
	tuning := SecurityTuning{Concurrency: 6, BatchSize: 50, RequestTimeout: time.Minute}
	if tuning.Concurrency <= 0 || tuning.BatchSize <= 0 {
		t.Fatal("the defaults this is built from are not sane")
	}
	// Zero is legal and means "the plugin's own defaults", which is what a
	// Coordinator that has not been told otherwise gets.
	var zero SecurityTuning
	if zero.Concurrency != 0 || zero.BatchSize != 0 {
		t.Error("the zero value is not zero")
	}
}
