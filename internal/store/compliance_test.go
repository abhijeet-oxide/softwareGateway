package store

import (
	"testing"
	"time"
)

func seedRun(t *testing.T, p *Packages, packageID int64, id string) {
	t.Helper()
	if err := p.StartComplianceRun(t.Context(), id, packageID, "manual"); err != nil {
		t.Fatal(err)
	}
}

// The claim is a row, not a lock: two Coordinators must not both run a release.
func TestComplianceRunningIsVisibleImmediately(t *testing.T) {
	st := openTestStore(t)
	p := NewPackages(st)
	pkg := seedPackageFor(t, st)

	if _, running, err := p.ComplianceRunning(t.Context(), pkg); err != nil || running {
		t.Fatalf("a fresh release reported a run in progress (err=%v)", err)
	}
	seedRun(t, p, pkg, "run-1")

	id, running, err := p.ComplianceRunning(t.Context(), pkg)
	if err != nil || !running || id != "run-1" {
		t.Fatalf("running = %v, id = %q, err = %v", running, id, err)
	}

	// The listing summary flips at the START, so somebody who pressed the
	// button sees the run rather than pressing it again.
	sum, err := p.PackageCompliance(t.Context(), []int64{pkg})
	if err != nil {
		t.Fatal(err)
	}
	if sum[pkg].State != ComplianceRunning {
		t.Errorf("listing state = %q, want running", sum[pkg].State)
	}
}

