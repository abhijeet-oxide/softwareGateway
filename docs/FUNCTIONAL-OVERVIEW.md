# softwareGateway — Functional Overview

> **Audience:** anyone who wants to understand what this tool *does* and what it is like to live with — stakeholders, new joiners, operators.
> **Not this document:** how to build it. That is the 18-document set in [`docs/design/`](design/README.md), written for implementers.
>
> **On "microservices": there are none, deliberately.** Three binaries and one PostgreSQL database. [16 §4](design/16-technology-choices.md) lists what was rejected — Redis, Kafka, controller-runtime, gRPC, a service mesh, a workflow engine — and why. What follows is the *logical* decomposition: the domain modules inside those three binaries and how they interact.

---

## 1. What it does

Software vendors publish their products into OCI registries. A "software package" is one tag that resolves to an index containing container images, Helm charts, configuration bundles, signatures, and SBOMs — routinely **30–60 GB across hundreds to thousands of blobs**.

softwareGateway watches those vendor registries and moves what it finds into yours.

| Verb | What it means |
|---|---|
| **Discover** | Continuously notice when a vendor publishes something new, and record it exactly once |
| **Replicate** | Move a package from a vendor registry into one or more internal registries |
| **Promote** | Move a package between internal registries — lab → production |
| **Verify** | Prove, with cosign/Sigstore, that what landed is what the vendor signed |

A Red Hat Quay destination can also **delegate** the move: instead of our workers pushing into it, Quay can be configured to mirror from an upstream on a schedule, or to act as a proxy cache that fills when a pod pulls. That buys convergence which keeps working while this tool is down, at the cost of byte-level progress and exact timing — so a delegated target reports a *state* rather than a percentage, and the choice is per target. Which mode to pick, and why, is [18 — Quay Replication Strategies](design/18-quay-replication.md).

Most estates do not perform those four verbs one at a time. A release goes vendor → JFrog → Quay, verified at each end, and the sequence is the same every time — so it can be **declared once** as a download rule and run by anyone, from the CLI or later the UI, with the steps ordered, the verification acting as a gate, and nothing reaching the cluster's registry if what landed in storage did not verify. That is [20 — Download Rules](design/20-download-rules.md).

**The concrete outcome:** a 45 GB vendor release is discovered within 15 minutes of publication, replicated into your lab registry in about 11 minutes — of which roughly a quarter never crosses the network because you already had those layers — verified against the vendor's signing identity, and recorded in an audit trail that can answer "what did we ship in March" a year later.

### Why not just script it

`crane copy` in a CronJob gets you a copy. It does not get you:

- a record that a package was ever discovered, or a way to notice you missed one;
- resumption — an interrupted 60 GB copy starts over;
- deduplication — the same base layer moves again for every product that uses it;
- an audit trail, or per-package progress, or a way to pause and reprioritize;
- throughput beyond what a single process can push.

Those five gaps are the reason this exists.

---

## 2. Logical components

Three binaries. Bytes flow along exactly one path; everything else moves records.

```
                  ┌────────────────────────────────────────────────────┐
  transferctl ───►│                   COORDINATOR                       │
  (CLI — pure     │                  (control plane)                    │
   API client)    │                                                     │
                  │   api ──────── HTTP surface, all routes             │
                  │    │                                                │
                  │    ├── product ──── config load, validate, watch    │
                  │    ├── discovery ── scan sources, dedupe, rules     │
                  │    ├── scheduler ── due-time expansion, loops       │
                  │    ├── transfer ─── PLANNER half: walk, classify    │
                  │    ├── queue ────── jobs, leases, waves, budgets    │
                  │    ├── verification ─ cosign trust policy           │
                  │    ├── notification ─ outbox, email, teams          │
                  │    └── audit ────── durable event record            │
                  │                    │                                │
                  │                 store ── sole DB writer             │
                  └────────────────────┬───────────────┬────────────────┘
                                       │ SQL           │ HTTP: lease,
                                       ▼               │ heartbeat,
                              ┌─────────────────┐      │ progress, complete
                              │   PostgreSQL    │      │ (records only —
                              │  queue · state  │      │  never blob bytes)
                              │     audit       │      │
                              └─────────────────┘      ▼
                                              ┌──────────────────────┐
                                              │       WORKER         │
                                              │    (data plane)      │
                                              │   stateless  ×N      │
                                              │                      │
                                              │  worker ── lease loop│
                                              │  transfer ─ ENGINE   │
                                              │  registry ─ OCI I/O  │
                                              └───┬──────────────┬───┘
                                                  │              │
                                        GET blob  │              │  PUT blob
                                                  ▼              ▼
                                      ┌───────────────┐  ┌───────────────┐
                                      │    SOURCE     │  │    TARGET     │
                                      │   REGISTRY    │  │   REGISTRY    │
                                      └───────────────┘  └───────────────┘
                                          ═══ GIGABYTES FLOW HERE ═══
                                             and nowhere else
```

