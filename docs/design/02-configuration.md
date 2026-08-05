# 02 — Configuration

> **Prerequisite:** [01 — Domain Model](01-domain-model.md) · **Consumed by:** [06](06-registry-abstraction.md), [07](07-discovery.md), [08](08-verification.md), [11](11-resiliency-and-backpressure.md), [14](14-deployment-and-development.md)

---

## 1. Principles

1. **One product, one ConfigMap, one YAML document.** Everything about a product is in one place. No cross-file references, no inheritance chains, no overlays that change meaning.
2. **Declarative and GitOps-native.** Flux applies it. Git is the source of truth. Nothing about a product's *configuration* is mutable through the API — the API creates transfer requests, not config.
3. **Secrets by reference, never by value.** VSO materializes Kubernetes Secrets; config names them.
4. **Schema-versioned.** Every document carries `apiVersion`/`kind` so it can be validated and migrated, even though these are not CRDs.

## 2. Why ConfigMaps and not CRDs

> **Decision — Product configuration is a ConfigMap, not a Custom Resource Definition.**
>
> *Alternative considered:* define `Product` as a CRD with an OpenAPI schema and a controller-runtime reconciler. This buys server-side schema validation, `kubectl get products`, status subresources, and admission-time rejection of bad config.
>
> *Rejected because* it requires controller-runtime, a CRD lifecycle to install and version, RBAC for a custom API group, and a reconciler loop whose job — "make the cluster match Git" — is precisely what Flux already does. We would be writing a second GitOps engine to sit behind the first one.
>
> *What we lose and how we compensate:*
> | Lost | Compensation |
> |---|---|
> | Admission-time validation | Validation at load, fail-closed per product (§7), plus a `transferctl config validate` that runs the same validator in CI before merge |
> | `kubectl get products` | `transferctl products list`, which shows live state the CRD status field would only approximate |
> | Status subresource | Product health is in `/readyz` detail, metrics, and the API |
>
> *Migration path if this proves wrong:* the YAML already carries `apiVersion: softwaregateway.io/v1alpha1` and `kind: Product`, so the document body becomes a CRD `spec` unchanged. The loader gains a second source; nothing else in the system moves.

## 3. Where configuration comes from

Products are read from a **directory of YAML files**, not from the Kubernetes API.

```
/etc/softwaregateway/products/     ← projected volume: one ConfigMap per product
    vendor-a-platform.yaml
    vendor-b-database.yaml
/etc/softwaregateway/secrets/      ← projected volume: VSO-managed Secrets
    vendor-a-registry/{username,password}
    internal-registry/{username,password}
/etc/softwaregateway/config.yaml   ← system config (§8)
```

> **Decision — configuration and secrets are read from mounted volumes with `fsnotify`, not through the Kubernetes API.**
>
> *Alternative:* a client-go informer watching ConfigMaps and Secrets.
>
> *Chosen:* volume mounts. No client-go dependency, no RBAC to grant (in particular, no cluster-wide Secret read permission), no API-server load, and — the reason that matters most day to day — **the exact same code path works in local development against a plain directory**. A developer copies a YAML file into a folder and the Coordinator picks it up, with no cluster and no mocking.
>
> *Cost accepted:* kubelet propagates ConfigMap and Secret updates on a refresh cycle (typically ~60 s, and not at all for `subPath` mounts — so we do not use `subPath`). Config changes therefore take up to a minute to apply rather than being instantaneous. For GitOps-managed configuration that is irrelevant; Flux reconciliation is slower than that anyway. VSO credential rotation propagates through the same mechanism.

## 4. Complete Product example

Annotated, exhaustive, and intended to be copied. Every field the system understands appears here.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: product-vendor-a-platform
  namespace: softwaregateway
  labels:
    softwaregateway.io/config: product      # how the loader finds it, if watching the API