func TestFinishWritesRunChartsAndResults(t *testing.T) {
	st := openTestStore(t)
	p := NewPackages(st)
	pkg := seedPackageFor(t, st)
	seedRun(t, p, pkg, "run-2")

	run := ComplianceRunRow{
		ID: "run-2", PackageID: pkg, State: ComplianceComplete, Verdict: "fail",
		BundleDigest: "sha256:bundle", HelmVersion: "v3.16.3", KubeVersion: "1.30.0",
		Checks: 71, Pass: 100, Fail: 3, Skip: 5, Blocking: 2, Warning: 1,
	}
	charts := []ComplianceChartRow{
		{Name: "alpha", Version: "1.0.0", Status: "ok", Resources: 12},
		{Name: "beta", Version: "2.0.0", Status: "failed", Error: "template: beta/x.yaml:3", Resources: 0},
	}
	results := []ComplianceResultRow{
		{Seq: 0, CheckID: "SEC-01", Severity: "block", Outcome: "fail", Determinacy: "fixed",
			Chart: "alpha", SourceFile: "templates/d.yaml", Kind: "Deployment", Name: "app",
			Container: "main", Locus: "securityContext.runAsNonRoot",
			Message: "runs as root", Fingerprint: "fp-1"},
		{Seq: 1, CheckID: "RES-01", Severity: "block", Outcome: "pass",
			Chart: "alpha", Kind: "Deployment", Name: "app", Container: "main"},
	}
	if err := p.FinishComplianceRun(t.Context(), run, charts, results); err != nil {
		t.Fatal(err)
	}

	got, err := p.LatestComplianceRun(t.Context(), pkg)
	if err != nil {
		t.Fatal(err)
	}
	if got.Verdict != "fail" || got.Blocking != 2 || got.Checks != 71 {
		t.Errorf("run = %+v", got)
	}
	// Rule 5: a report that cannot say what produced it cannot be re-derived,
	// and re-deriving it is exactly what happens when a vendor disputes it.
	if got.BundleDigest == "" || got.HelmVersion == "" || got.KubeVersion == "" {
		t.Error("the run does not record what produced it")
	}
	if got.FinishedAt == nil || got.HeartbeatAt != nil {
		t.Error("a finished run still holds its claim")
	}

	// The charts are the DENOMINATOR: a release where one of two charts did not
	// render is not the same as one where both did, and the finding count
	// cannot tell them apart.
	gotCharts, err := p.ComplianceCharts(t.Context(), "run-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(gotCharts) != 2 {
		t.Fatalf("want 2 charts, got %d", len(gotCharts))
	}
	var failed bool
	for _, c := range gotCharts {
		if c.Status == "failed" && c.Error != "" {
			failed = true
		}
	}
	if !failed {
		t.Error("the chart that did not render lost its reason")
	}

	rows, total, err := p.ComplianceResults(t.Context(), "run-2", ComplianceFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(rows) != 2 {
		t.Fatalf("total = %d, rows = %d", total, len(rows))
	}
	// Read back in the order the engine assigned, which is reading order.
	if rows[0].CheckID != "SEC-01" || rows[0].Locus != "securityContext.runAsNonRoot" {
		t.Errorf("first row = %+v", rows[0])
	}
	// Passes are stored too. Without them "40 workloads, all compliant" and
	// "the traversal never reached them" are the same empty list.
	if rows[1].Outcome != "pass" {
		t.Error("the passing result was not stored")
	}
}

func TestComplianceFilters(t *testing.T) {
	st := openTestStore(t)
	p := NewPackages(st)
	pkg := seedPackageFor(t, st)
	seedRun(t, p, pkg, "run-3")

	results := []ComplianceResultRow{
		{Seq: 0, CheckID: "SEC-01", Severity: "block", Outcome: "fail", Determinacy: "fixed",
			Chart: "alpha", Kind: "Deployment", Name: "api", Message: "runs as root"},
		{Seq: 1, CheckID: "SEC-02", Severity: "warn", Outcome: "fail", Determinacy: "configurable",
			Chart: "beta", Kind: "StatefulSet", Name: "db", Message: "escalation allowed"},
		{Seq: 2, CheckID: "RES-01", Severity: "block", Outcome: "pass",
			Chart: "alpha", Kind: "Deployment", Name: "api"},
	}
	if err := p.FinishComplianceRun(t.Context(),
		ComplianceRunRow{ID: "run-3", PackageID: pkg, State: ComplianceComplete, Verdict: "fail"},
		nil, results); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		filter ComplianceFilter
		want   int
	}{
		{"everything", ComplianceFilter{}, 3},
		{"failures only", ComplianceFilter{Outcomes: []string{"fail"}}, 2},
		{"blocking only", ComplianceFilter{Severities: []string{"block"}}, 2},
		{"one chart", ComplianceFilter{Charts: []string{"alpha"}}, 2},
		{"one check", ComplianceFilter{Checks: []string{"SEC-01"}}, 1},
		{"one kind", ComplianceFilter{Kinds: []string{"StatefulSet"}}, 1},
		// The split a reader makes first: the vendor's problem, not the site's.
		{"vendor's to fix", ComplianceFilter{Determinacy: []string{"fixed"}}, 1},
		{"search by message", ComplianceFilter{Search: "root"}, 1},
		{"search by name", ComplianceFilter{Search: "db"}, 1},
		{"search misses", ComplianceFilter{Search: "nothing-here"}, 0},
	}
	for _, c := range cases {
		rows, total, err := p.ComplianceResults(t.Context(), "run-3", c.filter)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if total != c.want || len(rows) != c.want {
			t.Errorf("%s: total = %d, rows = %d, want %d", c.name, total, len(rows), c.want)
		}
	}

	// A page must still report the whole count, or it lies by omission.
	rows, total, err := p.ComplianceResults(t.Context(), "run-3", ComplianceFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || total != 3 {
		t.Errorf("paged: rows = %d, total = %d, want 1 and 3", len(rows), total)
	}
}

// A release whose Coordinator died must become checkable again, or it is stuck
// forever in a state nobody can leave without a database console.
func TestStaleRunsAreReleased(t *testing.T) {
	st := openTestStore(t)
	p := NewPackages(st)
	pkg := seedPackageFor(t, st)
	seedRun(t, p, pkg, "run-4")

	// A live heartbeat is not stale.
	n, err := p.ReleaseStaleComplianceRuns(t.Context(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("released %d live run(s)", n)
	}

	if _, err := p.DB().ExecContext(t.Context(),
		p.Dialect().Rewrite(`UPDATE compliance_runs SET heartbeat_at = ? WHERE id = ?`),
		time.Now().UTC().Add(-2*time.Hour), "run-4"); err != nil {
		t.Fatal(err)
	}

	n, err = p.ReleaseStaleComplianceRuns(t.Context(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("released %d run(s), want 1", n)
	}
	if _, running, _ := p.ComplianceRunning(t.Context(), pkg); running {
		t.Error("the release is still claimed")
	}
	got, err := p.ComplianceRun(t.Context(), "run-4")
	if err != nil {
		t.Fatal(err)
	}
	if got.Error == "" {
		t.Error("a released claim does not say why it was released")
	}
}

// A failed run is kept. "The last attempt failed because the registry refused
// us" is not the same as "this release has never been checked".
func TestFailedRunIsKept(t *testing.T) {
	st := openTestStore(t)
	p := NewPackages(st)
	pkg := seedPackageFor(t, st)
	seedRun(t, p, pkg, "run-5")

	if err := p.FailComplianceRun(t.Context(), "run-5", pkg, "the registry refused us"); err != nil {
		t.Fatal(err)
	}
	got, err := p.LatestComplianceRun(t.Context(), pkg)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != ComplianceFailed || got.Error != "the registry refused us" {
		t.Errorf("run = %+v", got)
	}
	sum, _ := p.PackageCompliance(t.Context(), []int64{pkg})
	if sum[pkg].State != ComplianceFailed {
		t.Errorf("listing state = %q", sum[pkg].State)
	}
}

// Absent means NOT CHECKED. A listing that rendered it as a pass would be
// wrong in the direction that ships.
func TestUncheckedPackageHasNoSummary(t *testing.T) {
	st := openTestStore(t)
	p := NewPackages(st)
	pkg := seedPackageFor(t, st)
	sum, err := p.PackageCompliance(t.Context(), []int64{pkg})
	if err != nil {
		t.Fatal(err)
	}
	if _, present := sum[pkg]; present {
		t.Error("an unchecked release has a compliance summary")
	}
}