**Two properties this picture exists to make obvious:**

1. **Artifact bytes never enter the Coordinator, the database, or worker disk.** A worker opens a read against the source and pipes it straight into a write against the target. The Coordinator moves job records measured in bytes while workers move blobs measured in gigabytes.
2. **Workers hold no database credentials and contain no SQL.** The `store` module sits entirely inside the Coordinator boundary. Nothing crosses from `worker` to it — workers ask the Coordinator for work over HTTP.

### Module reference

| Module | Binary | Owns | Talks to | Spec |
|---|---|---|---|---|
| `api` | coordinator | Routing, DTOs, middleware, HTTP semantics | every domain module | [09](design/09-api.md) |
| `product` | coordinator | Config schema, loading, validation, hot reload | `store`, filesystem | [02](design/02-configuration.md) |
| `discovery` | coordinator | Scanning, package identity, auto-download rules | `registry`, `store`, `notification` | [07](design/07-discovery.md) |
| `scheduler` | coordinator | Due-time expansion, leader-gated loops | `queue`, `transfer` | [04 §10](design/04-queue-and-scheduling.md) |
| `transfer` (planner) | coordinator | Manifest walk, dedupe classification, waves, dry run | `registry`, `store` | [05 §3](design/05-transfer-engine.md) |
| `transfer` (engine) | **worker** | The four fast paths, streaming, digest verify | `registry` | [05 §4](design/05-transfer-engine.md) |
| `queue` | coordinator | Job lifecycle, leasing, priority, retry, budgets | `store` | [04](design/04-queue-and-scheduling.md) |
| `registry` | both | All OCI I/O, auth, rate limiting, capabilities | vendor + internal registries | [06](design/06-registry-abstraction.md) |
| `verification` | coordinator | Signature verification, trust policy | `registry`, Sigstore | [08](design/08-verification.md) |
| `notification` | coordinator | Outbox drain, email, Teams | SMTP, Power Automate | [12 §5](design/12-observability-and-audit.md) |
| `audit` | coordinator | Durable event record and query | `store` | [12 §4](design/12-observability-and-audit.md) |
| `worker` | **worker** | Lease loop, local concurrency, heartbeat, reporting | Coordinator API | [09 §7](design/09-api.md) |
| `store` | coordinator | SQL, both dialects, transaction boundaries | PostgreSQL / SQLite | [03](design/03-persistence.md) |
| `platform` | all | Config, logging, metrics, tracing, backoff, state machine, leader election | — | [15](design/15-code-layout.md) |

Two Coordinator replicas run for API availability; **one holds a `pg_advisory_lock` and runs the background loops** (discovery, scheduler, reaper, outbox, GC, backpressure). The other serves reads and waits.

---

## 3. Code folder structure

Every significant file with its purpose and the design document that specifies it. The cross-reference column is the point — it makes 39,000 words of design navigable *from the code*, which is what stops documentation going stale.

