package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Persistence for NATIVE promotion: a hop a registry carries out itself.
//
// See db/migrations/*/00028_native_promotion.sql for why this is two tables.
// In one sentence: the promotion is what we asked for and the names are what
// it consists of, and keeping the names lets a promotion interrupted half way
// through resume at the exact one rather than re-issue every call it had
// already made.
//
// It sits beside store/replication.go rather than inside it because the two
// answer different questions about different things - a mirror's history is
// about a target, a promotion's is about one transfer - and folding them
// together would mean every reader of one filtered out the other.

// Promotions is the persistence surface for native promotion.
type Promotions struct {
	db      *sql.DB
	dialect Dialect
}

// NewPromotions builds the store.
func NewPromotions(s Store) *Promotions {
	return &Promotions{db: s.DB(), dialect: DialectFor(s.Driver())}
}

// PromotionName is one name a promotion publishes at the destination.
type PromotionName struct {
	// Repository is RELATIVE to each end's configured base path.
	Repository string
	Tag        string
	Digest     string
}

// PromotionNameState is one name plus how it went.
type PromotionNameState struct {
	PromotionName
	Position  int
	State     string
	LastError string
}

// Promotion is one transfer's native promotion.
type Promotion struct {
	ID         int64
	TransferID string
	Promoter   string
	State      string

	NamesTotal int
	NamesDone  int

	Attempts  int
	LastError string

	ClaimedBy   string
	RequestedAt sql.NullString
	StartedAt   sql.NullString
	FinishedAt  sql.NullString

	// The hop, carried on the claim so the runner needs no second query. The
	// endpoints are ROW IDs rather than names because that is what the
	// transfer recorded: a request's intent is durable, and re-deriving the
	// destination from current configuration is exactly the mistake
	// transfer/request.go documents not making.
	ProductID         int64
	ProductName       string
	OriginRepoID      int64
	DestinationRepoID int64
	PackageTag        string
	PackageDigest     string
}

// Open records a claimed promotion and moves its transfer into `promoting`.
//
// One transaction, because the two halves are one fact: a transfer in
// `promoting` with no promotion row is invisible to the runner forever, and a
// promotion row whose transfer never left `planning` would be run twice.
//
// Idempotent by the UNIQUE on transfer_id. A re-expansion - which happens when
// a Coordinator is killed between opening this and settling it - finds the
// existing row and its recorded names rather than appending a second set.
func (p *Promotions) Open(
	ctx context.Context, transferID, promoter string, names []PromotionName,
) error {
	if len(names) == 0 {
		return fmt.Errorf("promotion of transfer %s publishes no names", transferID)
	}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin promotion of %s: %w", transferID, err)
	}
	defer func() { _ = tx.Rollback() }()

	var id int64
	err = tx.QueryRowContext(ctx, p.dialect.Rewrite(
		`SELECT id FROM promotions WHERE transfer_id = ?`), transferID).Scan(&id)
	switch {
	case err == nil:
		// Already opened. The names it carries are the ones decided when
		// somebody asked, and they stay - see the migration. What is reopened
		// is the promotion's own state, and only from `failed`: a `requested`
		// or `running` row may be held by another Coordinator right now, and
		// resetting it would hand the same release to two of them.
		//
		// The NAMES are deliberately not reset. Everything already published
		// stays published, so a retry re-issues what is left rather than the
		// whole release - which on a 260-name bundle is the difference between
		// seconds and minutes.
		reopen := p.dialect.Rewrite(`
			UPDATE promotions
			   SET state = 'requested', claimed_by = '', heartbeat_at = NULL,
			       finished_at = NULL, updated_at = ` + p.dialect.Now() + `
			 WHERE id = ? AND state = 'failed'`)
		if _, err := tx.ExecContext(ctx, reopen, id); err != nil {
			return fmt.Errorf("reopen the promotion of %s: %w", transferID, err)
		}
	case errors.Is(err, sql.ErrNoRows):
		if id, err = p.insert(ctx, tx, transferID, promoter, names); err != nil {
			return err
		}
	default:
		return fmt.Errorf("look up the promotion of %s: %w", transferID, err)
	}

	// `planning` is where the expander is when it claims; `failed` is a retry.
	// Anything else is a transfer somebody has since paused, stopped or
	// settled, and this must not drag it back.
	stmt := p.dialect.Rewrite(`
		UPDATE transfers
		   SET state = 'promoting', strategy = 'relocate',
		       started_at = COALESCE(started_at, ` + p.dialect.Now() + `),
		       updated_at = ` + p.dialect.Now() + `
		 WHERE id = ? AND state IN ('pending','planning','failed')`)
	if _, err := tx.ExecContext(ctx, stmt, transferID); err != nil {
		return fmt.Errorf("mark transfer %s promoting: %w", transferID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit promotion of %s: %w", transferID, err)
	}
	_ = id
	return nil
}

