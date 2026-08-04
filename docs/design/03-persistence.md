# 03 — Persistence

> **Prerequisite:** [01 — Domain Model](01-domain-model.md) · **Consumed by:** [04](04-queue-and-scheduling.md), [05](05-transfer-engine.md), [07](07-discovery.md), [09](09-api.md), [10](10-state-machines.md), [12](12-observability-and-audit.md)

This document is authoritative for storage. It is intended to be sufficient to write the migrations without further invention.

---

## 1. Choice of store

> **Decision — a single PostgreSQL database holds queue, state, and audit. No Redis, no Kafka, no RabbitMQ, no object store.**
>
> *Alternatives:* Kafka or RabbitMQ for the queue with Postgres for state; Redis for the queue; a NoSQL store for audit volume.
>
> *Rejected because* each adds an operational component with its own failure modes, upgrade path, and on-call surface, to solve a problem Postgres already solves well. `SELECT … FOR UPDATE SKIP LOCKED` has been a production-grade work queue since 9.5. Our throughput requirement is measured in **gigabytes per second of blob traffic**, not messages per second of queue traffic — a 60 GB package with 1,000 blobs is *1,000 queue operations*. Postgres will do that four orders of magnitude faster than we need.
>
> The decisive argument is transactional: *dequeue a job and record its state change* must be atomic with *update the transfer's progress* and *write the audit event*. With a separate broker that is a distributed transaction, or an outbox, or a reconciliation loop. In one database it is `BEGIN … COMMIT`. Removing an entire class of consistency bug is worth more than a broker's throughput ceiling we will never reach.
>
> *What would change our mind:* sustained queue operations beyond ~10 k/s, or a need for fan-out to consumers outside this system.

## 2. Dual dialect: PostgreSQL and SQLite

Production is PostgreSQL. **Development defaults to SQLite** so a developer needs nothing but `go run` ([14](14-deployment-and-development.md) §5).

The logical schema is identical. Divergence is confined to three places, and is stated precisely rather than hand-waved:

| Concern | PostgreSQL | SQLite | Why it does not leak |
|---|---|---|---|
| Concurrent dequeue | `FOR UPDATE SKIP LOCKED` | Not needed — SQLite serializes writers, so `BEGIN IMMEDIATE` + `UPDATE … WHERE id IN (SELECT … LIMIT n)` is already exclusive | Both are correct; only throughput differs, and SQLite is a single-developer store |
| Types | `TIMESTAMPTZ`, `BIGINT`, `JSONB`, `UUID`, `TEXT` | `TEXT` (RFC3339 UTC), `INTEGER`, `TEXT` (JSON), `TEXT`, `TEXT` | Isolated in the store layer's scanners |
| Audit partitioning | Monthly `RANGE` partitions, retention by `DROP` | Single table, retention by batched `DELETE` | GC strategy is a store-layer detail |
| Advisory locks | `pg_advisory_lock` (leader election, blob claims) | No-op: one process, no contention | Guarded by an interface, not `if driver == …` scattered through the code |

Queries live in `db/queries/postgres/*.sql` and `db/queries/sqlite/*.sql`, compiled by `sqlc` into two implementations of one Go interface. Migrations are `goose`, in `db/migrations/{postgres,sqlite}/`.

**SQLite is explicitly not supported in production**, and the Coordinator logs a startup warning saying so. It exists to make development free, not to be a second production target.

## 3. Conventions

- `snake_case` identifiers; plural table names.
- Every table has `created_at TIMESTAMPTZ NOT NULL DEFAULT now()`; mutable tables also have `updated_at`.
- Primary keys are `BIGSERIAL` for high-volume internal tables (`jobs`, `audit_events`) — smaller index footprint and better locality than UUIDs, which matters at hundreds of millions of rows. **`UUID` is used where the ID is externally visible** (`transfer_requests`, `transfers`) so identifiers are not guessable or enumerable and can be minted client-side.
- Digests are `TEXT` storing the full `algo:hex` form. Not `BYTEA`: they appear in URLs, logs, and API responses, and a conversion at every boundary buys 30 bytes a row.
- Enumerations are `TEXT` with `CHECK` constraints, not Postgres `ENUM` types — adding a value to a Postgres enum is a schema migration with locking implications, while a `CHECK` is cheap to alter and portable to SQLite.
- All state columns carry a `CHECK` constraint enumerating exactly the states in [10 — State Machines](10-state-machines.md). **The database refuses to store a state the state machine does not define.**

## 4. Catalog tables

