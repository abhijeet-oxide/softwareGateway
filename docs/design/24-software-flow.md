# Configuration simplification, and flow as configuration

**Status:** proposal, for review. Nothing here is implemented.
**Answers:** `docs/software-flow.md`, plus the review that followed it.

The goal of this round is **not** the flow. It is the configuration. The flow in
`docs/software-flow.md` is one flow among many, and the test of this design is
that the tool does not know it exists: `download`, `onboard` and `promote` are
words a configuration file supplies, not behaviours compiled into the product.

---

## 0. Decisions taken from the review

These reverse or narrow what the first draft proposed.

| Question | Decision |
|---|---|
| Per-product flow override | **Yes, now.** Not deferred. A product may declare its own pipeline. |
| Order of work | **Configuration simplification first**, then everything else. |
| Verification gate | **Deferred, and absent means skipped.** A gate the build does not implement is recorded as skipped, never as failed and never as blocking. When verification lands, the same configuration starts enforcing with no edit. |
| Email notifications | **Left unimplemented on purpose.** The schema keeps the seam; nothing is built. |
| Findings after a hop | **No re-registration on the move.** A resync pulls from the release's *current* pointer, wherever it now lives. |
| Security result storage | **Configurable, and disposable.** Everything stored is recreatable by a resync. |
| Anchore | Endpoint, credential and account: **site default, product override**, with the credential rule below. |

### A misreading to clear up first

The first draft proposed moving Quay's *keys* out of the generic schema into a
`quay:` block **on the target**, owned by the Quay plugin. It did not propose
moving Quay configuration to the site. But it said "registry-type plumbing moves
under its type" a few lines after a large site-configuration listing, and that
is a fair thing to read the wrong way.

The review adds requirements the draft did not cover at all, and they change the
shape: there is **more than one Quay**, each product has its **own
organization**, and a mirror fan-out happens **as part of the onboard action**
rather than as a stage of its own. §5 is new and answers that; the split it
lands on is site owns the connection, product owns the organization.

### One correction to the first draft

I wrote that Anchore has no TLS, CA or proxy settings. That was wrong. Anchore
reaches out through the **product's** `network:` block —
`internal/regclient/security.go:287` — so `insecureSkipVerify` for Anchore is
reachable today.

The real defect is narrower and worth stating properly: it is *coupled*. There
is no way to relax TLS for Anchore alone, because the only switch is the one the
product's registries also read, and there is no site-level network block for
Anchore at all. §6 fixes the coupling rather than adding a setting that exists.

---

## 1. Principles this schema is held to

1. **One place per fact.** If two keys can disagree, one of them is deleted.
2. **Generic by default, deviation by type.** Anything true only of JFrog or
   only of Quay lives under that type's own block, not in the core schema.
3. **Site says how, product says what.** Tuning, endpoints and stage semantics
   are site configuration. A product names its software and its repositories.
4. **A missing capability skips, never blocks.** Gates degrade to "skipped".
5. **Nothing in the tool names a stage.** Not the API, not the UI, not the
   database. `external`, `lab`, `prod` are strings a site chose.

---

## 2. Site configuration — `config.yaml`

The stage vocabulary lives here, once, for every product that does not override
it.

