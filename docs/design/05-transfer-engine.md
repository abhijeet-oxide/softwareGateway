# 05 — Transfer Engine

> **Prerequisite:** [04 — Queue and Scheduling](04-queue-and-scheduling.md) · **Depends on:** [06 — Registry Abstraction](06-registry-abstraction.md)

This is the throughput document — goal G1. Everything here is written against the `Repository` interface in [06](06-registry-abstraction.md), never against a specific OCI library, because [ADR-001](16-technology-choices.md#adr-001) is deliberately open.

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
             ├───└──────────────┬───────────────┘
      skipped│                  ▼ not same registry / 202 fallback
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

When origin and destination are on the **same registry**, OCI Distribution allows a server-side relocation:

```http
POST /v2/<dst-repo>/blobs/uploads/?mount=<digest>&from=<src-repo>
```

- `201 Created` — mounted. **Zero bytes over the wire**, regardless of blob size.
- `202 Accepted` — the registry declined and opened a normal upload session instead. Fall through to streaming, reusing the session it just handed us.

This is the dominant optimization for **promotion** (lab → production within one internal registry), where a 45 GB promotion can complete in seconds. It applies to replication only when a vendor happens to share a registry with us, which is rare.

Support is uneven in practice, which is why `202` is treated as a normal outcome and not an error, and why the mount attempt is skipped entirely for registries known not to support it ([06](06-registry-abstraction.md) §6).

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
