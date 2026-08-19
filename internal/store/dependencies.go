package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Per-artifact readiness: a manifest waits for its OWN content.
//
// # What this replaces, and what it deliberately does not
//
// Wave gating is a global barrier - wave N+1 opens only once wave N has drained
// entirely. Invariant I1 is not that. I1 is a statement about ONE manifest: it
// may be pushed once every blob and child manifest IT references is present.
// The barrier implies I1 and is much stronger than it, and the difference is
// measured in artifacts that sit finished-but-unwritable for hours.
//
// The edges here state I1 exactly. A manifest job is created `blocked` when it
// has dependencies at all, and is promoted to `pending` the moment the last of
// them reaches a terminal successful state - which happens inside the same
// transaction as the completion that made it true, so there is no window in
// which a runnable job is invisible.
//
// The wave machinery STAYS, unchanged, for three reasons that are each on their
// own sufficient:
//
//  1. It is the backstop. Dependency edges are written by the planner; a
//     transfer planned before this existed has none, and advanceWave is what
//     still moves it.
//  2. It drives transfer COMPLETION. `settleTransfer` decides a transfer is
//     done by draining the top wave, and that logic is untouched by promotion
//     happening earlier - a job promoted ahead of its wave is simply already
//     `succeeded` when its wave opens.
//  3. Every dependency of a manifest lives in a strictly LOWER wave (a child
//     has greater depth, and wave is maxDepth-depth+1; blobs are wave 0). So
//     advanceWave can only ever promote jobs whose dependencies have already
//     drained, and the two mechanisms cannot disagree.
//
// What changes is only that a job no longer has to WAIT for its wave.

// JobEdge is one dependency: JobID may not run until DependsOnID has succeeded.
type JobEdge struct {
	JobID       int64
	DependsOnID int64
}

// AddJobDependencies records the edges of a plan.
//
// Batched, because a bundle of two thousand artifacts has more edges than
// artifacts and a round trip each would make planning the slow half of a
// transfer. ON CONFLICT DO NOTHING for the same reason InsertJob has it: a
// replan re-derives an identical edge set and must find it free.
func (p *Packages) AddJobDependencies(ctx context.Context, tx *sql.Tx, edges []JobEdge) error {
	const perBatch = 500

	for start := 0; start < len(edges); start += perBatch {
		end := min(start+perBatch, len(edges))

		var values strings.Builder
		args := make([]any, 0, 2*(end-start))
		for _, e := range edges[start:end] {
			if e.JobID == 0 || e.DependsOnID == 0 || e.JobID == e.DependsOnID {
				// A zero ID means a dependency the planner did not create - a
				// blob already at the destination, say. That is not an edge
				// that failed to be written, it is a dependency that does not
				// exist, and inserting it would block the job forever.
				continue
			}
			if values.Len() > 0 {
				values.WriteByte(',')
			}
			values.WriteString("(?,?)")
			args = append(args, e.JobID, e.DependsOnID)
		}
		if len(args) == 0 {
			continue
		}

		if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(
			`INSERT INTO job_dependencies (job_id, depends_on_id) VALUES `+
				values.String()+` ON CONFLICT DO NOTHING`), args...); err != nil {
			return fmt.Errorf("record job dependencies: %w", err)
		}
	}
	return nil
}

// PromoteDependents makes runnable whatever was waiting on a finished job.
//
// Called from the completion path with the job that just succeeded or was
// skipped. The two conditions are separate on purpose:
//
//	EXISTS     … depends_on_id = ?    only jobs that were waiting on THIS one
//	NOT EXISTS … outstanding          and only if nothing else is outstanding
//
// The first bounds the work to the handful of manifests naming this digest.
// The second is the invariant. Dropping either one breaks something: without
// the first this becomes a full scan on every completion, and without the
// second a manifest with ten blobs would be pushed when the first one landed.
//
// Returns how many jobs became runnable, which the caller logs - a transfer
// where nothing is ever promoted has a dependency graph that is wrong, and that
// is otherwise invisible.
func (p *Packages) PromoteDependents(ctx context.Context, tx *sql.Tx, jobID int64) (int, error) {
	res, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE jobs
		   SET state = 'pending', updated_at = `+p.dialect.Now()+`
		 WHERE jobs.state = 'blocked'
		   AND EXISTS (SELECT 1 FROM job_dependencies d
		                WHERE d.job_id = jobs.id AND d.depends_on_id = ?)
		   AND NOT EXISTS (SELECT 1 FROM job_dependencies o
		                     JOIN jobs dep ON dep.id = o.depends_on_id
		                    WHERE o.job_id = jobs.id
		                      AND dep.state NOT IN ('succeeded','skipped'))`), jobID)
	if err != nil {
		return 0, fmt.Errorf("promote the dependents of job %d: %w", jobID, err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count the dependents promoted by job %d: %w", jobID, err)
	}
	return int(n), nil
}

// PromoteReadyJobs promotes every blocked job of a transfer whose dependencies
// are all satisfied.
//
// The sweep form, for the paths that are not a single completion: a retry that
// requeues a failed blob under manifests already marked blocked, and a replan
// that adopted jobs it did not create.
//
// It considers only jobs that HAVE edges. That restriction is what makes it
// safe to run against any transfer: one planned before dependencies existed has
// none, its blocked manifests are gated by wave alone, and promoting them here
// on the grounds that they have no unmet dependency would push every manifest
// in the transfer at once.
func (p *Packages) PromoteReadyJobs(ctx context.Context, tx *sql.Tx, transferID string) (int, error) {
	res, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE jobs
		   SET state = 'pending', updated_at = `+p.dialect.Now()+`
		 WHERE jobs.transfer_id = ?
		   AND jobs.state = 'blocked'
		   AND EXISTS (SELECT 1 FROM job_dependencies d WHERE d.job_id = jobs.id)
		   AND NOT EXISTS (SELECT 1 FROM job_dependencies o
		                     JOIN jobs dep ON dep.id = o.depends_on_id
		                    WHERE o.job_id = jobs.id
		                      AND dep.state NOT IN ('succeeded','skipped'))`), transferID)
	if err != nil {
		return 0, fmt.Errorf("promote the ready jobs of transfer %s: %w", transferID, err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count the ready jobs of transfer %s: %w", transferID, err)
	}
	return int(n), nil
}

