# 09 — API

> **Prerequisites:** [01 — Domain Model](01-domain-model.md), [03 — Persistence](03-persistence.md), [04 — Queue and Scheduling](04-queue-and-scheduling.md)
> **Consumed by:** [13 — CLI](13-cli.md), [14 — Deployment](14-deployment-and-development.md)

The Coordinator's REST API is the **only** interface. `transferctl` uses it, workers use it, HPA scrapes it. There is no second path to the data.

---

## 1. Conventions

Google **AIP** (API Improvement Proposals) for resource naming and method shape, because it is a coherent, documented, widely-understood standard rather than a set of local habits.

| Concern | Convention | Reference |
|---|---|---|
| Base path | `/api/v1` | Versioned from day one |
| Resource names | Plural collections, `{id}` members: `/products/{product}/packages/{package}` | AIP-122 |
| Standard methods | `GET` list/get, `POST` create, `PATCH` update, `DELETE` delete | AIP-131…135 |
| Custom methods | `POST /resource/{id}:verb` — colon, lowerCamelCase verb | AIP-136 |
| Field names | `lowerCamelCase` in JSON | AIP-140 |
| Pagination | `pageSize` / `pageToken` → `nextPageToken` | AIP-158 |
| Filtering | `filter` query parameter | AIP-160 |
| Validate-only | `validateOnly=true` — **this is how dry run works** | AIP-163 |
| Long-running work | Resource state fields; polled, not an Operation resource | see §4.3 |
| Errors | RFC 9457 `application/problem+json` | §8 |
| Timestamps | RFC 3339 UTC, `…Z` | AIP-142 |
| Durations | Seconds as a number, or a duration string in human-facing fields | |

**Custom methods over invented resources.** `POST /transfers/{id}:pause` rather than `POST /transfers/{id}/pause` or `PATCH` with `{"state": "paused"}`. Pausing is a verb with side effects, not a sub-resource and not a field assignment; the colon form says exactly that and keeps `PATCH` meaning "change this field".

## 2. Route table

`{product}`, `{package}`, `{transfer}` are resource IDs. `//` marks control-plane-internal routes.

### Products

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/v1/products` | List configured products |
| `GET` | `/api/v1/products/{product}` | Get one, with source/target/rule summary |

| `POST` | `/api/v1/products:checkConnectivity` | Probe every product's registries and credentials |
| `POST` | `/api/v1/products/{product}:checkConnectivity` | Probe one product's |
| `POST` | `/api/v1/products/{product}:calibrate` | Measure one source-to-target path and recommend settings ([05](05-transfer-engine.md) §9) |

Products are **read-only over the API.** Configuration comes from Git ([02](02-configuration.md)); an API that could mutate it would create a second source of truth that Flux would immediately revert.

#### Target replication

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/v1/products/{product}/targets/{target}/replication` | Desired mode and settings, what was last applied, and any drift |
| `POST` | `/api/v1/products/{product}/targets/{target}/replication:apply` | Write it to the registry. `validateOnly=true` renders the diff only |
| `POST` | `/api/v1/products/{product}/targets/{target}/replication:sync` | Request a mirror sync now |
| `POST` | `/api/v1/products/{product}/targets/{target}/replication:cancelSync` | Stop an in-progress sync |
| `GET` | `/api/v1/products/{product}/targets/{target}/syncs` | Observed sync history |

These do not contradict the paragraph above: configuration still comes from Git, and `:apply` pushes what Git already says into a *third-party registry's* own configuration store. It never edits a product. See [18](18-quay-replication.md) §7–8.

