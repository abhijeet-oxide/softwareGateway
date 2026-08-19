# 00 - Overview

> **Status:** Draft for review · **Audience:** everyone · **Read first**

---

## 1. Problem

Software vendors publish their products as **software packages** into OCI-compliant registries. A package is not a single container image - it is a tag that resolves to an OCI index containing container images, Helm charts, configuration bundles, signatures, SBOMs, and other artifacts. A single package is routinely **30–60 GB across hundreds to thousands of blobs**.

We consume many such products from many vendors. For each product we must:

1. Continuously notice when a vendor publishes something new.
2. Replicate it into one or more internal registries, quickly and reliably.
3. Promote it between internal registries (lab → production) once validated.
4. Prove it is what the vendor signed.

Doing this by hand, or with `crane copy` in a CronJob, breaks down on all four counts: no discovery record, no progress visibility, no resumption after failure, no deduplication, no audit trail, and throughput bounded by a single process.

## 2. Goals

| # | Goal | Why it drives design |
|---|---|---|
| G1 | **Maximum transfer throughput** | The dominant cost is moving tens of GB. Everything else is bookkeeping. |
| G2 | **Never buffer artifacts to disk** | A worker must stream source→destination. Disk would cap concurrency and add a failure mode. |
| G3 | **Survive anything, resume automatically** | Node loss mid-transfer must cost seconds, not a restart of a 60 GB copy. |
| G4 | **Every operation idempotent** | Retries, duplicate API calls, and re-scans must never duplicate work. |
| G5 | **Transfer each blob at most once** | OCI content-addressing makes this free if we track placements. |
| G6 | **Declarative, GitOps-native config** | Flux applies ConfigMaps and Secrets; VSO supplies credentials. |
| G7 | **Operable by someone who did not write it** | Explicit states, real metrics, an audit trail, a CLI that tells the truth. |

## 3. Non-goals

Naming these prevents scope creep later.

- **Not a registry.** We do not store artifacts. We move them between registries that already exist.
- **Not a pull-through cache or mirroring proxy.** No artifact byte is ever served by us, and no transfer is ever a side effect of somebody's pull. We may, however, **configure and observe** a registry that does those things - a Quay target can delegate replication to Quay's own mirroring or proxy cache ([18](18-quay-replication.md)). The non-goal is about our data path; that is about our control plane.
- **No graphical UI in v1.** `transferctl` and Prometheus/Grafana are the interfaces. The design is API-first and a UI follows - direction, scope and the gates before it in [19](19-user-interface.md).
- **No vendor-side publishing.** We only read from vendor repositories.
- **No multi-tenancy or per-user quotas in v1.** One organization, one deployment.
- **No API authentication in v1.** The Coordinator sits behind a NetworkPolicy. The auth seam is specified in [09 - API](09-api.md) §10 but not implemented. This is an accepted, documented risk.
- **No general artifact transformation.** We do not re-tag, re-sign, rebuild, or rewrite manifests. Bytes in, identical bytes out - that is what makes signatures survive the trip.

## 4. Design philosophy

Every decision in this document set is resolved against this ordering. When two options conflict, the higher priority wins, and the document says so explicitly.

```
1. Maximum transfer throughput
2. Resiliency and automatic recovery
3. Simplicity
4. Operational maintainability
5. Scalability
```

Two corollaries used repeatedly:

- **If a simpler design is equally robust, it wins.** This is why the queue is a Postgres table and not Kafka, why config is a ConfigMap and not a CRD, and why job ordering is an integer and not a dependency graph.
- **Complexity must be paid for in throughput or resiliency.** Adaptive backpressure is complex and earns its place under G1/G3. A distributed lock service would be complex and earn nothing.

## 5. Architecture

Three binaries. One database. No message broker, no cache, no sidecar.

