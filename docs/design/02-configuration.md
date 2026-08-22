# 02 - Configuration

> **Prerequisite:** [01 - Domain Model](01-domain-model.md) · **Consumed by:** [06](06-registry-abstraction.md), [07](07-discovery.md), [08](08-verification.md), [11](11-resiliency-and-backpressure.md), [14](14-deployment-and-development.md)

---

## 1. Principles

1. **One product, one ConfigMap, one YAML document.** Everything about a product is in one place. No cross-file references, no inheritance chains, no overlays that change meaning.
2. **Declarative and GitOps-native.** Flux applies it. Git is the source of truth. Nothing about a product's *configuration* is mutable through the API - the API creates transfer requests, not config.
3. **Secrets by reference, never by value.** VSO materializes Kubernetes Secrets; config names them.
4. **Schema-versioned.** Every document carries `apiVersion`/`kind` so it can be validated and migrated, even though these are not CRDs.

## 2. Why ConfigMaps and not CRDs

> **Decision - Product configuration is a ConfigMap, not a Custom Resource Definition.**
>
> *Alternative considered:* define `Product` as a CRD with an OpenAPI schema and a controller-runtime reconciler. This buys server-side schema validation, `kubectl get products`, status subresources, and admission-time rejection of bad config.
>
> *Rejected because* it requires controller-runtime, a CRD lifecycle to install and version, RBAC for a custom API group, and a reconciler loop whose job - "make the cluster match Git" - is precisely what Flux already does. We would be writing a second GitOps engine to sit behind the first one.
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
    vendor-platform.yaml
    vendor-database.yaml
/etc/softwaregateway/secrets/      ← projected volume: VSO-managed Secrets
    vendor-registry/{username,password}
    internal-registry/{username,password}
