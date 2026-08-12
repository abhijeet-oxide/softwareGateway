# 04 — Queue and Scheduling

> **Prerequisite:** [03 — Persistence](03-persistence.md) · **Consumed by:** [05](05-transfer-engine.md), [09](09-api.md), [10](10-state-machines.md), [11](11-resiliency-and-backpressure.md)

The heart of the system. Everything here serves two properties: **a package saturates the whole fleet**, and **nothing is lost when anything dies**.

---

## 1. Why a database table is the right queue

> **Decision — the queue is a PostgreSQL table consumed with `FOR UPDATE SKIP LOCKED`.**
>
> *Alternatives:* Kafka (durable log, high throughput, consumer groups); RabbitMQ (mature broker, native priorities and DLQ); Redis lists or streams (fast, simple).
>
> *Rejected because* our queue volume is trivial and our consistency requirement is not. A 60 GB package with 1,000 blobs generates **1,000 queue operations** — the entire monthly volume of a busy deployment is a few hundred thousand operations, which Postgres handles without noticing. Meanwhile every broker forces the same problem: a job's state lives in the broker and the transfer's state lives in the database, so "job succeeded" and "transfer progressed" cannot be made atomic without an outbox or a reconciler. In one database, that is one `COMMIT`.
>
> Brokers are also a poor fit for what we actually need: **mutable queue entries.** Change a job's priority, pause a transfer, cancel 900 jobs at once, requeue everything a dead worker held. In SQL these are one `UPDATE` each. In Kafka they range from awkward to impossible — a log is not designed to be edited.
>
> *What would change our mind:* sustained rates beyond ~10 k jobs/s, or consumers outside this system needing the same stream.

## 2. Job granularity

**A worker never transfers a package. It transfers one blob or pushes one manifest.**

This single choice produces most of the system's good properties:

| Property | Why it follows |
|---|---|
| One package saturates the fleet | 1,000 independent jobs distribute across every worker; a package-level job would pin to one process |
| Failure blast radius is one blob | A network blip costs a retry of ~50 MB, not a restart of 60 GB |
| Workers are trivially stateless | A job is self-contained: two repository references, a digest, a size |
| Scaling needs no rebalancing | Add or remove workers freely; leases handle the rest |
| Progress is exact | `SUM(bytes_transferred)` over jobs — a derived value cannot drift (invariant I6) |
| Deduplication is natural | The unit of work *is* the unit of content addressing |

Package-level progress is always a rollup, never a maintained counter ([01](01-domain-model.md) §3.3).

## 3. Ordering: waves

### 3.1 The constraint

OCI requires that a manifest's referenced blobs exist before the manifest is pushed. Push an index before its image manifests and the registry rejects it — or worse, accepts it and leaves a tag resolving to missing content.

**Invariant I1: a tag never appears at the destination until everything under it is present.** This is what makes an interrupted transfer safe: a consumer either sees the old tag or the complete new one, never a half-written one.

### 3.2 Waves, not a dependency graph

> **Decision — job ordering is a single `wave SMALLINT` per job, not a dependency-edge table.**
>
> *Alternative:* a general DAG with a `job_dependencies(job_id, depends_on_job_id)` table, resolving readiness by counting unmet predecessors.
>
> *Rejected because* it is a general solution to a specific, shallow, regular problem. An OCI package tree is: blobs at the bottom, image and chart manifests above them, an index on top. Depth is 2–3 and known at planning time. A DAG buys arbitrary-shape support we will never use, and costs an edge table (potentially ~1,000 rows per package), a join on the hot path, and a readiness computation on every completion.
>
> *Chosen:* topological depth becomes an integer.
>
> | Wave | Contents |
> |---|---|
> | 0 | All blobs (config blobs and layers) |
> | 1 | Image manifests, Helm chart manifests, config-bundle manifests |
> | 2 | The top-level index |
> | 3+ | Deeper nesting, if a vendor ever ships it — `depth` in `package_artifacts` generalizes |
>
> Correctness is identical for the tree shapes OCI produces. Cost is one column.

### 3.3 Gating without a join

Jobs in wave 0 are created `pending`. Jobs in waves ≥ 1 are created **`blocked`**.

When a wave drains, the Coordinator promotes the next:

