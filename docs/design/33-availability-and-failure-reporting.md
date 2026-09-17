# 33 - Availability and failure reporting

> **Consumed by:** [09](09-api.md), [19](19-user-interface.md), [03](03-persistence.md)
> **Status:** implemented. §3 is `internal/api/faults.go` and
> `internal/store/faults.go`, §4 is `web/src/uikit/connection.ts` and
> `web/src/api/client.ts`, §5 is `internal/api/handlers.go` (`handlePing`) and
> `web/src/api/health.ts`, §6 is `internal/store/availability.go`,
> `internal/maintenance/availability.go`, `internal/api/availability.go` and
> `web/src/components/availability.tsx`.

---

## 1. Why this document exists

A deployment with nothing wrong with it reported itself as repeatedly falling
over.

Opening one release's Compliance tab produced this, in a loop, several times a
minute, for as long as the page was open:

```
Software Gateway is temporarily unavailable
Connection restored
Software Gateway is temporarily unavailable
```

Nothing had restarted. The Coordinator was serving every other page throughout.
The network tab filled with hundreds of requests - `whoami`, `version`,
`workers`, `transfers:activity`, every query on the page - re-issued over and
over, and the console showed a 401 on the health probe every forty-five seconds
on top of it.

Four separate decisions, each defensible on its own, combined into that. This
document records all four and what they are now, because the failure was not in
any one of them: it was in what they meant *together*, and that is exactly the
kind of thing a pull-request comment cannot hold.

## 2. What actually happened

One query was wrong. `COALESCE(pa.annotations, '')` on a column Postgres
declares `JSONB`, which it refuses with `SQLSTATE 22P02`; SQLite, whose dialect
is largely the identity function, had never minded. The endpoint behind the
Compliance tab could not work on Postgres at all.

From there:

1. The handler reported its own failure as `UNAVAILABLE`, which is **503**.
2. The browser's HTTP client reads 503 as a statement about reachability and
   reported it to the connection monitor.
3. The monitor treated a 503 from ordinary traffic as a confirmed outage - no
   check needed, since a service that answers 503 has answered - and put an
   outage banner over the whole application.
4. Its probe then found the service healthy a second later, which counted as a
   **recovery**, which invalidated every active query so the screen would fill
   back in.
5. One of those queries was the broken one. Back to 1.

Each step is reasonable. The loop is not, and neither is any part of what the
reader was told: there was no outage, the backend never restarted, and the one
thing that was actually broken - a query - was the one thing never named.

## 3. A status code is a promise, and 503 is the strongest one

**503 means this service is not serving right now.** A restart, a maintenance
window, a replica pulled out of rotation. It is the one status a client may act
on by *waiting*, and every client in this system does: the connection monitor
draws an outage, the boot screen draws a maintenance page, a CLI retries.

An error that will fail identically on every retry must therefore never wear
it. That rule was being broken wholesale - every read in the API answered
`UNAVAILABLE` when its query failed, regardless of why.

The distinction is now made where the error comes from:

- **The database could not be reached** - a refused or dropped socket, a pool
  with no good connections, a server shutting down or refusing new sessions
  (SQLSTATE class `08`, `57P01`-`57P03`, `53300`, `53400`). This is an outage.
  503.
- **The database refused a statement** - a syntax error, a constraint, a type
  the column will not take. This is our own fault, it will happen again next
  time, and somebody must be told rather than asked to wait. 500.

`store.Unreachable` draws that line and `Server.fault` applies it. A timeout is
deliberately on the *not an outage* side: a statement that ran out of time is
usually a slow query rather than an absent server, and filing it as an outage
hands the same lie back under a different name.

### What was considered and rejected

**Keep 503 and teach the client to ignore some of them.** This is the
arrangement that failed. Every client would need the same table of exceptions,
and the first one to forget re-creates the loop.

**Answer 500 for everything, including an unreachable database.** Cheaper, and
it loses the one case where waiting genuinely is the right response - which is
also the case where an operator most needs to be sent to look at Postgres
rather than at a query.

## 4. One endpoint is not the service

Even with §3 correct, the monitor had a second copy of the same mistake: an
`unavailable` verdict from ordinary traffic bypassed the confirming check.

**Only a probe may declare an outage now.** A failing request is evidence, and
what it buys is a check - the monitor asks the service directly and the answer
decides. The reasoning that a 503 needs no confirmation is sound about the
*service* and wrong about a *request*: a 503 arrives from one endpoint, and one
endpoint is not the service.

The recovery side is the same argument. `unstable` exists so that a blip says
nothing; coming out of one is the blip resolving, not an event, so it no longer
announces a recovery or refetches the screen. Only a confirmed outage -
`offline` or `unavailable` - can be recovered *from*.

