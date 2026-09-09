# 15 - Code Layout

> **Consumed by:** [17 - Delivery Plan](17-delivery-plan.md)

Organized around **business domains, not technical layers**. There is no `models/`, `services/`, or `handlers/` directory, because those group code by what it *is* rather than by what it is *about* - and a change to how promotion works should touch one directory, not five.

---

## 1. Tree

Ten directories at the root, and each answers a question none of the others
answers. That is the whole rule: a newcomer should be able to pick the right
one from its name without opening it.

```
softwareGateway/
├── cmd/                             The three binaries, and nothing else
│   ├── coordinator/                 Control plane
│   ├── worker/                      Data plane
│   └── transferctl/                 CLI
│
├── internal/                        Not importable outside this module - deliberate
│   ├── product/                     Config model, loader, validation, watch
│   ├── discovery/                   Scanner, dedupe, auto-download rules
│   ├── expand/                      Walk a package's tree once, for whoever asks first
│   ├── transfer/                    Planner, engine, dry run, progress
│   ├── replication/                 Delegated replication and its runner
│   ├── promoter/                    The Promoter INTERFACE and its plugin registry
│   │   └── jfrog/                   One vendor's implementation, deletable
│   ├── promotion/                   Binds those plugins to product configuration
│   ├── download/                    Request-side entry point for downloads
│   ├── maintenance/                 Leader-gated housekeeping loops
│   ├── registry/                    Repository interface + implementations
│   │   ├── transport/               Auth, token cache, rate limit, retry, CA, proxy
│   │   └── generic/  acr/  artifactory/  quay/
│   ├── regclient/  oci/             Registry client plumbing and OCI primitives
│   ├── queue/                       Jobs, leases, waves, priorities, retry
│   ├── pipeline/                    Stage sequencing over the queue
│   ├── security/                    Scanner integration, findings, posture
│   ├── compliance/                  Check catalog, policy evaluation, reports
│   ├── catalog/  compare/  export/  calibrate/  preflight/  vendors/
│   ├── worker/                      Worker loop, lease client, concurrency
│   ├── api/                         HTTP: router, handlers, DTOs, middleware
│   ├── store/                       Persistence: interfaces + postgres/ + sqlite/
│   └── platform/                    Cross-cutting infrastructure
│       ├── config/  log/  metrics/  tracing/  health/  tlscompat/
│       └── backoff/  statemachine/  leader/  version/
│
├── pkg/                             PUBLIC - importable by consumers
│   ├── apis/softwaregateway/v1/     Request/response types, enums, client
│   └── authz/                       Identity, the Cerbos engine, middleware
│
├── web/                             The SPA (React + TypeScript + Vite)
│   └── src/{pages,components,api,auth,domain,uikit,tablekit}/
│
├── db/                              Schema, and only schema
│   └── migrations/{postgres,sqlite}/
│
├── config/                          What an ADMINISTRATOR manages - see 27
│   ├── config.yaml                  One file, read by task run, compose and Flux
│   ├── products/  users/  access/   Products, people, roles and policies
│   └── secrets/                     Manifests for a cluster; local/ is gitignored
│
├── deploy/                          How it is BUILT and SHIPPED
│   ├── build/                       The four Dockerfiles
│   ├── zitadel/  web/  cerbos/  postgres/    Entrypoints, configs, the seeder
│   ├── certs/  go/  npm/            Trust and registry material for the build
│   ├── dev/                         Compose for Postgres and local registries
│   └── scripts/                     Operator helpers
│
├── docs/                            Why it is the way it is
│   ├── design/                      This document set, 00 to 29
│   ├── compliance/  security/  ui/
│   └── DEVELOPER-GUIDE.md  FUNCTIONAL-OVERVIEW.md
│
├── test/                            What PROVES it works
│   ├── cmd/fakeregistry/            A standalone vendor registry, as a command
│   ├── fakeregistry/                The same double, as a package tests import
│   ├── mockEntra/                   A Microsoft directory whose answers can be read
│   ├── seed/                        Brings a full local estate up with data in it
│   └── products.example/  secrets/  Fixtures
│
└── dev/                             One laptop's runtime state. Disposable, ignored
```