data:
  product.yaml: |
    apiVersion: softwaregateway.io/v1alpha1
    kind: Product
    metadata:
      # Resource ID: lowercase alphanumeric and hyphens, <= 63 chars (AIP-122).
      # Immutable. Appears in API paths, metric labels, and audit records.
      name: vendor-a-platform
      displayName: "Vendor A Platform Suite"
      description: "Core platform, shipped quarterly."
      owner: platform-infra@example.com
      labels:
        tier: critical
        vendor: vendor-a

    spec:
      # ─────────────────────────────────────────────────────────────
      # SOURCES — vendor-side, read-only. Polled by discovery.
      # ─────────────────────────────────────────────────────────────
      sources:
        - name: primary
          registry: registry.vendor-a.example.com
          repository: platform/suite
          credentialsRef:
            secretName: vendor-a-registry     # VSO-managed
            usernameKey: username             # default: username
            passwordKey: password             # default: password
          # Optional: anonymous access
          # anonymous: true

          discovery:
            enabled: true
            interval: 15m                     # per-source poll interval
            # Only tags matching these are recorded at all. Applied before
            # autoDownload rules. Omit to record every tag.
            tagFilters:
              include: ['^v\d+\.\d+\.\d+$']   # RE2 syntax (Go regexp)
              exclude: ['-(rc|beta|alpha)\.']

          rateLimits:
            maxConcurrentDownloads: 16        # in-flight blob GETs
            maxConcurrentUploads: 0           # sources are read-only
            maxConnections: 32                # transport pool ceiling
            requestsPerSecond: 50             # token bucket; 0 = unlimited
            burst: 100

        - name: mirror
          registry: registry-eu.vendor-a.example.com
          repository: platform/suite
          credentialsRef:
            secretName: vendor-a-registry
          discovery:
            enabled: false                    # failover only; do not double-discover
          rateLimits:
            maxConcurrentDownloads: 8
            maxConnections: 16
            requestsPerSecond: 25

      # ─────────────────────────────────────────────────────────────
      # TARGETS — internal, read-write. Replication destinations and
      # promotion endpoints (both directions; see 01 §2.1).
      # ─────────────────────────────────────────────────────────────
      targets:
        - name: lab
          registry: internal.azurecr.io
          repository: vendor-a/platform
          type: acr                           # generic | acr | artifactory | quay
          credentialsRef:
            secretName: internal-acr
          rateLimits:
            maxConcurrentUploads: 24
            maxConcurrentDownloads: 24        # non-zero: targets are promotion sources
            maxConnections: 64
            requestsPerSecond: 200
            burst: 400
          default: true                       # used when a request names no target

        - name: production
          registry: internal.azurecr.io       # same registry as lab =>
          repository: vendor-a/platform-prod  # cross-repo mount applies (05 §4.2)
          type: acr
          credentialsRef:
            secretName: internal-acr
          rateLimits:
            maxConcurrentUploads: 12          # deliberately conservative
            maxConcurrentDownloads: 12
            maxConnections: 32
            requestsPerSecond: 100
          # Promotion-only: replication may not target this directly.
          promotionOnly: true

      # ─────────────────────────────────────────────────────────────
      # AUTO-DOWNLOAD — evaluated on each newly discovered package,
      # in order; first match wins. See 07 §4.
      # ─────────────────────────────────────────────────────────────
      autoDownload:
        enabled: true
        rules:
          - name: ga-releases
            tagPattern: '^v\d+\.\d+\.\d+$'    # RE2
            targets: [lab]
            priority: 100                     # 0-1000, higher first (04 §6)
            verifyBeforeTransfer: true

          - name: release-candidates
            tagPattern: '^v\d+\.\d+\.\d+-rc\.\d+$'
            targets: [lab]
            priority: 10

      # ─────────────────────────────────────────────────────────────
      # VERIFICATION — cosign/sigstore. See 08.
      # ─────────────────────────────────────────────────────────────
      verification:
        enabled: true
        # enforce: verification failure blocks/fails the transfer
        # warn:    record the failure, notify, continue
        policy: enforce

        atSource: true                        # before transferring
        atDestination: true                   # after transferring
        transferSignatures: true              # copy signature artifacts too,
                                              # so the destination is independently
                                              # verifiable (08 §5)
        cosign:
          mode: keyless                       # keyless | key
          keyless:
            certificateIdentity: 'https://github.com/vendor-a/platform/.github/workflows/release.yaml@refs/heads/main'
            certificateOidcIssuer: 'https://token.actions.githubusercontent.com'
            # Optional overrides for air-gapped or private Sigstore
            # rekorPublicKeysRef: {secretName: sigstore-roots, key: rekor.pub}
            # fulcioCertsRef:     {secretName: sigstore-roots, key: fulcio.crt}
          # mode: key
          # key:
          #   publicKeyRef: {secretName: vendor-a-cosign, key: cosign.pub}

      # ─────────────────────────────────────────────────────────────
      # NOTIFICATIONS — recipients per event type. See 12 §5.
      # ─────────────────────────────────────────────────────────────
      notifications:
        enabled: true
        channels:
          - name: platform-email
            type: email
            email:
              recipients:
                - platform-infra@example.com
                - release-managers@example.com

          - name: platform-teams
            type: teams
            teams:
              # Power Automate workflow URL. NOT a legacy O365 connector
              # webhook -- those are retired. See 16.
              webhookUrlRef:
                secretName: teams-webhooks
                key: platform-channel

        # Which events go where. Unlisted events are not notified.
        subscriptions:
          - events: [PackageDiscovered]
            channels: [platform-teams]
          - events: [TransferCompleted, PromotionCompleted]
            channels: [platform-teams]
          - events: [TransferFailed, VerificationFailed]
            channels: [platform-teams, platform-email]

      # ─────────────────────────────────────────────────────────────
      # NETWORK — applies to every repository in this product unless
      # overridden per-repository. See 06 §5.
      # ─────────────────────────────────────────────────────────────
      network:
        # Additional trusted CAs, appended to the system pool.
        caBundleRef:
          secretName: vendor-a-ca
          key: ca.crt
        proxy:
          httpsProxy: http://proxy.internal.example.com:3128
          noProxy: ['internal.azurecr.io', '.svc.cluster.local']
        # Per-request timeouts. Blob transfers are governed by a stall
        # detector rather than a total deadline -- a 40 GB blob is not
        # slow, it is large. See 05 §5.
        timeouts:
          connect: 10s
          responseHeader: 30s
          idleStall: 60s

      # ─────────────────────────────────────────────────────────────
      # RETENTION — overrides system defaults for this product. See 03 §8.
      # ─────────────────────────────────────────────────────────────
      retention:
        completedJobs: 168h        # 7d
        discoveryHistory: 2160h    # 90d
        notificationHistory: 720h  # 30d
        auditHistory: 8760h        # 365d