#### Download rules

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/v1/products/{product}/downloadRules` | Rules, their derived chains, suspension state and last run |
| `GET` | `/api/v1/products/{product}/downloadRules/{rule}` | One, with the step order and the gates it applies |
| `POST` | `/api/v1/products/{product}/downloadRules/{rule}:run` | Trigger. `validateOnly=true` renders the plan and moves nothing |
| `POST` | `/api/v1/products/{product}/downloadRules/{rule}:suspend` | Stop it now. Requires a reason; never edits configuration |
| `POST` | `/api/v1/products/{product}/downloadRules/{rule}:resume` | |

Same caveat as above, and one deliberate absence: there is **no** `/downloadRules/{rule}/runs`. A run is a transfer request, and `GET /transfers?filter=rule="ga-releases"` already returns them (§3). See [20](20-download-rules.md) §10.

#### Why calibration is per product and synchronous

There is deliberately no `/products:calibrate`. Calibrating everything would mean saturating every vendor link this deployment has, one after another, and the answer for one path says nothing about another — a fleet-wide verb here would be load with no information in it.

It is also a plain synchronous request rather than a long-running operation, despite taking minutes. The result is a **report for a person**, not a resource for a system: nobody calibrates in a script, nobody needs the run to outlive the client, and a stored calibration is a measurement of a network as it was last Tuesday — the exact kind of stale number the feature exists to replace. Clients must raise their own timeout to cover the requested budget; `transferctl` derives one from the flags.

### Packages

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/v1/products/{product}/packages` | List discovered packages |
| `GET` | `/api/v1/products/{product}/packages/{package}` | Get one, with artifacts and per-target status |
| `GET` | `/api/v1/products/{product}/packages/{package}/artifacts` | Artifact tree |
| `POST` | `/api/v1/products/{product}/packages:discover` | Trigger an immediate scan ([07](07-discovery.md) §8) |
| `POST` | `/api/v1/products/{product}/packages/{package}:inspect` | Walk the manifest tree and measure it ([07](07-discovery.md) §13) |
| `POST` | `/api/v1/products/{product}/packages/{package}:verify` | On-demand verification ([08](08-verification.md) §4) |

#### Package references, and the custom-method colon

`{package}` is a tag or a digest. The `repository:tag` form a person types is **not** in the path — a repository path contains slashes, `%2F` is decoded before routing, and percent-encoding it twice works right up until a proxy normalises the path. So the wire form is `/packages/{tag}?repository=orbs/core`, and the CLI does that rewrite.

The custom-method colon is then split **in the handler, not by the router**, and this is worth stating because getting it wrong is silent. Registering `/packages/{package}:inspect` as a route looks correct and is: chi supports a parameter followed by literal text within a segment. But it matches at the *first* occurrence of the delimiter, and a digest reference contains a colon of its own — so `sha256:ccbd…:inspect` bound `{package}` to `sha256`, failed to match `:inspect` against `:ccbd…`, fell through to the `GET`-only route, and returned:

```
INVALID_ARGUMENT: POST is not supported on /api/v1/products/…
```

which names neither the real problem nor anything the caller could change. The route is now the plain package pattern with the verb split at the **last** colon by the handler, which is unambiguous for every reference form: a tag may not contain a colon, and a digest contains exactly one that is never last when a verb follows.

### Transfers

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/v1/transfers` | Create a request (replicate, promote, or scheduled). `validateOnly=true` ⇒ dry run |
| `GET` | `/api/v1/transfers` | List, filterable |
| `GET` | `/api/v1/transfers/{transfer}` | Get one, with progress |
| `GET` | `/api/v1/transfers/{transfer}/jobs` | Layer-level progress |
| `GET` | `/api/v1/transfers/{transfer}/failures` | Why it is failing, grouped by cause |
| `POST` | `/api/v1/transfers/{transfer}:pause` | ([04](04-queue-and-scheduling.md) §8) |
| `POST` | `/api/v1/transfers/{transfer}:resume` | |
| `POST` | `/api/v1/transfers/{transfer}:cancel` | |
| `POST` | `/api/v1/transfers/{transfer}:retry` | Requeue failed jobs ([10](10-state-machines.md) §3) |
| `POST` | `/api/v1/transfers:retry` | Requeue the failed jobs of every transfer that has any |
| `GET` | `/api/v1/workers` | The fleet: build, configured ceiling, load, last heartbeat |
| `POST` | `/api/v1/transfers/{transfer}:setPriority` | |
| `GET` | `/api/v1/transfers/{transfer}/logs` | Worker logs for this transfer, served by the Coordinator |

### Scheduled requests

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/v1/scheduledRequests` | List pending schedules |
| `GET` | `/api/v1/scheduledRequests/{id}` | Get one |
| `POST` | `/api/v1/scheduledRequests/{id}:cancel` | Cancel before it fires |

Schedules are created through `POST /transfers` with `scheduleAt` — one creation path, not two ([04](04-queue-and-scheduling.md) §10).

