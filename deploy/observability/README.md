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
query interface at `/vmui`, which reads every `.json` file in `dashboards/`
(`-vmui.customDashboardsPath`) and shows one tab per file. Grafana would be a
second image, a second configuration language and a provisioning directory, to
draw panels this already draws.

The same files are mounted into the cluster from a ConfigMap, so what an
operator sees in a lab is what a developer sees on a laptop.

## The four dashboards

They are numbered because vmui orders the tabs by filename, and the order is
the order to read them in.

**01 Overview** — one screen, nine panels, three questions. Is anybody using
it; is the work moving; is anything broken. Start here and leave when one panel
looks wrong.

**02 API** — the service that fronts the work. *Slowest endpoints* says which
route; *time in the database* and *round trips per request* say why. Read
against each other: a route at two seconds with 1.9 of them in the database is
a query to fix, and the same route with 0.01 there is a handler or an upstream
registry, which no amount of index work will touch. *Round trips* is the panel
that finds an N+1 - a count that rises with the size of a page is a listing
asking once per row, and `internal/api/apicost_test.go` holds the same number
in CI.

**03 Transfers and fleet** — the work itself. *Oldest job still waiting* is the
one to alert on: depth is large in a busy queue and in a stalled one, but this
is flat in the first and climbing minute-for-minute in the second. *Throughput*
splits bytes that crossed the wire from bytes a dedupe or a server-side mount
meant nobody had to move, which is the measure of whether this system is
earning its keep. *Fleet concurrency* is three lines and two gaps, and the gaps
are the diagnosis.

**04 Runtime and resources** — CPU, memory, garbage collection, file
descriptors, database size. Where a slow endpoint with a flat query count and
flat database time is usually found.

## What is a gauge and what is a counter

The queue panels are gauges, sampled from the database every fifteen seconds,
because "how much is outstanding" is a question about the present that the
database already answers and that survives a restart.

What the fleet DID is counters, because the rows that hold the answer are
archived - and a gauge that fell when the archiver ran would read as work being
undone. So `queue_jobs` never counts a settled job, and `jobs_completed_total`
never counts an outstanding one.

`jobs_completed_total` takes terminal outcomes only. A registry having a bad day
fails forty in-flight jobs, each retried up to eight times; counting those here
would report three hundred permanent failures from an incident that resolved
itself. The retries are `job_errors_total`, which is worth watching on its own -
a class whose retries are climbing is a dependency degrading before it breaks.

## Changing the scrape interval

It is stated once, in `scrape.yml`, and every panel is written against it. The
`rate()` windows are `5m` rather than something tighter so that lowering or
raising the interval does not silently turn the graphs into noise - a rate over
a window shorter than about four intervals is mostly sampling artefact.

## Adding a panel, or a dashboard

Add a file to `dashboards/` and restart the container; vmui reads them at
startup. Nothing else needs editing - the chart globs the directory and the
compose file mounts it - and `deploy/dashboard_test.go` will check whatever
lands there.

A panel needs a `title`, an `expr` list and a `unit`, and it earns its place the
same way a test does: by telling you something you would otherwise have to
guess at. The `description` is what a reader sees when they do not already know
what they are looking at, which is most of the time.

Three fields are worth knowing:

- `width` is columns out of twelve. Four panels of `6` are a two-by-two grid;
  omitting it gives a full-width panel, which is right for one panel and
  wasteful for four.
- `alias` is a legend template, one entry per `expr`, and `{{label}}`
  interpolates. Without it the legend prints raw label sets - `{ route =
  "/api/v1/transfers" }` - which is most of the visual noise on an unstyled
  dashboard.
- `unit` is a suffix, not a scale. vmui does not convert, so a byte count is
  divided in the expression and the unit says which unit it was divided into.