```

## 5. Field reference

Types, defaults, and validation rules. Validation is enforced at load (§7) and by `transferctl config validate`.

### 5.1 `metadata`

| Field | Type | Required | Rules |
|---|---|---|---|
| `name` | string | yes | `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`, ≤63 chars, unique, immutable (AIP-122) |
| `displayName` | string | no | ≤200 chars, free text |
| `description` | string | no | ≤2000 chars |
| `owner` | string | no | Email; used as notification fallback |
| `labels` | map | no | Keys/values ≤63 chars. **Not** propagated to metric labels — see [12](12-observability-and-audit.md) §2.1 on cardinality |

### 5.2 `sources[]` and `targets[]`

| Field | Type | Required | Default | Rules |
|---|---|---|---|---|
| `name` | string | yes | — | Unique within its list; same charset as product name |
| `registry` | string | yes | — | Host, optional port. No scheme — HTTPS assumed |
| `repository` | string | yes | — | Repository path, no tag or digest |
| `type` | enum | no | `generic` | `generic`, `acr`, `artifactory`, `quay` ([06](06-registry-abstraction.md)) |
| `anonymous` | bool | no | `false` | Mutually exclusive with `credentialsRef` |
| `credentialsRef` | object | conditional | — | Required unless `anonymous` |
| `rateLimits` | object | no | see §5.3 | |
| `network` | object | no | inherits product | Same shape as `spec.network` |
| `default` (targets) | bool | no | `false` | At most one per product |
| `promotionOnly` (targets) | bool | no | `false` | Rejects replication requests naming this target |
| `discovery` (sources) | object | no | `enabled: true` | |

### 5.3 `rateLimits`

Per repository, independently configurable, exactly as required. These are **ceilings**; the adaptive controller ([11](11-resiliency-and-backpressure.md) §3) operates strictly within them and may run lower.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `maxConcurrentDownloads` | int | 8 | In-flight blob GETs against this repository, fleet-wide |
| `maxConcurrentUploads` | int | 8 | In-flight blob pushes, fleet-wide |
| `maxConnections` | int | 32 | Transport pool ceiling per worker |
| `requestsPerSecond` | int | 0 | Token-bucket rate; `0` = unlimited |
| `burst` | int | 2×rps | Token-bucket burst |

**Fleet-wide, not per-worker.** The Coordinator divides the budget across active workers and ships each worker its share in the lease response. This is why concurrency limits are meaningful even as the worker count changes under HPA — a per-worker limit would silently multiply by the replica count and flatten the vendor's registry the moment HPA scaled out.

### 5.4 `autoDownload.rules[]`

Evaluated in order against each newly discovered package; **first match wins**, remaining rules are skipped. Ordered-first-match rather than all-match, because two rules matching the same tag with different priorities has no sensible interpretation.

| Field | Type | Required | Default | Rules |
|---|---|---|---|---|
| `name` | string | yes | — | Unique within the product; appears in audit records |
| `tagPattern` | string | yes | — | RE2 (Go `regexp`). Rejected at load if it does not compile |
| `targets` | []string | no | the `default` target | Must name declared, non-`promotionOnly` targets |
| `priority` | int | no | 50 | 0–1000 ([04](04-queue-and-scheduling.md) §6) |
| `verifyBeforeTransfer` | bool | no | product setting | Per-rule override |

> **On regex safety:** Go's `regexp` is RE2 — linear time, no backtracking, so a pathological pattern cannot hang discovery. This is a genuine reason to state the dialect explicitly rather than saying "regular expression": with PCRE, a user-supplied pattern in a polling loop would be a denial-of-service vector.

### 5.5 `credentialsRef` and other secret references

```yaml
credentialsRef:
  secretName: vendor-a-registry   # required; a Kubernetes Secret name
  usernameKey: username           # default: username
  passwordKey: password           # default: password