```
softwareGateway/
│
├── cmd/                                    ── three entry points, wiring only
│   ├── coordinator/main.go                 Construct, inject, run the control plane
│   ├── worker/main.go                      Construct, inject, run the data plane
│   └── transferctl/
│       ├── main.go
│       ├── root.go                         Global flags; flag→env→file precedence  (13 §10)
│       ├── health.go                       Deep dependency check                   (13 §3)
│       ├── products.go                     list / describe
│       ├── packages.go                     list / describe / discover              (13 §4)
│       ├── download.go                     Replicate; --dry-run, --at, --watch     (13 §5)
│       ├── promote.go                      Target → target                         (13 §5)
│       ├── verify.go                       On-demand verification                  (13 §5)
│       ├── transfers.go                    list / describe / jobs / logs           (13 §6)
│       ├── control.go                      pause / resume / cancel / retry / priority (13 §7)
│       ├── schedules.go                    list / cancel
│       ├── workers.go                      list / logs
│       ├── audit.go                        Query the audit trail
│       ├── config.go                       view / validate — runs in CI            (13 §9)
│       └── output/                         table · json · yaml · wide renderers    (13 §1)
│
├── internal/                               ── not importable outside this module
│   │
│   ├── product/                            ── configuration domain
│   │   ├── product.go                      Product aggregate; in-memory registry
│   │   ├── schema.go                       apiVersion/kind document types          (02 §4)
│   │   ├── loader.go                       Read directory, parse, resolve refs     (02 §3)
│   │   ├── validate.go                     Fail-closed per product                 (02 §7)
│   │   ├── watch.go                        fsnotify + debounce + atomic swap       (02 §6)
│   │   └── secrets.go                      Mounted-file secrets; redacting type    (02 §5.5)
│   │
│   ├── discovery/                          ── find new packages
│   │   ├── loop.go                         Per-source goroutine; backoff; leader   (07 §1,§7)
│   │   ├── scanner.go                      Full scan, no cursor                    (07 §2,§3)
│   │   ├── resolve.go                      HEAD tag → manifest digest              (07 §2)
│   │   ├── record.go                       ON CONFLICT insert; supersession        (07 §4)
│   │   └── rules.go                        Auto-download; first match wins         (07 §5)
│   │
│   ├── transfer/                           ── planner (coordinator) + engine (worker)
│   │   ├── planner.go                      Package + target → job set              (05 §3)
│   │   ├── walk.go                         BFS manifest tree; distinct blob set    (05 §3)
│   │   ├── classify.go                     placement / mountable / stream          (05 §3)
│   │   ├── waves.go                        Topological depth → wave integer        (04 §3.2)
│   │   ├── engine.go                       Per-job execution: the four fast paths  (05 §4)
│   │   ├── stream.go                       The copy loop — inline digest, no disk  (05 §4.3)
│   │   ├── mount.go                        Cross-repo mount; 202 fallback          (05 §4.2)
│   │   ├── upload.go                       Monolithic vs chunked; resume           (05 §4.6)
│   │   ├── manifest.go                     Manifest push; BLOB_UNKNOWN self-heal   (05 §9)
│   │   ├── dryrun.go                       Renders the plan — same planner         (05 §7)
│   │   ├── estimate.go                     EWMA per route → ETA                    (05 §7)
│   │   └── progress.go                     Counting reader; throttled reporting    (09 §7.2)
│   │
│   ├── registry/                           ── all OCI I/O
│   │   ├── registry.go                     THE Repository interface                (06 §2)
│   │   ├── descriptor.go                   Our Descriptor — no library types leak  (06 §2)
│   │   ├── capabilities.go                 Probe and cache per registry            (06 §3)
│   │   ├── errors.go                       Sentinel classification                 (06 §7)
│   │   ├── factory.go                      registry_type → constructor             (06 §6.5)
│   │   ├── transport/
│   │   │   ├── transport.go                Layered client; h1 forced for blobs     (05 §5)
│   │   │   ├── auth.go                     OCI bearer token flow                   (06 §4)
│   │   │   ├── tokencache.go               Per scope, single-flight refresh        (06 §4)
│   │   │   ├── ratelimit.go                Token bucket — outermost layer          (06 §5)
│   │   │   ├── retry.go                    Backoff; honours Retry-After            (06 §5)
│   │   │   ├── tls.go                      Per-product CA pool                     (02 §4)
│   │   │   └── proxy.go                    Per-product proxy                       (02 §4)
│   │   ├── generic/                        ── the default path for all registries
│   │   │   ├── repository.go               OCI Distribution v2                     (06 §6.1)
│   │   │   ├── blobs.go                    Stat / fetch / push / mount / resume
│   │   │   ├── manifests.go                Fetch verbatim; push raw; tag
│   │   │   ├── tags.go                     Link-header pagination                  (07 §2)
│   │   │   └── referrers.go                OCI 1.1 + tag-schema fallback           (08 §3)
│   │   ├── acr/acr.go                      AAD exchange — delta only               (06 §6.2)
│   │   ├── artifactory/artifactory.go      Repo-key paths, pagination              (06 §6.3)
│   │   └── quay/quay.go                    Robot accounts, rate headers            (06 §6.4)
│   │
│   ├── queue/                              ── the heart
│   │   ├── queue.go                        Job lifecycle facade
│   │   ├── lease.go                        Dequeue SQL; in-flight suppression      (04 §4.1,§5)
│   │   ├── reaper.go                       Expired-lease sweep                     (04 §4.3)
│   │   ├── waves.go                        Drain check + bulk advance              (04 §3.3)
│   │   ├── control.go                      Pause / resume / cancel / retry         (04 §8)
│   │   ├── priority.go                     Bulk reprioritize; optional aging       (04 §6)
│   │   ├── retry.go                        Error class → attempts + backoff        (10 §6)
│   │   └── budget.go                       Fleet-wide budget ÷ active workers      (05 §8)
│   │
│   ├── scheduler/
│   │   ├── scheduler.go                    Due-time tick                           (04 §10)
│   │   ├── expand.go                       Request → transfers → jobs
│   │   └── loops.go                        Leader-gated loop registry              (04 §9)
│   │
│   ├── verification/
│   │   ├── verifier.go                     Verifier interface — the seam           (08 §6)
│   │   ├── cosign.go                       Keyed and keyless                       (08 §2)
│   │   ├── discover.go                     Signature discovery                     (08 §3)
│   │   ├── policy.go                       Trust policy from product config        (08 §7)
│   │   └── record.go                       Per-artifact results                    (08 §6)
│   │
│   ├── notification/
│   │   ├── outbox.go                       Transactional outbox drain              (12 §5)
│   │   ├── route.go                        Subscriptions → channels                (02 §4)
│   │   ├── render.go                       Templates per event type
│   │   ├── email.go                        SMTP
│   │   └── teams.go                        Adaptive Card → Power Automate          (16 §3)
│   │
│   ├── audit/
│   │   ├── audit.go                        Recorder — writes in caller's tx        (12 §4)
│   │   ├── events.go                       Event type catalog                      (12 §4.1)
│   │   └── query.go                        Filtered query backing the API
│   │
│   ├── worker/                             ── data plane loop
│   │   ├── worker.go                       Register, run, drain
│   │   ├── lease.go                        Lease client; server-directed backoff   (09 §7.1)
│   │   ├── runner.go                       errgroup + semaphore within grant       (05 §8)
│   │   ├── heartbeat.go                    Renewal, cancellation, telemetry        (09 §7.3)
│   │   ├── report.go                       Throttled progress; completion          (09 §7.2)
│   │   ├── stall.go                        Idle-stall detector                     (11 §2.1)
│   │   └── logship.go                      Ship log lines to the Coordinator       (12 §6)
│   │
│   ├── api/
│   │   ├── router.go                       The full route table                    (09 §2)
│   │   ├── problem.go                      RFC 9457 problem+json                   (09 §8)
│   │   ├── paginate.go                     Opaque cursor encode/decode             (09 §3)
│   │   ├── filter.go                       AIP-160 subset → parameterized SQL      (09 §3)
│   │   ├── middleware/
│   │   │   ├── requestid.go  logging.go  tracing.go  metrics.go  recovery.go
│   │   │   └── auth.go                     NO-OP TODAY — the designed seam         (09 §10.1)
│   │   └── v1/
│   │       ├── products.go   packages.go   transfers.go                            (09 §4)
│   │       ├── control.go                  :pause :resume :cancel :retry :setPriority
│   │       ├── schedules.go  verifications.go  audit.go
│   │       ├── workers.go                  Worker plane: lease/progress/complete   (09 §7)
│   │       ├── system.go                   healthCheck, version                    (09 §9)
│   │       └── dto/                        Mapping to pkg/apis types
│   │
│   ├── store/
│   │   ├── store.go                        Store interfaces
│   │   ├── tx.go                           One tx: state + audit + outbox          (09 §7.2)
│   │   ├── postgres/                       sqlc-generated + hand-written dequeue   (03 §2)
│   │   ├── sqlite/                         Same interface; BEGIN IMMEDIATE         (03 §2)
│   │   └── migrate/migrate.go              goose under advisory lock               (03 §9)
│   │
│   └── platform/                           ── no domain imports, ever
│       ├── config/                         SystemConfig; precedence                (02 §8)
│       ├── log/                            slog + correlation keys                 (12 §6)
│       ├── metrics/                        Prometheus registry + catalog           (12 §2)
│       ├── tracing/                        OTel setup and propagation              (12 §3)
│       ├── health/                         healthz · readyz · deep check           (09 §9.1)
│       ├── backoff/                        Full-jitter exponential                 (10 §6)
│       ├── statemachine/                   Generic Transition guard                (10 §1)
│       ├── leader/                         pg_advisory_lock election               (04 §9)
│       └── version/                        Build info                              (12 §2.7)
│
├── pkg/apis/softwaregateway/v1/            ── the ONLY public surface
│   ├── types.go                            Request/response types
│   ├── enums.go                            SCREAMING_SNAKE_CASE enums              (09 §3)
│   ├── errors.go                           Problem detail + code enum              (09 §8)
│   └── client.go                           Go client used by transferctl
│
├── db/
│   ├── migrations/{postgres,sqlite}/       goose, embedded via embed.FS            (03 §9)
│   └── queries/{postgres,sqlite}/          sqlc input — dual dialect               (03 §2)
│
├── deploy/                                 ── Flux + Kustomize                     (14 §1)
│   ├── base/{coordinator,worker,postgres,config,network}/
│   ├── overlays/{dev,staging,production}/
│   ├── products/                           One ConfigMap per product — data, not infra
│   ├── flux/                               GitRepository + Kustomization
│   └── observability/                      ServiceMonitor, alerts, dashboards      (12 §7)
│
├── test/
│   ├── integration/                        testcontainers: Postgres + registries
│   ├── chaos/                              The cockroach suite C1–C12              (11 §5)
│   └── fixtures/                           Multi-arch test package
│
└── docs/
    ├── FUNCTIONAL-OVERVIEW.md              This document
    └── design/                             00–17, the implementer's set
```

