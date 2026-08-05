# 16 — Technology Choices

> **Principle: do not reinvent existing solutions.** Build only what is business logic unique to this product. Everything else is a mature, well-maintained library.

Each decision below states the alternatives considered and **what would change our mind**, so a future reader can tell whether a decision is still valid rather than merely inherited.

---

## ADR-001

### OCI client library — `go-containerregistry` vs `oras-go/v2`

**Status: OPEN. Closed by measurement at M3** ([17](17-delivery-plan.md)).

The most consequential library choice in the system. It gets a full section rather than a table row because the candidates are genuinely close, the obvious argument against ORAS is wrong, and the honest tiebreaker is empirical.

**The design does not depend on the outcome.** [05](05-transfer-engine.md) and [08](08-verification.md) are written against the `Repository` interface in [06](06-registry-abstraction.md) §2, and [15](15-code-layout.md) §4 requires that swapping the library touch only `internal/registry/`. This ADR is a leaf decision by construction.

#### The argument that is wrong, recorded so it is not re-raised

> *"oras-go only does whole-artifact copies, so it cannot express a per-blob job model."*

**This is false.** Below `oras.Copy`/`ExtendedCopy`, oras-go v2 exposes every primitive the layer-as-job model needs:

```go
repo.Blobs().Exists(ctx, desc)          // dedupe check
repo.Blobs().Fetch(ctx, desc)           // streaming read
repo.Blobs().Push(ctx, desc, reader)    // streaming write
repo.Mount(ctx, desc, fromRepo, ...)    // cross-repository mount
repo.Referrers(ctx, desc, artifactType) // OCI 1.1
```

Both libraries can express our unit of work. The decision must rest on something else.

#### The case for `oras-go/v2`

1. **Artifact-native.** Its model is `ocispec.Descriptor` plus a manifest graph, agnostic to content type. `go-containerregistry`'s ergonomic surface is `v1.Image`/`v1.Layer` — container-shaped. **Our packages explicitly contain Helm charts and configuration bundles**, which is precisely the case ORAS exists to serve. GGCR handles arbitrary manifests through raw `remote.Get`/`remote.Put`, but that is working against the grain of its high-level API.
2. **`registry.Repository` is essentially the interface we specified.** [06](06-registry-abstraction.md) §2 is modelled on it. GGCR's `remote` package is free functions plus options rather than an interface, so substituting or faking it requires an adapter we would write ourselves.
3. **OCI 1.1 conformance lands there first**, including referrers with the fallback tag schema — directly relevant to [08](08-verification.md) §3.

#### The case for `go-containerregistry`

1. **One OCI type system shared with cosign.** Cosign's registry-facing packages (`cosign/pkg/oci/remote` — signature discovery via referrers or the `sha256-<digest>.sig` tag convention) are built on `name.Reference`, `v1.Image`, `v1.Hash`. Choosing ORAS for transfer means carrying **both dependency trees** and converting `ocispec.Descriptor` ↔ `v1.Descriptor`/`v1.Hash` at every point where verification meets transfer — which in this design is every source pre-check and every destination post-check ([08](08-verification.md) §4). Two OCI type systems in one binary is a durable source of digest-handling bugs. **This is a correctness argument, not a convenience one.**
2. `remote.WriteLayer` is a single call providing mount attempt, retry, and digest verification — roughly 30 lines of explicit orchestration in oras-go. *Convenience.*
3. `pkg/registry` ships an in-memory OCI registry as an `http.Handler`, so engine tests need neither Docker nor testcontainers ([15](15-code-layout.md) §5). *Convenience.*

#### The limit of the deciding argument

`sigstore-go` itself is bundle-centric and largely GGCR-neutral; it is **cosign's registry packages** that pull GGCR. [08](08-verification.md) §3.3 asks whether signature *discovery* can be hand-rolled against `Repository.Referrers` — which is only the two mechanisms in [08](08-verification.md) §3.1–3.2, both of which our interface must expose anyway.