### Verifications and audit

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/v1/verifications` | List, filterable by package/state/stage |
| `GET` | `/api/v1/verifications/{id}` | Per-artifact detail |
| `GET` | `/api/v1/auditEvents` | Query the audit trail ([12](12-observability-and-audit.md) §4) |

### Workers and system

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/v1/workers` | Fleet status |
| `GET` | `/api/v1/workers/{worker}/logs` | Recent logs from one worker |
| `POST` | `/api/v1/workers:register` | // worker → Coordinator |
| `POST` | `/api/v1/workers/{worker}:heartbeat` | // liveness, lease renewal, cancellation signal |
| `POST` | `/api/v1/jobs:lease` | // lease a batch ([04](04-queue-and-scheduling.md) §4.1) |
| `POST` | `/api/v1/jobs/{job}:reportProgress` | // bytes transferred |
| `POST` | `/api/v1/jobs/{job}:complete` | // terminal outcome |
| `GET` | `/api/v1/system:healthCheck` | Deep dependency check ([13](13-cli.md)) |
| `GET` | `/api/v1/system/version` | Build info |
| `GET` | `/healthz` | Liveness — **process-local only** |
| `GET` | `/readyz` | Readiness — DB + config |
| `GET` | `/metrics` | Prometheus |

## 3. Listing

```http
GET /api/v1/products/vendor-a-platform/packages?pageSize=50&filter=state=discovered&orderBy=discoveredAt desc
```

```json
{
  "packages": [
    {
      "name": "products/vendor-a-platform/packages/v2.14.0",
      "packageId": "1042",
      "tag": "v2.14.0",
      "manifestDigest": "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
      "mediaType": "application/vnd.oci.image.index.v1+json",
      "totalBytes": "48533438464",
      "artifactCount": 5,
      "blobCount": 847,
      "state": "DISCOVERED",
      "discoveredAt": "2026-08-04T09:12:44Z",
      "targets": [
        {"target": "lab", "state": "VERIFIED",  "transferId": "9c1e…"},
        {"target": "production", "state": "NOT_TRANSFERRED"}
      ]
    }
  ],
  "nextPageToken": "eyJvZmZzZXQiOjUwfQ"
}
```

- **Cursor pagination, not offset.** `pageToken` is an opaque base64 cursor over `(discovered_at, id)`. Offset pagination on a table receiving inserts silently skips and duplicates rows as the underlying set shifts, which is exactly what a paging client must not experience.
- `pageSize` defaults to 50, caps at 1000.
- Enum values are `SCREAMING_SNAKE_CASE` in JSON (AIP-126), lowercase in the database ([03](03-persistence.md) §3). Converted at the boundary.
- **`int64` is serialized as a string** (`"48533438464"`) per AIP-141 — JSON numbers are IEEE-754 doubles and lose precision above 2^53. Byte counts here reach 10^11 today; the failure would be silent and rare enough to survive testing.

**Filter syntax** — a deliberately small subset of AIP-160: `field=value`, `field!=value`, `field>value`, `AND`. Parsed into parameterized SQL against an allowlist of filterable fields; never string-concatenated. A full CEL implementation is not warranted, and a permissive parser over a SQL builder is how injection happens.

## 4. Creating transfers

One endpoint covers replicate, promote, schedule, and dry run, because they differ only in fields.

```http
POST /api/v1/transfers
Content-Type: application/json
Idempotency-Key: 7c9e6679-7425-40de-944b-e07fc1f90ae7
X-Request-Id: 3f2504e0-4f89-11d3-9a0c-0305e82c3301

{
  "product": "vendor-a-platform",
  "package": "products/vendor-a-platform/packages/v2.14.0",
  "operation": "REPLICATE",
  "sourceRepository": "primary",
  "targets": ["lab", "staging"],
  "priority": 100,
  "scheduleAt": null,
  "verifyBeforeTransfer": true
}
```

```http
HTTP/1.1 201 Created
Location: /api/v1/transfers/9c1e8f2a-...
```

```json
{
  "requestId": "7c9e6679-7425-40de-944b-e07fc1f90ae7",
  "operation": "REPLICATE",
  "state": "EXPANDED",
  "priority": 100,
  "transfers": [
    {"transferId": "9c1e8f2a-…", "target": "lab",     "state": "PLANNING"},
    {"transferId": "a3d7b1c4-…", "target": "staging", "state": "PLANNING"}
  ],
  "createdAt": "2026-08-04T10:03:11Z"
}
```

**Two targets, two Transfers** ([01](01-domain-model.md) §3.2) — they succeed and fail independently.

### 4.1 Promotion

Same endpoint, `"operation": "PROMOTE"` and a `sourceRepository` naming a **target**. The API validates that the origin is a configured target and that no destination is `promotionOnly`-blocked from this origin. Everything downstream is identical ([05](05-transfer-engine.md) §6).

