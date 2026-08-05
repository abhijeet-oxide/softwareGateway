# Developer Guide

How to build, configure, test and run softwareGateway on your own machine.

This is the practical companion to [`docs/design/`](design/README.md). The design docs explain *why*; this explains *how to get it running in the next ten minutes*.

> **What works today.** Discovery works end to end: point it at an OCI registry and it finds what is published, records packages with their artifact trees, and evaluates auto-download rules. **Byte transfer does not exist yet** — a transfer request will sit in `pending`. That is expected, not a bug. See [Where the milestones are](#where-the-milestones-are).

---

## Contents

1. [Prerequisites](#1-prerequisites)
2. [Build](#2-build)
3. [Configure](#3-configure)
4. [Run](#4-run)
5. [Test](#5-test)
6. [A complete worked example](#6-a-complete-worked-example)
7. [Troubleshooting](#7-troubleshooting)
8. [Where the milestones are](#8-where-the-milestones-are)

---

## 1. Prerequisites

| Tool | Version | Needed for |
|---|---|---|
| **Go** | **1.25+** | everything. Non-negotiable: `prometheus/client_golang` and the OpenTelemetry SDK both require it |
| **Task** | 3.x | the task runner. `go install github.com/go-task/task/v3/cmd/task@latest` |
| `git` | any | version stamping in the binary |
| `golangci-lint` | v2.x | `task lint`. **v2, not v1** — the config uses the v2 schema and a v1 binary rejects it |
| Docker | any | **not required.** Only for Postgres and the integration suite |

```bash
go version        # must be >= 1.25
task --version    # 3.x
```

Installing Task:

```bash
go install github.com/go-task/task/v3/cmd/task@latest   # any platform, needs Go
brew install go-task                                     # macOS
winget install Task.Task                                 # Windows
```

Everything Task runs is a plain `go` command. If you would rather not install it, `task --list` shows every task and `task --dry <name>` prints the commands without running them — copy and paste as you like.

### Why Task and not Make

The Makefile it replaced needed `bash` and `find`, so **PowerShell and cmd could not run it at all** — a Windows developer had to install Git Bash or WSL before their first build.

Task interprets shell **syntax** in-process (pipes, `&&`, `if`, redirection), so those work everywhere. But an **external command still has to exist on PATH**, and that distinction matters: `date`, `rm`, `tail`, `grep`, `sed` and `awk` do not exist in PowerShell. Shelling out to one fails the whole Taskfile before any task runs:

```
task: Command "date -u +%Y-%m-%dT%H:%M:%SZ" failed: exit status 127
"date": executable file not found in $PATH
```

The Taskfile therefore avoids them: the build timestamp comes from a template function evaluated in-process, and the two places that genuinely need `rm` and `tail` are `platforms:`-gated with PowerShell equivalents. Two CI jobs keep it that way — one runs every task with PATH stripped to `go`, `gofmt`, `git` and `task`, and one builds and tests on a real `windows-latest` runner.

**There is no CGO in a shipped binary.** SQLite is `modernc.org/sqlite`, a pure-Go translation, so builds set `CGO_ENABLED=0` and there is no C toolchain to install. Tests are the exception: `go test -race` requires cgo, so `CGO_ENABLED=0` is set on the build tasks only, never globally.

---

## 2. Build

### The short version

```bash
task build          # → bin/coordinator, bin/worker, bin/transferctl
```

On Windows this produces `bin/coordinator.exe`, `bin/worker.exe`, `bin/transferctl.exe`. **You never have to rename anything.** If you are, you are on a build from before this was fixed — pull and rebuild.

<details>
<summary><b>Why the <code>.exe</code> problem existed, if you hit it</b></summary>

`go build` appends `.exe` on Windows **only when you do not pass `-o`**. The old Makefile passed `-o bin/transferctl`, so Go wrote exactly that — an extensionless file Windows refuses to execute.

Task has a built-in `{{exeExt}}`, but it keys off the **runtime** OS, so `GOOS=windows task build` on Linux would have reintroduced the same bug. The Taskfile resolves the suffix against the *target* instead:

```yaml
TARGET_OS: '{{default OS (env "GOOS")}}'
EXE: '{{if eq .TARGET_OS "windows"}}.exe{{end}}'
```

CI now asserts every Windows binary carries `.exe` and that no unsuffixed one exists, so this cannot come back silently.
</details>

### Everyday tasks

```bash
task                        # list every task with its description
task build                  # all three binaries
task build:transferctl      # just one
task build:all              # cross-compile: 3 OSes × 2 arches × 3 binaries
task clean                  # remove bin/, dist/, the dev database, coverage
```

`task build` skips work when nothing changed. Unlike make, Task compares **checksums rather than timestamps**, so `touch` alone will not trigger a pointless rebuild while a real edit always does.

### Cross-compiling

```bash
task build:all              # → dist/{linux,darwin,windows}-{amd64,arm64}/
GOOS=windows task build     # just one target, into bin/
```

Useful for handing a colleague a binary without asking them to install Go.

---

## 3. Configure

There are **two separate kinds of configuration**, and conflating them is the most common early mistake.

| | **System config** | **Product config** |
|---|---|---|
| **What** | addresses, database DSN, log level, tick intervals | which vendors, which repositories, which rules |
| **Who owns it** | the operator / platform team | the team that owns the product |
| **Where** | one file, `--config` | one YAML file **per product** in a directory |
| **In Kubernetes** | a ConfigMap or Helm values | one ConfigMap per product, managed by Flux |
| **Changed by** | redeploy | a Git commit — **hot-reloaded, no restart** |
| **Schema** | `internal/platform/config` | `internal/product/schema.go`, [doc 02](design/02-configuration.md) |

### 3.1 System config

Precedence, lowest to highest: **defaults → file → `SWGW_` environment variables**.

A missing file is not an error — defaults plus environment must be enough to start. That is what makes `go run ./cmd/coordinator` work with zero setup.

```yaml
# dev/config.yaml
apiVersion: softwaregateway.io/v1alpha1
kind: SystemConfig

# Root for products/ and secrets/. In-cluster this is /etc/softwaregateway.
# The tool looks for <configDir>/products and <configDir>/secrets.
configDir: ./dev

server:
  address: :8080
  shutdownGracePeriod: 15s

database:
  driver: sqlite            # sqlite | postgres
  dsn: ./dev/swgw.db

coordinator:
  leaderElection:
    enabled: true           # ignored on SQLite: one process has nothing to contend with
    lockID: 1
    retryInterval: 10s

observability:
  log:
    level: info             # debug | info | warn | error
    format: text            # text for a terminal, json in production
  metrics:
    enabled: true
    path: /metrics
```

#### The one key people get wrong

**`configDir`, not `paths.products`.** Product files and secrets are found *relative to* `configDir`:

```
<configDir>/
├── products/          ← one YAML per product
│   ├── vendor-a.yaml
│   └── vendor-b.yaml
└── secrets/           ← one directory per secret
    └── vendor-a-registry/
        ├── username
        └── password
```

If the log says `dir=/etc/softwaregateway/products`, your `configDir` did not take effect.

#### Environment overrides

`SWGW_` + the config path, with dots as underscores. Case does not matter:

```bash
SWGW_SERVER_ADDRESS=":9090"
SWGW_DATABASE_DRIVER=postgres
SWGW_DATABASE_DSN='postgres://swgw:swgw@localhost:5432/swgw?sslmode=disable'
SWGW_DATABASE_MAXOPENCONNS=50                     # → database.maxOpenConns
SWGW_COORDINATOR_LEADERELECTION_ENABLED=false     # → coordinator.leaderElection.enabled
SWGW_OBSERVABILITY_LOG_LEVEL=debug
```

**A typo'd `SWGW_` variable fails startup with an error naming it.** This is deliberate: an operator who believes they changed something they did not should find out at startup, not during an incident.

The DSN expands `${VAR}`, so a manifest can reference a secret without the literal appearing in a config file:

```yaml
database:
  dsn: "postgres://swgw:${PGPASSWORD}@postgres:5432/swgw?sslmode=require"
```

#### Full system config reference

<details>
<summary>Every key and its default</summary>

| Key | Default | Notes |
|---|---|---|
| `configDir` | `/etc/softwaregateway` | root for `products/` and `secrets/` |
| `server.address` | `:8080` | |
| `server.shutdownGracePeriod` | `30s` | drain time for in-flight requests |
| `database.driver` | `sqlite` | `sqlite` \| `postgres` |
| `database.dsn` | `./dev/swgw.db` | `${VAR}` is expanded |
| `database.maxOpenConns` | `25` | |
| `database.maxIdleConns` | `10` | |
| `database.connMaxLifetime` | `1h` | |
| `coordinator.leaderElection.enabled` | `true` | no-op on SQLite |
| `coordinator.leaderElection.lockID` | `1` | the `pg_advisory_lock` key |
| `coordinator.leaderElection.retryInterval` | `10s` | how often a follower tries |
| `coordinator.scheduler.tickInterval` | `10s` | M4 |
| `coordinator.reaper.tickInterval` | `30s` | M3 |
| `coordinator.reaper.leaseDuration` | `2m` | **must exceed `tickInterval`** or leases expire faster than the reaper can see them — validation rejects it |
| `coordinator.queue.maxLeaseBatchSize` | `32` | M3 |
| `coordinator.gc.tickInterval` | `1h` | M6 |
| `worker.coordinatorEndpoint` | `http://localhost:8080` | M3 |
| `worker.address` | `:8081` | |
| `worker.maxConcurrentJobs` | `16` | M3 |
| `worker.copyBufferSize` | `1048576` | 1 MiB; M3 |
| `observability.log.level` | `info` | |
| `observability.log.format` | `json` | `text` is easier at a terminal |
| `observability.metrics.enabled` | `true` | |
| `observability.tracing.enabled` | `false` | |
| `observability.tracing.sampleRatio` | `0.05` | must be within `[0,1]` |
| `retention.*` | 7d–365d | M6 |

</details>

### 3.2 Product config

**One file per product.** The blast radius of a mistake is one product — a syntax error in `vendor-b.yaml` never stops `vendor-a` from working. Invalid products are reported and skipped; the previously valid version keeps running.

Minimum viable product file:

```yaml
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata:
  name: vendor-a                    # lowercase, hyphens, ≤63 chars; appears in URLs and metrics
spec:
  sources:                          # where packages come from — read-only
    - name: vendor
      registry: registry.vendor-a.example.com
      repository: platform/suite
      credentialsRef:
        secretName: vendor-a-registry
      discovery:
        enabled: true
        interval: 15m
  targets:                          # where they go — read-write
    - name: internal
      registry: internal.example.com
      repository: mirror/vendor-a
      credentialsRef:
        secretName: internal-registry
      default: true
```

Validate before committing:

```bash
./bin/transferctl config validate ./dev/products
```

#### A source can cover several repositories

A product whose components ship as separate repositories declares them all under **one** source — they share a registry host, one credential and one rate-limit budget:

```yaml
  sources:
    - name: components
      registry: registry.vendor-c.example.com
      repositories:                     # instead of `repository:`
        - suite/core
        - suite/database
        - suite/frontend
      credentialsRef:
        secretName: vendor-c-registry
```

`repository:` (singular) still works for the common one-repository case, and the two merge if you use both.

#### Repository filters, and finding repositories automatically

`repositoryFilters` narrows the repository set the same way `tagFilters` narrows tags — include, then exclude, exclude always wins:

```yaml
      repositoryFilters:
        include: ['^suite/']
        exclude: ['-test$', '-scratch$']
```

To have the tool **find** repositories instead of listing them, turn on catalog enumeration:

```yaml
      repositoryDiscovery:
        enabled: true                   # uses /v2/_catalog
        maxRepositories: 100            # default 200
      repositoryFilters:
        include: ['^suite/']            # required in practice — see below
```

**Off by default, and validation insists on filters when you enable it.** An unfiltered catalog scan of a shared registry adopts every other team's repositories. Three more things worth knowing:

- Many **vendor** credentials cannot list a registry — they are scoped to pulling named repositories. The tool says so explicitly rather than reporting a generic 403, and **keeps scanning any repositories you named explicitly**. Catalog enumeration earns its place on an internal registry you control.
- The repository set is **re-resolved on every scan**, so a repository published since the last pass is found without a restart or a reload.
- Repositories found this way are marked `managed_by = discovery` in the database, so a configuration reload does not deactivate them.

#### Tag filters — applied *before* any network call

```yaml
      discovery:
        tagFilters:
          include: ['^v\d+\.\d+\.\d+$']       # GA releases only
          exclude: ['-(rc|beta|alpha)\.']     # exclude always wins
```

Filters bound **scan cost**, not just what gets stored: a filtered tag costs zero requests. This is the mitigation for a repository with tens of thousands of tags.

Patterns are RE2 (Go `regexp`) — linear time, no backtracking. A backtracking engine evaluating a user-supplied pattern inside a polling loop would be a denial-of-service vector.

#### Auto-download rules — first match wins

```yaml
  autoDownload:
    enabled: true
    rules:
      - name: ga-releases                     # evaluated in order
        tagPattern: '^v\d+\.\d+\.\d+$'
        targets: [internal]
        priority: 100
      - name: release-candidates
        tagPattern: '^v\d+\.\d+\.\d+-rc\.\d+$'
        targets: [internal]
        priority: 10
```

**First match wins, not all matches.** Two rules matching one tag with different priorities and different targets has no sensible interpretation, and "most specific" is not an order that exists over regexes.

`enabled: false` disables the rules without deleting them.

#### TLS: private and internal CAs

Put the CA bundle in a secret and point `network.caBundleRef` at it. It works at product level and per source or target:

```yaml
spec:
  network:
    caBundleRef:
      secretName: internal-ca
      key: ca.crt                       # default; override for another filename
    proxy:
      httpsProxy: http://proxy.example.com:3128
      noProxy: [".svc.cluster.local", "internal.example.com"]
    timeouts:
      connect: 10s
      responseHeader: 30s

  sources:
    - name: vendor
      # ...
      network:                          # overrides the product's where set
        proxy:
          noProxy: ["internal.example.com"]
```

The bundle is a PEM file at `<configDir>/secrets/internal-ca/ca.crt`, projected by VSO in-cluster.

Two properties worth knowing:

- It is **appended to the system roots, never replacing them.** A product that adds a private CA still needs to reach public registries and Sigstore.
- **There is deliberately no `insecureSkipVerify`.** Disabling verification is never the right fix, and an option to do it gets set in production "temporarily" exactly once. Supply the CA instead.

A complete example is in [`dev/products/vendor-c-multirepo.yaml`](../dev/products/vendor-c-multirepo.yaml).

#### Credentials

Every source and target needs **either** `credentialsRef` **or** `anonymous: true`. There is no third option — a missing `credentialsRef` fails loudly rather than silently downgrading to anonymous and failing later as a confusing 401.

Secrets are read from **projected volume mounts**, never the Kubernetes API. No client-go, no cluster-wide Secret read permission, no API-server load, and the same code path works locally against a plain directory:

```
<configDir>/secrets/vendor-a-registry/
├── username        ← file content is the value; trailing newlines are trimmed
└── password
```

```bash
mkdir -p dev/secrets/vendor-a-registry
printf 'svc-account' > dev/secrets/vendor-a-registry/username
printf 'the-token'   > dev/secrets/vendor-a-registry/password
```

In Kubernetes, **Vault Secrets Operator writes these**; the tool only reads what VSO projects.

A full annotated example with verification, notifications, promotion targets and rate limits is in [`dev/products/vendor-a-platform.yaml`](../dev/products/vendor-a-platform.yaml). The schema reference is [doc 02](design/02-configuration.md).

---

## 4. Run

### Zero setup

SQLite, no containers, no cluster:

```bash
task dev:coordinator
# or: go run ./cmd/coordinator --config ./dev/config.yaml
```

The database is created and migrated on first start. In another terminal:

```bash
export SWGW_ENDPOINT=http://localhost:8080

./bin/transferctl health
./bin/transferctl products list
./bin/transferctl packages list vendor-a
```

`SWGW_ENDPOINT` saves typing `--endpoint` on every call.

### With Postgres

```bash
docker compose up -d postgres

SWGW_DATABASE_DRIVER=postgres \
SWGW_DATABASE_DSN='postgres://swgw:swgw@localhost:5432/swgw?sslmode=disable' \
  go run ./cmd/coordinator --config ./dev/config.yaml
```

SQLite is a development convenience and is **not supported in production** — the Coordinator warns loudly at startup. Use Postgres for anything you care about; leader election and the M3 queue both depend on Postgres semantics that SQLite does not have.

### CLI commands that work today

```bash
transferctl version                              # client and server versions
transferctl health                               # deep check of every dependency
transferctl config validate <dir>                # validate product YAML

transferctl products list
transferctl products describe <product>

transferctl packages list <product>              # what has been discovered
transferctl packages list <product> --repository suite/core
transferctl packages list <product> --tag v1.0.0
transferctl packages list <product> --state superseded
transferctl packages list <product> --all        # follow pagination

transferctl packages describe <product> <tag-or-digest>    # artifact tree
transferctl packages discover <product>          # scan now, don't wait for the interval
transferctl packages discover <product> --source vendor
```

Every command takes `-o json` or `-o yaml` for scripting.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | generic failure |
| 2 | usage error — bad flags or arguments |
| 3 | Coordinator unreachable — the service is down |
| 4 | not found |
| 5 | failed precondition — e.g. `packages discover` on a follower |
| 6 | partial failure — the operation ran but something in it failed |

The distinctions earn their keep in scripts. 3 versus 4 separates "the service is down" from "you asked for something that does not exist". 6 exists so CI cannot report green on a scan where half the tags failed, or on a config directory where one file is invalid:

```bash
transferctl config validate ./products || exit $?   # 6 if any file is invalid
```

### Discovery behaviour worth knowing

- **Every scan is a full scan.** No cursor, no watermark. The OCI tag list has no ordering guarantee and no change feed, so an incremental scheme would need a full scan to reconcile anyway. The payoff: it is self-healing — a crash, an outage, or a stale replica all resolve on the next pass with no repair path.
- **Discovery runs on the leader only.** On a follower, `packages discover` returns a precondition failure explaining why, not a 404.
- **A scan that finds nothing is the normal steady state**, not a failure.
- **A source is never disabled by a failure.** It backs off to at most 4× its interval and recovers on its own. A source that turned itself off is one nobody remembers to turn back on.

---

## 5. Test

```bash
task test          # go test -race ./...   — the one to run before pushing
task test:short    # without the race detector, faster
task cover         # with a coverage summary
task lint          # golangci-lint (v2)
task check         # fmt + vet + lint + test
task ci            # exactly what the pipeline runs, including the tidy check
```

### The unit suite does not need Docker

This is a rule worth protecting. Tests run against an in-process OCI Distribution v2 server ([`test/fakeregistry`](../test/fakeregistry)) and SQLite. A suite that needs containers is a suite developers run less often, and the difference compounds.

The fake registry does token auth, `Link`-header pagination, manifest `HEAD`/`GET`, and injectable faults (503, 429 with `Retry-After`, mid-response truncation), so retry and backoff paths are tested deterministically rather than by hoping a real registry misbehaves.

### Running a subset

```bash
task test:pkg -- ./internal/discovery/            # one package, with -race
task test:run -- TestSupersession                 # everything matching a pattern, verbose

# or plain go, which is all the tasks above are
go test -race ./internal/discovery/
```

The loop tests are genuinely concurrent — run those with `-race`, which both task shortcuts do.

### Writing tests

Follow the existing harnesses rather than inventing new ones:

- `internal/discovery/discovery_test.go` — fake registry + migrated SQLite + reconciled catalog + Scanner. Copy `newHarness`.
- `internal/api/packages_test.go` — `httptest` server over a real store, with a fake `Discoverer`.
- `internal/store/store_test.go` — `openTestStore` gives a migrated SQLite store on a temp file.

Two conventions that matter:

**Use a fast retry policy in tests.** The production schedule is 8 attempts with backoff, so one persistent-failure test can spend two minutes in `time.Sleep`:

```go
Transport: transport.Config{
    MaxRetryAttempts: 3,
    RetryBaseDelay:   time.Millisecond,
    RetryMaxDelay:    5 * time.Millisecond,
},
```

**Clear injected faults before asserting recovery.** `FailNext(path, 503, 100)` queues 100 faults; the retry schedule consumes only a few. Call `reg.ClearFaults()` for the "outage is over" half of the test, or the recovery you meant to assert never happens and the failure looks like a bug in the code rather than in the test.

### Integration tests

```bash
task test:integration     # requires Docker
```

Build-tagged `integration`, excluded from the default run.

---

## 6. A complete worked example

From a fresh clone to seeing a package discovered, with no vendor account and no Docker.

**1 — Build.**

```bash
task build
```

**2 — Point a product at a registry you control.** For a real trial, `docker run -d -p 5000:5000 registry:2` and push an image; if Docker is unavailable, use any registry you can reach.

```bash
mkdir -p dev/products dev/secrets
cat > dev/products/demo.yaml <<'YAML'
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata:
  name: demo
spec:
  sources:
    - name: local
      registry: localhost:5000
      repository: library/demo
      anonymous: true
      discovery:
        enabled: true
        interval: 30s
  targets:
    - name: internal
      registry: localhost:5000
      repository: mirror/demo
      anonymous: true
      default: true
  autoDownload:
    enabled: true
    rules:
      - name: releases
        tagPattern: '^v\d+\.\d+\.\d+$'
        targets: [internal]
YAML
```

`localhost` and `127.0.0.1` are the only hosts allowed to use plain HTTP; everything else must be TLS.

**3 — Validate before running.**

```bash
./bin/transferctl config validate ./dev/products
```

**4 — Start the Coordinator.**

```bash
go run ./cmd/coordinator --config ./dev/config.yaml
```

Look for these lines — they are the checkpoints:

```
catalog: reconciled     products=1 repositories=2
discovery started       sources=1
discovered package      tag=v1.0.0 digest=sha256:... artifacts=1 blobs=2 requests=1
```

**5 — Look at it.**

```bash
export SWGW_ENDPOINT=http://localhost:8080

./bin/transferctl packages list demo
./bin/transferctl packages describe demo v1.0.0
./bin/transferctl packages discover demo          # re-scan: finds nothing, which is correct
```

**6 — Watch supersession.** Re-push the *same tag* with different content, then re-scan. A new package row appears and the old one becomes `superseded` with `superseded_by` set. Different tags never do this to each other — `v1.0.0` and `v1.1.0` coexist indefinitely.

**7 — Watch hot reload.** Edit `dev/products/demo.yaml` — change the interval, add a tag filter — and save. Discovery stops and restarts with the new configuration. No restart, no signal.

---

## 7. Troubleshooting

**`bin/transferctl` will not run on Windows / has no `.exe`**
Pull and rebuild — `task build` appends the suffix, and CI asserts it. If you would rather not install Task: `go build -o bin/transferctl.exe ./cmd/transferctl`.

**`task build` does nothing**
Correct — nothing changed. Task compares checksums, so it rebuilds on a real edit and skips a `touch`. `task clean build` forces it.

**`task: command not found`**
Install it: `go install github.com/go-task/task/v3/cmd/task@latest`, then make sure `$(go env GOPATH)/bin` is on your `PATH`. Or skip it — `task --dry <name>` prints the underlying `go` commands.

**`task: Command "<something>" failed: exit status 127` on Windows**
The Taskfile is calling a Unix command PowerShell does not have. Pull first — `date` was one, and it is fixed. If you hit a new one after editing the Taskfile, either replace it with a template function or gate it:

```yaml
cmds:
  - cmd: rm -rf bin
    platforms: [linux, darwin]
  - cmd: powershell -NoProfile -Command "Remove-Item -Recurse -Force bin"
    platforms: [windows]
```

The `portable` CI job catches this on Linux without waiting for the Windows runner.

**Products are not loading; the log shows `/etc/softwaregateway/products`**
Your `configDir` did not apply. The key is top-level `configDir`, not `paths.products`. Confirm with `--config` pointing at the right file.

**A `SWGW_` variable seems ignored**
It no longer can be — an unknown one fails startup by name. If you are on an older build, `SWGW_` variables for camelCase keys (`maxOpenConns`, `leaderElection`) were silently dropped. Pull and rebuild.

**`packages discover` returns a precondition failure**
Discovery runs on the leader only. Either this replica is a follower, or discovery has no enabled sources. Check the startup log for `discovery started sources=N`.

**`packages list` is empty**
Discovery polls on its interval; the first scan happens at startup. Force one with `transferctl packages discover <product>`. If it reports `tagsListed=0`, the repository path or credentials are wrong — `transferctl health` checks reachability.

**Discovery is slow to report a registry outage**
Expected, and worth understanding: a hard outage takes **~2 minutes** to surface. The transport retries the transient class eight times with backoff, and only when those are exhausted does the loop's own backoff engage. Both layers are right individually and they multiply. This is why the alert is on `discovery_last_success_timestamp_seconds` staleness rather than failure rate — staleness is visible immediately.

**`golangci-lint` rejects the config**
You have v1. The config uses the v2 schema. Install v2.

**Auth failures take a long time**
They should not — a non-retryable error returns immediately. If a missing credential takes minutes, you are on a build from before that fix.

---

## 8. Where the milestones are

| | Milestone | State |
|---|---|---|
| **M1** | Foundation — config, schema, migrations, health, API skeleton, three binaries | ✅ done |
| **M2** | **Discovery** — registry client, full-scan discovery, supersession, auto-download rules, packages API + CLI | ✅ **done** |
| M3 | **Transfer** — blob queue, worker lease loop, streaming copy, dedupe, mount | next |
| M4 | Scheduling, promotion, vendor registry backends | |
| M5 | Verification (cosign), notifications | |
| M6 | GC, retention, hardening | |

**What "M3 is next" means for you now.** Auto-download rules create `transfer_requests` rows and they stay `pending`, because nothing consumes the queue yet. `transferctl packages describe` says so explicitly rather than leaving you to wonder. Everything in the discovery path is real: real HTTP, real digests, real database rows.

M3 also closes [ADR-001](design/16-technology-choices.md) — whether to adopt `go-containerregistry` or `oras-go/v2`. It is deliberately still open: nothing built so far touches an OCI library (the registry client is plain `net/http`), so the decision gets made against measurements on the blob path, where the difference actually shows.

---

## See also

- [`docs/design/README.md`](design/README.md) — the full design set, 18 documents
- [`docs/FUNCTIONAL-OVERVIEW.md`](FUNCTIONAL-OVERVIEW.md) — what the tool does, in day-to-day terms
- [`docs/design/02-configuration.md`](design/02-configuration.md) — complete configuration schema
- [`docs/design/07-discovery.md`](design/07-discovery.md) — discovery semantics, including supersession
- [`docs/design/14-deployment-and-development.md`](design/14-deployment-and-development.md) — Kubernetes and Flux
- [`docs/design/16-technology-choices.md`](design/16-technology-choices.md) — library choices and recorded divergences
