package store

import (
	"context"
	"fmt"
	"time"
)

// Who is walking a release, and how the last attempt ended.
//
// # Three states, because a timestamp has two
//
// `expanded_at` says whether a release has been walked. That is the right fact
// and it is not enough: a release nobody has touched, a release being walked
// right now, and a release whose walk failed all read as "not analysed", and
// they are three different things to say and three different things to do.
//
// # The claim is a row, because the claimant can die
//
// A process that claims a release and then crashes must not leave that release
// claimed forever. So the claim carries the moment it was taken, and anything
// still held long after a walk could plausibly still be running is released -
// which is a query, run at startup, rather than a promise nobody can keep.

// Analysis states. Empty means nobody is walking it, which is also the state a
// finished walk returns to: `expanded_at` is what records success.
const (
	// AnalysisRunning means a process claimed this release and is walking it.
	AnalysisRunning = "analyzing"
	// AnalysisFailed means the last attempt did not finish. The reason is on
	// the row, because "it failed" that cannot say why is a dead end.
	AnalysisFailed = "failed"
)

// ClaimAnalysis takes the right to walk one release, if nobody else holds it.
//
// Reports whether the claim succeeded. False is an ordinary answer - somebody
// else got there first - and the caller moves on rather than treating it as an
// error.
//
// The atomicity is the WHERE clause: two processes issuing this at the same
// instant produce one update of one row and one update of none, because the
// second reads a state the first has already changed. No advisory lock, no
// coordination, and correct across replicas.
func (p *Packages) ClaimAnalysis(ctx context.Context, packageID int64) (bool, error) {
	res, err := p.db.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE packages
		   SET analysis_state = ?, analysis_started_at = `+p.dialect.Now()+`,
		       analysis_error = NULL, updated_at = `+p.dialect.Now()+`
		 WHERE id = ? AND analysis_state <> ?`),
		AnalysisRunning, packageID, AnalysisRunning)
	if err != nil {
		return false, fmt.Errorf("claim analysis of package %d: %w", packageID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim analysis of package %d: %w", packageID, err)
	}
	return n > 0, nil
}

// FinishAnalysis records how a walk ended and releases the claim.
//
// A nil error clears the state entirely: `expanded_at`, written by the walk
// itself, is what says it succeeded, and a second flag saying the same thing is
// a second flag that can disagree.
//
// A failure keeps the reason. "Analysis failed" with no reason is a dead end for
// whoever reads it a week later, and the reasons here are the actionable kind -
// a component the vendor withdrew, a credential that expired, a registry that
// refused.
func (p *Packages) FinishAnalysis(ctx context.Context, packageID int64, cause error) error {
	state, reason := "", any(nil)
	if cause != nil {
		state, reason = AnalysisFailed, cause.Error()
	}

	if _, err := p.db.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE packages
		   SET analysis_state = ?, analysis_error = ?, updated_at = `+p.dialect.Now()+`
		 WHERE id = ?`), state, reason, packageID); err != nil {
		return fmt.Errorf("record the end of analysis of package %d: %w", packageID, err)
	}
	return nil
}

// RecoverAnalyses releases claims nobody can still be holding.
//
// # Why this is not optional
//
// A Coordinator restarted mid-walk leaves rows marked `analyzing` with nothing
// running. Without this they stay that way: the queue skips them because they
// look claimed, the interface shows a spinner that will never stop, and the
// release is never walked. That is the "stale status" a crash must not be able
// to leave behind.
//
// # Why by AGE and not by owner
//
// Recording which process holds a claim would let this be exact, and it would
// also be wrong: a Coordinator that is running perfectly well under a different
// identity - a rolled pod, a renamed host - would have its live claims stolen.
// Age is the honest test. A walk that has been going for longer than any walk
// takes is not going.
//
// # Released, not failed
//
// The claim goes back to unheld, so the queue picks the release up again and
// finishes the work. Marking it failed would be technically true and
// operationally useless: nothing went wrong with the release, a process went
// away, and the answer is to walk it - which is what the queue does with an
// unheld row and will not do with a failed one.
//
// Returns how many were released, for the startup line that says what the
// restart had to clean up.
func (p *Packages) RecoverAnalyses(ctx context.Context, staleAfter time.Duration) (int, error) {
	if staleAfter <= 0 {
		staleAfter = time.Hour
	}

	res, err := p.db.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE packages
		   SET analysis_state = '', analysis_error = NULL, updated_at = `+p.dialect.Now()+`
		 WHERE analysis_state = ?
		   AND COALESCE(analysis_started_at, discovered_at) < `+p.dialect.TimeAgo("?")),
		AnalysisRunning, staleAfter.Seconds())
	if err != nil {
		return 0, fmt.Errorf("recover abandoned analyses: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("recover abandoned analyses: %w", err)
	}
	return int(n), nil
}