If it can, the remaining cosign surface is bundle and certificate verification, which barely touches registry types, and **argument 1 above mostly evaporates.** That is exactly why this ADR stays open rather than being decided from the armchair.

#### M3 spike — the procedure that closes it

Both backends prototyped behind [06](06-registry-abstraction.md) §2, run against a real 30–60 GB package containing container images, a Helm chart, and a configuration bundle, against at least two of the four target registries.

**Criteria fixed in advance, so the result cannot be rationalized afterwards:**

| Criterion | Weight | Measured how |
|---|---|---|
| Sustained throughput; CPU and allocations per GB | High | Same package, same concurrency, both backends |
| Mount + `Exists` fast-path hit rate | High | Fraction of blobs completing with zero bytes moved |
| Non-image artifact handling (Helm, config bundle) | High | Lines of workaround code; raw-manifest escape hatches needed |
| Cosign integration cost | High | Conversion code at the verify boundary; binary size with both trees |
| Resumable/chunked upload control | Medium | Behaviour under injected mid-blob disconnect ([05](05-transfer-engine.md) §4.6) |
| Registry-specific auth (ACR, Artifactory, Quay) | Medium | Whether vendor deltas fit the shared transport ([06](06-registry-abstraction.md) §5) |
| Test ergonomics | Low | Docker required for unit tests, yes/no |

**Ties break toward `go-containerregistry`** on the cosign type-system argument.

**The losing backend is deleted, not maintained.** A permanently dual-backend registry layer would be exactly the over-engineering this design exists to avoid, and would double the conformance-testing surface forever.

*API-surface caveat:* exact signatures for both libraries are to be confirmed against pinned versions at implementation time. The reasoning above is version-stable; the illustrative call signatures are not normative.

---

## 2. Decision table

