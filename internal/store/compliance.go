package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ChartArtifact is one Helm chart inside a release, with the one blob that
// holds it.
//
// # Why the store answers this rather than the caller assembling it
//
// The same argument FileInPackage makes. A compliance run needs chart bytes,
// and reading bytes means fetching a blob from a vendor registry on a
// credentialed connection. If the caller chose the digest, the endpoint behind
// it would be a proxy for arbitrary blobs - a request forgery with credentials
// attached, and one that would happily stream a 40 GB image layer through a
// Coordinator that is not allowed to touch image bytes at all.
//
// So the digest is not a parameter here either. This query returns ONLY the
// content layer of an artifact whose config media type says it is a Helm
// chart, for one package the caller has already resolved. There is no
// argument that makes it return anything else.
type ChartArtifact struct {
	ArtifactID int64
	// Digest of the chart MANIFEST, which is the artifact's identity and what
	// a finding's address records.
	Digest string
	// LayerDigest is the blob holding the chart tarball, and LayerSize is what
	// it weighs. The size is what lets a run refuse a chart before fetching it
	// rather than after.
	LayerDigest string
	LayerSize   int64
	MediaType   string
	// Ref is the repository path the chart sits at, from the artifact's
	// annotations. Part of what a vendor pulls to reproduce a finding.
	Ref string
}

// helmConfigMediaType is what makes an ordinary image manifest a Helm chart.
//
// Helm predates OCI 1.1's artifactType, so a chart is an image manifest whose
// CONFIG declares what it is. A classifier reading only the top-level fields
// sees an image manifest with nothing to distinguish it - which is how a
// vendor's 97 charts were once reported as images.
const helmConfigMediaType = "application/vnd.cncf.helm.config.v1+json"

