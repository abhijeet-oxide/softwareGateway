# Software Flow — design and plan

**Status:** proposal, for review. Nothing here is implemented yet.
**Answers:** `docs/software-flow.md`.

The request in that document is one question with a lot of surface: *how do we
express external → lab → prod generically, without every product repeating it,
and without the UI hard-coding it?* Everything else follows from the answer.

---

## 1. The design, in one idea

**A site declares its lifecycle once. A product only says which of its targets
sit in which environment. The pipeline is derived from those two facts.**

No product writes a pipeline. No product writes `download:` or `promotion:`.
A product tags targets with `environment:` — which the schema already has
(`internal/product/schema.go:354`) — and the flow falls out.

Three consequences, and they are the three things the document asks for:

- **Not repetitive.** A product adds one line per target.
- **Flexible.** A team with no external repository declares no `external`
  target; the stage is skipped and auto-download lands in the first stage the
  product actually has. Nothing else changes.
- **The UI stops naming stages.** The server returns the actions available on a
  release — verb, label, from, to, why-not. The page renders whatever comes
  back. A site that adds a fourth stage gets a fourth button with no frontend
  change.

### The rules

1. Stages are ordered. A release occupies exactly one — the furthest it has
   reached.
2. Stage 1 is the only stage that may read a source. Every later stage reads
   the stage before it. `prod` can therefore never be fed from the vendor, and
   that is a property of the model rather than a flag on a target.
3. A stage with no target in this product is **skipped**, and the next stage
   reads the nearest earlier stage that does exist.
4. Auto-download fires into stage 1, whichever stage that turns out to be.
5. The action that enters a stage is named by the stage. `download`,
   `onboard`, `promote` are three instances of one operation — move a release
   from where it is to the next place — and not three subsystems.

---

## 2. Sample configuration

### 2.1 Site — `config.yaml`

```yaml
flow:
  stages:
    - name: external
      action: download          # the verb; the API and the UI use it
      label: Download
      from: sources             # only legal on the first stage
      entry: auto               # auto-download rules may fire here
      gates: [signature, compliance]
      onLeave: keep             # keep | delete  (see §5, open question 1)
    - name: lab
      action: onboard
      label: Onboard to Lab
      entry: manual
    - name: prod
      action: promote
      label: Promote to Prod
      entry: manual

anchore:
  endpoint: https://anchore.example.com
  secretName: anchore-api
  network:                      # NEW — same shape as a repository's
    caBundleRef:
      secretName: internal-ca
      key: ca.crt
    tls:
      insecureSkipVerify: false
    proxy:
      direct: true

concurrency:
  perRegistry: 32               # products override only when they must
```

`from:` is implicit — the previous stage — and written only on stage 1.
Defaults keep the file short: a site that omits `flow:` gets exactly the three
stages above.

### 2.2 Product

```yaml
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata:
  name: cfx-5000
  owner: packet-core
spec:
  sources:
    - name: vendor
      type: near                # protocol + publishing convention, one field (§4.9)
      registry: registry.vendor.example.com
      repositories: ['orbs/cfx-5000-k8s']
      credentialsRef: { secretName: vendor-registry }
      network:
        caBundleRef: { secretName: vendor-ca, key: ca.crt }
        proxy: { httpsProxy: http://proxy.internal:3128 }
      discovery:
        enabled: true
        interval: 15m
        repositoryFilters: { include: ['^orbs/'] }
        tagFilters:
          include: ['^orb_']
          exclude: ['^orb_.*_base_.*$']
      verification:             # HOW this vendor is trusted
        enabled: true
        policy: enforce         # enforce | warn — does a failure stop the download
        transferSignatures: true
        cosign:
          mode: keyless
          keyless:
            certificateIdentity: 'https://github.com/vendor/platform/.github/workflows/release.yaml@refs/heads/main'
            certificateOidcIssuer: 'https://token.actions.githubusercontent.com'
      compliance:               # NEW — a gate, not just a report
        enabled: true
        policy: enforce

  targets:
    - name: external
      environment: external     # ← this line is the whole pipeline declaration
      registry: artifactory.internal.example.com
      repository: sw-external/cfx-5000
      type: jfrog
      xrayEnabled: true
      anchoreEnabled: true
      credentialsRef: { secretName: internal-registry }

    - name: lab
      environment: lab
      registry: artifactory.internal.example.com
      repository: sw-lab/cfx-5000
      type: jfrog
      xrayEnabled: true
      anchoreEnabled: true
      credentialsRef: { secretName: internal-registry }

    - name: gold
      environment: prod
      registry: artifactory.internal.example.com
      repository: sw-gold/cfx-5000
      type: jfrog
      xrayEnabled: true
      credentialsRef: { secretName: internal-registry }

  autoDownload:
    enabled: true
    rules:
      - name: ga-releases
        tagPattern: '^orb_.*'

  notifications:
    enabled: true
    channels:
      - name: owners
        type: email
        email: { recipients: ['packet-core@example.com'] }
    subscriptions:
      - events: [SoftwareAvailable, VerificationFailed, ComplianceFailed]
        channels: [owners]
```

