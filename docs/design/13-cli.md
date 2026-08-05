# 13 — CLI (`transferctl`)

> **Prerequisite:** [09 — API](09-api.md)

**Invariant: `transferctl` is a pure Coordinator API client.** It never contacts a registry, never opens a database connection, never talks to a worker. Every command maps to routes in [09](09-api.md). This is what keeps one audit chokepoint and makes the binary safe to distribute — it can do nothing a user could not do with `curl`.

---

## 1. Conventions

`transferctl <noun> <verb> [flags]` — kubectl grammar, because the audience already knows it.

| Convention | Detail |
|---|---|
| Output | `-o table` (default), `json`, `yaml`, `wide`, `name` |
| Selection | `--product`, `--tag`, or a resource ID |
| Long ops | Return immediately with an ID; `--watch` follows |
| Confirmation | Destructive verbs prompt; `--yes` skips |
| Config | `~/.config/softwaregateway/config.yaml`; `--endpoint` overrides |
| Precedence | flag → `SWGW_*` env → config file → default |
| Colour | Auto-detected; disabled when not a TTY or `NO_COLOR` is set |

**Exit codes** — distinct enough to be scripted against:

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | General error |
| 2 | Usage error |
| 3 | No answer: the Coordinator is unreachable, **or** it did not reply within the timeout |
| 4 | Not found |
| 5 | Precondition failed (illegal state transition) |
| 6 | Operation completed with failures (e.g. `--watch` on a transfer that failed) |

Code 6 matters for CI: `transferctl download --watch` must exit non-zero when the transfer fails, or a pipeline will report green on a failed replication.

Code 3 deliberately covers both no-answer cases. A script branching on it wants the same thing either way — retry or fail the pipeline — and splitting them would break the ones already written. The distinction is carried in the **message**, because that is what a human acts on, and the two lead in opposite directions: "the service is down" means go and look at the Coordinator, "it has not answered yet" means give it longer.

## 2. Command tree

```
transferctl
├── config
│   └── validate <dir>          Validate product YAML -- runs in CI, pre-merge
│
├── products
│   ├── list
│   ├── describe <product>
│   └── check [product]         Probe registries and credentials for real
│
├── discover [product]          Scan now, rather than waiting for the interval
│   └── status [product]        What discovery is doing right now
│
├── packages
│   ├── list <product>          Discovered packages
│   └── describe <product> <package> [--expand]
│
├── download <tag>              Replicate source -> target(s)
├── promote  <tag>              Promote target -> target(s)
├── verify   <tag>              On-demand signature verification
│
├── transfers
│   ├── list
│   ├── describe <id>           Progress, failed jobs, timeline
│   ├── jobs <id>               Layer-level progress
│   ├── logs <id>               Worker logs for this transfer
│   ├── pause <id>
│   ├── resume <id>
│   ├── cancel <id>
│   ├── retry <id>
│   └── priority <id> <value>
│
├── schedules
│   ├── list
│   └── cancel <id>
│
├── workers
│   ├── list
│   └── logs <worker>
│
├── audit
│   └── list                    Query the audit trail
│
├── health                      Check the Coordinator and every dependency
└── version                     Client and server versions
```

### Why discovery is a top-level verb

It was `packages discover`, and that was the wrong shape twice over.

**Discovery is not an operation on packages; it is what produces them.** Burying the primary verb of the system two levels down, next to the commands that read its output, is how a CLI ends up needing a cheat sheet. `discover` sits at the top with `download`, `promote` and `verify` — the things the tool *does* — while `packages` and `products` are the things you *look at*.

**And it could not express the most common operator action: scan everything.** `transferctl discover` with no argument scans every product being polled, which is what you want after a maintenance window or when you want to know what your vendors have shipped since you last looked. A shell loop over `products list` put the definition of "everything" in the caller, where it could disagree with which products were actually enabled. A fleet-wide scan never blocks; follow it with `discover status`.