```

Resolved to `/etc/softwaregateway/secrets/<secretName>/<key>`. Secret **values never appear** in config, logs, API responses, audit records, or error messages. The loader wraps credentials in a type whose `String()` returns `[REDACTED]`, so an accidental `%v` cannot leak one — a defence worth having because that mistake is otherwise a matter of time.

Bearer-token registries (and identity-token flows such as ACR) use the same shape with `passwordKey` holding the token; see [06](06-registry-abstraction.md) §4.

## 6. Configuration reload

1. `fsnotify` watches the products directory.
2. On change (debounced 2 s — kubelet writes are not atomic across files), reload **all** products.
3. Validate each independently.
4. Atomically swap the in-memory registry of valid products.
5. Emit `ConfigReloaded` audit events; update `softwaregateway_config_products_loaded` and `..._config_load_errors`.

**What reloads take effect immediately:** discovery intervals and filters, rate limits, auto-download rules, notification routing, verification policy, retention.

**What does not:** in-flight transfers keep the repository settings captured at plan time. Changing a target's registry hostname mid-transfer would otherwise mean a package half-written to one registry and half to another. New transfers pick up the new configuration.

**Removing a product** stops its discovery and rejects new requests for it. It does **not** cancel in-flight transfers or delete history — deletion of running work is never a side effect of a config edit. `transferctl` reports such transfers as belonging to an unconfigured product.

## 7. Validation and failure behaviour

**Fail closed per product, stay up overall.** A syntax error in `vendor-b.yaml` must never stop `vendor-a` from replicating.

On load, each product is validated for: schema conformance, unique names, resolvable secret references, compiling regexes, referenced targets existing, `promotionOnly` not named in auto-download rules, sane rate limits (non-negative; `burst >= rps` when rps > 0), and parseable durations.

| Outcome | Behaviour |
|---|---|
| Product valid | Loaded; replaces the previous version |
| Product invalid | **Previous valid version is retained**; error logged; `config_load_errors` incremented; surfaced in `/readyz` detail and `transferctl health` |
| Product invalid on first load | Not loaded; product unavailable; API returns `404` with a problem detail explaining the config error |
| **All** products invalid | Coordinator stays up and serves the API. `/readyz` fails, so it leaves rotation, but it does not crash-loop |

The last row is deliberate. A crash-looping Coordinator cannot tell anyone *why* it is unhappy; a running one with a failing readiness probe and a clear error can.

## 8. System configuration

Deployment-scoped settings, separate from products because they are operator concerns rather than product concerns. Precedence: **CLI flag → environment variable (`SWGW_` prefix) → config file → default.**

```yaml
# /etc/softwaregateway/config.yaml
apiVersion: softwaregateway.io/v1alpha1
kind: SystemConfig

server:
  address: :8080
  shutdownGracePeriod: 30s

database:
  driver: postgres                 # postgres | sqlite
  dsn: ${SWGW_DATABASE_DSN}        # env expansion; never a literal here
  maxOpenConns: 25
  maxIdleConns: 10
  connMaxLifetime: 1h

