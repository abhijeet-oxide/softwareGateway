# 06 — Registry Abstraction

> **Consumed by:** [05](05-transfer-engine.md), [07](07-discovery.md), [08](08-verification.md)
> **Special status:** [ADR-001](16-technology-choices.md#adr-001) closed at M3 on `oras-go/v2`, for the write path only. **The interface in §2 was the contract both candidate backends had to satisfy**, and every other document is written against it rather than against a library — which is why the closure touched one directory and no document above this one. The interface remains the boundary: it is what keeps the decision reversible now that it is made.

---

## 1. Principles

1. **Generic OCI first.** All four required registries speak OCI Distribution v2. That is the implementation. Vendor-specific code exists only where a registry genuinely deviates.
2. **Narrow interface.** Only what the engine, discovery, and verification actually call. A wide interface is expensive to implement per backend and per registry, and most of it would go unused.
3. **No library types in the domain.** The interface speaks `go-digest` and our own descriptor type. Neither candidate library's types cross this boundary — that is what keeps ADR-001 a leaf decision.
4. **Cross-cutting concerns live in the transport, not in vendor code.** Authentication, CA bundles, proxies, rate limiting, and retries are configured once and apply to every implementation. A vendor implementation that had to re-implement rate limiting would be a design failure.

## 2. The interface

```go
package registry

// Descriptor is our own type, deliberately structurally similar to
// ocispec.Descriptor but owned by us, so that neither candidate library's
// types appear in domain packages. Each backend converts at its own boundary.
type Descriptor struct {
    MediaType    string
    Digest       digest.Digest
    Size         int64
    ArtifactType string            // OCI 1.1, when present
    Annotations  map[string]string
    Platform     *Platform         // nil for non-image artifacts
}

// Repository is a single OCI repository on a single registry. Implementations
// are safe for concurrent use: one instance is shared by all in-flight jobs
// against that repository, so the connection pool and token cache are shared.
type Repository interface {
    // ---- identity ----
    Name() string          // "registry.example.com/vendor-a/platform"
    Registry() string      // "registry.example.com"
    Path() string          // "vendor-a/platform"

    // ---- discovery (07) ----
    // ListTags returns tags in registry order, following Link-header
    // pagination. `last` resumes from a previous page; "" starts.
    ListTags(ctx context.Context, last string, limit int) (tags []string, next string, err error)

    // ---- manifests ----
    // ResolveTag returns the descriptor a tag currently points at, without
    // fetching the body. Used by discovery, which only needs the digest.
    ResolveTag(ctx context.Context, tag string) (Descriptor, error)

    // FetchManifest returns manifest bytes VERBATIM. Implementations must not
    // re-serialize: the digest -- and every signature over it -- is the hash of
    // these exact bytes. See 03 section 5 and 05 section 3.
    FetchManifest(ctx context.Context, ref string) (Descriptor, []byte, error)

    // PushManifest writes raw bytes. Idempotent: pushing an identical manifest
    // to the same digest is a no-op at the registry.
    PushManifest(ctx context.Context, ref string, mediaType string, raw []byte) (Descriptor, error)

    // Tag points a tag at an already-pushed manifest. Called last (invariant I1).
    Tag(ctx context.Context, dgst digest.Digest, tag string) error

    // ---- blobs ----
    // StatBlob reports whether a blob exists, without fetching it (HEAD).
    // ErrNotFound means absent; any other error means unknown.
    StatBlob(ctx context.Context, dgst digest.Digest) (Descriptor, error)

    // FetchBlob opens a stream. The caller MUST Close. Implementations must
    // not buffer the body (invariant I5).
    FetchBlob(ctx context.Context, dgst digest.Digest) (io.ReadCloser, error)

    // PushBlob consumes r and returns once the registry has committed the
    // blob. size must be exact; the registry verifies the digest on commit.
    // Implementations choose monolithic vs chunked per 05 section 4.6.
    PushBlob(ctx context.Context, dgst digest.Digest, size int64, r io.Reader) error

    // MountBlob attempts a cross-repository mount (05 section 4.2).
    // Returns ErrMountUnsupported or ErrMountDeclined when the caller should
    // fall back to streaming -- neither is a failure.
    MountBlob(ctx context.Context, dgst digest.Digest, fromRepo string) error

    // ResumeUpload continues a previously interrupted chunked upload.
    // ErrResumeUnsupported means restart the blob; always safe.
    ResumeUpload(ctx context.Context, state UploadState, r io.Reader) error

    // ---- referrers (08) ----
    // Referrers lists artifacts referring to subject -- cosign signatures,
    // SBOMs, attestations. Implementations fall back to the tag schema
    // (sha256-<hex>.sig) where the referrers API is unavailable.
    Referrers(ctx context.Context, subject digest.Digest, artifactType string) ([]Descriptor, error)

    // ---- capability & health ----
    Capabilities(ctx context.Context) Capabilities
    Ping(ctx context.Context) error       // backs transferctl health (13)
}
```

**Twenty methods, and each has a named caller.** `ListTags`/`ResolveTag` serve discovery; `FetchBlob`/`PushBlob`/`MountBlob`/`StatBlob` serve the transfer engine; `Referrers`/`FetchManifest` serve verification; `Ping` serves the health check. Nothing here exists speculatively.

> **On interface shape and ADR-001.** This is close to `oras-go`'s `registry.Repository`, which is not a coincidence — that library already found roughly the minimal useful surface for artifact-generic registry access, and there is no merit in inventing a different one. It is equally expressible over `go-containerregistry`, at the cost of a thin adapter, because GGCR's `remote` package is free functions rather than an interface. Deliberately modelling on the shape we might not choose is what keeps the decision reversible.

## 3. Capabilities

Registries differ in ways that change our behaviour but not our correctness. Probed once at startup, cached, and re-probed on configuration reload.

```go
type Capabilities struct {
    SupportsMount          bool  // cross-repo blob mount (05 section 4.2)
    SupportsChunkedUpload  bool  // PATCH with Range
    SupportsResumeUpload   bool  // session survives a client disconnect
    SupportsReferrersAPI   bool  // OCI 1.1 /v2/<name>/referrers/<digest>
    SupportsCatalog        bool  // /v2/_catalog
    TagPaginationStyle     PaginationStyle
    MaxChunkSize           int64
}
```

Probing is cheap and empirical: attempt the operation once against a known-absent digest and interpret the response. **We probe rather than hardcode by vendor** because the same product behaves differently across versions and storage backends — an Artifactory on S3 and one on filesystem differ, and hardcoding by `registry_type` would encode a lie that ages badly.

`SupportsResumeUpload` is the one capability we cannot fully determine by probing (it requires actually dropping a connection), so it starts at the vendor default and is corrected by observation: repeated resume failures against a registry flip it off and record `softwaregateway_upload_resume_total{result="unsupported"}` ([05](05-transfer-engine.md) §4.6).

## 4. Authentication

Every supported registry uses the OCI **token flow** (RFC 6750 bearer over the Distribution auth spec):

```
1. GET /v2/  ->  401 with WWW-Authenticate: Bearer realm=..,service=..,scope=..
2. GET <realm>?service=..&scope=repository:<name>:pull,push  (Basic credentials)
3. -> {"token": "...", "expires_in": 300}
4. Retry with Authorization: Bearer <token>
```

Implemented **once**, in the shared transport. Vendor differences are entirely in step 2's credential shape:

| Registry | Credential | Notes |
|---|---|---|
| Generic | username / password or token | Basic to the token endpoint |
| ACR | ACR refresh token, or SP/managed identity | AAD token exchanged at `/oauth2/exchange`, then `/oauth2/token` |
| Artifactory | username / API key or identity token | Standard flow; API key in the password field |
| Quay | robot account or OAuth token | Robot name is `org+robot` |

**Token caching**, keyed by `(registry, repository, scope)`, refreshed ~30 s before expiry, with single-flight so that sixteen concurrent jobs hitting an expiry do one refresh and not sixteen. As noted in [05](05-transfer-engine.md) §5, omitting this turns an 850-blob package into 850 token exchanges — a large, silent throughput loss and a reliable way to get rate-limited by the vendor.

Credentials come from mounted Secret files ([02](02-configuration.md) §5.5) and are wrapped in a redacting type so they cannot leak through `%v`.

## 5. Shared transport

One `http.Client` per repository, built from product configuration, carrying every cross-cutting concern so no implementation re-invents them:

```
        Repository implementation (generic / acr / artifactory / quay)
                              │
        ┌─────────────────────▼─────────────────────┐
        │  rate limiter    token bucket, per repo   │  02 section 5.3
        ├───────────────────────────────────────────┤
        │  concurrency     semaphore, granted       │  05 section 8
        ├───────────────────────────────────────────┤
        │  retry           backoff + jitter,        │  04 section 11
        │                  Retry-After honoured     │
        ├───────────────────────────────────────────┤
        │  auth            token cache, refresh     │  section 4
        ├───────────────────────────────────────────┤
        │  observability   metrics, tracing, logs   │  12
        ├───────────────────────────────────────────┤
        │  http.Transport  h1 forced, no compression│  05 section 5
        │                  product CA pool, proxy   │  02 section 4
        └───────────────────────────────────────────┘
```

**Ordering matters and is deliberate.** The rate limiter is outermost so that *retries are rate-limited too* — otherwise a burst of retries against a struggling registry bypasses the very limit meant to protect it, which is precisely how a transient error becomes an outage.

**One stack per SOURCE, not per repository.** The pool, the rate limiter and the token cache belong to the source; only the retry and auth layers are built per repository, and auth only because it derives its token scope from the repository path.

This was originally per repository, and the bug it caused is worth recording because it was invisible from the configuration. Every repository built its own everything, so the configured ceilings were multiplied by however many repositories were being scanned at once: `maxConnections: 32` across sixteen parallel repositories permitted **512 concurrent connections to one host**, and `requestsPerSecond: 50` permitted **800**. Through a corporate proxy that is not a faster scan, it is a self-inflicted overload — and a document that says 32 while the process opens 512 is worse than one that says nothing.

Sharing also buys one token exchange per source instead of one per repository, and warm keep-alives across repositories rather than a fresh TLS handshake for each.

**Tracing sits outside everything.** The trace layer wraps the rate limiter, so the duration it reports is the cost the CALLER paid — including any wait for a rate-limit token and any retry backoff. A timer inside those layers would report a healthy 200 ms for a request that cost the scanner thirty seconds, which is exactly the lie that makes this class of problem hard to find. Failures and requests slower than ten seconds are logged at WARN regardless of level; everything else is DEBUG.

`Retry-After` is honoured when present. A registry telling us how long to wait is better information than our backoff formula, and ignoring it is a good way to get an IP blocked.

**TLS lives at the bottom of the stack, and has two knobs.** `network.caBundleRef` appends a private CA to the system pool — appends, not replaces, because a product that adds an internal CA still needs to reach public registries and Sigstore. `network.tls.insecureSkipVerify` turns verification off entirely for one repository; it is opt-in, logged on every reload, and reported by `products check`. Neither of them fixes `x509: negative serial number`, which fails while parsing the certificate before any verification runs — see [02 §TLS](02-configuration.md#tls-two-different-failures-two-different-fixes).

## 6. Registry implementations

### 6.1 Generic (default)

Plain OCI Distribution v2. **This is the expected path for all four registries** and should handle the overwhelming majority of operations.

### 6.2 Azure Container Registry

Deltas from generic:

- **Auth.** With a service principal or managed identity, exchange an AAD token at `POST /oauth2/exchange` for an ACR refresh token, then obtain scoped access tokens at `/oauth2/token`. With admin credentials, the generic flow works unchanged.
- **Catalog.** `_catalog` is supported but paginates differently under load; we use configured repository paths for discovery rather than catalog enumeration ([07](07-discovery.md) §2), so this rarely matters.
- **Mount.** Supported within a registry — the main reason lab→production promotion within one ACR is near-instant ([05](05-transfer-engine.md) §6).
- **Throttling.** ACR returns `429` with `Retry-After` under sustained load. Honoured by the shared transport (§5), and the primary signal for the adaptive controller ([11](11-resiliency-and-backpressure.md) §3).

### 6.3 JFrog Artifactory

- **Repository paths** are prefixed by the Artifactory repository key (`docker-remote/vendor-a/platform`). Handled as configuration, not code — `repository:` in the product YAML carries the full path.
- **Tag pagination** historically deviates from the `Link`-header convention on some versions; the `TagPaginationStyle` capability selects the right strategy.
- **Referrers API** availability depends on version; the tag-schema fallback covers older deployments ([08](08-verification.md) §3).
- **Virtual repositories** may resolve reads and writes to different backing repositories, which can make a just-pushed blob briefly invisible to `StatBlob`. Because our fast path is "check, then transfer", a false negative costs a redundant transfer and never corruption.

### 6.4 Quay

- **Robot accounts** use `org+robotname` as the username. No code difference, but a documentation trap worth naming.
- **Rate-limit headers** are informative and feed the adaptive controller.
- **Mount** support varies by deployment (quay.io vs Project Quay); probed, not assumed.
- **Quay has replication mechanisms of its own** — repository mirroring and proxy cache — and they are reached over the **management API (`/api/v1`)** rather than the distribution API (`/v2`). Two protocols on one host, so two clients: this `Repository` implementation stays generic, and `internal/registry/quay` holds the management client. A target may therefore declare `replication.mode: copy | mirror | proxy`, which changes *who moves the bytes* and consequently what the system can promise about them. Specified in [18](18-quay-replication.md); nothing above this bullet changes for a `copy`-mode Quay target, which is the default.

### 6.5 Adding a fifth registry

The whole point of the abstraction. Concretely:

```go
// internal/registry/harbor/harbor.go
package harbor

type Repository struct{ *generic.Repository }   // embed; override only deltas

func New(cfg registry.Config) (registry.Repository, error) {
    base, err := generic.New(cfg)
    if err != nil {
        return nil, err
    }
    return &Repository{Repository: base}, nil
}

// Only if it actually differs. If nothing differs, this file is 10 lines.
func (r *Repository) Referrers(ctx context.Context, subj digest.Digest, at string) ([]registry.Descriptor, error) {
    // ... registry-specific behaviour ...
}
```

```go
// internal/registry/factory.go
func init() { Register("harbor", harbor.New) }
```

Plus `harbor` in the `registry_type` `CHECK` constraint ([03](03-persistence.md) §4) and in the config enum ([02](02-configuration.md) §5.2). **Embedding the generic implementation and overriding only real deviations** is what keeps vendor code proportional to actual vendor differences — most registries need nothing but the constructor.

## 7. Errors

Registry errors are classified once, at the boundary, into a small set the rest of the system reasons about. Retry policy ([11](11-resiliency-and-backpressure.md) §2.3) keys off the class, never off an HTTP status scattered through the codebase.

| Sentinel | Typical cause | Retryable |
|---|---|---|
| `ErrNotFound` | 404, `BLOB_UNKNOWN`, `MANIFEST_UNKNOWN` | Context-dependent — a `404` on `StatBlob` is a normal negative answer, not an error |
| `ErrUnauthorized` | 401 | No — 1 attempt |
| `ErrForbidden` | 403 | No — 1 attempt |
| `ErrRateLimited` | 429 | Yes, honouring `Retry-After` |
| `ErrUnavailable` | 500/502/503/504 | Yes, full backoff |
| `ErrTimeout` | Connect, header, or stall timeout | Yes |
| `ErrDigestMismatch` | Content did not hash as claimed | Yes, capped at 2 ([05](05-transfer-engine.md) §4.4) |
| `ErrMountUnsupported` / `ErrMountDeclined` | Registry cannot or will not mount | **Not an error** — fall through to streaming |
| `ErrResumeUnsupported` | Session not retained | **Not an error** — restart the blob |
| `ErrUnsupported` | Capability absent | No |

The last three are the interesting ones: they are *expected outcomes on an optimization path*, and modelling them as failures would turn a normal fallback into a retry storm and a red dashboard.

## 8. Testing

- **In-memory OCI registry** for unit tests of the engine — fast, hermetic, no Docker. Availability of a ready-made one differs between the ADR-001 candidates and is a scored criterion ([16](16-technology-choices.md#adr-001)); if the chosen library does not ship one, we run the reference `distribution` registry via `testcontainers-go` and accept the slower loop.
- **Conformance suite** per implementation, run against real registries in a nightly job rather than in PR CI: push, pull, mount, resume, referrers, pagination, 429 handling. This is how capability assumptions (§3) are validated against reality instead of against documentation.
- **Fault injection** at the transport layer — mid-body disconnects, `429` bursts, slow-loris responses, digest corruption — driving the recovery paths in [11](11-resiliency-and-backpressure.md).