No `download:`. No `promotion:`. No `concurrency:`. No per-stage plumbing.

### 2.3 The product with no external repository

Delete the `external` target. That is the entire diff. Auto-download now lands
in `lab`, the primary button on a new release reads **Download to Lab**, and
`Promote to Prod` follows it. Nothing else in the document changes.

### 2.4 What the interface is handed

```
GET /api/v1/products/cfx-5000/packages/{ref}/actions

{
  "location": { "stage": "external", "target": "external", "since": "2026-09-01T09:12:04Z" },
  "actions": [
    { "name": "onboard", "label": "Onboard to Lab",
      "from": "external", "to": "lab", "available": true },
    { "name": "promote", "label": "Promote to Prod",
      "from": "lab", "to": "prod", "available": false,
      "reason": "This release is in external. Onboard it to lab first." }
  ]
}
```

The page shows the one available action as its primary button, and nothing
else. That is the document's `Download` / `Onboard to Lab` / `Promote to Prod`
rule expressed as data rather than as three `if`s in `PackageDetail.tsx`.

---

## 3. What already exists, and what has to be built

Traced against the current tree. This is the honest inventory.

### Already there, reusable as-is

| Thing | Where |
|---|---|
| `environment` on a target, and promotion resolving against it | `internal/product/schema.go:354`, `internal/api/promotions.go` |
| Target-to-target hops with a JFrog fast path | `internal/promote`, `internal/promotion` |
| Chained destinations closed over `mirror.from` | `internal/download/chain.go` |
| Anchore as system default + per-product override, secretRef-shaped | `internal/product/anchore.go:140`, `config.go:412` |
| Xray/Anchore per repository, either scanner | `internal/regclient/security.go` |
| Sync-logs button and side panel — the pattern the release Logs panel copies | `web/src/components/security.tsx:1506` |
| Notification outbox, written in the same transaction as the fact | `internal/store/packages.go:503` |

### Missing, and load-bearing for this flow

| Gap | Evidence | Impact |
|---|---|---|
| **Signature verification is not implemented.** The schema, the policy and signature *discovery* exist; the cryptography does not — there is no sigstore/cosign dependency in `go.mod`. | `internal/vendors/layout.go`, `go.mod` | Step 1 of §2.2 of the flow document ("verify at source") does nothing today. `policy: enforce` gates nothing. Largest single item. |
| **No compliance gate on download.** Compliance is a run somebody starts; the download path never consults it. | `internal/download/run.go` | Step 2 of §2.2 does not exist. |
| **Notifications are never delivered.** Rows are enqueued; there is no sender, no SMTP, no Teams client. | no sender anywhere in `internal/` | "notify the user with a link to the package page" is a dispatcher plus one new event. |
| **Security scope picks the first reached target in config order**, not the release's current stage. | `internal/regclient/security.go:528` | After `Onboard to Lab` a sync would keep asking `external`, which is exactly the break the document describes. |
| **Compliance and file reads always pull from the vendor source.** | `cmd/coordinator/blobs.go:35` (`ReadBlob` — "the SOURCE, never a target") | Re-running compliance after onboarding pulls charts from the vendor, not from lab. |
| **No deletion after a hop**, so the "delete from external" half of the document has no implementation to break in the first place. | — | Needs building alongside the location resolver, not before it. |
| **Timeline is three hard-coded moments** with no durations, no expansion, no stages. | `web/src/components/layout.tsx:226` | `Published → Signature → Compliance → Download → Security → Lab → Production` is a new component over new stage records. |
| **Anchore has no TLS/CA/proxy settings.** | `internal/platform/config/config.go:412` | The document's "every connection must be able to skip cert validation in lab". |

---

## 4. How each question in the document is answered

1. **"We need an onboard wrapper."** There is no wrapper. `onboard` is
   `promote` with `from: external, to: lab` — the same hop machinery
   (`internal/promotion`), reached through one generic `advance` operation.
   `download` and `promote` stay as names and become aliases for it.