```yaml
apiVersion: softwaregateway.io/v1alpha2
kind: SystemConfig

configDir: /etc/softwaregateway

# ─── REGISTRIES: EVERY CONNECTION THIS ESTATE HAS, NAMED ONCE ────────────────
# A product references one by NAME and adds only what is the product's own: a
# repository path, and for Quay an organization. Host, credential, network and
# type-specific tuning are stated here and never repeated.
#
# This is the single largest reduction in a product document. See §5.
registries:
  artifactory:
    host: artifactory.internal.example.com:9444
    type: jfrog
    credentialsRef: { secretName: internal-registry }
    network:
      caBundleRef: { secretName: internal-ca, key: ca.crt }
      tls: { insecureSkipVerify: false }
    jfrog:
      endpoint: https://artifactory.internal.example.com:9444   # also serves Xray

  quay-eu:
    host: quay-eu.example.com
    type: quay
    credentialsRef: { secretName: quay-eu-robot }
    network:
      tls: { insecureSkipVerify: false }
    quay:
      apiEndpoint: https://quay-eu.example.com/api/v1
      apiTokenRef: { secretName: quay-eu-api }
      # Defaults for every mirror INTO this Quay. These describe Quay's own
      # behaviour, not ours - see internal/product/replication.go.
      mirror:
        mode: mirror                # mirror (Quay pulls) | copy (we push) — §5.4
        interval: 24h
        skopeoTimeout: 30m
        acceptUnsignedImages: false
        syncOnRequest: true
        manage: apply
        network: { verifyTLS: true }

  quay-us:
    host: quay-us.example.com
    type: quay
    credentialsRef: { secretName: quay-us-robot }
    quay:
      apiEndpoint: https://quay-us.example.com/api/v1
      apiTokenRef: { secretName: quay-us-api }
      mirror: { mode: mirror, interval: 24h, syncOnRequest: true }

  vendor-near:                      # sources may be named here too, when shared
    host: registry.vendor.example.com
    type: near
    credentialsRef: { secretName: vendor-registry }
    network:
      proxy: { httpsProxy: 'http://proxy.internal:3128' }

# ─── THE DEFAULT FLOW ────────────────────────────────────────────────────────
# What a stage MEANS: its verb, how it is entered, what gates it, what happens
# to the copy it leaves behind. A product picks stages from here by name and
# binds each to one of its targets. A product may also declare stages of its
# own; see §3.4.
flow:
  default: [external, lab, prod]      # the pipeline a product gets if it says nothing

  stages:
    external:
      action: download                # the verb the API and the UI use
      label: Download                 # what the button says
      from: source                    # `source` is legal on the FIRST stage only
      trigger: auto                   # auto | manual
      gates: [signature, compliance]  # evaluated in order; unavailable ones skip
      onLeave: keep                   # keep | delete  — when the release moves on

    lab:
      action: onboard
      label: Onboard to Lab
      trigger: manual                 # `from` defaults to the preceding stage
      gates: []
      onLeave: keep
      # FAN-OUT. Entering this stage lands the release in the product's lab
      # target AND mirrors it into these registries, as one action. A product
      # that declares no `quay.organization` gets no mirrors and is told so.
      mirrors:
        registries: [quay-eu, quay-us]
        policy: required              # required | bestEffort — §5.3

    prod:
      action: promote
      label: Promote to Prod
      trigger: manual
      gates: []

# ─── GATES ───────────────────────────────────────────────────────────────────
# A gate is a named check a stage may require. `available: false` — or a gate
# this build does not implement — records "skipped" on the timeline and lets the
# release through. This is how verification is deferred without a config change
# later: flip `available` when it ships.
gates:
  signature:
    available: false                  # not implemented yet; skipped, not failed
    policy: enforce                   # enforce | warn — read once it is available
  compliance:
    available: true
    policy: warn                      # see §10, open item 2

# ─── SCANNERS ────────────────────────────────────────────────────────────────
# One shape for every scanner. A target opts in with `scan: [...]`; a product
# overrides endpoint, credential or account. See §6.
scanners:
  anchore:
    endpoint: https://anchore.example.com
    credentialsRef:
      secretName: anchore-api         # username/password keys, as everywhere else
    account: ""                       # default account; product may override
    network:                          # ← Anchore's OWN network, not the product's
      caBundleRef: { secretName: internal-ca, key: ca.crt }
      tls: { insecureSkipVerify: false }
      proxy: { direct: true }
      timeouts: { connect: 10s, responseHeader: 30s }
    concurrency: 12
    submit: true                      # may this deployment register images
    sbomFormat: spdx-json
    grouping:                         # how a release is named in Anchore
      application: '{{ .Product }}'
      version: '{{ .Version }}'

  xray:
    # Nothing to configure. Xray is an endpoint on a JFrog platform and is
    # reached with the target's own client, credential, CA and proxy.
    {}

# ─── SECURITY RESULTS: WHERE THEY LIVE ───────────────────────────────────────
# All of it is a cache in front of the scanners. Deleting any of it costs a
# resync and nothing else, which is why `documents` may sit somewhere cheap.
security:
  storage:
    summary:
      driver: database                # always database: the listing sorts on it
      retention: 2160h                # 90d
    documents:                        # SBOMs and raw scanner bodies — the bulk
      driver: database                # database | filesystem | s3
      path: /var/lib/softwaregateway/security   # filesystem driver
      # s3: { bucket: swgw-security, region: eu-west-1, credentialsRef: {...} }
      budgetBytes: 5368709120         # 5 GiB, LRU-evicted
      retention: 720h                 # 30d
  sync:
    maxAge: 6h                        # a sync re-asks about anything older
    kinds: [vulnerabilities, sbom, policy]

# ─── TUNING: SITE-WIDE, PRODUCTS OVERRIDE ONLY IF THEY MUST ──────────────────
concurrency:
  perRegistry: 32
  requestsPerSecond: 0                # 0 = no artificial rate ceiling

timeouts:
  connect: 10s
  responseHeader: 30s
  idleStall: 5m

network:                              # the estate's default egress
  proxy: { httpsProxy: "", noProxy: [] }
  tls: { insecureSkipVerify: false }

notifications:
  # Delivery is deliberately not implemented. Events are recorded in the outbox
  # and this block reserves the shape. See §10, open item 4.
  enabled: false

server:      { address: ":8080", shutdownGracePeriod: 15s }
database:    { driver: postgres, dsn: "" }
observability:
  log: { level: info, format: json }
  metrics: { enabled: true, path: /metrics }
tls:
  allowNegativeSerialNumbers: false   # process-wide; see the existing comment
```

