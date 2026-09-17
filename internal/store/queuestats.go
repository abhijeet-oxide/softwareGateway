package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// What the queue looks like right now.
//
// # Why this is a snapshot and not a set of counters
//
// Everything here is a QUESTION ABOUT THE PRESENT - how much work is
// outstanding, how long the oldest piece has been waiting, how many workers
// are holding leases. The database already knows all of it, authoritatively,
// and it survives a Coordinator restart. A counter incremented in Go would
// have to be rebuilt after every deployment and would drift from the queue the
// first time a write took a path nobody remembered to instrument.
//
// The complement is jobs_completed_total, which IS a counter, because "how
// many jobs failed today" is a question about the past and the database only
// keeps the answer until the rows are archived.
//
// # Why the Coordinator samples it on a timer
//
// Rather than reading it inside the Prometheus collector, where it would run
// once per scrape per scraping server and have no context to cancel with. A
// timer bounds the load on the database to one pass per interval no matter how
// many things are watching.
type QueueSnapshot struct {
	// Jobs outstanding by state: blocked, pending, leased. Settled states are
	// deliberately absent - see the type comment.
	Jobs map[string]int64
	// JobsPaused is the part of Jobs whose transfer is paused. Counted
	// separately because a deep queue that is paused is somebody's decision
	// and a deep queue that is not is an incident.
	JobsPaused int64
	// OldestPendingSeconds is how long the oldest unstarted job has been
	// waiting. Zero when nothing is pending.
	//
	// THE SINGLE MOST USEFUL NUMBER HERE. Depth alone cannot distinguish a
	// queue that is deep because it is busy from one that is deep because it
	// is stuck; this can, because in a moving queue it stays flat however deep
	// the backlog gets, and in a stalled one it climbs in real time.
	OldestPendingSeconds float64
	// PendingBytes is the planned size of everything not yet started, and
	// InFlightBytes what leased jobs have moved so far. Together they are the
	// honest answer to "how much is left", which a job count is not: a queue
	// of nine manifests and one 23 GB blob is one job from done and hours from
	// finished.
	PendingBytes  int64
	InFlightBytes int64

	// Transfers outstanding by state - what a person sees on the transfers
	// page, aggregated.
	Transfers map[string]int64

	// Workers by state: active, draining, stale.
	Workers map[string]int64
	// Slots is fleet concurrency: how many jobs are in hand against how much
	// capacity has been granted and how much exists. Granted below Max is the
	// budget controller holding back; Active at Granted with a deep queue is
	// the fleet being the bottleneck.
	SlotsActive  int64
	SlotsGranted int64
	SlotsMax     int64

	// DatabaseBytes is what the database occupies on disk.
	DatabaseBytes int64
}

// The states a queue gauge asks about: everything that has not settled.
//
// THESE MUST MATCH THE PREDICATES OF jobs_live_state_idx AND
// transfers_live_state_idx, in db/migrations/*/00054_queue_state_index.sql. A
// state listed here but not in the index turns an index-only read into a table
// scan taken on a timer, which is the failure mode that migration exists to
// prevent - and nothing about the result would look wrong, so
// TestQueueGaugeStatesMatchTheirIndexes holds the two together.
//
// Note this is NOT store.liveTransferStates, which means "can still be given
// to a worker" and excludes `waiting`. A transfer waiting on a schedule has
// settled nothing and belongs on a depth gauge.
const (
	unsettledJobStates = `'blocked','pending','leased'`

	unsettledTransferStates = `'waiting','pending','planning','ready','running',` +
		`'paused','syncing','promoting','verifying','cancelling'`
)

// QueueSnapshot reads the whole picture in four queries.
//
// Four rather than one because they are four different tables and a join would
// multiply rows before aggregating them; four round trips on a timer is not
// something worth a CTE that has to be read twice to trust.
func (p *Packages) QueueSnapshot(ctx context.Context) (QueueSnapshot, error) {
	s := QueueSnapshot{
		Jobs:      map[string]int64{},
		Transfers: map[string]int64{},
		Workers:   map[string]int64{},
	}
	for _, state := range statesIn(unsettledJobStates) {
		s.Jobs[state] = 0
	}
	for _, state := range statesIn(unsettledTransferStates) {
		s.Transfers[state] = 0
	}

	if err := p.readJobDepth(ctx, &s); err != nil {
		return s, err
	}
	if err := p.readTransferDepth(ctx, &s); err != nil {
		return s, err
	}
	if err := p.readWorkers(ctx, &s); err != nil {
		return s, err
	}
	if err := p.readDatabaseBytes(ctx, &s); err != nil {
		return s, err
	}
	return s, nil
}