2. **"Products shouldn't each define pipelines."** They don't. They tag
   targets with `environment`; the site declares the stages.
3. **"What about a team with no external repo?"** Stage skipping, §1 rule 3.
4. **"How does the UI show a new flow?"** It never names stages — §2.4.
5. **"Sync must follow the release."** A release's *location* becomes a
   first-class fact, and security scope, compliance chart fetch and file reads
   all resolve against it instead of against config order or the vendor.
6. **"Concurrency and timeouts default at the site."** Already the case for
   concurrency (`config.yaml: concurrency.perRegistry`); the change is to stop
   writing them in product documents and to say so in validation warnings.
7. **"Anchore config should look like repository config."** Add a `network:`
   block to `AnchoreConfig` with the same `caBundleRef` / `proxy` / `tls`
   shape, resolved by the same code.
8. **"Verification belongs at repo level."** It already is —
   `Source.Verification` overrides the product's. `atSource` / `atDestination`
   get deprecated: the download's `verify.before` / `verify.after` say the same
   thing and are the ones the engine reads.
9. **"Change `vendor:` to `type:`."** Recommendation with a caveat. `type` is
   the wire protocol and `vendor` is the publishing convention, and they vary
   independently — NEAR speaks plain OCI (`schema.go:157`). So rather than
   merge the concepts, accept `type: near` as the written form and resolve it
   to *protocol = generic, convention = near*. One field to write, both facts
   preserved. `vendor:` stays accepted and deprecated.

---

## 5. Open questions — please answer before Phase 1

1. **Delete after onboarding.** The document says the release is deleted from
   `external` on the move to lab. Should that be per-stage configuration
   (`onLeave: delete|keep`, default `keep`) or unconditional? Proposal:
   configurable, default `keep`, because a deletion that cannot be turned off
   is a deletion nobody can debug.
2. **Scanning at every stage, or once?** If `external`, `lab` and `gold` all
   have `anchoreEnabled`/`xrayEnabled`, does each hop re-register the release
   and produce its own findings, or do findings follow the release? Proposal:
   register at each stage that asks for it; the sync reads the current stage.
3. **Per-product flow override.** Site-level stages plus environment tagging
   covers every case in the document. Do we need `spec.flow` for a product that
   genuinely deviates, or defer it until something needs it? Proposal: defer.
4. **`enforce` on compliance.** Blocking a download on a compliance failure
   means the artifact never lands and cannot be inspected — the evidence for
   the failure is in a repository we did not download to. Proposal: `enforce`
   downloads into stage 1 and blocks the *next* hop, and only `signature`
   blocks the download itself. This wants your call, it changes what the word
   means.

---

## 6. Work plan

Phases are independently shippable, and each leaves the system working.

**Phase 0 — approve.** This document, plus the sample configs above committed
to `dev/products.example/`. No code.

**Phase 1 — the flow model.** *(config + derivation, no behaviour change)*
`flow` in `SystemConfig`; derivation of a product's stages from its targets'
environments; skip rule; validation and `transferctl config check` output that
prints the derived pipeline. Existing `download:` / `promotion:` blocks keep
working and are reported as superseded.

**Phase 2 — one generic action.** The `advance` operation and the
`/packages/{ref}/actions` endpoint. `onboard` becomes real by being a hop.
`PackageDetail` renders the primary button from the endpoint and stops
hard-coding Download/Promote. **This is the phase that makes the document's
flow work end to end**, minus the gates.

**Phase 3 — location.** Release location as a stored fact; security scope,
compliance chart fetch and blob reads resolve against it; optional
`onLeave: delete`. Fixes the Xray-link break the document is worried about.

**Phase 4 — timeline and logs.** Stage records with start, end and duration; the
expandable release timeline; the `Logs` side panel built on the existing
sync-log panel.

**Phase 5 — the gates.** Real cosign/PKCS#7 verification, and the compliance
gate. Independent of 1–4 and the largest item; can run in parallel with them.

**Phase 6 — notifications.** Outbox dispatcher, email and Teams senders, and a
`SoftwareAvailable` event carrying the package-detail link.

**Phase 7 — config hygiene.** Anchore `network:`, `type: near`, deprecation of
`atSource`/`atDestination` and of per-product concurrency, and the doc updates.

Phases 1–4 deliver everything in §2 of the flow document that is about *moving
and showing*. Phase 5 delivers what it says about *trusting*, and it is the one
to start early if the signature gate is what the demo has to show.