`packages discovery-status` moved to `discover status` for the same reason — status belongs with the thing it reports on — and with no argument it reports every product, one block each, which is the shape you want when you are looking for the one that is stuck.

### Why `inspect` folded into `describe --expand`

`packages inspect` and `packages describe` were two commands that answered the same question at different depths, and the split leaked an implementation detail: that discovery records a package's manifest without fetching what it lists. A user does not want to inspect and then describe; they want to see the package, and sometimes they want the size too.

So `describe` gained `--expand`, which walks the tree first and then renders — the output is the expanded truth rather than a stale row plus a separate report. Without the flag, `SIZE` and `BLOBS` read `n/a` and the output says why.

Both old spellings still work, hidden, because they are in scripts and in muscle memory. Breaking those to tidy a help screen is a bad trade.

## 3. Health

```
$ transferctl health

Coordinator            http://coordinator.softwaregateway.svc:8080
  status               HEALTHY
  version              1.4.2 (a1b2c3d)
  uptime               6d 4h 12m
  leader               coordinator-7d9f4b6c8-x2k4

Database               PostgreSQL 16.3
  status               HEALTHY          latency 1.2ms
  migrations           0041 (current)

Workers                12 active, 0 draining
  concurrency          142 / 192 granted

Products               3 loaded, 0 errors

Repositories
  vendor-a-platform
    primary            HEALTHY   142ms   registry.vendor-a.example.com
    mirror             HEALTHY   201ms   registry-eu.vendor-a.example.com
    lab                HEALTHY    18ms   internal.azurecr.io
    production         HEALTHY    19ms   internal.azurecr.io
  vendor-b-database
    primary            DEGRADED  4.2s    registry.vendor-b.example.com
                       └─ 3 of last 20 requests returned 503
    lab                HEALTHY    17ms   internal.azurecr.io

Notifications
  smtp.internal.example.com:587          HEALTHY
  teams: platform-channel                HEALTHY

Overall: DEGRADED (1 of 7 repositories degraded)
```

Backed by `GET /api/v1/system:healthCheck` ([09](09-api.md) §9.1) — **the deep check, not a probe.** It validates connectivity to every configured dependency as required, which is exactly why it must not be what Kubernetes polls: a slow, thorough check is the right thing for a human and the wrong thing for a liveness probe.

Exit code 0 healthy, 1 degraded, 3 unreachable.

## 4. Discovery and packages

```
$ transferctl discover vendor-a-platform
Triggered discovery for vendor-a-platform (2 source repositories)
  primary     scanning...  found 3 new packages
  mirror      skipped (discovery disabled)

New packages:
  v2.14.0   sha256:9f86d081…   45.2 GiB   847 blobs
  v2.13.4   sha256:2c26b46b…   44.8 GiB   839 blobs
  v2.13.3   sha256:fcde2b2e…   44.7 GiB   836 blobs

Auto-download rules matched 3 packages -> lab (priority 100)
```

```
$ transferctl packages list --product vendor-a-platform --limit 5

TAG        DISCOVERED        SIZE      BLOBS  STATE        LAB        PRODUCTION
v2.14.0    2026-08-04 09:12  45.2 GiB    847  VERIFIED     ✓ verified  –
v2.13.4    2026-07-28 09:14  44.8 GiB    839  VERIFIED     ✓ verified  ✓ verified
v2.13.3    2026-07-21 09:11  44.7 GiB    836  SUPERSEDED   ✓ verified  ✓ verified
v2.13.2    2026-07-14 09:13  44.6 GiB    833  VERIFIED     ✓ verified  ✓ verified
v2.13.1    2026-07-07 09:12  44.5 GiB    831  FAILED       ✗ failed    –

$ transferctl packages describe v2.14.0 --product vendor-a-platform

Package      vendor-a-platform / v2.14.0
Digest       sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08
Media type   application/vnd.oci.image.index.v1+json
Discovered   2026-08-04 09:12:44 UTC   (source: primary)
Size         45.2 GiB across 847 blobs, 5 artifacts

Artifacts
  sha256:9f86d081…  index                 5 manifests
  ├── sha256:6b86b273…  image  linux/amd64      18.4 GiB   312 layers
  ├── sha256:d4735e3a…  image  linux/arm64      17.9 GiB   298 layers
  ├── sha256:4e074085…  image  linux/amd64       8.1 GiB   231 layers
  └── sha256:4b227777…  helm chart                2.1 MiB     3 layers

Targets
  lab          VERIFIED    2026-08-04 10:16   transfer 9c1e8f2a
  production   –           not transferred

Signatures   1 cosign signature, keyless
             identity  https://github.com/vendor-a/platform/.github/workflows/release.yaml@refs/heads/main
             issuer    https://token.actions.githubusercontent.com
```

