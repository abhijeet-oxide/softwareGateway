package store

import (
	"context"
	"encoding/json"
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
