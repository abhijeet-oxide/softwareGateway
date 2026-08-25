# softwareGateway - System Design

A cloud-native platform that continuously discovers software packages published to vendor OCI registries and replicates them into internal registries - fast, resiliently, and with a durable record of what moved.

**Start here: [00 - Overview](00-overview.md).**

---

## Document set

| # | Document | Covers |
|---|---|---|
| 00 | [Overview](00-overview.md) | Problem, goals, non-goals, architecture, life of a package |
| 01 | [Domain Model](01-domain-model.md) | Product aggregate, entities, vocabulary, invariants |
| 02 | [Configuration](02-configuration.md) | Product ConfigMap schema, secrets, reload, validation |
| 03 | [Persistence](03-persistence.md) | Full DDL, indexes, dual dialect, retention, migrations |
| 04 | [Queue and Scheduling](04-queue-and-scheduling.md) | Dequeue SQL, waves, leases, priority, idempotency, scheduling |
| 05 | [Transfer Engine](05-transfer-engine.md) | Fast paths, streaming, transport tuning, dry run, promotion |
| 06 | [Registry Abstraction](06-registry-abstraction.md) | The `Repository` interface, vendor deltas, auth, capabilities |
| 07 | [Discovery](07-discovery.md) | Scanning, supersession, auto-download rules |
| 08 | [Verification](08-verification.md) | Cosign/Sigstore, stages, trust policy |
| 09 | [API](09-api.md) | AIP route table, worker plane, errors, probes, HPA, auth seam |
| 10 | [State Machines](10-state-machines.md) | Five transition tables and the guard |
| 11 | [Resiliency and Backpressure](11-resiliency-and-backpressure.md) | Failure matrix, AIMD, the cockroach test |
| 12 | [Observability and Audit](12-observability-and-audit.md) | Metric catalog, tracing, audit trail, notifications, alerts |
| 13 | [CLI](13-cli.md) | `transferctl` command tree and output |
| 14 | [Deployment and Development](14-deployment-and-development.md) | Flux, workloads, HPA, local dev |
| 15 | [Code Layout](15-code-layout.md) | Package structure, dependency rules |
| 16 | [Technology Choices](16-technology-choices.md) | **ADR-001** and the decision table |
| 17 | [Delivery Plan](17-delivery-plan.md) | Milestones, testing, open questions |
| 18 | [Quay Replication Strategies](18-quay-replication.md) | `copy` / `mirror` / `proxy` per target, Quay's own mechanisms, what each promises |
| 19 | [User Interface](19-user-interface.md) | Why v1 is CLI-only, what the UI must do, the gates before it ships |
| 20 | [Downloads and Auto-Download](20-download-rules.md) | What happens when software comes in, and when that happens by itself: derived chains, verification gates |
| 21 | [Security Posture](21-security-posture.md) | Is this release safer than the one it replaces: the Xray integration, the normalized model, comparison rules, caching |
| 22 | [Promotion](22-promotion.md) | Lab to production: the promoter plugins, native JFrog relocation, and why it is a plugin rather than a branch |

## Reading order

| If you are… | Read |
|---|---|
| **Wondering what this tool actually does** | **[Functional Overview](../FUNCTIONAL-OVERVIEW.md)** - start there, not here |
| Reviewing the architecture | 00 → 01 → 04 → 05 → 11 |
| Implementing the Coordinator | 03 → 04 → 09 → 10 → 07 |
| Implementing the Worker | 05 → 06 → 04 §4 → 11 |
| Implementing the CLI | 13 → 09 |
| Implementing Quay replication modes | 18 → 06 → 02 → 05 |
| Implementing downloads and auto-download | 20 → 07 → 18 → 10 → 08 |
| Implementing security posture | 21 → 06 → 02 → 03 → 09 |
| Building the UI | 19 → 09 → 13 |
| Operating it | 02 → 12 → 14 → 11 |
| Auditing the technology choices | 16 → 03 → 06 |

## The design in ten lines

