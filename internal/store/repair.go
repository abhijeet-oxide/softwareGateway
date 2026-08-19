package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
)

// Repair levels, in the order a repair escalates through them.
//
// The ladder they disable is ordered cheapest-first (docs/design/05 §4), and
// the lying answer sits ABOVE the mount rather than below it - which is why
// disabling the whole ladder at once is both correct and far more expensive
// than it needs to be. See migration 00013.
const (
	// RepairDistrustCache skips the placement record and the HEAD, and still
	// tries the cross-repository mount. Zero bytes when the registry relocates
	// the blob, a stream when it declines.
	RepairDistrustCache = 1
	// RepairStreamOnly takes no fast path at all.
	RepairStreamOnly = 2
	// MaxRepairLevel is where escalation stops.
	MaxRepairLevel = RepairStreamOnly
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
// to have the blob - eight times, and then permanently.
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
//     not runnable - and with dependency edges (docs/design/04 §3.5) it becomes
//     runnable again automatically, the moment the last repaired blob lands.
//
// The cost is one full upload of the blobs of ONE manifest. The alternative,
// which is what happened, is a transfer that moves 63.7 GiB and delivers
// nothing.
//
// # What bounds it
//
// A repair that does not help must not repeat. Two things stop it: the
// manifest keeps its attempt count across a repair, so the ordinary budget
// still runs out; and a blob escalates at most twice, because there is nothing
// stronger than streaming the bytes with every fast path disabled.
//
// Both are needed. Without the first, each repair would reset the budget and
// the loop would never end. Without the second, it would end only after eight
// full re-uploads of content the destination already has.

// RepairResult is what one repair did.
type RepairResult struct {
	// Placements is how many placement records were withdrawn.
	Placements int
	// Blobs is how many blob jobs were requeued for a forced upload.
	Blobs int
	// AlreadyRepaired reports that this manifest's blobs had ALREADY reached the
	// top repair level and the push was rejected anyway. Nothing was changed,
	// and the job is allowed to fail on its own attempt budget.
	//
	// Reported because it is the line between two things that look identical
	// from outside and need opposite responses: "the placement cache was
	// wrong", which repairs itself, and "the registry will not accept this
	// manifest", which needs a human. Before the repair existed they were
	// indistinguishable - both were simply a manifest that would not push.
	AlreadyRepaired bool
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

	// WRITE THE EDGES FIRST, because the repair is about to depend on them.
	//
	// Blocking a manifest is only safe if something can unblock it, and what
	// unblocks it is a dependency edge from it to the blob jobs it is waiting
	// for. A transfer planned before those edges existed has none - so blocking
	// its manifest is a BLACK HOLE: nothing promotes it, and wave advancement
	// cannot help either, since the wave it sits in can never drain while it is
	// blocked. That is a deadlock, and it happened.
	//
	// The repair has already computed exactly the set of edges it needs - the
	// manifest, and the blob jobs for the artifact it describes at this
	// destination - so it writes them rather than assuming somebody else did.
	// Idempotent, so a transfer that already has them pays nothing.
	if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
		INSERT INTO job_dependencies (job_id, depends_on_id)
		SELECT ?, j.id
		  FROM jobs j
		 WHERE j.transfer_id = ? AND j.kind = 'blob' AND j.target_repo_id = ?
		   AND j.digest IN (`+blobsOfArtifact+`)
		ON CONFLICT DO NOTHING`),
		jobID, transferID, targetRepoID, artifactID.Int64); err != nil {
		return res, fmt.Errorf("record the dependencies of job %d for repair: %w", jobID, err)
	}

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

	// ESCALATE, one level per repair, per blob.
	//
	// Level 1 distrusts the cache: skip the placement record and the HEAD, but
	// still try the mount. That is the right answer for a stale placement, and
	// it is nearly free - the copy of a component published under its own name
	// is the same digest already sitting in a sibling repository of the same
	// registry, so the registry relocates it internally rather than us sending
	// it across the WAN a second time.
	//
	// Level 2 distrusts everything and streams. Reached only when a level-1
	// repair did not fix the push, which means the mount reported success
	// without materialising the blob.
	//
	// Per BLOB rather than per manifest, because two manifests can share a
	// blob: one having already escalated X must not stop the other from
	// repairing its own Y. And capped at 2, which is what BOUNDS the whole
	// thing - see the note on RepairResult.AlreadyRepaired.
	//
	// The blob's attempt budget IS reset - for the blob this is a first attempt
	// at a genuinely different operation, not a ninth attempt at the same one.
	requeued, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE jobs
		   SET state            = 'pending',
		       repair_level     = repair_level + 1,
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
		   AND repair_level < `+strconv.Itoa(MaxRepairLevel)+`
		   AND digest IN (`+blobsOfArtifact+`)`),
		transferID, targetRepoID, artifactID.Int64)
	if err != nil {
		return res, fmt.Errorf("requeue the blobs of job %d: %w", jobID, err)
	}
	if n, err := requeued.RowsAffected(); err == nil {
		res.Blobs = int(n)
	}

	if res.Blobs == 0 {
		// Every blob this manifest names is already at the top level and the
		// push was rejected regardless. The cache is not the problem, so there
		// is nothing left to repair - say so, change nothing, and let the
		// manifest fail on its own attempt budget. Leaving it blocked here
		// would be a deadlock: no completion is coming to promote it.
		res.AlreadyRepaired = true
		return res, nil
	}

	// Back to waiting. Its content is not at the destination, so it is not
	// runnable, and the dependency edges promote it again on their own once the
	// repaired blobs land.
	//
	// The backoff is dropped, and the ATTEMPT COUNT IS NOT. Those look like the
	// same decision and are opposite ones. The backoff exists to space out
	// attempts at an operation that has not changed, and this one has - the
	// content it was missing is being uploaded - so waiting out a delay
	// computed from a failure it no longer has adds latency and nothing else.
	//
	// The attempt count is what BOUNDS the repair. Resetting it would make a
	// repair that never helps loop forever: push, repair, re-upload, push,
	// repair, with no exit and every appearance of progress. Keeping it means
	// a destination that rejects the manifest for some other reason spends the
	// same eight attempts everything else gets, and then stops.
	if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE jobs
		   SET state            = 'blocked',
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