// maxCascadeDepth bounds the unreachability sweep.
//
// One iteration per level of the manifest tree is enough to reach a fixpoint,
// and real bundles are three or four deep. The bound is a guard against a cycle
// the planner should never create, not a tuning knob.
const maxCascadeDepth = 32

// FailUnreachableJobs marks blocked jobs that can never run.
//
// # Why this has to exist at all
//
// Per-artifact readiness makes the good case better and the bad case WORSE, and
// the second half is easy to miss. Under wave gating, one permanently failed
// blob left every manifest blocked, and SettleStalledTransfers reported the
// transfer as stopped only when nothing anywhere was runnable - which was true,
// because everything was blocked behind the one wave.
//
// With edges, the manifests that do not depend on the broken blob are promoted,
// run, and succeed. What is left is a handful of blocked jobs waiting on a
// dependency that is terminally `failed`, and those count as "runnable" to the
// stall check. The transfer would sit at `running` with nothing in flight and
// nothing that could ever start - exactly the symptom the settle work was
// written to remove, in a narrower and more confusing form.
//
// So a failure has to propagate: a blocked job whose dependency has failed has
// failed too, and says so. Iterated to a fixpoint because failure is transitive
// - an index waiting on a child manifest waiting on a blob is two levels deep.
//
// The reason recorded is deliberately distinguishable from a real one. Nobody
// should have to guess which of forty failures was the cause and which were
// consequences.
func (p *Packages) FailUnreachableJobs(
	ctx context.Context, tx *sql.Tx, transferID string,
) (int, error) {
	total := 0

	for range maxCascadeDepth {
		res, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
			UPDATE jobs
			   SET state            = 'failed',
			       last_error       = 'a dependency of this job failed permanently',
			       last_error_class = 'dependency',
			       completed_at     = `+p.dialect.Now()+`,
			       updated_at       = `+p.dialect.Now()+`
			 WHERE jobs.transfer_id = ?
			   AND jobs.state = 'blocked'
			   AND EXISTS (SELECT 1 FROM job_dependencies o
			                 JOIN jobs dep ON dep.id = o.depends_on_id
			                WHERE o.job_id = jobs.id
			                  AND dep.state = 'failed')`), transferID)
		if err != nil {
			return total, fmt.Errorf("fail the unreachable jobs of transfer %s: %w", transferID, err)
		}

		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("count the unreachable jobs of transfer %s: %w", transferID, err)
		}
		if n == 0 {
			return total, nil
		}
		total += int(n)
	}
	return total, nil
}

// ClearDependencyFailures returns cascade-failed jobs to `blocked`.
//
// A retry requeues the job that actually broke. The jobs that failed only
// BECAUSE of it must go back to waiting rather than to running: their
// dependency is pending again, not satisfied, and promoting them would push a
// manifest whose blob is still in flight.
//
// Keyed on the error class rather than on the edge set, because that is the
// question being asked - "which of these failures were consequences" - and the
// edges are the same whether a job failed on its own or by cascade.
func (p *Packages) ClearDependencyFailures(
	ctx context.Context, tx *sql.Tx, transferID string,
) (int, error) {
	res, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE jobs
		   SET state            = 'blocked',
		       attempts         = 0,
		       last_error       = NULL,
		       last_error_class = NULL,
		       completed_at     = NULL,
		       updated_at       = `+p.dialect.Now()+`
		 WHERE transfer_id = ? AND state = 'failed' AND last_error_class = 'dependency'`),
		transferID)
	if err != nil {
		return 0, fmt.Errorf("clear the dependency failures of transfer %s: %w", transferID, err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count the cleared failures of transfer %s: %w", transferID, err)
	}
	return int(n), nil
}
