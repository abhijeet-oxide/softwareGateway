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
├── calibrate <product>         Measure the source->target path and suggest settings
│
├── packages
│   ├── list <product>          Discovered packages
│   ├── describe <product> <package>   Everything known about one package
│   └── inspect  <product> <package>   Pull its full contents and measure it
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
│   ├── retry <id> | --all      Requeue failed jobs and carry on (alias: resume)
│   ├── pause <id>
│   ├── resume <id>
│   ├── cancel <id>
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

### Why `inspect` is a verb and `describe` is a read

These were briefly one command: `describe --expand`, on the argument that a user does not want to inspect and then describe, they want to see the package and sometimes the size too. Two things were wrong with that.

**A flag hid the cost.** `describe` is a database read that answers instantly. `describe --expand` opened dozens of connections to a vendor's registry and could take minutes. Those are different operations, and a boolean is not enough warning — particularly on a command whose name promises a read.

**And it put the result in the wrong place.** Inspecting is not a rendering option. It *writes*: artifacts, their blobs, and a measured transfer size. Everything that reads the package afterwards sees them — `describe`, `list`, and a transfer alike — and a transfer that plans an already-inspected package skips the walk entirely, because it is literally the same walk (`internal/expand`).

So the split is by what the command does to the world, not by how much detail it shows:

- `packages inspect` is a verb. It contacts the vendor's registry, builds out what discovery deliberately left out, and records it. Idempotent — the tree under a digest cannot change, so a second run fetches nothing and says so.
- `packages describe` is a read. It shows everything known, *including* what inspect gathered: size, blob count, the artifact tree, and when it was measured. A package nobody has inspected says so and prints the command.

You never have to run `inspect`: a transfer performs the same walk if nobody has. It is for deciding whether you want one.

### Argument errors are part of the interface

Cobra's default for a missing argument is one line:

```
Error: accepts 2 arg(s), received 1
```

It names a count. It does not say what the two arguments *are*, which one is missing, what shape the missing one takes, or how to find a value for it — and it is produced by the command that already knows all four. Someone typing `transferctl packages inspect my-product` learns only that they need one more word, not that the missing word is a *tag*.

So a command declares its arguments once, and that declaration produces the usage line, the `--help` section, and the error:

```
Error: transferctl packages inspect needs 2 arguments, and got 1.

  transferctl packages inspect <product> <package>
                                         ^^^^^^^^^ missing

  <product>  the configured product to look in
             list them: transferctl products list
  <package>  a tag, a digest, or repository:tag — e.g. orb_23.8.1076, …
             list them: transferctl packages list <product>
```

Three properties, each of which was a real failure:

- **The three cannot drift.** `Use` said `<product> <package>` while the error said "accepts 2 arg(s)", and only one of those was in front of the user at the moment it mattered.
- **A usage error exits 2**, not 1. A script that cannot tell "I called this wrong" from "the thing does not exist" retries the one that will never succeed.
- **A mistyped subcommand fails.** `transferctl pkg inspct a b` printed the help text and exited **zero** — indistinguishable from success to anything automated. It now exits 2 and suggests `inspect`.

The same reasoning applies to `NOT_FOUND`. "package not found" reads as *the package is gone*, when the likelier cause is a reference that was never going to match — a product name typed where a tag belongs, or a version without the vendor's prefix. The message names the reference forms and the command that lists what exists.

### Shortened names, and where the rule comes from

Where a source declares a `vendor`, listings show that vendor's shortened spelling — `cfx-5000-k8s` rather than `orbs/cfx-5000-k8s`, `23.8.1076` rather than `orb_23.8.1076`. The full names are what is stored, transferred and returned by `-o json`.

Both spellings resolve as input, everywhere: `--repository`, `--tag`, and the `repository:tag` form a `describe` takes. An abbreviation you cannot type back is a trap — someone copies what is on their screen, gets "not found", and reasonably concludes the package is gone.

The tag was already shortened this way, by the source's vendor plugin at discovery. The REPOSITORY was not: the CLI dropped whichever prefix every row on the page happened to share. That needed no vendor knowledge, which was the appeal, and it was wrong twice — it shortened paths on registries with no such convention, and it made a row say different things depending on which other rows were in view. Both now come from a source stating its vendor, computed once, stored, and rendered verbatim.

## 3. Health

`health` reports the Coordinator, its dependencies **and the fleet**:

```
WORKER        STATE    LOAD   VERSION   LAST SEEN    DETAIL
INBLR1761     ACTIVE   1/32   1.4.0     4s ago       –
INBLR1762     ACTIVE   0/16   1.3.9     9s ago       idle
old-pod-7c9   STALE    0/16   1.3.0     31m00s ago   not heartbeating; its jobs are being returned to the queue
```