**Where a change lands** — the real test of a layout:

| Change | Touches |
|---|---|
| Add Harbor support | `internal/registry/harbor/` + `factory.go` |
| Add a notification channel | `internal/notification/` |
| Change retry policy | `internal/queue/retry.go` |
| Add a CLI command | `cmd/transferctl/` (+ `pkg/apis` if the API changes) |
| **Swap the OCI library** | `internal/registry/` only — nothing else moves |

That last row is [ADR-001](design/16-technology-choices.md#adr-001) restated as a layout property. If swapping the library touched more than one directory, the abstraction would have failed.

---

## 4. CLI, by what you are trying to do

Full flags and output in [13](design/13-cli.md). Grouped here by intent, because that is how a command actually gets looked up.

### "Is everything working?"

```bash
transferctl health          # every dependency: DB, workers, all registries, SMTP, Teams
transferctl workers list    # fleet status and granted concurrency
```

### "What is available?"

```bash
transferctl products list
transferctl packages list --product vendor-a-platform
transferctl packages describe v2.14.0 --product vendor-a-platform   # artifacts, targets, signatures
transferctl discover vendor-a-platform                              # scan now, don't wait 15m
transferctl discover                                                # scan every product
```

### "I need this release in lab"

```bash
transferctl download v2.14.0 --product vendor-a-platform --target lab --dry-run
transferctl download v2.14.0 --product vendor-a-platform --target lab --priority 100
transferctl transfers describe 9c1e8f2a --watch
```

### "Promote it to production"

```bash
transferctl promote v2.14.0 --product vendor-a-platform --from lab --to production
transferctl promote v2.14.0 --product vendor-a-platform --from lab --to production \
    --at 2026-08-16T02:00:00Z          # maintenance window
transferctl schedules list
```

### "Something is stuck"

```bash
transferctl transfers describe 9c1e8f2a           # progress, failed jobs, error classes
transferctl transfers jobs 9c1e8f2a --state failed
transferctl transfers logs 9c1e8f2a --follow
transferctl transfers retry 9c1e8f2a              # requeue failed jobs; resumes, not restarts
```

### "Slow down / speed up / stop"

```bash
transferctl transfers priority 9c1e8f2a 900
transferctl transfers pause 9c1e8f2a              # in-flight finishes; nothing new is leased
transferctl transfers resume 9c1e8f2a
transferctl transfers cancel 9c1e8f2a
```

### "Prove this is what the vendor signed"

```bash
transferctl verify v2.14.0 --product vendor-a-platform --at destination --target lab
transferctl verify v2.14.0 --product vendor-a-platform --at source
```

### "What happened, and when?"

```bash
transferctl audit list --product vendor-a-platform --since 2026-03-01 --until 2026-04-01
transferctl audit list --subject-id 9c1e8f2a          # everything about one transfer
```

### "Is my config valid?" — runs in CI, pre-merge

```bash
transferctl config validate ./deploy/products/
```

---

## 5. A day in the life

Five people, ten situations. Each scenario names **the design decision that produced the outcome** — otherwise this is a feature list with narration.

| Persona | Cares about | Touches |
|---|---|---|
| **Dan** — product owner | Onboarding vendors; what's available | Git PRs, `packages list` |
| **Priya** — release engineer | Getting a release into lab today | `download`, `--dry-run`, `--watch` |
| **Marcus** — platform SRE | Nothing being on fire | `transfers describe`, dashboards |
| **Aisha** — security engineer | Signatures and provenance | `verify`, `audit list` |
| **Sam** — change manager | Production changes in the window | `promote --at`, `schedules list` |

### 5.1 Onboarding a new vendor — Dan, Monday morning

Dan writes **one file**. Everything about the product — sources, targets, credentials, CA bundle, proxy, rate limits, notification recipients, auto-download rules, verification policy — is in that one ConfigMap.

```bash
$ vim deploy/products/vendor-c-analytics.yaml
$ transferctl config validate ./deploy/products/

  vendor-a-platform.yaml     OK   2 sources, 2 targets, 2 rules
  vendor-c-analytics.yaml    ERROR

    spec.autoDownload.rules[0].tagPattern: invalid regexp
      '^v(\d+\.\d+' — missing closing )
    spec.verification.cosign.keyless: certificateIdentity is required in
      keyless mode — without it, any valid Sigstore signature would be accepted

$ # fix both, re-run, green. Open PR.
```

He merges. Flux applies within 5 minutes; the Coordinator reloads within about 60 seconds. Within 15 minutes discovery has found 40 tags and Teams has a message.

> **Design decision at work:** config validation runs offline in CI, using the *same validator* the Coordinator runs at load ([02 §7](design/02-configuration.md)). This is the deliberate compensation for choosing ConfigMaps over CRDs — no admission webhook, so catch it in the pull request instead. The second error is the valuable one: a keyless policy without an identity constraint is syntactically fine and *semantically useless*, and would have looked secure in production.

### 5.2 Overnight — nobody is awake

```
02:14   discovery scans registry.vendor-a.example.com
02:14   new package: v2.14.0  sha256:9f86d081…  45.2 GiB  847 blobs
02:14   audit: PackageDiscovered · notification queued · rules evaluated
02:14   rule 'ga-releases' matches ^v\d+\.\d+\.\d+$ → request created, priority 100
02:14   planner: 847 blobs → 291 already present (12.1 GiB) → 556 jobs, 33.1 GiB
02:15   HPA scales workers 2 → 14 on queue backlog
02:15   transfer begins
02:26   all waves drained → verifying
02:26   cosign keyless verification passes, 5 of 5 artifacts
02:26   tag applied at destination. Teams message sent.
02:41   HPA scales workers back to 2
```

Priya reads the Teams message at standup. Nobody typed anything.

> **Design decisions at work:** auto-download rules with derived idempotency keys, so a discovery re-run or a Coordinator restart mid-expansion produces one request, not two ([07 §5](design/07-discovery.md)). Deduplication removed 27% before a byte moved. The tag was applied **last**, after the index manifest committed — so at no point could a consumer see a half-written package ([04 §3](design/04-queue-and-scheduling.md), invariant I1).

### 5.3 "I need v2.14.0 in lab before the 2pm demo" — Priya

```bash
$ transferctl download v2.14.0 --product vendor-a-platform --target lab --dry-run

Transfer plan — vendor-a-platform / v2.14.0 → lab
  Blobs                 847   total 45.2 GiB
    already present     291         12.1 GiB   placement hit
    to transfer         556         33.1 GiB
  Estimated duration   ~11m20s   at 48.6 MiB/s (EWMA, 14 recent transfers this route)
  Bandwidth saved       12.1 GiB (27%)
  No data transferred (dry run).
```

She has time. She runs it for real and watches.

> **Design decision at work:** the dry run is the planner's output rendered and discarded — `validateOnly=true` on the same endpoint, sharing the same code path ([05 §7](design/05-transfer-engine.md)). A dry run that used different code would eventually disagree with reality, and a dry run nobody trusts has no reason to exist. The ETA comes from measured throughput on that specific route, not a configured constant; with no history it says so rather than inventing a number.

### 5.4 Something is stuck — Marcus, 11:07

```bash
$ transferctl transfers describe 9c1e8f2a

  [██████████████████████████░░░░░░░░░░░░░]  51.4%      RUNNING
  Jobs   402 succeeded · 12 skipped · 14 in flight · 131 pending · 2 failed

  Failed jobs (2):
    sha256:3c9e1f7d…   134.2 MiB   attempt 3/8   ErrUnavailable  503 from target
    sha256:8a1b2c3d…    45.8 MiB   attempt 2/8   ErrTimeout      idle stall
```

Marcus checks the registry dashboard: `repository_concurrency_limit` for the lab target has dropped from 24 to 11. The controller noticed the 503s and backed off. Both jobs retry successfully within four minutes. **He does nothing.**

> **Design decision at work:** AIMD backpressure ([11 §3](design/11-resiliency-and-backpressure.md)) — the same control law as TCP congestion control, chosen over a cleverer gradient controller specifically because an on-call engineer can predict its behaviour on a whiteboard. Retries use full jitter, so 40 jobs failing simultaneously against one struggling registry do not retry in lockstep and turn a blip into an outage.

### 5.5 Production promotion in the maintenance window — Sam

```bash
$ transferctl promote v2.14.0 --product vendor-a-platform --from lab --to production \
      --at 2026-08-16T02:00:00Z

Scheduled request 3d8f1a92-… for 2026-08-16 02:00:00 UTC (in 4d 11h)
  No queue entries created until the scheduled time.
```

Sunday at 02:00 it expands and completes in **90 seconds** — not 11 minutes.

> **Design decisions at work:** two of them. First, the queue contains only *executable* work ([04 §10](design/04-queue-and-scheduling.md)) — the request sat in `scheduled_requests` for four days without a single queue row, so queue depth stayed honest and HPA never spun up workers for work that wasn't due. Second, lab and production share a registry, so **cross-repository mount** relocates blobs server-side ([05 §4.2](design/05-transfer-engine.md)): zero bytes over the wire, regardless of package size. Planning also happens at execution time, not scheduling time, so anything replicated during those four days is correctly deduplicated.

### 5.6 Verification failure — Aisha, paged at 03:12

```bash
$ transferctl transfers describe 5a7b9c1d
  State  FAILED — verification failed at destination

$ transferctl verify v2.14.1 --product vendor-a-platform --at destination --target lab
  sha256:8c2d4f1a…  linux/amd64   ✗  certificate identity mismatch
                                     expected .../release.yaml@refs/heads/main
                                     got      .../nightly.yaml@refs/heads/dev
VERIFICATION FAILED — 4 of 5 artifacts passed
```

The state is `failed`, not `error`. That distinction is the first thing Aisha checks, and it tells her this is a **security event**, not a Sigstore outage. The artifacts are still at the destination for inspection — and no tag was ever applied, so nothing is exposed to consumers.

> **Design decisions at work:** `failed` and `error` are deliberately separate states ([08 §8](design/08-verification.md)). Collapsing them would make a Rekor outage indistinguishable from a supply-chain attack; they page different people and imply different responses, so `error` retries with backoff and `failed` does not retry at all. On failure we **do not delete** — the blobs may be legitimately shared with other packages, and an operator investigating needs the evidence, not a clean scene.

### 5.7 Vendor registry outage — three hours, zero human action

```
14:02  vendor-a registry begins returning 503
14:02  jobs retry with full-jitter backoff; adaptive limit drops 16 → 4 → 1
14:05  discovery backs off to a 4× interval — but is never disabled
14:20  transfer 7f3a2b1c fails after 8 attempts (~20 min of backoff)
       audit: TransferFailed · Teams notification sent
17:31  vendor registry recovers
17:31  discovery's next scan succeeds — a full scan, so it simply catches up
17:35  Marcus: transferctl transfers retry 7f3a2b1c
       → resumes from 71%. Completed jobs stayed completed.
```

> **Design decisions at work:** failing after ~20 minutes is deliberate ([11 §2.3](design/11-resiliency-and-backpressure.md)). An indefinitely-retrying transfer holds queue slots and hides the problem behind a green dashboard while still needing a human eventually. Failing makes it visible; retry resumes from exactly where it stopped. And discovery is a **stateless full scan** with no cursor ([07 §3](design/07-discovery.md)), so a three-hour outage needs no repair path — the next scan is just a scan.

### 5.8 Big release day — three vendors ship at once

```
09:00  3 packages discovered · 3 transfers planned · 1,612 jobs queued
09:01  queue_backlog_per_worker = 806   → HPA scales 2 → 24
09:04  aggregate throughput 780 MiB/s across 24 workers
09:31  all three complete
09:36  backlog drains → HPA holds (300s stabilization window)
09:41  scales back to 2
```

Nobody scaled anything. Nobody rebalanced anything.

> **Design decisions at work:** the unit of work is a **blob, not a package** ([04 §2](design/04-queue-and-scheduling.md)), so three packages become 1,612 independent jobs that spread across the whole fleet — a package-level job would pin to one process. Workers are stateless, so scaling needs no rebalancing and no partition assignment. HPA scales on backlog *per worker* rather than raw depth, because a ratio converges and an absolute count oscillates ([09 §9.2](design/09-api.md)). Crucially, adding workers does **not** multiply load on the vendor: rate limits are fleet-wide, divided across active workers by the Coordinator ([05 §8](design/05-transfer-engine.md)).

### 5.9 "What did we ship in March?" — Aisha, during an audit

```bash
$ transferctl audit list --product vendor-a-platform \
      --since 2026-03-01 --until 2026-04-01 --event-type TransferCompleted

OCCURRED              EVENT               PACKAGE   TARGET      ACTOR       DIGEST
2026-03-04 02:31:07   TransferCompleted   v2.11.0   lab         auto_rule   sha256:1a2b…
2026-03-04 09:14:22   PromotionCompleted  v2.11.0   production  sam@…       sha256:1a2b…
2026-03-18 02:29:55   TransferCompleted   v2.12.0   lab         auto_rule   sha256:3c4d…
```

> **Design decisions at work:** the audit trail is **separate from application logs** and written *in the same transaction* as the change it records ([12 §4](design/12-observability-and-audit.md)). It is therefore impossible to have performed an audited action without its record, or to hold a record of something that rolled back. It is retained for a year in a monthly-partitioned table, so expiring old data is a `DROP TABLE` rather than a mass delete. Note that a package is identified by tag **and digest** — a vendor re-pushing a tag creates a new record and marks the old one superseded, so this query answers what bytes actually shipped, not what a mutable tag says today.

### 5.10 Emergency stop — Marcus, during a network incident

```bash
$ kubectl scale deploy/worker --replicas=0
```

That is the whole procedure. Leases expire within about 150 seconds and every job returns to the queue. Nothing is lost, nothing is corrupted, no partial state needs cleaning up. When the incident clears:

```bash
$ kubectl scale deploy/worker --replicas=3
```

Everything resumes from where it stopped.

> **Design decision at work:** this is the clearest practical expression of the stateless-worker model. Worker liveness is a **lease timestamp** — a `SIGKILL`ed pod on a dead node performs no cleanup, sends no message, and runs no shutdown hook ([04 §4.3](design/04-queue-and-scheduling.md)). "Crashed", "network-partitioned", and "scaled to zero" are literally the same code path. This is only safe because blob transfers are content-addressed and therefore idempotent; the same lease mechanism on a non-idempotent workload would need fencing tokens and a much harder design.

---

## 6. What runs while nobody is watching

Six loops, all on the leader Coordinator except the worker heartbeat.

| Loop | Interval | Does | If it stopped, you'd see |
|---|---|---|---|
| **Discovery** | 15 m per source | Scan tags, record new packages, fire rules | `discovery_last_success_timestamp_seconds` goes stale |
| **Scheduler** | 10 s | Expand due scheduled requests into jobs | Schedules fire late |
| **Lease reaper** | 30 s | Requeue jobs whose lease expired | `queue_leased_jobs` stuck high, throughput drops |
| **Notification outbox** | 15 s | Deliver queued notifications, retry failures | `notification_outbox_pending` climbs |
| **Backpressure controller** | 30 s | Adjust per-repository concurrency | Limits frozen at last value |
| **Retention GC** | 1 h | Batched deletes; create/drop audit partitions | Database grows |

> **The failure worth designing for is the quiet one.** A discovery loop that stops throws no errors — the error rate is *zero*, the dashboard is green, and the system looks perfectly healthy while silently finding nothing. This is why `discovery_last_success_timestamp_seconds` exists and why the alert is on **staleness, not error rate** ([12 §2.1](design/12-observability-and-audit.md)).

Nothing in this table can block a transfer. Notifications, tracing, and metrics are side channels — SMTP being down retries a notification and never touches a replication ([11 §4](design/11-resiliency-and-backpressure.md)). Putting the notification send inside the completion transaction rather than in an outbox would break that, which is an easy accident and worth naming.

---

## 7. A Tuesday

```
   ▼ human touchpoint          ░ automated

00:00 ░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░  discovery ×4, scheduler ×360, reaper ×120
02:14 ░░ discovery finds v2.14.0 → auto-download → 45 GB → verified → Teams
02:41 ░░ HPA back to 2 workers
03:00 ░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░  GC: 41k completed jobs deleted
      ░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░
09:15 ▼  Priya reads the Teams message. Does nothing — it's already in lab.
09:30 ▼  Dan opens a PR adding vendor-c-analytics
10:02 ░░ Flux applies · config reload · discovery begins on the new source
11:07 ▼  Marcus glances at a transfer with 2 failed jobs. Backpressure handles it.
11:11 ░░ both jobs retry and succeed
14:00 ▼  Sam schedules Sunday's promotion. Queue stays empty.
      ░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░
18:00 ░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░

   4 human interactions        ~2,900 automated actions
   45 GB replicated            12 GB never moved (deduplicated)
   0 pages                     0 manual interventions
```

**That ratio is the product.** The tool is designed to surface to a human only on exceptions — a verification failure, a terminal transfer failure, a stale discovery loop — and to handle everything else itself. Every design decision in the 18 documents is ultimately in service of keeping the left column small.

---

## Where to go next

| Question | Document |
|---|---|
| How is this built? | [design/00 — Overview](design/00-overview.md) |
| Every CLI flag and output format | [design/13 — CLI](design/13-cli.md) |
| How do I configure a product? | [design/02 — Configuration](design/02-configuration.md) |
| What happens when X fails? | [design/11 — Resiliency](design/11-resiliency-and-backpressure.md) |
| What are the known limitations? | [design/README](design/README.md#known-limitations) |

> **Maintenance note.** This document is the source of record for the functional view and is mirrored as a published visual artifact for sharing. The two must be updated together; this file is canonical.
