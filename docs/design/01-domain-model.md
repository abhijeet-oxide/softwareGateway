# 01 — Domain Model

> **Prerequisite:** [00 — Overview](00-overview.md) · **Consumed by:** [02](02-configuration.md), [03](03-persistence.md), [09](09-api.md), [10](10-state-machines.md)

This document fixes the vocabulary and the entity relationships. Every other document uses these terms exactly as defined here. Where the domain and the database diverge, [03 — Persistence](03-persistence.md) is authoritative for storage and this document is authoritative for meaning.

---

## 1. Product — the root aggregate

**A Product is the unit of configuration, ownership, and blast radius.** It is the only top-level object an operator creates.

The requirements are explicit that configuration is organized around products rather than repositories, and this has consequences well beyond file layout:

- **Configuration.** One product is one ConfigMap, one YAML document. Everything about that product — sources, targets, credentials, CA bundle, proxy, rate limits, notification recipients, auto-download rules, verification policy — is in that one place. Nothing about a product is found by looking somewhere else.
- **Ownership.** Products map to the team that consumes the vendor's software. Notification recipients are a product property because the people who care about `vendor-a-platform` are not the people who care about `vendor-b-database`.
- **Blast radius.** A malformed product config fails that product and no others ([02](02-configuration.md) §7). A misbehaving vendor registry throttles that product's repositories and no others ([11](11-resiliency-and-backpressure.md) §3).

A Product is *not* a security boundary in v1 — there is no per-product authorization, because there is no authentication ([09](09-api.md) §10).

## 2. Entity relationships

```
Product ─┬── SourceRepository (1..n)   vendor-side, read-only
         └── TargetRepository (1..n)   internal, read-write
                    │
                    │  (a Package is discovered in exactly one SourceRepository,
                    │   and replicated into 1..n TargetRepositories)
                    ▼
              Package  (one tag = one software package)
                    │
                    ├── Artifact (1..n)   one OCI manifest each
                    │        │
                    │        └── references Blob (1..n)
                    │
                    └── Blob references are content-addressed, so:

              Blob ────── GLOBAL, keyed by digest alone.
                          Shared across artifacts, packages, products,
                          and vendors. Never scoped to a Product.

              BlobPlacement = (TargetRepository, digest) → present
                          The dedupe index. See §4.
```

### 2.1 SourceRepository vs TargetRepository

Both are OCI repositories reached through the same `Repository` interface ([06](06-registry-abstraction.md)), and the distinction is **role, not type**:

| | SourceRepository | TargetRepository |
|---|---|---|
| Direction | Read only | Read and write |
| Discovery polls it | Yes | No |
| Can be a transfer origin | Yes (replication) | Yes (**promotion**) |
| Can be a transfer destination | No | Yes |
| Credentials | Vendor-supplied | Internal |

Promotion falls out of this table rather than needing its own machinery: a promotion is a transfer whose origin happens to be a TargetRepository. See §3.4.

### 2.2 Package

**A Package is a tag in a SourceRepository, together with the manifest digest that tag resolved to at discovery time.**

The tag alone is not an identity, because tags are mutable — a vendor can re-push `v2.14.0` with different content. Identity is therefore `(source_repository, tag, manifest_digest)`. A re-push produces a *new* Package row, and the previous one is marked `Superseded` ([10](10-state-machines.md) §2). This is deliberate: silently overwriting would destroy the audit trail and make "which bytes did we actually replicate in March" unanswerable.

A Package is a *discovery record and a transfer subject*. It is not a copy of anything — we store metadata about it, never its content.

### 2.3 Artifact

**An Artifact is one OCI manifest within a Package.** A package tag normally resolves to an OCI image index, whose entries are image manifests, Helm chart manifests, config-bundle manifests, and possibly nested indexes. Each is an Artifact. Artifacts form a shallow tree, which is what makes wave-based ordering sufficient ([04](04-queue-and-scheduling.md) §3.2).

### 2.4 Blob — global and content-addressed

**A Blob is content, identified by its digest and nothing else.** This is the single most exploited property in the design.

Because `sha256:abc…` means the same bytes everywhere in the world, a Blob is never scoped to a product, a package, or a vendor. Two products from two different vendors that both ship the same UBI base layer reference *the same Blob*. If one product already put it in a target repository, the other product's transfer of it is free.

The consequence for the data model: `blobs` is keyed by digest globally, and the association to packages is a join table. Scoping blobs per-product would be the obvious mistake and would forfeit most of the deduplication win.

## 3. The transfer vocabulary

These three concepts are routinely conflated and must not be. The API ([09](09-api.md)) exposes all three.

### 3.1 TransferRequest — intent

What a user (or an auto-download rule) asked for: *replicate package P to targets [X, Y, Z], at priority N, optionally at time T.* It carries an **idempotency key**. Submitting the same request twice returns the same TransferRequest and creates no additional work ([04](04-queue-and-scheduling.md) §7).

A TransferRequest is user-facing and durable. It is the thing a scheduled download is scheduled as.

### 3.2 Transfer — one request against one target

A TransferRequest naming three targets expands into **three Transfers**. This is the unit of progress, pause, resume, cancel, and priority.

Separating them matters because targets fail independently. If the production registry is down while lab is healthy, the lab Transfer succeeds and the production Transfer retries. Modelling the request as a single unit would force an all-or-nothing outcome and waste the work already done.

### 3.3 Job — one blob or one manifest

The atomic unit of work. **A worker only ever sees a Job.** It has no concept of a Package.

