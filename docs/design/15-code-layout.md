# 15 — Code Layout

> **Consumed by:** [17 — Delivery Plan](17-delivery-plan.md)

Organized around **business domains, not technical layers**. There is no `models/`, `services/`, or `handlers/` directory, because those group code by what it *is* rather than by what it is *about* — and a change to how promotion works should touch one directory, not five.

---

## 1. Tree

```
softwareGateway/
├── cmd/
│   ├── coordinator/main.go          Control plane
│   ├── worker/main.go               Data plane
│   └── transferctl/main.go          CLI
│
├── internal/                        Not importable outside this module — deliberate
│   ├── product/                     Config model, loader, validation, watch
│   ├── discovery/                   Scanner, dedupe, auto-download rules
│   ├── transfer/                    Planner, engine, dry run, progress
│   ├── registry/                    Repository interface + implementations
│   │   ├── registry.go              THE interface (06 section 2)
│   │   ├── transport/               Auth, token cache, rate limit, retry, CA, proxy
│   │   ├── generic/                 OCI Distribution v2 -- the default
│   │   ├── acr/  artifactory/  quay/    Deltas only (06 section 6)
│   │   └── factory.go               Type -> constructor registration
│   ├── queue/                       Jobs, leases, waves, priorities, retry
│   ├── scheduler/                   Due-time expansion, leader loops
│   ├── verification/                Verifier interface + cosign
│   ├── notification/                Outbox, email, teams, templates
│   ├── audit/                       Event recording and query
│   ├── worker/                      Worker loop, lease client, concurrency
│   ├── api/                         HTTP: router, handlers, DTOs, middleware
│   │   ├── router.go  middleware/  v1/{handlers,dto}/
│   ├── store/                       Persistence
│   │   ├── store.go                 Store interfaces
│   │   ├── postgres/  sqlite/       sqlc-generated + hand-written
│   │   └── migrate/                 goose runner
│   └── platform/                    Cross-cutting infrastructure
│       ├── config/  log/  metrics/  tracing/  health/
│       ├── backoff/  statemachine/  leader/  version/
│
├── pkg/                             PUBLIC -- API types shared with consumers
│   └── apis/softwaregateway/v1/     Request/response types, enums, client
│
├── db/
│   ├── migrations/{postgres,sqlite}/
│   └── queries/{postgres,sqlite}/   sqlc input
│
├── deploy/                          14
├── docs/design/                     This document set
└── test/
    ├── integration/  chaos/  fixtures/
```

## 2. Package responsibilities

| Package | Owns | Does **not** own |
|---|---|---|
| `product` | Config schema, loading, validation, hot reload | Anything about transfers |
| `discovery` | Scanning, package identity, auto-download evaluation | Executing transfers |
| `transfer` | Planning, waves, the streaming engine, dry run, progress | HTTP, SQL |
| `registry` | The `Repository` interface, all registry I/O, auth, rate limiting | Business rules |
| `queue` | Job lifecycle, leasing, priority, retry policy | What a job *means* |
| `scheduler` | Due-time expansion, leader-elected loops | Job execution |
| `verification` | Signature verification, trust policy | Transfers |
| `notification` | Outbox draining, channel delivery, templates | When to notify (callers decide) |
| `audit` | Event recording and query | Interpreting events |
| `worker` | Lease loop, local concurrency, progress reporting | SQL, planning |
| `api` | Routing, DTOs, middleware, HTTP semantics | Business logic |
| `store` | SQL, both dialects, transactions | Business logic |
| `platform` | Config, logging, metrics, tracing, backoff, state machine, leader | Any domain concept |

**The two lines that matter most:** `api` owns no business logic (a handler parses, calls a domain package, and serializes), and `store` owns no business logic (a query returns rows; it does not decide what they mean). Violating either is how a codebase ends up with the same rule implemented three times, slightly differently.

## 3. Dependency rules

Enforced in CI by `depguard` rules inside `golangci-lint` (see `.golangci.yml`) — a rule nobody checks is a rule that decays.

