package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
)

// ChartCandidate is one artifact of a release, with the evidence needed to say
// what it is and the blob that holds it.
//
// # Why this returns candidates rather than charts
//
// It used to return charts, selected in SQL by the config media type Helm
// declares. That was wrong twice over, and both ways are documented elsewhere
// in this repository as bugs that already shipped once:
//
//   - An index's children are recorded from what the index LISTED, without
//     fetching each child, so the config media type - the field that normally
//     tells a chart from an image - is not there to compare against.
//   - A NEAR orb's charts are plain OCI image manifests carrying an ordinary
//     image config. Media type, artifact type and config media type are
//     IDENTICAL for its charts and its images, and the only evidence anywhere
//     is the annotation the vendor wrote.
//
// So a release of 95 charts answered "no Helm charts", which is the same
// failure that once reported an orb of 157 images and 97 charts as 254 images.
//
// The fix is not a cleverer query. It is to stop having a second opinion:
// deciding what an artifact IS belongs to vendors.Classifier, which the
// artifact listing, the transfer breakdown and the comparison all already use.
// The store hands out the evidence and the caller classifies, so this reader
// cannot disagree with the page a person was just looking at.
type ChartCandidate struct {
	ArtifactID int64
	// Digest of the MANIFEST, which is the artifact's identity and what a
	// finding's address records.
	Digest string

	// The classification evidence, in the order vendors.Classifier takes it.
	MediaType       string
	ArtifactType    string
	ConfigMediaType string
	Annotations     map[string]string

	// LayerDigest is the blob holding the content, and LayerSize is what it
	// weighs. The size is what lets a run refuse an artifact BEFORE fetching
	// it rather than after - the bound that keeps a mislabelled 40 GB layer
	// out of the Coordinator even if something upstream called it a chart.
	LayerDigest string
	LayerSize   int64
	LayerCount  int
	// Ref is the repository path the artifact sits at, from its annotations.
	Ref string
}