## 5. Download, promote, verify

```
$ transferctl download v2.14.0 --product vendor-a-platform --target lab --priority 100

Transfer request 7c9e6679-7425-40de-944b-e07fc1f90ae7 created
  lab   transfer 9c1e8f2a-…   PLANNING

Follow with:  transferctl transfers describe 9c1e8f2a --watch
```

**Dry run** — `--dry-run` maps to `validateOnly=true` ([09](09-api.md) §4.2), so it exercises the real planner ([05](05-transfer-engine.md) §7):

```
$ transferctl download v2.14.0 --product vendor-a-platform --target lab --dry-run

Transfer plan — vendor-a-platform / v2.14.0 → lab

  Artifacts               5   (1 index, 3 images, 1 helm chart)
  Blobs                 847   total 45.2 GiB
    already present     291         12.1 GiB   placement hit
    mountable             0          0 B       different registry
    to transfer         556         33.1 GiB
  Manifests to push       5
  Waves                   3   (blobs → manifests → index)

  Estimated duration   ~11m20s   at 48.6 MiB/s (EWMA, 14 recent transfers this route)
  Bandwidth saved       12.1 GiB (27%)

  Planned operations (first 5 of 561):
    STREAM  sha256:4a5b8c9d…   256.0 MiB   wave 0
    STREAM  sha256:7e2f1a3b…   198.4 MiB   wave 0
    SKIP    sha256:1c4d9e0f…    89.2 MiB   wave 0   already present
    STREAM  sha256:9b3e2d1c…    64.0 MiB   wave 0
    PUSH    sha256:6b86b273…     2.1 KiB   wave 1   manifest

  No data transferred (dry run).
```

**Multiple targets, multiple packages:**

```
$ transferctl download v2.14.0 --product vendor-a-platform --target lab --target staging
$ transferctl download --product vendor-a-platform --tag-match '^v2\.14\.' --target lab
```

**Scheduling** — persisted as a scheduled request, expanded into jobs only when due ([04](04-queue-and-scheduling.md) §10):

```
$ transferctl download v2.14.0 --product vendor-a-platform --target lab \
      --at 2026-08-11T02:00:00Z

Scheduled request 3d8f1a92-… for 2026-08-11 02:00:00 UTC (in 6d 15h)
  No queue entries created until the scheduled time.
```

**Promote** — same engine, target → target ([05](05-transfer-engine.md) §6):

```
$ transferctl promote v2.14.0 --product vendor-a-platform --from lab --to production

Promotion request b2c4e6f8-… created
  production   transfer 5a7b9c1d-…   PLANNING

Note: lab and production share registry internal.azurecr.io
      → cross-repository mount applies; most blobs will not cross the network.
```

**Verify:**

```
$ transferctl verify v2.14.0 --product vendor-a-platform --at destination --target lab

Verifying vendor-a-platform / v2.14.0 at lab (internal.azurecr.io/vendor-a/platform)

  sha256:9f86d081…  index         ✓  1 signature
  sha256:6b86b273…  linux/amd64   ✓  1 signature
  sha256:d4735e3a…  linux/arm64   ✓  1 signature
  sha256:4e074085…  linux/amd64   ✓  1 signature
  sha256:4b227777…  helm chart    ✓  1 signature

  Policy      keyless
  Identity    https://github.com/vendor-a/platform/.github/workflows/release.yaml@refs/heads/main
  Issuer      https://token.actions.githubusercontent.com

VERIFIED — 5 of 5 artifacts (1.8s)
```