coordinator:
  leaderElection:
    enabled: true
    lockID: 1                      # pg advisory lock key (04 §9)
  scheduler:
    tickInterval: 10s              # scheduled-request due check
  reaper:
    tickInterval: 30s              # expired-lease sweep
    leaseDuration: 2m
  queue:
    maxLeaseBatchSize: 32
  gc:
    tickInterval: 1h
    batchSize: 5000                # bounded per tick; GC never stalls transfers

worker:
  coordinatorEndpoint: http://coordinator.softwaregateway.svc:8080
  workerID: ${POD_NAME}            # defaults to hostname
  maxConcurrentJobs: 16            # local ceiling; the Coordinator may grant less
  copyBufferSize: 1MiB             # memory ceiling = maxConcurrentJobs x this (05 §4.5)
  heartbeatInterval: 20s

notifications:
  email:
    host: smtp.internal.example.com
    port: 587
    from: softwaregateway@example.com
    credentialsRef: {secretName: smtp-credentials}
  outbox:
    tickInterval: 15s
    maxAttempts: 5

observability:
  log:
    level: info
    format: json
  metrics:
    enabled: true
    path: /metrics
  tracing:
    enabled: true
    endpoint: otel-collector.observability.svc:4317
    sampleRatio: 0.05

retention:                          # defaults; products may override (§4)
  completedJobs: 168h
  queueHistory: 168h
  discoveryHistory: 2160h
  notificationHistory: 720h
  auditHistory: 8760h

tls:                                # PROCESS-WIDE X.509 relaxations. See below.
  allowNegativeSerialNumbers: false # the only fix for "x509: negative serial number"
```

`tls` sits here rather than under a product's `network` block because it is implemented with Go's `GODEBUG` mechanism, which is per process and cannot be scoped to one connection. It applies to the process that reads it — set it on the Coordinator *and* the Worker, or discovery succeeds and every transfer fails at the handshake. Both binaries log a warning at startup saying so.

## 9. Development

The identical loader reads a plain directory, so local development needs no cluster and no Kubernetes objects:

```
./dev/
  config.yaml
  products/vendor-a-platform.yaml
  secrets/vendor-a-registry/{username,password}   # plain files
