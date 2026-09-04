package store

import (
	"errors"
	"fmt"
	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
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
		Log: []compliance.ProgressEvent{
			{At: time.Now().UTC().Truncate(time.Second), Sec: 0, Kind: "info", Text: "Compliance check started"},
			{At: time.Now().UTC().Truncate(time.Second), Sec: 4.5, Kind: "fail", Text: "beta did not render"},
		},
	}
	expires := time.Now().UTC().Add(30 * 24 * time.Hour)
	charts := []ComplianceChartRow{
		{Name: "alpha", Version: "1.0.0", Status: "ok", Resources: 12},
		{Name: "beta", Version: "2.0.0", Status: "failed", Error: "template: beta/x.yaml:3", Resources: 0},
	}
	results := []ComplianceResultRow{
		{Seq: 0, CheckID: "SEC-01", Severity: "critical", Outcome: "fail", Determinacy: "fixed",
			Chart: "alpha", SourceFile: "templates/d.yaml", Kind: "Deployment", Name: "app",
			Container: "main", Locus: "securityContext.runAsNonRoot",
			Message: "runs as root", Fingerprint: "fp-1"},
		{Seq: 1, CheckID: "RES-01", Severity: "critical", Outcome: "pass",
			Chart: "alpha", Kind: "Deployment", Name: "app", Container: "main"},
		// A waived result, so the waiver's expiry goes through the same
		// timestamp path a run's dates do.
		{Seq: 2, CheckID: "SEC-05", Severity: "warning", Outcome: "waived",
			Chart: "alpha", Kind: "Deployment", Name: "app",
			Waiver: "WAIVER-1", WaiverExpires: &expires},
	}
	rendered := []ComplianceRenderedRow{
		{Seq: 0, Chart: "alpha", ChartVersion: "1.0.0",
			Content: "kind: Deployment\nmetadata:\n  name: app\n", Lines: 3, Bytes: 38},
		{Seq: 1, SourceFile: "manifests/crd.yaml",
			Content: "kind: CustomResourceDefinition\n", Lines: 1, Bytes: 30, Truncated: true},
	}
	if err := p.FinishComplianceRun(t.Context(), run, charts, results, rendered); err != nil {
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
	// The transcript, kept with the run. Without it the timeline a watcher read
	// while the check ran disappears the moment it ends - and "which charts
	// refused, and what did the nine minutes go on" is asked afterwards.
	if len(got.Log) != 2 || got.Log[1].Kind != "fail" || got.Log[1].Text != "beta did not render" {
		t.Errorf("run log = %+v, want the two events the run recorded", got.Log)
	}
	if got.Log[1].Sec != 4.5 {
		t.Errorf("run log elapsed = %v, want 4.5", got.Log[1].Sec)
	}
	// The timestamps have to survive the round trip, and a nil check alone
	// would not notice if they did not: SQLite stores these columns as TEXT,
	// and binding a time.Time renders it in Go's own String() layout, which
	// nothing reads back. Every date then came back as the zero value in
	// silence - a finished run looking like one that never finished.
	if got.StartedAt.IsZero() {
		t.Error("startedAt did not survive the round trip")
	}
	if got.FinishedAt != nil && got.FinishedAt.Before(got.StartedAt) {
		t.Errorf("finishedAt %v is before startedAt %v", got.FinishedAt, got.StartedAt)
	}
	if since := time.Since(got.StartedAt); since < 0 || since > time.Hour {
		t.Errorf("startedAt is %v ago, so it did not parse as the time it was written", since)
	}

	// And the listing summary carries the same date, for the same reason: a
	// column that renders "never" over a release checked a minute ago is the
	// same bug one screen further out.
	sum, err := p.PackageCompliance(t.Context(), []int64{pkg})
	if err != nil {
		t.Fatal(err)
	}
	if sum[pkg].CheckedAt == nil {
		t.Error("the listing summary has no checked-at date")
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
	if total != 3 || len(rows) != 3 {
		t.Fatalf("total = %d, rows = %d", total, len(rows))
	}
	// A waiver whose expiry nobody can read is a permanent exception.
	if rows[2].WaiverExpires == nil {
		t.Error("the waiver expiry did not survive the round trip")
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
		{Seq: 0, CheckID: "SEC-01", Severity: "critical", Outcome: "fail", Determinacy: "fixed",
			Chart: "alpha", Kind: "Deployment", Name: "api", Message: "runs as root",
			Subcategory: "Run-as user", Keywords: "runAsUser runAsNonRoot securityContext",
			Locus: "spec.template.spec.securityContext.runAsNonRoot"},
		{Seq: 1, CheckID: "SEC-02", Severity: "warning", Outcome: "fail", Determinacy: "configurable",
			Chart: "beta", Kind: "StatefulSet", Name: "db", Message: "escalation allowed",
			Subcategory: "Privilege escalation", Keywords: "allowPrivilegeEscalation setuid",
			Locus: "spec.template.spec.containers[0].securityContext.allowPrivilegeEscalation"},
		{Seq: 2, CheckID: "RES-01", Severity: "critical", Outcome: "pass",
			Chart: "alpha", Kind: "Deployment", Name: "api",
			Subcategory: "Resource requests", Keywords: "resources.requests.cpu QoS"},
	}
	if err := p.FinishComplianceRun(t.Context(),
		ComplianceRunRow{ID: "run-3", PackageID: pkg, State: ComplianceComplete, Verdict: "fail"},
		nil, results, nil); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		filter ComplianceFilter
		want   int
	}{
		{"everything", ComplianceFilter{}, 3},
		{"failures only", ComplianceFilter{Outcomes: []string{"fail"}}, 2},
		{"critical only", ComplianceFilter{Severities: []string{"critical"}}, 2},
		// The spelling this value used to have, from a link somebody saved or a
		// script somebody wrote. It has to keep narrowing to the same rows: a
		// rename that turns a bookmark into an empty table is a rename that
		// gets reported as data loss.
		{"critical, asked for by its old name", ComplianceFilter{Severities: []string{"block"}}, 2},
		{"one chart", ComplianceFilter{Charts: []string{"alpha"}}, 2},
		{"one check", ComplianceFilter{Checks: []string{"SEC-01"}}, 1},
		{"one kind", ComplianceFilter{Kinds: []string{"StatefulSet"}}, 1},
		// The split a reader makes first: the vendor's problem, not the site's.
		{"vendor's to fix", ComplianceFilter{Determinacy: []string{"fixed"}}, 1},
		{"search by message", ComplianceFilter{Search: "root"}, 1},
		{"search by name", ComplianceFilter{Search: "db"}, 1},
		{"search misses", ComplianceFilter{Search: "nothing-here"}, 0},

		// The engineer's vocabulary. The messages here are written in plain
		// language and contain none of these words - which is the whole reason
		// the keywords are carried on the row. Without them, an engineer
		// searching for the mechanism gets nothing back from a report that is
		// full of findings about it.
		{"search by keyword", ComplianceFilter{Search: "allowPrivilegeEscalation"}, 1},
		{"search by mechanism", ComplianceFilter{Search: "Run-as user"}, 1},
		{"search by field path", ComplianceFilter{Search: "securityContext"}, 2},
		{"search by acronym in keywords", ComplianceFilter{Search: "QoS"}, 1},
		// And the mechanism as a filter, which is the split an engineer makes
		// first and the one the category is too coarse for.
		{"one mechanism", ComplianceFilter{Subcategories: []string{"Privilege escalation"}}, 1},
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

	// A RESULT written before the rename - a run recorded by an older build, a
	// database restored from an older backup - is still read as what it is. The
	// migration rewrites the stored rows, and this is the belt for that brace:
	// counting it under nothing would make an old run render as having no
	// critical findings at all, which reads as a clean release.
	legacy := []ComplianceResultRow{
		{Seq: 0, CheckID: "SEC-03", Severity: "block", Outcome: "fail",
			Chart: "alpha", Kind: "Deployment", Name: "api", Message: "privileged"},
	}
	seedRun(t, p, pkg, "run-legacy")
	if err := p.FinishComplianceRun(t.Context(),
		ComplianceRunRow{ID: "run-legacy", PackageID: pkg, State: ComplianceComplete, Verdict: "fail"},
		nil, legacy, nil); err != nil {
		t.Fatal(err)
	}
	unique, err := p.ComplianceUniqueChecks(t.Context(), "run-legacy")
	if err != nil {
		t.Fatal(err)
	}
	if unique.Blocking != 1 {
		t.Errorf("a result stored as %q counts %d critical checks, want 1", "block", unique.Blocking)
	}
	for _, asked := range []string{"critical", "block"} {
		rows, _, err := p.ComplianceResults(t.Context(), "run-legacy",
			ComplianceFilter{Severities: []string{asked}})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Errorf("filtering a legacy row by %q returned %d rows, want 1", asked, len(rows))
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

// The evidence a finding is shown against: kept with the run, addressed the two
// ways a document can be named, and reclaimed when a later run supersedes it.
func TestComplianceEvidenceIsKeptAndSuperseded(t *testing.T) {
	st := openTestStore(t)
	p := NewPackages(st)
	pkg := seedPackageFor(t, st)

	seedRun(t, p, pkg, "run-ev-1")
	first := []ComplianceRenderedRow{
		{Seq: 0, Chart: "alpha", ChartVersion: "1.0.0", Content: "kind: Deployment\n", Lines: 1, Bytes: 17},
		{Seq: 1, SourceFile: "manifests/ns.yaml", Content: "kind: Namespace\n", Lines: 1, Bytes: 16},
	}
	if err := p.FinishComplianceRun(t.Context(),
		ComplianceRunRow{ID: "run-ev-1", PackageID: pkg, State: ComplianceComplete, Verdict: "pass"},
		nil, nil, first); err != nil {
		t.Fatal(err)
	}

	index, err := p.ComplianceRenderedIndex(t.Context(), "run-ev-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(index) != 2 {
		t.Fatalf("index has %d documents, want 2", len(index))
	}
	// The index carries no content: it is rendered beside a coverage table and
	// the content is megabytes.
	for _, d := range index {
		if d.Content != "" {
			t.Errorf("the index carried the content of %s%s", d.Chart, d.SourceFile)
		}
	}

	// A chart is addressed by its name, a plain manifest by its path.
	byChart, err := p.ComplianceRendered(t.Context(), "run-ev-1", "alpha")
	if err != nil || byChart.Content != "kind: Deployment\n" {
		t.Fatalf("by chart: %+v, %v", byChart, err)
	}
	byFile, err := p.ComplianceRendered(t.Context(), "run-ev-1", "manifests/ns.yaml")
	if err != nil || byFile.Content != "kind: Namespace\n" {
		t.Fatalf("by file: %+v, %v", byFile, err)
	}
	if _, err := p.ComplianceRendered(t.Context(), "run-ev-1", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a document that does not exist returned %v", err)
	}

	// A second run supersedes the first. Its evidence is reclaimed; its
	// RESULTS are not, so the older run still reads back as a run.
	seedRun(t, p, pkg, "run-ev-2")
	if err := p.FinishComplianceRun(t.Context(),
		ComplianceRunRow{ID: "run-ev-2", PackageID: pkg, State: ComplianceComplete, Verdict: "pass"},
		nil, nil, []ComplianceRenderedRow{
			{Seq: 0, Chart: "alpha", Content: "kind: Deployment\n", Lines: 1, Bytes: 17},
		}); err != nil {
		t.Fatal(err)
	}

	old, err := p.ComplianceRenderedIndex(t.Context(), "run-ev-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(old) != 0 {
		t.Fatalf("the superseded run kept %d documents", len(old))
	}
	if _, err := p.ComplianceRun(t.Context(), "run-ev-1"); err != nil {
		t.Fatalf("the superseded RUN was removed too: %v", err)
	}
	fresh, err := p.ComplianceRenderedAll(t.Context(), "run-ev-2")
	if err != nil || len(fresh) != 1 || fresh[0].Content == "" {
		t.Fatalf("the latest run's evidence: %+v, %v", fresh, err)
	}
}

// Compliance runs accumulate: a nightly schedule over a hundred releases is
// thirty-six thousand runs a year, each with its results and the rendered
// manifests behind them. The sweep keeps the newest few of each RELEASE.
// seedSiblingPackage adds a second release to the product a package belongs to,
// because the sweep's whole claim is that it counts PER RELEASE.
func seedSiblingPackage(t *testing.T, st Store, sibling int64, tag string) int64 {
	t.Helper()
	var productID, repoID int64
	if err := st.DB().QueryRowContext(t.Context(),
		`SELECT product_id, source_repo_id FROM packages WHERE id = ?`, sibling,
	).Scan(&productID, &repoID); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := st.DB().QueryRowContext(t.Context(),
		`INSERT INTO packages (product_id, source_repo_id, tag, manifest_digest, media_type)
		 VALUES (?, ?, ?, 'sha256:bbb', 'application/vnd.oci.image.index.v1+json')
		 RETURNING id`, productID, repoID, tag).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRetentionKeepsTheNewestRunsPerRelease(t *testing.T) {
	st := openTestStore(t)
	p := NewPackages(st)
	a := seedPackageFor(t, st)
	b := seedSiblingPackage(t, st, a, "25.8.0")

	// Six runs of one release and two of another, oldest first so the
	// started_at ordering the sweep ranks on is unambiguous.
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("a-%d", i)
		seedRun(t, p, a, id)
		if err := p.FinishComplianceRun(t.Context(),
			ComplianceRunRow{ID: id, PackageID: a, State: ComplianceComplete, Verdict: "pass"},
			nil,
			[]ComplianceResultRow{{Seq: 0, CheckID: "SEC-01", Outcome: "pass"}},
			[]ComplianceRenderedRow{{Seq: 0, Chart: "alpha", Content: "kind: Deployment\n", Lines: 1}},
		); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("b-%d", i)
		seedRun(t, p, b, id)
		if err := p.FinishComplianceRun(t.Context(),
			ComplianceRunRow{ID: id, PackageID: b, State: ComplianceComplete, Verdict: "pass"},
			nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}

	res, err := p.SweepRetention(t.Context(), RetentionPolicy{ComplianceRuns: 3})
	if err != nil {
		t.Fatal(err)
	}
	if res.ComplianceRuns != 3 {
		t.Fatalf("swept %d runs, want 3 (six of one release, keeping three)", res.ComplianceRuns)
	}

	// PER RELEASE: the one checked twice keeps both, because what a release's
	// history is for is "the last few times we checked THIS".
	kept, err := p.ComplianceRuns(t.Context(), b, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 2 {
		t.Fatalf("the release with two runs kept %d", len(kept))
	}

	kept, err = p.ComplianceRuns(t.Context(), a, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 3 {
		t.Fatalf("kept %d runs, want 3", len(kept))
	}
	// The NEWEST three.
	for _, r := range kept {
		if r.ID == "a-0" || r.ID == "a-1" || r.ID == "a-2" {
			t.Errorf("kept %s, which is one of the three oldest", r.ID)
		}
	}

	// A swept run takes its results and its manifests with it. Half a run is
	// worse than no run: a verdict nobody can look behind.
	rows, _, err := p.ComplianceResults(t.Context(), "a-0", ComplianceFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("a swept run left %d result rows behind", len(rows))
	}
	docs, err := p.ComplianceRenderedIndex(t.Context(), "a-0")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 0 {
		t.Errorf("a swept run left %d rendered manifests behind", len(docs))
	}

	// And the LISTING summary survives, because it is what the Software page
	// reads and a swept history must not turn a checked release back into an
	// unchecked one on screen.
	sum, err := p.PackageCompliance(t.Context(), []int64{a})
	if err != nil {
		t.Fatal(err)
	}
	if sum[a].Verdict != "pass" {
		t.Errorf("the listing summary was lost with the runs: %+v", sum[a])
	}
}
