package discovery

import (
	"net/http"
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/test/fakeregistry"
)

// A vendor registry serves a catalogue spanning every customer, and refuses the
// products this one has not bought. That refusal is the entitlement check
// WORKING, and it recurs on every scan forever — so it is neither an error to
// raise nor a fact to discard.

func TestContentTheVendorWillNotServeIsClassifiedRatherThanFailed(t *testing.T) {
	h := newHarness(t, baseDoc)
	h.reg.AddImage(testRepoPath, "v1.0.0", fakeregistry.NewLayer("layer-a"))
	h.reg.AddImage(testRepoPath, "v2.0.0", fakeregistry.NewLayer("layer-b"))

	// The vendor answers 403 for everything it is asked to resolve.
	h.reg.FailNext("/manifests/", http.StatusForbidden, 100)

	res, err := h.scanner.Scan(t.Context())
	if err != nil {
		t.Fatalf("the scan itself failed: %v", err)
	}
	if len(res.TagErrors) == 0 {
		t.Fatal("nothing was refused; the fixture is not reproducing the case")
	}

	for _, e := range res.TagErrors {
		if e.Class != ClassNotEntitled {
			t.Errorf("%s:%s classified %q, want %q — a 403 on a read from a "+
				"vendor registry means this account may not have this product",
				e.Repository, e.Tag, e.Class, ClassNotEntitled)
		}
	}
}

// A credential that did not authenticate at all is a REAL fault affecting
// everything, and folding it in here would silence an outage.
func TestAnUnauthenticatedCredentialIsNotAnEntitlementFact(t *testing.T) {
	h := newHarness(t, baseDoc)
	h.reg.AddImage(testRepoPath, "v1.0.0", fakeregistry.NewLayer("layer-a"))

	h.reg.FailNext("/manifests/", http.StatusUnauthorized, 100)

	res, err := h.scanner.Scan(t.Context())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.TagErrors) == 0 {
		t.Fatal("nothing failed; the fixture is not reproducing the case")
	}
	for _, e := range res.TagErrors {
		if e.Class == ClassNotEntitled {
			t.Errorf("a 401 was classified as an entitlement fact, which would "+
				"silence a broken credential: %s:%s", e.Repository, e.Tag)
		}
	}
}

// And what was refused is written down, so "which orbs are we not entitled to?"
// has an answer between scans.
func TestWhatTheVendorRefusedIsRemembered(t *testing.T) {
	h := newHarness(t, baseDoc)
	h.reg.AddImage(testRepoPath, "v1.0.0", fakeregistry.NewLayer("layer-a"))
	h.reg.FailNext("/manifests/", http.StatusForbidden, 100)

	if _, err := h.scanner.Scan(t.Context()); err != nil {
		t.Fatalf("scan: %v", err)
	}

	rows, err := h.packages.ListUnavailable(t.Context(), "vendor-a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("recorded %d refusals, want 1", len(rows))
	}
	if rows[0].Tag != "v1.0.0" || !strings.Contains(rows[0].Repository, testRepoPath) {
		t.Errorf("recorded %s:%s, want %s:v1.0.0", rows[0].Repository, rows[0].Tag, testRepoPath)
	}
	if rows[0].Reason != "not_entitled" {
		t.Errorf("reason = %q, want not_entitled", rows[0].Reason)
	}

	// Repeating the scan must not accumulate rows: the interesting object is
	// the artifact, not the attempt.
	if _, err := h.scanner.Scan(t.Context()); err != nil {
		t.Fatalf("second scan: %v", err)
	}
	rows, err = h.packages.ListUnavailable(t.Context(), "vendor-a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Errorf("a second scan of the same refusal produced %d rows, want 1", len(rows))
	}
}
