-- Ordering between the transfers of one request.
--
-- The system already orders work with a single integer: jobs carry a wave, and
-- wave n+1 is blocked until wave n drains. A download rule's steps need exactly
-- the same thing one level up, so they get exactly the same thing rather than
-- something new.
--
-- `depends_on_transfer_id` is singular because `mirror.from` names exactly one
-- upstream. The graph is a forest, not a general DAG, and that is what keeps
-- the sort six lines long instead of a worklist with cycle detection at run
-- time.
--
-- `skipped` is a terminal state distinct from `failed`, and the distinction is
-- the point. The Quay step of a run whose JFrog step failed did not fail: it
-- never started, nothing was attempted against Quay, and no operator should go
-- looking there for the cause. Collapsing the two would report two problems
-- where there is one, and the second report would point at the wrong system.

-- +goose Up
ALTER TABLE transfers ADD COLUMN step_index SMALLINT NOT NULL DEFAULT 0;
ALTER TABLE transfers ADD COLUMN depends_on_transfer_id UUID
    REFERENCES transfers(id) ON DELETE SET NULL;

CREATE INDEX transfers_waiting_idx ON transfers (depends_on_transfer_id)
    WHERE depends_on_transfer_id IS NOT NULL;

ALTER TABLE transfers DROP CONSTRAINT transfers_state_check;
ALTER TABLE transfers ADD CONSTRAINT transfers_state_check
    CHECK (state IN ('waiting','pending','planning','ready','running','paused','syncing',
                     'verifying','succeeded','diverged','skipped','failed',
                     'cancelling','cancelled'));

-- The rule and the revision that produced a request.
--
-- rule_revision is what stops an edited rule being swallowed by the old one's
-- idempotency key: tightening verify.policy from warn to enforce, re-running,
-- and getting "already exists" would leave somebody believing the stricter
-- policy had been applied when nothing ran at all.
ALTER TABLE transfer_requests ADD COLUMN rule_revision TEXT NOT NULL DEFAULT '';
ALTER TABLE transfer_requests ADD COLUMN trigger TEXT NOT NULL DEFAULT ''
    CHECK (trigger IN ('', 'discovery', 'manual', 'schedule'));

-- +goose Down
ALTER TABLE transfer_requests DROP COLUMN trigger;
ALTER TABLE transfer_requests DROP COLUMN rule_revision;
UPDATE transfers SET state = 'failed', failure_reason = 'downgraded from waiting'
 WHERE state = 'waiting';
UPDATE transfers SET state = 'failed', failure_reason = 'downgraded from skipped'
 WHERE state = 'skipped';
ALTER TABLE transfers DROP CONSTRAINT transfers_state_check;
ALTER TABLE transfers ADD CONSTRAINT transfers_state_check
    CHECK (state IN ('pending','planning','ready','running','paused','syncing',
                     'verifying','succeeded','diverged','failed',
                     'cancelling','cancelled'));
DROP INDEX transfers_waiting_idx;
ALTER TABLE transfers DROP COLUMN depends_on_transfer_id;
ALTER TABLE transfers DROP COLUMN step_index;
