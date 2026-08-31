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
// the repository asked about, `exists_at_target` is not evidence at all - and
// that is invisible in a page reporting "62.2 GiB of 63.7 GiB transferred",
// because the 1.5 GiB difference has no line of its own.
//
// It should have had one from the start: docs/design/12 §2 names
// `sum(size_bytes) grouped by skip_reason` as the bandwidth-saved metric, and
// docs/design/10 §4 says skipped is a first-class success rather than an
// exception precisely so that it stays measurable. It was measurable in the
// jobs table and reported nowhere, which is the same as not being measured.
//
// The number answers the question an operator actually asks after a repair -
// "why is it re-sending things that already succeeded?" - because the blobs
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
	// Name is the vendor's own name for it - `cfx-5000-product/bgcf:2511.174.0`
	// - or empty for a component the release names only by digest.
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
	// Bytes is what of this component the destination already holds, and Blobs
	// how many pieces of content that is.
	//
	// Counted per BLOB and each blob attributed to one component, so a base
	// layer shared by forty images is counted once and the parts add up to the
	// whole saving.
	Bytes int64
	Blobs int
	// Outstanding is how much of it is still to move. Zero means the whole
	// component was already there; more than zero means part of it was, which
	// is an ordinary and different thing to say.
	Outstanding int
}