/etc/softwaregateway/config.yaml   ← system config (§8)
```

> **Decision - configuration and secrets are read from mounted volumes with `fsnotify`, not through the Kubernetes API.**
>
> *Alternative:* a client-go informer watching ConfigMaps and Secrets.
>
> *Chosen:* volume mounts. No client-go dependency, no RBAC to grant (in particular, no cluster-wide Secret read permission), no API-server load, and - the reason that matters most day to day - **the exact same code path works in local development against a plain directory**. A developer copies a YAML file into a folder and the Coordinator picks it up, with no cluster and no mocking.
>
> *Cost accepted:* kubelet propagates ConfigMap and Secret updates on a refresh cycle (typically ~60 s, and not at all for `subPath` mounts - so we do not use `subPath`). Config changes therefore take up to a minute to apply rather than being instantaneous. For GitOps-managed configuration that is irrelevant; Flux reconciliation is slower than that anyway. VSO credential rotation propagates through the same mechanism.

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
      # SOURCES - vendor-side, read-only. Polled by discovery.
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

          # Usually ABSENT: the application-level `concurrency` is inherited.
          # Spelled out here because this vendor asked us to stay under 50 rps.
          concurrency:
            perRegistry: 32                   # in flight, and the pool size
            requestsPerSecond: 50             # token bucket; 0 = unlimited

        - name: mirror
          registry: registry-eu.vendor-a.example.com
          repository: platform/suite
          credentialsRef:
            secretName: vendor-a-registry
          discovery:
            enabled: false                    # failover only; do not double-discover
          concurrency:
            perRegistry: 16
            requestsPerSecond: 25

        - name: jfrog-store
          registry: acme.jfrog.io
          repository: docker-local/platform/suite
          type: jfrog                         # or `artifactory` - one backend, two spellings
          credentialsRef:
            secretName: jfrog                 # ALSO the Xray credential. There is no second one.
          discovery:
            enabled: false

          # Optional. Valid only on a JFrog repository, and OFF unless said.
          # Everything answerable by "the same as the repository" is absent:
          # the endpoint, the credential, the CA bundle, the proxy and the
          # timeouts above are the ones Xray is reached with. See 21 section 3.
          xray:
            enabled: true
            # Only when the docker host is a subdomain and the platform is not.
            endpoint: https://acme.jfrog.io
            repositoryKey: docker-local
            watches: [production]             # scope to named Xray watches
            concurrency: 6                    # requests in flight, max 32
            batchSize: 50                     # artifacts per request, max 200
            timeout: 60s
            detailTtl: 15m                    # complete responses, capped at 24h
            summaryTtl: 6h                    # counts and severities

      # ─────────────────────────────────────────────────────────────
      # TARGETS - internal, read-write. Replication destinations and
      # promotion endpoints (both directions; see 01 §2.1).
      # ─────────────────────────────────────────────────────────────
      targets:
        - name: lab
          registry: internal.azurecr.io
          repository: vendor-a/platform
          type: acr                           # generic | acr | artifactory | quay
          credentialsRef:
            secretName: internal-acr
          # A registry we own, so we can be less polite than with a vendor.
          concurrency:
            perRegistry: 64
            requestsPerSecond: 200
          default: true                       # used when a request names no target

        - name: production
          registry: internal.azurecr.io       # same registry as lab =>
          repository: vendor-a/platform-prod  # cross-repo mount applies (05 §4.2)
          type: acr
          credentialsRef:
            secretName: internal-acr
          concurrency:
            perRegistry: 32                   # deliberately conservative
            requestsPerSecond: 100
          # Promotion-only: replication may not target this directly.
          promotionOnly: true

      # ─────────────────────────────────────────────────────────────
      # DOWNLOAD - WHAT happens when software is brought in. Targets,
      # gates, priority; no pattern, because by the time one runs the
      # software has been chosen. One entry is the default. See 20 §3.1.
      # ─────────────────────────────────────────────────────────────
      download:
        - name: internal
          targets: [lab]
          priority: 100                     # 0-1000, higher first (04 §6)
          verify: {before: true, after: true, policy: enforce}
          default: true

      # ─────────────────────────────────────────────────────────────
      # AUTO-DOWNLOAD - WHEN a download happens by itself. Evaluated on
      # each newly discovered package, in order; first match wins. It
      # triggers the download above rather than performing one of its
      # own. See 07 §4 and 20 §3.4.
      # ─────────────────────────────────────────────────────────────
      autoDownload:
        enabled: true
        rules:
          - name: ga-releases
            tagPattern: '^v\d+\.\d+\.\d+$'    # RE2

          - name: release-candidates
            tagPattern: '^v\d+\.\d+\.\d+-rc\.\d+$'
            sources: [primary]

      # ─────────────────────────────────────────────────────────────
      # VERIFICATION - cosign/sigstore. See 08.
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
      # NOTIFICATIONS - recipients per event type. See 12 §5.
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
      # NETWORK - applies to every repository in this product unless
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
      # RETENTION - overrides system defaults for this product. See 03 §8.
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
| `labels` | map | no | Keys/values ≤63 chars. **Not** propagated to metric labels - see [12](12-observability-and-audit.md) §2.1 on cardinality |

### 5.2 `sources[]` and `targets[]`

| Field | Type | Required | Default | Rules |
|---|---|---|---|---|
| `name` | string | yes | - | Unique within its list; same charset as product name |
| `registry` | string | yes | - | Host, optional port. No scheme - HTTPS assumed |
| `repository` | string | yes | - | Repository path, no tag or digest |
| `type` | enum | no | `generic` | `generic`, `acr`, `artifactory`, `quay` ([06](06-registry-abstraction.md)) |
| `vendor` (sources) | string | no | - | Publishing convention: `near`, or empty for anything conformant. See below |
| `anonymous` | bool | no | `false` | Mutually exclusive with `credentialsRef` |
| `credentialsRef` | object | conditional | - | Required unless `anonymous` |
| `concurrency` | object | no | inherits the application-level value | see §5.3 |
| `network` | object | no | inherits product | Same shape as `spec.network` |
| `default` (targets) | bool | no | `false` | At most one per product |
| `promotionOnly` (targets) | bool | no | `false` | Rejects replication requests naming this target |
| `replication` (targets) | object | no | `mode: copy` | Which mechanism puts content in this target: `copy` (our workers move it), `mirror` or `proxy` (Quay does). Requires `type: quay` for the latter two. Full schema, field reference and validation rules in [18](18-quay-replication.md) §5 |
| `discovery` (sources) | object | no | `enabled: true` | |

#### `vendor` - and why it is not `type`

`type` says **how to speak to the registry**, and for every vendor met so far - including Nokia's NEAR - that is plain OCI Distribution v2. `vendor` says **how the vendor publishes**, which is an entirely separate axis, and it is the single switch for every vendor-specific behaviour there is:

- how a release's tags are grouped into one package (NEAR publishes three per release);
- which of them is the transfer root, so a signature travels with its payload;
- which part of a tag is structural noise a listing can drop (`orb_`);
- which part of a repository path is (`orbs/`).

**It is opt-in per source, and that is the point.** Without it a NEAR registry is read as an ordinary one: three packages per release, no signature grouping, no shortening. Just as importantly, a registry that is *not* NEAR gets none of NEAR's rewriting. That second half is why the field exists - `orbs/` was being trimmed off repository paths for every source on the strength of what a page of results happened to look like, rather than on a statement about the vendor.

Shortening is cosmetic throughout. The real tag and repository path are what is stored, transferred and returned by `-o json`; both spellings resolve as input everywhere ([03](03-persistence.md) §11).

An unknown value is fatal for that source and no other, reported at startup. Falling back to standard behaviour on a typo would silently disable signature discovery, and nothing in any output would say why.

**Setting it takes effect on the next scan, including for packages already discovered.** That is not free - discovery skips a tag it has already recorded, so the display names of existing packages would otherwise never be revisited and no amount of re-scanning would fix them - so a scan reconciles every stored display name against the vendor plugin, at the cost of one query per repository and no registry traffic. `transferctl discover <product>` reports `Display names corrected` when it changes any. Removing the vendor clears them again by the same route.

Grouping is a different matter and is **not** retroactive: whether three tags become one package is decided when those tags are first recorded, and existing rows keep the shape they were discovered with. Only the naming is reconciled.

`signatures.layout` is the older spelling of this field and still works. It was nested under `signatures` when grouping tags was all it controlled; it now also decides how a package is NAMED, which is not a signature concern. Setting both to *different* values is a validation error rather than a precedence rule - picking one silently would leave the operator's other spelling doing nothing while looking as though it does something.

### 5.2.1 `xray`

Optional, and valid only on a `jfrog` (or `artifactory`) repository. Switches on
the JFrog Xray integration for that one repository, and configures only the
parts that are genuinely about Xray:

```yaml
xray:
  enabled: true          # absent means OFF
  endpoint: https://acme.jfrog.io   # only when the docker host is a subdomain
  repositoryKey: docker-local
  watches: [production]
  concurrency: 6
  batchSize: 50
  timeout: 60s
  detailTtl: 15m         # complete responses, capped at 24h
  summaryTtl: 6h         # counts and severities
