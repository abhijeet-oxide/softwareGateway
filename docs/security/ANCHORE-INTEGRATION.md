# Anchore, as this platform uses it

What this integration does, what it needs from an Anchore deployment, and what
each failure it can report actually means.

The methodology it implements is [Anchore.md](Anchore.md); the endpoints and
schemas are [anchore_5.22_openapi.yaml](anchore_5.22_openapi.yaml), which is the
final authority. The design argument is
[21 - Security Posture](../design/21-security-posture.md) §12-14.

---

## 1. What it is for

The platform already reads JFrog Xray. Anchore is the second scanner, and the
point of a second scanner is not more findings - it is **disagreement you can
audit**. Two questions become answerable:

- **What did Anchore find that Xray did not**, and how much of it is
  known-exploited? That is what decides whether running two is worth it.
- **What does Anchore know about a finding Xray also reported?** The advisory's
  prose, its EPSS score, and whether it is on a known-exploited catalogue - none
  of which the Xray versions this platform has met supply.

The second is the more valuable one on most days and it does not show up as a
bigger number anywhere. See §7.

---

## 2. What Anchore has to be able to do

Anchore **pulls the image itself**, from its own network. This platform never
uploads anything to it.

| Requirement | Where it is configured | What happens without it |
|---|---|---|
| Reach the internal registry a release lands in | **In Anchore**: `POST /v2/registries` | Every image reports "Anchore would not accept this image for analysis" |
| Credentials for that registry | **In Anchore**, same place | Same |
| An account whose credential this platform holds | `coordinator.security.anchore.secretName` | "Anchore refused the credential" |
| Permission to create Applications and Versions | Anchore RBAC | Findings still arrive; the release is not grouped, and the transcript says so |

**Anchore does not need to reach the vendor's registry.** It analyses the
*internal copy* - the one this platform replicated. That is why
`anchoreEnabled` belongs on a target.

---

## 3. Configuration

Two places, and only one of them is per product.

### The deployment (system configuration)

```yaml
coordinator:
  security:
    anchore:
      endpoint: https://anchore.example.com   # `/v2` appended if absent
      secretName: anchore-api                 # <secretsDir>/anchore-api/{username,password}
```

Everything else has a default that works:

| Key | Default | Raise or lower it when |
|---|---|---|
| `concurrency` | 12 | Anchore rate-limits (lower), or a first sync of a 200-image release is slow and Anchore has headroom (raise) |
| `requestTimeout` | 60s | A single image's vulnerability list is large and times out |
| `analysisWait` | 10m | Analysis routinely takes longer here (raise), or you submit at transfer time and sync later (set negative to skip waiting) |
| `pollInterval` | 15s | Rarely |
| `submit` | true | Your own pipeline registers images with Anchore and you want this platform to read rather than add |
| `sbomFormat` | `spdx-json` | Your tooling reads CycloneDX |
| `account` | (empty) | An admin credential must act as another account |

**An empty `endpoint` means this deployment has no Anchore**, whatever a product
document says. A product asking for it is told that, rather than failing every
sync against a URL nobody set.

**A missing or unreadable credential is a warning at startup, not a refusal to
start.** An unreachable scanner must not be an outage: a Coordinator that
refused to start over a secret that has not been projected yet takes down
replication, discovery, promotion and every read of everything already scanned,
to protect a feature whose absence is one tab.

### The product

```yaml
spec:
  targets:
    - name: internal-jfrog
      registry: artifact.example.com
      repository: apm0014228-oci-stage
      type: jfrog
      credentialsRef: {secretName: cfx-jfrog-secret}
      xrayEnabled: true
      anchoreEnabled: true      # <- the whole of it
```

One field. Unlike `xrayEnabled` it is valid on **any** registry type, because
Anchore pulls over the registry API rather than being a JFrog endpoint.

Setting it on a **source** is a warning: it asks your Anchore to reach a
vendor's registry across the internet, with a credential it does not have, for
images already sitting in your own.

---

## 4. What a sync does

Press **Sync vulnerabilities** on a release, or **Sync Anchore only** from the
menu beside it.

```
1  Take stock       one request: what does Anchore already know about these 157 images?
2  Submit           only the images it has never been told about, BY DIGEST
3  Wait             up to analysisWait, saying how many are left
4  Read             one request per analysed image, in parallel
5  Group            find-or-create Application + Version, associate, READ BACK
```

**A first sync of a release is mostly step 3.** Anchore analyses a typical
container image in one to three minutes and works through a release in parallel,
but a 157-image release submitted from cold will not all be finished inside ten
minutes. That is not a failure: the sync records what did finish, reports the
rest as still analysing, and **pressing Sync again picks them up**. The
transcript says exactly this.

**A re-sync is cheap.** Stored answers are keyed by image and releases of one
product share nearly all of theirs, so a sync asks about the images it has no
answer for or whose answer is past `coordinator.security.maxAge`.

### The grouping, and why the read-back matters

The release becomes `Application: <product name>` / `Version: <release tag>` in
Anchore's own interface, with its images associated. A successful write is not
evidence of the final state, so the associations are read back and reconciled -
an application version holding three quarters of a release reports three
quarters of the truth while reading like all of it, and the transcript says
which it is.