---

## 3. Product configuration

### 3.1 The minimum that works

The default flow, one vendor, three internal repositories. This is the whole
document.

```yaml
apiVersion: softwaregateway.io/v1alpha2
kind: Product
metadata:
  name: cfx-5000
  owner: packet-core

spec:
  sources:
    - name: vendor
      registry: vendor-near            # a registry named in site config
      repositories: ['orbs/cfx-5000-k8s']
      discovery:
        interval: 15m
        tagFilters:
          include: ['^orb_']
          exclude: ['_base_']

  targets:
    - name: external
      registry: artifactory            # host, type, credential, CA: site config
      repository: sw-external/cfx-5000
      scan: [xray, anchore]

    - name: lab
      registry: artifactory
      repository: sw-lab/cfx-5000
      scan: [xray, anchore]

    - name: prod
      registry: artifactory
      repository: sw-gold/cfx-5000
      scan: [xray]

  # THE PRODUCT'S OWN QUAY IDENTITY. The site says which Quays the lab stage
  # mirrors into and how to reach them; this says who this product is on them.
  quay:
    organization: packet-core
    robot: packet-core+swgw

  # No `pipeline:` — so the site default applies: [external, lab, prod], each
  # bound to the target of the same name. That binding by name is the only
  # magic in this schema, and it exists to make the common document short.

  autoDownload:
    rules:
      - name: ga-releases
        tagPattern: '^orb_.*'
```

There is no `download:`, no `promotion:`, no `concurrency:`, no `environment:`,
no `xrayEnabled`, no `anchoreEnabled`.

### 3.2 The full form, with every optional block

