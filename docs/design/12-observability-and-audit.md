# 12 — Observability and Audit

> **Prerequisites:** [03 — Persistence](03-persistence.md), [09 — API](09-api.md)

Four distinct channels, deliberately not merged: **metrics** (aggregate, cheap, alertable), **traces** (per-request causality), **logs** (detail, ephemeral), **audit** (durable business record). Each answers a different question, and collapsing any pair produces something that answers neither well.

---

## 1. Metric conventions

Prometheus naming: `softwaregateway_` prefix, base units (bytes, seconds), `_total` on counters, `_seconds`/`_bytes` suffixes.

> **Cardinality is the constraint that shapes this catalog.** A Prometheus time series exists for every label-value combination, and a metric with a digest label would create one series per blob — millions, permanently, and a dead Prometheus. Digests are traced and logged; they are never metric labels.

| Never a label | Why | Where it lives instead |
|---|---|---|
| Blob or manifest digest | Unbounded | Traces, logs, audit |
| Transfer ID / job ID | Unbounded | Traces, logs, audit, API |
| Package tag | Grows without bound | Audit, API |
| Product `labels` from config | Operator-controlled, unbounded | Audit |
| Error message | Unbounded | Logs (`error_class` is the label) |

**Bounded labels only:** `product` (tens), `repository` (hundreds), `registry_type` (4), `state`/`outcome`/`error_class`/`skip_reason` (single digits), `direction` (2), `worker` (tens — the one to watch under aggressive HPA, and the reason worker-labelled metrics are kept to a handful).

## 2. Metric catalog

### 2.1 Discovery

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `discovery_scans_total` | counter | `repository`, `outcome` | Scans run |
| `discovery_packages_discovered_total` | counter | `product`, `repository` | **New packages** — a required metric |
| `discovery_packages_superseded_total` | counter | `product` | Vendor re-pushed a tag |
| `discovery_scan_duration_seconds` | histogram | `repository` | |
| `discovery_last_success_timestamp_seconds` | gauge | `repository` | **The one to alert on** |
| `discovery_errors_total` | counter | `repository`, `error_class` | |

> `discovery_last_success_timestamp_seconds` catches the dangerous failure, which is not "discovery is erroring loudly" but "discovery quietly stopped finding anything" — a silently-expired credential, a paused loop, a leader that never took over. Alert on staleness, not on error rate; an error rate of zero is exactly what this failure looks like.

### 2.2 Queue

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `queue_pending_jobs` | gauge | `priority_band` | Leasable now ([04](04-queue-and-scheduling.md) §10) |
| `queue_pending_bytes` | gauge | — | Work remaining |
| `queue_blocked_jobs` | gauge | — | Waiting on a wave |
| `queue_leased_jobs` | gauge | — | In flight |
| `queue_backlog_per_worker` | gauge | — | **HPA signal** ([09](09-api.md) §9.2) |
| `queue_oldest_pending_age_seconds` | gauge | `priority_band` | **Starvation detector** ([04](04-queue-and-scheduling.md) §6) |
| `queue_lease_duration_seconds` | histogram | — | Dequeue cost; trigger for the index escalation ([04](04-queue-and-scheduling.md) §4.2) |
| `queue_lease_expirations_total` | counter | — | Worker deaths |
| `queue_job_retries_total` | counter | `error_class` | |
| `queue_dead_letter_jobs` | gauge | — | Terminal failures awaiting a human |

### 2.3 Transfer — the throughput metrics

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `transfers_active` | gauge | `product` | **Active transfers** — required |
| `transfer_bytes_total` | counter | `product`, `direction`, `registry_type` | Bytes actually moved |
| `transfer_throughput_bytes_per_second` | gauge | `source_registry`, `target_registry` | **Current speed** — required; per route, since that is what varies |
| `transfer_peak_throughput_bytes_per_second` | gauge | — | **Peak speed** — required |
| `transfer_duration_seconds` | histogram | `operation` | Elapsed, completed transfers |
| `transfer_progress_ratio` | gauge | `product` | 0–1 |
| `transfer_eta_seconds` | gauge | `product` | Estimated completion |
| `transfers_completed_total` | counter | `product`, `operation`, `outcome` | |
| `blob_transfer_duration_seconds` | histogram | `size_bucket` | Bucketed size, not raw — cardinality |