// PresentComponents names what the destination already holds of a release.
//
// # Why a list and not a number
//
// "Saved 56.5 GB" is the system's best claim about itself and it is unverifiable
// as stated. The question an operator asks next is which things were already
// there - and that is answerable exactly, because every skipped job is attached
// to the artifact it belongs to.
//
// Named where the vendor names it. A component the release lists only by digest
// keeps its digest, which is still a better answer than a byte count.
//
// Ordered by what each SAVED, because the reader is looking at a saving and the
// biggest contributors are the explanation.
func (p *Packages) PresentComponents(ctx context.Context, transferID string) ([]PresentComponent, error) {
	// # Why this reads PLACEMENTS and not only jobs
	//
	// Most of a saving leaves no job behind. Planning asks the destination what
	// it already holds, and content it holds gets NO JOB AT ALL - that is the
	// whole point of the check, and those bytes land in `dedupe_skipped_bytes`.
	// Only content that got a job and was then skipped by a worker has a row.
	//
	// Reading jobs alone therefore reported "nothing has been found at the
	// destination yet" beside a headline of `Saved 63.7 GB`, because the great
	// majority of that 63.7 GB was decided before a single job existed.
	//
	// So content is taken from the RELEASE, and counted as already there when
	// either the transfer skipped it or the placement record says the target
	// holds it. Each blob is attributed to ONE artifact - the lowest id that
	// references it - so a base layer shared by forty images is counted once
	// and the parts still add up to the whole.
	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(`
		WITH tr AS (
			SELECT package_id, target_repo_id FROM transfers WHERE id = ?
		),
		-- EVERY repository this transfer writes to. A bundle's components each
		-- land in their own destination path with a catalog row of their own,
		-- so the transfer's own target_repo_id is the FALLBACK - the root - and
		-- almost never where the content actually goes. Asking for placements
		-- at that row alone matches nothing on a real bundle.
		targets AS (
			SELECT target_repo_id AS repository_id FROM jobs WHERE transfer_id = ?
			UNION
			SELECT target_repo_id FROM tr
		),
		owned AS (
			SELECT ab.digest AS digest, MIN(ab.artifact_id) AS artifact_id
			  FROM package_artifacts pa
			  JOIN artifact_blobs ab ON ab.artifact_id = pa.id
			 WHERE pa.package_id = (SELECT package_id FROM tr)
			 GROUP BY ab.digest
		),
		here AS (
			SELECT o.artifact_id,
			       COALESCE(b.size_bytes, 0) AS size_bytes,
			       CASE WHEN EXISTS (
			              SELECT 1 FROM jobs j
			               WHERE j.transfer_id = ? AND j.digest = o.digest
			                 AND j.state = 'skipped')
			            -- NO JOB AT ALL, and the destination has it: the
			            -- planner found it already there and never queued it.
			            -- A job that exists only to be skipped still costs a
			            -- lease and a round trip, so those blobs are dropped at
			            -- plan time and this is the only trace of them.
			            --
			            -- The "no job" half keeps it honest: a digest this
			            -- transfer is actually pushing has a job, and that
			            -- job's state decides, so a blob still outstanding for
			            -- one repository is never called present because
			            -- another repository holds it.
			            OR (NOT EXISTS (
			                  SELECT 1 FROM jobs j
			                   WHERE j.transfer_id = ? AND j.digest = o.digest)
			                AND EXISTS (
			                  SELECT 1 FROM blob_placements bp
			                   WHERE bp.digest = o.digest
			                     AND bp.repository_id IN
			                         (SELECT repository_id FROM targets)))
			       THEN 1 ELSE 0 END AS present,
			       CASE WHEN EXISTS (
			              SELECT 1 FROM jobs j
			               WHERE j.transfer_id = ? AND j.digest = o.digest
			                 AND j.state IN ('pending','blocked','leased'))
			       THEN 1 ELSE 0 END AS outstanding
			  FROM owned o
			  LEFT JOIN blobs b ON b.digest = o.digest
		)
		SELECT pa.digest,
		       pa.media_type,
		       COALESCE(pa.artifact_type, ''),
		       COALESCE(pa.annotations, ''),
		       COALESCE((SELECT b.media_type
		                   FROM artifact_blobs ab
		                   JOIN blobs b ON b.digest = ab.digest
		                  WHERE ab.artifact_id = pa.id AND ab.kind = 'config'
		                  LIMIT 1), ''),
		       COALESCE(SUM(CASE WHEN h.present = 1 THEN h.size_bytes ELSE 0 END), 0),
		       COALESCE(SUM(h.present), 0),
		       COALESCE(SUM(h.outstanding), 0)
		  FROM here h
		  JOIN package_artifacts pa ON pa.id = h.artifact_id
		 GROUP BY pa.id, pa.digest, pa.media_type, pa.artifact_type, pa.annotations
		HAVING SUM(h.present) > 0
		 ORDER BY SUM(CASE WHEN h.present = 1 THEN h.size_bytes ELSE 0 END) DESC`),
		// tr, targets, then the three EXISTS clauses over jobs.
		transferID, transferID, transferID, transferID, transferID)
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
			&c.ConfigMediaType, &c.Bytes, &c.Blobs, &c.Outstanding); err != nil {
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

// ContentBytes is a transfer's byte account over DISTINCT content.
//
// # Why the per-repository figures are the wrong axis for a bar
//
// A component published under its own name as well as inside the bundle needs
// its layers in two repositories, so the planner counts them twice and is right
// to: two repositories is two pieces of bookkeeping. But the second copy costs
// NO BYTES - the registry mounts it, or the destination already had it - so a
// byte total counted per (repository, digest) says a 29.8 GB release is 63.7 GB
// of traffic, which never happens.
//
// Bytes are therefore counted per DIGEST. Each distinct piece of content is
// weighed once, and is either moved or found already there. That is the axis a
// person means by "how big is this download", and the one that reconciles with
// the release's own size.
type ContentBytes struct {
	// Total is what the release's distinct content weighs.
	Total int64
	// Moved is what has actually been streamed - including the part of a blob
	// a worker is streaming right now - and Present what the destination
	// already had by any route. Both over distinct digests, so Moved + Present
	// converges on Total and never exceeds it.
	Moved   int64
	Present int64
}

// TransferContentBytes weighs a transfer's content once per digest.
//
// The population is the RELEASE - every manifest and every blob it contains,
// each counted once - rather than the transfer's jobs, because a job exists per
// destination repository and the same content has several. What varies per
// digest is only how much of it has arrived.
//
// # Bytes in flight count, and count for what they are
//
// A digest with a worker on it used to count for its WHOLE size the moment the
// first byte landed, which made a bar jump a blob at a time and report content
// as moved that was still moving. It now counts for what the worker has
// actually reported - capped at the digest's own size, because a resumed job
// resumes its accounting and a retried one can report more than once.
//
// A finished job counts for the whole size whatever its last progress report
// said: the content IS there, and a report that never arrived (the last one is
// lossy by design - see ReportProgress) must not leave a completed digest
// weighing less than it does.
func (p *Packages) TransferContentBytes(ctx context.Context, transferID string) (ContentBytes, error) {
	var out ContentBytes
	err := p.db.QueryRowContext(ctx, p.dialect.Rewrite(`
		WITH tr AS (
			SELECT package_id, target_repo_id FROM transfers WHERE id = ?
		),
		-- EVERY repository this transfer writes to. A bundle's components each
		-- land in their own destination path with a catalog row of their own,
		-- so the transfer's own target_repo_id is the FALLBACK - the root - and
		-- almost never where the content actually goes. Asking for placements
		-- at that row alone matches nothing on a real bundle.
		targets AS (
			SELECT target_repo_id AS repository_id FROM jobs WHERE transfer_id = ?
			UNION
			SELECT target_repo_id FROM tr
		),
		owned AS (
			-- The blobs, once each however many components reference them.
			SELECT DISTINCT ab.digest AS digest, COALESCE(b.size_bytes, 0) AS size_bytes
			  FROM package_artifacts pa
			  JOIN artifact_blobs ab ON ab.artifact_id = pa.id
			  LEFT JOIN blobs b ON b.digest = ab.digest
			 WHERE pa.package_id = (SELECT package_id FROM tr)
			UNION
			-- And the manifests, which are content too and are what the
			-- transfer is still pushing once every byte has arrived.
			SELECT DISTINCT pa.digest, COALESCE(pa.size_bytes, 0)
			  FROM package_artifacts pa
			 WHERE pa.package_id = (SELECT package_id FROM tr)
		),
		state AS (
			SELECT o.size_bytes AS size_bytes,
			       COALESCE((SELECT MAX(j.bytes_transferred) FROM jobs j
			                  WHERE j.transfer_id = ? AND j.digest = o.digest), 0) AS moved,
			       CASE WHEN EXISTS (
			              SELECT 1 FROM jobs j
			               WHERE j.transfer_id = ? AND j.digest = o.digest
			                 AND j.state = 'succeeded')
			       THEN 1 ELSE 0 END AS finished,
			       CASE WHEN EXISTS (
			              SELECT 1 FROM jobs j
			               WHERE j.transfer_id = ? AND j.digest = o.digest
			                 AND j.state = 'skipped')
			            -- NO JOB AT ALL, and the destination has it: the
			            -- planner found it already there and never queued it.
			            -- A job that exists only to be skipped still costs a
			            -- lease and a round trip, so those blobs are dropped at
			            -- plan time and this is the only trace of them.
			            --
			            -- The "no job" half keeps it honest: a digest this
			            -- transfer is actually pushing has a job, and that
			            -- job's state decides, so a blob still outstanding for
			            -- one repository is never called present because
			            -- another repository holds it.
			            OR (NOT EXISTS (
			                  SELECT 1 FROM jobs j
			                   WHERE j.transfer_id = ? AND j.digest = o.digest)
			                AND EXISTS (
			                  SELECT 1 FROM blob_placements bp
			                   WHERE bp.digest = o.digest
			                     AND bp.repository_id IN
			                         (SELECT repository_id FROM targets)))
			       THEN 1 ELSE 0 END AS present
			  FROM owned o
		)
		SELECT COALESCE(SUM(size_bytes), 0),
		       -- Streamed wins over present: content this transfer actually
		       -- moved is moved, whatever a placement record says about it now.
		       -- Written as a CASE rather than MIN/LEAST because the two
		       -- dialects spell the two-argument minimum differently.
		       COALESCE(SUM(CASE WHEN finished = 1        THEN size_bytes
		                         WHEN moved > size_bytes  THEN size_bytes
		                         ELSE moved END), 0),
		       COALESCE(SUM(CASE WHEN finished = 0 AND moved = 0 AND present = 1
		                         THEN size_bytes ELSE 0 END), 0)
		  FROM state`),
		// tr, targets, then the four EXISTS clauses over jobs.
		transferID, transferID, transferID, transferID, transferID, transferID).
		Scan(&out.Total, &out.Moved, &out.Present)
	if err != nil {
		return ContentBytes{}, fmt.Errorf("weigh the content of transfer %s: %w", transferID, err)
	}
	return out, nil
}
