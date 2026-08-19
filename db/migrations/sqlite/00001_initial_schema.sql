-- +goose Up
-- SQLite mirror of the Postgres schema. DEVELOPMENT ONLY - see
-- docs/design/03-persistence.md section 2. The Coordinator warns at startup.
--
-- The logical schema is identical. Divergence is confined to three things and
-- is deliberate rather than incidental:
--
--   1. Types.        TIMESTAMPTZ -> TEXT (RFC3339 UTC), BIGSERIAL -> INTEGER
--                    PRIMARY KEY AUTOINCREMENT, JSONB/BYTEA -> TEXT/BLOB,
--                    BOOLEAN -> INTEGER (0/1).
--   2. Partitioning. audit_events is a plain table; retention is a batched
--                    DELETE rather than DROP PARTITION.
--   3. Dequeue.      SQLite serializes writers, so BEGIN IMMEDIATE is already
--                    exclusive and FOR UPDATE SKIP LOCKED is unnecessary -
--                    not merely unavailable.
--
-- Every CHECK constraint is identical to Postgres, so a state the machine does
-- not define is rejected in development exactly as it would be in production.

PRAGMA foreign_keys = ON;

CREATE TABLE products (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT    NOT NULL,
    display_name TEXT,
    owner        TEXT,
    config_hash  TEXT    NOT NULL,
    config       TEXT    NOT NULL,
    active       INTEGER NOT NULL DEFAULT 1,
    created_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    UNIQUE (name, config_hash)
);
CREATE UNIQUE INDEX products_active_name_idx ON products (name) WHERE active = 1;

CREATE TABLE repositories (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    product_id      INTEGER NOT NULL REFERENCES products(id),
    role            TEXT    NOT NULL CHECK (role IN ('source','target')),
    name            TEXT    NOT NULL,
    registry_host   TEXT    NOT NULL,
    repository_path TEXT    NOT NULL,
    registry_type   TEXT    NOT NULL DEFAULT 'generic'
                      CHECK (registry_type IN ('generic','acr','artifactory','quay')),
    active          INTEGER NOT NULL DEFAULT 1,
    created_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    UNIQUE (product_id, role, name)
);
CREATE UNIQUE INDEX repositories_physical_idx ON repositories (registry_host, repository_path);

CREATE TABLE packages (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    product_id      INTEGER NOT NULL REFERENCES products(id),
    source_repo_id  INTEGER NOT NULL REFERENCES repositories(id),
    tag             TEXT    NOT NULL,
    manifest_digest TEXT    NOT NULL,
    media_type      TEXT    NOT NULL,
    total_bytes     INTEGER,
    artifact_count  INTEGER,
    blob_count      INTEGER,
    state           TEXT    NOT NULL DEFAULT 'discovered'
                      CHECK (state IN ('discovered','queued','transferring',
                                       'transferred','verifying','verified',
                                       'verification_failed','failed','superseded')),
    discovered_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    superseded_by   INTEGER REFERENCES packages(id),
    created_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    UNIQUE (source_repo_id, tag, manifest_digest)
);
CREATE INDEX packages_product_discovered_idx ON packages (product_id, discovered_at DESC);
CREATE INDEX packages_state_idx ON packages (state)
    WHERE state NOT IN ('verified','failed','superseded');

CREATE TABLE package_artifacts (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    package_id    INTEGER NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
    parent_id     INTEGER REFERENCES package_artifacts(id) ON DELETE CASCADE,
    digest        TEXT    NOT NULL,
    media_type    TEXT    NOT NULL,
    artifact_type TEXT,
    size_bytes    INTEGER NOT NULL,
    platform      TEXT,
    depth         INTEGER NOT NULL DEFAULT 0,
    raw           BLOB    NOT NULL,
    created_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    UNIQUE (package_id, digest)
);
CREATE INDEX package_artifacts_parent_idx ON package_artifacts (parent_id);