**Average speed** is `rate(transfer_bytes_total[5m])` — derived in PromQL rather than stored, because a stored average is a stored decision about a window, and different questions want different windows.

### 2.4 Deduplication — the value metrics

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `dedupe_skipped_blobs_total` | counter | `skip_reason` | `placement_hit`, `exists_at_target`, `mounted` |
| `dedupe_skipped_bytes_total` | counter | `skip_reason` | **Bandwidth saved** |
| `dedupe_hit_ratio` | gauge | `target_repository` | Skipped ÷ planned |
| `mount_attempts_total` | counter | `result` | `mounted`, `declined`, `unsupported` |
| `placement_invalidations_total` | counter | — | Stale placements caught ([11](11-resiliency-and-backpressure.md) §2.5) |

> These justify the system's existence. `dedupe_skipped_bytes_total` is the headline number in any report on whether this tool was worth building — and, more practically, a sudden drop in `dedupe_hit_ratio` is the earliest signal that a target registry's garbage collector is deleting content we rely on.

### 2.5 Backpressure, registries, workers, verification

| Metric | Type | Labels |
|---|---|---|
| `repository_concurrency_limit` | gauge | `repository`, `direction` |
| `repository_concurrency_in_use` | gauge | `repository`, `direction` |
| `repository_limit_adjustments_total` | gauge→counter | `repository`, `action` (`increase`/`decrease`) |
| `registry_requests_total` | counter | `repository`, `method`, `status_class` |
| `registry_request_duration_seconds` | histogram | `repository`, `method` |
| `registry_rate_limited_total` | counter | `repository` |
| `workers_active` | gauge | — |
| `worker_jobs_in_flight` | gauge | `worker` |
| `worker_granted_concurrency` | gauge | `worker` |
| `upload_resume_total` | counter | `repository`, `result` — validates §4.6 assumptions empirically |
| `verifications_total` | counter | `product`, `stage`, `outcome` |
| `packages_unverified` | gauge | `product` — products running with verification off ([08](08-verification.md) §2) |
| `verification_policy_warn` | gauge | `product` — products verifying under `warn` rather than `enforce` ([08](08-verification.md) §4) |

### 2.6 Coordinator and configuration

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `config_products_loaded` | gauge | — | Products currently valid and loaded ([02](02-configuration.md) §6) |
| `config_load_errors` | gauge | `product` | Products whose latest config failed validation ([02](02-configuration.md) §7) |
| `config_last_reload_timestamp_seconds` | gauge | — | Last successful reload |
| `leader_elected` | gauge | — | `1` on the leader, `0` on followers. Backs the `CoordinatorNoLeader` alert (§7) |
| `api_requests_total` | counter | `route`, `method`, `status_class` | `route` is the **template** (`/api/v1/transfers/{transfer}`), never the populated path — cardinality |
| `api_request_duration_seconds` | histogram | `route`, `method` | |
| `gc_rows_deleted_total` | counter | `table` | Retention progress ([03](03-persistence.md) §8) |
| `notifications_sent_total` | counter | `channel_type`, `outcome` | |
| `notification_outbox_pending` | gauge | — | Undelivered notifications; sustained growth means a broken channel |

`config_load_errors` is worth alerting on. A product whose config fails validation keeps running on its **previous** valid version ([02](02-configuration.md) §7), which is the right behaviour and also means a broken config edit can go unnoticed indefinitely — the system stays healthy while quietly ignoring what someone merged.