Configuration lives in Git ([02](02-configuration.md)); these tables exist so that discovered packages and historical transfers can reference a stable ID, and so audit records survive a product being reconfigured or removed.

```sql
-- Snapshot of loaded product configuration. One row per product per config
-- version, so an audit record from March still resolves to the config in
-- force in March.
CREATE TABLE products (
    id              BIGSERIAL PRIMARY KEY,
    name            TEXT        NOT NULL,
    display_name    TEXT,
    owner           TEXT,
    config_hash     TEXT        NOT NULL,   -- sha256 of the canonical YAML
    config          JSONB       NOT NULL,   -- the parsed document, for audit
    active          BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (name, config_hash)
);
CREATE UNIQUE INDEX products_active_name_idx
    ON products (name) WHERE active;

-- Repositories, both roles, keyed by identity rather than config position so
-- that renaming a source in YAML does not orphan its discovery history.
CREATE TABLE repositories (
    id              BIGSERIAL PRIMARY KEY,
    product_id      BIGINT      NOT NULL REFERENCES products(id),
    role            TEXT        NOT NULL CHECK (role IN ('source','target')),
    name            TEXT        NOT NULL,   -- as declared in config
    registry_host   TEXT        NOT NULL,
    repository_path TEXT        NOT NULL,
    registry_type   TEXT        NOT NULL DEFAULT 'generic'
                       CHECK (registry_type IN ('generic','acr','artifactory','quay')),
    active          BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (product_id, role, name)
);

-- Identity of the physical repository, independent of which products point at
-- it. Two products replicating into the same repository share placements --
-- this index is what makes cross-product dedupe work (01 section 4).
CREATE UNIQUE INDEX repositories_physical_idx
    ON repositories (registry_host, repository_path);
```

> **Note on the last index.** It enforces that one physical repository is one row, which is exactly what makes `blob_placements` shared across products. If two products both replicate into `internal.azurecr.io/shared/base`, the second one benefits from the first one's uploads. Keying placements by *config entry* instead would forfeit that silently.

## 5. Discovery and package tables

```sql
CREATE TABLE packages (
    id                BIGSERIAL PRIMARY KEY,
    product_id        BIGINT      NOT NULL REFERENCES products(id),
    source_repo_id    BIGINT      NOT NULL REFERENCES repositories(id),
    tag               TEXT        NOT NULL,
    manifest_digest   TEXT        NOT NULL,
    media_type        TEXT        NOT NULL,
    -- Populated at discovery from the manifest; total_bytes is the sum of
    -- distinct blob sizes in the package (not the sum of what we will move).
    total_bytes       BIGINT,
    artifact_count    INTEGER,
    blob_count        INTEGER,
    state             TEXT        NOT NULL DEFAULT 'discovered'
                        CHECK (state IN ('discovered','queued','transferring',
                                         'transferred','verifying','verified',
                                         'verification_failed','failed','superseded')),
    discovered_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    superseded_by     BIGINT      REFERENCES packages(id),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- I4: a re-scan can never create a duplicate. Idempotency is structural.
    UNIQUE (source_repo_id, tag, manifest_digest)
);
CREATE INDEX packages_product_discovered_idx
    ON packages (product_id, discovered_at DESC);
CREATE INDEX packages_state_idx ON packages (state)
    WHERE state NOT IN ('verified','failed','superseded');

-- One row per manifest inside a package (01 section 2.3).
CREATE TABLE package_artifacts (
    id              BIGSERIAL PRIMARY KEY,
    package_id      BIGINT      NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
    parent_id       BIGINT      REFERENCES package_artifacts(id) ON DELETE CASCADE,
    digest          TEXT        NOT NULL,
    media_type      TEXT        NOT NULL,
    artifact_type   TEXT,                    -- OCI 1.1 artifactType, when present
    size_bytes      BIGINT      NOT NULL,
    platform        TEXT,                    -- "linux/amd64", when applicable
    depth           SMALLINT    NOT NULL DEFAULT 0,   -- feeds wave assignment (04 section 3.2)
    raw             BYTEA       NOT NULL,    -- verbatim manifest; see note below
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (package_id, digest)
);

-- Global blob catalog. Keyed by digest ALONE -- never scoped to a product.
CREATE TABLE blobs (
    digest      TEXT        PRIMARY KEY,
    size_bytes  BIGINT      NOT NULL,
    media_type  TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE artifact_blobs (
    artifact_id BIGINT NOT NULL REFERENCES package_artifacts(id) ON DELETE CASCADE,
    digest      TEXT   NOT NULL REFERENCES blobs(digest),
    kind        TEXT   NOT NULL CHECK (kind IN ('config','layer')),
    ordinal     SMALLINT NOT NULL,
    PRIMARY KEY (artifact_id, digest, kind)
);
```