```sql
-- Advance transfer to the next wave: one bulk UPDATE, no per-job dependency
-- resolution. Runs when the wave-drain check (section 3.4) fires.
WITH advanced AS (
    UPDATE transfers
       SET current_wave = current_wave + 1, updated_at = now()
     WHERE id = $1 AND current_wave < max_wave
    RETURNING id, current_wave
)
UPDATE jobs j
   SET state = 'pending', updated_at = now()
  FROM advanced a
 WHERE j.transfer_id = a.id
   AND j.wave = a.current_wave
   AND j.state = 'blocked';
```

**This is the reason the dequeue path needs no join.** A job is leasable if and only if `state = 'pending'`; the wave logic has already been resolved into that column. Had we filtered `j.wave = t.current_wave` at dequeue time, every lease would join `transfers` and the hot index could not be a clean match for the `ORDER BY`.

### 3.4 Detecting a drained wave

After each job completion, in the same transaction:

```sql
SELECT count(*) = 0 AS wave_drained
  FROM jobs
 WHERE transfer_id = $1
   AND wave = $2
   AND state NOT IN ('succeeded','skipped');
```

Served by `jobs_transfer_state_idx`. If drained and `current_wave < max_wave`, advance (§3.3). If drained at `max_wave`, the transfer moves to `verifying` or `succeeded` ([10](10-state-machines.md) §3).

A `failed` job (terminal, attempts exhausted) never satisfies the drain check, so the transfer correctly stalls rather than pushing a manifest whose blobs are missing. The transfer then fails, which is the right outcome — see [11](11-resiliency-and-backpressure.md) §2.6.

## 4. Leasing

### 4.1 The dequeue statement

Workers do not run this — they call `POST /api/v1/jobs:lease` and the Coordinator runs it ([09](09-api.md) §7). One statement, one round trip:

```sql
-- Lease up to $4 jobs for worker $3.
-- $1 / $2: repository IDs with download / upload budget available (section 6).
WITH candidate AS (
    SELECT id
      FROM jobs
     WHERE state = 'pending'
       AND NOT paused
       AND next_visible_at <= now()
       AND source_repo_id = ANY($1::bigint[])
       AND target_repo_id = ANY($2::bigint[])
       -- Suppress concurrent duplicate work: if another worker is already
       -- moving this exact blob to this exact repository, skip it. Section 5.
       AND NOT EXISTS (
             SELECT 1 FROM jobs inflight
              WHERE inflight.state = 'leased'
                AND inflight.target_repo_id = jobs.target_repo_id
                AND inflight.digest = jobs.digest
           )
     ORDER BY priority DESC, next_visible_at, id
       FOR UPDATE SKIP LOCKED
     LIMIT $4
)
UPDATE jobs j
   SET state            = 'leased',
       lease_owner      = $3,
       lease_expires_at = now() + $5::interval,
       attempts         = attempts + 1,
       started_at       = COALESCE(j.started_at, now()),
       updated_at       = now()
  FROM candidate c
 WHERE j.id = c.id
RETURNING j.id, j.transfer_id, j.kind, j.digest, j.size_bytes,
          j.source_repo_id, j.target_repo_id, j.media_type,
          j.artifact_id, j.attempts, j.upload_state, j.lease_expires_at;
```

**`SKIP LOCKED` is what makes this safe under concurrency.** Without it, ten workers leasing simultaneously would serialize behind row locks. With it, each takes a different set and none waits. This is the well-trodden Postgres queue pattern, not something invented here.

`attempts` is incremented **at lease time, not on failure**. A worker that dies without reporting anything has still consumed an attempt, so a job that reliably kills its worker cannot loop forever. Counting only reported failures would make a poison job immortal.

### 4.2 Index support, honestly

The supporting index ([03](03-persistence.md) §6.1):

```sql
CREATE INDEX jobs_dequeue_idx
    ON jobs (priority DESC, next_visible_at, id)
    WHERE state = 'pending' AND NOT paused;
```

Column order matches `ORDER BY` exactly, so the planner scans the index in order and stops at `LIMIT` — no sort, no heap scan of the whole queue. The partial predicate keeps the index sized to *outstanding work* rather than all work ever.

**The honest caveat:** `source_repo_id`/`target_repo_id` are not in the index, so they are filtered after the index scan. If a large backlog is concentrated on repositories that are all at their concurrency ceiling, a lease may scan many non-matching entries before finding leasable work. In normal operation the pending set is small and this is free.

*If it becomes a problem*, the escalation is a second partial index leading with `target_repo_id`:

```sql
CREATE INDEX jobs_dequeue_by_target_idx
    ON jobs (target_repo_id, priority DESC, next_visible_at, id)
    WHERE state = 'pending' AND NOT paused;
```