Two kinds:
- `blob` — move one blob to the destination (or discover it need not be moved).
- `manifest` — push one manifest, once everything it references is present.

Package-level progress is a **rollup** over jobs (`SUM(bytes_transferred)`), never a separately maintained counter. A derived value cannot drift from the jobs it derives from; a maintained counter can, and would eventually report 103% of a package complete.

### 3.4 The four operations, and why there are really two

| Operation | Origin | Destination | Moves bytes |
|---|---|---|---|
| **Replicate** (download) | SourceRepository | TargetRepository(s) | Yes |
| **Promote** | TargetRepository | TargetRepository(s) | Yes |
| **Verify** | either | — | No |
| **Dry run** | either | TargetRepository(s) | No |

Replicate and promote are **the same code path** with different origins — the origin is simply a `Repository` ([06](06-registry-abstraction.md)), and the engine does not care which role it plays. Implementing promotion as a distinct subsystem would duplicate the planner, the queue, the retry logic, and the state machine to gain nothing.

Dry run is likewise not a separate implementation: it is the planner's output, rendered and then discarded ([05](05-transfer-engine.md) §7). A dry run that used different code from a real transfer would eventually lie, which defeats its only purpose.

## 4. BlobPlacement — the dedupe index

**`BlobPlacement = (TargetRepository, digest) → confirmed present`**

This is a small table doing disproportionate work. It answers, without a network call: *does this exact content already exist in this exact destination?*

Its value compounds. Version 2.14.0 of a product shares most of its base layers with 2.13.0, so the second replication of a product typically moves a fraction of its nominal size.

**Placements are scoped to a repository, not to a registry.** Two products replicating into *different* repositories do not share placement rows, even when they share base layers and even on the same registry — a blob present in one repository is genuinely not present in the other. What serves that case is **cross-repository mount** ([05](05-transfer-engine.md) §4.2), which relocates the blob server-side for zero bytes. And a physical repository belongs to exactly one product ([03](03-persistence.md) §4), so the "two products sharing one repository" case does not arise.

**Trust model.** A placement is a cache of a fact about a remote registry, and remote registries can have content deleted out from under us by garbage collection or an administrator. The design treats a placement as *strong evidence, not proof*:

- A placement hit skips the transfer.
- Placements carry `verified_at`; entries older than a configurable TTL are re-confirmed with a cheap `HEAD` before being trusted.
- A manifest push that fails with `BLOB_UNKNOWN` invalidates the placements for that manifest's blobs and requeues them. This is the backstop that makes the optimistic path safe — the registry itself tells us when our cache is wrong.

See [05](05-transfer-engine.md) §4.1 for where this sits in the fast-path ordering, and [03](03-persistence.md) §4 for the schema.

## 5. Glossary

| Term | Meaning |
|---|---|
| **Coordinator** | The control-plane binary. The requirements also call this "Controller"; this document set says Coordinator, matching `cmd/coordinator`. |
| **Worker** | The data-plane binary. Stateless, streams blobs, holds no DB credentials. |
| **transferctl** | The CLI. A pure Coordinator API client. |
| **Product** | Root configuration aggregate. One ConfigMap. |
| **Package** | One tag in a source repository, pinned to the digest it resolved to. |
| **Artifact** | One OCI manifest inside a package. |
| **Blob** | Content addressed by digest. Global, never product-scoped. |
| **BlobPlacement** | Evidence that a digest is present in a target repository. |
| **TransferRequest** | User intent; idempotency-keyed; may name several targets. |
| **Transfer** | One request against one target. Unit of pause/resume/cancel/priority. |
| **Job** | One blob or one manifest. The unit workers execute. |
| **Wave** | Integer topological depth ordering jobs within a transfer (blobs 0, manifests 1+). |
| **Lease** | Time-bounded claim on a job by a worker. Expiry is the crash-recovery mechanism. |
| **Replicate / download** | Source → target(s). |
| **Promote** | Target → target(s). Same engine, different origin. |
| **Verify** | Check signatures and digests. Moves no bytes. |
| **Dry run** | Render the transfer plan without executing it. |
| **Mount** | OCI cross-repository blob mount — server-side relocation, zero bytes transferred. |

## 6. Invariants

Properties the implementation must never violate. Each is enforced by a named mechanism, not by convention.

| # | Invariant | Enforced by |
|---|---|---|
| I1 | A package tag never appears at the destination before all its blobs are present | Wave ordering ([04](04-queue-and-scheduling.md) §3.2) |
| I2 | A blob is not transferred to a given target twice concurrently | In-flight suppression at lease time, plus the placement fast path ([04](04-queue-and-scheduling.md) §5) |
| I3 | A duplicate TransferRequest creates no additional work | Unique index on `idempotency_key` ([03](03-persistence.md) §5) |
| I4 | A re-scan of a source repository creates no duplicate packages | Unique index on `(source_repo_id, tag, manifest_digest)` |
| I5 | Bytes are never written to worker disk | Streaming copy only; no temp files ([05](05-transfer-engine.md) §4.3) |
| I6 | Package progress never exceeds 100% or moves backwards | Progress is a `SUM` over jobs, never a maintained counter |
| I7 | An illegal state transition errors rather than corrupting state | `Transition()` guard ([10](10-state-machines.md) §1) |
| I8 | Worker death requires no cleanup | Lease expiry + reaper ([04](04-queue-and-scheduling.md) §4) |
| I9 | Transferred bytes are exactly the vendor's bytes | Inline digest verification; no manifest rewriting ([05](05-transfer-engine.md) §4.4) |