> **Why store `raw` manifests verbatim.** A manifest must be pushed to the destination byte-for-byte identical to the source, because its digest — and therefore every signature over it — is the hash of those exact bytes. Re-serializing from a parsed struct would change whitespace or key order and produce a different digest, breaking verification. Storing the original bytes makes the correct behaviour the easy one. Manifests are kilobytes; this costs nothing.

**The dedupe index.** Small table, disproportionate value ([01](01-domain-model.md) §4):

```sql
CREATE TABLE blob_placements (
    repository_id   BIGINT      NOT NULL REFERENCES repositories(id),
    digest          TEXT        NOT NULL REFERENCES blobs(digest),
    size_bytes      BIGINT      NOT NULL,
    -- How we learned it is there. Feeds the mount/skip hit-rate metrics (12).
    source          TEXT        NOT NULL CHECK (source IN ('transferred','mounted','observed')),
    verified_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (repository_id, digest)
);
CREATE INDEX blob_placements_verified_idx ON blob_placements (verified_at);
```

`verified_at` drives the staleness TTL, and `blob_placements_verified_idx` supports both the TTL sweep and the `BLOB_UNKNOWN` invalidation path described in [01](01-domain-model.md) §4.

## 6. Transfer tables

```sql
-- User intent. Externally visible UUID; carries the idempotency key.
CREATE TABLE transfer_requests (
    id                UUID        PRIMARY KEY,
    product_id        BIGINT      NOT NULL REFERENCES products(id),
    package_id        BIGINT      NOT NULL REFERENCES packages(id),
    operation         TEXT        NOT NULL
                        CHECK (operation IN ('replicate','promote','verify')),
    source_repo_id    BIGINT      NOT NULL REFERENCES repositories(id),
    priority          SMALLINT    NOT NULL DEFAULT 50
                        CHECK (priority BETWEEN 0 AND 1000),
    scheduled_at      TIMESTAMPTZ,            -- NULL = execute immediately
    idempotency_key   TEXT        NOT NULL,
    requested_by      TEXT        NOT NULL DEFAULT 'system',
    request_origin    TEXT        NOT NULL DEFAULT 'api'
                        CHECK (request_origin IN ('api','cli','auto_download','schedule')),
    auto_rule_name    TEXT,                   -- which rule fired, when applicable
    state             TEXT        NOT NULL DEFAULT 'pending'
                        CHECK (state IN ('pending','scheduled','expanded',
                                         'completed','failed','cancelled')),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- I3: a duplicate request creates no additional work.
    UNIQUE (idempotency_key)
);
CREATE INDEX transfer_requests_pkg_idx ON transfer_requests (package_id);

-- One request against one target (01 section 3.2). Unit of pause/resume/
-- cancel/priority and of progress reporting.
CREATE TABLE transfers (
    id                  UUID        PRIMARY KEY,
    request_id          UUID        NOT NULL REFERENCES transfer_requests(id),
    package_id          BIGINT      NOT NULL REFERENCES packages(id),
    source_repo_id      BIGINT      NOT NULL REFERENCES repositories(id),
    target_repo_id      BIGINT      NOT NULL REFERENCES repositories(id),
    state               TEXT        NOT NULL DEFAULT 'pending'
                          CHECK (state IN ('pending','planning','ready','running',
                                           'paused','verifying','succeeded',
                                           'failed','cancelling','cancelled')),
    priority            SMALLINT    NOT NULL DEFAULT 50,
    current_wave        SMALLINT    NOT NULL DEFAULT 0,
    max_wave            SMALLINT    NOT NULL DEFAULT 0,

    -- Plan totals, fixed at planning time. Progress is always computed from
    -- jobs (I6); these are denominators, not counters.
    planned_job_count   INTEGER     NOT NULL DEFAULT 0,
    planned_bytes       BIGINT      NOT NULL DEFAULT 0,
    dedupe_skipped_bytes BIGINT     NOT NULL DEFAULT 0,
    mountable_bytes     BIGINT      NOT NULL DEFAULT 0,

    failure_reason      TEXT,
    started_at          TIMESTAMPTZ,
    completed_at        TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (request_id, target_repo_id)      -- idempotent expansion
);
CREATE INDEX transfers_active_idx ON transfers (state, priority DESC)
    WHERE state IN ('ready','running','paused');
```