```
                         ┌────────────────────────────────────┐
   transferctl  ────────►│           COORDINATOR              │
   (CLI, pure API        │           (control plane)          │
    client)              │                                    │
                         │  REST API (AIP-style, /api/v1)     │
                         │  Discovery loop     ─┐             │
                         │  Scheduler          │ leader-only  │
                         │  Lease reaper       │ (pg advisory │
                         │  Notification outbox│  lock)       │
                         │  Retention GC       ─┘             │
                         │  Backpressure controller           │
                         │  Audit · Metrics · Tracing         │
                         └──────┬──────────────────┬──────────┘
                                │ SQL              │ HTTP
                                │ (sole writer)    │ lease / heartbeat
                                ▼                  │ progress / complete
                         ┌─────────────┐           │
                         │ PostgreSQL  │           │   (control only -
                         │  queue      │           │    no artifact bytes)
                         │  state      │           │
                         │  audit      │           ▼
                         └─────────────┘   ┌──────────────────┐
                                           │     WORKERS      │
                                           │   (data plane)   │
                                           │   stateless · N  │
                                           └───┬──────────┬───┘
                                               │          │
                                    GET blob   │          │  POST/PUT blob
                                               ▼          ▼
                                      ┌────────────┐  ┌────────────┐
                                      │  SOURCE    │  │  TARGET    │
                                      │  REGISTRY  │  │  REGISTRY  │
                                      └────────────┘  └────────────┘
                                          artifact bytes stream
                                          directly between these two
```

**The load-bearing property of this picture:** artifact bytes flow only along the bottom edge. They never enter the Coordinator, never touch Postgres, and never land on a worker's disk. The Coordinator moves job records measured in bytes; workers move blobs measured in gigabytes.

### 5.1 Coordinator - control plane

Owns all state. Sole writer to Postgres. Runs 2 replicas: both serve the API, one holds the leader lock and runs the background loops. Responsibilities: REST API, discovery, scheduling, queue management, transfer planning, lease issuance, per-repository concurrency budgets, notifications, audit, metrics.

### 5.2 Worker - data plane

Stateless. Holds no database credentials and contains no SQL. Leases jobs from the Coordinator over HTTP, streams blobs registry-to-registry, reports progress, exits. Scaled horizontally by HPA on queue backlog. A worker can be killed at any instant with no cleanup and no data loss - its leases simply expire.

> **Decision - workers lease over HTTP rather than dequeuing from Postgres directly.**
> *Alternative:* give every worker a Postgres connection and let it run `SELECT … FOR UPDATE SKIP LOCKED` itself. That removes the Coordinator from the work-acquisition path, so transfers continue through a Coordinator outage.
> *Chosen:* HTTP leasing. Database credentials and schema knowledge stay in one binary, which makes migrations a single-deployment concern and keeps the security surface small. More importantly, per-repository concurrency budgets and adaptive backpressure ([11](11-resiliency-and-backpressure.md)) need a global view; computing them centrally and shipping the answer in the lease response is far simpler than coordinating semaphores in SQL across N pods.
> *Cost accepted:* new work cannot be acquired while the Coordinator is down. Mitigated by 2 replicas and lease durations long enough that in-flight transfers continue uninterrupted across a rolling restart. Bytes already in flight are never affected - they do not traverse the Coordinator.

### 5.3 transferctl - CLI

Talks **only** to the Coordinator API. Never contacts a registry, never opens a database connection, never talks to a worker. This is what makes the CLI safe to hand to anyone and keeps one authorization and audit chokepoint. Even `transferctl logs`, which shows worker output, is served by the Coordinator from logs workers ship on their control channel.

## 6. Terminology

The requirements use "Controller" and "Coordinator" interchangeably. **This document set uses _Coordinator_ throughout**, matching the `cmd/coordinator` entry point. Full vocabulary in [01 - Domain Model](01-domain-model.md) §5.

## 7. Life of a package

A concrete trace of one 45 GB package, from vendor push to verified-at-destination. Section references point at the document that specifies each step.

**1. Discovery.** The leader Coordinator polls the vendor repository, lists tags, and sees `v2.14.0` for the first time. It resolves the tag to a manifest digest with a `HEAD`, then inserts a `packages` row. The unique constraint on `(source_repo_id, tag, manifest_digest)` means a concurrent or repeated scan is a no-op - idempotency is structural, not procedural. → [07](07-discovery.md)

**2. Notification and auto-download.** The insert emits an audit event and queues a `PackageDiscovered` notification to the outbox. The product's auto-download rules are evaluated; `^v2\.\d+\.\d+$` matches, so a transfer request is created automatically against the `lab` target. → [07](07-discovery.md) §4, [02](02-configuration.md) §6

**3. Planning.** The Coordinator walks the manifest tree from the source registry: index → 3 image manifests → 1 Helm chart → 847 blobs totalling 45 GB. It checks `blob_placements` and finds 291 blobs (12 GB) already present in the target from an earlier version of this product. It creates jobs only for the remaining 556 blobs, plus 5 manifest jobs. **33 GB of real work, 12 GB deduplicated away before a single byte moves.** → [05](05-transfer-engine.md) §3

