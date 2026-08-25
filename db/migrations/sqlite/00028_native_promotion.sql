-- A promotion the REGISTRY carries out, and the record of what it did.
-- See the Postgres migration of the same name for the argument.
--
-- The table is rebuilt again because SQLite cannot alter a CHECK in place.
-- Same reasoning as 00017 and 00018: foreign keys off rather than deferred,
-- because jobs.transfer_id is ON DELETE CASCADE; legacy_alter_table on, so the
-- RENAME does not try to validate the dead_letter_jobs view at the one moment
-- the old table is gone and the new one is not yet named.

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
                                            'promoting','verifying','succeeded','diverged','skipped','failed',
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
                           CHECK (strategy IN ('copy', 'mirror', 'proxy', 'relocate')),
    step_index           INTEGER NOT NULL DEFAULT 0,
    depends_on_transfer_id TEXT  REFERENCES transfers(id) ON DELETE SET NULL,
    auto_retries         INTEGER NOT NULL DEFAULT 0,
    UNIQUE (request_id, target_repo_id)
);

INSERT INTO transfers_new
    (id, request_id, package_id, source_repo_id, target_repo_id, state, priority,
     current_wave, max_wave, planned_job_count, planned_bytes, dedupe_skipped_bytes,
     mountable_bytes, failure_reason, started_at, completed_at, created_at, updated_at,
     strategy, step_index, depends_on_transfer_id, auto_retries)
SELECT
     id, request_id, package_id, source_repo_id, target_repo_id, state, priority,
     current_wave, max_wave, planned_job_count, planned_bytes, dedupe_skipped_bytes,
     mountable_bytes, failure_reason, started_at, completed_at, created_at, updated_at,
     strategy, step_index, depends_on_transfer_id, auto_retries
  FROM transfers;

DROP TABLE transfers;
ALTER TABLE transfers_new RENAME TO transfers;

CREATE INDEX transfers_active_idx ON transfers (state, priority DESC)
    WHERE state IN ('ready','running','paused');
CREATE INDEX transfers_waiting_idx ON transfers (depends_on_transfer_id)
    WHERE depends_on_transfer_id IS NOT NULL;

PRAGMA legacy_alter_table=off;
PRAGMA foreign_keys=on;

CREATE TABLE promotions (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    transfer_id     TEXT    NOT NULL REFERENCES transfers(id) ON DELETE CASCADE,

    promoter        TEXT    NOT NULL,

    state           TEXT    NOT NULL
        CHECK (state IN ('requested', 'running', 'succeeded', 'failed')),

    names_total     INTEGER NOT NULL DEFAULT 0,
    names_done      INTEGER NOT NULL DEFAULT 0,

    attempts        INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT    NOT NULL DEFAULT '',

    claimed_by      TEXT    NOT NULL DEFAULT '',
    heartbeat_at    TEXT,

    requested_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    started_at      TEXT,
    finished_at     TEXT,
    created_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),

    UNIQUE (transfer_id)
);

CREATE INDEX promotions_open_idx ON promotions (state, requested_at)
    WHERE state IN ('requested', 'running');

-- The NAMES one promotion publishes, and how far it got.
--
-- A child table rather than a JSON column on `promotions`, for one reason that
-- pays for the join: a promotion interrupted half way through can be RESUMED
-- at the exact name rather than restarted. A native promotion is idempotent,
-- so a restart would be correct - but on a 260-name release it would re-issue
-- two hundred calls to discover they were already done, every time a
-- Coordinator was rolled.
--
-- It is also what makes progress honest. A native promotion moves no bytes by
-- construction, so the only true denominator it has is names, and a table of
-- them is where that number comes from rather than a counter somebody has to
-- remember to keep in step.
--
-- Recorded when the promotion is OPENED rather than derived when it runs.
-- What has to arrive at the destination was decided by the tree as it stood
-- when somebody asked; re-deriving it later would let a release re-analysed in
-- between silently change what a promotion means.
CREATE TABLE promotion_names (
    promotion_id    INTEGER NOT NULL REFERENCES promotions(id) ON DELETE CASCADE,
    position        INTEGER NOT NULL,

    -- repository is RELATIVE to each end's configured base path, so one value
    -- re-bases under the origin's prefix to say where to read and under the
    -- destination's to say where to write. Storing either end's absolute path
    -- would bake one of them into both.
    repository      TEXT    NOT NULL,
    tag             TEXT    NOT NULL,
    digest          TEXT    NOT NULL,

    state           TEXT    NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'promoted', 'failed')),
    last_error      TEXT    NOT NULL DEFAULT '',
    promoted_at     TEXT,

    PRIMARY KEY (promotion_id, position)
);

CREATE INDEX promotion_names_pending_idx ON promotion_names (promotion_id, position)
    WHERE state <> 'promoted';

-- +goose Down
DROP TABLE promotion_names;
DROP TABLE promotions;

UPDATE transfers SET strategy = 'copy' WHERE strategy = 'relocate';
UPDATE transfers SET state = 'failed', failure_reason = 'downgraded from promoting'
 WHERE state = 'promoting';
