package store

import (
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/security"
)

func testScope() security.Scope {
	return security.Scope{Product: "cfx", Repository: "vendor-jfrog", Role: "source", Provider: "jfrog-xray"}
}

func securityRef(name, digest string) security.ArtifactRef {
	return security.ArtifactRef{Name: name, Digest: digest, Tag: "25.7.2131", Kind: "image", Repository: "docker-local/" + name}
}

func securityReport(name, digest string, findings ...security.Finding) security.Report {
	scanned := time.Date(2026, 6, 29, 1, 14, 0, 0, time.UTC)
	r := security.Report{
		Artifact:    securityRef(name, digest),
		Status:      security.StatusScanned,
		Provider:    "jfrog-xray",
		Findings:    findings,
		ScannedAt:   &scanned,
		RetrievedAt: time.Now().UTC(),
	}
	r.Recount()
	return r
}

func securityFinding(cve string, sev security.Severity, pkg, version string, fixable bool) security.Finding {
	f := security.Finding{
		CVE: cve, ID: "XRAY-1", Severity: sev, Fixable: fixable, Provider: "jfrog-xray",
		Summary:   "A vulnerability in " + pkg,
		Component: security.Component{ID: "deb://" + pkg, Name: pkg, Version: version, Type: "deb"},
	}
	if fixable {
		f.FixedIn = []string{"9.9.9"}
	}
	return f
}

func longTTL() security.CacheTTL {
	return security.CacheTTL{Summary: time.Hour, Detail: 30 * time.Minute}
}