**4. Enqueue.** Jobs land in the `jobs` table. Blob jobs get `wave = 0`, child manifests `wave = 1`, the index `wave = 2`. Only wave 0 is leasable, which is what guarantees a manifest is never pushed before the blobs it references. → [04](04-queue-and-scheduling.md) §3

**5. Transfer.** Twelve workers lease batches of blob jobs. Each job: check the destination with a `HEAD` (a few more turn out to be present - mark complete, zero bytes); attempt a cross-repository mount where source and destination share a registry; otherwise open a `GET` against the source and pipe the body straight into the destination upload, verifying the digest inline as bytes pass. Nothing is buffered beyond a few MB. → [05](05-transfer-engine.md) §4

**6. Interruption.** A node is preempted 20 GB in. Four workers vanish mid-blob. Their leases expire ~60 s later; the reaper returns those jobs to `pending`; other workers pick them up. **The other 20 GB is untouched, because completion is tracked per blob.** Partially-uploaded blobs restart, and only those blobs. → [11](11-resiliency-and-backpressure.md) §2

**7. Manifests.** Wave 0 drains. The transfer advances to wave 1; image and chart manifests are pushed. Then wave 2: the index. The tag now resolves at the destination - and only now, so an interrupted transfer never leaves a tag pointing at missing blobs. → [04](04-queue-and-scheduling.md) §3.2

**8. Verification.** Cosign signature discovery runs against the destination, verifying against the product's trust policy. On success the package reaches `Verified`. On failure it reaches `VerificationFailed` and fires a notification - the artifacts stay put for inspection rather than being silently deleted. → [08](08-verification.md)

**9. Record.** Every step above wrote an `audit_events` row: who, what, when, which digests, which outcome. `transferctl` reports 45 GB in 11 minutes, 12 GB deduplicated, 3 blobs retried, 0 failed. → [12](12-observability-and-audit.md) §4

**10. Promotion.** Days later, someone runs `transferctl promote --package v2.14.0 --from lab --to production`. This is the same machinery with a target registry as the source. Because both registries hold most of these blobs already, most of the package is mounted or skipped outright. → [05](05-transfer-engine.md) §6

## 8. What makes this fast

Four mechanisms, in descending order of impact. Detail in [05](05-transfer-engine.md).

1. **Never move a blob twice.** `blob_placements` plus a destination `HEAD` turns repeat content into zero I/O. Across successive versions of the same product this is typically the largest single win - base layers rarely change.
2. **Cross-repository mount.** When source and destination live in the same registry, the OCI `mount` parameter relocates a blob server-side. Zero bytes over the wire.
3. **Blob-level parallelism.** The unit of work is a blob, not a package, so one package saturates the whole worker fleet instead of pinning to a single process. Concurrency exists at three levels: packages, workers, and blobs-in-flight per worker.
4. **A transport tuned for large bodies.** HTTP/1.1 for blob data (h2's flow control and head-of-line blocking hurt multi-GB transfers), compression disabled (blobs are already compressed), connection pools sized for the concurrency, and cached registry tokens so a 1,000-blob package does not perform 1,000 token exchanges.

## 9. What makes this survivable

- **Leases, not locks.** Worker liveness is a timestamp. A `SIGKILL`ed pod on a dead node needs no cleanup handshake - the lease expires and the job returns to the queue.
- **Blob-granular progress.** The blast radius of any failure is one blob, not one package.
- **Structural idempotency.** Unique constraints, not application logic, prevent duplicate packages, duplicate requests, and duplicate jobs.
- **Explicit state machines.** Five of them, as transition tables, with a guard function that rejects illegal transitions rather than corrupting state. → [10](10-state-machines.md)
- **No worker state.** Scaling from 3 to 30 to 0 workers requires no rebalancing, no partition assignment, and no draining protocol.

## 10. Reading order

| If you are… | Read |
|---|---|
| Reviewing the architecture | 00 → 01 → 04 → 05 → 11 |
| Implementing the Coordinator | 03 → 04 → 09 → 10 → 07 |
| Implementing the Worker | 05 → 06 → 04 §4 → 11 |
| Implementing the CLI | 13 → 09 |
| Operating it | 02 → 12 → 14 → 11 |
| Deciding whether the tech choices are right | 16 → 03 → 06 |

Full index and requirement traceability: [docs/design/README.md](README.md).
