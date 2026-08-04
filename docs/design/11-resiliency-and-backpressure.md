# 11 — Resiliency and Backpressure

> **Prerequisites:** [04 — Queue and Scheduling](04-queue-and-scheduling.md), [05 — Transfer Engine](05-transfer-engine.md)

> *"The application should behave like a cockroach. Even after complete disruption it should recover automatically and continue processing."*

This document states exactly what "complete disruption" means, what happens in each case, and how it is tested.

---

## 1. The recovery principle

**Recovery is not a feature; it is the absence of state that needs recovering.**

Everything here follows from three structural choices made elsewhere:

| Choice | Consequence |
|---|---|
| Workers hold no state ([00](00-overview.md) §5.2) | A dead worker has nothing to recover. Its leases expire |
| The unit of work is a blob ([04](04-queue-and-scheduling.md) §2) | Any failure costs one blob, never a package |
| Queue state *is* database state ([03](03-persistence.md) §1) | There is no journal to replay and no cache to rebuild |

There is no recovery subsystem, no repair job, and no reconciliation loop, because there is nothing that can diverge. Almost every row in §2 resolves to "a timestamp stops advancing and the reaper requeues" — that repetition is the design working, not a gap in the analysis.

## 2. Failure taxonomy

### 2.1 Worker failures

| Failure | Detection | Recovery | Worst case | Bytes lost |
|---|---|---|---|---|
| Worker `SIGKILL` / OOM | Lease expiry | Reaper requeues; another worker takes over | 1 lease + 1 reaper tick ≈ 150 s | Partial blob only |
| Worker node preempted | Lease expiry | Same | Same | Partial blob only |
| Worker partitioned from Coordinator | Lease expiry | Same. The worker keeps transferring until its leases lapse, then idles and retries registration | Same | Usually none — in-flight work often completes |
| Worker scaled down (HPA) | `SIGTERM` → drain | Finishes in-flight jobs, stops leasing, exits within `terminationGracePeriodSeconds` | Graceful | None |
| Worker scaled down, grace exceeded | Lease expiry | Falls back to the crash path | 150 s | Partial blob only |
| Worker hangs (no crash, no progress) | No progress reports; heartbeat may continue | Stall detector aborts the job ([05](05-transfer-engine.md) §5); if the process is wedged, the liveness probe restarts it | ~2× stall timeout | Partial blob only |

**The important row is the last one.** A hung worker is worse than a dead one, because a dead worker's leases expire while a hung one may keep heartbeating and holding work. Two independent defences: a per-transfer **stall detector** (no bytes for `idleStall`, default 60 s → abort the job and report failure), and a **liveness probe** that fails if the worker's main loop stops ticking. Relying on lease expiry alone would let a wedged worker hold 16 jobs indefinitely.

> **Why double execution is harmless.** A worker declared dead may still be alive and mid-upload when another worker retakes its job. Both may write the same blob. This is safe because OCI blobs are content-addressed and the registry verifies the digest on commit: the loser writes identical bytes or is rejected. **Leases are safe here precisely because the workload is idempotent** — the same mechanism on a non-idempotent workload would require fencing tokens, and would be a much harder design.

### 2.2 Coordinator failures

| Failure | Detection | Recovery | Effect on in-flight transfers |
|---|---|---|---|
| Replica crash | Kubernetes | Restart; rejoin | **None** — bytes do not traverse the Coordinator |
| Leader crash | Advisory lock released with the connection | Other replica acquires within ~10 s, runs the reaper immediately | None |
| All replicas down | Readiness fails | Restart | Existing leases keep running to completion; **no new work is leased** |
| Rolling upgrade | — | Leader lock migrates | None — leases outlive a rolling restart by design |
| Deadlock / wedged | Liveness probe | Restart | None |

**This is the cost accepted in [00](00-overview.md) §5.2.** With a Coordinator outage longer than a lease period, workers finish what they hold and go idle. They do not crash, do not lose work, and resume leasing the moment the Coordinator returns. Two replicas plus a lease duration comfortably longer than a rolling restart make the window rare and harmless.

On regaining leadership the reaper runs **immediately** rather than waiting for its tick, so leases orphaned during the outage are requeued at once.

### 2.3 Registry failures

Classified once at the boundary ([06](06-registry-abstraction.md) §7); retry policy keys off the class ([10](10-state-machines.md) §6).

| Failure | Class | Attempts | Additional behaviour |
|---|---|---|---|
| 500/502/503/504 | `ErrUnavailable` | 8 | Full-jitter backoff; adaptive controller reduces concurrency (§3) |
| 429 | `ErrRateLimited` | 8 | **`Retry-After` honoured over our backoff**; strongest controller signal |
| Connection reset mid-blob | `ErrTimeout` | 8 | Resume from `upload_state` where supported ([05](05-transfer-engine.md) §4.6) |
| TLS failure | `ErrUnavailable` | 8 | Usually a CA misconfiguration — surfaced in `system:healthCheck` |
| 401/403 | `ErrUnauthorized`/`ErrForbidden` | 1 | Notification: needs a human |
| Source blob 404 mid-transfer | `ErrNotFound` | 1 | Vendor deleted content mid-transfer. Fail loudly |
| Digest mismatch | `ErrDigestMismatch` | 2 | Possible corruption; surfaced rather than absorbed |
| Total registry outage | `ErrUnavailable` | 8, then `failed` | Backoff to a 5 m cap; `transfers:retry` resumes when the vendor returns |

