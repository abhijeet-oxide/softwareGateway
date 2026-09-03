package store

import (
	"context"
	"fmt"
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

// An aged row is still an answer.
//
// This asserted the opposite until the store stopped expiring things, and the
// old assertion was the bug in a test: a release whose findings aged out kept
// its counts - those live in package_security and never expired - and lost the
// table behind them, so the page said "90,808 vulnerabilities" over nothing.
// Age is now a fact the interface reports, not a reason to withhold the row.
func TestSecurityAgedRowsAreStillServed(t *testing.T) {
	st := openTestStore(t)
	sec := NewSecurity(st)
	scope := testScope()
	report := securityReport("cfx-main", "sha256:aaa",
		securityFinding("CVE-1", security.SeverityHigh, "openssl", "1.0", true))

	if err := sec.Save(t.Context(), scope, []security.Report{report}, true, longTTL()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Aged by hand rather than by sleeping. Save coerces a non-positive TTL to
	// its default - a caller passing zero means "use the configured retention",
	// not "evict immediately" - so the only honest way to reach an evictable
	// row in a test is to write one.
	past := securityTime(time.Now().UTC().Add(-time.Hour))
	for _, table := range []string{"security_scans", "security_details"} {
		if _, err := st.DB().ExecContext(t.Context(),
			"UPDATE "+table+" SET evictable_at = ?", past); err != nil {
			t.Fatalf("age %s: %v", table, err)
		}
	}

	refs := []security.ArtifactRef{securityRef("cfx-main", "sha256:aaa")}
	if got, _ := sec.LoadSummaries(t.Context(), scope, refs); len(got) != 1 {
		t.Errorf("served %d aged summaries, want 1", len(got))
	}
	if got, _ := sec.LoadDetails(t.Context(), scope, refs); len(got) != 1 {
		t.Errorf("served %d aged details, want 1", len(got))
	}

	// And inside the budget the sweep leaves them where they are, however old.
	res, err := sec.Sweep(t.Context(), CacheBudget{})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.Freed() {
		t.Errorf("an unbudgeted sweep deleted %d rows; it should delete nothing",
			res.Details+res.Documents+res.Orphans)
	}
	if got, _ := sec.LoadDetails(t.Context(), scope, refs); len(got) != 1 {
		t.Error("the sweep removed a detail row that was inside the budget")
	}
}

// Over the budget, the heavy tier goes and the index stays.
//
// The order is the point: a detail payload costs one scanner request to rebuild
// and a scan row plus its findings costs a whole sync, so the budget is spent
// on the cheap half first and the expensive half is not in it at all.
func TestSecuritySweepEvictsHeavyTierOverBudget(t *testing.T) {
	st := openTestStore(t)
	sec := NewSecurity(st)
	scope := testScope()

	var reports []security.Report
	for _, digest := range []string{"sha256:aaa", "sha256:bbb", "sha256:ccc"} {
		reports = append(reports, securityReport("cfx-"+digest[7:10], digest,
			securityFinding("CVE-1", security.SeverityHigh, "openssl", "1.0", true)))
	}
	if err := sec.Save(t.Context(), scope, reports, true, longTTL()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	past := securityTime(time.Now().UTC().Add(-time.Hour))
	if _, err := st.DB().ExecContext(t.Context(),
		"UPDATE security_details SET evictable_at = ?", past); err != nil {
		t.Fatalf("age details: %v", err)
	}

	before, err := sec.Usage(t.Context())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if before.Details != 3 {
		t.Fatalf("stored %d detail rows, want 3", before.Details)
	}

	// A budget of one byte: everything evictable has to go.
	res, err := sec.Sweep(t.Context(), CacheBudget{Bytes: 1})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.Details == 0 {
		t.Error("an over-budget sweep evicted no detail rows")
	}

	after, err := sec.Usage(t.Context())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if after.Scans != before.Scans {
		t.Errorf("the sweep took %d scan rows; the index tier is not in the budget",
			before.Scans-after.Scans)
	}
	if after.Findings != before.Findings {
		t.Errorf("the sweep took %d finding rows; the index tier is not in the budget",
			before.Findings-after.Findings)
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

// A release bigger than one transaction and one statement round-trips whole.
//
// The write is chunked twice over - `saveChunk` artifacts per transaction,
// `findingsPerStatement` rows per insert - and both boundaries are arithmetic
// nobody looking at a page would notice going wrong. A release that lost its
// last five images, or every finding after the two hundredth, would read as a
// scanner that returned less rather than as a cache that dropped it.
func TestSecuritySavesAcrossChunkBoundaries(t *testing.T) {
	sec := NewSecurity(openTestStore(t))
	scope := testScope()

	// Deliberately not a multiple of either bound: the last partial chunk and
	// the last partial statement are where an off-by-one lives.
	const artifacts = saveChunk*2 + 3
	const findingsEach = findingsPerStatement + 7

	reports := make([]security.Report, 0, artifacts)
	refs := make([]security.ArtifactRef, 0, artifacts)
	for i := 0; i < artifacts; i++ {
		name := fmt.Sprintf("image-%03d", i)
		digest := fmt.Sprintf("sha256:%03d", i)
		findings := make([]security.Finding, 0, findingsEach)
		for j := 0; j < findingsEach; j++ {
			findings = append(findings, securityFinding(
				fmt.Sprintf("CVE-2026-%05d", j), security.SeverityHigh,
				fmt.Sprintf("pkg-%03d", j), "1.0", j%2 == 0))
		}
		reports = append(reports, securityReport(name, digest, findings...))
		refs = append(refs, securityRef(name, digest))
	}

	if err := sec.Save(t.Context(), scope, reports, true, longTTL()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	summaries, err := sec.LoadSummaries(t.Context(), scope, refs)
	if err != nil {
		t.Fatalf("LoadSummaries: %v", err)
	}
	if len(summaries) != artifacts {
		t.Fatalf("read back %d artifacts, want %d", len(summaries), artifacts)
	}
	for _, ref := range refs {
		got, ok := summaries[ref.Ref()]
		if !ok {
			t.Fatalf("%s was not stored", ref.Name)
		}
		if got.Counts.Total != findingsEach {
			t.Errorf("%s counts = %d, want %d", ref.Name, got.Counts.Total, findingsEach)
		}
	}

	// The index rows, which is what search and comparison read: every finding
	// of every artifact, not just the first statement's worth.
	var stored int
	if err := sec.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM security_findings`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if want := artifacts * findingsEach; stored != want {
		t.Errorf("stored %d findings, want %d", stored, want)
	}

	// And a re-save replaces rather than accumulates - the delete is chunked
	// too, and a delete that missed a chunk would double every count.
	if err := sec.Save(t.Context(), scope, reports, true, longTTL()); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	if err := sec.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM security_findings`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if want := artifacts * findingsEach; stored != want {
		t.Errorf("after a re-save, stored %d findings, want %d", stored, want)
	}
}

// A reader that only counts and classifies must not pay for the prose.
//
// The prose tier is per-CVE data written per OCCURRENCE, so merging it means
// decompressing and parsing every stored payload for the release. A comparison
// of two large releases reads none of it and was spending most of its time
// there; security.IndexOnly is the way to say so, and this pins that the two
// tiers genuinely differ rather than the flag being decorative.
func TestSecurityReportsForSkipsTheProseTierWhenAskedTo(t *testing.T) {
	sec := NewSecurity(openTestStore(t))
	scope := testScope()

	rich := securityFinding("CVE-2024-3094", security.SeverityCritical, "xz-utils", "5.4.5", true)
	rich.Description = "A backdoor was planted in the upstream release tarballs."
	rich.References = []string{"https://nvd.nist.gov/vuln/detail/CVE-2024-3094"}
	rich.CVSSScore = 10
	rich.CVSSVector = "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"

	report := securityReport("cfx-main", "sha256:aaa", rich)
	report.Malware = []security.Finding{
		securityFinding("", security.SeverityCritical, "evil-pkg", "0.0.1", false),
	}
	if err := sec.Save(t.Context(), scope, []security.Report{report}, true, longTTL()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	refs := []security.ArtifactRef{securityRef("cfx-main", "sha256:aaa")}

	full, err := sec.ReportsFor(t.Context(), scope, refs, security.WithProse)
	if err != nil {
		t.Fatalf("ReportsFor(WithProse): %v", err)
	}
	if len(full) != 1 || len(full[0].Findings) != 1 {
		t.Fatalf("WithProse returned %d reports", len(full))
	}
	// The premise: without this the test below would pass with the flag
	// ignored, because there would be no prose to leave out.
	if full[0].Findings[0].Description == "" || full[0].Findings[0].CVSSScore == 0 {
		t.Fatalf("WithProse lost the prose: %+v", full[0].Findings[0])
	}
	if len(full[0].Malware) != 1 {
		t.Errorf("WithProse dropped malware, which lives only in that tier")
	}

	lean, err := sec.ReportsFor(t.Context(), scope, refs, security.IndexOnly)
	if err != nil {
		t.Fatalf("ReportsFor(IndexOnly): %v", err)
	}
	if len(lean) != 1 || len(lean[0].Findings) != 1 {
		t.Fatalf("IndexOnly returned %d reports", len(lean))
	}
	got := lean[0].Findings[0]
	// Everything the index holds is still there - identity, grade, remedy -
	// because that is what makes a finding countable and comparable.
	if got.CVE != "CVE-2024-3094" || got.Severity != security.SeverityCritical || !got.Fixable {
		t.Errorf("IndexOnly lost the finding's identity or grade: %+v", got)
	}
	if got.Component.Name != "xz-utils" || got.Component.Version != "5.4.5" {
		t.Errorf("IndexOnly lost the component: %+v", got.Component)
	}
	// And none of the prose, which is the whole point.
	if got.Description != "" || len(got.References) != 0 || got.CVSSScore != 0 || got.CVSSVector != "" {
		t.Errorf("IndexOnly read the detail tier anyway: %+v", got)
	}
}

// The known-exploited flag has to survive a round trip through the index tier,
// because that tier is what a listing reads and what survives eviction of the
// prose. A flag that only lived in the detail payload would vanish from the
// badge the day the payload was evicted.
func TestKEVSurvivesTheIndexTier(t *testing.T) {
	cache := NewSecurity(openTestStore(t))
	ctx := context.Background()

	scope := security.Scope{Product: "cfx", Repository: "lab", Role: "target", Provider: "anchore"}
	ref := security.ArtifactRef{Name: "app", Tag: "1.0", Digest: "sha256:aaa", Kind: "image"}

	report := security.Report{
		Artifact: ref, Status: security.StatusScanned, Provider: "anchore",
		RetrievedAt: time.Now().UTC(),
		Findings: []security.Finding{
			{
				CVE: "CVE-2024-1", Severity: security.SeverityMedium, Fixable: true,
				KEV: true, KEVSource: "anchore",
				Component: security.Component{
					ID: "deb://openssl", Name: "openssl", Version: "1.1.1n", Type: "deb",
				},
				FixedIn: []string{"1.1.1n-1"},
			},
			{
				CVE: "CVE-2024-2", Severity: security.SeverityCritical,
				Component: security.Component{
					ID: "deb://zlib", Name: "zlib", Version: "1.2", Type: "deb",
				},
			},
		},
	}
	report.Recount()

	if err := cache.Save(ctx, scope, []security.Report{report}, true, security.CacheTTL{
		Summary: time.Hour, Detail: time.Hour,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	// The summary tier: the counts a listing draws its badge from.
	summaries, err := cache.LoadSummaries(ctx, scope, []security.ArtifactRef{ref})
	if err != nil {
		t.Fatalf("load summaries: %v", err)
	}
	if got := summaries[ref.Ref()].Counts.KEV; got != 1 {
		t.Errorf("stored KEV count = %d, want 1", got)
	}
	if got := summaries[ref.Ref()].Counts.KEVFixable; got != 1 {
		t.Errorf("stored fixable KEV count = %d, want 1", got)
	}

	// The index tier: the rows a table sorts, WITHOUT the prose.
	reports, err := cache.ReportsFor(ctx, scope, []security.ArtifactRef{ref}, security.IndexOnly)
	if err != nil {
		t.Fatalf("reports: %v", err)
	}
	if len(reports) != 1 || len(reports[0].Findings) != 2 {
		t.Fatalf("expected two stored findings, got %+v", reports)
	}
	// Sorted exploited-first even though it is the LOWER severity: that is the
	// whole rule.
	first := reports[0].Findings[0]
	if !first.KEV || first.CVE != "CVE-2024-1" {
		t.Errorf("the exploited medium did not sort above the plain critical: %+v", reports[0].Findings)
	}
	if first.KEVSource != "anchore" {
		t.Errorf("lost which scanner claimed the exploited flag: %q", first.KEVSource)
	}
	if len(first.Sources) != 1 || first.Sources[0] != "anchore" {
		t.Errorf("a row read back from one scanner's scope has no source: %+v", first.Sources)
	}
}

// The search's known-exploited filter is what somebody uses on the day a
// catalogue is updated: not "what is critical in this product", which they
// know, but "which of my releases carry the four added this morning".
func TestSearchNarrowsToExploited(t *testing.T) {
	cache := NewSecurity(openTestStore(t))
	ctx := context.Background()

	scope := security.Scope{Product: "cfx", Repository: "lab", Role: "target", Provider: "anchore"}
	ref := security.ArtifactRef{Name: "app", Digest: "sha256:aaa", Kind: "image"}
	report := security.Report{
		Artifact: ref, Status: security.StatusScanned, Provider: "anchore",
		RetrievedAt: time.Now().UTC(),
		Findings: []security.Finding{
			{CVE: "CVE-2024-1", Severity: security.SeverityLow, KEV: true,
				Component: security.Component{ID: "deb://openssl", Name: "openssl", Version: "1.0"}},
			{CVE: "CVE-2024-2", Severity: security.SeverityCritical,
				Component: security.Component{ID: "deb://openssl", Name: "openssl", Version: "1.0"}},
		},
	}
	report.Recount()
	if err := cache.Save(ctx, scope, []security.Report{report}, true, security.CacheTTL{
		Summary: time.Hour, Detail: time.Hour,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	all, err := cache.Search(ctx, SearchFilter{Product: "cfx", Kind: SearchComponent, Query: "openssl"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("unfiltered search returned %d hits, want 2", len(all))
	}
	// Exploited first even though it is the lower severity.
	if !all[0].KEV {
		t.Errorf("search did not order the exploited hit first: %+v", all)
	}

	only, err := cache.Search(ctx, SearchFilter{
		Product: "cfx", Kind: SearchComponent, Query: "openssl", KEVOnly: true,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(only) != 1 || only[0].CVE != "CVE-2024-1" {
		t.Fatalf("exploited-only search returned %+v", only)
	}
}