| Concern | Choice | Alternatives | Rationale | What would change our mind |
|---|---|---|---|---|
| **Database** | PostgreSQL | MySQL, CockroachDB, SQLite-only | `SKIP LOCKED`, partitioning, advisory locks, `JSONB` — every one of which this design uses ([03](03-persistence.md) §1) | Multi-region active-active |
| **Queue** | Postgres table | Kafka, RabbitMQ, Redis, NATS | Volume is trivial; atomicity with state is not ([04](04-queue-and-scheduling.md) §1) | >10 k jobs/s, or external consumers |
| **DB driver** | `pgx/v5` | `lib/pq`, `database/sql` + driver | Native protocol, better performance, first-class `COPY` and `LISTEN` if ever needed | — |
| **SQL generation** | `sqlc` | GORM, ent, squirrel, raw | Compile-time-checked SQL from real `.sql` files. **No ORM**: our hardest queries are hand-tuned dequeues where an ORM is an obstacle, not a help | — |
| **Dev database** | `modernc.org/sqlite` | `mattn/go-sqlite3` | **Pure Go, so `CGO_ENABLED=0` still builds a static distroless binary** ([14](14-deployment-and-development.md) §6). The cgo driver would force a fatter production image for a development convenience | — |
| **Migrations** | `goose` | golang-migrate, atlas, dbmate | Embeddable via `embed.FS`, per-dialect directories, `NO TRANSACTION` for `CREATE INDEX CONCURRENTLY` ([03](03-persistence.md) §9) | — |
| **HTTP router** | `chi` | stdlib `net/http`, gin, echo | `net/http`-compatible, mature middleware, no framework lock-in. **Go 1.22+ stdlib routing is now a credible alternative**; chi wins on middleware ecosystem alone, and this is a low-regret decision either way | Middleware needs shrink |
| **CLI** | `cobra` + `koanf` | urfave/cli, kingpin, viper | kubectl-shaped grammar users already know ([13](13-cli.md)). **`koanf` over `viper`**: lighter, no global state, explicit precedence | — |
| **Config parsing** | `sigs.k8s.io/yaml`, hand-rolled validation | Raw yaml.v3; `go-playground/validator` | Kubernetes-consistent YAML→JSON semantics, so a document behaves the same whether it is a ConfigMap or a file. **Validation is hand-rolled — see the M1 note below** | — |
| **Signing** | `sigstore-go` + cosign verify | notation-go, both | Ecosystem default ([08](08-verification.md) §2). Notary Project is behind the `Verifier` seam | A vendor requiring Notary Project |
| **Retry** | `cenkalti/backoff/v4` | hashicorp/go-retryablehttp, hand-rolled | Full jitter, context-aware, composable. `go-retryablehttp` is transport-level and would sit awkwardly under our per-error-class caps ([10](10-state-machines.md) §6) | — |
| **Metrics** | `prometheus/client_golang` | OTel metrics | Native Prometheus; prometheus-adapter needs it for HPA ([09](09-api.md) §9.2) | Org-wide OTel-only mandate |
| **Tracing** | OpenTelemetry + `otelhttp` | Jaeger client, Zipkin | The standard; vendor-neutral | — |
| **Logging** | `log/slog` | zap, zerolog | Stdlib, structured, zero dependencies. Fast enough — logging is not on the hot path; blob copying is | Profiling shows logging cost |
| **Email** | `wneessen/go-mail` | `net/smtp`, gomail | `net/smtp` is frozen and lacks modern auth and TLS handling. Actively maintained | — |
| **Teams** | Adaptive Cards → Power Automate workflow URL | O365 connector webhook | **O365 connectors are retired.** See §3 | — |
| **Leader election** | `pg_advisory_lock` | k8s Lease + client-go | No client-go, no RBAC, works in local dev ([04](04-queue-and-scheduling.md) §9) | Coordinator without a database |
| **Testing** | `testcontainers-go`, `testify` | dockertest, hand-rolled | Standard, hermetic ([15](15-code-layout.md) §5) | — |
| **Arch linting** | `depguard` (inside golangci-lint) | `go-arch-lint`; convention only | Dependency rules nobody checks decay ([15](15-code-layout.md) §3). **Changed from `go-arch-lint` during M1 — see below** | — |

## 3. Traps worth naming

Failure modes that are easy to hit and expensive to diagnose.

**Microsoft Teams — O365 connectors are retired.** Most tutorials and much existing code still use `https://outlook.office.com/webhook/...`. That path no longer works in current tenants. The supported mechanism is an **Adaptive Card posted to a Power Automate workflow URL**, which has a different URL shape and a different payload envelope. Config calls the field `webhookUrlRef` and the documentation says which kind ([02](02-configuration.md) §4).

**`CGO_ENABLED=0` and SQLite.** The obvious SQLite driver (`mattn/go-sqlite3`) requires cgo, which breaks the static distroless build. `modernc.org/sqlite` is pure Go. Getting this wrong means discovering at image-build time that a development convenience has compromised the production artifact.

**HTTP/2 for large blob transfers.** Go enables h2 automatically over TLS, and it is the wrong default here — flow-control windows and head-of-line blocking on a shared connection throttle multi-GB bodies ([05](05-transfer-engine.md) §5). This must be explicitly disabled; it will not announce itself, it will just be slow.

**`int64` in JSON.** Byte counts exceed 2^53. Serialize as strings (AIP-141), or lose precision silently on values large enough to be rare in testing and routine in production ([09](09-api.md) §3).

**`subPath` volume mounts.** They do not receive ConfigMap or Secret updates, which would silently break config reload and VSO credential rotation ([14](14-deployment-and-development.md) §3.1).

**Liveness probes that check dependencies.** Turns a database blip into a fleet-wide crash-loop ([09](09-api.md) §9.1).

## 4. What we are deliberately not adding

