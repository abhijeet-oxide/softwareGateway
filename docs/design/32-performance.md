# 32 - Performance

> **Consumed by:** [03](03-persistence.md), [09](09-api.md)
> **Status:** implemented. The measurement is `task bench`, `task bench:api`
> and the `Performance` job in `.github/workflows/ci.yml`; §5.1 and §5.3 are in
> `internal/store/queue.go` and `web/src/pages/Downloads.tsx`, §5.4 in
> `cmd/coordinator`, §5.5 in `internal/api/replication.go` and §5.2 in
> `internal/store/rollupcache.go`. All five are in.

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

**Concurrent readers.** Before §5.1 a seeded estate supported roughly **one to
two** readers of the Downloads page before p95 passed a second. After it,
twenty readers see a p95 of 0.238 s against the same estate, and a single reader
0.040 s. What remains is proportional to the jobs of the transfers on the page
that are still RUNNING - a settled page costs nothing beyond reading it.

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

### 5.1 One pass over `jobs` instead of twelve - DONE

The twelve correlated subqueries are now a single grouped aggregate over the
page's jobs. The page is chosen FIRST, in a leading CTE, so `jobs` is never
read for a row that is about to be discarded:

```
WITH page AS (SELECT id ... ORDER BY ... LIMIT ?),
     rollup AS (SELECT transfer_id, <twelve aggregates>
                  FROM jobs WHERE transfer_id IN (SELECT id FROM page)
                 GROUP BY transfer_id)
SELECT ... FROM transfers t JOIN page ... LEFT JOIN rollup ...
```

A derived table rather than `LATERAL`, and `SUM(CASE WHEN ...)` rather than
`FILTER`, because this projection is shared by both dialects and SQLite has
neither. The page leads because `Dialect.Rewrite` numbers placeholders in
textual order, which is what lets both shapes keep taking the same arguments.

Measured on the seeded estate, through the API:

| request | before | after |
|---|---|---|
| `/transfers?pageSize=25` | 1.0 - 3.7 s | **0.12 - 0.36 s** |
| `/transfers?pageSize=100` | 1.6 - 2.0 s | **0.25 - 0.27 s** |

Under k6 at twenty readers, p95 went from **5.9 s to 0.68 s**, and a single
reader from **1.01 s to 0.13 s** - the run that used to breach the thresholds
in `test/load/api.js` now passes them.

THE CHEAP PATH IS UNCHANGED, deliberately. An earlier version of this put every
listing through the page CTE, which cost `view=summary` 39% for a rollup it
does not read. `transferListQuery` now emits the flat query when no rollups are
wanted, and `benchstat` confirms that path is unmoved.

On SQLite the same change is worth about 16% rather than 8x: its correlated
subqueries were already seeking `jobs_transfer_state_idx` cheaply, and the
twelve passes cost far less on a database in the same process. SQLite is not
supported in production (`config/config.yaml`), so the Postgres number is the
one that matters - but it is why `task bench`, which runs on SQLite, reports a
modest gain for a change that is transformative in a deployment.

**What is asserted, and what would change our mind.** All twelve rollups are
pinned against a hand-built estate in
`internal/store/transferrollup_test.go`, on both dialects and through both
`ListTransfers` and `GetTransfer` - a rewrite like this fails quietly, and a
count that is wrong by the rows of one state is a progress bar that says 94%
forever. That test was checked to FAIL against a deliberately broken aggregate
before it was trusted. If a future rollup cannot be expressed in one pass
without changing a single one of those numbers, it does not ship.

### 5.2 Remember the rollups that cannot change - DONE

A transfer's rollup is the shape of its jobs. While it runs that changes
constantly; once it has SETTLED it cannot change again, because no job of a
succeeded, failed or cancelled transfer will ever be touched. So the listing
memoises the rollup of terminal transfers and computes everything else on every
read (`internal/store/rollupcache.go`).

A page whose rollups are all known asks nothing of `jobs` at all - it runs the
same flat plan `view=summary` uses, and the twelve columns come out of the memo.
That is what a Downloads *history* is once somebody has looked at it.

Measured on the seeded estate, through the API:

| request | before all of this | after §5.1 | after §5.2 |
|---|---|---|---|
| `/transfers?pageSize=25` | 1.0 - 3.7 s | 0.12 - 0.36 s | **0.039 - 0.050 s** |
| a page of settled rows | - | - | **0.0116 s** |
| k6, one reader, p95 | 1.01 s | 0.13 s | **0.040 s** |
| k6, twenty readers, p95 | 5.94 s | 0.68 s | **0.238 s** |

