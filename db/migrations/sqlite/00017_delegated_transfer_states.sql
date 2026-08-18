-- Two states a transfer can be in when the REGISTRY moves the bytes.
-- See the Postgres migration of the same name for the argument.
--
-- SQLite cannot alter a CHECK constraint in place, so the table is rebuilt.
-- Foreign keys are disabled for the rebuild rather than deferred, because
-- `jobs.transfer_id` is ON DELETE CASCADE: dropping the old table with
-- enforcement on would cascade-delete every job row. The pool is a single
-- connection (internal/store/sqlite.go), so the pragma reliably applies to the
-- same connection the statements run on.

-- +goose NO TRANSACTION
-- +goose Up
PRAGMA foreign_keys=off;
-- legacy_alter_table stops the RENAME from trying to rewrite every reference to
-- `transfers` elsewhere in the schema. Without it SQLite validates the
-- dead_letter_jobs view mid-rebuild, at the one moment the old table is gone
-- and the new one is not yet named — and fails on a view that is perfectly
-- correct before and after. The references are by NAME and resolve again the
-- instant the rename completes, which is exactly what legacy behaviour assumes.
PRAGMA legacy_alter_table=on;

CREATE TABLE transfers_new (
    id                   TEXT    PRIMARY KEY,
    request_id           TEXT    NOT NULL REFERENCES transfer_requests(id),
    package_id           INTEGER NOT NULL REFERENCES packages(id),
    source_repo_id       INTEGER NOT NULL REFERENCES repositories(id),
    target_repo_id       INTEGER NOT NULL REFERENCES repositories(id),
    state                TEXT    NOT NULL DEFAULT 'pending'
                           CHECK (state IN ('pending','planning','ready','running','paused','syncing',
                                            'verifying','succeeded','diverged','failed',
                                            'cancelling','cancelled')),
    priority             INTEGER NOT NULL DEFAULT 50,
    current_wave         INTEGER NOT NULL DEFAULT 0,
    max_wave             INTEGER NOT NULL DEFAULT 0,
    planned_job_count    INTEGER NOT NULL DEFAULT 0,
    planned_bytes        INTEGER NOT NULL DEFAULT 0,
    dedupe_skipped_bytes INTEGER NOT NULL DEFAULT 0,
    mountable_bytes      INTEGER NOT NULL DEFAULT 0,
    failure_reason       TEXT,
    started_at           TEXT,
    completed_at         TEXT,
    created_at           TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at           TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    strategy             TEXT    NOT NULL DEFAULT 'copy'
                           CHECK (strategy IN ('copy', 'mirror', 'proxy')),
    UNIQUE (request_id, target_repo_id)
);

INSERT INTO transfers_new
    (id, request_id, package_id, source_repo_id, target_repo_id, state, priority,
     current_wave, max_wave, planned_job_count, planned_bytes, dedupe_skipped_bytes,
     mountable_bytes, failure_reason, started_at, completed_at, created_at, updated_at, strategy)
SELECT
     id, request_id, package_id, source_repo_id, target_repo_id, state, priority,
     current_wave, max_wave, planned_job_count, planned_bytes, dedupe_skipped_bytes,
     mountable_bytes, failure_reason, started_at, completed_at, created_at, updated_at, strategy
  FROM transfers;

DROP TABLE transfers;
ALTER TABLE transfers_new RENAME TO transfers;

CREATE INDEX transfers_active_idx ON transfers (state, priority DESC)
    WHERE state IN ('ready','running','paused');

PRAGMA legacy_alter_table=off;
PRAGMA foreign_keys=on;

-- +goose Down
UPDATE transfers SET state = 'succeeded' WHERE state = 'diverged';
UPDATE transfers SET state = 'failed', failure_reason = 'downgraded from syncing'
 WHERE state = 'syncing';