### 4.2 Dry run

```http
POST /api/v1/transfers?validateOnly=true
```

Returns `200 OK` with the plan and creates nothing ([05](05-transfer-engine.md) §7):

```json
{
  "validateOnly": true,
  "plans": [{
    "target": "lab",
    "artifactCount": 5,
    "blobCount": 847,
    "totalBytes": "48533438464",
    "alreadyPresentBytes": "12992892928",
    "alreadyPresentBlobs": 291,
    "mountableBytes": "0",
    "bytesToTransfer": "35540545536",
    "manifestCount": 5,
    "waveCount": 3,
    "estimatedDurationSeconds": 680,
    "estimateBasis": "EWMA over 14 recent transfers on this route"
  }]
}
```

`validateOnly` as a parameter rather than a `/transfers:dryRun` endpoint is AIP-163, and it is what guarantees the dry run exercises the same code as the real thing.

### 4.3 Why no Operation resource

AIP-151 defines a long-running-operation resource for asynchronous work. We do not use it: a **Transfer already is** the durable, addressable, pollable representation of the work, with richer state than a generic `Operation` (waves, per-job progress, dedupe accounting). Wrapping it in an `Operation` would add indirection and a second identifier for the same thing.

## 5. Control operations

```http
POST /api/v1/transfers/{transfer}:pause
POST /api/v1/transfers/{transfer}:setPriority   {"priority": 900}
POST /api/v1/transfers/{transfer}:cancel        {"reason": "superseded by v2.14.1"}
```

All return the updated Transfer. All are **idempotent**: pausing a paused transfer is `200`, not an error — a retried request after a timeout must not fail, and the caller's intent ("be paused") is satisfied either way.

Illegal transitions return `409 Conflict` with the current state ([10](10-state-machines.md) §1). Cancelling a `succeeded` transfer is a genuine conflict, not a no-op, because the caller's intent cannot be satisfied.

**Optimistic concurrency.** Mutations accept `If-Match` with the ETag from a prior `GET`. Mismatch is `412 Precondition Failed`. This is opt-in; without it, last-write-wins. It exists so a CLI that reads-then-modifies cannot silently clobber a concurrent change.

## 6. Progress

```http
GET /api/v1/transfers/9c1e8f2a-…
```

```json
{
  "transferId": "9c1e8f2a-…",
  "state": "RUNNING",
  "priority": 100,
  "progress": {
    "bytesTransferred": "18253611008",
    "bytesTotal": "35540545536",
    "percentComplete": 51.4,
    "jobsTotal": 561, "jobsSucceeded": 402, "jobsSkipped": 12,
    "jobsInFlight": 14, "jobsPending": 131, "jobsFailed": 2,
    "currentWave": 0, "maxWave": 2,
    "currentBytesPerSecond": "51380224",
    "averageBytesPerSecond": "47185920",
    "peakBytesPerSecond": "68157440",
    "elapsedSeconds": 387,
    "estimatedCompletionTime": "2026-08-04T10:16:22Z"
  },
  "dedupe": {"skippedBytes": "12992892928", "mountedBytes": "0", "savingsPercent": 26.8}
}
```

Every field is computed from `jobs` at request time (invariant I6). Nothing here is a maintained counter that could drift.

`GET /transfers/{id}/jobs?filter=state=FAILED` gives layer-level detail — digest, size, attempts, last error, worker, bytes transferred — which is what an operator needs when a transfer is stuck.

## 7. The worker plane

Workers speak only to these routes. They hold no database credentials ([00](00-overview.md) §5.2).

### 7.1 Lease

```http
POST /api/v1/jobs:lease
{"workerId": "worker-7d9f-x2k4", "capacity": 16, "activeJobs": 3}
```

```json
{
  "jobs": [{
    "jobId": "88431",
    "transferId": "9c1e8f2a-…",
    "kind": "BLOB",
    "digest": "sha256:4a5b…",
    "sizeBytes": "268435456",
    "source": {"registry": "registry.vendor-a.example.com", "repository": "platform/suite",
               "credentialsRef": "vendor-a-registry", "forceHttp1": true},
    "target": {"registry": "internal.azurecr.io", "repository": "vendor-a/platform",
               "credentialsRef": "internal-acr", "forceHttp1": true},
    "mountHint": {"eligible": false},
    "attempt": 1,
    "leaseExpiresAt": "2026-08-04T10:09:44Z"
  }],
  "grantedConcurrency": 12,
  "knownPlacements": ["sha256:1111…", "sha256:2222…"],
  "leaseDurationSeconds": 120,
  "nextPollAfterSeconds": 2
}
```

