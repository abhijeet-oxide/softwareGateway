# 17 — Delivery Plan

> **Prerequisites:** all preceding documents

Milestones are sized so each ends at something **demonstrable**, not at "the persistence layer is done". Every milestone has acceptance criteria a reviewer can check without reading the diff.

---

## 1. Milestones

### M1 — Foundation

Skeleton, configuration, schema. Nothing transfers yet.

- `cmd/{coordinator,worker,transferctl}` building and running
- Package layout and dependency rules enforced by `depguard` inside golangci-lint ([15](15-code-layout.md) §3)
- Product config loader: parsing, validation, `fsnotify` reload, secret resolution ([02](02-configuration.md))
- Full schema and migrations, both dialects ([03](03-persistence.md))
- `platform/`: logging, metrics, tracing, health, backoff, state machine, leader election
- API skeleton: middleware chain including the **no-op auth slot** ([09](09-api.md) §10.1), `/healthz`, `/readyz`, `/metrics`, error model
- `transferctl health`, `version`, `config validate`

**Acceptance:** `transferctl config validate ./dev/products/` catches every error class in [13](13-cli.md) §9. Coordinator starts against SQLite with no setup. Two Coordinators elect one leader. `make test` passes without Docker.

### M2 — Discovery

First externally visible behaviour.

- `Repository` interface and the generic OCI implementation ([06](06-registry-abstraction.md))
- Shared transport: auth, token cache, rate limiting, retry, CA, proxy ([06](06-registry-abstraction.md) §5)
- Discovery loop, pagination, tag filters, supersession ([07](07-discovery.md))
- Package and artifact persistence; manifest trees stored verbatim
- Auto-download rule evaluation creating transfer requests (not yet executed)
- `packages list|describe|discover`, `products list|describe`

**Acceptance:** point it at a real vendor registry; it discovers packages, does not duplicate on re-scan, records artifact trees, and evaluates rules. Kill it mid-scan — the next scan completes correctly.

### M3 — Transfer (the vertical slice) + close ADR-001

**The milestone that proves the design.** First real multi-GB transfer.

- Planner, wave assignment, dedupe classification ([05](05-transfer-engine.md) §3)
- Queue: dequeue, leases, reaper, wave advancement, retry ([04](04-queue-and-scheduling.md))
- Worker: lease loop, streaming engine, inline digest verification, progress
- Fast paths: placement hit, destination `HEAD`, cross-repo mount ([05](05-transfer-engine.md) §4)
- Transport tuning, forced HTTP/1.1, token caching ([05](05-transfer-engine.md) §5)
- Worker-plane API ([09](09-api.md) §7)
- `download`, `transfers describe|jobs`, `--watch`

**Acceptance:**
- A 30–60 GB package transfers end to end.
- **Worker RSS stays within the [05](05-transfer-engine.md) §4.5 formula throughout** — the check that proves streaming actually streams.
- `kill -9` all workers at 50%; the transfer completes with bytes re-transferred ≤ (workers × in-flight blob size). This is chaos scenario **C1** ([11](11-resiliency-and-backpressure.md) §5), run here rather than deferred, because it validates the core recovery premise.
- Re-running the same transfer is a near no-op (dedupe).
- Worker containers run with `readOnlyRootFilesystem` and no writable volume (**C10**).