```

**There is no credential here, and there will not be.** Xray sits on the JFrog
platform this repository already reaches, and takes the credential this
repository already declares in `credentialsRef`. A second credential model for
one host is not a feature - it is a second place for one password to be rotated,
and the second place is the one that is missed.

`enabled` defaults to **off**, inverting the convention every other `enabled`
in this schema follows. Deliberately: the others turn off something the document
asked for, this one would turn ON traffic to a third system the document never
mentioned.

An `xray` block on a repository that is not JFrog is rejected. That document is
silently wrong otherwise - well-formed, applied, never read - and the operator
sees a repository they believe reports vulnerabilities and which reports none.

Full argument in [21 - Security Posture](21-security-posture.md) §3.

### 5.3 `concurrency`

One number, at the application level, overridable per source or target.

```yaml
# system configuration - what every product inherits
concurrency:
  perRegistry: 32
  requestsPerSecond: 0
```

| Field | Type | Default | Meaning |
|---|---|---|---|
| `perRegistry` | int | 32 (from system config) | Requests in flight against one registry, **and** the size of the connection pool serving them |
| `requestsPerSecond` | int | 0 | Optional politeness ceiling on top of it; `0` = no artificial limit |

`burst` is derived (2×rps) and not configurable.

**Why one number.** A source used to carry seven: `rateLimits.maxConcurrentDownloads`, `maxConcurrentUploads`, `maxConnections`, `requestsPerSecond`, `burst`, plus `discovery.concurrency.repositories` and `.tags` - set per source, per target, in every product document. Nobody could predict what changing one of them would do, because the answer depended on the other six.

They were not independent either. Every request goes through one connection pool, so **the pool is the concurrency limit**: point more goroutines at a pool of 32 and you get 32 in-flight requests and a queue. The old defaults concealed this by agreeing with each other - 4 repositories × 8 tags = 32 = `maxConnections` - an agreement nobody wrote down and any edit would break. A pool sized above the worker count is idle sockets; below it is goroutines blocked on a semaphore they cannot see.

The default is 32 rather than something rounder because it is what the old defaults multiplied out to. The simplification changed the shape of the configuration, not the load it produces.

**Where it belongs.** At the application level, because it is a property of the *deployment* - the bandwidth it has, the proxy it sits behind, the politeness its vendors expect - not of any one product. The per-source block exists for the case that genuinely differs: one fragile vendor that needs a smaller number than the rest of the fleet.

**Fleet-wide, not per-worker.** The Coordinator divides the budget across active workers and ships each worker its share in the lease response. A per-worker limit would silently multiply by the replica count and flatten the vendor's registry the moment HPA scaled out.

**Superseded keys.** `rateLimits` and `discovery.concurrency` are still parsed and folded forward - `maxConnections` becomes `perRegistry`, and `repositories × tags` becomes `perRegistry` when that is all a document has. Silently ignoring a number someone deliberately set would be worse than either honouring it or rejecting it. `transferctl config validate` reports them as deprecations without failing, because a document that keeps working is worth more than a tidy schema.


### 5.4 `download[]` and `autoDownload.rules[]`

Two blocks, because they answer two questions. `download` says **what happens** when software is brought in; `autoDownload` says **when that happens without being asked**. Full treatment in [20](20-download-rules.md) §3.

#### `download[]`

A list. A product declaring one entry needs neither a `name` nor `default: true` - it is the default by being the only one. A product declaring several must name them and must mark exactly one default, which is what a bare `transferctl download` runs and what a rule naming none triggers.

| Field | Type | Required | Default | Rules |
|---|---|---|---|---|
| `name` | string | only with ≥2 | - | Unique within the product |
| `targets` | []string | no | the `default` target | Destinations, not hops: the set is closed over the targets' own `replication.mirror.from` and then ordered ([20](20-download-rules.md) §3.6). Must name declared, enabled, non-`promotionOnly`, non-`proxy` targets |
| `verify.before` | bool | no | product/target setting | Source-side gate |
| `verify.after` | bool | no | product/target setting | Destination-side gate |
| `verify.policy` | enum | no | product setting | `enforce` \| `warn`. Under `enforce` a target that fails verification stops every target that mirrors from it |
| `priority` | int | no | 50 | 0–1000 ([04](04-queue-and-scheduling.md) §6) |
| `default` | bool | only with ≥2 | - | Exactly one, once there are several |

There is **no `tagPattern` here, and there will not be one.** A download runs against software somebody has already named.

#### `autoDownload.rules[]`

> **Changed at [M9](17-delivery-plan.md#m9--downloads-and-auto-download):** this block is split in two. `spec.download` holds the targets, the gates and the priority and carries **no pattern**; `spec.autoDownload` keeps the tag pattern, gains `sources`, and names the download it triggers. A rule written the old way - targets and priority inline - keeps loading and keeps behaving identically; the compatibility contract is in [20](20-download-rules.md) §3.5.

Evaluated in order against each newly discovered package; **first match wins**, remaining rules are skipped. Ordered-first-match rather than all-match, because two rules matching the same tag with different downloads has no sensible interpretation.

A rule decides **which software**, and nothing else. Where it goes and what gates it is `spec.download` ([20](20-download-rules.md) §3.1), and a rule triggers that download rather than performing one - so the same work can be asked for by hand, with no pattern involved.

| Field | Type | Required | Default | Rules |
|---|---|---|---|---|
| `name` | string | yes | - | Unique within the product; appears in audit records |
| `tagPattern` | string | yes | - | RE2 (Go `regexp`). Rejected at load if it does not compile. A rule without one is rejected, and the error points at `spec.download` - downloading by hand needs no rule at all |
| `sources` | []string | no | every source | Narrows which sources the rule watches |
| `download` | string | no | the default download | Which download to trigger |
| `enabled` | bool | no | `true` | The only way to turn a rule off. There is deliberately no runtime override ([20](20-download-rules.md) §9) |
| `targets` | []string | no | - | **The older spelling.** A rule carrying it describes its own download inline and keeps working unchanged; it may not also name a `download` ([20](20-download-rules.md) §3.5) |
| `priority` | int | no | - | The older spelling, as above |
| `verifyBeforeTransfer` | bool | no | - | The older spelling, as above |

> **On regex safety:** Go's `regexp` is RE2 - linear time, no backtracking, so a pathological pattern cannot hang discovery. This is a genuine reason to state the dialect explicitly rather than saying "regular expression": with PCRE, a user-supplied pattern in a polling loop would be a denial-of-service vector.

### 5.5 `credentialsRef` and other secret references

```yaml
credentialsRef:
  secretName: vendor-a-registry   # required; a Kubernetes Secret name
  usernameKey: username           # default: username
  passwordKey: password           # default: password
