-- +goose Up
-- Initial schema. Translates docs/design/03-persistence.md sections 4-7.
--
-- Every state column carries a CHECK constraint enumerating exactly the states
-- defined in internal/platform/statemachine/machines.go. The database refuses
-- to store a state the state machine does not define; a test asserts the two
-- lists stay in step.
--
-- Note on CREATE INDEX CONCURRENTLY: doc 03 section 9 calls for it, but that is
-- for adding an index to a LIVE, POPULATED table. This migration creates empty
-- tables, so plain CREATE INDEX inside the transaction is correct and faster.
-- The goose NO TRANSACTION annotation belongs on later migrations that add an
-- index to a populated table, not on this one.
--
-- CAUTION: never write a goose directive's literal text inside a comment here.
-- The parser inspects every SQL-comment line for its marker and will try to
-- interpret the comment as a real directive, failing the migration at startup.
-- migrations_test.go enforces this.

-- ---------------------------------------------------------------------------
-- Catalog
-- ---------------------------------------------------------------------------

CREATE TABLE products (
    id           BIGSERIAL PRIMARY KEY,
    name         TEXT        NOT NULL,
    display_name TEXT,
    owner        TEXT,
    config_hash  TEXT        NOT NULL,
    config       JSONB       NOT NULL,
    active       BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (name, config_hash)
);
CREATE UNIQUE INDEX products_active_name_idx ON products (name) WHERE active;

CREATE TABLE repositories (
    id              BIGSERIAL PRIMARY KEY,
    product_id      BIGINT      NOT NULL REFERENCES products(id),
    role            TEXT        NOT NULL CHECK (role IN ('source','target')),
    name            TEXT        NOT NULL,
    registry_host   TEXT        NOT NULL,
    repository_path TEXT        NOT NULL,
    registry_type   TEXT        NOT NULL DEFAULT 'generic'
                      CHECK (registry_type IN ('generic','acr','artifactory','quay')),
    active          BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (product_id, role, name)
);

-- One physical repository is one row, independent of how many products point
-- at it. This is what makes blob_placements shared across products: if two
-- products replicate into the same repository, the second benefits from the
-- first's uploads. Keying placements by config entry would forfeit that.
CREATE UNIQUE INDEX repositories_physical_idx
    ON repositories (registry_host, repository_path);

-- ---------------------------------------------------------------------------
-- Discovery and packages
-- ---------------------------------------------------------------------------

