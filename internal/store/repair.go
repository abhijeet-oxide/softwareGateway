package store

import (
	"context"
	"database/sql"
	"fmt"
)

// The backstop the placement cache was always resting on.
//
// # What went wrong, in the order it happened
//
// A placement record is strong evidence and not proof, and the blob fast path
// says so: it skips an upload on a placement, and failing that on a HEAD
// against the destination. Both are optimistic, and docs/design/11 §2.5 names
// the manifest push as the backstop that catches the cases where the optimism
// was wrong.
//
// The backstop was specified and never built. The engine produced the
// `blob_unknown` class and nothing anywhere consumed it, so a manifest rejected
// for a missing blob simply retried against a destination that went on claiming
// to have the blob — eight times, and then permanently.
//
// It surfaced against Artifactory, which answers `HEAD /v2/<path>/blobs/<digest>`
// from a checksum index spanning the whole Artifactory repository rather than
// the image path the request named. A blob present anywhere under the target
// therefore reads as present under every path in it. The upload is skipped, a
// placement is recorded, and the truth arrives at manifest push:
//
//	HTTP 400: manifest invalid: map[description:Failed to copy blob sha256:… to
//	orbs/cfx-5000-…/sha256:…/sha256__dc82908b11cf…]
//
// Every manifest of a 63.7 GiB bundle failed that way while every blob job
// reported success.
//
// # What repair has to do, and why each part is necessary
//
//  1. DELETE the placements. They are the record that caused the skip, and
//     leaving them means the next attempt skips again.
//  2. Requeue the blob jobs with force_upload. Deleting the placements is not
//     enough on its own: the HEAD fast path asks the destination directly and
//     gets the same wrong answer. A repaired job must take no fast path at all.
//  3. Put the manifest back to `blocked`. Its content is not present, so it is
//     not runnable — and with dependency edges (docs/design/04 §3.5) it becomes
//     runnable again automatically, the moment the last repaired blob lands.
//
// The cost is one full upload of the blobs of ONE manifest. The alternative,
// which is what happened, is a transfer that moves 63.7 GiB and delivers
// nothing.

// RepairResult is what one repair did.
type RepairResult struct {
	// Placements is how many placement records were withdrawn.
	Placements int
	// Blobs is how many blob jobs were requeued for a forced upload.
	Blobs int
}

// RepairMissingBlobs withdraws what the destination has denied having, and
// requeues it.
//
// Called with the MANIFEST job whose push was rejected. The blobs are resolved
// through the artifact that manifest describes, so this repairs exactly the
// content the rejected push referenced rather than guessing at a blast radius.
func (p *Packages) RepairMissingBlobs(
	ctx context.Context, tx *sql.Tx, jobID int64,
) (RepairResult, error) {
	var res RepairResult

	var (
		transferID   string
		targetRepoID int64
		artifactID   sql.NullInt64
	)
	err := tx.QueryRowContext(ctx, p.dialect.Rewrite(`
		SELECT transfer_id, target_repo_id, artifact_id FROM jobs WHERE id = ?`), jobID).
		Scan(&transferID, &targetRepoID, &artifactID)
	if err != nil {
		return res, fmt.Errorf("read manifest job %d for repair: %w", jobID, err)
	}
	if !artifactID.Valid {
		// A manifest job with no artifact cannot name its blobs. Nothing to
		// repair, and inventing a blast radius would be worse than declining.
		return res, nil
	}

	// The blobs this manifest references, at THIS destination. A component
	// published in two repositories has two blob jobs per digest, and only the
	// one the rejected push was against is suspect.
	const blobsOfArtifact = `SELECT digest FROM artifact_blobs WHERE artifact_id = ?`

	deleted, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
		DELETE FROM blob_placements
		 WHERE repository_id = ?
		   AND digest IN (`+blobsOfArtifact+`)`), targetRepoID, artifactID.Int64)
	if err != nil {
		return res, fmt.Errorf("withdraw placements for job %d: %w", jobID, err)
	}
	if n, err := deleted.RowsAffected(); err == nil {
		res.Placements = int(n)
	}

	// force_upload, and the attempt budget reset: this is a first attempt at a
	// genuinely different operation, not a ninth attempt at the same one.
	requeued, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE jobs
		   SET state            = 'pending',
		       force_upload     = `+p.dialect.Bool(true)+`,
		       attempts         = 0,
		       lease_owner      = NULL,
		       lease_expires_at = NULL,
		       next_visible_at  = `+p.dialect.Now()+`,
		       completed_at     = NULL,
		       last_error       = NULL,
		       last_error_class = NULL,
		       updated_at       = `+p.dialect.Now()+`
		 WHERE transfer_id = ? AND kind = 'blob' AND target_repo_id = ?
		   AND state IN ('succeeded','skipped','failed','pending')
		   AND digest IN (`+blobsOfArtifact+`)`),
		transferID, targetRepoID, artifactID.Int64)
	if err != nil {
		return res, fmt.Errorf("requeue the blobs of job %d: %w", jobID, err)
	}
	if n, err := requeued.RowsAffected(); err == nil {
		res.Blobs = int(n)
	}

	if res.Blobs == 0 {
		// Nothing to put back. Leaving the manifest blocked here would be a
		// deadlock: there is no completion coming to promote it.
		return res, nil
	}

	// Back to waiting. Its content is not at the destination, so it is not
	// runnable, and the dependency edges promote it again on their own once the
	// repaired blobs land.
	//
	// The backoff goes with the attempts. A retry backoff exists to space out
	// attempts at an operation that has not changed; this one HAS changed —
	// the content it was missing is being uploaded — so making it wait out a
	// delay computed from the failure it no longer has adds latency and
	// nothing else.
	if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE jobs
		   SET state            = 'blocked',
		       attempts         = 0,
		       lease_owner      = NULL,
		       lease_expires_at = NULL,
		       next_visible_at  = `+p.dialect.Now()+`,
		       completed_at     = NULL,
		       updated_at       = `+p.dialect.Now()+`
		 WHERE id = ?`), jobID); err != nil {
		return res, fmt.Errorf("reblock manifest job %d: %w", jobID, err)
	}
	return res, nil
}

// ClearForcedUpload drops the flag once the blob has actually been uploaded.
//
// The flag exists to get one job past a destination that lied about holding
// content. Leaving it set would make every future retry of that job re-upload
// the blob for no reason, which is the opposite of what the fast paths are for.
func (p *Packages) ClearForcedUpload(ctx context.Context, tx *sql.Tx, jobID int64) error {
	_, err := tx.ExecContext(ctx, p.dialect.Rewrite(
		`UPDATE jobs SET force_upload = `+p.dialect.Bool(false)+` WHERE id = ?`), jobID)
	if err != nil {
		return fmt.Errorf("clear the forced upload on job %d: %w", jobID, err)
	}
	return nil
}