### 2.6.1 Delegated replication (proposed — the mechanism landed at M8, these did not)

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `mirror_sync_total` | counter | `product`, `target`, `result` | Observed Quay mirror syncs ([18](18-quay-replication.md) §7) |
| `mirror_sync_duration_seconds` | histogram | `product`, `target` | As reported by Quay, not measured by us |
| `mirror_config_drift` | gauge | `product`, `target` | `1` when the registry's configuration differs from Git ([18](18-quay-replication.md) §8) |
| `proxy_cache_probe_total` | counter | `product`, `target`, `result` | Whether a package is currently cached |

There is deliberately **no** byte or throughput metric for a delegated target. We do not move those bytes and cannot count them; a gauge that looked like throughput but was derived from elapsed time would be worse than the absence of one ([18](18-quay-replication.md) §6.1).

### 2.6.2 Download rules (proposed, M9)

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `download_rule_matches_total` | counter | `product`, `rule` | Packages a rule matched. High cardinality in time, not in labels — this is the metric that exists *instead of* an audit event ([20](20-download-rules.md) §10) |
| `download_run_total` | counter | `product`, `rule`, `trigger`, `result` | Runs, by how they were triggered and how they ended |
| `download_step_skipped_total` | counter | `product`, `rule`, `target` | Steps that never ran because a predecessor did not succeed ([20](20-download-rules.md) §6) |
| `download_rule_suspended` | gauge | `product`, `rule` | `1` while an operational suspension is in force ([20](20-download-rules.md) §9) |

`download_rule_suspended` is an alerting target, not a dashboard tile: a suspension that outlives the incident it was declared for is the failure mode this metric exists to catch.

### 2.7 Build info

```
softwaregateway_build_info{version="1.4.2", commit="a1b2c3d", go_version="go1.24.0", component="coordinator"} 1
```

The constant-1 gauge pattern — **tool version** as required, and the standard way to correlate a behaviour change with a deployment on a Grafana dashboard.

## 3. Tracing

OpenTelemetry, OTLP/gRPC, sampled (default 5%; errors always sampled).

```
transfer.execute                                    [coordinator]
├── transfer.plan
│   ├── registry.fetch_manifest        registry, digest
│   ├── registry.walk_tree             artifact_count
│   └── db.classify_blobs              blob_count, placement_hits
├── job.lease                          worker_id, batch_size
├── job.execute                        [worker]  ← context propagated
│   ├── blob.check_placement           result
│   ├── registry.stat_blob             registry, digest
│   ├── registry.mount_blob            result
│   ├── blob.stream                    bytes, duration
│   │   ├── registry.fetch_blob        source
│   │   └── registry.push_blob         target, chunked
│   └── job.complete                   outcome
├── wave.advance                       from_wave, to_wave
└── verification.verify                [coordinator]
```

**Trace context propagates Coordinator → worker → registry** via W3C `traceparent`, injected into the lease response and into outbound registry requests. A trace therefore covers a job end to end across two processes — which is the whole reason to have tracing here, since the interesting failures span the boundary.

Digests, transfer IDs, and job IDs live on spans as attributes. Traces are where high-cardinality identifiers belong (§1).

Sampling is per *transfer*, not per span, so a sampled transfer yields a complete trace rather than a scattering of unrelated spans.

## 4. Audit trail

> **Requirement: a persistent audit history independent of application logs.**

Separate from logs because they answer different questions and have different lifetimes: logs are ephemeral, high-volume, and shipped to a cluster-wide aggregator; audit is durable, low-volume, queryable through the API, and retained for a year ([03](03-persistence.md) §8).

Written **in the same transaction as the change it records** ([10](10-state-machines.md) §8, S6). It is therefore impossible to perform an audited action without the audit record, or to record one that did not happen. A logging call after the commit could not offer that.

### 4.1 Event catalog

