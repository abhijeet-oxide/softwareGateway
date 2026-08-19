package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// What a transfer did NOT move, and on whose word.
//
// # Why this needs its own reporting
//
// "1976 of 1976 blobs done" is four different claims wearing one word, and
// only one of them is evidence that bytes reached the destination:
//
//	succeeded          we streamed it. Proof.
//	skipped/mounted    the registry relocated it internally. Its own action.
//	skipped/placement  our RECORD said it was there. A cache.
//	skipped/exists     the registry's HEAD said it was there. Its claim.
//
// The last two moved nothing and are only as good as the answer they trusted.
// Against a destination that answers HEAD for its whole storage rather than
// the repository asked about, `exists_at_target` is not evidence at all — and
// that is invisible in a page reporting "62.2 GiB of 63.7 GiB transferred",
// because the 1.5 GiB difference has no line of its own.
//
// It should have had one from the start: docs/design/12 §2 names
// `sum(size_bytes) grouped by skip_reason` as the bandwidth-saved metric, and
// docs/design/10 §4 says skipped is a first-class success rather than an
// exception precisely so that it stays measurable. It was measurable in the
// jobs table and reported nowhere, which is the same as not being measured.
//
// The number answers the question an operator actually asks after a repair —
// "why is it re-sending things that already succeeded?" — because the blobs
// being repaired are exactly the ones counted here.

// SkipSummary is one reason a transfer moved no bytes, and how much it saved.
type SkipSummary struct {
	// Reason is the skip_reason vocabulary: placement_hit, exists_at_target,
	// mounted.
	Reason string
	Jobs   int
	// Bytes is what those jobs would have moved. For `mounted` it is a genuine
	// saving; for the other two it is a saving ONLY if the claim was true.
	Bytes int64
	// Trusted reports whether this reason rests on somebody's claim rather than
	// on an action. A mount is something the registry DID; a placement hit and
	// a HEAD are things it or we SAID.
	Trusted bool
}

// SkipBreakdown reports what a transfer did not move, by reason.
func (p *Packages) SkipBreakdown(ctx context.Context, transferID string) ([]SkipSummary, error) {
	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(`
		SELECT COALESCE(skip_reason, ''), count(*), COALESCE(SUM(size_bytes), 0)
		  FROM jobs
		 WHERE transfer_id = ? AND state = 'skipped'
		 GROUP BY skip_reason
		 ORDER BY SUM(size_bytes) DESC`), transferID)
	if err != nil {
		return nil, fmt.Errorf("read the skips of transfer %s: %w", transferID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []SkipSummary
	for rows.Next() {
		var s SkipSummary
		if err := rows.Scan(&s.Reason, &s.Jobs, &s.Bytes); err != nil {
			return nil, fmt.Errorf("scan a skip of transfer %s: %w", transferID, err)
		}
		s.Trusted = s.Reason == "mounted"
		out = append(out, s)
	}
	return out, rows.Err()
}

// PresentComponent is one component the destination already held.
type PresentComponent struct {
	// Name is the vendor's own name for it — `cfx-5000-product/bgcf:2511.174.0`
	// — or empty for a component the release names only by digest.
	Name   string
	Digest string
	// The four fields a classifier needs to say what this IS. Verbatim, and
	// classified by the caller: deciding that `application/vnd.cncf.helm…` is
	// what a person calls a chart is protocol knowledge, and it lives in one
	// place.
	MediaType       string
	ArtifactType    string
	ConfigMediaType string
	Annotations     map[string]string
	// Bytes is what this component's skipped jobs would have moved, and Jobs
	// how many there were.
	Bytes int64
	Jobs  int
	// Outstanding is how many of its jobs are still to run. Zero means the
	// whole component was already there; more than zero means part of it was,
	// which is an ordinary and different thing to say.
	Outstanding int
}

// PresentComponents names what a transfer did not have to move.
//
// # Why a list and not a number
//
// "Saved 56.5 GB" is the system's best claim about itself and it is unverifiable
// as stated. The question an operator asks next is which things were already
// there — and that is answerable exactly, because every skipped job is attached
// to the artifact it belongs to.
//
// Named where the vendor names it. A component the release lists only by digest
// keeps its digest, which is still a better answer than a byte count.
//
// Ordered by what each SAVED, because the reader is looking at a saving and the
// biggest contributors are the explanation.
func (p *Packages) PresentComponents(ctx context.Context, transferID string) ([]PresentComponent, error) {
	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(`
		SELECT pa.digest,
		       pa.media_type,
		       COALESCE(pa.artifact_type, ''),
		       COALESCE(pa.annotations, ''),
		       COALESCE((SELECT b.media_type
		                   FROM artifact_blobs ab
		                   JOIN blobs b ON b.digest = ab.digest
		                  WHERE ab.artifact_id = pa.id AND ab.kind = 'config'
		                  LIMIT 1), ''),
		       COALESCE(SUM(CASE WHEN j.state = 'skipped' THEN j.size_bytes ELSE 0 END), 0),
		       SUM(CASE WHEN j.state = 'skipped' THEN 1 ELSE 0 END),
		       SUM(CASE WHEN j.state IN ('pending','blocked','leased') THEN 1 ELSE 0 END)
		  FROM jobs j
		  JOIN package_artifacts pa ON pa.id = j.artifact_id
		 WHERE j.transfer_id = ?
		 GROUP BY pa.id, pa.digest, pa.media_type, pa.artifact_type, pa.annotations
		HAVING SUM(CASE WHEN j.state = 'skipped' THEN 1 ELSE 0 END) > 0
		 ORDER BY SUM(CASE WHEN j.state = 'skipped' THEN j.size_bytes ELSE 0 END) DESC`),
		transferID)
	if err != nil {
		return nil, fmt.Errorf("list what transfer %s did not move: %w", transferID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []PresentComponent
	for rows.Next() {
		var (
			c           PresentComponent
			annotations []byte
		)
		if err := rows.Scan(&c.Digest, &c.MediaType, &c.ArtifactType, &annotations,
			&c.ConfigMediaType, &c.Bytes, &c.Jobs, &c.Outstanding); err != nil {
			return nil, fmt.Errorf("scan a present component of transfer %s: %w", transferID, err)
		}
		if len(annotations) > 0 {
			var a map[string]string
			if err := json.Unmarshal(annotations, &a); err == nil {
				c.Annotations = a
				c.Name = a[annotationRefName]
			}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