func TestSecurityRoundTripsBothTiers(t *testing.T) {
	sec := NewSecurity(openTestStore(t))
	scope := testScope()
	report := securityReport("cfx-main", "sha256:aaa",
		securityFinding("CVE-2024-3094", security.SeverityCritical, "openssl", "1.1.1n", true),
		securityFinding("CVE-2024-21887", security.SeverityMedium, "zlib", "1.2.11", false),
	)

	if err := sec.Save(t.Context(), scope, []security.Report{report}, true, longTTL()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	refs := []security.ArtifactRef{securityRef("cfx-main", "sha256:aaa")}

	summaries, err := sec.LoadSummaries(t.Context(), scope, refs)
	if err != nil {
		t.Fatalf("LoadSummaries: %v", err)
	}
	got, ok := summaries["sha256:aaa"]
	if !ok {
		t.Fatal("summary not found")
	}
	if got.Status != security.StatusScanned {
		t.Errorf("status = %q", got.Status)
	}
	if got.Counts.Total != 2 || got.Counts.Fixable != 1 || got.Counts.BySeverity.Critical != 1 {
		t.Errorf("counts = %+v", got.Counts)
	}
	if got.ScannedAt == nil {
		t.Error("scannedAt was not round-tripped")
	}
	// The summary tier is counts only: shipping findings would defeat it.
	if len(got.Findings) != 0 {
		t.Errorf("summary carried %d findings, want 0", len(got.Findings))
	}

	details, err := sec.LoadDetails(t.Context(), scope, refs)
	if err != nil {
		t.Fatalf("LoadDetails: %v", err)
	}
	full, ok := details["sha256:aaa"]
	if !ok {
		t.Fatal("detail not found")
	}
	if len(full.Findings) != 2 {
		t.Fatalf("detail carried %d findings, want 2", len(full.Findings))
	}
	if full.Findings[0].CVE != "CVE-2024-3094" {
		t.Errorf("findings out of order: %q first", full.Findings[0].CVE)
	}
}

// The scope is an authorization boundary. The same digest under another product
// is a different row, and must not be served to this one.
func TestSecurityCacheIsScoped(t *testing.T) {
	sec := NewSecurity(openTestStore(t))
	mine := testScope()
	theirs := security.Scope{Product: "other-product", Repository: "vendor-jfrog", Provider: "jfrog-xray"}

	report := securityReport("cfx-main", "sha256:aaa",
		securityFinding("CVE-2024-3094", security.SeverityCritical, "openssl", "1.1.1n", true))
	if err := sec.Save(t.Context(), mine, []security.Report{report}, true, longTTL()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	refs := []security.ArtifactRef{securityRef("cfx-main", "sha256:aaa")}
	for _, tier := range []string{"summary", "detail"} {
		var n int
		if tier == "summary" {
			got, err := sec.LoadSummaries(t.Context(), theirs, refs)
			if err != nil {
				t.Fatal(err)
			}
			n = len(got)
		} else {
			got, err := sec.LoadDetails(t.Context(), theirs, refs)
			if err != nil {
				t.Fatal(err)
			}
			n = len(got)
		}
		if n != 0 {
			t.Errorf("%s tier leaked %d rows across products", tier, n)
		}
	}
}

func TestSecurityExpiredRowsAreNotServed(t *testing.T) {
	st := openTestStore(t)
	sec := NewSecurity(st)
	scope := testScope()
	report := securityReport("cfx-main", "sha256:aaa",
		securityFinding("CVE-1", security.SeverityHigh, "openssl", "1.0", true))

	if err := sec.Save(t.Context(), scope, []security.Report{report}, true, longTTL()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Age both rows by hand rather than by sleeping. Save deliberately coerces
	// a non-positive TTL to its default - a caller passing zero means "use the
	// configured retention", not "expire immediately" - so the only honest way
	// to reach an expired row in a test is to write one.
	past := securityTime(time.Now().UTC().Add(-time.Hour))
	for _, table := range []string{"security_scans", "security_details"} {
		if _, err := st.DB().ExecContext(t.Context(),
			"UPDATE "+table+" SET expires_at = ?", past); err != nil {
			t.Fatalf("age %s: %v", table, err)
		}
	}

	refs := []security.ArtifactRef{securityRef("cfx-main", "sha256:aaa")}
	if got, _ := sec.LoadSummaries(t.Context(), scope, refs); len(got) != 0 {
		t.Errorf("served %d expired summaries", len(got))
	}
	if got, _ := sec.LoadDetails(t.Context(), scope, refs); len(got) != 0 {
		t.Errorf("served %d expired details", len(got))
	}

	scans, details, err := sec.Sweep(t.Context())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if scans != 1 || details != 1 {
		t.Errorf("swept %d scans and %d details, want 1 and 1", scans, details)
	}

	// The cascade takes the index rows with the scan they belong to.
	var findings int
	if err := st.DB().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM security_findings").Scan(&findings); err != nil {
		t.Fatal(err)
	}
	if findings != 0 {
		t.Errorf("%d index rows outlived the scan they belong to", findings)
	}
}

// A refresh drops both tiers. Leaving stale details behind would show a
// refreshed count over an unrefreshed list, which is worse than either.
func TestSecurityInvalidateClearsBothTiers(t *testing.T) {
	sec := NewSecurity(openTestStore(t))
	scope := testScope()
	refs := []security.ArtifactRef{securityRef("cfx-main", "sha256:aaa")}

	if err := sec.Save(t.Context(), scope, []security.Report{
		securityReport("cfx-main", "sha256:aaa", securityFinding("CVE-1", security.SeverityHigh, "openssl", "1.0", true)),
	}, true, longTTL()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := sec.Invalidate(t.Context(), scope, refs); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if got, _ := sec.LoadSummaries(t.Context(), scope, refs); len(got) != 0 {
		t.Errorf("summary survived invalidation")
	}
	if got, _ := sec.LoadDetails(t.Context(), scope, refs); len(got) != 0 {
		t.Errorf("detail survived invalidation")
	}
}

// A re-scan that resolved a finding must remove its index row. A merge that
// only upserted would leave a search naming an image that no longer has it.
func TestSecurityResolvedFindingLeavesTheIndex(t *testing.T) {
	sec := NewSecurity(openTestStore(t))
	scope := testScope()

	before := securityReport("cfx-main", "sha256:aaa",
		securityFinding("CVE-GONE", security.SeverityHigh, "openssl", "1.0", true),
		securityFinding("CVE-STAYS", security.SeverityLow, "zlib", "1.2", false))
	if err := sec.Save(t.Context(), scope, []security.Report{before}, true, longTTL()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	after := securityReport("cfx-main", "sha256:aaa",
		securityFinding("CVE-STAYS", security.SeverityLow, "zlib", "1.2", false))
	if err := sec.Save(t.Context(), scope, []security.Report{after}, true, longTTL()); err != nil {
		t.Fatalf("re-Save: %v", err)
	}

	hits, err := sec.Search(t.Context(), SearchFilter{Product: "cfx", Kind: SearchCVE, Query: "CVE-GONE", Exact: true})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("a resolved finding is still in the search index: %+v", hits)
	}
	if hits, _ := sec.Search(t.Context(), SearchFilter{Product: "cfx", Kind: SearchCVE, Query: "CVE-STAYS", Exact: true}); len(hits) != 1 {
		t.Errorf("the surviving finding left the index too: got %d hits", len(hits))
	}
}

// A counts-only write must not clear the detail tier: the listing that caused
// it renders one column, and the person who opens the release next would pay
// for a full re-fetch.
func TestSecurityCountsOnlySaveKeepsDetails(t *testing.T) {
	sec := NewSecurity(openTestStore(t))
	scope := testScope()
	refs := []security.ArtifactRef{securityRef("cfx-main", "sha256:aaa")}

	full := securityReport("cfx-main", "sha256:aaa",
		securityFinding("CVE-1", security.SeverityHigh, "openssl", "1.0", true))
	if err := sec.Save(t.Context(), scope, []security.Report{full}, true, longTTL()); err != nil {
		t.Fatalf("Save detail: %v", err)
	}

	countsOnly := full
	countsOnly.Findings = nil
	if err := sec.Save(t.Context(), scope, []security.Report{countsOnly}, false, longTTL()); err != nil {
		t.Fatalf("Save counts: %v", err)
	}

	details, err := sec.LoadDetails(t.Context(), scope, refs)
	if err != nil {
		t.Fatalf("LoadDetails: %v", err)
	}
	if len(details["sha256:aaa"].Findings) != 1 {
		t.Error("a counts-only write destroyed the cached findings")
	}
}

// A disabled scanner is a fact about configuration. Caching it would outlive
// the configuration change that fixes it.
func TestSecurityDoesNotCacheDisabledReports(t *testing.T) {
	sec := NewSecurity(openTestStore(t))
	scope := testScope()
	refs := []security.ArtifactRef{securityRef("cfx-main", "sha256:aaa")}

	disabled := security.Report{
		Artifact: securityRef("cfx-main", "sha256:aaa"), Status: security.StatusDisabled,
		Provider: "jfrog-xray", Message: "Xray is not enabled", RetrievedAt: time.Now().UTC(),
	}
	if err := sec.Save(t.Context(), scope, []security.Report{disabled}, true, longTTL()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got, _ := sec.LoadSummaries(t.Context(), scope, refs); len(got) != 0 {
		t.Error("a disabled report was cached")
	}
}

// A scanner failure must not erase the answer another release is reading.
//
// These rows are shared by every release holding the same artifact, so a busy
// Xray that would not answer during one release's sync used to replace another
// release's stored counts and findings with nothing - a page whose summary card
// said 241 vulnerabilities over a table with no rows in it.
func TestSecurityUnavailableDoesNotErasePreviousResult(t *testing.T) {
	sec := NewSecurity(openTestStore(t))
	scope := testScope()
	refs := []security.ArtifactRef{securityRef("cfx-main", "sha256:aaa")}

	report := securityReport("cfx-main", "sha256:aaa",
		securityFinding("CVE-2024-3094", security.SeverityCritical, "openssl", "1.1.1n", true))
	if err := sec.Save(t.Context(), scope, []security.Report{report}, true, longTTL()); err != nil {
		t.Fatalf("Save scanned: %v", err)
	}

	unavailable := security.Report{
		Artifact: securityRef("cfx-main", "sha256:aaa"), Status: security.StatusUnavailable,
		Provider: "jfrog-xray", Message: "JFrog Xray did not answer in time.",
		RetrievedAt: time.Now().UTC(),
	}
	if err := sec.Save(t.Context(), scope, []security.Report{unavailable}, true, longTTL()); err != nil {
		t.Fatalf("Save unavailable: %v", err)
	}

	summaries, err := sec.LoadSummaries(t.Context(), scope, refs)
	if err != nil {
		t.Fatalf("LoadSummaries: %v", err)
	}
	got := summaries["sha256:aaa"]
	if got.Status != security.StatusScanned || got.Counts.Total != 1 {
		t.Errorf("a failed scan overwrote the stored result: status %q, counts %+v", got.Status, got.Counts)
	}

	details, err := sec.LoadDetails(t.Context(), scope, refs)
	if err != nil {
		t.Fatalf("LoadDetails: %v", err)
	}
	if len(details["sha256:aaa"].Findings) != 1 {
		t.Error("a failed scan deleted the stored findings")
	}
}

// An artifact with nothing stored still records the failure, because "the
// scanner would not answer" is a better answer than "never synced".
func TestSecurityUnavailableFillsAnEmptyRow(t *testing.T) {
	sec := NewSecurity(openTestStore(t))
	scope := testScope()
	refs := []security.ArtifactRef{securityRef("cfx-side", "sha256:bbb")}

	report := security.Report{
		Artifact: securityRef("cfx-side", "sha256:bbb"), Status: security.StatusUnavailable,
		Provider: "jfrog-xray", Message: "JFrog Xray did not answer in time.",
		RetrievedAt: time.Now().UTC(),
	}
	if err := sec.Save(t.Context(), scope, []security.Report{report}, true, longTTL()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	summaries, err := sec.LoadSummaries(t.Context(), scope, refs)
	if err != nil {
		t.Fatalf("LoadSummaries: %v", err)
	}
	if summaries["sha256:bbb"].Status != security.StatusUnavailable {
		t.Errorf("status = %q, want unavailable", summaries["sha256:bbb"].Status)
	}
}

// "not scanned" IS cached, and comes back as itself rather than as clean.
func TestSecurityNotScannedSurvivesTheRoundTrip(t *testing.T) {
	sec := NewSecurity(openTestStore(t))
	scope := testScope()
	refs := []security.ArtifactRef{securityRef("cfx-side", "sha256:bbb")}

	report := security.Report{
		Artifact: securityRef("cfx-side", "sha256:bbb"), Status: security.StatusNotScanned,
		Provider: "jfrog-xray", Message: "JFrog Xray has no scan result for this artifact yet.",
		RetrievedAt: time.Now().UTC(),
	}
	if err := sec.Save(t.Context(), scope, []security.Report{report}, true, longTTL()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := sec.LoadSummaries(t.Context(), scope, refs)
	if err != nil {
		t.Fatalf("LoadSummaries: %v", err)
	}
	r := got["sha256:bbb"]
	if r.Status != security.StatusNotScanned {
		t.Fatalf("status = %q, want not_scanned", r.Status)
	}
	if r.Status.Conclusive() {
		t.Error("an unscanned artifact came back conclusive")
	}
	if r.Message == "" {
		t.Error("the reason was lost in the round trip")
	}
}

func TestSecuritySearchByEveryKind(t *testing.T) {
	sec := NewSecurity(openTestStore(t))
	scope := testScope()

	reports := []security.Report{
		securityReport("cfx-main", "sha256:aaa",
			securityFinding("CVE-2024-3094", security.SeverityCritical, "openssl", "1.1.1n", true)),
		securityReport("cfx-sidecar", "sha256:bbb",
			securityFinding("CVE-2024-3094", security.SeverityHigh, "openssl", "1.1.1w", true),
			securityFinding("CVE-2024-0001", security.SeverityLow, "zlib", "1.2.11", false)),
	}
	if err := sec.Save(t.Context(), scope, reports, true, longTTL()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	t.Run("by cve names every image that has it", func(t *testing.T) {
		hits, err := sec.Search(t.Context(), SearchFilter{Product: "cfx", Kind: SearchCVE, Query: "cve-2024-3094"})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 2 {
			t.Fatalf("got %d hits, want 2", len(hits))
		}
		// Worst first, so a truncated page shows the one that matters.
		if hits[0].Severity != "critical" {
			t.Errorf("first hit severity = %q, want critical", hits[0].Severity)
		}
		seen := map[string]bool{hits[0].ArtifactKey: true, hits[1].ArtifactKey: true}
		if !seen["cfx-main"] || !seen["cfx-sidecar"] {
			t.Errorf("hits do not name both images: %v", seen)
		}
	})

	t.Run("by package finds partial matches", func(t *testing.T) {
		hits, err := sec.Search(t.Context(), SearchFilter{Product: "cfx", Kind: SearchComponent, Query: "ssl"})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 2 {
			t.Fatalf("got %d hits, want 2", len(hits))
		}
		// The version rides along even though it is not the identity.
		if hits[0].ComponentVersion == "" {
			t.Error("component version was lost")
		}
	})

	t.Run("by image finds everything wrong with it", func(t *testing.T) {
		hits, err := sec.Search(t.Context(), SearchFilter{Product: "cfx", Kind: SearchImage, Query: "sidecar"})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 2 {
			t.Fatalf("got %d hits, want 2", len(hits))
		}
	})

	t.Run("exact match does not match a substring", func(t *testing.T) {
		hits, err := sec.Search(t.Context(), SearchFilter{Product: "cfx", Kind: SearchComponent, Query: "ssl", Exact: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 0 {
			t.Errorf("exact search matched a substring: %d hits", len(hits))
		}
	})

	t.Run("another product sees nothing", func(t *testing.T) {
		hits, err := sec.Search(t.Context(), SearchFilter{Product: "elsewhere", Kind: SearchCVE, Query: "CVE-2024-3094"})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 0 {
			t.Errorf("search crossed the product boundary: %d hits", len(hits))
		}
	})
}