### 6.1 `jobs` — the queue

The hot table. Every column exists for the dequeue path, the retry path, or progress; nothing decorative.

```sql
CREATE TABLE jobs (
    id                  BIGSERIAL   PRIMARY KEY,
    transfer_id         UUID        NOT NULL REFERENCES transfers(id) ON DELETE CASCADE,
    kind                TEXT        NOT NULL CHECK (kind IN ('blob','manifest')),

    -- What to move. For kind='manifest', digest is the manifest digest and
    -- artifact_id points at the stored raw bytes.
    digest              TEXT        NOT NULL,
    size_bytes          BIGINT      NOT NULL DEFAULT 0,
    artifact_id         BIGINT      REFERENCES package_artifacts(id),
    media_type          TEXT,
    -- Denormalised from transfers so the dequeue path needs no join.
    source_repo_id      BIGINT      NOT NULL REFERENCES repositories(id),
    target_repo_id      BIGINT      NOT NULL REFERENCES repositories(id),

    state               TEXT        NOT NULL DEFAULT 'pending'
                          CHECK (state IN ('blocked','pending','leased','succeeded',
                                           'skipped','failed','cancelled')),
    -- Why no bytes moved, when state='skipped' (feeds dedupe metrics, 12 section 2).
    skip_reason         TEXT        CHECK (skip_reason IN
                                      ('placement_hit','exists_at_target','mounted')),

    wave                SMALLINT    NOT NULL DEFAULT 0,
    priority            SMALLINT    NOT NULL DEFAULT 50,
    -- Denormalised from transfers.state so pause is a bulk UPDATE and the hot
    -- index stays join-free (04 section 8).
    paused              BOOLEAN     NOT NULL DEFAULT FALSE,

    attempts            SMALLINT    NOT NULL DEFAULT 0,
    max_attempts        SMALLINT    NOT NULL DEFAULT 8,
    next_visible_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error          TEXT,
    last_error_class    TEXT,       -- see 11 section 2.3

    lease_owner         TEXT,       -- worker ID
    lease_expires_at    TIMESTAMPTZ,

    bytes_transferred   BIGINT      NOT NULL DEFAULT 0,
    -- Resumable-upload state: {"location": "...", "offset": 12345}. NULL when
    -- the registry does not support resume, or on the monolithic path (05 section 4.6).
    upload_state        JSONB,

    started_at          TIMESTAMPTZ,
    completed_at        TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- No duplicate work within a transfer.
    UNIQUE (transfer_id, kind, digest)
);
```

**Indexes, and why each exists:**

```sql
-- THE hot index. Partial, so it holds only leasable work: a queue with ten
-- million completed rows still has a small index. Column order matches the
-- dequeue ORDER BY exactly, so the planner does an index scan with no sort.
-- Note there is no `wave` column here: wave gating is resolved into the state
-- column ('blocked' vs 'pending') at plan and wave-advance time, so the
-- dequeue never needs it. See 04 section 3.3.
CREATE INDEX jobs_dequeue_idx
    ON jobs (priority DESC, next_visible_at, id)
    WHERE state = 'pending' AND NOT paused;

-- Reaper: find expired leases (04 section 4).
CREATE INDEX jobs_lease_expiry_idx
    ON jobs (lease_expires_at)
    WHERE state = 'leased';

-- Concurrent duplicate suppression: is this exact blob already being moved to
-- this exact repository right now? (04 section 5)
CREATE INDEX jobs_inflight_blob_idx
    ON jobs (target_repo_id, digest)
    WHERE state = 'leased';

-- Progress rollups and wave advancement, per transfer.
CREATE INDEX jobs_transfer_state_idx ON jobs (transfer_id, state);

-- Retention GC (section 8).
CREATE INDEX jobs_completed_at_idx ON jobs (completed_at)
    WHERE state IN ('succeeded','skipped','failed','cancelled');
```

> **On the partial index.** This is the difference between a queue that stays fast and one that degrades. Without `WHERE state = 'pending'`, the index carries every job ever created and grows without bound; every dequeue pays for entries that can never match. With it, index size tracks *outstanding work*, which is bounded by what is actually in flight. Detail in [04](04-queue-and-scheduling.md) §3.3.

### 6.2 Scheduled requests