| Not using | Why |
|---|---|
| Redis | Nothing needs a cache. Placements are in Postgres and are small |
| Kafka / RabbitMQ / NATS | The queue is a table ([04](04-queue-and-scheduling.md) §1) |
| controller-runtime / CRDs | Config is ConfigMaps; Flux is already the reconciler ([02](02-configuration.md) §2) |
| An ORM | The critical queries are hand-tuned; an ORM obstructs them |
| gRPC | One HTTP API serves CLI, workers, and Prometheus. A second protocol would need a second auth story, a second error model, and a second client |
| A service mesh | Two services and a database. mTLS between them is not worth a mesh |
| An object store | Nothing is stored. Bytes stream registry to registry ([05](05-transfer-engine.md) §4.3) |
| A workflow engine (Temporal, Argo) | The state machines are ten states across five machines ([10](10-state-machines.md)); an engine would be larger than the thing it orchestrates |
| A separate scheduler service | It is a 10-second tick on the leader ([04](04-queue-and-scheduling.md) §10) |

**Each row is a component that does not need upgrading, monitoring, securing, or explaining to the next on-call engineer.** The requirement was explicit that infrastructure stay simple; this table is where that requirement is actually honoured, and it is worth re-reading before adding anything.

## 5. Go version

**Go 1.25+.** Used deliberately: generics for the state machine ([10](10-state-machines.md) §1) and typed stores, `log/slog`, `errors.Join`, `t.Context()` in tests, and range-over-func iterators for paginated registry listings.

> **Revised during M1 from the 1.24 originally specified.** `prometheus/client_golang` and the OpenTelemetry stack now declare `go 1.25`, so `go mod tidy` raised the module directive. Pinning the observability stack backwards to preserve 1.24 would mean running behind on metrics and tracing for no benefit, so 1.25 is the floor. Container images build on `golang:1.25` and CI pins the same.

---

## 6. Divergences recorded during implementation

Doc 17 §4 item 6 requires that implementation divergences be written down rather than silently absorbed. These were found while building M1.

### `go-playground/validator` was dropped

*Planned:* struct-tag validation for product configuration.

*Actual:* hand-rolled validation in `internal/product/validate.go`.

*Why:* the three error classes that justify the validator's existence ([13](13-cli.md) §9) are a non-compiling `tagPattern`, a rule naming an undeclared target, and keyless verification with no `certificateIdentity`. All three are semantic or cross-field, and **none is expressible as a struct tag.** The library would have covered only the trivial field checks — each about three lines by hand — while still needing its messages translated into the `spec.path[i].field` form the CLI prints. Hand-rolling produced better errors *and* removed a dependency, which is what §4 of this document asks for.

### `depguard` replaced `go-arch-lint`

*Planned:* `go-arch-lint` enforcing the dependency direction rules in [15](15-code-layout.md) §3.

*Actual:* `depguard` rules inside the existing `golangci-lint` pass.

*Why:* it encodes the same rules — platform never imports a domain package, domain never imports `api`, store never imports domain — with no additional tool to install in CI and no second config format. One lint invocation, one place to look when it fails.

### `revive`'s `exported` rule is disabled

It fired ~50 times, almost entirely on state-machine and enum constants whose names already carry their meaning. `// JobPending is the pending state` is the restatement-of-identifier noise that teaches readers to skip comments. The narrative lives in package doc comments and in this document set. Types and functions with non-obvious behaviour are documented regardless — enforced by review, which can tell explanation from repetition.

---

These were found while building M2.

### `sqlc` was deferred again — and this is the last time

*Planned:* generated, compile-time-checked queries.

*Actual:* hand-written SQL behind the `Dialect` placeholder rewriter, in `internal/store/packages.go` and `internal/catalog/catalog.go`.

*Why:* M2's queries are simple — inserts with `ON CONFLICT DO NOTHING`, one filtered list, one ordered lookup — and dual-dialect codegen costs more setup than it returns on that shape. The decision is made in M3 against the real test case: the dequeue statement with `FOR UPDATE SKIP LOCKED` is hand-tuned, correctness-critical, and dialect-divergent enough that it will not go through the rewriter at all. That is where compile-time checking pays, so that is where the choice gets made — not guessed at here.

