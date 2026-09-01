# 17 - Delivery Plan

> **Prerequisites:** all preceding documents

Milestones are sized so each ends at something **demonstrable**, not at "the persistence layer is done". Every milestone has acceptance criteria a reviewer can check without reading the diff.

---

## 1. Milestones

### M1 - Foundation

Skeleton, configuration, schema. Nothing transfers yet.

- `cmd/{coordinator,worker,transferctl}` building and running
- Package layout and dependency rules enforced by `depguard` inside golangci-lint ([15](15-code-layout.md) §3)
- Product config loader: parsing, validation, `fsnotify` reload, secret resolution ([02](02-configuration.md))
- Full schema and migrations, both dialects ([03](03-persistence.md))
- `platform/`: logging, metrics, tracing, health, backoff, state machine, leader election
- API skeleton: middleware chain including the **no-op auth slot** ([09](09-api.md) §10.1), `/healthz`, `/readyz`, `/metrics`, error model
- `transferctl health`, `version`, `config validate`

**Acceptance:** `transferctl config validate ./dev/products/` catches every error class in [13](13-cli.md) §9. Coordinator starts against SQLite with no setup. Two Coordinators elect one leader. `task test` passes without Docker.

### M2 - Discovery

First externally visible behaviour.

- `Repository` interface and the generic OCI implementation ([06](06-registry-abstraction.md))
- Shared transport: auth, token cache, rate limiting, retry, CA, proxy ([06](06-registry-abstraction.md) §5)
- Discovery loop, pagination, tag filters, supersession ([07](07-discovery.md))
- Package and artifact persistence; manifest trees stored verbatim
- Auto-download rule evaluation creating transfer requests (not yet executed)
- `packages list|describe|discover`, `products list|describe`

**Acceptance:** point it at a real vendor registry; it discovers packages, does not duplicate on re-scan, records artifact trees, and evaluates rules. Kill it mid-scan - the next scan completes correctly.

### M3 - Transfer (the vertical slice) + close ADR-001

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
- **Worker RSS stays within the [05](05-transfer-engine.md) §4.5 formula throughout** - the check that proves streaming actually streams.
- `kill -9` all workers at 50%; the transfer completes with bytes re-transferred ≤ (workers × in-flight blob size). This is chaos scenario **C1** ([11](11-resiliency-and-backpressure.md) §5), run here rather than deferred, because it validates the core recovery premise.
- Re-running the same transfer is a near no-op (dedupe).
- Worker containers run with `readOnlyRootFilesystem` and no writable volume (**C10**).