Those two together are what break the loop. A broken endpoint now fails on the
page that asked for it, with its cause and a request ID, and the rest of the
application does not notice.

## 5. The probe has an endpoint of its own

The monitor sends no credentials, on purpose: a check running every forty-five
seconds in every open tab has no business in the token renewal path, where an
expiry during an outage could start a sign-in nobody asked for.

It asked `/api/v1/system/version`, which is authenticated, and read the 401 as
a healthy answer - reachability is not authorisation, and a service that
refuses an anonymous caller has demonstrably received the request. Sound, and
indistinguishable from a broken session: an administrator inspecting a page
full of errors found their own health check being refused on a timer and
reasonably concluded their sign-in had failed.

`/api/v1/system/ping` is anonymous **by contract**. It is on the `/api/v1`
prefix rather than at the root, because whether a browser can reach `/healthz`
depends on how the proxy in front of the deployment is routed, and an address
that answers 404 to the probe while the API works perfectly would report an
outage that does not exist. It touches no dependency and carries one field:
anything more would be published to whoever can reach the address, and a probe
that checked the database would report a degraded deployment as an unreachable
one.

Readiness remains `/readyz`: a different question, asked by kubelet, with
different consequences.

## 6. The service records its own availability

Sections 3 to 5 stop the interface from *inventing* an outage. They do not give
anybody a way to answer the question that was underneath the whole incident:
**was it up, and how often has it not been?**

The deployment ships VictoriaMetrics, which holds `up` for whoever has it open
and knows what to type into it. That is not an answer for a release manager who
has been told the gateway broke this morning, in the product, on the page they
already have open - and a vacuum in the interface is what the browser's
guesswork filled.

So the Coordinator records it. `service_availability` holds one row per
continuous **run**: one replica, one status, serving from `began_at` to
`until_at`. A beat every fifteen seconds extends the run it belongs to; a beat
too late to be continuous with the last one starts a new row, and the gaps
between rows are the outages.

Three properties earn the design:

- **It cannot lie about being up.** Writing a row requires the process to be
  running and the database to be reachable. Nothing can record that it was
  serving during a crash, a failed deployment or a database outage, because
  recording it needs the very things that were not working.
- **It is small.** A row per restart rather than a row per beat: a replica that
  runs for a month is one row, and reading a month of history is a handful of
  them rather than an aggregation over a hundred thousand.
- **It is honest about what it does not know.** A gap is also what "nobody had
  this deployed yet" looks like, so the window is clipped to the start of the
  record and `UNKNOWN` is a reported status. **Unknown is not down**: they are
  the same absence in the data and opposite facts to a reader.

### The union, which is the whole point

Each replica records its own runs, and availability is the **union** of them:
the service was there whenever any replica was serving. Summing them would
report two replicas as two hundred per cent uptime, and - worse - a rolling
restart in which one replica always held the traffic would show a gap in each
individual record and an outage the service never had. A deployment that pages
somebody every time it ships is a deployment nobody ships.

This is also why the recorder is the one loop in `internal/maintenance` that is
**not leader-gated**. A gated recorder goes quiet the moment the leader dies,
which is the exact event it exists to capture. Only the pruning is gated.

### What was considered and rejected

**Query VictoriaMetrics from the Coordinator.** It is the more accurate record
and it is already scraping. It also makes a metrics stack a hard dependency of
the Overview page, needs an address, a credential and a network path in every
environment including `task run`, and answers nothing on a deployment that
chose not to run it. The record here is worse at "how slow was it at 14:05" and
better at the only question being asked.

**A row per heartbeat.** Five and a half thousand rows a day per replica, all
saying the same thing, and "when was it down" becomes a scan to find the gaps.

**Write a closing row on shutdown.** It would distinguish a clean stop from a
crash. It would also make the record kinder to this process than to its users,
who could not reach the service either way - so the record is what an outside
observer would have seen, and nothing is written on the way out.

### The resolution is part of the answer

Fifteen seconds, and the panel says so. An interruption shorter than a beat
cannot be seen by anything that samples - here or in a metrics stack - and a
record that did not state its resolution would invite somebody to conclude a
thirty-second outage never happened because it is not listed.

## 7. What would change our mind

- **A deployment needs availability across components, not just the
  Coordinator.** The table is keyed by component already; the worker would beat
  into it the same way, and the panel would gain a row rather than a design.
- **Somebody needs sub-second resolution, or per-endpoint availability.** Both
  are the metrics stack's, and the answer is a link to it rather than a second
  copy of it here. The panel deliberately shows up, down, when and how often,
  and nothing else.
- **A gap becomes ambiguous enough to be misleading** - for example if a
  deployment routinely stops the Coordinator on purpose overnight. The record
  would then need a way to mark an interruption as intended, and the argument
  in §6 against a closing row would have to be revisited with it.