Three fields carry design weight:

- **`grantedConcurrency`** — the worker's share of fleet-wide per-repository budgets, computed centrally ([05](05-transfer-engine.md) §8). This is how adding workers does not multiply load on a vendor registry.
- **`knownPlacements`** — the placement fast path shipped with the batch, so resolving 16 jobs costs zero extra calls ([05](05-transfer-engine.md) §4.1).
- **`nextPollAfterSeconds`** — server-directed backoff. An idle worker is told when to come back, so an empty queue with 40 workers does not become a poll storm. Long-polling was considered; server-directed intervals are simpler and give the Coordinator direct control.

### 7.2 Progress and completion

```http
POST /api/v1/jobs/88431:reportProgress   {"workerId": "...", "bytesTransferred": "134217728"}

POST /api/v1/jobs/88431:complete
{"workerId": "…", "outcome": "SUCCEEDED", "bytesTransferred": "268435456",
 "durationMs": 5240, "skipReason": null}
```

Outcomes: `SUCCEEDED`, `SKIPPED` (with `skipReason`), `FAILED` (with `errorClass` and `errorMessage`), `CANCELLED`.

Completion is transactional: job state, transfer progress, `blob_placements` insert, wave-drain check, audit event, and any notification commit together ([04](04-queue-and-scheduling.md) §3.4). **This atomicity is the reason the queue lives in the same database as the state** ([03](03-persistence.md) §1).

Progress reports are throttled by the worker (every ~2 s or 5% of a blob, whichever is less frequent) — a 250 MB blob should produce a handful of reports, not thousands. They are also **lossy by design**: `reportProgress` is a best-effort UI signal, and dropping one costs nothing. `complete` is not.

### 7.3 Heartbeat

```http
POST /api/v1/workers/worker-7d9f-x2k4:heartbeat
{"activeJobIds": ["88431", "88432"], "cpuPercent": 34.2, "memoryBytes": "142606336"}
```

```json
{"leasesRenewed": ["88431","88432"], "cancelledJobIds": [], "drainRequested": false,
 "grantedConcurrency": 12}
```

Carries three signals in one call: **lease renewal** ([04](04-queue-and-scheduling.md) §4.3), **cancellation delivery** (§7.4), and **resource telemetry** feeding the adaptive controller ([11](11-resiliency-and-backpressure.md) §3).

### 7.4 Cancellation

There is no push channel to workers. Cancellation is delivered in the heartbeat response, so a cancelled job aborts within one heartbeat interval (≤20 s). The transfer sits in `cancelling` until in-flight jobs drain ([04](04-queue-and-scheduling.md) §8).

Polling rather than a push channel (WebSocket, gRPC stream, watch) is a deliberate simplification: it needs no connection management, no reconnect logic, and no server-side registry of live connections, and it degrades gracefully — a worker that cannot reach the Coordinator simply keeps working until its leases expire, which is the correct behaviour anyway.

## 8. Errors

RFC 9457 `application/problem+json`:

```json
{
  "type": "https://softwaregateway.io/errors/invalid-argument",
  "title": "Invalid argument",
  "status": 400,
  "detail": "targets[1]: 'production' is promotionOnly and cannot be a replication target",
  "code": "INVALID_ARGUMENT",
  "requestId": "3f2504e0-4f89-11d3-9a0c-0305e82c3301",
  "errors": [{"field": "targets[1]", "reason": "PROMOTION_ONLY_TARGET"}]
}
```

| `code` | HTTP | Meaning |
|---|---|---|
| `INVALID_ARGUMENT` | 400 | Malformed or semantically invalid |
| `NOT_FOUND` | 404 | Resource, or a product whose config failed to load ([02](02-configuration.md) §7) |
| `ALREADY_EXISTS` | 409 | Unique constraint |
| `FAILED_PRECONDITION` | 409 | Illegal state transition; includes `currentState` |
| `ABORTED` | 412 | `If-Match` mismatch |
| `RESOURCE_EXHAUSTED` | 429 | Server-side throttle; `Retry-After` set |
| `UNAVAILABLE` | 503 | Database or a critical dependency down; `Retry-After` set |
| `INTERNAL` | 500 | Bug. `detail` is generic; specifics go to logs keyed by `requestId` |