```

Resolved to `/etc/softwaregateway/secrets/<secretName>/<key>`. Secret **values never appear** in config, logs, API responses, audit records, or error messages. The loader wraps credentials in a type whose `String()` returns `[REDACTED]`, so an accidental `%v` cannot leak one - a defence worth having because that mistake is otherwise a matter of time.

Bearer-token registries (and identity-token flows such as ACR) use the same shape with `passwordKey` holding the token; see [06](06-registry-abstraction.md) §4.

## 6. Configuration reload

1. `fsnotify` watches the products directory.
2. On change (debounced 2 s - kubelet writes are not atomic across files), reload **all** products.
3. Validate each independently.
4. Atomically swap the in-memory registry of valid products.
5. Emit `ConfigReloaded` audit events; update `softwaregateway_config_products_loaded` and `..._config_load_errors`.

**What reloads take effect immediately:** discovery intervals and filters, rate limits, auto-download rules, notification routing, verification policy, retention.

**What does not:** in-flight transfers keep the repository settings captured at plan time. Changing a target's registry hostname mid-transfer would otherwise mean a package half-written to one registry and half to another. New transfers pick up the new configuration.

**Removing a product** stops its discovery and rejects new requests for it. It does **not** cancel in-flight transfers or delete history - deletion of running work is never a side effect of a config edit. `transferctl` reports such transfers as belonging to an unconfigured product.

## 7. Validation and failure behaviour

**Fail closed per product, stay up overall.** A syntax error in `vendor-b.yaml` must never stop `vendor-a` from replicating.

On load, each product is validated for: schema conformance, unique names, resolvable secret references, compiling regexes, referenced targets existing, `promotionOnly` not named in auto-download rules, sane concurrency (non-negative, within the per-registry cap, and no superseded block left alongside a `concurrency` that overrides it), and parseable durations.

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

# How hard this installation works ANY ONE registry. Every product inherits
# it; a product may override it per source or target (§5.3).
concurrency:
  perRegistry: 32                  # in flight, and the connection pool size
  requestsPerSecond: 0             # 0 = no artificial limit

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
  # Bounds the CACHED MANIFEST BODIES - the only thing this system stores that
  # grows without limit and can be discarded without losing a fact (03 §12).
  # What a package IS stays forever; what it was SERVED AS is reclaimed.
  manifestCache:
    budgetBytes: 536870912         # 512 MiB; 0 disables the budget
    ttl: 168h                      # reclaim bodies untouched for a week; 0 disables
    sweepInterval: 15m

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

`tls` sits here rather than under a product's `network` block because it is implemented with Go's `GODEBUG` mechanism, which is per process and cannot be scoped to one connection. It applies to the process that reads it - set it on the Coordinator *and* the Worker, or discovery succeeds and every transfer fails at the handshake. Both binaries log a warning at startup saying so.

## 9. Development

The identical loader reads a plain directory, so local development needs no cluster and no Kubernetes objects:

```
./dev/
  config.yaml
  products/reference.yaml                         # every field, annotated
  secrets/vendor-registry/{username,password}     # plain files