- **Three binaries**: Coordinator (control plane), Worker (data plane), `transferctl` (CLI). One PostgreSQL database. Nothing else.
- **A worker never transfers a package** - it transfers one blob. That single choice gives fleet-wide parallelism, blob-sized failure blast radius, and stateless workers.
- **Bytes never touch disk and never traverse the Coordinator.** Worker memory is independent of blob size.
- **The queue is a Postgres table** consumed with `SKIP LOCKED`, so a job's state change and its transfer's progress commit in one transaction.
- **Recovery is the absence of state needing recovery.** Worker liveness is a lease timestamp; a `SIGKILL`ed pod needs no cleanup.
- **The fastest transfer is the one that does not happen.** Placement lookups and cross-repository mounts routinely eliminate 30–70% of a package.
- **Idempotency is structural** - unique constraints, not application logic.
- **Ordering is one integer.** Waves replace a dependency graph; a tag never appears before its blobs.
- **Backpressure is AIMD**, chosen over cleverer controllers because it can be reasoned about at 3 a.m.
- **The OCI client library decision** ([ADR-001](16-technology-choices.md#adr-001)) closed at M3 on `oras-go/v2`, for the write path only - and touched exactly one directory, which was the point of leaving it open behind an interface.

## Known limitations

Stated here so they are found before they are discovered.

| Limitation | Detail |
|---|---|
| **No API authentication in v1** | Unauthenticated behind a NetworkPolicy. Anyone with network reach can control transfers and read the audit trail. Seam designed ([09](09-api.md) §10); enabling it is a gate before any exposure beyond the cluster |
| Strict priority can starve | Low-priority work can be indefinitely delayed. Alerting first, optional aging second ([04](04-queue-and-scheduling.md) §6) |
| Chunked-upload resume is uneven | Registry support varies; treated as an optimization that may fail, never as correctness ([05](05-transfer-engine.md) §4.6) |
| Coordinator outage stops *new* leases | In-flight transfers continue. Mitigated by 2 replicas and long leases ([11](11-resiliency-and-backpressure.md) §2.2) |
| Notary Project signatures unsupported | Cosign only in v1; `Verifier` seam defined ([08](08-verification.md) §2) |
| No multi-tenancy | One organization, one deployment. Products are not a security boundary |
| SQLite is development-only | Not supported in production; the Coordinator warns at startup ([03](03-persistence.md) §2) |
| Delegated replication is observed, not measured | A `mirror` target reports a sync state and no byte progress; a `proxy` target holds nothing until someone pulls ([18](18-quay-replication.md) §6) |
| No UI in v1 | `transferctl` and Grafana are the interfaces. Direction and gates in [19](19-user-interface.md); the API-auth gate above blocks it |
| `warm` is not built | A proxy cache fills when a pod pulls. Populating one deliberately moves a whole release at line rate, so it belongs in the worker plane and needs a third `jobs.kind` ([18](18-quay-replication.md) §6.3) |
| A download cannot express an arbitrary workflow | The only ordering primitive is a content dependency the targets already declare. No conditionals, branches or user-defined steps - deliberately ([20](20-download-rules.md) §12) |
| An auto-download rule can only be turned off by a commit | Deliberate: a runtime override would be a second source of truth for whether a rule fires. During an incident the fast path is `transfers pause`, which stops the work rather than editing configuration ([20](20-download-rules.md) §9) |

## Requirement traceability

Every section of the original requirement, mapped to where it is specified. This table exists so coverage can be **checked** rather than asserted.

| Requirement | Specified in |
|---|---|
| Product-centric configuration | [01](01-domain-model.md) §1, [02](02-configuration.md) §4 |
| Source/target repos, credentials, CA, proxy in the Product | [02](02-configuration.md) §4–5 |
| Declarative, GitOps, ConfigMaps + Secrets, Flux | [02](02-configuration.md) §2–3, [14](14-deployment-and-development.md) §2 |
| Secrets via VSO, read as k8s Secrets | [02](02-configuration.md) §3, §5.5 |
| Generic OCI + ACR + Artifactory + Quay; extensible | [06](06-registry-abstraction.md) §6, §6.5 |
| Quay replication type selectable per target - copy, mirror, proxy | [18](18-quay-replication.md) §4–6 |
| A UI, after a CLI-first release, over the same API | [19](19-user-interface.md) |
| Continuous discovery, persisted, no duplicates | [07](07-discovery.md) §2–3, [03](03-persistence.md) §5 |
| Discovery → notifications, manual and auto download | [07](07-discovery.md) §5–6 |
| Regex auto-download rules | [02](02-configuration.md) §5.4, [07](07-discovery.md) §5 |
| A download triggered by CLI or UI, to one target or many, with the Quay step in the chain - and an auto-download rule that fires the same thing | [20](20-download-rules.md) §1.1, §3–4, §8 |
| Verification before and after a download, enabled or disabled per rule | [20](20-download-rules.md) §5 |
| A rule that can be turned off without a commit | [20](20-download-rules.md) §9 |
| Notification policy per product; events; email + Teams | [02](02-configuration.md) §4, [12](12-observability-and-audit.md) §5 |
| Download, replicate to one or many targets, concurrently | [01](01-domain-model.md) §3.2, [05](05-transfer-engine.md) §8, [09](09-api.md) §4 |
| 30–60 GB packages, thousands of layers | [05](05-transfer-engine.md) §1 |
| **Stream directly; never fully download to disk** | [05](05-transfer-engine.md) §4.3, invariant I5, [14](14-deployment-and-development.md) §3.2 |
| Promote between configured targets | [01](01-domain-model.md) §3.4, [05](05-transfer-engine.md) §6 |
| Scheduling; not GitOps; persisted; queue holds only executable work | [04](04-queue-and-scheduling.md) §10, [03](03-persistence.md) §6.2 |
| Package decomposed into layer jobs; workers collaborate | [04](04-queue-and-scheduling.md) §2 |
| Package-level progress over layer-level internals | [01](01-domain-model.md) §3.3, [09](09-api.md) §6 |
| Queue: priorities, pause, resume, cancel, retries, recovery | [04](04-queue-and-scheduling.md) §6, §8, §11, §12 |
| Stateless, dynamically scalable workers | [00](00-overview.md) §5.2, [14](14-deployment-and-development.md) §4 |
| Idempotency everywhere | [04](04-queue-and-scheduling.md) §7 |
| Existing blob ⇒ mark complete | [05](05-transfer-engine.md) §4.1, [10](10-state-machines.md) §4 (`skipped`) |
| Retries continue rather than restart | [04](04-queue-and-scheduling.md) §11, [05](05-transfer-engine.md) §4.6 |
| Content-addressed dedupe; reuse existing blobs | [01](01-domain-model.md) §4, [05](05-transfer-engine.md) §4.1 |
| Max throughput; concurrency at multiple levels | [05](05-transfer-engine.md) §5, §8 |
| Dynamic optimal concurrency | [11](11-resiliency-and-backpressure.md) §3 |
| Adaptive backpressure (latency, bandwidth, CPU, memory, storage) | [11](11-resiliency-and-backpressure.md) §3.3, §3.5 |
| Per-repository rate limits (up/down/conn/req) | [02](02-configuration.md) §5.3 |
| Cockroach resilience; tolerate every listed failure | [11](11-resiliency-and-backpressure.md) §2, §5 |
| Queue and transfer state survive restarts | [03](03-persistence.md), [04](04-queue-and-scheduling.md) §12 |
| Exponential backoff on layer failure | [10](10-state-machines.md) §6 |
| Durable persistence of all listed state | [03](03-persistence.md) §4–7 |
| Verification: package + image, source + destination, auto + on demand | [08](08-verification.md) §4 |
| SHA validation during transfer and verification | [05](05-transfer-engine.md) §4.4, [08](08-verification.md) §1 |
| Per-product trusted CAs | [02](02-configuration.md) §4, [06](06-registry-abstraction.md) §5 |
| Dry run: layers, size, existing blobs, ETA, planned ops | [05](05-transfer-engine.md) §7, [13](13-cli.md) §5 |
| CLI: all listed operations; talks only to the Coordinator | [13](13-cli.md) §2 |
| Health check validates every dependency | [13](13-cli.md) §3, [09](09-api.md) §9.1 |
| Metrics: discovery, active, depth, progress, speeds, elapsed, ETA, version | [12](12-observability-and-audit.md) §2 |
| Prometheus + OpenTelemetry | [12](12-observability-and-audit.md) §2–3 |
| Audit trail independent of logs, all listed events | [12](12-observability-and-audit.md) §4 |
| Configurable retention for all listed classes | [03](03-persistence.md) §8, [02](02-configuration.md) §4 |
| Explicit state machines; no implicit states | [10](10-state-machines.md) |
| Three binaries; Coordinator control plane, Workers data plane | [00](00-overview.md) §5 |
| `cmd/coordinator`, `cmd/worker`, `cmd/transferctl` | [15](15-code-layout.md) §1 |
| Domain-oriented Go layout | [15](15-code-layout.md) §1–3 |
| Single PostgreSQL; no Redis/Kafka/RabbitMQ | [03](03-persistence.md) §1, [16](16-technology-choices.md) §4 |
| Queue via Postgres locking | [04](04-queue-and-scheduling.md) §1, §4.1 |
| Mature libraries for all listed concerns | [16](16-technology-choices.md) §2 |
| Local dev without Kubernetes; SQLite default; Docker Compose Postgres | [14](14-deployment-and-development.md) §5 |
| Flux deployment: Deployments, PG, PVs, ConfigMaps, Secrets, Services | [14](14-deployment-and-development.md) §1–3 |
| Horizontal worker scaling without architectural change | [00](00-overview.md) §5.2, [14](14-deployment-and-development.md) §4 |
| Readiness and liveness probes; HPA API for workers | [09](09-api.md) §9, [14](14-deployment-and-development.md) §3–4 |
| Design philosophy priority ordering applied | [00](00-overview.md) §4, and every decision block |