CREATE TABLE blobs (
    digest     TEXT    PRIMARY KEY,
    size_bytes INTEGER NOT NULL,
    media_type TEXT,
    created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE artifact_blobs (
    artifact_id INTEGER NOT NULL REFERENCES package_artifacts(id) ON DELETE CASCADE,
    digest      TEXT    NOT NULL REFERENCES blobs(digest),
    kind        TEXT    NOT NULL CHECK (kind IN ('config','layer')),
    ordinal     INTEGER NOT NULL,
    PRIMARY KEY (artifact_id, digest, kind)
);

CREATE TABLE blob_placements (
    repository_id INTEGER NOT NULL REFERENCES repositories(id),
    digest        TEXT    NOT NULL REFERENCES blobs(digest),
    size_bytes    INTEGER NOT NULL,
    source        TEXT    NOT NULL CHECK (source IN ('transferred','mounted','observed')),
    verified_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    created_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY (repository_id, digest)
);
CREATE INDEX blob_placements_verified_idx ON blob_placements (verified_at);

CREATE TABLE transfer_requests (
    id              TEXT    PRIMARY KEY,
    product_id      INTEGER NOT NULL REFERENCES products(id),
    package_id      INTEGER NOT NULL REFERENCES packages(id),
    operation       TEXT    NOT NULL CHECK (operation IN ('replicate','promote','verify')),
    source_repo_id  INTEGER NOT NULL REFERENCES repositories(id),
    priority        INTEGER NOT NULL DEFAULT 50 CHECK (priority BETWEEN 0 AND 1000),
    scheduled_at    TEXT,
    idempotency_key TEXT    NOT NULL,
    requested_by    TEXT    NOT NULL DEFAULT 'system',
    request_origin  TEXT    NOT NULL DEFAULT 'api'
                      CHECK (request_origin IN ('api','cli','auto_download','schedule')),
    auto_rule_name  TEXT,
    state           TEXT    NOT NULL DEFAULT 'pending'
                      CHECK (state IN ('pending','scheduled','expanded','completed','failed','cancelled')),
    created_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    UNIQUE (idempotency_key)
);
CREATE INDEX transfer_requests_pkg_idx ON transfer_requests (package_id);

CREATE TABLE transfers (
    id                   TEXT    PRIMARY KEY,
    request_id           TEXT    NOT NULL REFERENCES transfer_requests(id),
    package_id           INTEGER NOT NULL REFERENCES packages(id),
    source_repo_id       INTEGER NOT NULL REFERENCES repositories(id),
    target_repo_id       INTEGER NOT NULL REFERENCES repositories(id),
    state                TEXT    NOT NULL DEFAULT 'pending'
                           CHECK (state IN ('pending','planning','ready','running','paused',
                                            'verifying','succeeded','failed','cancelling','cancelled')),
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
    UNIQUE (request_id, target_repo_id)
);
CREATE INDEX transfers_active_idx ON transfers (state, priority DESC)
    WHERE state IN ('ready','running','paused');

CREATE TABLE jobs (
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
    UNIQUE (transfer_id, kind, digest)
);
CREATE INDEX jobs_dequeue_idx ON jobs (priority DESC, next_visible_at, id)
    WHERE state = 'pending' AND paused = 0;
CREATE INDEX jobs_lease_expiry_idx  ON jobs (lease_expires_at) WHERE state = 'leased';
CREATE INDEX jobs_inflight_blob_idx ON jobs (target_repo_id, digest) WHERE state = 'leased';
CREATE INDEX jobs_transfer_state_idx ON jobs (transfer_id, state);
CREATE INDEX jobs_completed_at_idx ON jobs (completed_at)
    WHERE state IN ('succeeded','skipped','failed','cancelled');

CREATE TABLE scheduled_requests (
    id             TEXT    PRIMARY KEY,
    request_id     TEXT    NOT NULL REFERENCES transfer_requests(id),
    execute_at     TEXT    NOT NULL,
    state          TEXT    NOT NULL DEFAULT 'scheduled'
                     CHECK (state IN ('scheduled','due','expanded','cancelled','failed')),
    expanded_at    TEXT,
    failure_reason TEXT,
    created_at     TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at     TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE INDEX scheduled_requests_due_idx ON scheduled_requests (execute_at) WHERE state = 'scheduled';

CREATE TABLE verifications (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    package_id     INTEGER NOT NULL REFERENCES packages(id),
    transfer_id    TEXT    REFERENCES transfers(id),
    repository_id  INTEGER NOT NULL REFERENCES repositories(id),
    stage          TEXT    NOT NULL CHECK (stage IN ('source','destination','on_demand')),
    state          TEXT    NOT NULL DEFAULT 'pending'
                     CHECK (state IN ('pending','running','passed','failed','error','skipped')),
    policy         TEXT    NOT NULL CHECK (policy IN ('enforce','warn')),
    subject_digest TEXT    NOT NULL,
    details        TEXT,
    failure_reason TEXT,
    started_at     TEXT,
    completed_at   TEXT,
    created_at     TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE INDEX verifications_package_idx ON verifications (package_id, created_at DESC);

CREATE TABLE notifications (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    product_id      INTEGER NOT NULL REFERENCES products(id),
    event_type      TEXT    NOT NULL,
    channel_name    TEXT    NOT NULL,
    channel_type    TEXT    NOT NULL CHECK (channel_type IN ('email','teams')),
    subject_kind    TEXT    NOT NULL,
    subject_id      TEXT    NOT NULL,
    payload         TEXT    NOT NULL,
    state           TEXT    NOT NULL DEFAULT 'pending'
                      CHECK (state IN ('pending','sending','sent','failed','suppressed')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    max_attempts    INTEGER NOT NULL DEFAULT 5,
    next_visible_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    last_error      TEXT,
    dedupe_key      TEXT    NOT NULL,
    sent_at         TEXT,
    created_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    UNIQUE (dedupe_key)
);
CREATE INDEX notifications_outbox_idx ON notifications (next_visible_at, id) WHERE state = 'pending';

-- Plain table, not partitioned: SQLite has no declarative partitioning, and a
-- development store never accumulates a year of audit history anyway.
CREATE TABLE audit_events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    occurred_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    event_type   TEXT    NOT NULL,
    actor        TEXT    NOT NULL DEFAULT 'system',
    actor_kind   TEXT    NOT NULL DEFAULT 'system'
                   CHECK (actor_kind IN ('user','system','worker','schedule','auto_rule')),
    product_name TEXT,
    subject_kind TEXT,
    subject_id   TEXT,
    request_id   TEXT,
    trace_id     TEXT,
    outcome      TEXT    NOT NULL DEFAULT 'success' CHECK (outcome IN ('success','failure')),
    detail       TEXT
);
CREATE INDEX audit_events_subject_idx ON audit_events (subject_kind, subject_id, occurred_at DESC);
CREATE INDEX audit_events_product_idx ON audit_events (product_name, occurred_at DESC);
CREATE INDEX audit_events_type_idx    ON audit_events (event_type, occurred_at DESC);

CREATE TABLE workers (
    id                  TEXT    PRIMARY KEY,
    version             TEXT    NOT NULL,
    max_concurrency     INTEGER NOT NULL,
    granted_concurrency INTEGER NOT NULL DEFAULT 0,
    active_jobs         INTEGER NOT NULL DEFAULT 0,
    last_heartbeat_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    state               TEXT    NOT NULL DEFAULT 'active'
                          CHECK (state IN ('active','draining','stale')),
    created_at          TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE INDEX workers_heartbeat_idx ON workers (last_heartbeat_at);

CREATE TABLE repository_budgets (
    repository_id    INTEGER NOT NULL REFERENCES repositories(id),
    direction        TEXT    NOT NULL CHECK (direction IN ('upload','download')),
    configured_max   INTEGER NOT NULL,
    current_limit    INTEGER NOT NULL,
    observed_p95_ms  INTEGER,
    error_rate_ppm   INTEGER,
    last_adjusted_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY (repository_id, direction)
);

CREATE TABLE worker_logs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    worker_id   TEXT    NOT NULL,
    job_id      INTEGER,
    transfer_id TEXT,
    level       TEXT    NOT NULL,
    message     TEXT    NOT NULL,
    attrs       TEXT,
    logged_at   TEXT    NOT NULL
);
CREATE INDEX worker_logs_transfer_idx  ON worker_logs (transfer_id, logged_at DESC);
CREATE INDEX worker_logs_logged_at_idx ON worker_logs (logged_at);

CREATE VIEW dead_letter_jobs AS
SELECT j.*, t.package_id
  FROM jobs j
  JOIN transfers t ON t.id = j.transfer_id
 WHERE j.state = 'failed';

-- +goose Down
DROP VIEW IF EXISTS dead_letter_jobs;
DROP TABLE IF EXISTS worker_logs;
DROP TABLE IF EXISTS repository_budgets;
DROP TABLE IF EXISTS workers;
DROP TABLE IF EXISTS audit_events;
DROP TABLE IF EXISTS notifications;
DROP TABLE IF EXISTS verifications;
DROP TABLE IF EXISTS scheduled_requests;
DROP TABLE IF EXISTS jobs;
DROP TABLE IF EXISTS transfers;
DROP TABLE IF EXISTS transfer_requests;
DROP TABLE IF EXISTS blob_placements;
DROP TABLE IF EXISTS artifact_blobs;
DROP TABLE IF EXISTS blobs;
DROP TABLE IF EXISTS package_artifacts;
DROP TABLE IF EXISTS packages;
DROP TABLE IF EXISTS repositories;
DROP TABLE IF EXISTS products;