```

`dev/products/reference.yaml` is deliberately the ONLY product in the tree. It
exercises every field the schema has - which is what makes it worth copying, and
what makes `task validate` a real check: a field that stops being accepted
breaks CI there first.

```bash
SWGW_CONFIG_DIR=./dev go run ./cmd/coordinator
```

with `database.driver: sqlite` as the development default - zero setup. See [14](14-deployment-and-development.md) §5.


---

## Enablement

Every level of the document has an `enabled` switch, defaulting to **true** so a document that says nothing is on.

| Field | Effect |
|---|---|
| `metadata.enabled` | the whole product runs or does not |
| `spec.sources[].enabled` | the source exists or does not |
| `spec.sources[].discovery.enabled` | the source is polled or not - it still exists |
| `spec.targets[].enabled` | the target receives transfers or does not |

> **Decision - disable rather than delete.**
>
> *Alternative:* remove the document to pause a product, which is what the design originally required.
>
> *Rejected because* deleting loses exactly what you most want back: the registries, credentials, filters and rules that were working. Re-creating them from memory during an incident is how a "temporary" pause becomes a subtly different configuration - and the difference is usually discovered later, by a package that quietly stopped being replicated.
>
> *What a disabled product still does:* it loads, it VALIDATES, and it appears in `products list` marked `DISABLED`. Validating it is the point - a mistake is reported now rather than found on the day someone re-enables it. Its `products` and `repositories` rows are DEACTIVATED, never deleted, so the packages and transfer history referencing them survive ([03](03-persistence.md) §4).
>
> *What would change our mind:* nothing likely. The cost is one field and a state column.

**`enabled` versus `discovery.enabled` on a source** is a real distinction, not a duplicate. `enabled: false` removes the source from every purpose; `discovery.enabled: false` stops the polling while leaving the source usable for an explicit transfer request - which is what a failover mirror needs, since it must stay reachable but must not double-discover every tag the primary already found.

Validation rejects the combinations that would otherwise fail silently:

- every source disabled while the product is enabled - the product would discover nothing while appearing active
- `autoDownload` enabled with no download and no enabled target - rules would match and have nowhere to send
- an auto-download rule naming a disabled target - this one fails the first time a package matches it, potentially weeks after the edit

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

**Why `cosign` is atomic.** It is one coherent trust decision - a mode plus the identity or key that mode requires. Merging field by field would silently produce combinations nobody wrote: a product's keyless `certificateIdentity` paired with a repository's `key` mode, or a Fulcio issuer left over from a block that no longer applies. A trust configuration assembled from two documents is one nobody can audit, and auditability is the entire point of verification.

**Why `proxy.direct` exists.** A product-level proxy is inherited, and "everything through the corporate proxy except this one internal registry" is the normal shape. Without an explicit switch the only way to express it is to repeat the registry's own hostname in `noProxy` at every level - which works, and is easy to get subtly wrong when the host has a port or an alias. `direct: true` also bypasses `HTTPS_PROXY` from the environment: a repository that asked to go direct means it, and honouring a cluster-wide setting anyway would make the option a no-op in precisely the deployment that needs it.

**Why `insecureSkipVerify` is three-state.** Every other override is "set wins, unset inherits", which for a boolean cannot distinguish an omitted field from an explicit `false`. That distinction matters here and nowhere else: a product-level `insecureSkipVerify: true` is inherited by every source and target, and without a way to write `false` a repository with a perfectly good certificate could never opt back into verification. So the field is a pointer, and the two states are different questions - "say nothing" versus "verify, whatever the level above says".

---

## TLS: two different failures, two different fixes

An earlier revision of `internal/registry/transport` said `InsecureSkipVerify` would never exist, on the grounds that a CA bundle is the correct fix. That was too strong, and it is recorded here as a correction rather than quietly reversed. `caBundleRef` fixes an **untrusted chain**. It does nothing for a certificate that is expired, carries the wrong hostname, or belongs to a registry mid-migration - and an operator who has to move bytes past one of those today should not have to patch the binary.

So `network.tls.insecureSkipVerify` exists, per repository, with three things attached to make it hard to forget: a `WARN` line naming the product and source on every configuration reload, a `certificate verification  WARNING` step in `transferctl products check`, and a validation error if it appears alongside `caBundleRef` in the same block - where the bundle would never be consulted and is therefore dead configuration that reads as if it verifies.

### What it does not fix

It does not fix this:

```
tls: failed to parse certificate from server: x509: negative serial number
```

Measured on Go 1.25.7, not reasoned about - `internal/platform/tlscompat` carries the test, and it asserts the failure so that a future Go release quietly changing the behaviour breaks the build rather than the documentation:

| client | result |
|---|---|
| default | `x509: negative serial number` |
| `InsecureSkipVerify: true` | **the identical error** |
| `GODEBUG=x509negativeserial=1` | connects, verification fully on |

`crypto/x509` has rejected negative serial numbers since Go 1.23, and rejects them while **parsing** - before verification runs. `InsecureSkipVerify` disables a step that is never reached. Shipping it as the fix for this error would have been shipping something that looks like a fix.

### Where the real fix lives, and why

`tls.allowNegativeSerialNumbers`, in **system** configuration, applied by `internal/platform/tlscompat` at process start in both the Coordinator and the Worker.

Not under `network.tls`, and the reason is the whole point of the split: it is implemented with Go's `GODEBUG` mechanism, which is per process. It cannot be scoped to one connection. Putting it beside `insecureSkipVerify` would have made it look per repository when it relaxes X.509 parsing for every registry, every Sigstore call and every other outbound connection the process makes. A setting whose blast radius the schema misrepresents is worse than one that is merely inconvenient to find.

Two consequences follow from it being process-wide:

- **Both binaries need it.** The Coordinator discovers, the Worker moves bytes. Setting it on one gives a product that discovers cleanly and fails every transfer at the handshake.
- **The existing `GODEBUG` is preserved, not replaced.** Container images and Deployments commonly set it already. Clobbering it would silently undo whatever it was doing, and nobody would connect the breakage back to this setting.

RFC 5280 §4.1.2.2 requires a positive serial number, so such a certificate is genuinely malformed - typically an appliance or enterprise CA encoding a random 20-byte value without clearing the high bit. The certificate is otherwise sound and, with the relaxation on, is verified normally. The setting is an escape hatch for an estate that cannot be reissued on our schedule, not an endorsement.
