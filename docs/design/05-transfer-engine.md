# 05 — Transfer Engine

> **Prerequisite:** [04 — Queue and Scheduling](04-queue-and-scheduling.md) · **Depends on:** [06 — Registry Abstraction](06-registry-abstraction.md)

This is the throughput document — goal G1. Everything here is written against the `Repository` interface in [06](06-registry-abstraction.md), never against a specific OCI library. That was originally because [ADR-001](16-technology-choices.md#adr-001) was open; it is now because the abstraction is what keeps the closed decision reversible. ADR-001 closed at M3 on `oras-go/v2`, confined to the write path inside `internal/registry/generic/` — nothing in this document changed as a result, which was the design goal.

---

## 1. The shape of the problem

A 45 GB package, ~850 blobs. Blob sizes follow a heavy-tailed distribution: hundreds of small manifests and config blobs measured in kilobytes, a long middle of 10–200 MB layers, and a handful of multi-GB layers (ML model weights, embedded VM images, database seed data).

Three consequences drive the design:

1. **Per-request overhead matters for the small blobs.** 850 blobs × (auth + connect + TLS) is a lot of round trips if any of it is repeated per blob. Hence token caching and connection reuse (§5).
2. **Throughput matters for the large ones.** A single 8 GB layer cannot be parallelized across workers — OCI has no ranged upload that would let two workers write one blob — so it must be *fast*, and it must not have to start over. Hence transport tuning (§5) and resumption (§4.6).
3. **The best transfer is the one that does not happen.** Deduplication and mounting routinely eliminate 30–70% of a package. Hence the fast paths (§4.1–4.2), which is where the largest wins live.

## 2. Where work happens

| Stage | Runs on | Why there |
|---|---|---|
| Plan | Coordinator | Needs the database (placements, dedupe) and produces job rows |
| Transfer | Worker | Needs bandwidth, not state |
| Verify digest | Worker, inline | Free while bytes are already in hand |
| Record outcome | Coordinator | Sole DB writer ([00](00-overview.md) §5.2) |

## 3. Planning

The planner turns a Package plus a target into a set of jobs. It runs on the Coordinator when a transfer is created (or when a scheduled request comes due — [04](04-queue-and-scheduling.md) §10).

```
1. Resolve the package manifest from the origin repository.
      Manifest bytes are stored verbatim (03 section 5) -- never re-serialized,
      because the digest is the hash of these exact bytes and signatures
      are over that digest.

2. Walk the manifest tree, breadth-first:
      index -> child manifests -> config blobs + layers
   Record artifacts with their depth; collect the distinct blob set.
   DISTINCT matters: a package that references the same base layer from
   five images yields one blob, hence one job.

3. Classify every blob against the destination:
      a. blob_placements hit, within TTL   -> SKIP  (no job created)
      b. mountable (same registry)         -> job, mount hint set
      c. otherwise                         -> job, stream

4. Assign waves from artifact depth:
      blobs = 0, manifests = 1 + depth-from-leaf   (04 section 3.2)

5. Insert jobs: wave 0 as 'pending', waves >= 1 as 'blocked'.
   ON CONFLICT (transfer_id, kind, digest) DO NOTHING -- replanning is idempotent.

6. Record plan totals on the transfer: planned_job_count, planned_bytes,
   dedupe_skipped_bytes, mountable_bytes.
```

Step 3a is a database lookup, not a network call — that is what makes planning a 1,000-blob package fast. Registry `HEAD` checks are deferred to the worker, where they are parallel and where a stale placement is caught anyway (§4.1).

**Planning is idempotent.** A Coordinator that crashes mid-plan leaves a partial job set; on restart the transfer is still `planning` and the planner re-runs, with `ON CONFLICT DO NOTHING` making already-created jobs free.

## 4. Executing a blob job

Four fast paths, ordered cheapest-first. A job takes the first that applies.

```
                    ┌─────────────────────────┐
                    │   blob job leased       │
                    └───────────┬─────────────┘
                                ▼
                 ┌──────────────────────────────┐
             ┌───│ 1. placement hit (DB, cached)│  0 bytes, 0 RPC
      skipped│   └──────────────┬───────────────┘
    placement│                  ▼ miss
        _hit │   ┌──────────────────────────────┐
             │   │ 2. HEAD blob at destination  │  0 bytes, 1 RPC
             ├───└──────────────┬───────────────┘
      skipped│                  ▼ 404
  exists_at_ │   ┌──────────────────────────────┐
      target │   │ 3. cross-repo MOUNT          │  0 bytes, 1 RPC
             │   │    a. sibling dest repository│
             │   │    b. source, if same registry│
             ├───└──────────────┬───────────────┘
      skipped│                  ▼ nothing nearby holds it / 202 fallback
      mounted│   ┌──────────────────────────────┐
             │   │ 4. STREAM source -> dest     │  N bytes
             └───└──────────────────────────────┘
                                ▼
                        succeeded / retry
```

### 4.1 Placement hit

`blob_placements` ([03](03-persistence.md) §5) answers "is this digest already in this repository" without a network call. On a hit the job completes as `skipped` with `skip_reason = 'placement_hit'`.

The placement set is shipped to the worker **in the lease response** rather than queried per job, so a worker holding 16 jobs makes zero extra calls to resolve them.

**Staleness.** A placement is strong evidence, not proof ([01](01-domain-model.md) §4) — a registry's garbage collector can remove content underneath us. Two defences:

- Entries older than `placementTTL` (default 24 h) are not trusted; the job falls through to path 2, and success refreshes `verified_at`.
- **The backstop:** if a manifest push later fails with `BLOB_UNKNOWN`, the Coordinator invalidates the placements for that manifest's blobs and requeues them ([11](11-resiliency-and-backpressure.md) §2.5). The registry itself tells us when the cache is wrong, which is what makes the optimistic path safe rather than merely fast.

### 4.2 Cross-repository mount

Where the destination registry **already holds the blob in another of its own repositories**, OCI Distribution allows a server-side relocation:

```http
POST /v2/<dst-repo>/blobs/uploads/?mount=<digest>&from=<other-repo>
```

- `201 Created` — mounted. **Zero bytes over the wire**, regardless of blob size.
- `202 Accepted` — the registry declined and opened a normal upload session instead. Fall through to streaming, reusing the session it just handed us.

There are **two** repositories worth trying, and they cover different cases:

1. **A sibling destination repository.** A bundle's components are published twice — inside the bundle so its index resolves, and under their own name so they can be pulled as themselves ([§ layout](../../internal/transfer/layout.go)). One digest, two destination repositories, two jobs. The Coordinator resolves a sibling that already holds the digest and ships its path in the lease as `mountFromRepository`, so the second job relocates rather than transfers.

2. **The source repository**, when source and destination share a registry. This is the **promotion** case (lab → production within one internal registry), where a 45 GB promotion can complete in seconds.

Only the second existed at first, and its test — "not the same registry, so do not mount" — passes for every replication from a vendor. So the first copy of a relocated component streamed and the second streamed as well, doubling the WAN cost of nearly every ORB. It is worth being precise about how that hid: both jobs are correct, both report success, the transfer completes, and the only symptom is duration. `TestBundleFetchesEachBlobFromTheVendorOnce` asserts the vendor serves each blob once, because nothing else would notice.

Concurrent duplicate suppression ([04](04-queue-and-scheduling.md) §5) is what makes case 1 fire at all: the two jobs are created consecutively and would otherwise be leased in the same batch and stream simultaneously, before either had placed anything for the other to mount. It is therefore keyed by **registry**, not by repository.

Support is uneven in practice, which is why `202` is treated as a normal outcome and not an error, and why the mount attempt is skipped entirely for registries known not to support it ([06](06-registry-abstraction.md) §6). A mount that fails for any reason falls through to streaming, which is always correct.

#### The mount hint looks much further back than the placement skip

Both read `blob_placements`, and they need different degrees of trust because being wrong costs different things.

| Use | If the record is wrong | Horizon |
|---|---|---|
| skip (§4.1) | the content is not there, and the manifest referencing it fails | `placementTTL`, 24 h |
| mount hint | the registry declines and the worker streams | 90 days |

They shared the 24-hour TTL, and that was expensive in exactly the case where it matters most. **A vendor's bundle carries its release in its repository path** — `orbs/cfx-5000-k8s-215952-edgenac-25.7-2131_…` — so every new release lands in a brand-new destination repository that necessarily holds no placements of its own. Its components go to version-*stable* paths and dedupe normally; the bundle-internal copy has nothing to dedupe against and depends entirely on mounting from where the last release put the same digests.

A release shipped more than a day after the previous one therefore found those records expired, got no mount candidate, and re-streamed the whole bundle across the WAN — for content the destination registry was holding the entire time. Nothing about correctness changes with the longer horizon: a stale hint costs one request.

### 4.3 Streaming — the core loop

**Invariant I5: bytes never touch worker disk.**

```go
// Simplified; the real implementation carries context, metrics, and the
// progress reporter. Written against the Repository interface (06), not
// against any specific OCI library.
func (e *Engine) streamBlob(ctx context.Context, job Job, src, dst Repository) error {
    rc, err := src.FetchBlob(ctx, job.Digest)   // HTTP GET; body is a stream
    if err != nil {
        return err
    }
    defer rc.Close()

    // Verify the digest of what we READ, as we read it. See 4.4.
    verifier := digest.Verify(job.Digest, rc)

    // Report bytes as they pass, without a second copy. See 4.5.
    counted := progress.Wrap(verifier, job.ID, e.reporter)

    // dst.PushBlob consumes the reader and returns when the registry has
    // committed the blob. No temp file, no buffering of the whole body.
    if err := dst.PushBlob(ctx, job.Digest, job.Size, counted); err != nil {
        return err
    }
    if !verifier.Verified() {
        return ErrDigestMismatch      // 11 section 2.3: low retry cap
    }
    return nil
}
```

**What is deliberately absent:**

- No `io.ReadAll`. Reading an 8 GB layer into memory would OOM the pod.
- No temp file. Disk would cap concurrency at the volume's IOPS, add a failure mode (disk full), and require cleanup on crash — the one thing workers are designed not to need.
- No decompression. Blobs move as opaque bytes. Decompressing to recompress would burn CPU and change the digest, which would break every signature.
- No manifest rewriting (invariant I9). Bytes in, identical bytes out.

**Backpressure is free.** `io.Copy` from an HTTP body into an HTTP request body means a slow destination naturally slows reads from the source, via TCP flow control on both sides. There is no queue between them to grow and no buffer to size. This is why the pipe is one `Copy` rather than a producer/consumer pair with a channel — the simpler structure has better behaviour.

### 4.4 Digest verification during transfer

Every streamed blob is verified inline, as a wrapper on the read side. This costs one SHA-256 pass over data already in cache — on modern hardware with SHA extensions, low single-digit percent of the transfer's CPU, and entirely overlapped with network I/O.

Two independent checks, and they catch different things:

| Check | Catches |
|---|---|
| Our verifier on the **read** side | A source registry serving wrong or corrupted bytes |
| The registry's own check on `PUT` | Corruption anywhere in our path, including in transit to the destination |

Together they give end-to-end integrity without trusting either registry or the network. A mismatch fails the job with `ErrDigestMismatch`, retried at most twice ([11](11-resiliency-and-backpressure.md) §2.3) — if a source is serving bad bytes, retrying is unlikely to help and the failure should be surfaced, not absorbed.

This is **separate from signature verification** ([08](08-verification.md)) and the two must not be conflated: digest verification proves *the bytes are the bytes we asked for*; signature verification proves *the vendor vouched for them*.

### 4.5 Memory

The ceiling is explicit and is the basis for the worker's memory request ([14](14-deployment-and-development.md) §3):

```
peak ≈ maxConcurrentJobs × (copyBufferSize + transport buffers)
     ≈ 16 × (1 MiB + ~256 KiB)   ≈ 20 MiB   for blob data
```

Plus the Go runtime, the manifest cache, and a lease's worth of job records — call it 128 MiB request, 256 MiB limit for a 16-way worker. **Crucially this is independent of blob size:** a worker moving eight 8 GB layers uses the same memory as one moving eight 8 MB layers. That property is what makes the "no disk" rule affordable, and it is a direct consequence of streaming.

Buffer sizing: 1 MiB by default. Larger buffers stop helping once the bottleneck is the network; smaller ones increase syscall overhead on fast links. Tunable via `worker.copyBufferSize` ([02](02-configuration.md) §8).

### 4.6 Uploads: monolithic by default, chunked for resumption

OCI Distribution offers two upload styles, and the choice is a genuine trade-off rather than an obvious win.

**Monolithic (default).** The source `HEAD`/`GET` gives us `Content-Length`, so we know the size up front and can do a single `POST` + `PUT` with the body streamed. Fewest round trips, no per-chunk latency, best throughput. **Failure means starting that blob over.**

**Chunked.** `PATCH` a sequence of ranges, then `PUT` to commit. The registry returns a `Location` and accepts a `Range`, so an interrupted upload can resume. We persist `{location, offset}` in `jobs.upload_state` ([03](03-persistence.md) §6.1) and resume on retry.

**Policy:**

```
size < chunkedUploadThreshold (default 1 GiB)  -> monolithic
size >= threshold AND registry supports resume -> chunked
otherwise                                      -> monolithic
```

Rationale: below the threshold, restarting a failed blob costs less than the per-chunk overhead of every successful one. Above it, restarting an 8 GB upload at 95% is painful enough to pay for chunking.

> **Honest weak spot.** Registry support for *resuming* a chunked upload after a client disconnect is uneven — the specification permits it, several registries accept the `PATCH`/`Range` protocol but do not durably retain a session across a connection drop, and behaviour varies by version and by storage backend. We therefore:
>
> - treat resumption as an **optimization that may fail**, never a correctness requirement;
> - probe support per registry at startup and cache the result ([06](06-registry-abstraction.md) §6);
> - fall back to restarting the blob on any resume failure, which is always correct because blobs are content-addressed and idempotent;
> - record `softwaregateway_upload_resume_total{result}` so we learn which registries actually honour it in our environment rather than trusting documentation.
>
> The failure mode of guessing wrong is a repeated transfer, not corruption. This is the M3 spike's "resumable upload control" criterion ([16](16-technology-choices.md#adr-001)).

## 5. Transport tuning

Defaults tuned for large-body transfers rather than for API traffic. Each setting below deviates from Go's defaults for a stated reason.

```go
&http.Transport{
    // Force HTTP/1.1 for the blob data plane. See the decision below.
    ForceAttemptHTTP2:   false,
    TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},

    // Blobs are already gzip/zstd compressed. Attempting again burns CPU,
    // and -- worse -- transparent decompression would break the digest.
    DisableCompression:  true,

    // Default is 2. With 16 concurrent jobs against one registry that would
    // serialize almost everything behind connection churn.
    MaxIdleConnsPerHost: cfg.MaxConnections,
    MaxConnsPerHost:     cfg.MaxConnections,
    IdleConnTimeout:     90 * time.Second,

    WriteBufferSize:     64 * 1024,   // default 4 KiB: too many syscalls
    ReadBufferSize:      64 * 1024,

    DialContext: (&net.Dialer{
        Timeout:   cfg.ConnectTimeout,
        KeepAlive: 30 * time.Second,
    }).DialContext,
    TLSHandshakeTimeout:   10 * time.Second,
    ResponseHeaderTimeout: cfg.ResponseHeaderTimeout,
    Proxy:                 productProxy(cfg),      // 02 section 4
    TLSClientConfig:       &tls.Config{RootCAs: productCAPool(cfg)},
}
```

> **Decision — HTTP/1.1 for blob transfer, HTTP/2 permitted for control traffic.**
>
> *Why this is counter-intuitive:* h2 is newer and better for most workloads, and Go enables it automatically over TLS.
>
> *Why it loses here:* HTTP/2 multiplexes every stream onto **one TCP connection**. For many small requests that is a large win — one handshake, no head-of-line blocking at the connection level. For a handful of multi-gigabyte bodies it inverts:
> - Per-stream and per-connection **flow-control windows** (65 KB default, and not always well auto-tuned) throttle a single large body far below link capacity on high bandwidth-delay-product paths.
> - All streams share one congestion window, so one slow blob **stalls the others behind it**.
> - Frame multiplexing adds per-chunk CPU for no benefit on a body that is one logical stream.
>
> With HTTP/1.1 and a connection pool, sixteen concurrent blobs are sixteen independent TCP connections, each with its own congestion window, scaling independently. This is why `crane`, `skopeo`, and most registry clients see better bulk throughput on h1.
>
> *Cost accepted:* more sockets and more TLS handshakes. Both are amortized by the connection pool, and neither is close to a bottleneck.
>
> *Not dogma:* this is a per-registry setting (`forceHTTP1`, default true). A registry that demonstrably performs better on h2 can opt out, and `softwaregateway_transfer_throughput_bytes_per_second{registry}` is the evidence.

**Token caching.** Registry bearer tokens are cached per `(registry, repository, scope)` until shortly before expiry. Without this, an 850-blob package performs 850 token exchanges against the auth endpoint — adding a round trip to every blob and frequently tripping the registry's own rate limits. This is one of the highest-value small optimizations in the system, and one of the easiest to omit by accident.

## 6. Promotion

**Promotion runs the same engine.** The only difference is that the origin is a TargetRepository ([01](01-domain-model.md) §2.1).

No separate planner, queue, state machine, or retry logic — a promotion is a `transfer_requests` row with `operation = 'promote'` and a `source_repo_id` that happens to reference a target. The engine sees two `Repository` values and does not care about their roles.

Promotions are usually very fast, because both fast paths apply maximally: lab and production frequently share a registry (cross-repo mount, §4.2), and production often already holds most blobs from prior promotions (placement hits, §4.1). A 45 GB promotion moving 200 MB of genuinely new content is the normal case, not the exception.

The one asymmetry worth stating: promotion is guarded by `promotionOnly` on the target ([02](02-configuration.md) §5.2), so a production registry can be configured to be reachable *only* by promotion and never by direct replication from a vendor.

## 7. Dry run

> **Dry run is the planner's output, rendered. It is not a second implementation.**

`POST /api/v1/transfers` with `validateOnly=true` ([09](09-api.md) §4, AIP-163) runs §3 steps 1–4, returns the plan, and rolls back the transaction. Nothing is inserted; no bytes move.

```
$ transferctl download --product vendor-a-platform --tag v2.14.0 --target lab --dry-run

Transfer plan — vendor-a-platform / v2.14.0 → lab
  Artifacts               5   (1 index, 3 images, 1 helm chart)
  Blobs                 847   total 45.2 GiB
    already present     291         12.1 GiB   (placement hit)
    mountable             0          0 B       (different registry)
    to transfer         556         33.1 GiB
  Manifests to push       5
  Waves                   3

  Estimated duration   ~11m20s   at 48.6 MiB/s observed for this route
  Bandwidth saved       12.1 GiB (27%)

  No data transferred (dry run).
```

**Estimation** uses an EWMA of recently observed throughput for the same `(source registry, target registry)` route, not a configured constant. Absent history it says so rather than inventing a number — a confidently wrong ETA is worse than none. The estimate accounts for effective concurrency, since a 33 GB transfer across 16 workers is not 33 GB serially.

Sharing the planner is the whole point: a dry run that used different code would eventually disagree with reality, and a dry run nobody trusts has no reason to exist.

## 8. Concurrency, at three levels

| Level | Bounded by | Set by |
|---|---|---|
| Packages in flight | Number of active transfers | Users and auto-download rules |
| Workers | HPA replica count | Queue backlog ([09](09-api.md) §9) |
| Blobs per worker | `min(worker.maxConcurrentJobs, granted budget)` | Coordinator, per lease |

These compose against **fleet-wide per-repository ceilings** ([02](02-configuration.md) §5.3). The Coordinator divides each repository's budget across active workers and ships each worker its share in the lease response. Two things follow:

- Adding workers does **not** multiply load on a vendor registry. A per-worker limit would: scaling from 4 to 40 workers would take a configured "8 concurrent downloads" to 320 and flatten the vendor.
- Scaling changes take effect on the next lease, with no coordination protocol.

Within its grant, a worker runs jobs in an `errgroup` with a semaphore. It requests more work when it falls below its grant, so the lease call is amortized across many jobs rather than being per-job.

The adaptive controller ([11](11-resiliency-and-backpressure.md) §3) moves the effective limit **within** the configured ceiling based on observed latency and error rates. Configuration sets the maximum; the controller decides how much of it is safe right now.

### 8.1 Calibration — deciding those numbers by measurement

The table above lists what can be turned. It does not say what to turn it *to*, and nothing in a configuration file does either: the answer depends on the link, the proxy, the vendor's own limits and the distance between the two registries, and it is different for every path. Left to guesswork the failures are systematic and all in one direction — raising concurrency against a link that is already saturated, or adding a worker while a proxy quietly halves the line rate.

`POST /api/v1/products/{product}:calibrate` (`transferctl calibrate`, [13](13-cli.md) §11) measures the path instead. It is implemented in `internal/calibrate` and runs in the **Coordinator** process, for the same reason preflight does: `transferctl` is a pure API client and never opens a connection to a registry itself.

**What it measures**

| Probe | How | What it settles |
|---|---|---|
| Route | `DescribeProxy`, then a direct `/v2/` when a proxy is in force | Whether the proxy is mandatory, and what bypassing it is worth |
| Latency | Three pings, minimum kept | Whether a single stream is bandwidth-delay limited |
| Read | Real blobs from the source, discarded | The source ceiling and its knee |
| Write | Real bytes into an upload session that is then **cancelled** | The target ceiling and its knee |

The write probe is the part worth understanding. Distribution v2 separates the upload *session* from the *commit*: `POST …/blobs/uploads/` opens one, `PATCH` streams into it, and a blob joins the repository only when a `PUT` names its digest. Calibration never sends that `PUT` — it PATCHes, measures, and `DELETE`s the session. So the bytes cross the same proxy, TLS, front end and storage backend a transfer's bytes cross, and the repository ends the run exactly as it started. The alternative — push a real blob and delete it afterwards — needs a delete permission nothing else here uses, is visible between the two steps, and leaves an artefact behind if the process dies mid-run.

**What the sweep does and does not honour**

It overrides `maxConnections`, because that is the variable under test: sweeping to sixteen streams through a pool configured for four would measure the pool four times and call the result a plateau. It honours `requestsPerSecond`, because that is a promise to a vendor, and a calibration that broke it would report a throughput no honest configuration could reproduce. It stops early when a level improves on its predecessor by less than 10% — past that point each further level doubles the load on somebody else's registry to confirm something already known.

**Finding something to measure**

The read probe needs real blobs, and reaching them means descending — as far as it takes. A bundle is an **index of indexes**: the ORB lists its components, each component is a multi-platform index, and the layers are a level below that. A search that descends once lands on a component index, finds no layers because an index *has* none, and concludes that a repository holding gigabytes contains nothing worth measuring. It descends to the same depth the transfer walk does, depth-first, and stops at the first manifest that has layers — a handful of requests rather than the whole tree.

Tag order matters for the same reason. Registries serve tags lexically and guarantee nothing about it, so the first tag is the oldest spelling; newest-first is the better sample. But newest-first walks straight into the signatures, which sort adjacent to the release they belong to and, for both conventions this system knows, sort *after* it — so `signature_orb_25.7` and cosign's `sha256-….sig` are pushed to the back of the queue and opened last. They are still opened: a repository holding nothing else is still measurable.

And when nothing clears the 256 KiB the probe would like, it measures the largest blob there is and **says the sample was small**, rather than refusing. A number with a caveat beats a refusal citing a threshold the reader cannot see. Every report states how many blobs were sampled and how large the largest was, so a throughput measured over signature blobs cannot be mistaken for one measured over layers.

**Setup is not throughput**

Each level builds a fresh client, deliberately — a level must not inherit the previous one's warm sockets. That means its first request pays a proxy `CONNECT`, a TLS handshake, a token exchange and a blob resolve before a byte of payload moves. Against a registry 900 ms away through a corporate proxy that is five round trips, more than the whole default budget, and every level's first request was still in flight when the level ended: a complete sweep of zeroes, reported without comment.

So the connections and the token are established **before the clock starts** — one `HEAD` per stream on the read side, one upload session per stream on the write side — and the per-level budget follows the link rather than a constant: ten round trips, capped at three times what was asked for, since a client is waiting on a timeout derived from that number. A level that still completes nothing says so and names `--budget`, because a row of dashes with no explanation reads as a broken probe rather than as a window too short for the path.

The direct-route probe gets a short deadline of its own for the same reason. Where the proxy is mandatory it does not fail, it hangs — a handshake to a host the network will not route to sits until the transport's own thirty-second timeout, twice. A route that cannot handshake in ten seconds is not one this would recommend.

**Which repository is measured**

One, out of however many a product spans, so the choice decides whether the numbers mean anything. `transferctl` picks the repository holding the largest discovered package and shows that choice for confirmation ([13](13-cli.md) §11); the Coordinator's own fallback, for API callers and for products nothing has been discovered from, walks the candidate repositories until one yields a blob of at least 256 KiB rather than judging a source by whichever repository sorts first. Within a repository it opens the newest-looking tags first: registries serve tags lexically, so "the first tag" reliably lands on the oldest and smallest.

The write probe's path is `DestinationPath(target base, source repository)` — the same join the planner uses. Probing the target's configured repository directly does not work: a base path is a prefix, not an image repository, and an upload session opened against it returns `404` from a healthy registry.

**The knee, not the peak**

Advice targets the smallest concurrency within a tenth of the best measured. Configuring the peak instead typically buys single-digit percent for double the concurrent load, which is a bad trade against a registry that is not ours.

**What it cannot tell you**

Which host the workers are on. Every report names the host that measured it, and if the workers sit on a different network the numbers describe a path no transfer takes. Dispatching the probe to a named worker is the natural next step and needs the worker's address, which the fleet does not currently report in its heartbeat.

## 9. Manifest jobs

Simpler than blob jobs, and gated by waves so everything they reference already exists.

```
1. HEAD the manifest at the destination.
   Present with a matching digest -> skipped (idempotent re-push).
2. PUT the stored raw bytes (03 section 5), verbatim.
3. On BLOB_UNKNOWN: invalidate placements for this manifest's blobs,
   requeue those blob jobs, return this job to 'blocked' for a later wave.
   (11 section 2.5)
4. On success at the top wave, tag the destination.
```

Step 3 is the self-healing path for stale placements, and the reason the optimistic fast path in §4.1 is safe rather than merely fast.

Tagging happens **last**, only after the index manifest is committed — invariant I1. Until that moment the destination holds a set of unreferenced blobs, which are harmless, invisible to consumers, and useful to the next transfer.