`detail` names the offending field and why. `"invalid request"` forces the user into the logs, which they usually cannot read.

**`requestId` is on every error and every response**, and is written to the audit trail ([03](03-persistence.md) §7). It is the thread from a user's failure to the server logs, the trace, and the audit record.

## 9. Health, readiness, and HPA

### 9.1 Probes

| Endpoint | Checks | Purpose |
|---|---|---|
| `/healthz` | Process responsive. **Nothing external.** | Liveness |
| `/readyz` | DB reachable, migrations applied, ≥1 product loaded | Readiness |
| `/api/v1/system:healthCheck` | Everything, including every registry, SMTP, Teams | Diagnostics |

> **`/healthz` must never check dependencies.** A liveness probe that fails when Postgres is briefly unavailable causes Kubernetes to restart every Coordinator — turning a recoverable database blip into a crash-loop across the fleet, at exactly the moment recovery needs the process alive to retry. Liveness answers "is this process wedged"; readiness answers "should it get traffic". Conflating them is one of the most common and most damaging Kubernetes mistakes, which is why it is called out here rather than left to reviewer instinct.

Deep checks live at a third endpoint precisely so they can be thorough and slow without a probe ever depending on them.

### 9.2 Scaling workers

`/metrics` exposes:

```
softwaregateway_queue_pending_jobs
softwaregateway_queue_pending_bytes
softwaregateway_workers_active
softwaregateway_queue_backlog_per_worker      # pending_jobs / max(workers_active, 1)
```

Scaled with `prometheus-adapter` on `backlog_per_worker`:

```yaml
metrics:
  - type: External
    external:
      metric: {name: softwaregateway_queue_backlog_per_worker}
      target: {type: AverageValue, averageValue: "20"}
```

**A ratio, not raw depth.** Raw `pending_jobs` does not converge: a target of "500 pending" is satisfied at 5 workers and at 50, so HPA oscillates. Backlog *per worker* is a proper control signal — it falls as replicas rise, which is what a controller needs to settle. Scale-down uses a long stabilization window (default 300 s) so that finishing a large package does not immediately kill workers that the next package will need. Full policy in [14](14-deployment-and-development.md) §4.

## 10. The authentication seam

**v1 ships with no authentication.** The Coordinator is unauthenticated behind a NetworkPolicy ([14](14-deployment-and-development.md) §3).

> **This is a real risk and is stated plainly:** anyone with network reach to the Coordinator can create, cancel, or re-prioritize transfers, and can read the full audit trail. The mitigating control is network isolation alone. Do not expose this service outside the cluster, and do not add an Ingress without first implementing §10.2.

### 10.1 What is already in place

The design is auth-shaped even though auth is absent, so adding it is not a refactor:

1. **Middleware position fixed.** The chain is `RequestID → Logging → Tracing → Metrics → Recovery → [AUTH] → Handler`, with a no-op authenticator in the slot today.
2. **Identity threaded through.** Handlers already take an `Identity` from the request context; today it is `{Subject: "anonymous", Roles: [admin]}`. Every mutating handler already records `identity.Subject` as the audit actor, so `requested_by` and `audit_events.actor` are populated and the schema needs no change.
3. **Roles defined and mapped.** Every route is already annotated with a required role.

| Role | Grants |
|---|---|
| `viewer` | All `GET`; no mutations |
| `operator` | `viewer` + create/pause/resume/cancel/retry/setPriority/verify/discover |
| `admin` | `operator` + worker-plane routes + audit export |

### 10.2 The intended implementation

Swap the no-op authenticator for one that accepts:

- **OIDC bearer tokens** for humans — validate signature against the IdP's JWKS, check `iss`/`aud`/`exp`, map a groups claim to roles.
- **Kubernetes ServiceAccount tokens** for in-cluster callers and workers — `TokenReview`, map the SA to a role.

Workers move to `admin` via their ServiceAccount. `transferctl` gains `--token` and a device-code login. **No route, handler, or schema changes** — the whole change is one middleware and a config block.

## 11. Versioning

`/api/v1` from the start. Within v1: adding fields, adding endpoints, and adding enum values are non-breaking; clients must ignore unknown fields and handle unknown enum values gracefully. Removing or renaming a field, or changing a type, requires `/api/v2`.

`GET /api/v1/system/version` returns build version, git commit, API version, and schema migration version — the four things needed to interpret a bug report.