```yaml
apiVersion: softwaregateway.io/v1alpha2
kind: Product
metadata:
  name: cfx-5000
  displayName: CFX 5000
  owner: packet-core
  enabled: true

spec:
  # ─── PIPELINE: explicit binding of stage → target ──────────────────────────
  pipeline:
    - stage: external
      target: external-repo
    - stage: lab
      target: lab-repo
      onLeave: delete                 # overrides the site's `keep` for this product
      mirrors: [quay-eu]              # narrows the site's [quay-eu, quay-us]
    - stage: prod
      target: gold-repo

  sources:
    - name: vendor
      registry: vendor-near
      repositories: ['orbs/cfx-5000-k8s']
      network:                        # overrides the registry's; unset inherit
        caBundleRef: { secretName: vendor-ca, key: ca.crt }
        proxy: { httpsProxy: 'http://proxy.internal:3128' }
        tls: { insecureSkipVerify: false }
        timeouts: { connect: 15s }
      concurrency: { perRegistry: 8 } # this vendor is fragile
      discovery:
        enabled: true
        interval: 15m
        maxRepositories: 10000
        repositoryFilters: { include: ['^orbs/'] }
        tagFilters:
          include: ['^orb_']
          exclude: ['^orb_.*_base_.*$']
      trust:                          # WAS `verification`. Renamed: it describes
        transferSignatures: true      # who is trusted, not whether we check.
        cosign:
          mode: keyless
          keyless:
            certificateIdentity: 'https://github.com/vendor/platform/.github/workflows/release.yaml@refs/heads/main'
            certificateOidcIssuer: 'https://token.actions.githubusercontent.com'

  targets:
    - name: external-repo
      registry: artifactory
      repository: sw-external/cfx-5000
      scan: [xray, anchore]
      network:
        tls: { insecureSkipVerify: true }   # this one repo only; see §4.5
      jfrog:                          # overrides the registry's jfrog block
        repositoryKey: sw-external

    - name: lab-repo
      registry: artifactory
      repository: sw-lab/cfx-5000
      scan: [xray, anchore]

    - name: gold-repo
      registry: artifactory
      repository: sw-gold/cfx-5000
      scan: [xray]

  quay:
    organization: packet-core         # this product's org on EVERY mirror Quay
    robot: packet-core+swgw           # the robot Quay pushes with
    repository: '{{ .Product }}'      # default; the path inside the org

  autoDownload:
    enabled: true
    rules:
      - name: ga-releases
        tagPattern: '^orb_.*'
        sources: [vendor]

  scanners:                           # product override of the site's scanners
    anchore:
      account: packet-core            # the common case: one line, inherits the rest
      # endpoint: https://anchore.customer.example.com
      # credentialsRef: { secretName: customer-anchore }   # REQUIRED with endpoint

  gates:                              # product override of the site's gate policy
    compliance: { policy: enforce }

  notifications:
    channels:
      - name: owners
        type: email
        email: { recipients: ['packet-core@example.com'] }
    subscriptions:
      - events: [SoftwareAvailable, GateFailed, TransferFailed]
        channels: [owners]

  retention:
    packages: 8760h
```

### 3.3 A product that is not this flow — plain copy, no promotion

The document's flow is one flow. This is another, and it needs no new feature:

```yaml
spec:
  pipeline:
    - stage: mirror
      target: internal
  sources:
    - name: upstream
      registry: quay-io                # declared in site config, anonymous
      repositories: ['openshift/cli']
  targets:
    - name: internal
      registry: artifactory
      repository: mirror/openshift-cli
  autoDownload:
    rules: [{ name: all, tagPattern: '.*' }]
```

One stage. Auto-download lands in it. The release page shows one action —
whatever `flow.stages.mirror.action` is named — and never mentions promotion,
because this product has no second stage. `mirror` is not a stage the site
declared, so it is declared inline; see §3.4.

### 3.4 A product declaring stages of its own

```yaml
spec:
  pipeline:
    - stage: staging
      target: staging-repo
      action: download                # verb, label, trigger and gates supplied
      label: Download to Staging      # inline because the site never declared
      from: source                    # a stage called `staging`
      trigger: auto
      gates: [signature]
    - stage: canary
      target: canary-repo
      action: release
      label: Release to Canary
      trigger: manual
    - stage: fleet
      target: fleet-repo
      action: rollout
      label: Roll Out to Fleet
      trigger: manual
```

Three stages, three verbs, none of which exist anywhere in the tool. The release
page shows `Download to Staging`, then `Release to Canary`, then
`Roll Out to Fleet`, one at a time, because the page renders what the server
says is available.

### 3.5 What the interface is handed

