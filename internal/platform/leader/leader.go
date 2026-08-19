// Package leader implements single-leader election among Coordinator replicas.
//
// See docs/design/04-queue-and-scheduling.md section 9.
//
// Election uses pg_try_advisory_lock rather than the Kubernetes Lease API: we
// already hold a mandatory, highly-available Postgres connection, so the lock
// needs no client-go, no RBAC, and works identically in local development.
// It also fails in the right direction - if Postgres is unreachable the leader
// could not act anyway, so tying leadership to the database avoids a split
// state where a leader exists but cannot do anything.
//
// The lock is SESSION-scoped: it is held for the connection's lifetime and
// released automatically when the connection drops or the process dies. There
// is no lease renewal to get wrong.
package leader

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"
)

// Elector acquires and holds leadership.
type Elector struct {
	db       *sql.DB
	lockID   int64
	retry    time.Duration
	log      *slog.Logger
	onChange func(bool)

	isLeader atomic.Bool
	conn     *sql.Conn
}

// Options configure an Elector.
type Options struct {
	LockID        int64
	RetryInterval time.Duration
	Logger        *slog.Logger
	// OnChange fires on every leadership transition, with the new state.
	// Used to start and stop the background loops and to set the
	// leader_elected gauge.
	OnChange func(isLeader bool)
}

// New builds an Elector. It does not acquire anything until Run is called.
func New(db *sql.DB, opts Options) *Elector {
	if opts.RetryInterval <= 0 {
		opts.RetryInterval = 10 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.OnChange == nil {
		opts.OnChange = func(bool) {}
	}
	return &Elector{
		db:       db,
		lockID:   opts.LockID,
		retry:    opts.RetryInterval,
		log:      opts.Logger,
		onChange: opts.OnChange,
	}
}

// IsLeader reports current leadership. Safe for concurrent use.
func (e *Elector) IsLeader() bool { return e.isLeader.Load() }

// Run contends for leadership until the context is cancelled.
//
// Failover safety does not depend on this being fast: every background loop
// guarded by leadership is idempotent, so a brief period with two leaders
// duplicates work without corrupting anything. A double reaper run requeues
// already-requeued jobs; a double discovery pass hits ON CONFLICT DO NOTHING.
func (e *Elector) Run(ctx context.Context) error {
	defer e.release()

	ticker := time.NewTicker(e.retry)
	defer ticker.Stop()

	e.tryAcquire(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if e.IsLeader() {
				e.verifyStillHeld(ctx)
			} else {
				e.tryAcquire(ctx)
			}
		}
	}
}

func (e *Elector) tryAcquire(ctx context.Context) {
	// A dedicated connection is required: an advisory lock belongs to the
	// session that took it, and a pooled connection could be handed to another
	// caller - or returned and reused - silently dropping our leadership.
	conn, err := e.db.Conn(ctx)
	if err != nil {
		e.log.Warn("leader: cannot open connection", "error", err)
		return
	}

	var acquired bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", e.lockID).Scan(&acquired); err != nil {
		_ = conn.Close()
		if !errors.Is(err, context.Canceled) {
			e.log.Warn("leader: acquire failed", "error", err)
		}
		return
	}

	if !acquired {
		_ = conn.Close()
		return
	}

	e.conn = conn
	e.isLeader.Store(true)
	e.log.Info("leader: acquired leadership", "lock_id", e.lockID)
	e.onChange(true)
}

// verifyStillHeld detects a dropped connection. If the session died, Postgres
// released the lock and another replica may already hold it, so we must stop
// acting as leader immediately rather than at the next acquire attempt.
func (e *Elector) verifyStillHeld(ctx context.Context) {
	if e.conn == nil {
		e.demote()
		return
	}
	if err := e.conn.PingContext(ctx); err != nil {
		e.log.Warn("leader: lost connection, demoting", "error", err)
		_ = e.conn.Close()
		e.conn = nil
		e.demote()
	}
}

func (e *Elector) demote() {
	if e.isLeader.CompareAndSwap(true, false) {
		e.log.Info("leader: lost leadership")
		e.onChange(false)
	}
}

func (e *Elector) release() {
	if e.conn == nil {
		e.demote()
		return
	}
	// Best effort: closing the connection releases the lock anyway.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := e.conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", e.lockID); err != nil {
		e.log.Debug("leader: explicit unlock failed; connection close will release", "error", err)
	}
	_ = e.conn.Close()
	e.conn = nil
	e.demote()
}

// AlwaysLeader is the SQLite implementation: a single-process development
// store has no other replica to contend with, so leadership is unconditional.
// It satisfies the same interface so no caller needs a driver check.
type AlwaysLeader struct{ onChange func(bool) }

// NewAlwaysLeader returns an elector that is immediately and permanently leader.
func NewAlwaysLeader(onChange func(bool)) *AlwaysLeader {
	if onChange == nil {
		onChange = func(bool) {}
	}
	return &AlwaysLeader{onChange: onChange}
}

func (a *AlwaysLeader) IsLeader() bool { return true }

func (a *AlwaysLeader) Run(ctx context.Context) error {
	a.onChange(true)
	<-ctx.Done()
	a.onChange(false)
	return nil
}

// Interface is satisfied by both implementations.
type Interface interface {
	IsLeader() bool
	Run(context.Context) error
}

var (
	_ Interface = (*Elector)(nil)
	_ Interface = (*AlwaysLeader)(nil)
)
