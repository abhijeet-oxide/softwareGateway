-- A bundle does not land in one repository, and its components keep their
-- names. See the Postgres migration of the same name for the argument.
--
-- One thing differs, and it is the reason this file is long where the Postgres
-- one is short: SQLite cannot drop a table constraint. Changing the job
-- uniqueness key means rebuilding the table, so the whole definition is
-- restated here — which is also why the two dialects' schemas have to be read
-- together rather than assumed identical.
--
-- The rebuild is the standard SQLite recipe: create the new shape, copy, drop,
-- rename, recreate the indexes. Goose runs each migration in a transaction, so
-- a failure part-way leaves the old table intact.

-- +goose Up
ALTER TABLE jobs ADD COLUMN target_tags TEXT;
ALTER TABLE jobs ADD COLUMN target_repository TEXT;

-- The dead-letter view selects j.* from jobs, so it must go before the table
-- it reads and come back after. SQLite validates views on DDL that touches
-- their tables, which is what makes this ordering load-bearing rather than
-- tidiness.
DROP VIEW IF EXISTS dead_letter_jobs;

CREATE TABLE jobs_rebuilt (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    transfer_id       TEXT    NOT NULL REFERENCES transfers(id) ON DELETE CASCADE,
    kind              TEXT    NOT NULL CHECK (kind IN ('blob','manifest')),

    digest            TEXT    NOT NULL,
    size_bytes        INTEGER NOT NULL DEFAULT 0,
    artifact_id       INTEGER REFERENCES package_artifacts(id),
    media_type        TEXT,
    source_repo_id    INTEGER NOT NULL REFERENCES repositories(id),
    target_repo_id    INTEGER NOT NULL REFERENCES repositories(id),

    state             TEXT    NOT NULL DEFAULT 'pending'
                        CHECK (state IN ('blocked','pending','leased','succeeded',
                                         'skipped','failed','cancelled')),
    skip_reason       TEXT    CHECK (skip_reason IN ('placement_hit','exists_at_target','mounted')),

    wave              INTEGER NOT NULL DEFAULT 0,
    priority          INTEGER NOT NULL DEFAULT 50,
    paused            INTEGER NOT NULL DEFAULT 0,

    attempts          INTEGER NOT NULL DEFAULT 0,
    max_attempts      INTEGER NOT NULL DEFAULT 8,
    next_visible_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    last_error        TEXT,
    last_error_class  TEXT,

    lease_owner       TEXT,
    lease_expires_at  TEXT,

    bytes_transferred INTEGER NOT NULL DEFAULT 0,
    upload_state      TEXT,

    started_at        TEXT,
    completed_at      TEXT,
    created_at        TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at        TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),

    target_tags       TEXT,
    target_repository TEXT,

    -- The change this migration exists for: a blob bound for two different
    -- destination repositories is two jobs, because a registry stores blobs
    -- per repository and "already uploaded" is only true within one.
    UNIQUE (transfer_id, kind, digest, target_repo_id)
);

INSERT INTO jobs_rebuilt
    SELECT id, transfer_id, kind, digest, size_bytes, artifact_id, media_type,
           source_repo_id, target_repo_id, state, skip_reason, wave, priority,
           paused, attempts, max_attempts, next_visible_at, last_error,
           last_error_class, lease_owner, lease_expires_at, bytes_transferred,
           upload_state, started_at, completed_at, created_at, updated_at,
           target_tags, target_repository
      FROM jobs;

DROP TABLE jobs;
ALTER TABLE jobs_rebuilt RENAME TO jobs;

CREATE INDEX jobs_dequeue_idx
    ON jobs (priority DESC, next_visible_at, id)
    WHERE state = 'pending' AND NOT paused;
CREATE INDEX jobs_lease_expiry_idx ON jobs (lease_expires_at) WHERE state = 'leased';
CREATE INDEX jobs_inflight_blob_idx ON jobs (target_repo_id, digest) WHERE state = 'leased';
CREATE INDEX jobs_transfer_state_idx ON jobs (transfer_id, state);
CREATE INDEX jobs_completed_at_idx ON jobs (completed_at)
    WHERE state IN ('succeeded','skipped','failed','cancelled');

CREATE VIEW dead_letter_jobs AS
SELECT j.*, t.package_id
  FROM jobs j
  JOIN transfers t ON t.id = j.transfer_id
 WHERE j.state = 'failed';

-- +goose Down
-- Not reversible without losing the rows the new key permits: two jobs for one
-- digest in one transfer cannot both survive the old constraint. Refusing is
-- honest; silently dropping one would leave a transfer that can never finish.
SELECT RAISE(ABORT, 'irreversible: the old job key cannot hold multi-repository transfers');
