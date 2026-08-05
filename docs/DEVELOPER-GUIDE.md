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
| `tls.allowNegativeSerialNumbers` | `false` | accepts certificates whose serial number is negative. **Process-wide**, and the only fix for `x509: negative serial number` — see [below](#insecureskipverify-will-not-fix-x509-negative-serial-number) |

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

#### Repositories: name one, name several, or name none

A source is **one registry**. Which repositories on it get scanned is decided by one question: *did you name any?*

```yaml
  sources:
    # One.
    - name: vendor
      registry: registry.vendor-a.example.com
      repository: platform/suite

    # Several — a product whose components ship separately.
    - name: components
      registry: registry.vendor-c.example.com
      repositories:
        - suite/core
        - suite/database
        - suite/frontend

    # NONE — "I do not know them yet, find them."
    - name: plugins
      registry: internal.example.com
      discovery:
        repositoryFilters:
          include: ['^vendor-c/plugins/']
```

**Naming none is the important case.** If every new component ships as a *new* repository, you cannot list them in advance — and listing them by hand means a component is silently not replicated until somebody remembers to edit the ConfigMap. Naming nothing means "every repository on this registry", re-resolved on **every scan**, so a repository published five minutes ago is found on the next pass. No restart, no reload.

There is deliberately **no `enabled` switch** for this. Naming nothing *is* the statement. A separate flag would let configuration say one thing and mean another — repositories listed with discovery off, or nothing listed with discovery off, which scans nothing while looking configured.

Declaring several under one source rather than one source each is also deliberate: they share a registry host, one credential and one rate-limit budget. Three sources would duplicate all three and let the per-repository budgets multiply against a vendor that only ever sees one client.

#### Both filters live under `discovery`

Because discovery is what they govern. A scan finds **repositories**, then **tags**, and each step gets a filter:

```yaml
      discovery:
        enabled: true
        interval: 15m
        repositoryFilters:                    # which repositories to scan
          include: ['^suite/']
          exclude: ['-test$', '-scratch$']
        tagFilters:                           # which tags within them to record
          include: ['^v\d+\.\d+\.\d+$']
        maxRepositories: 200                  # safety bound; default 200
```

`repositoryFilters` matters most for a source that names nothing. On a registry shared with other teams it is what keeps the scope to yours — without it, *every* repository on that host gets scanned.

It is **not mandatory**, because a registry dedicated to one vendor genuinely needs no filter and refusing to start there would be wrong. Instead, `transferctl products check` reports the number that decides it:

```
WARNING  can list repositories
         47 repositories on this registry, and no repositoryFilters
         → every one of them will be scanned. Correct if this registry is
           dedicated to this product; on a shared registry add
           discovery.repositoryFilters.include to scope it
```

Both filters are RE2 (Go `regexp`) — include, then exclude, exclude always wins, and no include patterns admits everything.

#### When the registry will not list its repositories

Many **vendor** credentials cannot enumerate a registry — they are scoped to pulling named repositories. A source that names none has nothing to fall back on, so the scan fails, and the message says what to do:

```
the registry refused to list its repositories: this credential is probably
scoped to pulling named repositories rather than enumerating the registry.
Name them under `repositories:` instead
```

That is the practical split: **name repositories for a vendor registry, name none for an internal one you control.** `products check` tells you which you have before discovery ever runs.

#### Tag filters — applied *before* any network call

```yaml
      discovery:
        tagFilters:
          include: ['^v\d+\.\d+\.\d+$']       # GA releases only
          exclude: ['-(rc|beta|alpha)\.']     # exclude always wins
```

Filters bound **scan cost**, not just what gets stored: a filtered tag costs zero requests. This is the mitigation for a repository with tens of thousands of tags.

Patterns are RE2 (Go `regexp`) — linear time, no backtracking. A backtracking engine evaluating a user-supplied pattern inside a polling loop would be a denial-of-service vector.

#### Auto-download rules

This is the part that most repays reading once, because the mental model is not obvious from the schema.

**A rule is not a filter. It is a routing decision.**

Discovery records *every* tag that survives `tagFilters` — that is what makes the package list a complete picture of what the vendor has published. Rules answer a separate question about each newly discovered package:

> Should we pull this automatically, **where** should it go, and **how urgently**?

A package that matches no rule is still discovered, still listed, still describable. It simply is not fetched without someone asking. **No rules at all is a perfectly good configuration** — it means "tell me what exists, I will decide what to pull."

**Why more than one rule?** Because "should we pull this automatically" rarely has one answer for a whole repository. Different *classes of release* deserve different treatment, and the class is encoded in the tag:

```yaml
  autoDownload:
    enabled: true
    rules:
      # Evaluated top to bottom. FIRST MATCH WINS.

      - name: security-hotfixes
        tagPattern: '^v\d+\.\d+\.\d+-hotfix\.\d+$'
        targets: [lab, production]      # straight to both
        priority: 1000                  # ahead of everything queued
        verifyBeforeTransfer: true

      - name: ga-releases
        tagPattern: '^v\d+\.\d+\.\d+$'
        targets: [lab]                  # lab only; promotion to prod is a human decision
        priority: 100
        verifyBeforeTransfer: true

      - name: release-candidates
        tagPattern: '^v\d+\.\d+\.\d+-rc\.\d+$'
        targets: [lab]
        priority: 10                    # behind real releases when both are queued

      # Nightlies and anything else match nothing, so they are recorded and
      # left alone. That is the "no rule" case doing useful work.
```

Read as a table, which is what it is:

| Tag looks like | Goes to | Priority | Verified first |
|---|---|---|---|
| `v2.4.1-hotfix.1` | lab **and** production | 1000 | yes |
| `v2.4.1` | lab | 100 | yes |
| `v2.5.0-rc.3` | lab | 10 | no |
| `nightly-2026-01-14` | *nowhere* | — | — |

Three things each rule controls, and each is a real operational decision:

- **`targets` — where.** A hotfix might justify going straight to production; a release candidate must never. This is how you express "auto-download, but only somewhere safe."
- **`priority` — how urgently.** Priority orders the transfer queue. When a 40 GB nightly and a security hotfix are both waiting, priority is what decides which moves first. It matters most exactly when you are busiest.
- **`verifyBeforeTransfer` — how carefully.** Check the vendor's signature before spending an hour on the bytes.

**First match wins, and order is therefore load-bearing.** Put the *most specific* pattern first. In the example, `^v\d+\.\d+\.\d+-hotfix\.\d+$` must precede `^v\d+\.\d+\.\d+$`; reversed, a hotfix would… actually still not match the GA pattern (the `$` anchor saves it) — but with looser patterns like `^v2\.` first, everything below becomes dead code. Anchor your patterns and order most-specific-first, and this class of mistake disappears.

Why first-match rather than all-match: two rules matching one tag with different targets and different priorities has no sensible combined meaning — is it priority 1000 or 10? And "most specific wins" is not an order that exists over regular expressions, so it cannot be computed. Explicit order is the only unambiguous rule.

**Each match produces exactly one transfer request, forever.** The idempotency key is derived from the package, the resolved targets and the priority, so a re-scan, a Coordinator restart mid-evaluation, or two Coordinators briefly both believing they are leader all produce one request. This matters more here than anywhere else in the system: an auto-download rule is the one path that creates tens of gigabytes of work with nobody watching.

`enabled: false` turns the rules off without deleting them — useful while investigating whether a vendor's tagging changed.

#### Turning things off without deleting them

`enabled: false` at three levels. Every one defaults to **true**, so a document
that says nothing is on.

```yaml
metadata:
  name: vendor-a
  enabled: false          # the whole product stops

spec:
  sources:
    - name: primary
      enabled: false      # this source stops entirely
      discovery:
        enabled: false    # ...or just stop POLLING it (see below)
  targets:
    - name: decommissioned
      enabled: false      # stops receiving transfers
```

**Why not just delete the file?** Because deleting loses the thing you most want back — the exact registries, credentials, filters and rules that were working. Re-creating it from memory during an incident is how a "temporary" pause becomes a subtly different configuration.

A disabled product is still **loaded, validated and listed**. It just does nothing:

```
NAME     STATE      SOURCES   REPOSITORIES   TARGETS   DISCOVERY  ...
live     active     1         1              1         1 of 1
paused   DISABLED   1         1              1         1 of 1

1 product(s) disabled: still configured and validated, but not running.
Re-enable with `metadata.enabled: true`; their discovered packages are kept.
```

Validation still runs on it, deliberately — a mistake is reported now rather than discovered on the day someone re-enables it. Already-discovered packages are kept, and the catalog row is deactivated rather than deleted, so the transfer history that references it survives.

`transferctl products check` reports a disabled product as `[skip]` rather than probing it: there is nothing to diagnose about configuration that is deliberately not running.

**`enabled: false` vs `discovery.enabled: false` on a source** — a real distinction:

| | Effect |
|---|---|
| `enabled: false` | the source does not exist for any purpose |
| `discovery.enabled: false` | the source exists and can be transferred from on request, but is not polled |

Use the first when a vendor relationship is paused. Use the second for a failover mirror that must stay usable but must not double-discover every tag.

Validation catches the combinations that would fail silently: every source disabled while the product is on, `autoDownload` enabled with no enabled target, and a rule pointing at a disabled target — that last one would otherwise fail the first time a package matched it, potentially weeks later.

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

**Per-repository proxies.** Different registries routinely need different routes. Set fields win, unset ones inherit:

```yaml
spec:
  network:
    proxy:
      httpsProxy: http://corporate-proxy:3128     # the default for everything

  sources:
    - name: vendor-behind-other-proxy
      network:
        proxy:
          httpsProxy: http://dmz-proxy:8080       # a different one

    - name: internal-registry
      network:
        proxy:
          direct: true                            # no proxy at all
```

`direct: true` exists because a product-level proxy is **inherited**, and "everything through the corporate proxy except this one internal registry" is the normal shape. It also ignores `HTTPS_PROXY` from the environment — a repository that asked to bypass the proxy means it, and silently honouring a cluster-wide setting would make the option a no-op in exactly the deployment that needs it.

Targets take the same block: a destination inside the datacentre and a vendor outside it need different routes.

#### Turning certificate verification off for one repository

```yaml
spec:
  sources:
    - name: vendor-mid-migration
      network:
        tls:
          insecureSkipVerify: true
```

This stops the chain, the expiry **and** the hostname from being checked for that repository. The connection is still encrypted; it is no longer authenticated, so anything that can intercept it can serve you different bytes and nothing will notice.

It inherits like every other network setting, and because it is a three-state field you can turn it back **off** below a product that turned it on:

```yaml
spec:
  network:
    tls: {insecureSkipVerify: true}     # the estate is a mess

  sources:
    - name: the-one-good-registry
      network:
        tls: {insecureSkipVerify: false}   # ...except this one
```

Omitting the field means *inherit*. Writing `false` means *verify, whatever the level above says*. That distinction is the whole reason the field is not a plain boolean.

You will hear about it: the Coordinator logs a warning naming the product and source on every configuration reload, and `transferctl products check` reports a `certificate verification  WARNING` step for the repository. Setting `caBundleRef` and `insecureSkipVerify` in the **same** `network` block is rejected at validation — the bundle would never be consulted, so keeping both is dead configuration that reads as if it verifies.

#### `insecureSkipVerify` will **not** fix `x509: negative serial number`

If discovery fails with

```
tls: failed to parse certificate from server: x509: negative serial number
```

then skipping verification changes nothing, and neither does a CA bundle. Both were measured, not assumed — `internal/platform/tlscompat` has the test:

| client | result |
|---|---|
| default | `tls: failed to parse certificate from server: x509: negative serial number` |
| `insecureSkipVerify: true` | **the identical error** |
| `tls.allowNegativeSerialNumbers: true` | connects, with verification still fully on |

The reason is where the failure happens. Go's `crypto/x509` has rejected negative serial numbers since Go 1.23, and it rejects them while **parsing** the certificate — before any verification runs. `InsecureSkipVerify` turns off a step that is never reached.

The fix is in system config, not product config:

```yaml
# dev/config.yaml — or SWGW_TLS_ALLOWNEGATIVESERIALNUMBERS=true
tls:
  allowNegativeSerialNumbers: true
```

Set it on **both** the Coordinator and the Worker. The Coordinator discovers; the Worker moves the bytes. Setting it on only one gives you a product that discovers fine and fails every transfer at the handshake.

It lives in system config because it is implemented with Go's `GODEBUG` mechanism, which is per **process**. It cannot be scoped to one repository, and putting it under `network.tls` would have been a lie about its blast radius: it relaxes parsing for every registry, every Sigstore call, and every other outbound connection the process makes. Both binaries say so at startup, and the existing `GODEBUG` value is preserved rather than overwritten.

RFC 5280 §4.1.2.2 requires a positive serial number, so a certificate with a negative one is genuinely malformed — usually an appliance or enterprise CA encoding a random 20-byte value without clearing the high bit. The certificate is otherwise fine. Ask whoever runs that CA to reissue; until they do, this is the switch.

#### Signing trust per repository

Two vendors do not share a signing identity, so `verification` sits on sources and targets as well as on the product:

```yaml
spec:
  verification:                          # the default for everything
    enabled: true
    policy: enforce
    atSource: true
    cosign:
      mode: keyless
      keyless:
        certificateIdentity: 'https://github.com/vendor-a/...'
        certificateOidcIssuer: 'https://token.actions.githubusercontent.com'

  sources:
    - name: vendor-b
      verification:
        enabled: true
        atSource: true
        policy: warn                     # still onboarding their signing
        cosign:
          mode: key                      # they use a key, not keyless
          key:
            publicKeyRef: {secretName: vendor-b-cosign}
```

The merge is deliberately asymmetric:

- **Scalars inherit.** `policy`, `transferSignatures` and the rest are overridden individually, so a product states its posture once.
- **`cosign` replaces wholesale.** It is one coherent trust decision — a mode plus the identity or key that mode requires. Merging it field by field would silently produce combinations nobody wrote: a product's keyless certificate identity paired with a repository's key mode. A trust configuration assembled from two documents is one nobody can audit.

Every rule that applies to the product's cosign block applies to a repository's — including the important one, that keyless mode without `certificateIdentity` is rejected, because it would verify that *someone* signed the artifact rather than that the vendor did.

> Verification **executes** in M5. This is the configuration for it, validated now so it is correct when the machinery arrives.

Two properties worth knowing:

- It is **appended to the system roots, never replacing them.** A product that adds a private CA still needs to reach public registries and Sigstore.
- **There is deliberately no `insecureSkipVerify`.** Disabling verification is never the right fix, and an option to do it gets set in production "temporarily" exactly once. Supply the CA instead.

A complete example is in [`dev/products/vendor-c-multirepo.yaml`](../dev/products/vendor-c-multirepo.yaml).

#### Why `metadata.name` must be lowercase

Because it becomes a **Kubernetes object name**. Products are ConfigMaps, and RFC 1123 requires lowercase alphanumerics and hyphens — an uppercase product name simply cannot be applied to a cluster. Everything else (API paths, metric labels, database rows) would tolerate mixed case; the cluster will not, and finding that out at `kubectl apply` rather than at `config validate` is a worse experience.

`displayName` is the field for human-facing casing, and it has no restrictions:

```yaml
metadata:
  name: vendor-a-platform             # the machine identity
  displayName: "Vendor A Platform Suite"   # what people read
```

`displayName` is what `products describe` shows.

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
transferctl health                               # is the SERVICE working?
transferctl config validate <dir>                # validate product YAML offline

transferctl products list                        # every product, at a glance
transferctl products describe <product>          # full configuration
transferctl products check [product]             # can we reach the registries?

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
| 3 | no answer — the Coordinator is unreachable, **or** it did not reply within the timeout |
| 4 | not found |
| 5 | failed precondition — e.g. `packages discover` on a follower |
| 6 | partial failure — the operation ran but something in it failed |

The distinctions earn their keep in scripts. 3 versus 4 separates "the service is down" from "you asked for something that does not exist". 6 exists so CI cannot report green on a scan where half the tags failed, or on a config directory where one file is invalid:

```bash
transferctl config validate ./products || exit $?   # 6 if any file is invalid
```

Code 3 covers both "nothing is listening" and "it is still working on your request": from a script's point of view no answer came back either way. The *message* distinguishes them, because a human acts on them in opposite directions.

### Timeouts

`--timeout` bounds one request to the Coordinator. It defaults to **30 seconds**, except on the two commands that reach third-party registries through it:

| Command | Default timeout |
|---|---|
| everything else | 30s |
| `transferctl products check` | 10m |
| `transferctl packages discover` | 10m |

Those two are slow because the work is slow, not because anything is wrong. `products check` opens a TLS connection to every repository a product declares and runs several round trips against each; `discover` lists every tag of every repository and then resolves each one. Through a corporate proxy, against a registry across a WAN link, minutes is normal. A shared 30-second deadline made both of them fail almost every time, and fail with

```
Error: coordinator unreachable: ... context deadline exceeded
       (Client.Timeout exceeded while awaiting headers)
```

which blames the Coordinator for being slow to answer a question that is genuinely slow to answer. It now reads:

```
Error: the request timed out after 10m0s: http://localhost:8080 did not answer in time: ...

The Coordinator accepted the connection but had not answered yet. If the
work is genuinely slow — a check across many registries, or a scan of a
large one — raise the deadline:

  transferctl --timeout 15m ...
  SWGW_TIMEOUT=15m transferctl ...
```

Set `--timeout` or `SWGW_TIMEOUT` yourself and your value is used everywhere, including on the slow commands. An explicit deadline is a decision, not a suggestion — there are good reasons to want a short one, such as a scripted probe.

**The scan does not stop when the client does.** `packages discover` triggers work on the Coordinator; giving up on the response only stops you waiting for it. Re-running the command finds the in-progress scan rather than starting a second one.

### `health` vs `products check` — two different questions

They look similar and are deliberately not the same thing.

| | `transferctl health` | `transferctl products check` |
|---|---|---|
| Asks | Is the **service** working? | Is my **configuration** usable? |
| Checks | Coordinator, database, config parsed | DNS, TLS, credentials, per-repository permissions |
| Talks to third parties | **no** | yes — that is the point |
| Speed | milliseconds | seconds, sometimes longer |
| Run it | constantly; it backs readiness | after editing config, onboarding a vendor, or when discovery finds nothing |

**Why `health` deliberately does not probe registries.** The same machinery backs Kubernetes readiness. If a vendor's registry going down made health fail, that vendor's bad afternoon would pull *your* pods out of the Service — an outage you did not cause and cannot fix. It would also destroy health's usefulness during an incident: seeing `DEGRADED` would not tell you whether the fault was yours or someone else's.

So they stay separate. `health` answers for things you own; `products check` answers for things you depend on.

```
$ transferctl products check vendor-a

[FAIL]  vendor-a

  [FAIL]  registry.vendor-a.example.com/platform/suite (source)
      declared as "primary"
      OK       credentials
               resolved from secret vendor-a-registry as user svc-replication
      OK       reachable  (34ms)
      FAILED   authenticated
               HTTP 401: UNAUTHORIZED: authentication required
               → the username or password in secret vendor-a-registry was rejected.
                 Check for a trailing newline in the file — it is the most common
                 cause and the hardest to see
      SKIPPED  can list tags
               authentication failed
```

Checks run in dependency order and stop at the first real cause: there is no point asking whether a credential grants read access when the host does not resolve, and four failures for one cause is noise you have to work through. A **skipped** check is shown rather than hidden — "we did not check" and "it passed" must never look the same.

Exit code 6 when any repository fails, so it works in CI.

**What it does not check:** write access to a target. Confirming that requires an upload, and preflight will not leave artefacts in your registry. It is reported as `SKIPPED` with that reason, and the first transfer exercises it for real (M3).

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

**`packages discover` sometimes scans and sometimes returns instantly**
Fixed — pull and rebuild. A trigger arriving while a scan was already running used to return the *previous* result (or zeros, if no scan had finished yet) instead of joining the running one, so the same command took seconds or 0ms depending on timing. It now joins the running scan and returns its real numbers, with `collapsed: true` in the JSON and "Joined a scan already in progress" in the table output.

**`packages discover` reports `Repositories scanned 0` and "Nothing new"**
Those are two different things and the output now separates them. `Repositories scanned 0` means nothing was looked at, which is not a steady state. Either `discovery.repositoryFilters` rejected every candidate — the count is shown — or the source names no repositories and the registry's `/v2/_catalog` returned none. `transferctl products check` tells you which.

**`packages discover` blocks for minutes with no output**
Fixed. It now prints a live progress line to stderr — phase, which repository, tag counts, elapsed — polled from `GET /api/v1/products/{product}/discovery`. Stdout still carries only the result, so `-o json | jq` is unaffected. Two more ways to avoid staring at it:

```bash
transferctl packages discover <product> --wait=false   # start it, return now
transferctl packages discovery-status <product> --watch # follow it
```

Stopping the client never stops the scan — it runs on the Coordinator.

**`tag scan failed … net/http: timeout awaiting response headers`**
Listing tags worked and fetching a manifest did not. Discovery does one HEAD and one GET per tag, so this fails every scan. Raise the per-repository deadline:

```yaml
spec:
  network:
    timeouts:
      responseHeader: 2m
```

If the traffic goes through an inspecting proxy, that is the usual cause: a proxy that scans response bodies answers `HEAD` promptly and stalls on `GET`. `transferctl products check` now probes a real manifest fetch, so it tells you this before discovery does.

**Discovery is very slow — one repository, one tag, minutes**
Fixed. A scan used to be strictly sequential. It now runs repositories and tags in parallel, bounded:

```yaml
spec:
  sources:
    - name: vendor
      discovery:
        concurrency:
          repositories: 4        # default 4
          tags: 8                # default 8; they share the source's client
          artifacts: 8           # default 8; siblings of one index
```

Both clamp at 64. Raise them for a registry you own; leave them alone for a vendor's.

There was also a retry amplification: a manifest `GET` that blew the 30s deadline was retried up to eight times, so **one unresponsive request cost up to four minutes** — and discovery makes two per tag. A 90-second total budget now bounds it.

There was also a third cause, and it was the worst of the three: **every repository built its own connection pool and its own rate limiter**. So `maxConnections: 32` with 16 repositories in flight permitted 512 concurrent connections to one host, and `requestsPerSecond: 50` permitted 800. Through a corporate proxy that is not a faster scan — it is a self-inflicted overload, and the configuration said the opposite of what was happening. A source now has one pool, one limiter and one token cache, so those numbers mean what they say.

If it is still slow after that, the requests themselves are slow: turn on request tracing (above), and `transferctl products check` times a real manifest fetch.

**Requests are succeeding but the tag counter does not move**
A tag is only counted when its ENTIRE artifact tree has been fetched. A bundle whose index references sixty artifacts used to cost sixty sequential manifest fetches inside one tag — minutes at a time, with every request returning 200 and the counter frozen. Those siblings are now fetched in parallel (`discovery.concurrency.artifacts`), and the progress line reports `N artifacts`, which is the counter that keeps moving when nothing else does.

**I have no way to see what discovery is doing**
Turn on request tracing. Every registry request is logged with its host, path, status and duration:

```yaml
observability:
  log:
    level: debug
    format: text
```

You do not need `debug` for the important half: **failed requests and slow ones (>10s) are logged at WARN regardless of level**, with the URL and how long they took. If a scan is crawling, that log says which requests are responsible.

Alongside it: `transferctl packages discovery-status <product> --watch`, and `transferctl products check <product>`, which times a real `HEAD` and a real manifest `GET`.

**`packages list` is empty**
Discovery polls on its interval; the first scan happens at startup. Force one with `transferctl packages discover <product>`. If it reports `tagsListed=0`, the repository path or credentials are wrong — `transferctl health` checks reachability.

**Discovery is slow to report a registry outage**
Expected, and worth understanding: a hard outage takes **~2 minutes** to surface. The transport retries the transient class eight times with backoff, and only when those are exhausted does the loop's own backoff engage. Both layers are right individually and they multiply. This is why the alert is on `discovery_last_success_timestamp_seconds` staleness rather than failure rate — staleness is visible immediately.

**`tls: failed to parse certificate from server: x509: negative serial number`**
`insecureSkipVerify` does **not** fix this — measured, and the message is byte-for-byte identical with it on. The parse fails before verification runs. Set `tls.allowNegativeSerialNumbers: true` in system config, on the Coordinator *and* the Worker, and see [the section on it](#insecureskipverify-will-not-fix-x509-negative-serial-number).

**`x509: certificate signed by unknown authority`**
This one *is* a trust problem, and the right fix is `network.caBundleRef` pointing at the issuing CA's PEM. `insecureSkipVerify: true` also works and is worse: it stops checking expiry and hostname too. `transferctl products check` tells you which of the two you have.

**`context deadline exceeded (Client.Timeout exceeded while awaiting headers)`**
An older build. `products check` and `packages discover` now default to a 10-minute deadline, and a timeout no longer reports itself as an unreachable Coordinator. If you still hit it on a current build, the work really is taking longer than ten minutes: `transferctl --timeout 30m ...`. See [Timeouts](#timeouts).

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