and issuing one lease query per budgeted repository instead of an `ANY` array. This trades a single round trip for several. **We start with the simple version and add this only if measurement demands it** — `softwaregateway_queue_lease_duration_seconds` ([12](12-observability-and-audit.md) §2) is the trigger.

### 4.3 Lease renewal and expiry

A leased job carries `lease_expires_at`, default 2 minutes. The worker's heartbeat (every 20 s) renews the leases it holds:

```sql
UPDATE jobs
   SET lease_expires_at = now() + $3::interval, updated_at = now()
 WHERE lease_owner = $1 AND id = ANY($2::bigint[]) AND state = 'leased';
```

The reaper, on the leader, every 30 s:

```sql
UPDATE jobs
   SET state = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'pending' END,
       lease_owner = NULL,
       lease_expires_at = NULL,
       next_visible_at = now() + LEAST(300, POWER(2, attempts)) * INTERVAL '1 second',
       last_error = COALESCE(last_error, 'lease expired'),
       last_error_class = 'lease_expired',
       updated_at = now()
 WHERE state = 'leased' AND lease_expires_at < now()
RETURNING id, transfer_id, state;
```

Served by `jobs_lease_expiry_idx`.

> **This is the entire worker crash-recovery story.** A worker that is `SIGKILL`ed, whose node is preempted, or which is partitioned from the network performs no cleanup, sends no message, and runs no shutdown hook. Its work returns to the queue within one lease period. There is no handshake to get wrong, no tombstone to leak, and no difference in handling between "crashed", "network-partitioned", and "scaled down" — the only signal is a timestamp that stopped advancing.
>
> The **only** correctness requirement is that a worker's in-flight upload must not corrupt anything after its lease expires and another worker retakes the job. It cannot: OCI blob uploads are digest-verified by the registry on completion, so a stale worker finishing late either writes identical bytes or is rejected. Content addressing makes double-execution harmless, which is why leases are safe here and would not be for a non-idempotent workload.

## 5. Concurrent duplicate suppression

Two transfers can need the same blob in the same target simultaneously (two packages sharing a base layer, replicated at once). Both jobs are independently leasable, so both could move the same 200 MB.

This is **wasteful, not incorrect** — the registry is content-addressed, so the second upload is a no-op or an identical overwrite. The mitigation is therefore sized to the problem: a `NOT EXISTS` check in the dequeue (§4.1) skips a job whose digest is already `leased` to the same **target registry** by someone.

```sql
CREATE INDEX jobs_inflight_blob_idx
    ON jobs (target_repo_id, digest) WHERE state = 'leased';
```

The skipped job stays `pending` and is picked up moments later — by which time the first has completed and written a `blob_placements` row, so the second either hits the placement fast path (same repository) or mounts from the first's repository (a sibling one), and moves zero bytes either way ([05](05-transfer-engine.md) §4.1–4.2).

**Why the key is the registry and not the repository.** The check was originally `(target_repo_id, digest)`, which is the narrowest thing that suppresses a true duplicate — and it missed the case that costs the most. A bundle's components are published in two destination repositories, so one digest yields two jobs; they are created consecutively, so they have adjacent ids, so they land in the **same lease batch** and both stream from the vendor at once. Neither has placed anything for the other to use. Widening the key to the registry is what serialises them, and serialising them is what turns the second into a mount. It costs a join against `repositories` inside the `NOT EXISTS`, which runs only over the handful of digests actually in flight.

> **Why not an advisory lock or a claims table.** A transaction-scoped advisory lock releases at `COMMIT` of the *lease* transaction, long before the transfer finishes, so it would not actually cover the window. A session-scoped lock or a dedicated claims table would cover it, at the cost of another lifecycle to leak on worker death. Given the failure mode is *duplicated bandwidth*, not *corruption*, a `NOT EXISTS` on an index is the proportionate answer.

## 6. Priority, and the starvation problem

`priority SMALLINT`, 0–1000, default 50. Higher first, FIFO within a band (`ORDER BY priority DESC, next_visible_at, id`).

Priority is set on the request, inherited by transfers and jobs, and changed at any time via `transfers:setPriority` ([09](09-api.md) §5) — a bulk `UPDATE` over the transfer's pending jobs. **A priority change affects only jobs not yet leased**; in-flight jobs run to completion. Preempting a 40 GB blob at 90% to start a higher-priority one would throw away more work than the reordering could recover.

