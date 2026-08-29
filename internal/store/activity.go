package store

import (
	"context"
	"fmt"
)

// What the whole estate is doing, as three numbers.
//
// # Why this exists rather than the shell counting a listing
//
// The application shell shows one line - "3 downloads running", "1 download
// failed" - on every page, and it computed it by asking for the hundred most
// recent transfers every few seconds and counting them in the browser.
//
// A transfer listing is not a cheap thing to ask for a hundred of. Each row
// carries a dozen aggregates over that transfer's jobs, so the cost is set by
// how much work the estate has done, not by how many numbers the caller wanted:
// measured at 158ms for a hundred rows over an estate of 150,000 jobs. On
// SQLite, where the pool is deliberately a single connection, that is 158ms in
// which no other request and no worker lease can touch the database - every few
// seconds, from every open tab, for the whole life of a download.
//
// This asks the database the question the shell actually has. It reads the
// transfers table and the leased jobs, and touches nothing else.
type ActivitySummary struct {
	// Moving is live transfers with at least one job in a worker's hands.
	Moving int
	// Held is live transfers with none - planned, queued, or waiting for a
	// fleet that is not there. The distinction the shell exists to draw: a
	// queue being drained and a queue nothing is draining look identical from
	// a count of "running".
	Held int
	// Failed is transfers that stopped and were not retried.
	Failed int
}

// Activity summarises every transfer in one query.
func (p *Packages) Activity(ctx context.Context) (ActivitySummary, error) {
	// EXISTS rather than a count of leased jobs: the question is whether ANY
	// job of this transfer is in a worker's hands, and a scan that stops at the
	// first is the whole of it.
	inFlight := `EXISTS (SELECT 1 FROM jobs j
	                      WHERE j.transfer_id = t.id AND j.state = 'leased')`

	query := p.dialect.Rewrite(`
		SELECT
		  COUNT(*) FILTER (WHERE t.state IN (` + liveTransferStates + `) AND ` + inFlight + `),
		  COUNT(*) FILTER (WHERE t.state IN (` + liveTransferStates + `) AND NOT ` + inFlight + `),
		  COUNT(*) FILTER (WHERE t.state = 'failed')
		  FROM transfers t`)

	var out ActivitySummary
	if err := p.db.QueryRowContext(ctx, query).
		Scan(&out.Moving, &out.Held, &out.Failed); err != nil {
		return ActivitySummary{}, fmt.Errorf("summarise transfer activity: %w", err)
	}
	return out, nil
}
