# 32 - Performance

> **Consumed by:** [03](03-persistence.md), [09](09-api.md)
> **Status:** the measurement is implemented (`task bench`, `task bench:api`,
> the `Performance` job in `.github/workflows/ci.yml`). The remedies in §5 are
> proposed, with the numbers that justify them.

---

## 1. Why this document exists

"The software feels laggy" is not a bug report anybody can act on, and the
reason it kept being the only thing available is that this repository had no
way to state what fast means. There were no throughput numbers, no answer to
how many readers it supports, and no way to tell a change that made a query
worse from a continuous-integration runner that was busy.

So this records two things: what was measured, and the two tools that keep
measuring it.

## 2. What was measured

A Coordinator on four vCPUs, authentication disabled, driven by k6. Two
estates: **empty**, and **seeded** with 200 transfers of 2,500 jobs - 500,000
job rows, which a real deployment passes in a few weeks.

### The per-request floor, empty estate

| Endpoint | req/s at 20 readers | p95 |
|---|---|---|
| `/healthz` | 29,700 | 4.8 ms |
| `/api/v1/whoami` | 24,700 | 2.4 ms |
| `/api/v1/discovery` | 24,000 | 2.4 ms |
| `/api/v1/products` | 20,500 | 2.9 ms |
| `/api/v1/workers` | 8,300 | 6.2 ms |
| `/api/v1/products/{p}/replication` | 3,700 | 9.9 ms |
| `/api/v1/packages` | 2,700 | 20.0 ms |
| **`/api/v1/transfers`** | **1,100** | **52.3 ms** |

The HTTP stack, the router and the authorization middleware are not the
constraint: they serve thirty thousand requests a second on this box. The
transfer listing costs **twenty-three times** what `/whoami` costs on a
database with nothing in it.

### The same listing, seeded estate

| Request | latency |
|---|---|
| `/transfers?pageSize=25` | **1.0 - 3.7 s** |
| `/transfers?pageSize=25&view=summary` | **5 ms** |
| `/transfers?pageSize=100` | 1.6 - 2.0 s |
| `/transfers?pageSize=100&view=summary` | 5 ms |

Under twenty readers the same page reaches a p95 of **5.9 s**, while
`/healthz` on the same box in the same run stays at **297 µs**. The machine is
not busy; the query is.

### Where the time goes

`EXPLAIN (ANALYZE, BUFFERS)` on one page of 25 rows:

```
Limit  (actual time=491.923..674.696 rows=25 loops=1)
  Buffers: shared hit=188806
```

**188,806 buffer hits - about 1.5 GB of buffer traffic - to return 25 rows.**

`Packages.transferSelect` (`internal/store/queue.go`) projects twelve
aggregates over `jobs`, each its own correlated subquery, evaluated per row of
the page. A page therefore reads the jobs of every transfer on it: 25 rows x
2,500 jobs x 12 passes. The cost follows what the estate has DONE, not what the
reader asked for.

Two of the twelve have no index to seek with - `SUM(j.bytes_transferred)` over
all of a transfer's jobs, and `count(*) WHERE j.repair_level > 0` - so those
two read every job row of every transfer on the page.

### What is NOT the cause

Three plausible explanations were tested and are wrong, and they are recorded
so nobody spends the afternoon again:

- **The SQLite single connection is not the ceiling.** `internal/store/sqlite.go`
  opens the pool with `SetMaxOpenConns(1)`, which looks like a throughput
  ceiling and is not: the same ladder against PostgreSQL with 25 connections
  produces the same plateau (~950-1,120 req/s) and the same latency curve. The
  constraint is CPU spent per request, not connections.
- **Neither is connection count generally.** Raising it moves nothing while a
  single request costs 188,806 buffer reads.
- **Neither is the replication poll.** `useReplicationForAll` fans out one
  request per product on the Downloads page, which is the "replication API is
  called constantly" people report - but it is already cached at five minutes
  (`web/src/api/queries.ts`) and it is a fifth of the cost of the listing
  beside it. It is worth fixing (§5.3); it is not why the page takes seconds.

## 3. The answers

**Throughput.** ~30,000 req/s on the cheap endpoints; ~1,100 req/s on the
transfer listing with an empty database; **under 20 req/s** on a seeded one.
The listing is the number that matters, because it is what the Downloads page
polls every five seconds while anything is running.

**Concurrent readers.** At the current cost per request, a seeded estate
supports roughly **one to two** readers of the Downloads page before p95 passes
a second. That is the honest answer and it is the finding, not a limit anybody
chose.