```
GET /api/v1/products/cfx-5000/packages/{ref}/actions

{
  "location": { "stage": "external", "target": "external-repo",
                "since": "2026-09-01T09:12:04Z" },
  "actions": [
    { "name": "onboard", "label": "Onboard to Lab",
      "from": "external", "to": "lab", "available": true },
    { "name": "promote", "label": "Promote to Prod",
      "from": "lab", "to": "prod", "available": false,
      "reason": "This release is in external. Onboard it to lab first." }
  ],
  "gates": [
    { "name": "signature",  "state": "skipped", "detail": "Signature verification is not available in this build." },
    { "name": "compliance", "state": "passed",  "at": "2026-09-01T09:14:40Z", "took": "38s" }
  ]
}
```

`PackageDetail.tsx` renders the one available action as its primary button and
the gates as timeline entries. It contains no string from this document.

---

## 4. What the simplification actually removes

The product schema carries **124 distinct keys** today
(`grep -ho 'json:"[a-zA-Z]*' internal/product/*.go | sort -u | wc -l`). The
target is roughly **70**. Where the 54 go:

### 4.1 Registry-type plumbing moves under its type — about 25 keys

`internal/product/replication.go` puts Quay's whole vocabulary inline in the
generic schema: `replication`, `mirror`, `proxy`, `robot`, `organization`,
`upstreamRegistry`, `upstreamCredentialsRef`, `sourceCredentialsRef`,
`externalReference`, `skopeoTimeout`, `prewarm`, `acceptUnsignedImages`,
`syncOnRequest`, `expiration`, `apiEndpoint`, `apiTokenRef`, `manage`,
`startAt`, `verifyTLS`, `insecure`, and more. Every product document carries the
possibility of them; one product uses them.

They become a `quay:` block, valid only on `type: quay`, owned and validated by
the Quay plugin. Likewise `xrayEndpoint`, `jfrogEndpoint`, `jfrogRepositoryKey`
become `jfrog: { endpoint, repositoryKey }`. This makes the document's own rule
— *"any repo specific action must be done by specifying repo type"* — literally
how the schema is shaped.

### 4.2 The flow blocks collapse into `pipeline` — about 8 keys

`download[]` (`name`, `targets`, `verify`, `priority`, `default`), `promotion`
(`from`, `to`), `promotionOnly`, `environment`, and `autoDownload.rules[].targets`
/ `.priority` / `.verifyBeforeTransfer` all describe pieces of a route. One
`pipeline` describes the route. `autoDownload` keeps only what is genuinely its
own: `enabled`, and rules with `name`, `tagPattern`, `sources`.

`promotionOnly` disappears as a concept, not just as a key: a stage that is not
first cannot be reached from a source, so production is promotion-only by
construction.

### 4.3 `vendor` and `type` become one written word — 1 key

They are two facts — wire protocol and publishing convention — and they stay two
facts internally, because NEAR speaks plain OCI (`schema.go:157` is right about
this). But a document writes `type: near` and the loader resolves it to
*protocol generic, convention near*. `vendor:` and `signatures.layout` stay
accepted and report as superseded.

### 4.4 Superseded blocks are removed, not just deprecated — about 12 keys

`rateLimits` (5 keys), `discovery.concurrency` (2), `signatures.layout`,
`verification.atSource`, `verification.atDestination`,
`rules[].verifyBeforeTransfer`. These are already marked superseded in code and
folded at load. `v1alpha2` drops them; `v1alpha1` documents keep working through
the converter (§6).

### 4.5 `insecureSkipVerify` is reachable on every connection — the ask, completed

After this it is settable at: site `network`, product `network`, source
`network`, target `network`, and `scanners.anchore.network` (site and product).
One resolution function — `product.SkipsTLSVerification`, which already exists —
serves all of them, and every client built with it logs the repository it
belongs to. The gap today is only the last one, and only because Anchore borrows
the product's block (§0).

---

## 5. Registries, and mirroring to several of them at once

Two requirements from the review that the earlier draft did not cover: there is
more than one Quay, and the mirror fan-out happens **as part of onboarding**
rather than as a stage of its own.

### 5.1 The split: the site owns the connection, the product owns the identity

