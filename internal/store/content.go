package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// What a transfer is made OF, as opposed to how much it weighs.
//
// # The question this answers
//
// A transfer reports `2486/2489 jobs` and `63.7 GiB`, and neither says what
// moved. An orb is container images, Helm charts and configuration files, and
// those are what an operator releases, verifies and is asked about - "did the
// charts land?" is a question the byte counts cannot answer at all, and the job
// counts answer only for somebody willing to read three thousand rows.
//
// # Per COMPONENT, not per job
//
// A component is one thing a person can name, and the jobs under it are an
// implementation detail of moving it: a component published under two names has
// two manifest jobs, and counting jobs would report it twice. So the rollup is
// per artifact, and the artifact's several jobs are folded into ONE outcome -
// the worst of them, because a component whose second site failed has not
// arrived, whatever its first site did.
//
// # Rows, not names
//
// The media type and artifact type are returned VERBATIM and grouped upstream.
// The store holds rows; deciding that `application/vnd.cncf.helm…` is what a
// person calls a chart is protocol knowledge, and it already lives in exactly
// one place (internal/oci). Duplicating it here to save a fold would be the
// second copy that eventually disagrees.

// ContentRow is one media type in one outcome, and how many components of the
// transfer are in it.
type ContentRow struct {
	MediaType    string
	ArtifactType string
	// ConfigMediaType is what the component's CONFIG blob says it is.
	//
	// Carried because the first two fields cannot tell a Helm chart from an
	// image: a chart is an ordinary image manifest whose config declares it,
	// and OCI 1.1 says the config's media type stands in for `artifactType`
	// wherever that is absent - which is every artifact Helm has ever pushed.
	// Without it an orb of 157 images and 97 charts reported 257 images.
	ConfigMediaType string
	// Annotations are what the VENDOR said this component is.
	//
	// Carried for the same reason as ConfigMediaType and one step further out.
	// A NEAR orb's charts are plain OCI image manifests whose config is an
	// image config: the three fields above cannot tell them from images, and
	// only `com.nokia.ncd.orb.type` can. Naming the key here would put vendor
	// knowledge in the store, so the map goes out verbatim and the layout
	// plugin reads it - the same evidence, and the same reader, that the
	// artifact listing already classifies by.
	//
	// It is also a GROUPING key, which is what keeps Count honest: two
	// components that agree on all four fields are one row, and two that
	// disagree are two.
	Annotations map[string]string
	// Outcome is one of copied, present, failed, outstanding.
	Outcome string
	Count   int
	// SavedBytes is what this transfer did NOT have to move for these
	// components, and CopiedBytes what it did.
	//
	// # Why bytes are here when ContentGroup says they cannot be
	//
	// Because these are not "what a component weighs". A base layer shared by
	// four images belongs to all four, so any per-kind SIZE either counts it
	// four times or picks an owner - which is why the transfer's own byte
	// totals are the ones to trust for that question.
	//
	// This is a different question: which JOBS were skipped. A blob is one job
	// however many components reference it, so summing the skipped jobs
	// partitions the saving exactly - every byte counted once, and the parts
	// add up to the whole. The only softness left is WHICH component a shared
	// blob is filed under, and within a kind that is nearly always the same
	// answer.
	SavedBytes  int64
	CopiedBytes int64

	// Units and the four counts beneath it are the JOBS of these components -
	// every layer, config and manifest that is pushed on its own.
	//
	// # Why a second population is here at all
	//
	// A component's outcome is all-or-nothing: an image is `copied` only once
	// its last layer AND its manifest have landed, because until then it is
	// not at the destination in any usable sense. That is the right answer to
	// "what is there", and a useless one to "how far along is this" - a
	// release of two hundred images sat at nought copied for the whole
	// download and then finished, while sixty thousand layers were visibly
	// moving underneath it.
	//
	// So the components answer the first question and the units answer the
	// second. A job belongs to exactly one artifact, so summing them
	// partitions the transfer exactly - the same property that makes the byte
	// columns above add up.
	Units            int
	UnitsCopied      int
	UnitsPresent     int
	UnitsFailed      int
	UnitsOutstanding int

	// NamedFiles is how many FILES these components carry - layers the
	// publisher gave a name, counted the way PackageFiles counts them.
	//
	// A vendor's file bundle is one component holding a hundred and twelve
	// named layers, so `2` is the right number of components and the wrong
	// answer to "how many files are in this release". The release page has
	// counted files as files since it learnt to list them; this is the same
	// count, for the transfer of the same release, so the two pages cannot
	// disagree about a number a person reads off both.
	NamedFiles int
}

