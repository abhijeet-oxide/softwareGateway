-- Ordering between the transfers of one request.
-- See the Postgres migration of the same name for the argument.
--
-- The table is rebuilt again because SQLite cannot alter a CHECK in place.
-- Same reasoning as 00017: foreign keys off rather than deferred, because
-- jobs.transfer_id is ON DELETE CASCADE; legacy_alter_table on, so the RENAME
-- does not try to validate the dead_letter_jobs view at the one moment the old
-- table is gone and the new one is not yet named.

-- +goose NO TRANSACTION
-- +goose Up
PRAGMA foreign_keys=off;
PRAGMA legacy_alter_table=on;

CREATE TABLE transfers_new (
    id                   TEXT    PRIMARY KEY,
    request_id           TEXT    NOT NULL REFERENCES transfer_requests(id),
    package_id           INTEGER NOT NULL REFERENCES packages(id),
    source_repo_id       INTEGER NOT NULL REFERENCES repositories(id),
    target_repo_id       INTEGER NOT NULL REFERENCES repositories(id),
    state                TEXT    NOT NULL DEFAULT 'pending'
                           CHECK (state IN ('waiting','pending','planning','ready','running','paused','syncing',
                                            'verifying','succeeded','diverged','skipped','failed',
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
    step_index           INTEGER NOT NULL DEFAULT 0,
    depends_on_transfer_id TEXT  REFERENCES transfers(id) ON DELETE SET NULL,
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
CREATE INDEX transfers_waiting_idx ON transfers (depends_on_transfer_id)
    WHERE depends_on_transfer_id IS NOT NULL;

PRAGMA legacy_alter_table=off;
PRAGMA foreign_keys=on;

ALTER TABLE transfer_requests ADD COLUMN rule_revision TEXT NOT NULL DEFAULT '';
ALTER TABLE transfer_requests ADD COLUMN trigger TEXT NOT NULL DEFAULT ''
    CHECK (trigger IN ('', 'discovery', 'manual', 'schedule'));

-- +goose Down
UPDATE transfers SET state = 'failed', failure_reason = 'downgraded from waiting'
 WHERE state = 'waiting';
UPDATE transfers SET state = 'failed', failure_reason = 'downgraded from skipped'
 WHERE state = 'skipped';