**LOAD is jobs in flight over the worker's configured ceiling** — `worker.maxConcurrentJobs`, reconstructed from what the worker asks for plus what it already holds, so a mismatch between this column and the config file means a worker running configuration nobody thinks it has.

It is also the column that answers "why is only one job running". A fleet sitting at 1/32 is not a stuck worker; it is a queue with nothing leasable in it, and `transfers describe` says why — outstanding jobs blocked behind a wave cannot be leased until the wave beneath them drains ([04](04-queue-and-scheduling.md) §3).

Every field here already crossed the wire on every lease and every heartbeat and was read for one decision and dropped. The `workers` table was created in the first migration and nothing wrote to it until this landed, which is why the fleet had no cardinality and a stale worker was invisible.


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
$ transferctl transfers jobs 9c1e8f2a --watch

STATE     KIND      SOURCE                                    TARGET                                        DIGEST            SIZE       MOVED      ATTEMPTS  WAVE  DETAIL
leased    blob      orbs/cfx-5000-k8s (mcc:25.7.2503) *       stage/orbs/cfx-5000-k8s                       sha256:4a5b8c9d…  256.0 MiB  200.1 MiB  1/8       0     held by worker-x2k4
leased    blob      orbs/cfx-5000-k8s (mcc:25.7.2503) *       stage/cfx-5000-product/mcc:25.7.2503          sha256:4a5b8c9d…  256.0 MiB   12.0 MiB  1/8       0     held by worker-p9m2
pending   blob      orbs/cfx-5000-k8s (mcc:25.7.2503)         stage/orbs/cfx-5000-k8s                       sha256:3c9e1f7d…  134.2 MiB        0 B  3/8       0     -
blocked   manifest  orbs/cfx-5000-k8s:orb_23.8.1076           stage/orbs/cfx-5000-k8s:orb_23.8.1076         sha256:9b3e2d1c…    2.1 KiB        0 B  -         2     -

$ transferctl transfers jobs 9c1e8f2a --state failed -o json | jq '.jobs[].digest'
```

Ordered by what is HAPPENING — leased, then runnable, then failed, blocked and
the outcomes — and largest first within each. A transfer has thousands of jobs
and a listing shows a page of them, so the order decides what an operator ever
sees; ordering by wave buried the handful actually moving under whatever was
planned first.

SOURCE and TARGET are the full paths, per row, and **a tag appears only where
one is really there.** Three things follow from the two-site placement (§[05](05-transfer-engine.md),
and `internal/transfer/layout.go`), and each is visible above:

- One digest appears twice with two targets. That is not a duplicate: a
  component is published inside its bundle, so the index referencing it stays
  resolvable, AND under the name it advertises, so it can be pulled as itself.
- The bundle-internal copy is UNTAGGED — the component's name is not that
  repository's to claim — so its TARGET carries no tag. Borrowing the name's tag
  to fill the column would advertise a reference the transfer never creates.
- Everything is read from one repository, because an index may only reference
  children co-located with it. So SOURCE is `repository:tag` only when the name
  belongs to that repository; otherwise the vendor's name goes in parentheses,
  saying what the content IS rather than where it sits.

A `*` marks content several artifacts share, where the one named is an example
rather than the whole truth.

`--watch` re-reads every two seconds (`--interval` to change it) and stops when
nothing is left running or runnable. `transfers list` and `transfers describe`
take the same pair; on `describe` it is what makes current and peak throughput
available at all, since a rate needs two readings and the server keeps no time
series.

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
Requeued 2 failed job(s). Transfer 9c1e8f2a is ready.

$ transferctl transfers retry --all
ID         REQUEUED   STATE   DETAIL
281614ab   691        ready   –
9c1e8f2a   2          ready   –

Requeued 693 job(s) across 2 transfer(s).
```

### Retry is the outage command, and it resumes rather than restarts

`--all` exists because an outage does not fail one transfer — it fails every transfer that was running, and making somebody copy IDs out of a listing to express "carry on with all of it" is busywork whose only effect is that some get missed.

Requeueing resets the attempt budget and drops the backoff: the operator running the command *is* the signal that the cause is gone. What it does **not** reset is progress. Jobs that succeeded stay succeeded, and a blob that was partway through keeps its byte count, so a transfer that was 80% done before the network went picks up at 80%. Blobs that landed are found by the placement check or a `HEAD` and cost nothing the second time ([05](05-transfer-engine.md) §4.1).

It will not restart a `cancelled` transfer — that was somebody's decision — or a `succeeded` one. A transfer that failed during *planning* has no jobs to requeue, and says so rather than reporting a successful retry of zero.

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
| `calibrate` | derived from `--budget` and `--concurrency` |

