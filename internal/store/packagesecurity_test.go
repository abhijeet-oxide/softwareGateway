package store

import (
	"errors"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/security"
)

func seedPackageFor(t *testing.T, st Store) int64 {
	t.Helper()
	ctx := t.Context()

	var productID int64
	if err := st.DB().QueryRowContext(ctx,
		`INSERT INTO products (name, config_hash, config) VALUES ('cfx', 'h', '{}') RETURNING id`,
	).Scan(&productID); err != nil {
		t.Fatal(err)
	}
	var repoID int64
	if err := st.DB().QueryRowContext(ctx,
		`INSERT INTO repositories (product_id, role, name, registry_host, repository_path)
		 VALUES (?, 'source', 'vendor', 'registry.example.com', 'cfx') RETURNING id`,
		productID).Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	var packageID int64
	if err := st.DB().QueryRowContext(ctx,
		`INSERT INTO packages (product_id, source_repo_id, tag, manifest_digest, media_type)
		 VALUES (?, ?, '25.7.2131', 'sha256:aaa', 'application/vnd.oci.image.index.v1+json')
		 RETURNING id`, productID, repoID).Scan(&packageID); err != nil {
		t.Fatal(err)
	}
	return packageID
}

// Two people pressing the button at the same moment must start one sync.
func TestClaimIsExclusive(t *testing.T) {
	st := openTestStore(t)
	p := NewPackageSecurity(st)
	id := seedPackageFor(t, st)

	if err := p.Claim(t.Context(), id, time.Hour); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := p.Claim(t.Context(), id, time.Hour); !errors.Is(err, ErrSyncInFlight) {
		t.Fatalf("second claim: %v, want ErrSyncInFlight", err)
	}

	row, ok, err := p.Get(t.Context(), id)
	if err != nil || !ok {
		t.Fatalf("Get: %v ok=%t", err, ok)
	}
	if row.State != PackageSecuritySyncing {
		t.Errorf("state = %q, want syncing", row.State)
	}
	if row.StartedAt == nil {
		t.Error("no startedAt, so the claim can never be recovered")
	}
}

// A claim held by a process that is gone must be reclaimable, or a release
// showing a spinner can never be synced again.
func TestStaleClaimIsReclaimable(t *testing.T) {
	st := openTestStore(t)
	p := NewPackageSecurity(st)
	id := seedPackageFor(t, st)

	if err := p.Claim(t.Context(), id, time.Hour); err != nil {
		t.Fatal(err)
	}

	// Age the claim past the bound. A real one is aged by a process dying;
	// here a few milliseconds and a millisecond bound say the same thing, and
	// say it against the real predicate rather than around it.
	time.Sleep(5 * time.Millisecond)

	if err := p.Claim(t.Context(), id, time.Millisecond); err != nil {
		t.Fatalf("reclaiming a stale claim: %v", err)
	}
	if err := p.Claim(t.Context(), id, time.Hour); !errors.Is(err, ErrSyncInFlight) {
		t.Fatalf("the reclaim did not take the lock: %v", err)
	}

	time.Sleep(5 * time.Millisecond)
	n, err := p.ReleaseAbandoned(t.Context(), time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("released %d claims, want 1", n)
	}
	row, _, _ := p.Get(t.Context(), id)
	if row.State != PackageSecurityFailed || row.Error == "" {
		t.Errorf("row = %+v, want a failed row explaining itself", row)
	}
}