// ChartCandidates returns every artifact of a package that could hold a chart,
// with the evidence needed to decide.
//
// # What is excluded here, and why that is still the safety boundary
//
// Indexes, and anything with no layer at all. What remains is manifests with
// content, which is the set a classifier has to choose from anyway. The
// protection against streaming an image layer through the Coordinator is
// therefore two things working together and neither alone: the caller
// classifies with the product's own classifier, and the fetcher enforces a
// per-artifact byte budget before it opens the blob. A chart is megabytes; the
// budget refuses anything that is not.
func (p *Packages) ChartCandidates(ctx context.Context, packageID int64) ([]ChartCandidate, error) {
	// The config blob's media type is LEFT joined: for an index's children it
	// is legitimately absent, and an inner join would silently drop exactly the
	// artifacts this exists to find.
	query := p.dialect.Rewrite(`
		SELECT pa.id, pa.digest, pa.media_type, COALESCE(pa.artifact_type, ''),
		       COALESCE(cfgb.media_type, ''), COALESCE(pa.annotations, ''),
		       layer.digest, COALESCE(b.size_bytes, 0), layer.ordinal
		  FROM package_artifacts pa
		  JOIN artifact_blobs layer ON layer.artifact_id = pa.id AND layer.kind = 'layer'
		  LEFT JOIN blobs b         ON b.digest = layer.digest
		  LEFT JOIN artifact_blobs cfg ON cfg.artifact_id = pa.id AND cfg.kind = 'config'
		  LEFT JOIN blobs cfgb         ON cfgb.digest = cfg.digest
		 WHERE pa.package_id = ?
		 ORDER BY pa.id, layer.ordinal`)

	rows, err := p.db.QueryContext(ctx, query, packageID)
	if err != nil {
		return nil, fmt.Errorf("list chart candidates for package %d: %w", packageID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []ChartCandidate
	byID := map[int64]int{}
	for rows.Next() {
		var (
			c           ChartCandidate
			annotations []byte
			ordinal     int
		)
		if err := rows.Scan(&c.ArtifactID, &c.Digest, &c.MediaType, &c.ArtifactType,
			&c.ConfigMediaType, &annotations, &c.LayerDigest, &c.LayerSize, &ordinal); err != nil {
			return nil, fmt.Errorf("scan chart candidate: %w", err)
		}
		// A packaged chart is one layer. An artifact with several is counted so
		// the caller can say so rather than silently unpacking the first of
		// twelve and reporting on a fraction of what shipped.
		if i, seen := byID[c.ArtifactID]; seen {
			out[i].LayerCount++
			continue
		}
		if len(annotations) > 0 {
			var a map[string]string
			if err := json.Unmarshal(annotations, &a); err == nil {
				c.Annotations = a
				c.Ref = a[annotationRefName]
			}
		}
		c.LayerCount = 1
		byID[c.ArtifactID] = len(out)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ComplianceRunRow is one run as the database holds it.
type ComplianceRunRow struct {
	ID        string
	PackageID int64
	State     string
	Error     string
	Verdict   string

	BundleDigest string
	HelmVersion  string
	KubeVersion  string
	Checks       int

	Pass, Fail, Skip, Errors, Waived int
	Blocking, Warning, Info          int
	Truncated                        bool
	Trigger                          string
	// Log is the run's transcript, kept so the finished run can show the same
	// timeline the live one did. Bounded by the tracker's ring before it gets
	// here - sixty events, failures kept ahead of routine progress.
	Log []compliance.ProgressEvent

	StartedAt   time.Time
	FinishedAt  *time.Time
	HeartbeatAt *time.Time
}

// Run states.
const (
	ComplianceRunning   = "running"
	ComplianceComplete  = "complete"
	ComplianceFailed    = "failed"
	ComplianceCancelled = "cancelled"
)

// ComplianceChartRow is one chart's contribution to a run.
type ComplianceChartRow struct {
	Name           string
	Version        string
	ArtifactDigest string
	ArtifactRef    string
	Status         string
	Error          string
	// ErrorKind classifies the failure and Attempts says how hard it was tried.
	// See render.FailureKind: ninety-five charts that failed four different ways
	// are four conversations, and an undifferentiated list of stack traces is
	// how they become one complaint about the tool.
	ErrorKind string
	Attempts  int
	Resources int
}

// ComplianceRenderedRow is one document a run judged, kept so a finding can be
// shown against the manifest it came from rather than only asserted.
//
// See db/migrations/postgres/00039_compliance_evidence.sql for why the unit is
// a whole chart stream and why only the latest run keeps its copy.
type ComplianceRenderedRow struct {
	Seq          int
	Chart        string
	ChartVersion string
	SourceFile   string
	Content      string
	Lines        int
	Bytes        int
	Truncated    bool
}

// ComplianceResultRow is one check's judgement about one subject.
//
// Flat on purpose: this is the row a report exports and a filter narrows, and
// every column on it is one the reader can sort or search by.
type ComplianceResultRow struct {
	Seq int

	CheckID     string
	CheckTitle  string
	Severity    string
	Tier        int
	Category    string
	Pack        string
	Remediation string
	Reference   string

	// The triage block: who fixes it, how much work, when it bites, and how
	// firmly the tool knows. Stored per result, not joined from the catalogue,
	// so an exported report keeps saying what it said.
	Confidence  string
	WhenItBites string
	FixOwner    string
	FixEffort   string
	FixExample  string

	Outcome     string
	Determinacy string

	Chart          string
	ChartVersion   string
	SubchartPath   string
	ArtifactDigest string
	ArtifactRef    string
	SourceFile     string
	RenderedLine   int
	APIVersion     string
	Kind           string
	Namespace      string
	Name           string
	Container      string
	ContainerType  string
	Locus          string

	Observed string
	Expected string
	Message  string
	Error    string

	Waiver        string
	WaiverExpires *time.Time
	Fingerprint   string
}

// StartComplianceRun claims a release for checking.
//
// # Why the claim is a row and not a lock
//
// Two Coordinators may both be leader-eligible, and a run takes minutes. An
// in-process mutex would let the second one start a duplicate run against the
// same release, and the two would race to write the summary. The row is the
// claim: a release with a live run cannot get a second one, and the heartbeat
// is what distinguishes a run in progress from a Coordinator that died holding
// one.
func (p *Packages) StartComplianceRun(ctx context.Context, id string, packageID int64, trigger string) error {
	// Written as TEXT in the canonical format, not as a time.Time.
	//
	// SQLite stores these columns as TEXT and the driver renders a bound
	// time.Time with Go's own String() layout - "2026-09-02 03:49:00.6 +0000
	// UTC" - which nothing reads back. Every timestamp then came back as the
	// zero value, silently: a finished run looked like one that had never
	// finished. securityTime is the format the rest of this file's neighbours
	// already write, so the two agree.
	now := securityTime(time.Now().UTC())
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin compliance run: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
		INSERT INTO compliance_runs (id, package_id, state, trigger, started_at, heartbeat_at)
		VALUES (?, ?, 'running', ?, ?, ?)`),
		id, packageID, trigger, now, now); err != nil {
		return fmt.Errorf("insert compliance run: %w", err)
	}

	// The listing summary flips to running immediately, so the Software page
	// shows the run the moment it starts rather than when it finishes. A user
	// who pressed a button and sees nothing presses it again.
	if err := upsertPackageCompliance(ctx, tx, p.dialect, packageID, id,
		ComplianceRunning, "", 0, 0, 0, 0, ComplianceUniqueCounts{}, nil); err != nil {
		return err
	}
	return tx.Commit()
}

// BeatComplianceRun records that the run is still alive.
func (p *Packages) BeatComplianceRun(ctx context.Context, id string) error {
	_, err := p.db.ExecContext(ctx, p.dialect.Rewrite(
		`UPDATE compliance_runs SET heartbeat_at = ? WHERE id = ? AND state = 'running'`),
		securityTime(time.Now().UTC()), id)
	if err != nil {
		return fmt.Errorf("heartbeat compliance run %s: %w", id, err)
	}
	return nil
}

// ComplianceRunning reports whether a release has a live run.
func (p *Packages) ComplianceRunning(ctx context.Context, packageID int64) (string, bool, error) {
	var id string
	err := p.db.QueryRowContext(ctx, p.dialect.Rewrite(
		`SELECT id FROM compliance_runs WHERE package_id = ? AND state = 'running' ORDER BY started_at DESC LIMIT 1`),
		packageID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("check for a running compliance run: %w", err)
	}
	return id, true, nil
}

// FinishComplianceRun writes the whole result of a run in one transaction.
//
// # Why one transaction and not a stream
//
// A partially written run is a run whose counts do not match its rows, and the
// screen built from it says "3 blocking" above a list of one. Ten to fifteen
// thousand rows is a large insert and a small transaction; the alternative -
// visible intermediate states - is a report that is wrong for as long as the
// write takes and unexplainable afterwards if it fails halfway.
func (p *Packages) FinishComplianceRun(
	ctx context.Context, run ComplianceRunRow,
	charts []ComplianceChartRow, results []ComplianceResultRow,
	rendered []ComplianceRenderedRow,
) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin compliance write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	finished := securityTime(time.Now().UTC())
	if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE compliance_runs
		   SET state = ?, error = ?, verdict = ?,
		       bundle_digest = ?, helm_version = ?, kube_version = ?, checks = ?,
		       pass_count = ?, fail_count = ?, skip_count = ?, error_count = ?, waived_count = ?,
		       blocking_count = ?, warning_count = ?, info_count = ?,
		       truncated = ?, log = ?, finished_at = ?, heartbeat_at = NULL
		 WHERE id = ?`),
		run.State, nullIfEmpty(run.Error), run.Verdict,
		run.BundleDigest, run.HelmVersion, run.KubeVersion, run.Checks,
		run.Pass, run.Fail, run.Skip, run.Errors, run.Waived,
		run.Blocking, run.Warning, run.Info,
		run.Truncated, complianceLogJSON(run.Log), finished, run.ID); err != nil {
		return fmt.Errorf("update compliance run: %w", err)
	}

	for _, c := range charts {
		if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
			INSERT INTO compliance_charts
			  (run_id, name, version, artifact_digest, artifact_ref, status, error,
			   error_kind, attempts, resources)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			run.ID, c.Name, c.Version, c.ArtifactDigest, c.ArtifactRef,
			c.Status, nullIfEmpty(c.Error), c.ErrorKind, c.Attempts, c.Resources); err != nil {
			return fmt.Errorf("insert compliance chart %s: %w", c.Name, err)
		}
	}

	stmt, err := tx.PrepareContext(ctx, p.dialect.Rewrite(`
		INSERT INTO compliance_results (
			run_id, seq, check_id, check_title, severity, tier, category, pack,
			remediation, reference, outcome, determinacy,
			confidence, when_it_bites, fix_owner, fix_effort, fix_example,
			chart, chart_version, subchart_path, artifact_digest, artifact_ref,
			source_file, rendered_line, api_version, kind, namespace, name,
			container, container_type, locus,
			observed, expected, message, error, waiver, waiver_expires, fingerprint)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`))
	if err != nil {
		return fmt.Errorf("prepare compliance result insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, r := range results {
		if _, err := stmt.ExecContext(ctx,
			run.ID, r.Seq, r.CheckID, r.CheckTitle, r.Severity, r.Tier, r.Category, r.Pack,
			r.Remediation, r.Reference, r.Outcome, r.Determinacy,
			r.Confidence, r.WhenItBites, r.FixOwner, r.FixEffort, r.FixExample,
			r.Chart, r.ChartVersion, r.SubchartPath, r.ArtifactDigest, r.ArtifactRef,
			r.SourceFile, r.RenderedLine, r.APIVersion, r.Kind, r.Namespace, r.Name,
			r.Container, r.ContainerType, r.Locus,
			r.Observed, r.Expected, r.Message, r.Error,
			r.Waiver, timeOrNil(r.WaiverExpires), r.Fingerprint); err != nil {
			return fmt.Errorf("insert compliance result %d (%s): %w", r.Seq, r.CheckID, err)
		}
	}

	// THE EVIDENCE, and the reclaiming of the evidence it supersedes.
	//
	// This is the one part of a run whose size the vendor sets - a chart that
	// renders four hundred ConfigMaps of embedded certificates is megabytes of
	// YAML - and without the delete every run of every release would keep its
	// own copy forever. The interface reads the latest run and nothing else
	// does, so the older copies are unreachable before they are unwanted.
	//
	// The older runs' RESULTS are untouched. What is reclaimed is the manifests
	// behind them, and a run whose evidence has gone says so rather than
	// showing an empty document.
	if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
		DELETE FROM compliance_rendered
		 WHERE run_id IN (SELECT id FROM compliance_runs
		                   WHERE package_id = ? AND id <> ?)`),
		run.PackageID, run.ID); err != nil {
		return fmt.Errorf("reclaim superseded compliance evidence: %w", err)
	}

	for _, d := range rendered {
		if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
			INSERT INTO compliance_rendered
			  (run_id, seq, chart, chart_version, source_file, content,
			   line_count, byte_count, truncated)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			run.ID, d.Seq, d.Chart, d.ChartVersion, d.SourceFile, d.Content,
			d.Lines, d.Bytes, d.Truncated); err != nil {
			return fmt.Errorf("insert compliance evidence for %s%s: %w",
				d.Chart, d.SourceFile, err)
		}
	}

	// The distinct checks, counted from the rows this transaction is inserting
	// rather than by a second pass over them.
	if err := upsertPackageCompliance(ctx, tx, p.dialect, run.PackageID, run.ID,
		run.State, run.Verdict, run.Blocking, run.Warning, run.Errors, run.Pass,
		uniqueChecksIn(results), finished); err != nil {
		return err
	}
	return tx.Commit()
}

// FailComplianceRun records a run that could not be completed.
//
// A failed run is kept rather than deleted. "The last attempt failed at 14:22
// because the registry refused us" is a thing an operator needs to see, and it
// is not the same as "this release has never been checked" - which is what a
// deleted row would leave behind.
func (p *Packages) FailComplianceRun(ctx context.Context, id string, packageID int64, reason string) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin compliance failure: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	finished := securityTime(time.Now().UTC())
	if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE compliance_runs
		   SET state = 'failed', error = ?, finished_at = ?, heartbeat_at = NULL
		 WHERE id = ?`), reason, finished, id); err != nil {
		return fmt.Errorf("fail compliance run: %w", err)
	}
	if err := upsertPackageCompliance(ctx, tx, p.dialect, packageID, id,
		ComplianceFailed, "", 0, 0, 0, 0, ComplianceUniqueCounts{}, finished); err != nil {
		return err
	}
	return tx.Commit()
}