## 6. Progress

```
$ transferctl transfers describe 9c1e8f2a --watch

Transfer  9c1e8f2a-…                                          RUNNING
Package   vendor-a-platform / v2.14.0  →  lab
Priority  100          Wave 1 of 3          Elapsed 6m27s

  [██████████████████████████░░░░░░░░░░░░░░░░░░░░░░░]  51.4%
  17.0 GiB / 33.1 GiB          ETA 5m21s  (2026-08-04 10:16:22 UTC)

  Speed     current  49.0 MiB/s     average  45.0 MiB/s     peak  65.0 MiB/s

  Jobs      561 total
            402 succeeded    12 skipped    14 in flight
            131 pending       2 failed      0 cancelled

  Dedupe    12.1 GiB skipped before transfer (26.8% saved)

  Failed jobs (2):
    sha256:3c9e1f7d…   134.2 MiB   attempt 3/8   ErrUnavailable  503 from target
    sha256:8a1b2c3d…    45.8 MiB   attempt 2/8   ErrTimeout      idle stall

  Workers   worker-x2k4 (4)  worker-p9m2 (3)  worker-t4v7 (4)  worker-z8n1 (3)
```

**Layer-level progress** — the required per-layer view:

```
$ transferctl transfers jobs 9c1e8f2a --state running

DIGEST            SIZE       PROGRESS              SPEED      WORKER        ATTEMPT
sha256:4a5b8c9d…  256.0 MiB  [████████░░]  78.2%   12.1 MiB/s worker-x2k4   1
sha256:7e2f1a3b…  198.4 MiB  [████░░░░░░]  41.0%   10.8 MiB/s worker-p9m2   1
sha256:9b3e2d1c…   64.0 MiB  [█████████░]  92.5%    9.2 MiB/s worker-t4v7   1
sha256:3c9e1f7d…  134.2 MiB  [██░░░░░░░░]  18.1%    4.1 MiB/s worker-z8n1   3

$ transferctl transfers jobs 9c1e8f2a --state failed -o json | jq '.jobs[].digest'
```

## 7. Queue control

```
$ transferctl transfers pause 9c1e8f2a
Transfer 9c1e8f2a paused.
  14 in-flight jobs will complete; no new jobs will be leased.

$ transferctl transfers priority 9c1e8f2a 900
Priority 100 → 900 for 131 pending jobs.
  In-flight jobs are unaffected.

$ transferctl transfers cancel 9c1e8f2a
Cancel transfer 9c1e8f2a (vendor-a-platform/v2.14.0 → lab, 51.4% complete)? [y/N] y

Transfer 9c1e8f2a cancelling.
  131 pending jobs cancelled; 14 in-flight jobs will abort within ~20s.
  Blobs already transferred remain at the destination and will be
  reused by future transfers. No tag was applied.

$ transferctl transfers retry 9c1e8f2a
Requeued 2 failed jobs. Transfer 9c1e8f2a → READY.
```

Each message states the semantics that are easy to get wrong ([04](04-queue-and-scheduling.md) §8): pause does not kill in-flight work, priority does not preempt, cancel does not roll back. The CLI is where those semantics actually reach a user, so it says them rather than assuming the documentation was read.

## 8. Worker logs

```
$ transferctl transfers logs 9c1e8f2a --follow --level info

10:07:33.104  worker-x2k4  INFO   blob completed   sha256:4a5b8c9d…  256.0 MiB  5.2s  48.8 MiB/s
10:07:34.882  worker-p9m2  INFO   blob skipped     sha256:1c4d9e0f…  placement_hit
10:07:36.117  worker-z8n1  WARN   retrying         sha256:3c9e1f7d…  attempt 3/8  503 from target
10:07:41.203  worker-t4v7  INFO   blob completed   sha256:9b3e2d1c…   64.0 MiB  2.1s  30.5 MiB/s
10:07:44.556  coordinator  INFO   wave advanced    0 → 1  (556 blobs complete)
```