func TestRecordAndReadBack(t *testing.T) {
	st := openTestStore(t)
	p := NewPackageSecurity(st)
	id := seedPackageFor(t, st)

	scanned := time.Date(2026, 6, 29, 1, 14, 0, 0, time.UTC)
	reports := []security.Report{
		{
			Artifact: security.ArtifactRef{Name: "cfx-main", Digest: "sha256:one"},
			Status:   security.StatusScanned, Provider: "jfrog-xray", ScannedAt: &scanned,
			Findings: []security.Finding{
				{CVE: "CVE-1", Severity: security.SeverityCritical, Fixable: true,
					Component: security.Component{ID: "deb://openssl", Name: "openssl"}},
				{CVE: "CVE-2", Severity: security.SeverityLow,
					Component: security.Component{ID: "deb://zlib", Name: "zlib"}},
			},
		},
		{
			Artifact: security.ArtifactRef{Name: "cfx-side", Digest: "sha256:two"},
			Status:   security.StatusNotScanned, Provider: "jfrog-xray",
		},
	}
	for i := range reports {
		reports[i].Recount()
	}

	if err := p.Record(t.Context(), security.PackageResult{
		PackageID: id, Provider: "jfrog-xray", Repository: "cfx-jfrog-lab", Role: "target",
		Posture: security.Summarize(reports), Fingerprint: "abc",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	row, ok, err := p.Get(t.Context(), id)
	if err != nil || !ok {
		t.Fatalf("Get: %v ok=%t", err, ok)
	}
	if row.State != PackageSecuritySynced {
		t.Fatalf("state = %q, want synced", row.State)
	}
	if row.Counts.Total != 2 || row.Counts.BySeverity.Critical != 1 || row.Counts.Fixable != 1 {
		t.Errorf("counts = %+v", row.Counts)
	}
	if row.Counts.NonFixable != 1 {
		t.Errorf("nonFixable = %d, want 1", row.Counts.NonFixable)
	}
	// Coverage travels with the counts, because "2 vulnerabilities" means one
	// thing at full coverage and something else with an artifact unscanned.
	if row.Coverage.Scanned != 1 || row.Coverage.NotScanned != 1 {
		t.Errorf("coverage = %+v", row.Coverage)
	}
	if row.Coverage.Complete() {
		t.Error("coverage reported complete with an unscanned artifact")
	}
	if row.Repository != "cfx-jfrog-lab" || row.Role != "target" {
		t.Errorf("scope = %q/%q, want the JFrog target", row.Role, row.Repository)
	}
	if row.SyncedAt == nil || row.ScannedAt == nil {
		t.Error("timestamps were not recorded")
	}
	if row.StartedAt != nil {
		t.Error("the claim was not released on completion")
	}
}

// A release that synced cleanly last week and whose scanner is unreachable
// today still knows what it knew. Clearing the counts would show nothing when
// something dated is known, which is the worse of the two answers.
func TestFailKeepsTheLastGoodCounts(t *testing.T) {
	st := openTestStore(t)
	p := NewPackageSecurity(st)
	id := seedPackageFor(t, st)

	report := security.Report{
		Artifact: security.ArtifactRef{Name: "cfx-main", Digest: "sha256:one"},
		Status:   security.StatusScanned, Provider: "jfrog-xray",
		Findings: []security.Finding{
			{CVE: "CVE-1", Severity: security.SeverityHigh,
				Component: security.Component{ID: "deb://openssl", Name: "openssl"}},
		},
	}
	report.Recount()
	if err := p.Record(t.Context(), security.PackageResult{
		PackageID: id, Posture: security.Summarize([]security.Report{report}),
	}); err != nil {
		t.Fatal(err)
	}

	if err := p.Fail(t.Context(), id, "Xray refused the credential"); err != nil {
		t.Fatal(err)
	}

	row, _, _ := p.Get(t.Context(), id)
	if row.State != PackageSecurityFailed {
		t.Fatalf("state = %q, want failed", row.State)
	}
	if row.Counts.Total != 1 {
		t.Errorf("counts were cleared by the failure: %+v", row.Counts)
	}
	if row.SyncedAt == nil {
		t.Error("the last successful sync time was lost")
	}
	if row.Error == "" {
		t.Error("the failure carries no reason")
	}
}

// The listing's read is one query for a page, which is why the column is free.
func TestForPackages(t *testing.T) {
	st := openTestStore(t)
	p := NewPackageSecurity(st)
	first := seedPackageFor(t, st)

	if err := p.Record(t.Context(), security.PackageResult{PackageID: first}); err != nil {
		t.Fatal(err)
	}

	got, err := p.ForPackages(t.Context(), []int64{first, first + 999})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	// A release with no row is ABSENT rather than zeroed: the caller renders
	// "never synced", which is not the same fact as "no vulnerabilities".
	if _, ok := got[first+999]; ok {
		t.Error("an unsynced release produced a row")
	}
}
