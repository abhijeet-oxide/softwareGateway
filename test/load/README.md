# Measuring this service

```sh
task bench          # what one query costs as the estate grows
task bench:api      # what the service does under concurrent readers
```

Two tools, because "is it fast" is two questions and they have different
answers here.

## `task bench` - the query cost

Go's own benchmarks (`internal/store/transferlisting_bench_test.go`). Nothing to
install, and `benchstat` compares two runs and says whether a difference is
real rather than noise:

```sh
git stash && task bench -- -count 10 > main.txt
git stash pop && task bench -- -count 10 > new.txt
task bench:compare
```

This is the layer where the regression that matters shows up. The transfer
listing projects a dozen aggregates over `jobs`, one correlated subquery each,
per row of the page - so a page costs what the ESTATE has done, not what the
reader asked for. On fifteen thousand job rows, which is about a day of real
use:

| listing                       | per page |
| ----------------------------- | -------- |
| `rollups` (the Downloads page) | ~12 ms   |
| `summary` (`view=summary`)     | ~0.6 ms  |

The gap is the whole cost of the job counts, and it grows with the estate while
the `summary` column does not. That ratio is the thing to watch.

## `task bench:api` - the service under load

[k6](https://k6.io), which is a single static binary with no runtime and no
agent, fetched to `bin/` on first use. `test/load/run.sh` starts a Coordinator
on its own database and port, runs `api.js` against it and stops it.

`api.js` polls the same endpoints the interface polls, in roughly the
proportions `web/src/api/queries.ts` polls them, at one reader and at twenty.

### Reading the result

Three numbers matter:

- **`calibration_healthz`** - `/healthz` touches no database and no
  configuration, so it measures the RUNNER. A run where everything is slow and
  this is slow too was a slow box; a run where this is fast and the listings
  are not is this code.
- **`listing_duration{phase:alone}`** - one reader, nobody else on the machine.
  This is the latency a person actually experiences.
- **`listing_duration{phase:team}`** - twenty readers. Everything above the
  `alone` number is queueing.

### What it gates, and what it does not

The thresholds sit roughly an order of magnitude above where the code sits
today. That is deliberate and it is a trade: these are wall-clock numbers on a
shared runner, and a bound tight enough to catch a ten percent drift would fail
on a noisy box often enough that the job would be removed. So they catch a
projection that started reading the whole table; they do not catch slow creep.

They also run against an **empty estate**, which is the per-request floor
rather than the number an operator feels. To measure a real one, point it at a
database with data in it:

```sh
test/seed/up.sh                   # a local estate with real transfers in it
DB=$PWD/dev/swgw.db test/load/run.sh --keep
```

The gap between those two runs is the subject of
`docs/design/32-performance.md`.

## What neither of these measures

**Transfer throughput.** Bytes moved per second is a property of the registries
and the network between them, not of this process, and a load generator pointed
at the API cannot see it. `internal/store/throughput_test.go` covers the part
that is ours: that the queue stays correct and does not deadlock while many
workers lease from it at once.