CREATE TABLE packages (
    id              BIGSERIAL PRIMARY KEY,
    product_id      BIGINT      NOT NULL REFERENCES products(id),
    source_repo_id  BIGINT      NOT NULL REFERENCES repositories(id),
    tag             TEXT        NOT NULL,
    manifest_digest TEXT        NOT NULL,
    media_type      TEXT        NOT NULL,
    total_bytes     BIGINT,
    artifact_count  INTEGER,
    blob_count      INTEGER,
    state           TEXT        NOT NULL DEFAULT 'discovered'
                      CHECK (state IN ('discovered','queued','transferring',
                                       'transferred','verifying','verified',
                                       'verification_failed','failed','superseded')),
    discovered_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    superseded_by   BIGINT      REFERENCES packages(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Invariant I4: a re-scan can never create a duplicate. Idempotency is
    -- structural, not procedural — there is no check-then-insert race.
    UNIQUE (source_repo_id, tag, manifest_digest)
);
CREATE INDEX packages_product_discovered_idx ON packages (product_id, discovered_at DESC);
CREATE INDEX packages_state_idx ON packages (state)
    WHERE state NOT IN ('verified','failed','superseded');

CREATE TABLE package_artifacts (
    id            BIGSERIAL PRIMARY KEY,
    package_id    BIGINT      NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
    parent_id     BIGINT      REFERENCES package_artifacts(id) ON DELETE CASCADE,
    digest        TEXT        NOT NULL,
    media_type    TEXT        NOT NULL,
    artifact_type TEXT,
    size_bytes    BIGINT      NOT NULL,
    platform      TEXT,
    depth         SMALLINT    NOT NULL DEFAULT 0,
    -- Stored VERBATIM. A manifest must be pushed byte-for-byte identical,
    -- because its digest — and every signature over it — is the hash of these
    -- exact bytes. Re-serializing from a parsed struct would change whitespace
    -- or key order and produce a different digest.
    raw           BYTEA       NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (package_id, digest)
);
CREATE INDEX package_artifacts_parent_idx ON package_artifacts (parent_id);

-- Global blob catalog, keyed by digest ALONE. Never scoped to a product:
-- sha256:abc... means the same bytes everywhere, so two products from two
-- vendors sharing a base layer reference the same row.
CREATE TABLE blobs (
    digest     TEXT        PRIMARY KEY,
    size_bytes BIGINT      NOT NULL,
    media_type TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE artifact_blobs (
    artifact_id BIGINT   NOT NULL REFERENCES package_artifacts(id) ON DELETE CASCADE,
    digest      TEXT     NOT NULL REFERENCES blobs(digest),
    kind        TEXT     NOT NULL CHECK (kind IN ('config','layer')),
    ordinal     SMALLINT NOT NULL,
    PRIMARY KEY (artifact_id, digest, kind)
);

-- The dedupe index. Small table, disproportionate value: answers "is this
-- digest already in this repository" without a network call.
CREATE TABLE blob_placements (
    repository_id BIGINT      NOT NULL REFERENCES repositories(id),
    digest        TEXT        NOT NULL REFERENCES blobs(digest),
    size_bytes    BIGINT      NOT NULL,
    source        TEXT        NOT NULL CHECK (source IN ('transferred','mounted','observed')),
    verified_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (repository_id, digest)
);
CREATE INDEX blob_placements_verified_idx ON blob_placements (verified_at);

-- ---------------------------------------------------------------------------
-- Transfers
-- ---------------------------------------------------------------------------

CREATE TABLE transfer_requests (
    id              UUID        PRIMARY KEY,
    product_id      BIGINT      NOT NULL REFERENCES products(id),
    package_id      BIGINT      NOT NULL REFERENCES packages(id),
    operation       TEXT        NOT NULL CHECK (operation IN ('replicate','promote','verify')),
    source_repo_id  BIGINT      NOT NULL REFERENCES repositories(id),
    priority        SMALLINT    NOT NULL DEFAULT 50 CHECK (priority BETWEEN 0 AND 1000),
    scheduled_at    TIMESTAMPTZ,
    idempotency_key TEXT        NOT NULL,
    requested_by    TEXT        NOT NULL DEFAULT 'system',
    request_origin  TEXT        NOT NULL DEFAULT 'api'
                      CHECK (request_origin IN ('api','cli','auto_download','schedule')),
    auto_rule_name  TEXT,
    state           TEXT        NOT NULL DEFAULT 'pending'
                      CHECK (state IN ('pending','scheduled','expanded','completed','failed','cancelled')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Invariant I3: a duplicate request creates no additional work.
    UNIQUE (idempotency_key)
);
CREATE INDEX transfer_requests_pkg_idx ON transfer_requests (package_id);

CREATE TABLE transfers (
    id                   UUID        PRIMARY KEY,
    request_id           UUID        NOT NULL REFERENCES transfer_requests(id),
    package_id           BIGINT      NOT NULL REFERENCES packages(id),
    source_repo_id       BIGINT      NOT NULL REFERENCES repositories(id),
    target_repo_id       BIGINT      NOT NULL REFERENCES repositories(id),
    state                TEXT        NOT NULL DEFAULT 'pending'
                           CHECK (state IN ('pending','planning','ready','running','paused',
                                            'verifying','succeeded','failed','cancelling','cancelled')),
    priority             SMALLINT    NOT NULL DEFAULT 50,
    current_wave         SMALLINT    NOT NULL DEFAULT 0,
    max_wave             SMALLINT    NOT NULL DEFAULT 0,

    -- Plan totals fixed at planning time. Progress is always computed from
    -- jobs (invariant I6); these are denominators, not counters.
    planned_job_count    INTEGER     NOT NULL DEFAULT 0,
    planned_bytes        BIGINT      NOT NULL DEFAULT 0,
    dedupe_skipped_bytes BIGINT      NOT NULL DEFAULT 0,
    mountable_bytes      BIGINT      NOT NULL DEFAULT 0,

    failure_reason       TEXT,
    started_at           TIMESTAMPTZ,
    completed_at         TIMESTAMPTZ,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (request_id, target_repo_id)
);
CREATE INDEX transfers_active_idx ON transfers (state, priority DESC)
    WHERE state IN ('ready','running','paused');

CREATE TABLE jobs (
    id                BIGSERIAL PRIMARY KEY,
    transfer_id       UUID        NOT NULL REFERENCES transfers(id) ON DELETE CASCADE,
    kind              TEXT        NOT NULL CHECK (kind IN ('blob','manifest')),

    digest            TEXT        NOT NULL,
    size_bytes        BIGINT      NOT NULL DEFAULT 0,
    artifact_id       BIGINT      REFERENCES package_artifacts(id),
    media_type        TEXT,
    -- Denormalised from transfers so the dequeue path needs no join.
    source_repo_id    BIGINT      NOT NULL REFERENCES repositories(id),
    target_repo_id    BIGINT      NOT NULL REFERENCES repositories(id),

    state             TEXT        NOT NULL DEFAULT 'pending'
                        CHECK (state IN ('blocked','pending','leased','succeeded',
                                         'skipped','failed','cancelled')),
    skip_reason       TEXT        CHECK (skip_reason IN ('placement_hit','exists_at_target','mounted')),

    wave              SMALLINT    NOT NULL DEFAULT 0,
    priority          SMALLINT    NOT NULL DEFAULT 50,
    -- Denormalised from transfers.state so pause is a bulk UPDATE and the hot
    -- index stays join-free.
    paused            BOOLEAN     NOT NULL DEFAULT FALSE,

    attempts          SMALLINT    NOT NULL DEFAULT 0,
    max_attempts      SMALLINT    NOT NULL DEFAULT 8,
    next_visible_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error        TEXT,
    last_error_class  TEXT,

    lease_owner       TEXT,
    lease_expires_at  TIMESTAMPTZ,

    bytes_transferred BIGINT      NOT NULL DEFAULT 0,
    upload_state      JSONB,

    started_at        TIMESTAMPTZ,
    completed_at      TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (transfer_id, kind, digest)
);

-- THE hot index. Partial, so it holds only leasable work: a queue with ten
-- million completed rows still has a small index. Column order matches the
-- dequeue ORDER BY exactly, so the planner scans in order with no sort.
--
-- No `wave` column here: wave gating is resolved into the state column
-- ('blocked' vs 'pending') at plan and wave-advance time.
CREATE INDEX jobs_dequeue_idx
    ON jobs (priority DESC, next_visible_at, id)
    WHERE state = 'pending' AND NOT paused;

CREATE INDEX jobs_lease_expiry_idx ON jobs (lease_expires_at) WHERE state = 'leased';

-- Concurrent duplicate suppression: is this exact blob already being moved to
-- this exact repository right now?
CREATE INDEX jobs_inflight_blob_idx ON jobs (target_repo_id, digest) WHERE state = 'leased';

CREATE INDEX jobs_transfer_state_idx ON jobs (transfer_id, state);
CREATE INDEX jobs_completed_at_idx ON jobs (completed_at)
    WHERE state IN ('succeeded','skipped','failed','cancelled');

-- Separate from jobs on purpose: the queue contains only EXECUTABLE work.
-- A request scheduled for next Tuesday is not work, it is an appointment.
CREATE TABLE scheduled_requests (
    id             UUID        PRIMARY KEY,
    request_id     UUID        NOT NULL REFERENCES transfer_requests(id),
    execute_at     TIMESTAMPTZ NOT NULL,
    state          TEXT        NOT NULL DEFAULT 'scheduled'
                     CHECK (state IN ('scheduled','due','expanded','cancelled','failed')),
    expanded_at    TIMESTAMPTZ,
    failure_reason TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX scheduled_requests_due_idx ON scheduled_requests (execute_at) WHERE state = 'scheduled';

-- ---------------------------------------------------------------------------
-- Verification, notification, audit, workers
-- ---------------------------------------------------------------------------

CREATE TABLE verifications (
    id             BIGSERIAL PRIMARY KEY,
    package_id     BIGINT      NOT NULL REFERENCES packages(id),
    transfer_id    UUID        REFERENCES transfers(id),
    repository_id  BIGINT      NOT NULL REFERENCES repositories(id),
    stage          TEXT        NOT NULL CHECK (stage IN ('source','destination','on_demand')),
    -- `failed` (signature did not check out) and `error` (verification could
    -- not run) are deliberately distinct: collapsing them would make a
    -- Sigstore outage indistinguishable from a supply-chain attack.
    state          TEXT        NOT NULL DEFAULT 'pending'
                     CHECK (state IN ('pending','running','passed','failed','error','skipped')),
    policy         TEXT        NOT NULL CHECK (policy IN ('enforce','warn')),
    subject_digest TEXT        NOT NULL,
    details        JSONB,
    failure_reason TEXT,
    started_at     TIMESTAMPTZ,
    completed_at   TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX verifications_package_idx ON verifications (package_id, created_at DESC);

-- Transactional outbox: a notification is committed in the SAME transaction as
-- the state change that caused it, so we can never notify about something that
-- did not happen, nor silently fail to notify about something that did.
CREATE TABLE notifications (
    id              BIGSERIAL PRIMARY KEY,
    product_id      BIGINT      NOT NULL REFERENCES products(id),
    event_type      TEXT        NOT NULL,
    channel_name    TEXT        NOT NULL,
    channel_type    TEXT        NOT NULL CHECK (channel_type IN ('email','teams')),
    subject_kind    TEXT        NOT NULL,
    subject_id      TEXT        NOT NULL,
    payload         JSONB       NOT NULL,
    state           TEXT        NOT NULL DEFAULT 'pending'
                      CHECK (state IN ('pending','sending','sent','failed','suppressed')),
    attempts        SMALLINT    NOT NULL DEFAULT 0,
    max_attempts    SMALLINT    NOT NULL DEFAULT 5,
    next_visible_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error      TEXT,
    dedupe_key      TEXT        NOT NULL,
    sent_at         TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (dedupe_key)
);
CREATE INDEX notifications_outbox_idx ON notifications (next_visible_at, id) WHERE state = 'pending';

-- Append-only, independent of application logs, queryable via the API.
-- Monthly RANGE partitions so expiring a month is a DROP TABLE — instant, no
-- row-by-row work, no bloat, no VACUUM debt. This is the highest-volume table
-- in the system and the only reason it is partitioned.
CREATE TABLE audit_events (
    id           BIGSERIAL,
    occurred_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    event_type   TEXT        NOT NULL,
    actor        TEXT        NOT NULL DEFAULT 'system',
    actor_kind   TEXT        NOT NULL DEFAULT 'system'
                   CHECK (actor_kind IN ('user','system','worker','schedule','auto_rule')),
    product_name TEXT,
    subject_kind TEXT,
    subject_id   TEXT,
    request_id   TEXT,
    trace_id     TEXT,
    outcome      TEXT        NOT NULL DEFAULT 'success' CHECK (outcome IN ('success','failure')),
    detail       JSONB,
    PRIMARY KEY (id, occurred_at)
) PARTITION BY RANGE (occurred_at);

CREATE INDEX audit_events_subject_idx ON audit_events (subject_kind, subject_id, occurred_at DESC);
CREATE INDEX audit_events_product_idx ON audit_events (product_name, occurred_at DESC);
CREATE INDEX audit_events_type_idx    ON audit_events (event_type, occurred_at DESC);

-- A default partition guarantees an insert never fails because the GC tick has
-- not yet created next month's partition. The retention job creates real
-- monthly partitions ahead of time and drains this one.
CREATE TABLE audit_events_default PARTITION OF audit_events DEFAULT;

-- Advisory: the authoritative worker-liveness signal is the lease on a job,
-- not a row here. This exists for `transferctl workers`, budget division and
-- log routing.
CREATE TABLE workers (
    id                  TEXT        PRIMARY KEY,
    version             TEXT        NOT NULL,
    max_concurrency     SMALLINT    NOT NULL,
    granted_concurrency SMALLINT    NOT NULL DEFAULT 0,
    active_jobs         SMALLINT    NOT NULL DEFAULT 0,
    last_heartbeat_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    state               TEXT        NOT NULL DEFAULT 'active'
                          CHECK (state IN ('active','draining','stale')),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX workers_heartbeat_idx ON workers (last_heartbeat_at);

-- Adaptive concurrency state. Persisted so a Coordinator restart resumes at
-- the learned value rather than re-probing a vendor registry from scratch
-- after every deployment.
CREATE TABLE repository_budgets (
    repository_id    BIGINT      NOT NULL REFERENCES repositories(id),
    direction        TEXT        NOT NULL CHECK (direction IN ('upload','download')),
    configured_max   SMALLINT    NOT NULL,
    current_limit    SMALLINT    NOT NULL,
    observed_p95_ms  INTEGER,
    error_rate_ppm   INTEGER,
    last_adjusted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (repository_id, direction)
);

-- A convenience tail for `transferctl logs`, not a log store. Deliberately
-- small and aggressively GC'd; cluster log aggregation remains the record.
CREATE TABLE worker_logs (
    id          BIGSERIAL PRIMARY KEY,
    worker_id   TEXT        NOT NULL,
    job_id      BIGINT,
    transfer_id UUID,
    level       TEXT        NOT NULL,
    message     TEXT        NOT NULL,
    attrs       JSONB,
    logged_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX worker_logs_transfer_idx  ON worker_logs (transfer_id, logged_at DESC);
CREATE INDEX worker_logs_logged_at_idx ON worker_logs (logged_at);

-- Terminal failures awaiting a human.
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