// Outcomes a component can be in. Ordered by precedence: the first that applies
// to any of a component's jobs is the component's outcome.
const (
	// ContentFailed is a component with a job that has given up.
	ContentFailed = "failed"
	// ContentOutstanding is a component with work still to do - including one
	// whose transfer has not finished planning, which has no jobs at all.
	ContentOutstanding = "outstanding"
	// ContentCopied is a component this transfer actually pushed.
	ContentCopied = "copied"
	// ContentPresent is a component the destination already held, so nothing
	// was pushed for it. This is the number that makes a delta transfer legible:
	// 253 present and 6 copied says what `2486 jobs` cannot.
	ContentPresent = "present"
)

// ContentBreakdown counts a transfer's components by what they are and how they
// went.
//
// One query. The inner select folds a component's jobs into counts, and the
// outer one turns those counts into a single outcome per component - so a
// component appears exactly once in the result however many places it lands.
//
// The outer grouping is by what a component IS, annotations included. A vendor
// that names its components individually therefore gets close to a row per
// component, which is the price of being able to tell its charts from its
// images at all; a registry that annotates nothing groups as tightly as before.
func (p *Packages) ContentBreakdown(ctx context.Context, transferID string) ([]ContentRow, error) {
	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(`
		WITH tr AS (
			SELECT id, package_id, target_repo_id FROM transfers WHERE id = ?
		),
		-- ---------------------------------------------------------------
		-- THE LAYERS, one row per distinct piece of content.
		--
		-- Counted over the RELEASE, not over the job queue, and that is the
		-- whole correction. Two things were wrong with reading jobs:
		--
		--  1. Only MANIFEST jobs carry an artifact_id, so joining jobs to a
		--     component by it matched manifests and nothing else. A manifest is
		--     blocked until everything beneath it lands, so every row read zero
		--     copied and zero present for the whole download and then moved at
		--     once at the end - the exact uselessness these columns were added
		--     to fix.
		--  2. A blob the destination already had within its TTL gets NO JOB AT
		--     ALL - deliberately, since a job that exists only to be skipped
		--     still costs a lease and a round trip. So the layers that make a
		--     delta download cheap were invisible to a job-based count, which
		--     is why a download reporting gigabytes already there reported zero
		--     layers already present.
		--
		-- This is the same population the BYTE account uses (see
		-- TransferContentBytes), so the two now reconcile by construction
		-- rather than by coincidence.
		--
		-- ATTRIBUTED ONCE. A base layer shared by fifty images is one layer,
		-- not fifty, so each digest is charged to the lowest component id that
		-- references it. The parts therefore add up to the whole, which is what
		-- lets these columns be summed for the header line.
		unit_owner AS (
			SELECT MIN(ab.artifact_id) AS artifact_id, ab.digest AS digest
			  FROM package_artifacts pa
			  JOIN artifact_blobs ab ON ab.artifact_id = pa.id
			 WHERE pa.package_id = (SELECT package_id FROM tr)
			 GROUP BY ab.digest
			UNION ALL
			-- A manifest is content too, and is what the transfer is still
			-- pushing once every byte beneath it has arrived. It belongs to
			-- itself.
			SELECT pa.id, pa.digest
			  FROM package_artifacts pa
			 WHERE pa.package_id = (SELECT package_id FROM tr)
		),
		unit_state AS (
			SELECT u.artifact_id AS artifact_id,
			       CASE
			         -- Copied wins over present: content this transfer actually
			         -- pushed is copied, whatever a placement record says about
			         -- it now. Same precedence as the byte account.
			         WHEN EXISTS (SELECT 1 FROM jobs j
			                       WHERE j.transfer_id = (SELECT id FROM tr)
			                         AND j.digest = u.digest
			                         AND j.state = 'succeeded') THEN 'copied'
			         WHEN EXISTS (SELECT 1 FROM jobs j
			                       WHERE j.transfer_id = (SELECT id FROM tr)
			                         AND j.digest = u.digest
			                         AND j.state = 'skipped') THEN 'present'
			         -- No job and a placement record: the planner found this
			         -- already at the destination and never queued it.
			         WHEN EXISTS (SELECT 1 FROM blob_placements bp
			                       WHERE bp.repository_id = (SELECT target_repo_id FROM tr)
			                         AND bp.digest = u.digest) THEN 'present'
			         WHEN EXISTS (SELECT 1 FROM jobs j
			                       WHERE j.transfer_id = (SELECT id FROM tr)
			                         AND j.digest = u.digest
			                         AND j.state = 'failed') THEN 'failed'
			         ELSE 'outstanding'
			       END AS outcome
			  FROM unit_owner u
		),
		component_units AS (
			SELECT artifact_id,
			       SUM(CASE WHEN outcome = 'copied'  THEN 1 ELSE 0 END) AS units_copied,
			       SUM(CASE WHEN outcome = 'present' THEN 1 ELSE 0 END) AS units_present,
			       SUM(CASE WHEN outcome = 'failed'  THEN 1 ELSE 0 END) AS units_failed,
			       SUM(CASE WHEN outcome = 'outstanding' THEN 1 ELSE 0 END) AS units_outstanding
			  FROM unit_state
			 GROUP BY artifact_id
		)
		SELECT media_type, artifact_type, config_media_type, annotations,
		       CASE WHEN failed      > 0 THEN 'failed'
		            WHEN outstanding > 0 THEN 'outstanding'
		            WHEN copied      > 0 THEN 'copied'
		            WHEN present     > 0 THEN 'present'
		            ELSE 'outstanding' END AS outcome,
		       count(*), SUM(saved_bytes), SUM(copied_bytes),
		       SUM(units_copied), SUM(units_present), SUM(units_failed),
		       SUM(units_outstanding),
		       SUM(named_files)
		  FROM (
		        SELECT pa.id,
		               pa.media_type                     AS media_type,
		               COALESCE(pa.artifact_type, '')    AS artifact_type,
		               pa.annotations                    AS annotations,
		               -- What the component's config blob says it is. Already
		               -- recorded: the walk stores a manifest's config
		               -- alongside its layers, marked by kind.
		               COALESCE((SELECT b.media_type
		                           FROM artifact_blobs ab
		                           JOIN blobs b ON b.digest = ab.digest
		                          WHERE ab.artifact_id = pa.id
		                            AND ab.kind = 'config'
		                          LIMIT 1), '')          AS config_media_type,
		               SUM(CASE WHEN j.state = 'failed' THEN 1 ELSE 0 END)      AS failed,
		               SUM(CASE WHEN j.state IN ('pending','blocked','leased')
		                        THEN 1 ELSE 0 END)                              AS outstanding,
		               SUM(CASE WHEN j.state = 'succeeded' THEN 1 ELSE 0 END)   AS copied,
		               SUM(CASE WHEN j.state = 'skipped' THEN 1 ELSE 0 END)     AS present,
		               -- The layers charged to this component. MAX, not SUM:
		               -- component_units holds exactly one row per component
		               -- and the join cannot multiply it, so the value is
		               -- constant across this group and any aggregate would do.
		               COALESCE(MAX(cu.units_copied), 0)      AS units_copied,
		               COALESCE(MAX(cu.units_present), 0)     AS units_present,
		               COALESCE(MAX(cu.units_failed), 0)      AS units_failed,
		               COALESCE(MAX(cu.units_outstanding), 0) AS units_outstanding,
		               -- PER JOB, not per component. A blob is one job however
		               -- many components reference it, so this partitions the
		               -- transfer's saving exactly: every byte counted once,
		               -- and the parts add up to the whole.
		               COALESCE(SUM(CASE WHEN j.state = 'skipped'
		                                 THEN j.size_bytes ELSE 0 END), 0)      AS saved_bytes,
		               COALESCE(SUM(CASE WHEN j.state = 'succeeded'
		                                 THEN j.size_bytes ELSE 0 END), 0)      AS copied_bytes,
		               -- The FILES this component carries: layers the publisher
		               -- named. Counted here rather than joined, because a
		               -- second join to artifact_blobs would multiply the job
		               -- rows above it and every byte total with them.
		               (SELECT count(*) FROM artifact_blobs abf
		                 WHERE abf.artifact_id = pa.id
		                   AND abf.kind = 'layer'
		                   AND abf.title IS NOT NULL)                           AS named_files
		          FROM transfers t
		          JOIN package_artifacts pa ON pa.package_id = t.package_id
		          LEFT JOIN jobs j
		                 ON j.artifact_id = pa.id
		                AND j.transfer_id = t.id
		          LEFT JOIN component_units cu ON cu.artifact_id = pa.id
		         WHERE t.id = ?
		         GROUP BY pa.id, pa.media_type, COALESCE(pa.artifact_type, ''),
		                  pa.annotations
		       ) AS components
		 GROUP BY media_type, artifact_type, config_media_type, annotations, outcome`),
		// Twice: once for the `tr` CTE that feeds the layer counts, once for
		// the component grouping's own WHERE.
		transferID, transferID)
	if err != nil {
		return nil, fmt.Errorf("content breakdown of transfer %s: %w", transferID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []ContentRow
	for rows.Next() {
		var (
			row         ContentRow
			annotations []byte
		)
		if err := rows.Scan(&row.MediaType, &row.ArtifactType, &row.ConfigMediaType,
			&annotations, &row.Outcome, &row.Count,
			&row.SavedBytes, &row.CopiedBytes,
			&row.UnitsCopied, &row.UnitsPresent, &row.UnitsFailed,
			&row.UnitsOutstanding, &row.NamedFiles); err != nil {
			return nil, fmt.Errorf("scan content breakdown: %w", err)
		}
		row.Units = row.UnitsCopied + row.UnitsPresent + row.UnitsFailed + row.UnitsOutstanding
		if len(annotations) > 0 {
			// Malformed annotations leave the map nil, so the component is
			// classified by its OCI fields alone. That is a worse answer than
			// the vendor's, and a better one than no breakdown at all.
			_ = json.Unmarshal(annotations, &row.Annotations)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// annotationRefName is the OCI reserved annotation naming a component.
//
// Spelt out here rather than imported from internal/registry: the store holds
// rows and depends on no protocol package, and one reserved string from a
// published specification is a smaller thing to carry than that dependency.
const annotationRefName = "org.opencontainers.image.ref.name"

// PackageFile is one named file inside one of a package's artifacts.
type PackageFile struct {
	// ArtifactID and ArtifactRef locate the component the file came from -
	// `orbs/cfx-5000-k8s/custo:25.7` - so a tree can say where a path lives.
	ArtifactID  int64
	ArtifactRef string
	// Path is the publisher's own name for the layer, which for a single-file
	// artifact IS the file's path.
	Path      string
	SizeBytes int64
	Digest    string
	MediaType string
}

// PackageFiles lists the named files of a package's artifacts.
//
// # What this can and cannot see
//
// A layer that carries `org.opencontainers.image.title` names one file, and
// that name was recorded when the artifact was analysed. Those are these rows:
// the configuration bundles, release notes and scripts a vendor ships, which is
// what somebody opening a release actually wants to look at.
//
// A layer with NO title is a tar of an unknown number of paths. It is not
// listed, because listing it as one entry called `layer sha256:…` would be a
// summary wearing the clothes of an answer - and its content cannot be known
// without downloading and unpacking it. Opaque reports how many there are, so a
// caller can say what it is not showing.
//
// Ordered by artifact and then by layer order, which is the order the publisher
// wrote them in and the order a filesystem would apply them.
func (p *Packages) PackageFiles(ctx context.Context, packageID int64) ([]PackageFile, int, error) {
	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(`
		SELECT pa.id,
		       COALESCE(pa.annotations, ''),
		       ab.title,
		       COALESCE(b.size_bytes, 0),
		       ab.digest,
		       COALESCE(b.media_type, '')
		  FROM package_artifacts pa
		  JOIN artifact_blobs ab ON ab.artifact_id = pa.id
		  LEFT JOIN blobs b ON b.digest = ab.digest
		 WHERE pa.package_id = ?
		   AND ab.kind = 'layer'
		   AND ab.title IS NOT NULL
		 ORDER BY pa.id, ab.ordinal`), packageID)
	if err != nil {
		return nil, 0, fmt.Errorf("list files of package %d: %w", packageID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []PackageFile
	for rows.Next() {
		var (
			f           PackageFile
			annotations []byte
		)
		if err := rows.Scan(&f.ArtifactID, &annotations, &f.Path,
			&f.SizeBytes, &f.Digest, &f.MediaType); err != nil {
			return nil, 0, fmt.Errorf("scan a package file: %w", err)
		}
		if len(annotations) > 0 {
			// The OCI reserved key, not a vendor's - this is the specification
			// saying what a component of an index is called, and every
			// publisher that names its components uses it.
			var a map[string]string
			if err := json.Unmarshal(annotations, &a); err == nil {
				f.ArtifactRef = a[annotationRefName]
			}
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	var opaque int
	if err := p.db.QueryRowContext(ctx, p.dialect.Rewrite(`
		SELECT count(*)
		  FROM package_artifacts pa
		  JOIN artifact_blobs ab ON ab.artifact_id = pa.id
		 WHERE pa.package_id = ? AND ab.kind = 'layer' AND ab.title IS NULL`),
		packageID).Scan(&opaque); err != nil {
		return nil, 0, fmt.Errorf("count opaque layers of package %d: %w", packageID, err)
	}
	return out, opaque, nil
}

// PackageAnalysed reports whether every artifact of a package has been walked.
//
// The question a page asks before it says a release contains nothing: an
// artifact that was only LISTED by its index has no blobs recorded, so a
// release nobody analysed looks identical to one with no content. They are
// different facts and only one of them is worth acting on.
func (p *Packages) PackageAnalysed(ctx context.Context, packageID int64) (bool, error) {
	var unfetched int
	if err := p.db.QueryRowContext(ctx, p.dialect.Rewrite(
		`SELECT count(*) FROM package_artifacts
		  WHERE package_id = ? AND fetched_at IS NULL`), packageID).Scan(&unfetched); err != nil {
		return false, fmt.Errorf("check whether package %d is analysed: %w", packageID, err)
	}
	return unfetched == 0, nil
}

// FileInPackage finds one named file of a package by its blob digest.
//
// # Why this exists rather than a digest fetch
//
// Reading a file's content means fetching a blob from the vendor registry, and
// the digest arrives in a URL. Without this, the handler would be a proxy that
// fetches whatever digest anybody names from whichever registry a product is
// configured with - a request forgery with credentials attached, and one that
// would also happily serve an image layer as though it were a document.
//
// So the digest is not a parameter, it is a LOOKUP KEY: it must already be
// recorded as a titled layer of the package being read. ErrNotFound is the
// answer for everything else, which is both the safe answer and the true one.
func (p *Packages) FileInPackage(ctx context.Context, packageID int64, digest string) (PackageFile, error) {
	var (
		f           PackageFile
		annotations []byte
	)
	err := p.db.QueryRowContext(ctx, p.dialect.Rewrite(`
		SELECT pa.id,
		       COALESCE(pa.annotations, ''),
		       ab.title,
		       COALESCE(b.size_bytes, 0),
		       ab.digest,
		       COALESCE(b.media_type, '')
		  FROM package_artifacts pa
		  JOIN artifact_blobs ab ON ab.artifact_id = pa.id
		  LEFT JOIN blobs b ON b.digest = ab.digest
		 WHERE pa.package_id = ?
		   AND ab.kind = 'layer'
		   AND ab.title IS NOT NULL
		   AND ab.digest = ?
		 LIMIT 1`), packageID, digest).Scan(
		&f.ArtifactID, &annotations, &f.Path, &f.SizeBytes, &f.Digest, &f.MediaType)
	if errors.Is(err, sql.ErrNoRows) {
		return PackageFile{}, ErrNotFound
	}
	if err != nil {
		return PackageFile{}, fmt.Errorf("find file %s of package %d: %w", digest, packageID, err)
	}
	if len(annotations) > 0 {
		var a map[string]string
		if err := json.Unmarshal(annotations, &a); err == nil {
			f.ArtifactRef = a[annotationRefName]
		}
	}
	return f, nil
}