func (p *Promotions) insert(
	ctx context.Context, tx *sql.Tx, transferID, promoter string, names []PromotionName,
) (int64, error) {
	var id int64

	if p.dialect.Name() == DriverPostgres {
		err := tx.QueryRowContext(ctx, p.dialect.Rewrite(`
			INSERT INTO promotions (transfer_id, promoter, state, names_total)
			VALUES (?, ?, 'requested', ?)
			RETURNING id`), transferID, promoter, len(names)).Scan(&id)
		if err != nil {
			return 0, fmt.Errorf("open promotion of %s: %w", transferID, err)
		}
	} else {
		res, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
			INSERT INTO promotions (transfer_id, promoter, state, names_total)
			VALUES (?, ?, 'requested', ?)`), transferID, promoter, len(names))
		if err != nil {
			return 0, fmt.Errorf("open promotion of %s: %w", transferID, err)
		}
		if id, err = res.LastInsertId(); err != nil {
			return 0, fmt.Errorf("open promotion of %s: %w", transferID, err)
		}
	}

	stmt := p.dialect.Rewrite(`
		INSERT INTO promotion_names (promotion_id, position, repository, tag, digest)
		VALUES (?, ?, ?, ?, ?)`)
	for i, n := range names {
		if _, err := tx.ExecContext(ctx, stmt, id, i, n.Repository, n.Tag, n.Digest); err != nil {
			return 0, fmt.Errorf("record name %s:%s of promotion %d: %w",
				n.Repository, n.Tag, id, err)
		}
	}
	return id, nil
}

// StaleAfter is how long a promotion may go without a heartbeat before another
// Coordinator may take it.
//
// Seconds rather than the reaper's minutes, for the reason 00027 gives about a
// security sync: a Coordinator killed mid-promotion leaves a row that reads as
// running on another replica, and on a single-Coordinator deployment that
// sentence is simply false. A stopped heartbeat is the one honest signal the
// holder is gone.
const StaleAfter = 90 * time.Second

// ClaimPromotion takes the next promotion that needs running.
//
// One statement, so two Coordinators racing produce one claim rather than two
// promotions of the same release. `owner` is recorded so a replica recognises
// its own abandoned claims after a restart, and so a stuck promotion is
// attributable in a log.
//
// Returns ErrNoRecord when there is nothing to do, which is the ordinary
// answer on almost every tick.
func (p *Promotions) ClaimPromotion(ctx context.Context, owner string) (Promotion, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return Promotion{}, fmt.Errorf("begin promotion claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// A promotion is runnable when it is `requested`, or when it is `running`
	// and its holder has stopped beating. The transfer has to still be in
	// `promoting`: one somebody stopped or retried as a copy must not be
	// picked up by this loop.
	query := p.dialect.Rewrite(`
		SELECT pm.id, pm.transfer_id, pm.promoter, pm.state, pm.names_total, pm.names_done,
		       pm.attempts, pm.last_error, pm.claimed_by,
		       pr.id, pr.name, t.source_repo_id, t.target_repo_id, pkg.tag, pkg.manifest_digest
		  FROM promotions pm
		  JOIN transfers t          ON t.id = pm.transfer_id
		  JOIN transfer_requests rq ON rq.id = t.request_id
		  JOIN products pr          ON pr.id = rq.product_id
		  JOIN packages pkg         ON pkg.id = t.package_id
		 WHERE t.state = 'promoting'
		   AND (pm.state = 'requested'
		        OR (pm.state = 'running'
		            AND (pm.heartbeat_at IS NULL
		                 OR pm.heartbeat_at < ` + p.dialect.TimeAgo("?") + `)))
		 ORDER BY pm.requested_at
		 LIMIT 1`)

	var pm Promotion
	err = tx.QueryRowContext(ctx, query, int(StaleAfter.Seconds())).Scan(
		&pm.ID, &pm.TransferID, &pm.Promoter, &pm.State, &pm.NamesTotal, &pm.NamesDone,
		&pm.Attempts, &pm.LastError, &pm.ClaimedBy,
		&pm.ProductID, &pm.ProductName, &pm.OriginRepoID, &pm.DestinationRepoID,
		&pm.PackageTag, &pm.PackageDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return Promotion{}, ErrNoRecord
	}
	if err != nil {
		return Promotion{}, fmt.Errorf("find a promotion to run: %w", err)
	}

	stmt := p.dialect.Rewrite(`
		UPDATE promotions
		   SET state = 'running', claimed_by = ?, attempts = attempts + 1,
		       heartbeat_at = ` + p.dialect.Now() + `,
		       started_at = COALESCE(started_at, ` + p.dialect.Now() + `),
		       updated_at = ` + p.dialect.Now() + `
		 WHERE id = ?`)
	if _, err := tx.ExecContext(ctx, stmt, owner, pm.ID); err != nil {
		return Promotion{}, fmt.Errorf("claim promotion %d: %w", pm.ID, err)
	}

	if err := tx.Commit(); err != nil {
		return Promotion{}, fmt.Errorf("commit promotion claim: %w", err)
	}

	pm.State, pm.ClaimedBy, pm.Attempts = "running", owner, pm.Attempts+1
	return pm, nil
}

// AllNames is every name of a promotion, published or not, in order.
//
// The whole set rather than the outstanding one, because the CLAIM is made
// against the hop as a whole - "these two targets are repositories of one
// Artifactory" is a fact about the pair - and a resumed promotion asking with
// a shrinking set would be asking with different evidence each time.
func (p *Promotions) AllNames(ctx context.Context, promotionID int64) ([]PromotionNameState, error) {
	return p.names(ctx, promotionID, false)
}

// PendingNames lists what a promotion has left to publish, in order.
func (p *Promotions) PendingNames(ctx context.Context, promotionID int64) ([]PromotionNameState, error) {
	return p.names(ctx, promotionID, true)
}

// names reads a promotion's names.
//
// Ordered by position, which is document order from the tree: the root - the
// name that makes the release resolvable at all - comes first, so a promotion
// interrupted part way has left a consistent prefix rather than an arbitrary
// subset.
func (p *Promotions) names(
	ctx context.Context, promotionID int64, pendingOnly bool,
) ([]PromotionNameState, error) {
	where := ""
	if pendingOnly {
		where = " AND state <> 'promoted'"
	}
	query := p.dialect.Rewrite(`
		SELECT position, repository, tag, digest, state, last_error
		  FROM promotion_names
		 WHERE promotion_id = ?` + where + `
		 ORDER BY position`)

	rows, err := p.db.QueryContext(ctx, query, promotionID)
	if err != nil {
		return nil, fmt.Errorf("list the names of promotion %d: %w", promotionID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []PromotionNameState
	for rows.Next() {
		var n PromotionNameState
		if err := rows.Scan(&n.Position, &n.Repository, &n.Tag, &n.Digest,
			&n.State, &n.LastError); err != nil {
			return nil, fmt.Errorf("scan promotion name: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// NamePromoted records one name as published, and beats the heartbeat.
//
// The two together rather than separately: a promotion making steady progress
// through two hundred names is alive by definition, and a heartbeat that only
// fired on a timer could still expire mid-release on a slow instance.
func (p *Promotions) NamePromoted(ctx context.Context, promotionID int64, position int) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin name completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt := p.dialect.Rewrite(`
		UPDATE promotion_names
		   SET state = 'promoted', last_error = '', promoted_at = ` + p.dialect.Now() + `
		 WHERE promotion_id = ? AND position = ? AND state <> 'promoted'`)
	res, err := tx.ExecContext(ctx, stmt, promotionID, position)
	if err != nil {
		return fmt.Errorf("record name %d of promotion %d: %w", position, promotionID, err)
	}
	// Counted from the rows this statement actually changed, so a name
	// recorded twice cannot inflate the total past the denominator.
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		bump := p.dialect.Rewrite(`
			UPDATE promotions
			   SET names_done = names_done + 1, heartbeat_at = ` + p.dialect.Now() + `,
			       updated_at = ` + p.dialect.Now() + `
			 WHERE id = ?`)
		if _, err := tx.ExecContext(ctx, bump, promotionID); err != nil {
			return fmt.Errorf("advance promotion %d: %w", promotionID, err)
		}
	}
	return tx.Commit()
}

// NameFailed records why one name did not publish.
func (p *Promotions) NameFailed(
	ctx context.Context, promotionID int64, position int, reason string,
) error {
	stmt := p.dialect.Rewrite(`
		UPDATE promotion_names SET state = 'failed', last_error = ?
		 WHERE promotion_id = ? AND position = ?`)
	if _, err := p.db.ExecContext(ctx, stmt, truncate(reason, 2000), promotionID, position); err != nil {
		return fmt.Errorf("record the failure of name %d: %w", position, err)
	}
	return nil
}

// Beat renews a claim.
func (p *Promotions) Beat(ctx context.Context, promotionID int64, owner string) error {
	stmt := p.dialect.Rewrite(`
		UPDATE promotions SET heartbeat_at = ` + p.dialect.Now() + `,
		       updated_at = ` + p.dialect.Now() + `
		 WHERE id = ? AND claimed_by = ?`)
	if _, err := p.db.ExecContext(ctx, stmt, promotionID, owner); err != nil {
		return fmt.Errorf("beat promotion %d: %w", promotionID, err)
	}
	return nil
}

// Settle closes a promotion and its transfer together.
//
// state is `succeeded` or `failed`, and there is no third. A mirror can
// diverge - it follows an upstream tag that moved, which is a fact rather than
// a fault - but a promotion is a copy WE asked one registry to make between
// two of its own repositories. A destination holding something else has gone
// wrong, and `failed` is the only honest word for it.
func (p *Promotions) Settle(ctx context.Context, promotionID int64, state, reason string) error {
	switch state {
	case "succeeded", "failed":
	default:
		return fmt.Errorf("settle promotion %d: %q is not a promotion outcome", promotionID, state)
	}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin promotion settle: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var transferID string
	if err := tx.QueryRowContext(ctx, p.dialect.Rewrite(
		`SELECT transfer_id FROM promotions WHERE id = ?`), promotionID).Scan(&transferID); err != nil {
		return fmt.Errorf("look up the transfer of promotion %d: %w", promotionID, err)
	}

	stmt := p.dialect.Rewrite(`
		UPDATE promotions
		   SET state = ?, last_error = ?, claimed_by = '',
		       finished_at = ` + p.dialect.Now() + `, updated_at = ` + p.dialect.Now() + `
		 WHERE id = ?`)
	if _, err := tx.ExecContext(ctx, stmt, state, truncate(reason, 2000), promotionID); err != nil {
		return fmt.Errorf("settle promotion %d: %w", promotionID, err)
	}

	settle := p.dialect.Rewrite(`
		UPDATE transfers
		   SET state = ?, failure_reason = ?, completed_at = ` + p.dialect.Now() + `,
		       updated_at = ` + p.dialect.Now() + `
		 WHERE id = ? AND state = 'promoting'`)
	if _, err := tx.ExecContext(ctx, settle, state, nullIfBlank(reason), transferID); err != nil {
		return fmt.Errorf("settle transfer %s as %s: %w", transferID, state, err)
	}

	return tx.Commit()
}

// ForTransfer reads one transfer's promotion, for the API and the CLI.
//
// ErrNoRecord when the transfer had none, which is every ordinary copy and
// therefore not an error at any caller.
func (p *Promotions) ForTransfer(ctx context.Context, transferID string) (Promotion, error) {
	query := p.dialect.Rewrite(`
		SELECT id, transfer_id, promoter, state, names_total, names_done,
		       attempts, last_error, claimed_by, requested_at, started_at, finished_at
		  FROM promotions WHERE transfer_id = ?`)

	var pm Promotion
	err := p.db.QueryRowContext(ctx, query, transferID).Scan(
		&pm.ID, &pm.TransferID, &pm.Promoter, &pm.State, &pm.NamesTotal, &pm.NamesDone,
		&pm.Attempts, &pm.LastError, &pm.ClaimedBy,
		&pm.RequestedAt, &pm.StartedAt, &pm.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Promotion{}, ErrNoRecord
	}
	if err != nil {
		return Promotion{}, fmt.Errorf("read the promotion of transfer %s: %w", transferID, err)
	}
	return pm, nil
}