// upsertPackageCompliance keeps the listing summary in step with the run.
func upsertPackageCompliance(
	ctx context.Context, tx *sql.Tx, d Dialect,
	packageID int64, runID, state, verdict string,
	blocking, warning, errCount, pass int, unique ComplianceUniqueCounts, checkedAt any,
) error {
	query := `
		INSERT INTO package_compliance
		  (package_id, run_id, state, verdict, blocking_count, warning_count, error_count, pass_count,
		   unique_blocking, unique_warning, checked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (package_id) DO UPDATE SET
		  run_id = EXCLUDED.run_id, state = EXCLUDED.state, verdict = EXCLUDED.verdict,
		  blocking_count = EXCLUDED.blocking_count, warning_count = EXCLUDED.warning_count,
		  error_count = EXCLUDED.error_count, pass_count = EXCLUDED.pass_count,
		  unique_blocking = EXCLUDED.unique_blocking, unique_warning = EXCLUDED.unique_warning,
		  checked_at = EXCLUDED.checked_at`
	if _, err := tx.ExecContext(ctx, d.Rewrite(query),
		packageID, runID, state, verdict, blocking, warning, errCount, pass,
		unique.Blocking, unique.Warning, checkedAt); err != nil {
		return fmt.Errorf("update package compliance summary: %w", err)
	}
	return nil
}