**Also closes [ADR-001](16-technology-choices.md#adr-001).** Sequenced so the spike never blocks the slice: build against `Repository` with whichever backend reaches a working blob transfer first; then prototype the second behind the same interface; then score both against the fixed criteria table. Exit criteria include a written ADR closure with the measurements and **deletion of the losing backend**.

[08](08-verification.md) §3.3 - whether signature discovery can be hand-rolled against `Repository.Referrers` - is an M3 input, since it is the condition that most weakens the cosign-alignment argument.

> **Schedule risk, stated honestly:** the second prototype costs a few days before M3 completes. Bought deliberately, to close the system's most consequential library choice with evidence rather than argument.
>
> **What actually happened:** the second prototype was not built, and the risk above was therefore not incurred. §3.3 resolved affirmatively, which removed the tiebreaker the comparison existed to settle - so the ADR closed on that rather than on the criteria table. Recorded in full, including the four criteria left unmeasured, in the [closure](16-technology-choices.md#11-adr-001-closure).

### M4 - Operations

Everything a user needs to actually run transfers.

- Promotion ([05](05-transfer-engine.md) §6)
- Pause, resume, cancel, retry, priority ([04](04-queue-and-scheduling.md) §8)
- Scheduling with due-time expansion ([04](04-queue-and-scheduling.md) §10)
- Dry run via `validateOnly` ([05](05-transfer-engine.md) §7)
- Chunked upload and resumption ([05](05-transfer-engine.md) §4.6)
- ACR, Artifactory, Quay implementations + capability probing ([06](06-registry-abstraction.md) §3)
- Full CLI

**Acceptance:** promote lab → production within one registry and confirm most blobs mount rather than transfer. A scheduled request creates **zero** queue rows until due. Dry-run output matches the actual transfer that follows. Conformance suite green against all four registry types.

### M5 - Verification

- Cosign keyed and keyless; referrers with tag-schema fallback ([08](08-verification.md) §3)
- Source, destination, and on-demand stages
- Signature artifact transfer ([08](08-verification.md) §5)
- Per-product trust policy; air-gapped Sigstore roots
- `verify`; verification state machine ([10](10-state-machines.md) §5)

**Acceptance:** a real vendor-signed package verifies at source and destination. A tampered manifest fails **`failed`**; an unreachable Rekor fails **`error`** - the distinction that separates a security event from an availability event ([08](08-verification.md) §8). Transferred signatures verify independently at the destination using the vendor's own policy.

### M6 - Notifications, audit, retention

- Transactional outbox; email and Teams (Power Automate) ([12](12-observability-and-audit.md) §5)
- Audit recording across all events; `auditEvents` query API
- Retention GC, batched; audit partition management ([03](03-persistence.md) §8)
- Worker log shipping; `transfers logs`, `workers logs`

**Acceptance:** every event in the [12](12-observability-and-audit.md) §4.1 catalog is emitted and queryable. SMTP down retries notifications and **does not affect transfers**. GC deletes a million job rows without stalling an in-flight transfer. Dropping a year-old audit partition is instant.

### M7 - Scale and chaos

- AIMD backpressure controller, persisted budgets ([11](11-resiliency-and-backpressure.md) §3)
- Fleet-wide budget division in lease responses
- HPA, prometheus-adapter, dashboards, alerts
- Flux manifests, NetworkPolicy, PDB
- **Full cockroach suite C1–C12**

**Acceptance:** all twelve chaos scenarios pass ([11](11-resiliency-and-backpressure.md) §5). HPA scales 2→20→2 under load without thrash. A registry returning 50% 429s for ten minutes completes the transfer with **zero failures** (C6). Adaptive limits converge and survive a Coordinator restart.

### M8 - Quay replication strategies

Delegation. A Quay target can stop being somewhere we push to and become somewhere that pulls for itself. Specified in [18](18-quay-replication.md).

**Status: complete except `warm`, which Q9 moved out of this milestone on purpose** ([18](18-quay-replication.md) §6.3). A mirror can be declared, validated, applied, observed, drifted and synced; a download against a mirror target delegates, waits, walks the destination and settles as `succeeded`, `diverged` or `failed`; and no delegated object reports a byte anywhere. `warm` needs a third job kind in the worker plane, because pulling a 45 GB release through the Coordinator would break the invariant in [00](00-overview.md) §5.

- `replication.mode` on a target: `copy` (default, unchanged), `mirror`, `proxy` ([18](18-quay-replication.md) §5)
- `internal/registry/quay`: the **management** API client - mirror config, proxy cache, `changestate`, robots - separate from the `/v2` data path
- The `Strategy` seam, one level above the planner and the engine, both unchanged ([18](18-quay-replication.md) §7)
- Explicit `apply` with a diff; continuous drift detection that never self-heals ([18](18-quay-replication.md) §8)
- Observed sync history and the `diverged` outcome; `warm` deferred to the worker plane by Q9 ([18](18-quay-replication.md) §6)
- `targets list|describe|apply|sync|drift`, the replication routes, the `Replication` audit category and its metrics

**Acceptance:**
- A `mode: mirror` target applies from configuration, syncs on request, and reaches `succeeded` when the destination digest matches the discovered one - and `diverged`, not `succeeded`, when the upstream tag has moved underneath it.
- A tag glob that would exclude `sha256-*.sig` signature tags is caught by `config validate` **before** it is applied ([18](18-quay-replication.md) §9). This is the failure this milestone exists to make impossible.
- A mirror edited by hand in the Quay UI shows as drift within one reload; `targets apply` closes it; **no config reload ever closes it by itself**.
- A `mode: proxy` target refuses `download` with a problem detail naming `warm`; `warm` populates the cache and reports the bytes it discarded.
- Byte columns for a delegated transfer render `-` everywhere. Nothing in any output synthesises a percentage ([18](18-quay-replication.md) §6.1).

**Entry criterion:** Q7 below - if a mirror sync cannot be observed well enough to report honestly, this milestone does not start.

### M9 - Downloads and auto-download

The declared form of the operation the estate actually performs: vendor → JFrog → Quay, with gates, as one reviewable object - and, separately, the rule that fires it without being asked. Specified in [20](20-download-rules.md).

**Status: complete except the metrics.** A download can be declared, validated, listed, dry-run and run by hand; its chain is derived from `mirror.from`, its steps are ordered, and a step whose predecessor did not succeed is `skipped` rather than failed. An auto-download rule matches a tag and triggers that same download.

- `spec.download` - targets, gates and priority, with **no pattern in it**: a download is performed, not fired ([20](20-download-rules.md) §3.1)
- `spec.autoDownload` - a tag pattern, the sources to watch, and the download to trigger. The only place a pattern belongs ([20](20-download-rules.md) §3.4)
- Rules written in the older inline shape keep loading and keep meaning what they meant ([20](20-download-rules.md) §3.5)
- Chain derivation from the targets' own `mirror.from` edges - a set of destinations in, an ordered plan out ([20](20-download-rules.md) §3.6, §4)
- `transfers.step_index` and `depends_on_transfer_id`; the `waiting` and `skipped` transfer states ([20](20-download-rules.md) §6)
- Verification as a gate: under `enforce`, a destination that fails verification stops the steps that depend on it ([20](20-download-rules.md) §5)
- Download revisions in the idempotency key, and the rule deliberately absent from it ([20](20-download-rules.md) §8.2)
- `transferctl download`, `downloads list`, `rules list|matches`; the `downloads` and `autoDownloadRules` routes; the `Download` audit category and its metrics

**Acceptance:**
- One download **naming only the Quay target** takes a newly discovered release from the vendor into JFrog and then into Quay, in that order, with no second command - and `transfers describe` shows two steps with two different kinds of progress and no combined percentage ([20](20-download-rules.md) §3.6, §7.1).
- `transferctl download <product> <tag>` performs the same work an auto-download rule performs, through the same chain and the same gates, **consulting no pattern** - and a rule fires nothing a person could not have asked for by hand ([20](20-download-rules.md) §1.1).
- Two downloads whose chains share the JFrog hop transfer that package to JFrog **once**, and the audit trail shows one transfer ([20](20-download-rules.md) §3.6).
- With `verify.policy: enforce`, a destination whose signature check fails leaves the Quay step `skipped`, not `failed`, and **nothing was written to Quay**. This is the failure this milestone exists to make impossible.
- Every product document that was valid at M8 is valid at M9 and produces byte-identical transfers ([20](20-download-rules.md) §3.5).
- A download naming a Quay target whose tag glob excludes `sha256-*.sig` is rejected by `config validate` when `verify.after` is set - the two blocks are only wrong together ([20](20-download-rules.md) §3.7).
- No API call, CLI command or UI control can change whether a rule fires. `enabled` in Git is the only switch, and the CLI's own tests assert that "suspend" and "resume" do not appear in its output ([20](20-download-rules.md) §9).
- Retrying a partially failed run does not re-transfer a step that already succeeded.

### M10 - Web UI

The second client of the same API. Direction, scope and the six gates in [19](19-user-interface.md); information architecture in the [UI generation brief](../ui/ui-generation-brief.md).

- **Gate first: API authentication, identity and roles** ([09](09-api.md) §10, Q6). Nothing else in this milestone may start before it
- OpenAPI generated from the router; server-sent events for live progress
- Ten pages, six top-level, covering everything `transferctl` does
- Static bundle, air-gapped, no fourth binary

**Acceptance:** a first-time user replicates a package into lab without documentation. Every CLI capability is reachable in the UI. Configuration is read-only with drift visible. No delegated object shows a progress percentage or ETA anywhere.

### M11 - Custom software validation

Does a release follow the organization's own Kubernetes and CNF standards. Ground truth in [validation/00](../validation/00-validation-model.md) and [validation/01](../validation/01-check-catalog.md); design in [23](23-validation.md) §18.

Three stages, and the first delivers on its own:

- **M11-A - engine and catalogue.** The 88 tier-1 checks, the Helm renderer with its pinned inputs and its determinacy probe, the policy loader, the store, and `transferctl validate`. No UI
- **M11-B - API, Validation tab, Policies page**, the `Validated` timeline moment and a compliance column on the Software listing
- **M11-C - the vendor report** (XLSX/CSV/JSON/ZIP), waivers with expiry, `autoRun: onAnalysis`, cross-release comparison, and Rego pack support

Needs `expand` and the blob-read path, both of which exist. Needs nothing from M10.

**Acceptance:** every shipped check has a positive and a negative fixture and a meta-test fails CI when one does not; the `good-app` fixture produces zero findings across the whole baseline; the same release validated twice is byte-identical; a Coordinator with no `helm` reports `inconclusive` and never `pass`; and a vendor receives one file that names every failure with its full address - product, release, package digest, chart, chart version, source file, resource, container, field - states the rule that produced it, and lists what passed.

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
     │
     ├── M8 Quay replication strategies   ← needs M4 (Quay) + M6 (audit, notifications)
     │        │
     │   M9 Download rules                ← needs M8 (the mirror step) + M5 (the gates)
     │
     └── M10 Web UI                       ← needs Q6 (API auth) as a GATE, not a dependency
     └── M11 Custom software validation   ← needs expand + the blob-read path only; independent of M10
```

**M9 follows M8 rather than running beside it**, even though its fan-out half needs nothing from Quay. The ordering model, the `skipped` state and the per-step rendering would all be built twice if they were built first for a chain whose steps are all copies - and the chain worth having is the one with a mirror at the end ([20](20-download-rules.md) §14).

M6 depends only on M2 and can run in parallel with M4/M5 if there is capacity. M7 is deliberately last: backpressure tuning is meaningless without a real transfer path to tune, and chaos testing needs the full system.

**M3 is scheduled third, not later, on purpose.** It carries essentially all the technical risk - streaming without disk, memory behaviour under concurrency, lease-based recovery, and the library decision. Discovering a problem there in week 6 is recoverable; discovering it in week 20 is not. M1 and M2 exist mainly to make M3 possible.

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
- **Dedupe hit rate.** Assert that re-transferring an already-present package moves ~zero bytes. Deduplication is the system's largest optimization and it can silently regress - a placement-key bug would show up as "slightly slower", not as a failure.

## 4. Definition of done

A milestone is complete when **all** hold:

1. Acceptance criteria demonstrated on real infrastructure, not only in tests.
2. Unit and integration tests pass in CI; coverage on new domain packages ≥ 70%.
3. `golangci-lint` clean (which includes the `depguard` dependency-direction rules).
4. Metrics and audit events emitted for new behaviour ([12](12-observability-and-audit.md)).
5. Every state persisted has a `CHECK` constraint and a transition table entry ([10](10-state-machines.md)).
6. Design documents updated where implementation diverged - **the divergence is written down, not silently absorbed.**
7. CLI help and examples updated.

Item 6 is the one that decays first and matters most. A design document that stops matching the code stops being read, and then stops being updated, and then actively misleads. Every milestone budgets for it.

## 5. Open questions

Deliberately unresolved. Each is recorded with when it must be answered, so none becomes an implicit decision.

| # | Question | Decide by |
|---|---|---|
| ~~Q1~~ | ~~**[ADR-001](16-technology-choices.md#adr-001)** - OCI client library~~ | **CLOSED at M3: `oras-go/v2`, write path only. Two divergences from the fixed procedure are recorded in the [closure](16-technology-choices.md#11-adr-001-closure)** |
| ~~Q2~~ | ~~Can signature discovery be hand-rolled against `Repository.Referrers`?~~ ([08](08-verification.md) §3.3) | **ANSWERED at M3: yes - both mechanisms behind one `Referrers` call. This is what closed Q1** |
| Q3 | Which registries actually honour chunked-upload resume in our environment? ([05](05-transfer-engine.md) §4.6) | M4, empirically via `upload_resume_total` |
| Q4 | Is priority aging needed, or does alerting on `queue_oldest_pending_age_seconds` suffice? ([04](04-queue-and-scheduling.md) §6) | M7, from production behaviour |
| Q5 | Does the dequeue need the per-target composite index? ([04](04-queue-and-scheduling.md) §4.2) | M7, triggered by `queue_lease_duration_seconds` |
| Q6 | **When is API authentication enabled?** ([09](09-api.md) §10) | **Before any exposure beyond the cluster - a deployment gate, not a milestone. Also gate G1 for M10** |
| Q7 | **Is a Quay mirror sync observable enough to report honestly?** Does `GET …/mirror` distinguish "never ran" from "running" from "failed" reliably, or must we depend on Quay's notifications? ([18](18-quay-replication.md) §13) | M8 entry - a negative answer drops `mirror` rather than shipping a status nobody can trust |
| Q8 | Should `manage: auto` exist - continuous reconciliation of Quay config - and under what guard? ([18](18-quay-replication.md) §8) | M8 exit, from how often observed drift turns out to be legitimate |
| ~~Q9~~ | ~~Does `warm` belong in the worker plane?~~ ([18](18-quay-replication.md) §6.3) | **ANSWERED at M8: yes.** It moves the whole package at line rate, and [00](00-overview.md) §5 says bytes never enter the Coordinator. It needs a third `jobs.kind`, so it left M8 rather than being built in the wrong place |
| Q10 | Can a mirror tag glob be generated from an `autoDownload` rule's RE2 pattern, or is the dialect gap a trap? ([18](18-quay-replication.md) §5.3) | M8 - the default answer is no |
| Q13 | Do recurring schedules belong here at all, or does a CronJob calling `transferctl download` cover every real case? ([20](20-download-rules.md) §12) | M10 - the default answer is the CronJob |
| Q14 | Is the run revision the hash of the download or of the whole product document? ([20](20-download-rules.md) §8.2) | M9 exit |
| Q15 | Does anyone declare a second download? If nobody does within two quarters, the list is flexibility that cost nothing and bought nothing ([20](20-download-rules.md) §3.2) | M10 |

Q6 is the one with real consequences. v1 ships unauthenticated behind a NetworkPolicy, and that is only acceptable while the NetworkPolicy is the whole story. Adding an Ingress without first implementing [09](09-api.md) §10.2 would expose transfer control and the full audit trail to anyone who can route to it.
