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
// those are what an operator releases, verifies and is asked about — "did the
// charts land?" is a question the byte counts cannot answer at all, and the job
// counts answer only for somebody willing to read three thousand rows.
//
// # Per COMPONENT, not per job
//
// A component is one thing a person can name, and the jobs under it are an
// implementation detail of moving it: a component published under two names has
// two manifest jobs, and counting jobs would report it twice. So the rollup is
// per artifact, and the artifact's several jobs are folded into ONE outcome —
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
	// wherever that is absent — which is every artifact Helm has ever pushed.
	// Without it an orb of 157 images and 97 charts reported 257 images.
	ConfigMediaType string
	// Annotations are what the VENDOR said this component is.
	//
	// Carried for the same reason as ConfigMediaType and one step further out.
	// A NEAR orb's charts are plain OCI image manifests whose config is an
	// image config: the three fields above cannot tell them from images, and
	// only `com.nokia.ncd.orb.type` can. Naming the key here would put vendor
	// knowledge in the store, so the map goes out verbatim and the layout
	// plugin reads it — the same evidence, and the same reader, that the
	// artifact listing already classifies by.
	//
	// It is also a GROUPING key, which is what keeps Count honest: two
	// components that agree on all four fields are one row, and two that
	// disagree are two.
	Annotations map[string]string
	// Outcome is one of copied, present, failed, outstanding.
	Outcome string
	Count   int
}

// Outcomes a component can be in. Ordered by precedence: the first that applies
// to any of a component's jobs is the component's outcome.
const (
	// ContentFailed is a component with a job that has given up.
	ContentFailed = "failed"
	// ContentOutstanding is a component with work still to do — including one
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
// outer one turns those counts into a single outcome per component — so a
// component appears exactly once in the result however many places it lands.
//
// The outer grouping is by what a component IS, annotations included. A vendor
// that names its components individually therefore gets close to a row per
// component, which is the price of being able to tell its charts from its
// images at all; a registry that annotates nothing groups as tightly as before.
func (p *Packages) ContentBreakdown(ctx context.Context, transferID string) ([]ContentRow, error) {
	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(`
		SELECT media_type, artifact_type, config_media_type, annotations,
		       CASE WHEN failed      > 0 THEN 'failed'
		            WHEN outstanding > 0 THEN 'outstanding'
		            WHEN copied      > 0 THEN 'copied'
		            WHEN present     > 0 THEN 'present'
		            ELSE 'outstanding' END AS outcome,
		       count(*)
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
		               SUM(CASE WHEN j.state = 'skipped' THEN 1 ELSE 0 END)     AS present
		          FROM transfers t
		          JOIN package_artifacts pa ON pa.package_id = t.package_id
		          LEFT JOIN jobs j
		                 ON j.artifact_id = pa.id
		                AND j.transfer_id = t.id
		         WHERE t.id = ?
		         GROUP BY pa.id, pa.media_type, COALESCE(pa.artifact_type, ''),
		                  pa.annotations
		       ) AS components
		 GROUP BY media_type, artifact_type, config_media_type, annotations, outcome`),
		transferID)
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
			&annotations, &row.Outcome, &row.Count); err != nil {
			return nil, fmt.Errorf("scan content breakdown: %w", err)
		}
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
	// ArtifactID and ArtifactRef locate the component the file came from —
	// `orbs/cfx-5000-k8s/custo:25.7` — so a tree can say where a path lives.
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
// summary wearing the clothes of an answer — and its content cannot be known
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
			// The OCI reserved key, not a vendor's — this is the specification
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
// configured with — a request forgery with credentials attached, and one that
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