**Resources.** The Coordinator idles at ~55 MB RSS; the chart requests 200m CPU
and 256Mi and that is sound. Throughput here is not memory-bound and will not
improve by being given more - a request that reads 1.5 GB of buffers is doing
work that should not exist, and no allocation of CPU makes twelve correlated
subqueries into one.

**Parallel downloads.** A worker runs `maxConcurrentJobs: 16`
(`config/config.yaml`); per-registry concurrency is a product setting
(`concurrency.perRegistry`, 32 in the samples). Those are independent of the
read path above - the interface gets slow while downloads run because the
listing that draws them got more expensive, not because the transfers contend
with it.

## 4. How it stays measured

| | |
|---|---|
| `task bench` | Go benchmarks over the polled queries. Recorded in the job summary, not gated: ns/op on a shared runner is too noisy to fail a pull request on. |
| `task bench:api` | k6 against a Coordinator it starts itself. Gated, on thresholds an order of magnitude above where the code sits. |

Both run in CI **only when the backend changed** - the `Performance` job is
`if: needs.changes.outputs.go == 'true'`, the same condition the Go test and
lint jobs use, so a frontend or documentation pull request does not pay for it.

Neither tool is written here. k6 is a single static binary, fetched on first
use; the benchmarks are `go test -bench`, compared with `benchstat`. See
`test/load/README.md` for how to read a run, and for why the thresholds are
loose.

## 5. What to do about it

Proposed, in the order the numbers justify.

### 5.1 One pass over `jobs` instead of twelve

Replace the twelve correlated subqueries with a single grouped aggregate over
the page's transfers, using `FILTER` (PostgreSQL) / `CASE WHEN` (SQLite):

```
LEFT JOIN (SELECT transfer_id, count(*) FILTER (WHERE state = 'failed') ...
             FROM jobs WHERE transfer_id IN (<the page>) GROUP BY transfer_id)
```

Measured on the seeded estate: **62,627 buffers and 100 ms**, against 188,806
and 675 ms - the same 25 rows, the same numbers out.

A plain derived table rather than `LATERAL`, because `LATERAL` does not exist
in SQLite and this projection is shared by both dialects (`internal/store/dialect.go`).

This is mechanical and it is a third of the cost. It is not the whole answer:
it still reads every job of every transfer on the page, which is why it is
first and not last.

**What would change our mind:** if the rewrite cannot be made to produce
identical values for every one of the twelve columns across both dialects, it
does not ship - `scanTransfer` is one function precisely so list and get cannot
disagree, and a rollup that is subtly wrong is worse than one that is slow.

### 5.2 Keep the rollups on the transfer row

The real fix, and the larger one: maintain the counts on `transfers` as jobs
complete, so a listing reads what it displays and the page costs its page size.
This turns the listing from O(jobs on the page) into O(1) per row.

The cost is a write on every job completion and a reconciliation path for when
those drift - which is why it is proposed second, and why §5.1 is worth doing
on its own first.

**What would change our mind:** if the write amplification on the job-completion
path measurably slows the queue - `internal/store/throughput_test.go` is where
that would show - the counters belong in a separate table updated in batches
rather than on the transfer row.

### 5.3 Stop the Downloads page asking for what it does not draw

`web/src/pages/Downloads.tsx` mounts `useTransfers` **twice** (replications and
promotions), neither with `view=summary`, and polls both every five seconds
while anything is live. `Overview.tsx` already uses `view=summary` and is
consequently 200x cheaper.

The promotions table and the completed rows do not draw a progress bar and do
not need the rollups. Asking for the cheap plan where nothing is being drawn is
a frontend change with no server cost at all.

### 5.4 Give the Coordinator a profiler

There is no `net/http/pprof` endpoint in this repository, so the way to find
out where the time goes is to reproduce the estate locally and use
`EXPLAIN`. A guarded pprof listener - off by default, bound to the metrics port
- is what made §2 take an afternoon instead of a day.

## 6. What is already right

Recorded so it does not get "optimised" away:

- **The indexes are correct.** `transfers_recent_idx (created_at DESC, id DESC)`
  matches the listing's sort exactly, and migration 00030 is why the listing
  pages rather than scans. `EXPLAIN` confirms the planner uses it.
- **`view=summary` exists and works**, and `TestTransferListingQueryPlan`
  asserts the SHAPE of the plan rather than a duration - which is the right
  way to gate this and is why that test does not flake.
- **The estate-wide reads are already de-fanned.** `useAllPackages`,
  `useDiscoveryStatus` and `useTransferActivity` each replaced a per-product
  fan-out, and the comments in `web/src/api/queries.ts` record what each one
  cost. The remaining fan-out is `useReplicationForAll` (§5.3).
