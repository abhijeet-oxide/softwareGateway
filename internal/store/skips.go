package store

import (
	"context"
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