In the benchmark, a page of settled transfers is 0.77 ms against 11.1 ms
computed - near enough the `view=summary` floor of 0.65 ms, which is the least
this listing can cost.

**Why this rather than counters on the row.** Twenty-two statements in
`internal/store` mutate `jobs` and a dozen mutate a transfer's state.
Maintained counters would have to be correct in every one of them, and the
failure - a count wrong by the rows of one state - is a progress bar reading
94% forever rather than a crash. Live counters would also have sixteen
concurrent jobs of one transfer contending on that single `transfers` row,
which is the write amplification this document warned would slow the queue.

Here there is no write path to get wrong, because there is no write path: a
value exists only once the thing it describes has stopped moving.

**The two things that make it correct**, both checked by tests that were run
against a deliberately broken version first:

- **Nothing non-terminal is ever stored.** `rollupCache.put` refuses it rather
  than trusting its caller. A running transfer is computed on every read, so a
  progress bar cannot freeze.
- **An entry carries the `updated_at` it was computed at**, and is used only
  while the row still carries the same one. A retry REOPENS a failed transfer
  (`recovery.go`), it runs again and settles again with different numbers; every
  statement that changes a transfer's state sets `updated_at`, so both
  transitions invalidate the memo without any of them knowing this file exists.
  (`activetime.go` deliberately does not set it, and does not change a rollup
  either.)

The memo is per process and bounded at four thousand transfers. Two replicas
computing the same rollup of the same immutable rows get the same answer, so
nothing needs coordinating; a restart costs the first listing after it.

**What would change our mind:** if a state is ever added from which a transfer
can be settled *and* resumed without `updated_at` moving, the version check
stops working and `terminalTransferStates` is no longer sufficient on its own.
That is the assumption to re-check, and it is asserted in
`TestAReopenedTransferIsNotServedItsOldRollup`.

### 5.3 Stop the Downloads page asking for what it does not draw - DONE

`web/src/pages/Downloads.tsx` mounted `useTransfers` **twice** - replications
and promotions - neither with `view=summary`, and polled both every five
seconds while anything was live.

The promotions table draws the route, the method, the state, the time spent and
when: every one of them a column of `transfers` itself. It read the dozen
aggregates over `jobs`, rendered none of them and threw them away. It now asks
for `view=summary`, measured at **0.134 s to 0.005 s** on a seeded estate.

Every component in that table was checked for a rollup field first, because the
failure mode of getting this wrong is not slowness: without the rollups those
fields are zero, and a progress bar reading a zero looks like a stalled
promotion rather than a missing request. The downloads table keeps the full
projection, because it does draw progress.

### 5.4 Give the Coordinator a profiler - DONE

`observability.profiling`, off by default and on loopback when it is on. It is
a listener of ITS OWN and is never mounted on the API router: a profiler hands
the heap - registry credentials included - to anything that can reach it, and
`/pprof` is already in `cel.operationalPaths`, which NET-06 fails a deployment
for publishing. The chart declares no containerPort for it, so reaching it is a
deliberate `kubectl port-forward`.

This is what would have made §2 an afternoon's work instead of a day's.

### 5.5 One replication read for the estate - DONE

`useReplicationForAll` issued a request PER PRODUCT to draw the Downloads
page's drift banner - thirty on a real deployment, every time somebody
navigated back to the page, each one authorized, logged, and competing with the
other twenty-nine and with the transfer listing beside them for the browser's
six connections per host.

`GET /api/v1/replication` answers all of it at once. The same shape, and the
same narrowing to `Identity.VisibleProducts`, that `/discovery` and `/packages`
already are - see `middleware.Requirement.AnyScope`, which is set for this path
and is only safe because the handler filters.

THE CONCURRENCY BOUND IS NOW OVER THE WHOLE CALL. The per-product handler
bounded its own fan-out at eight, which bounds nothing once one request reads
every product: thirty products of four targets would open a hundred and twenty
registry connections from a single GET. `TestFleetReplicationBoundsTheWholeFanOut`
asserts it, and was checked to fail when the limit is applied per product.

**A bug this found.** `ListReplicationResponse` was declared in TypeScript as
`{ replication: ReplicationView[] }`; the server has always sent `{ targets:
... }`. Nothing caught it - `api.get` casts the parsed body with `as T` rather
than validating it - so the field read `undefined` and every caller's `?? []`
turned that into an empty list. **The drift banner was drawn from it and
therefore never appeared**, and `DownloadDetail` decided whether a target was
mirrored from the same empty list, so a delegated target with no sync yet
showed no mirror step. Both looked like working code and neither had a test.
The type now matches the wire, and the compiler found both callers.

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