```sql
CREATE TABLE scheduled_requests (
    id              UUID        PRIMARY KEY,
    request_id      UUID        NOT NULL REFERENCES transfer_requests(id),
    execute_at      TIMESTAMPTZ NOT NULL,
    state           TEXT        NOT NULL DEFAULT 'scheduled'
                      CHECK (state IN ('scheduled','due','expanded','cancelled','failed')),
    expanded_at     TIMESTAMPTZ,
    failure_reason  TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX scheduled_requests_due_idx
    ON scheduled_requests (execute_at)
    WHERE state = 'scheduled';
```

Separate from `jobs` on purpose: **the queue contains only executable work**, as required. A request scheduled for next Tuesday is not work; it is an appointment. Keeping it out of `jobs` keeps the dequeue index small and means "queue depth" means what an operator thinks it means. See [04](04-queue-and-scheduling.md) §10.

## 7. Verification, notification, audit, workers

```sql
CREATE TABLE verifications (
    id              BIGSERIAL   PRIMARY KEY,
    package_id      BIGINT      NOT NULL REFERENCES packages(id),
    transfer_id     UUID        REFERENCES transfers(id),   -- NULL for on-demand
    repository_id   BIGINT      NOT NULL REFERENCES repositories(id),
    stage           TEXT        NOT NULL
                      CHECK (stage IN ('source','destination','on_demand')),
    state           TEXT        NOT NULL DEFAULT 'pending'
                      CHECK (state IN ('pending','running','passed','failed','error','skipped')),
    policy          TEXT        NOT NULL CHECK (policy IN ('enforce','warn')),
    subject_digest  TEXT        NOT NULL,
    -- Per-artifact outcomes, so a partial failure names the offending image.
    details         JSONB,
    failure_reason  TEXT,
    started_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX verifications_package_idx ON verifications (package_id, created_at DESC);

-- Transactional outbox: a notification is committed in the SAME transaction as
-- the state change that caused it, so we can never notify about something that
-- did not happen, nor silently fail to notify about something that did.
CREATE TABLE notifications (
    id              BIGSERIAL   PRIMARY KEY,
    product_id      BIGINT      NOT NULL REFERENCES products(id),
    event_type      TEXT        NOT NULL,
    channel_name    TEXT        NOT NULL,
    channel_type    TEXT        NOT NULL CHECK (channel_type IN ('email','teams')),
    subject_kind    TEXT        NOT NULL,   -- package | transfer | verification
    subject_id      TEXT        NOT NULL,
    payload         JSONB       NOT NULL,
    state           TEXT        NOT NULL DEFAULT 'pending'
                      CHECK (state IN ('pending','sending','sent','failed','suppressed')),
    attempts        SMALLINT    NOT NULL DEFAULT 0,
    max_attempts    SMALLINT    NOT NULL DEFAULT 5,
    next_visible_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error      TEXT,
    -- Idempotency: one notification per (event, subject, channel), so a retried
    -- state transition cannot double-send.
    dedupe_key      TEXT        NOT NULL,
    sent_at         TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (dedupe_key)
);
CREATE INDEX notifications_outbox_idx
    ON notifications (next_visible_at, id) WHERE state = 'pending';

-- Append-only. Independent of application logs, queryable via the API.
CREATE TABLE audit_events (
    id              BIGSERIAL,
    occurred_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    event_type      TEXT        NOT NULL,
    actor           TEXT        NOT NULL DEFAULT 'system',
    actor_kind      TEXT        NOT NULL DEFAULT 'system'
                      CHECK (actor_kind IN ('user','system','worker','schedule','auto_rule')),
    product_name    TEXT,
    subject_kind    TEXT,
    subject_id      TEXT,
    request_id      TEXT,          -- correlates to X-Request-Id (12 section 6)
    trace_id        TEXT,
    outcome         TEXT NOT NULL DEFAULT 'success'
                      CHECK (outcome IN ('success','failure')),
    detail          JSONB,
    PRIMARY KEY (id, occurred_at)
) PARTITION BY RANGE (occurred_at);

CREATE INDEX audit_events_subject_idx ON audit_events (subject_kind, subject_id, occurred_at DESC);
CREATE INDEX audit_events_product_idx  ON audit_events (product_name, occurred_at DESC);
CREATE INDEX audit_events_type_idx     ON audit_events (event_type, occurred_at DESC);

-- Worker registry. Advisory: the authoritative liveness signal is the lease on
-- a job, not a row here. This table exists for `transferctl workers`, for HPA
-- budget division, and for log routing.
CREATE TABLE workers (
    id                  TEXT        PRIMARY KEY,     -- pod name
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

-- Adaptive concurrency state, per (repository, direction). Persisted so a
-- Coordinator restart resumes at the learned value rather than re-probing a
-- vendor registry from scratch (11 section 3.4).
CREATE TABLE repository_budgets (
    repository_id       BIGINT      NOT NULL REFERENCES repositories(id),
    direction           TEXT        NOT NULL CHECK (direction IN ('upload','download')),
    configured_max      SMALLINT    NOT NULL,
    current_limit       SMALLINT    NOT NULL,
    observed_p95_ms     INTEGER,
    error_rate_ppm      INTEGER,
    last_adjusted_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (repository_id, direction)
);

-- Recent worker log lines, shipped on the control channel so `transferctl logs`
-- is served by the Coordinator and the CLI never contacts a worker (00 section 5.3).
-- Deliberately small and aggressively GC'd -- this is a convenience tail, not a
-- log store. Cluster log aggregation remains the system of record.
CREATE TABLE worker_logs (
    id          BIGSERIAL   PRIMARY KEY,
    worker_id   TEXT        NOT NULL,
    job_id      BIGINT,
    transfer_id UUID,
    level       TEXT        NOT NULL,
    message     TEXT        NOT NULL,
    attrs       JSONB,
    logged_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX worker_logs_transfer_idx ON worker_logs (transfer_id, logged_at DESC);
CREATE INDEX worker_logs_logged_at_idx ON worker_logs (logged_at);
```