```

```bash
SWGW_CONFIG_DIR=./dev go run ./cmd/coordinator
```

with `database.driver: sqlite` as the development default — zero setup. See [14](14-deployment-and-development.md) §5.


---

## Enablement

Every level of the document has an `enabled` switch, defaulting to **true** so a document that says nothing is on.

| Field | Effect |
|---|---|
| `metadata.enabled` | the whole product runs or does not |
| `spec.sources[].enabled` | the source exists or does not |
| `spec.sources[].discovery.enabled` | the source is polled or not — it still exists |
| `spec.targets[].enabled` | the target receives transfers or does not |

> **Decision — disable rather than delete.**
>
> *Alternative:* remove the document to pause a product, which is what the design originally required.
>
> *Rejected because* deleting loses exactly what you most want back: the registries, credentials, filters and rules that were working. Re-creating them from memory during an incident is how a "temporary" pause becomes a subtly different configuration — and the difference is usually discovered later, by a package that quietly stopped being replicated.
>
> *What a disabled product still does:* it loads, it VALIDATES, and it appears in `products list` marked `DISABLED`. Validating it is the point — a mistake is reported now rather than found on the day someone re-enables it. Its `products` and `repositories` rows are DEACTIVATED, never deleted, so the packages and transfer history referencing them survive ([03](03-persistence.md) §4).
>
> *What would change our mind:* nothing likely. The cost is one field and a state column.

**`enabled` versus `discovery.enabled` on a source** is a real distinction, not a duplicate. `enabled: false` removes the source from every purpose; `discovery.enabled: false` stops the polling while leaving the source usable for an explicit transfer request — which is what a failover mirror needs, since it must stay reachable but must not double-discover every tag the primary already found.

Validation rejects the combinations that would otherwise fail silently:

- every source disabled while the product is enabled — the product would discover nothing while appearing active
- `autoDownload` enabled with no enabled target — rules would match and have nowhere to send
- an auto-download rule naming a disabled target — this one fails the first time a package matches it, potentially weeks after the edit

## Inheritance

Three blocks are declared at product level and overridden per repository. The merge rule differs by block, and the difference is deliberate.

| Block | Where | Merge |
|---|---|---|
| `network.caBundleRef` | product, source, target | set wins, unset inherits |
| `network.proxy` | product, source, target | set wins; `direct: true` clears everything inherited |
| `network.tls.insecureSkipVerify` | product, source, target | **three-state**: absent inherits, `true` and `false` both win |
| `network.timeouts` | product, source, target | field by field |
| `verification` scalars | product, source, target | field by field |
| `verification.cosign` | product, source, target | **replaces wholesale** |

**Why `cosign` is atomic.** It is one coherent trust decision — a mode plus the identity or key that mode requires. Merging field by field would silently produce combinations nobody wrote: a product's keyless `certificateIdentity` paired with a repository's `key` mode, or a Fulcio issuer left over from a block that no longer applies. A trust configuration assembled from two documents is one nobody can audit, and auditability is the entire point of verification.

**Why `proxy.direct` exists.** A product-level proxy is inherited, and "everything through the corporate proxy except this one internal registry" is the normal shape. Without an explicit switch the only way to express it is to repeat the registry's own hostname in `noProxy` at every level — which works, and is easy to get subtly wrong when the host has a port or an alias. `direct: true` also bypasses `HTTPS_PROXY` from the environment: a repository that asked to go direct means it, and honouring a cluster-wide setting anyway would make the option a no-op in precisely the deployment that needs it.

**Why `insecureSkipVerify` is three-state.** Every other override is "set wins, unset inherits", which for a boolean cannot distinguish an omitted field from an explicit `false`. That distinction matters here and nowhere else: a product-level `insecureSkipVerify: true` is inherited by every source and target, and without a way to write `false` a repository with a perfectly good certificate could never opt back into verification. So the field is a pointer, and the two states are different questions — "say nothing" versus "verify, whatever the level above says".

---

## TLS: two different failures, two different fixes

An earlier revision of `internal/registry/transport` said `InsecureSkipVerify` would never exist, on the grounds that a CA bundle is the correct fix. That was too strong, and it is recorded here as a correction rather than quietly reversed. `caBundleRef` fixes an **untrusted chain**. It does nothing for a certificate that is expired, carries the wrong hostname, or belongs to a registry mid-migration — and an operator who has to move bytes past one of those today should not have to patch the binary.

So `network.tls.insecureSkipVerify` exists, per repository, with three things attached to make it hard to forget: a `WARN` line naming the product and source on every configuration reload, a `certificate verification  WARNING` step in `transferctl products check`, and a validation error if it appears alongside `caBundleRef` in the same block — where the bundle would never be consulted and is therefore dead configuration that reads as if it verifies.

### What it does not fix

It does not fix this:

```
tls: failed to parse certificate from server: x509: negative serial number
```

Measured on Go 1.25.7, not reasoned about — `internal/platform/tlscompat` carries the test, and it asserts the failure so that a future Go release quietly changing the behaviour breaks the build rather than the documentation:

| client | result |
|---|---|
| default | `x509: negative serial number` |
| `InsecureSkipVerify: true` | **the identical error** |
| `GODEBUG=x509negativeserial=1` | connects, verification fully on |

`crypto/x509` has rejected negative serial numbers since Go 1.23, and rejects them while **parsing** — before verification runs. `InsecureSkipVerify` disables a step that is never reached. Shipping it as the fix for this error would have been shipping something that looks like a fix.

### Where the real fix lives, and why

`tls.allowNegativeSerialNumbers`, in **system** configuration, applied by `internal/platform/tlscompat` at process start in both the Coordinator and the Worker.

Not under `network.tls`, and the reason is the whole point of the split: it is implemented with Go's `GODEBUG` mechanism, which is per process. It cannot be scoped to one connection. Putting it beside `insecureSkipVerify` would have made it look per repository when it relaxes X.509 parsing for every registry, every Sigstore call and every other outbound connection the process makes. A setting whose blast radius the schema misrepresents is worse than one that is merely inconvenient to find.

Two consequences follow from it being process-wide:

- **Both binaries need it.** The Coordinator discovers, the Worker moves bytes. Setting it on one gives a product that discovers cleanly and fails every transfer at the handshake.
- **The existing `GODEBUG` is preserved, not replaced.** Container images and Deployments commonly set it already. Clobbering it would silently undo whatever it was doing, and nobody would connect the breakage back to this setting.

RFC 5280 §4.1.2.2 requires a positive serial number, so such a certificate is genuinely malformed — typically an appliance or enterprise CA encoding a random 20-byte value without clearing the high bit. The certificate is otherwise sound and, with the relaxation on, is verified normally. The setting is an escape hatch for an estate that cannot be reissued on our schedule, not an endorsement.