Four boundaries carry the weight, and they are the ones people get wrong:

- **`config/` is content, `deploy/` is machinery.** Editing a product is a
  configuration change an administrator makes and CI applies; editing an nginx
  config is an engineering change that ships in an image. They travel
  differently, so they do not share a directory.
- **`db/` is schema, `config/` is configuration.** Adjacent names, opposite
  things. `db/` holds migrations and nothing else.
- **`test/` is tooling and fixtures, not unit tests.** Go tests live beside the
  code they test, as Go intends. What lives here is what a test needs and a
  package cannot hold: doubles, a seeded estate, example inputs.
- **`dev/` is the only disposable directory,** and the only one that is
  ignored. Deleting it is a reset.

There is no `src/`. `cmd/` and `internal/` at the root are Go's own
conventions, `internal/` in particular being a rule the toolchain enforces
rather than a name somebody chose, and burying either under a directory that
means "the code" would cost every import path a segment that carries no
information.


## 2. Package responsibilities

| Package | Owns | Does **not** own |
|---|---|---|
| `product` | Config schema, loading, validation, hot reload | Anything about transfers |
| `discovery` | Scanning, package identity, auto-download evaluation | Executing transfers |
| `expand` | Turning a discovered package into a fully known one, once | Deciding *why* someone wanted it |
| `maintenance` | *When* a piece of housekeeping runs, and what to say about it | The housekeeping itself - that lives with the data |
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

Enforced in CI by `depguard` rules inside `golangci-lint` (see `.golangci.yml`) - a rule nobody checks is a rule that decays.

```
        cmd/*
          │  wiring only: construct, inject, run
          ▼
    ┌─────────────────────────────────────────┐
    │  api          worker                    │   entry points
    └──────┬──────────────┬───────────────────┘
           ▼              ▼
    ┌─────────────────────────────────────────┐
    │ product discovery expand transfer queue  │   domain
    │ scheduler verification notification audit│
    │ maintenance                              │
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
| Domain packages talk through interfaces | `transfer` depends on `registry.Repository`, not on a concrete registry - the mechanism that makes [ADR-001](16-technology-choices.md#adr-001) reversible |
| `cmd/*` contains wiring only | Main constructs dependencies and starts things. Logic in `main` is untestable |
| Nothing outside `internal/` imports `internal/` | Enforced by the compiler |

**`pkg/apis/softwaregateway/v1` is the only public surface.** Request/response types, enums, and a generated Go client. It is what `transferctl` uses and what a third-party integration would import - a compile-time commitment to the API contract in [09](09-api.md), not a convention.

## 4. Where a change lands

The real test of a layout. Each row should touch one directory, plus its tests.

| Change | Where |
|---|---|
| Add Harbor support | `internal/registry/harbor/` + `factory.go` ([06](06-registry-abstraction.md) §6.5) |
| Teach a registry to promote for itself | `internal/promoter/<name>/` + one line in `cmd/coordinator` ([22](22-promotion.md) §3). Nothing in `internal/transfer` moves, and depguard enforces it |
| Add a notification channel | `internal/notification/` |
| Add a config field | `internal/product/` (+ [02](02-configuration.md)) |
| Change retry policy | `internal/queue/retry.go` |
| Add a CLI command | `cmd/transferctl/` + `pkg/apis/…/v1` if the API changes |
| Add a metric | The package that owns the behaviour |
| Change the dequeue query | `db/queries/{postgres,sqlite}/` + `internal/store/` |
| Add an API endpoint | `internal/api/v1/` + `pkg/apis/…/v1` |
| Swap the OCI library | `internal/registry/` only - nothing else moves |

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
- **Interfaces defined by the consumer**, not the producer - `transfer` declares the narrow slice of `registry.Repository` it needs. Go idiom, and it keeps mocks small.
- **`log/slog`** with the correlation keys from [12](12-observability-and-audit.md) §6.
- **`golangci-lint`** with `errcheck`, `govet`, `staticcheck`, `revive`, `gosec`, `bodyclose`. `bodyclose` earns its place specifically here: a leaked HTTP response body in the transfer engine leaks a connection, and at this concurrency that exhausts the pool rather than merely wasting memory.