Images Anchore has not finished analysing are **not** associated. They have no
vulnerabilities, so associating them would report the version as covering an
image it cannot speak for. The next sync picks them up.

Images somebody else put in that version are **left alone** and reported, not
removed. Deleting them would be this platform deleting somebody else's work.

---

## 5. What each failure means

Every one of these appears in the release's sync transcript with the sentence
that names the fix.

| What you see | What it means | What to do |
|---|---|---|
| **Anchore refused the credential** | 401/403 | Check the secret and that the account can read images |
| **Anchore is not answering at this address** | 404 on `/v2/account` | The endpoint is the UI host rather than the API host, or the deployment is not Anchore Enterprise 5.x |
| **Anchore would not accept N images for analysis** | The submission failed | Anchore has no registry configured for the host this release landed in |
| **Anchore has no record of this image** | Never submitted | `submit: false`, or the submission failed above |
| **Anchore has not finished analysing this image yet** | Still working | Sync again in a few minutes |
| **Anchore could not analyse this image** | Terminal failure inside Anchore | Look at the image in Anchore; it will not fix itself by waiting |
| **This image is not in the registry Anchore pulls from** | The release has not been replicated | Transfer the release, then sync |
| **Anchore is still analysing N images after 10m** | The wait ran out | Sync again; nothing is lost |
| **Anchore would not group them under an application version** | RBAC, or a duplicate application name | Findings are unaffected; check Anchore's Applications |
| **N applications are named X** | Two applications share a name | Resolve the duplicate in Anchore. This platform refuses to guess: picking one silently would split a product's releases across two applications |

---

## 6. Known-exploited vulnerabilities

The field this integration is worth having for, and the one Xray does not
supply here. Anchore reports it as `nvd_data[].is_kev`.

**It is not a severity.** "Critical" is a judgement that a vulnerability *would*
be bad to exploit; a KEV is a record that somebody *has*. So in this platform it
outranks severity everywhere: the default sort, the search order, the export's
row order, its own segment on the release page, its own badge, and its own line
in the sync transcript.

**"0 known-exploited" means two different things** and the interface says which:

- With Anchore switched on: a scanner with the catalogue looked and found none.
- With only Xray: nobody checked. The page says that, and names the scanner that
  would answer.

The **Exploited only here** column in the scanner comparison is the number that
decides whether Anchore stays switched on. Two thousand extra lows nobody will
read and one exploited advisory nobody else saw look identical in a plain
"unique findings" count.

---

## 7. What the enrichment actually buys

A finding both scanners reported becomes **one row that knows both**:

| Field | Typically from | What is lost without the merge |
|---|---|---|
| Severity | the worse of them | - |
| CVSS vector | Xray | The people who work in vectors |
| Description | Anchore | Every table row's "no description supplied" |
| Fixed version | either | An upgrade somebody could have asked for |
| Known-exploited | Anchore | The whole of §6 |
| EPSS | Anchore | The prioritisation between equal severities |
| Vendor vs NVD grading | Anchore | The evidence behind the one severity shown |

The **Enriched** column in the scanner comparison counts this: advisories the
other scanner also reported, where this one supplied a fact the other lacked. A
scanner whose exclusive-finding count is zero has still earned its place if it
explained several thousand findings better, and a comparison that could only
count rows would recommend switching it off.

---

## 8. What is downloaded and kept

Both scanners' raw bodies are stored separately and are separately
downloadable. The vulnerability response for one image exists once per scanner -
they are different documents about the same bytes, which is exactly why somebody
sends one to a vendor.

- **Beside each image**: a download menu offering each kind, plus one entry per
  scanner once two of them hold one.
- **The release bundle** (`security/export?format=zip`): one file per scanner
  per image per kind, under `<kind>/<image>__<tag>/<scanner>.json`.
- **The workbook** (`?format=xlsx`): a **By source** sheet and a **Scanner
  disagreement** sheet listing every advisory only one scanner reported - the
  full set, where the page caps its lists at 200.

Nothing is regenerated from this platform's own model. A CycloneDX document
rebuilt from our component list would be a different document with the same
component names in it, and handing that to somebody's compliance team is worse
than handing them nothing.

---

## 9. Anchore endpoints this integration uses

Everything else in the 5.22 API is untouched.

```
GET  /v2/account                                             health and credential check
GET  /v2/images?image_status=all                             take stock, one request
POST /v2/images                                              submit, by digest
GET  /v2/images/{digest}/vuln/all                            the findings, and the raw body
GET  /v2/images/{digest}/sboms/{format}                      on demand, behind the button
GET  /v2/images/{digest}/check                               the policy gate
GET  /v2/images/{digest}/content/malware                     the malware verdict
GET  /v2/images/{digest}/vex/openvex                         where a deployment records VEX
GET  /v2/applications                                        find the Application
POST /v2/applications                                        create it
GET  /v2/applications/{id}/versions                          find the Version
POST /v2/applications/{id}/versions                          create it
GET  /v2/applications/{id}/versions/{v}/artifacts            READ BACK the associations
POST /v2/applications/{id}/versions/{v}/artifacts            associate an analysed image
GET  /v2/applications/{id}/versions/{v}/vulnerabilities      the release-level report
```

`force` is deliberately never sent to `POST /v2/images`: it discards the
existing analysis and starts again, for bytes that cannot have changed.