| Belongs to the site (`registries:`) | Belongs to the product |
|---|---|
| Host, registry type | Which registry, by name |
| Credential (`credentialsRef`) | Repository path |
| CA bundle, proxy, TLS posture, timeouts | Organization and robot, for Quay |
| Quay API endpoint and API token | — |
| Quay mirror defaults: interval, skopeo timeout, unsigned-image policy, manage mode, Quay's own egress | — |

Neither half is a copy of the other, and neither can drift, because neither is
written twice.

This is worth doing for its own sake, before Quay enters it.
`dev/products.example/nokia-cmm.yaml` writes
`artifactory.internal.example.com:9444` three times, `credentialsRef:
internal-registry` twice, and an `xrayEndpoint` that restates the registry host
it is already sitting next to. Every product document in the estate repeats the
same handful of hosts and secret names. Naming them once removes four keys from
every target and is the single largest reduction in a product document.

### 5.2 Mirrors are derived targets, not a second kind of thing

A mirror destination has everything a target has — a registry, a repository, a
credential, a place in a transfer. So it **is** a target; it is just not one
anybody typed.

At load time the pipeline's `mirrors:` list and the product's `quay:` block
synthesise one target per (stage, registry):

```
  stage lab, mirrors [quay-eu, quay-us], quay.organization packet-core
    →  target  lab@quay-eu   quay-eu.example.com/packet-core/cfx-5000
       target  lab@quay-us   quay-us.example.com/packet-core/cfx-5000
```

Everything downstream — the transfer engine, the replication reconciler, the
security scope, the audit trail — sees ordinary targets and needs no new
concept. A product that needs a bespoke mirror still declares an explicit target
and names it in `mirrors:` instead.

**A stage with several destinations is already a shape this code has.**
`internal/download/chain.go` gives every step an index and states that "steps
sharing an index have no dependency between them and run concurrently — which is
how fan-out and chaining end up being the same feature rather than two." The
primary and its mirrors share an index. Nothing in the engine changes.

### 5.3 What happens when a mirror fails

`policy` on the stage's `mirrors:` block:

- **`required`** — the stage is not complete until every destination has the
  release. The location pointer stays where it was, the action keeps retrying,
  and the release page shows a row per destination. This is what "in the same
  moment" means if it is to mean anything.
- **`bestEffort`** — the primary target decides the stage. A mirror failure is
  recorded and surfaced on the timeline, and the release moves on.

Default `required`, on the reading that a release which reached lab but not the
Quays that serve lab clusters has not really been onboarded.

### 5.4 One thing to decide, because it changes what "at the same moment" can mean

Quay can be fed two ways, and the existing code supports both
(`internal/product/replication.go`):

- **`mode: mirror`** — we write a mirror configuration into Quay and Quay pulls
  for itself, on its own schedule. `syncOnRequest: true` turns an onboard into a
  sync-now plus a wait. Quay must be able to reach the upstream repository
  itself. This is right for continuous sync, and it means "at the same moment"
  is really "we trigger it and wait for Quay".
- **`mode: copy`** — our workers push the bytes. Predictable, immediate,
  identical to every other transfer, and no Quay-side network path to arrange.

For an onboard-time fan-out to several Quays I would default to **`copy`**, and
keep `mirror` for a destination that should keep tracking the upstream after the
onboard. The site config above shows `mode` per registry so an estate can run
both. See §10, open item 5.

### 5.5 A product that mirrors, in full

```yaml
spec:
  pipeline:
    - stage: external
      target: external
    - stage: lab
      target: lab
      mirrors: [quay-eu, quay-us]     # or omit, to take the site's list
    - stage: prod
      target: prod

  targets:
    - name: external
      registry: artifactory
      repository: sw-external/cfx-5000
      scan: [xray, anchore]
    - name: lab
      registry: artifactory
      repository: sw-lab/cfx-5000
      scan: [xray, anchore]
    - name: prod
      registry: artifactory
      repository: sw-gold/cfx-5000
      scan: [xray]

  quay:
    organization: packet-core
    robot: packet-core+swgw
```

Four lines of Quay configuration in the product — an organization, a robot, and
a list of two registry names — against the twenty-odd keys a Quay mirror needs
today. And `mirrors: []` on the stage opts a product out entirely.