Served from `worker_logs` via the Coordinator ([03](03-persistence.md) §7). **The CLI never connects to a worker.** This is a convenience tail for the common debugging question, not a log store — cluster log aggregation remains the system of record, and `transferctl` says so in `--help`.

## 9. Config validation in CI

```
$ transferctl config validate ./deploy/products/

  vendor-a-platform.yaml     OK   2 sources, 2 targets, 2 rules
  vendor-b-database.yaml     OK   1 source, 1 target, 1 rule
  vendor-c-analytics.yaml    ERROR

    spec.autoDownload.rules[0].tagPattern: invalid regexp
      '^v(\d+\.\d+' — missing closing )
    spec.targets[1].name: 'prod' referenced by rules[0].targets but not declared
    spec.verification.cosign.keyless: certificateIdentity is required in
      keyless mode — without it, any valid Sigstore signature would be accepted

3 files, 2 valid, 1 error
```

Runs the **same validator the Coordinator runs at load** ([02](02-configuration.md) §7), offline, with no cluster. This is the compensation for not having CRD admission validation ([02](02-configuration.md) §2): catch it in the pull request instead of at reconcile time.

The third error is the one worth having. A keyless policy without an identity constraint is syntactically fine and semantically useless — it verifies that *someone* signed the artifact, not that the vendor did. Rejecting it at validation time prevents a configuration that looks secure and is not.

## 10. Client configuration

```yaml
# ~/.config/softwaregateway/config.yaml
endpoint: http://coordinator.softwaregateway.svc:8080
defaultProduct: vendor-a-platform
output: table
timeout: 30s
```

### Two timeout defaults

`--timeout` (or `SWGW_TIMEOUT`) bounds one request. It defaults to 30 seconds, except for the two commands that reach third-party registries through the Coordinator, which default to **10 minutes**:

| Command | Default |
|---|---|
| everything else | 30s |
| `products check` | 10m |
| `discover` | 10m |

Those two are slow because the work is slow. `products check` opens a TLS connection to every repository a product declares and runs several round trips against each; `discover` lists every tag of every repository and resolves each one. Through a corporate proxy, across a WAN link, minutes is the normal case — so a single 30-second default made them fail almost every time, and report it as `coordinator unreachable`, which sent operators to investigate a service that was working.

The raise applies **only when the operator has not chosen a timeout**, by flag or by environment. An explicit `--timeout 5s` means five seconds even on a slow command; silently overriding it would make the flag a suggestion, and a short deadline is a legitimate choice for a scripted probe.

Giving up on the response does not stop the work. `discover` triggers a scan on the Coordinator; a client timeout only stops us waiting for the result, and re-running the command joins the in-progress scan rather than starting a second one.

### Progress, not a blank terminal

A blocking `discover` now polls `GET .../discovery` on a second connection and renders a live line to **stderr** — phase, current repository, tag counters, elapsed. Stderr, not stdout, so `-o json | jq` is unaffected; and a carriage-return redraw only when stderr is a terminal, because a redirected log full of `\r` is worse than no live display.

Two ways to not wait at all:

```
transferctl discover <product> --wait=false
transferctl discover status <product> [--watch]
```

`discover status` is also the answer to "is it stuck or just slow?", which a blocking command cannot give you while it is blocked.

```bash
export SWGW_ENDPOINT=http://localhost:8080
transferctl --endpoint http://localhost:8080 health
```

**Auth-ready.** `--token` and `SWGW_TOKEN` are accepted and sent as `Authorization: Bearer` today; the Coordinator ignores them in v1 ([09](09-api.md) §10). When authentication is enabled, existing scripts that already set a token keep working, and `transferctl auth login` (device-code) is added for humans. The flag existing now is what makes that a non-breaking change.
