# Seeing whether this deployment is slow

```sh
docker compose up -d
open http://localhost:8428/vmui        # the dashboards
open http://localhost:8428/targets     # what is being scraped, and whether it works
```

Nobody reports a slow page. They stop using it. So the numbers that answer
"which endpoint is slow, and why" are part of the default stack rather than
something to set up when somebody finally complains.

## One container, no Grafana

VictoriaMetrics scrapes on its own (`-promscrape.config`) and serves its own
query interface at `/vmui`, which reads the dashboards in this directory
(`-vmui.customDashboardsPath`). Grafana would be a second image, a second
configuration language and a provisioning directory, to draw nine panels this
already draws.

The same two files are mounted into the cluster from a ConfigMap, so what an
operator sees in a lab is what a developer sees on a laptop.

## Reading the dashboard

The rows are in the order you need them.

**Which endpoints are slow** — p95 and p99 by route template, and how often
each is called. A slow endpoint nobody calls is a different problem from a slow
endpoint on every page. p99 far above p95 means the slowness is occasional,
which points at contention rather than at the query.

**Why they are slow** — the row that usually answers it:

- *Database round trips per request* is the panel that finds an N+1. A route
  whose count rises with the size of its page is asking the database once per
  row. Latency cannot show this on a small database: twenty-five extra round
  trips cost forty milliseconds there and minutes in a real deployment. Two
  listings in this application shipped exactly that way. Anything above single
  digits for a listing deserves a look, and `internal/api/apicost_test.go`
  holds the same number in CI.
- *Connection pool* and *time waiting for a connection* are what turn one slow
  query into a slow page. A request that cannot get a connection waits without
  doing any work, and that wait is charged to whatever route it was serving -
  so when the pool is pinned, the route labels above point at victims rather
  than at the cause. Read this row before optimising anything the first row
  named.

**Errors and saturation** — a route that fails fast looks healthy on a latency
graph, and a route that is slow because it is retrying a failing dependency
looks like a slow query until the error rate is read beside it. `up` is last
because a dashboard that has stopped updating looks exactly like a system that
has stopped doing anything.

## Changing the scrape interval

It is stated once, in `scrape.yml`, and every panel is written against it. The
`rate()` windows are `5m` rather than something tighter so that lowering or
raising the interval does not silently turn the graphs into noise - a rate over
a window shorter than about four intervals is mostly sampling artefact.

## Adding a panel

Edit `dashboard-api.json` and restart the container; vmui reads the file at
startup. A panel needs a `title`, an `expr` list and a `unit`, and it earns its
place the same way a test does: by telling you something you would otherwise
have to guess at. The `description` is what a reader sees when they do not
already know what they are looking at, which is most of the time.