---

## 6. Anchore: how it is configured, and how a product overrides it

Most of this already exists and is the right shape. Stating it plainly because
it was asked, and marking the two changes.

**Site declares it once.** Endpoint, credential, account, concurrency, timeouts,
SBOM format, grouping templates, and — *new* — its own `network` block.

**A target opts in.** `scan: [anchore]` on the repository whose images Anchore
should pull. Per repository rather than per product, because "which images" is
answered by "the ones in this repository", and a product may want production
scanned and lab not.

**A product overrides three things, independently:**

| Override | Written | Meaning |
|---|---|---|
| Account only | `scanners.anchore.account: packet-core` | Same Anchore, same credential, findings land in this team's account. The common case, one line. |
| Credential only | `scanners.anchore.credentialsRef: {...}` | Same Anchore, a different service account. |
| A different Anchore | `endpoint:` **and** `credentialsRef:` | Both, together. |

**The credential rule, which is a security property and not a convenience.** A
product naming its own `endpoint` **must** name its own `credentialsRef`.
Falling back would send the deployment's Anchore credential to whatever host a
product document names — a credential leak written in four lines of YAML. This
is enforced at validation and re-checked at client build time
(`internal/regclient/security.go:343`), and it stays.

**Secrets are referenced, never inline**, resolved by the same `SecretResolver`
as every registry credential, from `<secretsDir>/<name>/<key>`, defaulting to the
`username` and `password` keys. An API key goes in the password key.

**The two changes:**
1. `scanners.anchore.network` at site and product level, so Anchore's TLS, CA
   and proxy stop being the product's registry network (§0).
2. `xrayEnabled` / `anchoreEnabled` booleans become `scan: [xray, anchore]`, so
   a third scanner is a list entry rather than a schema change.

---

## 7. Migration

`v1alpha1` documents must keep loading. A converter, not a rewrite:

- `internal/product/convert.go` reads either apiVersion and produces the
  `v1alpha2` in-memory model. Every folded key appends to `Deprecations`, which
  `transferctl config check` already prints.
- `transferctl config migrate <file>` writes the `v1alpha2` form to stdout, so an
  operator converts a document by reading the diff rather than by hand.
- The example products under `dev/products.example/` are converted as part of
  the change, and are the readable proof the schema works on the four shapes
  already there.

---

## 8. Where the code needs unpicking

Found by reading, not assumed. Items 1–4 are on the path of this work; 5–7 are
the standing mess and are listed so the decision to defer them is deliberate.

**1. The release has no location, so three places each guess differently.**
- `internal/regclient/security.go:528` picks the first *reached* target in
  config order — after an onboard it keeps asking `external`.
- `cmd/coordinator/blobs.go:35` always reads the **vendor source**, explicitly:
  *"The SOURCE, never a target."* Compliance charts come through it, so a
  re-run after onboarding pulls from the vendor rather than from lab.
- The compliance fetcher inherits that choice without knowing it made one.

Fix: one `internal/release` package owning the pointer — current stage, current
target, history — and all three resolve through it. This is what makes
"resync from the current pointer" true, and it is a deletion of duplicated
judgement, not an addition.

**2. `internal/product` is a schema and a resolver and a validator in one.**
4,795 lines over 15 files, with `schema.go` at 1,090 and `validate.go` at 1,071.
Split: `product/schema` (types only), `product/load` (parse, convert, defaults),
`product/validate`, and `product/plugin` for the per-type blocks of §4.1, each
type validating its own.

**3. `internal/api` is 13,394 lines in one package**, of which security is six
files and ~4,700 lines (`security.go`, `securitywire.go`, `securityexport.go`,
`securitysheets.go`, `securitydocuments.go`, `securityreplicate.go`) and
compliance is five and ~2,600. Move each to `internal/api/security/` and
`internal/api/compliance/`. Mechanical; no logic moves.

**4. `internal/store` is 15,721 lines**, with `packages.go` at 2,462 and
`queue.go` at 2,239. Split by aggregate along the lines the filenames already
suggest.