**A registry outage lasting longer than 8 backoffs (~20 minutes) fails the transfer**, and this is deliberate. An indefinitely retrying transfer holds queue slots, hides the problem behind a green dashboard, and eventually needs a human anyway. Failing makes it visible; `transfers:retry` resumes from exactly where it stopped, because completed jobs stay completed.

### 2.4 Database failures

| Failure | Behaviour |
|---|---|
| Brief unavailability | `pgx` pool retries; API returns `503` with `Retry-After`. **`/healthz` does not check the DB** ([09](09-api.md) §9.1), so no crash-loop |
| Failover to a replica | Connections drop and re-establish; in-flight transactions roll back. Job state reverts to its last committed value, so at worst a job is re-leased |
| Extended outage | Coordinator up but not ready; workers finish leases and idle. **No data loss** |
| Disk full | Writes fail; transfers stall. GC ([03](03-persistence.md) §8) is the preventative, and `pg_database_size` should be alerted on |

A rolled-back transaction can only lose the *record* of a completed job, causing it to be re-done. Re-doing a blob transfer is harmless (content-addressed), which is why at-least-once semantics are sufficient and exactly-once is not needed.

### 2.5 Consistency failures

| Failure | Detection | Recovery |
|---|---|---|
| **Stale placement** — we believe a blob is present, it is not | Manifest push returns `BLOB_UNKNOWN` | Invalidate placements for that manifest's blobs, requeue them, return the manifest job to `blocked` ([05](05-transfer-engine.md) §9) |
| Partial upload from a dead worker | Registry discards uncommitted sessions | Blob re-transferred |
| Tag re-pushed at source mid-transfer | New package row ([07](07-discovery.md) §4) | In-flight transfer completes against the **pinned digest** — never against a moving target |
| Blob deleted at destination between plan and push | `BLOB_UNKNOWN` | As stale placement |

The first row is the self-healing loop that makes the placement fast path safe rather than merely fast: **the registry itself tells us when our cache is wrong**, and the cost of being wrong is one requeued blob.

The third row matters more than it looks. Because a Package pins `manifest_digest` at discovery ([01](01-domain-model.md) §2.2), a vendor re-pushing a tag mid-transfer cannot cause us to replicate a mixture of two versions. We finish the version we started.

### 2.6 Poison jobs

A job that reliably kills its worker would, under naive handling, loop forever and take a worker down with it each time.

Prevented by incrementing `attempts` **at lease time** ([04](04-queue-and-scheduling.md) §4.1). A job that never reports anything still burns attempts and reaches `failed` after `max_attempts`. The transfer then fails rather than stalling, because a `failed` job never satisfies the wave-drain check ([04](04-queue-and-scheduling.md) §3.4) — the failure surfaces instead of becoming a permanently 97%-complete transfer.

`dead_letter_jobs` ([04](04-queue-and-scheduling.md) §11) makes these visible; `transferctl transfers describe` shows digest, attempts, error class, and last worker.

## 3. Adaptive backpressure

### 3.1 The problem

Static concurrency limits are wrong in both directions. Set for the worst case, they waste throughput most of the time. Set for the best case, they overwhelm a registry the moment it degrades — and the response to registry stress must be *to back off*, not to retry harder.

### 3.2 AIMD