| Category | Events |
|---|---|
| Discovery | `PackageDiscovered`, `PackageSuperseded`, `DiscoveryFailed`, `DiscoveryTriggered` |
| Request | `TransferRequested`, `TransferScheduled`, `ScheduleCancelled`, `AutoDownloadTriggered` |
| Queue | `TransferPlanned`, `TransferStarted`, `TransferPaused`, `TransferResumed`, `TransferCancelled`, `PriorityChanged` |
| Execution | `JobFailed`, `JobRetried`, `JobDeadLettered`, `WaveAdvanced` |
| Completion | `TransferCompleted`, `TransferFailed`, `PromotionCompleted` |
| Verification | `VerificationStarted`, `VerificationPassed`, `VerificationFailed`, `VerificationError` |
| Config | `ConfigReloaded`, `ConfigLoadFailed`, `ProductAdded`, `ProductRemoved` |
| Replication (proposed, M8) | `ReplicationConfigApplied`, `ReplicationConfigDrifted`, `MirrorSyncRequested`, `MirrorSyncSucceeded`, `MirrorSyncFailed`, `MirrorContentDiverged`, `ProxyCacheConfigured`, `CacheWarmed` ([18](18-quay-replication.md) §7) |
| Download (proposed, M9) | `DownloadRunRequested`, `DownloadRunCompleted`, `DownloadStepSkipped`, `DownloadRuleSuspended`, `DownloadRuleResumed` ([20](20-download-rules.md) §10). A rule *match* is deliberately not audited — it happens for every package on every scan, and burying five real events under thousands of routine ones is how an audit trail stops being read |
| System | `LeadershipAcquired`, `LeadershipLost`, `MigrationApplied`, `RetentionApplied` |

Not every job transition is audited — 850 `JobSucceeded` rows per package would bury the events a human cares about. **Individual job successes are metrics; job failures and user actions are audit.** The distinction is whether a human would ever ask about that specific occurrence.

### 4.2 Record

```json
{
  "id": "48211",
  "occurredAt": "2026-08-04T10:03:11.482Z",
  "eventType": "TransferRequested",
  "actor": "anonymous",
  "actorKind": "USER",
  "productName": "vendor-a-platform",
  "subjectKind": "transfer_request",
  "subjectId": "7c9e6679-7425-40de-944b-e07fc1f90ae7",
  "requestId": "3f2504e0-4f89-11d3-9a0c-0305e82c3301",
  "traceId": "4bf92f3577b34da6a3ce929d0e0e4736",
  "outcome": "SUCCESS",
  "detail": {
    "package": "v2.14.0", "manifestDigest": "sha256:9f86d081…",
    "targets": ["lab","staging"], "priority": 100, "origin": "cli"
  }
}
```

`actor` is `"anonymous"` in v1 because there is no authentication ([09](09-api.md) §10). **The field, the plumbing, and the recording already exist**, so enabling auth populates real identities with no schema change and no handler change — an audit trail retrofitted afterwards would have a year of unattributable history.

`requestId` and `traceId` are the join keys between audit, logs, and traces.

Queried via `GET /api/v1/auditEvents?filter=…` ([09](09-api.md) §2), typically by subject: *everything that ever happened to this package*.

## 5. Notifications

Event-driven, product-scoped ([02](02-configuration.md) §4), delivered through the transactional outbox ([03](03-persistence.md) §7).

| Event | Fires when | Typical audience |
|---|---|---|
| `PackageDiscovered` | New package | Teams |
| `TransferCompleted` | All targets succeeded | Teams |
| `PromotionCompleted` | Promotion succeeded | Teams |
| `TransferFailed` | A transfer failed terminally | Teams + email |
| `VerificationFailed` | Signature verification failed | Teams + email |
| `DiscoveryFailed` | Auth failure or sustained unreachability | Email |

**The outbox pattern, and why it is the right amount of machinery:**

```
  state change  ──┐
                  ├── ONE transaction ──► committed
  outbox insert ──┘
                            │
                            ▼  separate loop, leader-only
                    SELECT ... WHERE state='pending' AND next_visible_at <= now()
                      FOR UPDATE SKIP LOCKED
                            │
                            ▼
                    send → sent | retry with backoff | failed after 5 attempts
```