> **Strict priority can starve low-priority work, and this design accepts that by default.** A continuous stream of priority-100 work will indefinitely delay priority-10 work. This is stated rather than hidden, because it is a real operational hazard: an auto-download rule set to priority 100 that matches every nightly build will quietly stall everything else.
>
> *Mitigations, in the order we would apply them:*
> 1. **Observe it.** `softwaregateway_queue_oldest_pending_age_seconds{priority_band}` ([12](12-observability-and-audit.md) §2) makes starvation visible. Alert on it. This alone resolves most real cases, because starvation is nearly always a misconfigured rule.
> 2. **Aging** (implemented, off by default). A leader task periodically raises the effective priority of jobs pending beyond a threshold:
>    ```sql
>    UPDATE jobs SET priority = LEAST(priority + $1, $2)
>     WHERE state = 'pending' AND NOT paused
>       AND next_visible_at < now() - $3::interval;
>    ```
>    Off by default because it makes priority non-deterministic, which is worse than starvation when starvation is not actually occurring.
> 3. Weighted fair queueing across products was considered and rejected: it requires per-product accounting on the hot path to solve a problem that item 1 usually reveals to be a configuration error.

## 7. Idempotency

Required for discovery, download, promotion, verification, retry, and notification. The approach throughout is **structural** — a unique constraint, not application logic. Application-level "check then insert" has a race; a unique index does not.

| Operation | Mechanism |
|---|---|
| Discovery | `UNIQUE (source_repo_id, tag, manifest_digest)` + `ON CONFLICT DO NOTHING` |
| Transfer request | `UNIQUE (idempotency_key)`; a repeat returns the original request, `200` not `201` |
| Request expansion | `UNIQUE (request_id, target_repo_id)` on `transfers` |
| Job creation | `UNIQUE (transfer_id, kind, digest)` |
| Blob transfer | Content addressing: an already-present blob is `skipped` ([05](05-transfer-engine.md) §4.1) |
| Manifest push | `PUT` of an identical manifest to the same digest is a no-op at the registry |
| Retry | Resumes from `bytes_transferred`/`upload_state` rather than restarting |
| Notification | `UNIQUE (dedupe_key)` on the outbox |

**Idempotency key derivation.** Client-supplied via the `Idempotency-Key` header. When absent, the Coordinator derives one:

```
sha256(operation | package_id | sorted(target_repo_ids) | scheduled_at | priority)
```

So an auto-download rule that fires twice for the same package — because discovery ran twice, or a Coordinator restarted mid-expansion — produces one request, not two. Note that `priority` participates: deliberately re-requesting the same transfer at a *different* priority is a distinct intent and is allowed.

## 8. Pause, resume, cancel

Operate on a **Transfer**; jobs follow in bulk.

```sql
-- Pause. Denormalising into jobs.paused keeps the dequeue index join-free
-- (section 4.2) at the cost of one bulk UPDATE here. The right trade: pausing
-- is rare, leasing is constant.
UPDATE transfers SET state = 'paused', updated_at = now()
 WHERE id = $1 AND state IN ('ready','running');
UPDATE jobs SET paused = TRUE, updated_at = now()
 WHERE transfer_id = $1 AND state IN ('pending','blocked');
```

Resume is the inverse. Cancel:

```sql
UPDATE transfers SET state = 'cancelling', updated_at = now()
 WHERE id = $1 AND state NOT IN ('succeeded','failed','cancelled');
UPDATE jobs SET state = 'cancelled', updated_at = now(), completed_at = now()
 WHERE transfer_id = $1 AND state IN ('pending','blocked');
```

**Semantics that must be explicit:**

- **Pause does not stop in-flight jobs.** Leased jobs finish. Pausing a transfer with 16 blobs in flight completes those 16, then stops. Killing in-flight transfers to honour a pause instantly would discard partial work for no benefit; the operator wants the transfer to *stop consuming more*, which it does.
- **Cancel is cooperative for leased jobs.** Pending and blocked jobs are cancelled immediately. Leased jobs learn of the cancellation at their next heartbeat and abort the stream ([09](09-api.md) §7.4). The transfer sits in `cancelling` until they drain, then becomes `cancelled`. A distinct `cancelling` state exists precisely so this window is visible rather than looking like a stuck cancel.
- **Cancel does not roll back.** Blobs already at the destination stay. They are valid content, correctly digest-addressed, and useful to the next transfer via `blob_placements`. There is no tag pointing at them (invariant I1), so nothing is exposed. Deleting them would be slow, would risk removing blobs another package legitimately needs, and would throw away work.