// RecordComplianceRun adapts a finished run into this table's rows.
//
// The adapter lives here rather than in internal/compliance because the
// dependency points this way: the store knows about the compliance model, and
// the compliance model must not know about SQL. It is the same arrangement
// PackageSecurity.Record uses, for the same reason.
func (p *Packages) RecordComplianceRun(ctx context.Context, runID string, packageID int64, run *compliance.Run) error {
	row := ComplianceRunRow{
		ID: runID, PackageID: packageID, State: ComplianceComplete,
		Verdict:      string(run.Verdict),
		BundleDigest: run.BundleDigest,
		HelmVersion:  run.HelmVersion,
		KubeVersion:  run.KubeVersion,
		Checks:       run.Checks,
		Pass:         run.Counts.Pass,
		Fail:         run.Counts.Fail,
		Skip:         run.Counts.Skip,
		Errors:       run.Counts.Error,
		Waived:       run.Counts.Waived,
		Blocking:     run.Counts.Blocking,
		Warning:      run.Counts.Warning,
		Info:         run.Counts.Info,
		Truncated:    run.Truncated,
		Log:          run.Log,
	}

	charts := make([]ComplianceChartRow, 0, len(run.Charts))
	for _, c := range run.Charts {
		charts = append(charts, ComplianceChartRow{
			Name: c.Name, Version: c.Version, Status: c.Status,
			Error: c.Error, ErrorKind: c.ErrorKind, Attempts: c.Attempts,
			Resources: c.Resources,
		})
	}

	results := make([]ComplianceResultRow, 0, len(run.Results))
	for i, r := range run.Results {
		a := r.Address
		results = append(results, ComplianceResultRow{
			// seq is the engine's reading order - failures first, then the
			// undecidable, then passes - so a page read back by seq is the
			// page a person was shown. Reconstructing that order in SQL would
			// approximate it differently.
			Seq:         i,
			CheckID:     r.CheckID,
			CheckTitle:  r.CheckTitle,
			Severity:    string(r.Severity),
			Tier:        int(r.Tier),
			Category:    r.Category,
			Pack:        r.Pack,
			Remediation: r.Remediation,
			Reference:   r.Reference,
			Confidence:  string(r.Confidence),
			WhenItBites: string(r.WhenItBites),
			FixOwner:    string(r.FixOwner),
			FixEffort:   string(r.FixEffort),
			FixExample:  r.FixExample,
			Outcome:     string(r.Outcome),
			Determinacy: string(r.Determinacy),

			Chart: a.Chart, ChartVersion: a.ChartVersion, SubchartPath: a.SubchartPath,
			ArtifactDigest: a.ArtifactDigest, ArtifactRef: a.ArtifactRef,
			SourceFile: a.SourceFile, RenderedLine: a.RenderedLine,
			APIVersion: a.APIVersion, Kind: a.Kind, Namespace: a.Namespace, Name: a.Name,
			Container: a.Container, ContainerType: a.ContainerType, Locus: a.Locus,

			Observed: r.Observed, Expected: r.Expected, Message: r.Message, Error: r.Error,
			Waiver: r.Waiver, WaiverExpires: r.WaiverExpires,
			Fingerprint: r.Fingerprint(),
		})
	}
	rendered := make([]ComplianceRenderedRow, 0, len(run.Rendered))
	for i, d := range run.Rendered {
		rendered = append(rendered, ComplianceRenderedRow{
			Seq: i, Chart: d.Chart, ChartVersion: d.ChartVersion,
			SourceFile: d.SourceFile, Content: string(d.Content),
			Lines: d.Lines, Bytes: d.Bytes, Truncated: d.Truncated,
		})
	}

	return p.FinishComplianceRun(ctx, row, charts, results, rendered)
}

