package api

import (
	"fmt"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/security"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// WHAT A VULNERABILITY VIEW COSTS AS THE FINDINGS MULTIPLY.
//
// # Why this is a separate sweep
//
// apicost_test.go scales the number of ROWS a listing returns, which is the
// dimension the transfer and package listings grow in. The security views grow
// in a different one: a release has a handful of artifacts and each artifact
// can carry thousands of findings. Scaling pages would hold the interesting
// axis fixed and prove nothing.
//
// So this scales FINDINGS PER ARTIFACT, which is the axis an operator feels -
// "we are loading vulnerabilities at the scale of tens of thousands" - and
// asserts the same shape: the number of round trips must not grow with them.
//
// A view that reads its findings once per finding, or once per CVE, or once
// per component, is an N+1 that only shows up on a real scanner's output. On a
// test estate with three findings it is three extra queries and invisible; on
// a release with twenty thousand it is the page never loading.
func TestTheSecurityViewCostsAFixedNumberOfQueries(t *testing.T) {
	// Four times apart, for the reason apicost_test.go gives: a constant
	// second query would look like growth at 1->2 and is unmistakable at 1->4.
	const few, many = 25, 100

	small := sweepSecurityEndpoints(t, few)
	large := sweepSecurityEndpoints(t, many)

	t.Log("security endpoint cost, in database round trips:")
	t.Logf("  %-56s %7s %7s %8s", "endpoint",
		fmt.Sprintf("n=%d", few), fmt.Sprintf("n=%d", many), "growth")

	for name, a := range small {
		b, ok := large[name]
		if !ok {
			t.Errorf("%s was measured at n=%d and not at n=%d", name, few, many)
			continue
		}
		growth := b.queries - a.queries
		t.Logf("  %-56s %7d %7d %+8d   (%s / %s)", name, a.queries, b.queries,
			growth, a.took.Round(1000), b.took.Round(1000))

		if growth > 0 {
			t.Errorf("%s makes %d more round trips for %d more findings - it is "+
				"reading the database per finding. At a real scanner's output "+
				"that is the page never loading.",
				name, growth, many-few)
		}
	}
}

// sweepSecurityEndpoints seeds one release carrying `findings` findings and
// asks each security view what it costs.
//
// Deterministic: every CVE, component and severity is derived from its index.
func sweepSecurityEndpoints(t *testing.T, findings int) map[string]endpointCost {
	t.Helper()

	h := newSecurityHarness(t)
	tag := h.seedSecurityEstate(t, findings)

	// BY TAG, which is how this API identifies a release - a numeric id is
	// refused, and an endpoint answering 404 costs no queries and would pass
	// this sweep silently. The status check in cost() is what caught that.
	base := "/api/v1/products/vendor-a/packages/" + tag
	endpoints := []struct{ name, url string }{
		{"/packages/{pkg}/security", base + "/security"},
		{"/security/search", "/api/v1/products/vendor-a/security/search?q=CVE"},
	}

	out := make(map[string]endpointCost, len(endpoints))
	for _, ep := range endpoints {
		out[ep.name] = h.cost(t, ep.url)
	}
	return out
}

// seedSecurityEstate stores one scanned report of `n` findings and returns the
// package they belong to.
func (h *securityHarness) seedSecurityEstate(t *testing.T, n int) string {
	t.Helper()

	const tag = "v1.0"
	digest := fmt.Sprintf("sha256:%064x", 7)
	h.seedPackage(tag, digest)

	found := make([]security.Finding, 0, n)
	severities := []security.Severity{
		security.SeverityCritical, security.SeverityHigh,
		security.SeverityMedium, security.SeverityLow,
	}
	for i := range n {
		found = append(found, apiFinding(
			fmt.Sprintf("CVE-2026-%05d", i),
			severities[i%len(severities)],
			fmt.Sprintf("component-%d", i%17), // several findings per component
			i%3 == 0,
		))
	}

	report := scannedReport(found...)
	report.Artifact = security.ArtifactRef{
		Name: "app", Tag: "v1.0", Digest: digest,
		Repository: "vendor-a/platform", Registry: "registry.example.com",
		MediaType: "application/vnd.oci.image.manifest.v1+json", Kind: "image",
	}

	scope := security.Scope{
		Product: "vendor-a", Repository: "vendor", Role: "source",
		Provider: "jfrog-xray",
	}
	// Straight into the store rather than through a fake scanner: this is about
	// what READING the findings costs, and seeding them through a sync would
	// measure the sync.
	if err := store.NewSecurity(h.store).Save(t.Context(), scope,
		[]security.Report{report}, true, security.CacheTTL{}); err != nil {
		t.Fatalf("store %d findings: %v", n, err)
	}
	return tag
}