**Also closes [ADR-001](16-technology-choices.md#adr-001).** Sequenced so the spike never blocks the slice: build against `Repository` with whichever backend reaches a working blob transfer first; then prototype the second behind the same interface; then score both against the fixed criteria table. Exit criteria include a written ADR closure with the measurements and **deletion of the losing backend**.

[08](08-verification.md) §3.3 — whether signature discovery can be hand-rolled against `Repository.Referrers` — is an M3 input, since it is the condition that most weakens the cosign-alignment argument.

> **Schedule risk, stated honestly:** the second prototype costs a few days before M3 completes. Bought deliberately, to close the system's most consequential library choice with evidence rather than argument.

### M4 — Operations

Everything a user needs to actually run transfers.

- Promotion ([05](05-transfer-engine.md) §6)
- Pause, resume, cancel, retry, priority ([04](04-queue-and-scheduling.md) §8)
- Scheduling with due-time expansion ([04](04-queue-and-scheduling.md) §10)
- Dry run via `validateOnly` ([05](05-transfer-engine.md) §7)
- Chunked upload and resumption ([05](05-transfer-engine.md) §4.6)
- ACR, Artifactory, Quay implementations + capability probing ([06](06-registry-abstraction.md) §3)
- Full CLI

**Acceptance:** promote lab → production within one registry and confirm most blobs mount rather than transfer. A scheduled request creates **zero** queue rows until due. Dry-run output matches the actual transfer that follows. Conformance suite green against all four registry types.

### M5 — Verification

- Cosign keyed and keyless; referrers with tag-schema fallback ([08](08-verification.md) §3)
- Source, destination, and on-demand stages
- Signature artifact transfer ([08](08-verification.md) §5)
- Per-product trust policy; air-gapped Sigstore roots
- `verify`; verification state machine ([10](10-state-machines.md) §5)

**Acceptance:** a real vendor-signed package verifies at source and destination. A tampered manifest fails **`failed`**; an unreachable Rekor fails **`error`** — the distinction that separates a security event from an availability event ([08](08-verification.md) §8). Transferred signatures verify independently at the destination using the vendor's own policy.

### M6 — Notifications, audit, retention

- Transactional outbox; email and Teams (Power Automate) ([12](12-observability-and-audit.md) §5)
- Audit recording across all events; `auditEvents` query API
- Retention GC, batched; audit partition management ([03](03-persistence.md) §8)
- Worker log shipping; `transfers logs`, `workers logs`

**Acceptance:** every event in the [12](12-observability-and-audit.md) §4.1 catalog is emitted and queryable. SMTP down retries notifications and **does not affect transfers**. GC deletes a million job rows without stalling an in-flight transfer. Dropping a year-old audit partition is instant.

### M7 — Scale and chaos

- AIMD backpressure controller, persisted budgets ([11](11-resiliency-and-backpressure.md) §3)
- Fleet-wide budget division in lease responses
- HPA, prometheus-adapter, dashboards, alerts
- Flux manifests, NetworkPolicy, PDB
- **Full cockroach suite C1–C12**

**Acceptance:** all twelve chaos scenarios pass ([11](11-resiliency-and-backpressure.md) §5). HPA scales 2→20→2 under load without thrash. A registry returning 50% 429s for ten minutes completes the transfer with **zero failures** (C6). Adaptive limits converge and survive a Coordinator restart.

## 2. Sequencing

```
M1 Foundation
     │
M2 Discovery ──────────┐
     │                 │
M3 Transfer + ADR-001  │   ← the vertical slice; the riskiest milestone, done early
     │                 │
     ├── M4 Operations │
     ├── M5 Verification (needs M3's Referrers work)
     └── M6 Notifications, audit, retention  ← can start after M2
                       │
M7 Scale and chaos ────┘   ← needs M3 + M4
```

M6 depends only on M2 and can run in parallel with M4/M5 if there is capacity. M7 is deliberately last: backpressure tuning is meaningless without a real transfer path to tune, and chaos testing needs the full system.

**M3 is scheduled third, not later, on purpose.** It carries essentially all the technical risk — streaming without disk, memory behaviour under concurrency, lease-based recovery, and the library decision. Discovering a problem there in week 6 is recoverable; discovering it in week 20 is not. M1 and M2 exist mainly to make M3 possible.

## 3. Testing strategy

Per [15](15-code-layout.md) §5.

| Level | Runs | Gate |
|---|---|---|
| Unit + engine + store | Every PR, no Docker | Merge |
| Integration (testcontainers) | Every PR | Merge |
| Registry conformance | Nightly, real registries | Release |
| Chaos (C1–C12) | Nightly + pre-release | Release |
| Throughput benchmark | Pre-release | Release |

**Two gates that catch regressions nothing else would:**

- **Memory ceiling.** Assert worker RSS against the [05](05-transfer-engine.md) §4.5 formula during a large transfer. The most likely way to break the streaming invariant is an innocent-looking `io.ReadAll` or a buffering wrapper added later; this is the only test that would notice.
- **Dedupe hit rate.** Assert that re-transferring an already-present package moves ~zero bytes. Deduplication is the system's largest optimization and it can silently regress — a placement-key bug would show up as "slightly slower", not as a failure.

## 4. Definition of done

A milestone is complete when **all** hold:

1. Acceptance criteria demonstrated on real infrastructure, not only in tests.
2. Unit and integration tests pass in CI; coverage on new domain packages ≥ 70%.
3. `golangci-lint` clean (which includes the `depguard` dependency-direction rules).
4. Metrics and audit events emitted for new behaviour ([12](12-observability-and-audit.md)).
5. Every state persisted has a `CHECK` constraint and a transition table entry ([10](10-state-machines.md)).
6. Design documents updated where implementation diverged — **the divergence is written down, not silently absorbed.**
7. CLI help and examples updated.

Item 6 is the one that decays first and matters most. A design document that stops matching the code stops being read, and then stops being updated, and then actively misleads. Every milestone budgets for it.

## 5. Open questions

Deliberately unresolved. Each is recorded with when it must be answered, so none becomes an implicit decision.

| # | Question | Decide by |
|---|---|---|
| Q1 | **[ADR-001](16-technology-choices.md#adr-001)** — OCI client library | M3 (procedure fixed) |
| Q2 | Can signature discovery be hand-rolled against `Repository.Referrers`? ([08](08-verification.md) §3.3) | M3 — it is an ADR-001 input |
| Q3 | Which registries actually honour chunked-upload resume in our environment? ([05](05-transfer-engine.md) §4.6) | M4, empirically via `upload_resume_total` |
| Q4 | Is priority aging needed, or does alerting on `queue_oldest_pending_age_seconds` suffice? ([04](04-queue-and-scheduling.md) §6) | M7, from production behaviour |
| Q5 | Does the dequeue need the per-target composite index? ([04](04-queue-and-scheduling.md) §4.2) | M7, triggered by `queue_lease_duration_seconds` |
| Q6 | **When is API authentication enabled?** ([09](09-api.md) §10) | **Before any exposure beyond the cluster — a deployment gate, not a milestone** |

Q6 is the one with real consequences. v1 ships unauthenticated behind a NetworkPolicy, and that is only acceptable while the NetworkPolicy is the whole story. Adding an Ingress without first implementing [09](09-api.md) §10.2 would expose transfer control and the full audit trail to anyone who can route to it.