```
        cmd/*
          │  wiring only: construct, inject, run
          ▼
    ┌─────────────────────────────────────────┐
    │  api          worker                    │   entry points
    └──────┬──────────────┬───────────────────┘
           ▼              ▼
    ┌─────────────────────────────────────────┐
    │ product discovery transfer queue         │   domain
    │ scheduler verification notification audit│
    └──────┬──────────────┬───────────────────┘
           ▼              ▼
    ┌──────────────┐  ┌───────────────────────┐
    │   registry   │  │        store          │   infrastructure
    └──────┬───────┘  └───────────┬───────────┘
           └──────────┬───────────┘
                      ▼
              ┌───────────────┐
              │   platform    │                   no domain imports
              └───────────────┘
```

| Rule | Rationale |
|---|---|
| Domain packages never import `api` | Business logic must not know it is served over HTTP. Otherwise the CLI, a future gRPC surface, or a test harness cannot reuse it |
| `platform` never imports a domain package | Keeps infrastructure genuinely generic and prevents import cycles |
| `store` never imports a domain package | Persistence takes and returns data; interpretation belongs upstream |
| Domain packages talk through interfaces | `transfer` depends on `registry.Repository`, not on a concrete registry — the mechanism that makes [ADR-001](16-technology-choices.md#adr-001) reversible |
| `cmd/*` contains wiring only | Main constructs dependencies and starts things. Logic in `main` is untestable |
| Nothing outside `internal/` imports `internal/` | Enforced by the compiler |

**`pkg/apis/softwaregateway/v1` is the only public surface.** Request/response types, enums, and a generated Go client. It is what `transferctl` uses and what a third-party integration would import — a compile-time commitment to the API contract in [09](09-api.md), not a convention.

## 4. Where a change lands

The real test of a layout. Each row should touch one directory, plus its tests.

| Change | Where |
|---|---|
| Add Harbor support | `internal/registry/harbor/` + `factory.go` ([06](06-registry-abstraction.md) §6.5) |
| Add a notification channel | `internal/notification/` |
| Add a config field | `internal/product/` (+ [02](02-configuration.md)) |
| Change retry policy | `internal/queue/retry.go` |
| Add a CLI command | `cmd/transferctl/` + `pkg/apis/…/v1` if the API changes |
| Add a metric | The package that owns the behaviour |
| Change the dequeue query | `db/queries/{postgres,sqlite}/` + `internal/store/` |
| Add an API endpoint | `internal/api/v1/` + `pkg/apis/…/v1` |
| Swap the OCI library | `internal/registry/` only — nothing else moves |

The last row is the design goal for ADR-001 restated as a layout property: **if swapping the library touched more than one directory, the abstraction would have failed.**

## 5. Testing

| Level | Location | Dependencies | In PR CI |
|---|---|---|---|
| Unit | `*_test.go` beside the code | None | Yes |
| Engine | `internal/transfer/` | In-memory OCI registry | Yes |
| Store | `internal/store/` | SQLite in-memory | Yes |
| Integration | `test/integration/` | testcontainers: Postgres + registries | Yes |
| Conformance | `test/integration/registry/` | Real registries | Nightly |
| Chaos | `test/chaos/` | kind cluster | Nightly + pre-release |

**PR CI must not need Docker for unit tests.** In-memory registry plus in-memory SQLite keeps the inner loop in seconds, and a fast inner loop is run far more often than a thorough one.

Conformance tests run nightly against real ACR, Artifactory, and Quay instances, because capability assumptions ([06](06-registry-abstraction.md) §3) must be validated against reality rather than against documentation. They are excluded from PR CI: they need credentials, they are slow, and a vendor's outage should not block an unrelated merge.

## 6. Conventions

- **Errors** wrapped with `fmt.Errorf("...: %w", err)`; sentinels compared with `errors.Is`; classification once at the registry boundary ([06](06-registry-abstraction.md) §7), never by re-inspecting HTTP status codes deep in the call stack.
- **Context** first parameter everywhere; every blocking call respects cancellation. A worker aborting a cancelled job depends on this being universal.
- **No global state** except metric registries. Dependencies are injected, which is what makes the domain packages testable without a running Coordinator.
- **Interfaces defined by the consumer**, not the producer — `transfer` declares the narrow slice of `registry.Repository` it needs. Go idiom, and it keeps mocks small.
- **`log/slog`** with the correlation keys from [12](12-observability-and-audit.md) §6.
- **`golangci-lint`** with `errcheck`, `govet`, `staticcheck`, `revive`, `gosec`, `bodyclose`. `bodyclose` earns its place specifically here: a leaked HTTP response body in the transfer engine leaks a connection, and at this concurrency that exhausts the pool rather than merely wasting memory.