Guarantees, each of which matters:
- **No lost notifications.** Committed with the fact, not sent on a best-effort basis afterwards.
- **No phantom notifications.** A rolled-back transaction takes its notification with it.
- **No duplicates.** `UNIQUE (dedupe_key)` ([03](03-persistence.md) §7) — a retried state transition cannot double-send.
- **No coupling.** SMTP being down retries the notification and never touches the transfer ([11](11-resiliency-and-backpressure.md) §4).

**Channels.** Email via SMTP. Teams via **Adaptive Cards posted to a Power Automate workflow URL** — *not* a legacy O365 connector webhook, which is retired ([16](16-technology-choices.md)). This is a live trap: most tutorials still show the old `outlook.office.com/webhook/...` form, and code written against it will fail in current tenants.

## 6. Logging

`log/slog`, JSON, structured. Levels: `error` (needs a human), `warn` (recovered, e.g. a retry), `info` (lifecycle), `debug` (per-job detail, off by default).

Every log line carries the correlation keys that make the four channels one story:

```json
{"time":"2026-08-04T10:07:33.104Z","level":"INFO","msg":"blob transfer completed",
 "component":"worker","worker_id":"worker-7d9f-x2k4",
 "request_id":"3f2504e0-…","trace_id":"4bf92f35…",
 "transfer_id":"9c1e8f2a-…","job_id":"88431",
 "digest":"sha256:4a5b…","bytes":268435456,"duration_ms":5240,"throughput_mibs":48.8}
```

Worker logs are additionally shipped to the Coordinator on the control channel and retained briefly in `worker_logs` ([03](03-persistence.md) §7) so `transferctl logs` works without the CLI ever contacting a worker ([00](00-overview.md) §5.3). **This is a convenience tail, not a log store** — deliberately small, aggressively GC'd, and explicitly not a replacement for cluster log aggregation.

Credentials cannot be logged: they are wrapped in a redacting type whose `String()`/`LogValue()` return `[REDACTED]` ([02](02-configuration.md) §5.5), so even a careless `slog.Any("cfg", cfg)` is safe.

## 7. Dashboards and alerts

**Dashboards** (shipped as Grafana JSON in `deploy/observability/`):

1. **Fleet** — active transfers, aggregate throughput, queue depth, worker count, backlog per worker.
2. **Transfer detail** — per-transfer progress, per-route throughput, dedupe ratio, retry rate.
3. **Registry health** — per-repository latency p50/p95/p99, error rate, 429 rate, adaptive limit vs. configured ceiling.
4. **Supply chain** — verification outcomes, unverified products, signature coverage.

**Alerts**, and specifically the ones worth having:

| Alert | Condition | Why |
|---|---|---|
| DiscoveryStale | `time() - discovery_last_success_timestamp_seconds > 4 × interval` | Silent discovery failure (§2.1) |
| QueueStarvation | `queue_oldest_pending_age_seconds > 6h` | Priority misconfiguration ([04](04-queue-and-scheduling.md) §6) |
| DeadLetterGrowing | `queue_dead_letter_jobs > 0 for 1h` | Failures nobody has looked at |
| VerificationFailure | `increase(verifications_total{outcome="failed"}[15m]) > 0` | **Page.** Security event |
| RegistryDegraded | `rate(registry_requests_total{status_class="5xx"}[5m]) > 0.1` | |
| DedupeRatioDropped | `dedupe_hit_ratio < 0.5 × avg_over_time(…[7d])` | Target registry GC removing content we rely on |
| CoordinatorNoLeader | `absent(leader_elected == 1) for 2m` | Background loops stopped |

`VerificationFailure` pages; the rest ticket. A signature that does not verify is the one condition here that could indicate an attack, and it is distinguished from `VerificationError` — a Sigstore outage — precisely so this alert means what it says ([08](08-verification.md) §8).