// complianceLogJSON encodes a run's transcript for the row.
//
// NULL rather than "[]" for a run with no events, so a run recorded before this
// column existed and one that genuinely logged nothing read the same - both are
// "no transcript", and the interface offers no log button for either.
func complianceLogJSON(events []compliance.ProgressEvent) any {
	if len(events) == 0 {
		return nil
	}
	b, err := json.Marshal(events)
	if err != nil {
		// Unreachable for this shape, and not worth failing a whole run's write
		// over: the transcript is a convenience beside the results.
		return nil
	}
	return string(b)
}

// uniqueChecksIn counts the distinct checks that FAILED, by severity.
//
// From the rows being written, so the listing's number and the run's own can
// never disagree: they are two readings of one list rather than two queries.
// Failures only, like the severity counts beside them - a passing critical
// check is not a critical anything.
func uniqueChecksIn(results []ComplianceResultRow) ComplianceUniqueCounts {
	seen := make(map[string]struct{}, 64)
	var out ComplianceUniqueCounts
	for _, r := range results {
		if r.Outcome != string(compliance.OutcomeFail) {
			continue
		}
		if _, dup := seen[r.CheckID]; dup {
			continue
		}
		seen[r.CheckID] = struct{}{}
		switch r.Severity {
		case "block":
			out.Blocking++
		case "warn":
			out.Warning++
		case "info":
			out.Info++
		}
	}
	return out
}