// ChartArtifacts returns every Helm chart in a package, with its content blob.
//
// Charts only, and their layers only. An image's layers are not reachable
// through this query at all, which is the property that keeps the founding
// invariant intact: the Coordinator may read a few megabytes of chart YAML and
// must never read a container image.
func (p *Packages) ChartArtifacts(ctx context.Context, packageID int64) ([]ChartArtifact, error) {
	// The chart's config blob identifies it; the chart's layer holds it. Both
	// are rows of artifact_blobs for the same artifact, distinguished by kind,
	// so the join is to the same table twice.
	query := p.dialect.Rewrite(`
		SELECT pa.id, pa.digest, layer.digest, COALESCE(b.size_bytes, 0),
		       COALESCE(b.media_type, ''), COALESCE(pa.annotations, '')
		  FROM package_artifacts pa
		  JOIN artifact_blobs cfg   ON cfg.artifact_id = pa.id AND cfg.kind = 'config'
		  JOIN blobs cfgb           ON cfgb.digest = cfg.digest
		  JOIN artifact_blobs layer ON layer.artifact_id = pa.id AND layer.kind = 'layer'
		  LEFT JOIN blobs b         ON b.digest = layer.digest
		 WHERE pa.package_id = ?
		   AND cfgb.media_type = ?
		 ORDER BY pa.id, layer.ordinal`)

	rows, err := p.db.QueryContext(ctx, query, packageID, helmConfigMediaType)
	if err != nil {
		return nil, fmt.Errorf("list chart artifacts for package %d: %w", packageID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []ChartArtifact
	seen := map[int64]bool{}
	for rows.Next() {
		var (
			c           ChartArtifact
			annotations []byte
		)
		if err := rows.Scan(&c.ArtifactID, &c.Digest, &c.LayerDigest,
			&c.LayerSize, &c.MediaType, &annotations); err != nil {
			return nil, fmt.Errorf("scan chart artifact: %w", err)
		}
		// A chart has exactly one content layer. If a manifest declares more,
		// the first is the chart and the rest are provenance files; taking only
		// the first keeps this from unpacking something that is not a chart.
		if seen[c.ArtifactID] {
			continue
		}
		seen[c.ArtifactID] = true

		if len(annotations) > 0 {
			var a map[string]string
			if err := json.Unmarshal(annotations, &a); err == nil {
				c.Ref = a[annotationRefName]
			}
		}
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
	Resources      int
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
	now := time.Now().UTC()
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
		ComplianceRunning, "", 0, 0, 0, 0, nil); err != nil {
		return err
	}
	return tx.Commit()
}

// BeatComplianceRun records that the run is still alive.
func (p *Packages) BeatComplianceRun(ctx context.Context, id string) error {
	_, err := p.db.ExecContext(ctx, p.dialect.Rewrite(
		`UPDATE compliance_runs SET heartbeat_at = ? WHERE id = ? AND state = 'running'`),
		time.Now().UTC(), id)
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
) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin compliance write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	finished := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE compliance_runs
		   SET state = ?, error = ?, verdict = ?,
		       bundle_digest = ?, helm_version = ?, kube_version = ?, checks = ?,
		       pass_count = ?, fail_count = ?, skip_count = ?, error_count = ?, waived_count = ?,
		       blocking_count = ?, warning_count = ?, info_count = ?,
		       truncated = ?, finished_at = ?, heartbeat_at = NULL
		 WHERE id = ?`),
		run.State, nullIfEmpty(run.Error), run.Verdict,
		run.BundleDigest, run.HelmVersion, run.KubeVersion, run.Checks,
		run.Pass, run.Fail, run.Skip, run.Errors, run.Waived,
		run.Blocking, run.Warning, run.Info,
		run.Truncated, finished, run.ID); err != nil {
		return fmt.Errorf("update compliance run: %w", err)
	}

	for _, c := range charts {
		if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
			INSERT INTO compliance_charts
			  (run_id, name, version, artifact_digest, artifact_ref, status, error, resources)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
			run.ID, c.Name, c.Version, c.ArtifactDigest, c.ArtifactRef,
			c.Status, nullIfEmpty(c.Error), c.Resources); err != nil {
			return fmt.Errorf("insert compliance chart %s: %w", c.Name, err)
		}
	}

	stmt, err := tx.PrepareContext(ctx, p.dialect.Rewrite(`
		INSERT INTO compliance_results (
			run_id, seq, check_id, check_title, severity, tier, category, pack,
			remediation, reference, outcome, determinacy,
			chart, chart_version, subchart_path, artifact_digest, artifact_ref,
			source_file, rendered_line, api_version, kind, namespace, name,
			container, container_type, locus,
			observed, expected, message, error, waiver, waiver_expires, fingerprint)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`))
	if err != nil {
		return fmt.Errorf("prepare compliance result insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, r := range results {
		if _, err := stmt.ExecContext(ctx,
			run.ID, r.Seq, r.CheckID, r.CheckTitle, r.Severity, r.Tier, r.Category, r.Pack,
			r.Remediation, r.Reference, r.Outcome, r.Determinacy,
			r.Chart, r.ChartVersion, r.SubchartPath, r.ArtifactDigest, r.ArtifactRef,
			r.SourceFile, r.RenderedLine, r.APIVersion, r.Kind, r.Namespace, r.Name,
			r.Container, r.ContainerType, r.Locus,
			r.Observed, r.Expected, r.Message, r.Error,
			r.Waiver, r.WaiverExpires, r.Fingerprint); err != nil {
			return fmt.Errorf("insert compliance result %d (%s): %w", r.Seq, r.CheckID, err)
		}
	}

	if err := upsertPackageCompliance(ctx, tx, p.dialect, run.PackageID, run.ID,
		run.State, run.Verdict, run.Blocking, run.Warning, run.Errors, run.Pass, &finished); err != nil {
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

	finished := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE compliance_runs
		   SET state = 'failed', error = ?, finished_at = ?, heartbeat_at = NULL
		 WHERE id = ?`), reason, finished, id); err != nil {
		return fmt.Errorf("fail compliance run: %w", err)
	}
	if err := upsertPackageCompliance(ctx, tx, p.dialect, packageID, id,
		ComplianceFailed, "", 0, 0, 0, 0, &finished); err != nil {
		return err
	}
	return tx.Commit()
}

// upsertPackageCompliance keeps the listing summary in step with the run.
func upsertPackageCompliance(
	ctx context.Context, tx *sql.Tx, d Dialect,
	packageID int64, runID, state, verdict string,
	blocking, warning, errCount, pass int, checkedAt *time.Time,
) error {
	query := `
		INSERT INTO package_compliance
		  (package_id, run_id, state, verdict, blocking_count, warning_count, error_count, pass_count, checked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (package_id) DO UPDATE SET
		  run_id = EXCLUDED.run_id, state = EXCLUDED.state, verdict = EXCLUDED.verdict,
		  blocking_count = EXCLUDED.blocking_count, warning_count = EXCLUDED.warning_count,
		  error_count = EXCLUDED.error_count, pass_count = EXCLUDED.pass_count,
		  checked_at = EXCLUDED.checked_at`
	if _, err := tx.ExecContext(ctx, d.Rewrite(query),
		packageID, runID, state, verdict, blocking, warning, errCount, pass, checkedAt); err != nil {
		return fmt.Errorf("update package compliance summary: %w", err)
	}
	return nil
}