// readJobDepth is the one that has to stay cheap.
//
// Grouped by `paused` as well as state so the pause split comes out of the
// same pass, and ordered so the planner reads jobs_live_state_idx: every
// column in the WHERE clause is the index's own, and the rows it visits are
// bounded by the backlog rather than by the table.
func (p *Packages) readJobDepth(ctx context.Context, s *QueueSnapshot) error {
	q := fmt.Sprintf(`
		SELECT state,
		       paused,
		       COUNT(*),
		       COALESCE(SUM(size_bytes), 0),
		       COALESCE(SUM(bytes_transferred), 0),
		       COALESCE(%s, 0)
		  FROM jobs
		 WHERE state IN (%s)
		 GROUP BY state, paused`,
		p.dialect.SecondsBetween("MIN(created_at)", p.dialect.Now()),
		unsettledJobStates)

	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(q))
	if err != nil {
		return fmt.Errorf("queue depth: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			state              string
			paused             bool
			count, size, moved int64
			ageSeconds         float64
		)
		if err := rows.Scan(&state, &paused, &count, &size, &moved, &ageSeconds); err != nil {
			return fmt.Errorf("queue depth: %w", err)
		}
		s.Jobs[state] += count
		if paused {
			s.JobsPaused += count
		}
		switch state {
		case "pending", "blocked":
			s.PendingBytes += size
			if ageSeconds > s.OldestPendingSeconds {
				s.OldestPendingSeconds = ageSeconds
			}
		case "leased":
			s.InFlightBytes += moved
		}
	}
	return rows.Err()
}

func (p *Packages) readTransferDepth(ctx context.Context, s *QueueSnapshot) error {
	q := fmt.Sprintf(
		`SELECT state, COUNT(*) FROM transfers WHERE state IN (%s) GROUP BY state`,
		unsettledTransferStates)

	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(q))
	if err != nil {
		return fmt.Errorf("transfer depth: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			return fmt.Errorf("transfer depth: %w", err)
		}
		s.Transfers[state] = count
	}
	return rows.Err()
}

// readWorkers has no index and needs none: the table holds one row per worker
// process, which is tens on the largest deployment this is built for.
func (p *Packages) readWorkers(ctx context.Context, s *QueueSnapshot) error {
	const q = `
		SELECT state,
		       COUNT(*),
		       COALESCE(SUM(active_jobs), 0),
		       COALESCE(SUM(granted_concurrency), 0),
		       COALESCE(SUM(max_concurrency), 0)
		  FROM workers
		 GROUP BY state`

	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(q))
	if err != nil {
		return fmt.Errorf("workers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var count, active, granted, max int64
		if err := rows.Scan(&state, &count, &active, &granted, &max); err != nil {
			return fmt.Errorf("workers: %w", err)
		}
		s.Workers[state] = count
		// Only a worker that can take work contributes capacity. A stale one
		// still has rows saying it was granted eight slots, and counting those
		// would report a fleet that can absorb work it cannot reach.
		if state == "active" {
			s.SlotsActive += active
			s.SlotsGranted += granted
			s.SlotsMax += max
		}
	}
	return rows.Err()
}

// readDatabaseBytes answers "is the disk going to run out", which nothing else
// in this process can see.
//
// Dialect-specific because there is no portable spelling: Postgres has a
// function for it, SQLite multiplies two pragmas. Both are metadata reads
// rather than scans.
func (p *Packages) readDatabaseBytes(ctx context.Context, s *QueueSnapshot) error {
	q := `SELECT page_count * page_size FROM pragma_page_count(), pragma_page_size()`
	if p.dialect.Name() == DriverPostgres {
		q = `SELECT pg_database_size(current_database())`
	}
	var n sql.NullInt64
	if err := p.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return fmt.Errorf("database size: %w", err)
	}
	s.DatabaseBytes = n.Int64
	return nil
}

// statesIn unpacks a SQL literal list into the bare state names, so the list
// is written once and the map keys cannot drift from the query.
func statesIn(literal string) []string {
	var out []string
	for _, part := range strings.Split(literal, ",") {
		out = append(out, strings.Trim(strings.TrimSpace(part), "'"))
	}
	return out
}