**5. 30 MB of build output is committed to git.** `fakeregistry` (9.9 MB) and
`smoketmp` (19 MB) are tracked binaries; `tmpclaim/` and `tmpexists/` are
scratch `main` packages — one of them a one-off SQL fixup against the dev
database. Delete all four, extend `.gitignore`. Five minutes, and it is a third
of the repository.

**6. `cmd/transferctl` is 8,757 lines.** Rendering is inline with command
wiring, which is why there are `*view_test.go` files beside every command.
Extract `internal/cli/render`.

**7. The web bundle has four files over 1,900 lines** —
`securitypanel.tsx` (3,937), `Table.tsx` (3,293), `types.ts` (2,621),
`compliancepanel.tsx` (1,934). `types.ts` should be generated from the API
types rather than maintained.

---

## 9. The steps, in the order you asked for

**Step 1 — Schema `v1alpha2`.** Types, converter from `v1alpha1`, defaults,
validation, `config check` and `config migrate`, and the four example products
converted. No runtime behaviour changes: the loader produces the same resolved
model it does today. *This is the whole of the simplification ask, and it is
reviewable as a diff of `dev/products.example/`.*

**Step 2 — Type-owned config blocks.** `jfrog:` and `quay:` blocks, each
validated by its own plugin; the ~25 keys leave the core schema. Depends on 1.

**Step 3 — The pipeline model.** `flow` in site config, `pipeline` in products,
stage resolution, the skip rule, and `config check` printing the derived
pipeline per product. Still no behaviour change: `download` and `promote` keep
working, now derived rather than declared.

**Step 4 — One generic action.** The `advance` operation and
`/packages/{ref}/actions`. `onboard` becomes real by being a hop between two
stages — it needs no new transfer machinery, only a route. `PackageDetail.tsx`
renders the returned actions and stops naming Download and Promote. *End of this
step, the document's flow works end to end, minus gates.*

**Step 5 — The location pointer.** `internal/release`, the three call sites
above resolving through it, resync from the current pointer, and `onLeave:
delete`. Fixes the Xray break by construction.

**Step 6 — Gates, as records.** The gate framework, the timeline stage records
with durations, and the `Logs` side panel modelled on the existing sync-log
panel (`web/src/components/security.tsx:1506`). `signature` reports *skipped*
throughout; `compliance` becomes a real gate here.

**Step 7 — Security result storage.** The `filesystem` document driver behind
the existing `DocumentStore` interface (`internal/security/document.go:276`),
selected by `security.storage.documents.driver`. Small, because the seam is
already there.

**Step 8 — Verification.** When it is built: implement the `signature` gate,
flip `gates.signature.available` to true. No configuration changes.

**Step 9 — Notifications.** Left open. The outbox and the events exist; the
dispatcher and senders are not built and are not scheduled here.

Steps 1–2 are the configuration simplification. Steps 3–5 are the flow. Steps
6–7 are what makes it legible and cheap to store. 8–9 are yours to schedule.

The cleanups in §7 items 3–7 are not steps of their own. Each is done as the
step that touches that code arrives — the api split during step 4, the store
split during step 5 — except item 5, the committed binaries, which should be
deleted now regardless of any of this.

---

## 10. Open items

1. **Stage-to-target binding by name.** §3.1 binds `stage: lab` to `target: lab`
   when the names match, which is what makes the short document short. It is
   also the one implicit rule in the schema. Keep it, or require the explicit
   `pipeline:` block always?
2. **What `compliance: enforce` blocks.** Blocking the download means the
   evidence for the failure never lands anywhere inspectable. Proposal: a gate
   on the first stage blocks the *next* hop rather than the download itself,
   and only `signature` — once it exists — blocks the download. Your call; it
   changes what the word means.
3. **`onLeave: delete` default.** Proposed `keep` site-wide, with the document's
   external→lab deletion written per product. Or default `delete` on the first
   stage only?
4. **Notification events.** `SoftwareAvailable` and `GateFailed` are named in
   this schema and recorded in the outbox with nothing consuming them. Confirm
   the names now so the events written between here and the dispatcher are the
   right ones.