## 9. Leader election

Multiple Coordinator replicas serve the API. Exactly one runs the background loops: discovery, scheduler, reaper, wave advancement, outbox, GC, budget controller.

> **Decision — leader election via `pg_advisory_lock`, not the Kubernetes Lease API.**
>
> *Alternative:* `client-go`'s `leaderelection` package on a `coordination.k8s.io/Lease`.
>
> *Chosen:* the advisory lock. We already have a mandatory, highly-available Postgres connection; the lock is one call, needs no RBAC, no client-go, and — the property that matters — **works identically in local development**, where there is no cluster. It also fails in the right direction: if Postgres is unreachable, the leader cannot function anyway, so tying leadership to the database avoids a split state where a leader exists but cannot act.

```go
// Session-scoped: held for the connection's lifetime, released automatically
// if the connection drops or the process dies. No lease renewal to get wrong.
SELECT pg_try_advisory_lock($1)
```

A non-leader retries every 10 s. On loss of leadership the Coordinator stops its loops within one tick but keeps serving the API. **Failover safety** does not depend on election being instantaneous: every background loop is idempotent, so a brief period with two leaders duplicates work without corrupting anything — a double reaper run requeues already-requeued jobs, a double discovery pass hits `ON CONFLICT DO NOTHING`.

## 10. Scheduling

> **Requirement: the queue contains only executable work.** A download scheduled for next Tuesday must not sit in `jobs` for six days.

Scheduled requests live in `scheduled_requests` ([03](03-persistence.md) §6.2) and are **expanded into jobs only when due**.

```
transferctl schedule --at 2026-08-11T02:00:00Z
        │
        ▼
  transfer_requests (state='pending')  +  scheduled_requests (state='scheduled')
        │
        │   ... six days pass. jobs table is untouched. ...
        │
        ▼   scheduler tick, every 10 s:
  SELECT ... WHERE state='scheduled' AND execute_at <= now()
             ORDER BY execute_at FOR UPDATE SKIP LOCKED LIMIT 100
        │
        ▼
  PLAN (05 section 3) -> create transfers -> create jobs -> state='expanded'
```

Three consequences, all of them the point:

1. **Queue depth means what an operator thinks it means.** `queue_pending_jobs` counts work that could run right now — which is what HPA scales on ([09](09-api.md) §9). Scheduled work in the queue would make HPA spin up workers for jobs that are not due.
2. **Planning happens at execution time, not at scheduling time.** A transfer scheduled a week out is planned against the registry state on the night it runs, so blobs replicated in the meantime are correctly deduplicated. Planning at schedule time would produce a stale plan.
3. **The dequeue index stays small** ([03](03-persistence.md) §6.1).

Missed windows: if the Coordinator was down at the due time, the request is expanded at the next tick after recovery — `execute_at <= now()` is a threshold, not an equality, so lateness never causes a skip. A `maxDelay` may be set per request, beyond which the request fails rather than running unexpectedly late.

## 11. Retry

`attempts` and `next_visible_at` on the job. **Exponential backoff with full jitter:**

```
delay = random(0, min(base * 2^attempts, cap))     base = 1s, cap = 5m
```

Full jitter rather than plain exponential because the common failure is *correlated*: a registry returning 503 fails all 40 in-flight jobs at once. Without jitter they all retry together, re-hammering an already-struggling registry in synchronized waves. Full jitter spreads them. This is a well-established result and not worth re-deriving here.

Attempt caps by error class ([11](11-resiliency-and-backpressure.md) §2.3): transient network/5xx/429 get the full 8 attempts; a digest mismatch gets 2 (bytes are wrong, so retrying rarely helps and may indicate corruption worth surfacing); a 401/403 gets 1 (credentials will not fix themselves, and hammering an auth endpoint gets us rate-limited).

**Retries resume, they do not restart.** `bytes_transferred` and `upload_state` persist across attempts, so a blob interrupted at 80% resumes at 80% where the registry supports it ([05](05-transfer-engine.md) §4.6).

Exhausted jobs become `failed` — terminal, and visible:

```sql
CREATE VIEW dead_letter_jobs AS
SELECT j.*, t.package_id, p.name AS product_name
  FROM jobs j
  JOIN transfers t ON t.id = j.transfer_id
  JOIN products p  ON p.id = (SELECT product_id FROM packages WHERE id = t.package_id)
 WHERE j.state = 'failed';
```