## 8. Retention and garbage collection

Configurable per class ([02](02-configuration.md) §4, §8). Run by the leader on an hourly tick.

| Class | Table | Default | Mechanism |
|---|---|---|---|
| Completed jobs | `jobs` | 7 d | Batched `DELETE` where terminal and `completed_at` older than retention |
| Queue history | `transfers`, `transfer_requests` | 7 d | Batched `DELETE`, cascading to jobs |
| Discovery history | `packages` | 90 d | Batched `DELETE` for `superseded`/`failed` only — **never** delete a package with transfer history |
| Notification history | `notifications` | 30 d | Batched `DELETE` where terminal |
| Audit history | `audit_events` | 365 d | **`DROP TABLE` on the whole partition** |

**Two properties GC must have, and how they are obtained:**

1. **GC must never stall transfers.** Deletes are batched (`batchSize`, default 5,000) with a bounded number of batches per tick and a commit between each. A single `DELETE FROM jobs WHERE completed_at < …` against ten million rows would hold locks and bloat WAL long enough to be an outage. Batching is not an optimization here; it is the difference between a maintenance task and an incident.
2. **Audit retention must be cheap.** Monthly `RANGE` partitions mean expiring a month is a `DROP TABLE` — instant, no row-by-row work, no bloat, no `VACUUM` debt. This is the single reason `audit_events` is partitioned; it is also the highest-volume table in the system. Partitions are created a month ahead by the same GC tick, so nothing depends on a human remembering.

## 9. Migrations

`goose`, embedded in the binary via `embed.FS`, applied by the Coordinator at startup under a `pg_advisory_lock` so that two replicas starting simultaneously cannot race.

Rules:
- **Forward-only in production.** Down migrations exist for local development and are not run against production.
- **Every migration must be safe to apply while the previous version is still running**, because a rolling deployment runs both. In practice: add columns nullable or with defaults, never rename in place (add, backfill, switch, drop across three releases), and never take an `ACCESS EXCLUSIVE` lock on `jobs` during business hours.
- Index creation on `jobs` uses `CREATE INDEX CONCURRENTLY` (outside a transaction; `goose` supports this with `-- +goose NO TRANSACTION`).
- Workers run no migrations and hold no schema knowledge — a direct consequence of the HTTP-leasing decision ([00](00-overview.md) §5.2).

## 10. Sizing

For a deployment replicating 20 products, each ~8 packages a month, each package ~1,000 blobs, into 2 targets:

| Table | Rows/month | Note |
|---|---|---|
| `packages` | ~160 | Trivial |
| `package_artifacts` | ~1 k | Trivial |
| `blobs` | ~100 k | Grows, but dedupes heavily across versions |
| `blob_placements` | ~200 k | The valuable one; keep it |
| `jobs` | **~320 k** | Dominant; GC'd at 7 d, so steady state ~75 k |
| `audit_events` | ~1 M | Partitioned; one partition dropped per month at 365 d |

Steady state is a few GB. This is a small database, and that is the point: PostgreSQL is not being asked to do anything difficult, which is why it is sufficient (§1).