*What would change our mind sooner:* a query whose column list drifts from the schema without a test catching it. None has yet, because every query in this milestone is exercised against a migrated database in a test.

### The registry retry schedule and the discovery backoff compose

Not a divergence from a planned choice, but a behaviour worth writing down because it surprised us in verification.

A hard registry outage takes **~2 minutes** to surface as a failed scan, not ~1 second. The transport retries the transient class eight times with full-jitter backoff ([10](10-state-machines.md) §6), and only when those are exhausted does the scan return an error and the discovery loop's own backoff ([07](07-discovery.md) §7) engage. The two layers are correct individually and multiply.

This is the right behaviour — a registry blip should be absorbed by the transport without the loop ever noticing — but it means `discovery_last_success_timestamp_seconds` is the metric to alert on, not scan failure rate: the failure takes minutes to appear, while the staleness is visible immediately.

### Registry backends self-register

`internal/registry/factory.go` holds a name-to-constructor map that backends populate from `init`. The abstraction does not import its implementations, so `generic` — and later `acr`, `artifactory`, `quay` — can be added without editing the interface they implement. The cost is that something must import the backend for its side effect; `internal/discovery/clients.go` does, with a comment saying why.

### Task replaced make

*Planned:* a Makefile ([14](14-deployment-and-development.md) §5.3).

*Actual:* `Taskfile.yml` and [Task](https://taskfile.dev).

*Why:* the Makefile required `bash` and `find`, so on Windows it could not run at all — PowerShell and cmd were both out, and a Windows developer had to install Git Bash or WSL before their first build. The cross-platform claim was asserted and never tested, which is how `go build -o <name>` shipping binaries without a `.exe` suffix survived: nothing on any pipeline built for Windows.

Task ships its own POSIX shell interpreter, so one definition runs identically on Linux, macOS and Windows. CI gained a `windows-latest` job that builds and tests there on every commit, and an assertion that every cross-compiled Windows binary carries `.exe` and no unsuffixed one exists.

*Two bugs the migration itself introduced and the verification caught*, both worth recording because they are easy to repeat:

- Task's built-in `{{exeExt}}` keys off the **runtime** OS, not `GOOS`. Using it would have made `GOOS=windows task build` produce an extensionless binary — reintroducing the exact bug being fixed. The Taskfile derives the suffix from `{{default OS (env "GOOS")}}` instead.
- A Taskfile-level `env: CGO_ENABLED: '0'` broke the entire test suite: `go test -race` requires cgo. Shipped binaries are still static; the variable is set on the build tasks only.

*What would change our mind:* nothing likely. The cost is one tool to install, via a single `go install` — cheaper than the Git Bash prerequisite it replaced.

*A third bug, found by a developer rather than by us.* The first Taskfile shelled out to `date -u` for the build timestamp. There is no `date` executable on Windows — PowerShell's `date` is a `Get-Date` alias, not a binary — so the whole Taskfile failed to parse before any task could run:

```
task: Command "date -u +%Y-%m-%dT%H:%M:%SZ" failed: exit status 127
```

The lesson is a distinction that was glossed over when this was written: **Task interprets shell SYNTAX in-process, but an external command still has to exist on PATH.** Pipes, `&&`, `if` and redirection are portable; `date`, `rm`, `tail`, `grep`, `sed` and `awk` are not. The timestamp now comes from a template function evaluated in-process, and the two commands that genuinely need `rm` and `tail` are `platforms:`-gated.

It shipped because the Windows CI job had been written but had never run. There is now also a `portable` job that strips PATH to `go`, `gofmt`, `git` and `task` and runs every task — it reproduces the failure on Linux in seconds, so the class is caught without waiting on a Windows runner.