Surfaced by `transferctl transfers describe` and retried in bulk with `transfers:retry`, which resets `attempts` to 0 and `state` to `pending` for failed jobs only.

**And the transfer above them has to say it has stopped.** A failed job is terminal, and the wave-drain check counts only `succeeded` and `skipped` (§3.4) — so one exhausted job means the wave never drains and the transfer never advances. That is correct about the data and was, for a while, silent: nothing moved the *transfer* to `failed` on account of its jobs, so a transfer whose every job had died went on reporting `running` with nothing in flight indefinitely. The settle check ([10](10-state-machines.md) §3) closes it, on the completion path and on the reaper's tick — the latter because an outage produces its failures through lease expiry, where no completion is ever reported.

## 12. Recovery after restart

Nothing is held in Coordinator memory that cannot be reconstructed from the database. On startup:

1. Run migrations under an advisory lock ([03](03-persistence.md) §9).
2. Load and validate configuration ([02](02-configuration.md) §7).
3. Contend for leadership (§9).
4. On acquiring it, run the reaper immediately — leases held by workers that died during the outage are requeued at once rather than after a full lease period.
5. Resume loops.

**No recovery scan, no journal replay, no rebuild.** The queue state *is* the `jobs` table; it was never anywhere else. A Coordinator that has been down for an hour starts up and continues, and workers that stayed alive throughout kept transferring the jobs they had already leased — bytes do not flow through the Coordinator, so its absence does not stop work already in flight.

## 13. Scheduling: what is measured, and what is left

Written against a real 63.7 GiB ORB moving over a 917 ms link through a corporate proxy, because every number below came from that run rather than from reasoning.

### 13.1 What actually limits throughput

**One stream is worth about 280 KiB/s on that path, and no amount of tuning changes it.** A TCP stream cannot exceed its window divided by the round trip; at 917 ms with the window a proxy typically allows, that is the arithmetic. The observed per-stream rate was 274 KiB/s. So aggregate throughput is *almost exactly* concurrency × 280 KiB/s, and **the only lever that matters is how many jobs are in flight**.

That reframes everything. Buffer sizes, compression, chunk sizes — none of them move a number set by window ÷ RTT. Keeping the pipe full does.

### 13.2 The global wave barrier is stronger than the invariant requires

Invariant I1 is per manifest: *this* manifest may be pushed once every blob and child manifest *it* references is present. The implementation is a global barrier — wave N+1 opens only when wave N has drained entirely (§3.3) — which is a much stronger statement than the invariant makes.

The cost is **not** bandwidth. Manifests are kilobytes; pushing them earlier moves no bytes sooner. The cost is:

- **Nothing is visible at the destination until the very end.** A transfer 99% done delivers exactly nothing, because no tag has been written.
- **One failed blob blocks everything.** 517 manifests whose content is fully present cannot be written because two unrelated blobs have not landed.
- **A partial transfer has no durable output** beyond blobs that are only reusable through placement records.

The fix is per-artifact readiness: a `job_dependencies` edge table written at plan time, and a manifest promoted from `blocked` to `pending` when none of its dependencies is outstanding. The wave column becomes advisory ordering rather than a gate. This is a change to the invariant everything else rests on, so it is written down here before it is written in code.

### 13.3 Largest-first ordering conflicts with the mount, and the mount wins

The dequeue orders by insertion, which is arbitrary with respect to size: a multi-gigabyte layer leased last runs alone while every other slot idles. Largest-first (LPT) is the textbook fix and was implemented, measured, and **reverted**.

A bundle publishes each component twice — inside the bundle and under its own name — so one digest becomes two jobs of *identical size*. The planner emits them grouped by destination repository, which puts them far apart in id order, and that separation is load-bearing: the two land in different lease batches, the first writes a placement, and the second mounts inside the destination registry for zero bytes. Ordering by size makes equal-sized jobs adjacent, so both copies land in one batch and both stream from the vendor. Measured on the ORB fixture: **16 vendor GETs for 11 distinct blobs, and no mounts at all.**

Makespan is worth single-digit percent at the tail. The mount is worth up to half the bytes of the entire transfer.

To have both, the planner must rank the second site explicitly — a `site_rank` on the job, 0 for the copy that keeps the bundle resolvable and 1 for the copy published under the component's own name — so the dequeue can order by `site_rank, size DESC` and every rank-1 job runs after the placement it will mount from exists. That is strictly better than today, where the mount depends on batch timing rather than on an ordering the schema states.