`calibrate` is the exception to the exception: a fixed ten minutes is wrong in both directions, since `--budget 30s --concurrency 1,2,4,8,16,32` is a legitimate request that would blow through it, and the default sweep finishes in about a minute. It computes `2 × budget × (levels + 2) × sides + 1m` instead, which tracks whatever was asked for.

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

## 11. Calibration

```
transferctl calibrate <product> [--from source] [--to target]
                                [--source-repository path] [-y]
                                [--concurrency 1,2,4,8,16] [--budget 5s]
                                [--no-write] [--bundle-size 30GiB]
```

It asks before it starts:

```
Calibrate cfx-5000-product

  source        cfx-near  cfx-5000-product-orb-docker.swdp-us.support.nokia.com
    repository  cfx-5000-product/admin
                holds the largest discovered package (63.7 GiB); 41 other(s) not measured
  target        cfx-jfrog-lab  artifact.it.att.com
    writes to   apm0014228-oci-stage/…
  sweep         1, 2, 4, 8, 16 streams, 5s each
  write probe   on: real bytes, in an upload session that is cancelled, never committed
  estimated     1m10s

This moves real data against both registries. Continue? [y/N]:
```

Everything else in this CLI reports what the system did. This one runs an experiment and tells you what to change.

### The problem it exists for

At least six settings decide throughput, they interact, and none of them is self-evidently right: `concurrency.perRegistry`, `concurrency.requestsPerSecond`, `worker.maxConcurrentJobs`, the number of workers, `network.proxy`, and where the workers run. A configuration file states them without justifying them, so tuning is guesswork — and the guesses fail in a consistent direction. Raising concurrency against a link that is already full adds load and no bytes. Adding a worker while a proxy halves the line rate buys a second slow worker. Both look like effort and neither moves the number.

Calibration measures the path and reports the knee of the curve, so the recommendation is arithmetic rather than folklore. Output is a table per side and a list of suggestions, each carrying the measurement behind it:

```
SOURCE  near  near.registry.example.net/orbs/cfx-5000-k8s
  route              environment http://proxy.corp:8080 (nothing is configured for this registry)
  direct route       reachable: 18.0 MiB/s direct against 2.1 MiB/s through the proxy
  round trip         42ms

  STREAMS   RATE         PER STREAM   MOVED      REQUESTS   ERRORS            TTFB
  1         5.0 MiB/s    5.0 MiB/s    10.0 MiB   12         -                 180ms
  2         10.0 MiB/s   5.0 MiB/s    20.0 MiB   24         -                 190ms
> 4         18.0 MiB/s   4.5 MiB/s    36.0 MiB   44         -                 210ms
  8         19.0 MiB/s   2.4 MiB/s    38.0 MiB   48         2 (2 throttled)   460ms

Suggestions

  !! network.proxy.direct  (source near)  unset (traffic goes through the proxy) -> true
      direct moved 18.0 MiB/s against 2.1 MiB/s through the proxy — 757% faster.
```

`>` marks the knee — the level worth configuring. `!!` is a measured problem, `!` a measured improvement, unmarked a fact with no knob behind it.

### Which repository, and why it is shown

A product spans forty repositories and a calibration measures **one**. The first version picked the first one declared, measured `cfx-5000-product/aaa` — one tag, nothing over 256 KiB — and reported the whole source as unmeasurable while the repositories holding sixty gigabytes sat next to it in the same list.

The choice is now made from evidence the Coordinator already holds: the repository containing the largest **discovered** package, which is the one whose blobs a transfer will spend its time on. Where nothing has been discovered yet it says so rather than presenting an arbitrary pick as a considered one, and the Coordinator walks the candidates at run time until one yields a blob worth timing.

Either way the choice is **shown, with its reason, before anything runs**. A judgement made silently inside a five-minute network test is one nobody can check.

The same applies to anything mandatory that was not supplied. A product with two sources and no `--from` used to be an error; on a terminal it is now a question, with the options listed. Off a terminal it stays an error naming the flag — there is nobody to answer, and guessing is worse than stopping.

### It leaves nothing behind

The target probe pushes real bytes into an upload session and then **cancels** it, so nothing is committed and no blob, tag or manifest appears in the destination ([05](05-transfer-engine.md) §8.1). It writes to `base + source path` — what the planner computes for a real job — and not to the target's configured repository, which is a *prefix*: an upload session opened against a bare prefix returns `404 Not Found` from a registry that is working perfectly. `--no-write` skips it anyway for a target governed by a change process that this would technically violate; the cost is that only the read half is measured, and the write half is often the slower one.

### What it will not tell you

**Which host your workers are on.** The probes run in the Coordinator, because `transferctl` never contacts a registry itself. Every report names the measuring host for that reason: if the workers sit on a different network, the numbers describe a path no transfer takes.

**A number to trust forever.** It measures a shared link on a particular afternoon. Re-run it when the answer starts to matter.