> **Decision — AIMD (additive increase, multiplicative decrease) per `(repository, direction)`.**
>
> *Alternatives:* Vegas/gradient controllers (latency-gradient based, as in Netflix's `concurrency-limits`); a static configured limit; token buckets alone.
>
> *Rejected — static:* wrong in both directions, as above.
> *Rejected — gradient:* better steady-state utilization in theory, but it has more parameters, a longer convergence time, and behaviour that is genuinely hard to reason about at 3 a.m. when someone is asking why throughput dropped. Given priority 3 is simplicity and priority 4 is operability, a controller an on-call engineer can predict on a whiteboard beats one that is 10% more efficient.
> *Chosen — AIMD:* the same control law as TCP congestion control. Universally understood, provably converges to fairness, two parameters, and its behaviour is obvious from its name.

```
every controlInterval (default 30s), per (repository, direction):

    healthy  = p95_latency < latencyTarget
           AND error_rate  < errorThreshold
           AND no 429 in the window

    if healthy:  limit = min(limit + 1,          configuredMax)   # additive +1
    else:        limit = max(limit × 0.7, minLimit)               # multiplicative ×0.7

    minLimit = max(1, configuredMax / 8)
```

**A 429 forces a decrease regardless of latency.** It is the registry explicitly telling us to slow down — better information than any inference we could make from timing.

`configuredMax` from `rateLimits` ([02](02-configuration.md) §5.3) is a hard ceiling the controller never exceeds. **Configuration sets what is permitted; the controller decides what is safe right now.**

### 3.3 Signals

| Signal | Source | Effect |
|---|---|---|
| p95 request latency | Worker completion reports | Primary health signal |
| Error rate | Job outcomes by class | 5xx/timeouts trigger decrease |
| 429 count | Job outcomes | **Forces** decrease |
| `Retry-After` | Response headers | Overrides computed backoff |
| Worker CPU | Heartbeat ([09](09-api.md) §7.3) | Fleet-wide throttle above threshold |
| Worker memory | Heartbeat | Fleet-wide throttle; prevents OOM |
| Transfer throughput | Job outcomes | Detects the case where more concurrency stops helping |

Worker resource pressure throttles **globally rather than per repository**, because CPU exhaustion is a property of the fleet, not of any one registry.

### 3.4 Distribution and persistence

The controller runs on the leader; limits are divided across active workers and shipped in the lease response ([09](09-api.md) §7.1). Workers self-limit. **No distributed semaphore, no lock service, no coordination protocol** — the Coordinator already knows how many workers exist, so it just does the division.

State persists in `repository_budgets` ([03](03-persistence.md) §7) so a Coordinator restart resumes at the learned limit. Restarting a vendor registry's concurrency probe from scratch after every deployment would repeatedly re-discover the same ceiling, and repeatedly annoy the vendor while doing it.

### 3.5 Storage and network

Storage performance is not a factor: **nothing is written to disk** ([05](05-transfer-engine.md) §4.3). Removing an entire class of backpressure input is a side benefit of the no-disk rule that is easy to overlook.

Network bandwidth is not measured directly. It is observed indirectly: saturating a link raises latency, which the controller already responds to. Measuring bandwidth explicitly would require a baseline we cannot obtain reliably in a shared cluster.

## 4. Graceful degradation

| Degraded dependency | Behaviour |
|---|---|
| One source registry down | That product's discovery backs off. **Others unaffected** ([07](07-discovery.md) §1) |
| One target registry down | Transfers to it retry; transfers to other targets of the same request proceed independently ([01](01-domain-model.md) §3.2) |
| SMTP down | Notifications retry from the outbox. **Transfers are unaffected** |
| Teams webhook down | Same |
| Sigstore/Rekor unreachable | Verification → `error` (not `failed`), retried ([08](08-verification.md) §8) |
| OTel collector down | Spans dropped. Nothing else affected |
| Prometheus down | Nothing affected; HPA stops scaling on backlog and holds at current replicas |

**No notification failure, tracing failure, or metrics failure ever blocks a transfer.** Observability is a side channel. A design where a broken webhook stalls a 45 GB replication would be a bad design, and this is worth stating because it is an easy accident: putting the notification send in the completion transaction rather than in an outbox would do exactly that.

## 5. The cockroach test

Chaos scenarios the implementation must pass. **These are acceptance criteria, not aspirations** — they run in M7 ([17](17-delivery-plan.md)) and belong in CI thereafter.

| # | Scenario | Expected outcome |
|---|---|---|
| C1 | `kill -9` every worker at 50% of a 45 GB transfer | All workers restart, resume within ~150 s, transfer completes. **Bytes re-transferred ≤ (workers × in-flight blob size)** |
| C2 | Delete the Coordinator pod mid-transfer | Workers continue on held leases. Transfer completes. No duplicate jobs |
| C3 | Delete **both** Coordinator replicas | Workers finish leases, idle without crashing, resume on recovery |
| C4 | Restart Postgres mid-transfer | Coordinator reconnects; at most a few jobs re-leased; no duplicate placements |
| C5 | Blackhole the target registry for 5 min | Jobs back off; adaptive limit drops; transfer resumes on recovery without human action |
| C6 | Return 429 for 50% of requests for 10 min | Concurrency drops via AIMD; transfer completes slowly; **zero failures** |
| C7 | Corrupt one blob's bytes in flight | Digest mismatch detected; job retried twice; fails loudly. **Corrupt bytes never committed** |
| C8 | Delete a blob at the destination after planning | Manifest push → `BLOB_UNKNOWN`; placement invalidated; blob requeued; transfer completes |
| C9 | Scale workers 3 → 30 → 3 mid-transfer | No rebalancing, no duplicate work, no stalls |
| C10 | Fill the worker's disk | **No effect.** Nothing is written to disk |
| C11 | Kill a worker holding a manifest job between blob completion and manifest push | Manifest job requeued; **no partially-tagged package ever visible** (invariant I1) |
| C12 | Network partition between a worker and the Coordinator for 5 min | Worker completes in-flight jobs, cannot report, leases expire, work is redone. **No corruption** |

C10 and C11 are the two most valuable. C10 validates the no-disk invariant, which underpins the memory model and the concurrency ceiling. C11 validates the ordering invariant that makes an interrupted transfer safe for consumers — the property that lets us tolerate everything else on this list.
