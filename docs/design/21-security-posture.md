# 21 - Security Posture

> **Consumed by:** [02](02-configuration.md), [03](03-persistence.md), [06](06-registry-abstraction.md), [09](09-api.md), [19](19-user-interface.md)
> **Status:** implemented for JFrog Xray, through the JFrog repository plugin.

---

## 1. What changed about the product

The platform moved software. It now answers a question about the software it
moves: **is this release safer than the one it replaces?**

That is one sentence and it implies almost everything below. It is not "show me
the vulnerabilities" - a list of 1,286 findings answers no question anybody
asked. It is a comparison, in plain language, that a release manager can act on
in ten seconds, with every piece of evidence behind it for the people whose job
that evidence is.

Two audiences, two depths, one dataset:

| | Question | Answer |
|---|---|---|
| **Simple** | Is B better than A? | One word, one sentence, five numbers |
| **Detailed** | Why, exactly? | Every finding, classified, attributed to a release, artifact, package and CVE |

## 2. The one distinction everything rests on

**"Scanned and clean" and "nobody looked" are both an empty list.**

Every design decision in this document falls out of refusing to conflate them.
A release manager who reads the second as the first ships an unscanned image
believing it is clean, and the system told them so.

So `Status` is a first-class field on every artifact's result, with five values,
and nothing anywhere renders findings without it:

| Status | Means | Conclusive? |
|---|---|---|
| `scanned` | The scanner has results. Empty means genuinely clean. | Yes |
| `not_scanned` | The scanner knows the artifact and has not indexed it. | No |
| `unsupported` | Nothing to scan - a signature, an attestation. | N/A - excluded from coverage |
| `disabled` | Xray is switched off for this repository. | No |
| `unavailable` | We asked and could not find out. | No |

`unsupported` is excluded from coverage arithmetic on purpose. A cosign
signature is not something Xray declined to scan; it is something there is
nothing to scan in, and counting it would pin every release permanently below
full coverage and teach people to ignore the number.

## 3. Where Xray lives, and why it is not a plugin of its own

**Inside the JFrog repository plugin** (`internal/registry/artifactory`), not as
a top-level `xray` plugin. Three reasons, in order of weight:

1. **It takes the same credential.** JFrog is already a supported repository
   type; every JFrog repository already declares a `credentialsRef`, a CA
   bundle, a proxy and timeouts. Xray sits on the same platform and is reached
   the same way. A separate plugin would declare all of it again - and the
   second copy is the one that goes stale, failing weeks later as an integration
   that quietly stopped answering while replication kept working.
2. **It is scoped by the repository.** "Is Xray on" is a property of a
   configured JFrog repository, not of the estate.
3. **It is not a registry.** It cannot list a tag or serve a blob, so it does
   not implement `registry.Source` and must not be forced to.

This mirrors [18 §7](18-quay-replication.md) exactly: Quay's `/api/v1` is a
second protocol on one host with a different credential, and it lives beside the
registry implementation rather than pretending to be one.

### Configuration

```yaml
spec:
  targets:
    - name: cfx-jfrog-lab
      registry: artifact.example.com
      repository: apm0014228-oci-stage
      type: jfrog                    # or `artifactory` - one backend, two spellings
      credentialsRef:
        secretName: cfx-jfrog-secret # THE SAME CREDENTIAL. There is no second one.
      xrayEnabled: true
```

**One field.** Everything else Xray needs, the repository above it already
states:

| Xray needs | Comes from |
|---|---|
| Platform URL | `registry` |
| Credential | `credentialsRef` |
| CA bundle, proxy, timeouts | `network`, inherited |
| Artifactory repository key | the first segment of `repository` |
| Which backend | `type: jfrog` |

It was briefly a nested block with eight keys - an endpoint, a repository key, a
watch list, a concurrency, a batch size, a timeout and two retentions. That was
wrong twice over. Half of them restated what the repository already said, and
the repository key restated it so directly that a mismatch between the two would
have reported the vulnerabilities of a different repository. The other half -
concurrency, batch size, timeout, retention - are not properties of a PRODUCT at
all: how hard to push a scanner is a property of the scanner and the network to
it, and how long to keep an index is a property of this deployment's disk. They
moved to the system configuration ([02](02-configuration.md) §8), where they are
set once instead of drifting between documents.

One escape hatch survives. `xrayEndpoint` overrides the platform base URL, and
is absent from almost every document: JFrog serves Docker two ways and only one
can be derived. A repository-path deployment puts everything on one hostname -
`acme.jfrog.io/docker-local/app` - and there the platform base URL IS the
registry host. A subdomain deployment gives each repository its own name -
`acme-docker.jfrog.io/app` - and there Xray lives at `acme.jfrog.io`, and asking
the docker subdomain returns a 404 that reads like a missing artifact rather
than a wrong base URL. There is no way to tell those apart from a hostname.

**`xrayEnabled` defaults to OFF**, which inverts the convention every other
`enabled` in this schema follows. Deliberately: the others turn off something
the document asked for, whereas this one would turn ON traffic to a third
system the document never mentioned.

`xrayEnabled: true` on a repository that is not JFrog is a **validation error**.
That document is not merely wrong, it is silently wrong - well-formed, applied,
never read - so the operator sees a repository they believe reports
vulnerabilities and which reports none. That is this feature's core failure
mode arriving through configuration instead of through code.

**`type: jfrog` is stored as `artifactory`.** Both spellings select one backend
and `repositories.registry_type` carries a `CHECK` constraint; admitting a
second value there means recreating the table on SQLite, which cannot alter a
constraint in place - carrying every column and index added since the initial
schema. The first attempt at this dropped `display_path` and `grouped_layout` on
the way past and the package listing stopped working. `RegistryType.Canonical`
costs nothing and cannot do that.

## 4. The provider boundary

```go
type Provider interface {
    Name() string
    Enabled() bool
    Scan(ctx context.Context, refs []ArtifactRef, opts ScanOptions) ([]Report, error)
}
```

One implementation exists. The interface exists anyway, and the argument is not
"we might add Anchore": it is that **without it, the core platform speaks Xray's
JSON**. Every handler, cache row, export column and React component would learn
that an issue has `cves[]` and `components{}` keyed by `deb://openssl:1.1.1n`,
and the second scanner becomes a rewrite rather than a package.

The cost of the boundary is one interface and one translation function per
provider. `internal/registry/artifactory/normalize.go` is the whole width of it
for Xray; nothing above that directory imports an Xray type.

`Disabled` is a null-object provider that answers every request with a disabled
report. A nil to check would be a nil somebody forgets, and the failure mode of
forgetting it is a release that renders as clean.

### The model

- **`Severity`** - five values, and `unknown` is a real one: a scanner that
  looked and cannot grade an issue has said something different from a scanner
  that has not looked.
- **`Component`** - identity is `deb://openssl`, **without the version**. This
  is the single most consequential modelling decision here. Xray identifies
  `deb://openssl:1.1.1n`; carrying that version into the comparison key would
  make an upgraded image whose openssl moved 1.1.1n → 1.1.1w while still
  carrying the CVE read as *one finding resolved and one introduced* - a patch
  release reporting a fix it did not make. The version rides alongside as data.
- **`Finding`** - one CVE against one component. Xray returns one issue naming
  several CVEs across several components; those are expanded at the boundary,
  because "CVE-2024-3094 in openssl" is one job and the same CVE in zlib is a
  different one.
- **`ArtifactRef`** - `ArtifactKey()` (the name, for cross-release alignment)
  and `Ref()` (the digest, for identity within one release) are **different
  methods** and confusing them is §5.

## 5. Comparison

### 5.1 Aligning artifacts

Two releases are aligned **by artifact name, never by digest**. Digests are what
change between releases; aligning on them reports every artifact as removed and
every artifact as added, which is a diff containing no information.

Renames are rescued by digest. Without it a renamed artifact is one removal plus
one addition, double-counting every finding it carries - once as resolved and
once as introduced.

| Artifact fate | Classification |
|---|---|
| Same name, same digest | `common` |
| Same name, different digest | `upgraded` |
| Only in B | `added` |
| Only in A | `removed` |
| Different name, same digest | `common` (rename) |

Two releases do not contain the same artifacts. Ten images in a base release and
two in a patch means eight unchanged and two upgraded, and a comparison that
assumed a fixed set would be wrong before it started.

### 5.2 Classifying findings

| Where | Classification |
|---|---|
| In both, same severity and remediation | `unchanged` |
| In both, re-graded | `severity_increased` / `severity_decreased` |
| In both, fix became available or unavailable | `remediation_changed` |
| Only in B | `introduced` |
| Only in A, on a common or upgraded artifact | `resolved` |
| On an `added` artifact | `introduced` |
| On a `removed` artifact, present elsewhere in B | `unchanged` - it **moved** |
| On a `removed` artifact, absent from B, coverage complete | `resolved`, marked `viaRemoval` |
| On a `removed` artifact, absent from B, coverage partial | `removed_artifact` - reported separately |

The last two rows are the rule the requirement asks for and they are worth
stating plainly. **A removal counts as a resolution only when B's coverage is
complete.** With an unscanned artifact anywhere in B, "the CVE is gone" may only
mean "the CVE is where nobody looked", and calling that a fix credits a fix
nobody made.

A finding that left one artifact and appeared on another has **moved, not been
resolved**. Counting it as resolved lets a repackaging read as a security
improvement.

### 5.3 The verdict

```
score = weight(introduced) - weight(resolved) + Σ(weight(to) - weight(from))
weights: critical 1000, high 100, medium 10, low 1, unknown 0
```

The gaps are wide on purpose: **one critical must outweigh any number of lows.**
A release that trades one critical for fifty lows is better, and a linear scale
says otherwise - which tells a release manager to ship the worse release.

Re-gradings contribute the *difference* in weight, not the whole of the new
severity: a medium that became high is 90 points worse, not 100, because the
medium was already there.

`unknown` weighs nothing. It cannot be allowed to decide better-or-worse on its
own; a comparison resting on ungraded findings is reported inconclusive instead.

| Condition | Verdict |
|---|---|
| Either side has no scan results at all | `inconclusive` |
| score < 0 | `better` |
| score > 0 | `worse` |
| score == 0 **and** both sides complete | `unchanged` |
| score == 0 **and** coverage partial | `inconclusive` |

That last row is the interesting one. A zero score over partial data has two
entirely different causes - nothing changed, or the changes are in the part
nobody scanned - and they must not share a word. A *difference* found over
partial data is still a difference, so better/worse stand with a caveat; only
"no difference" is downgraded.

### 5.4 The sentence

> Release B is better than Release A. It resolves 3 high and 5 medium
> vulnerabilities and introduces 1 low vulnerability. 734 vulnerabilities carry
> over unchanged.

Composed on the server, not in the browser. A client deriving this from counts
is a client that will eventually derive it differently from the next client,
over the same data, and one of the two will be wrong in a release meeting.

It contains no CVE identifier, no percentage, no scanner name and no jargon.
That is the audience: somebody deciding whether to ship this afternoon who has
never read an advisory. Everything technical is one click away.

## 5.5 Where the scanner runs, and why it is not the source

A release is **discovered** on a vendor registry and **scanned** where it lands.
Those are different registries, and conflating them was the first shape of this
feature and the one wrong assumption that made it useless on the only topology
that matters:

```
Nokia NEAR  ──replicate──▶  JFrog (artifact.example.com)  ──▶  OpenShift
   source                     target, Xray runs here
```

Scoping the security read to the repository a release was discovered in finds no
scanner at all and reports every release as "no scanner configured", on an
estate where Xray is switched on and working.

So the repository is CHOSEN, in this order:

1. A target the release has actually been transferred to. The scanner can only
   have indexed a copy that exists.
2. The default target. A release queued but not yet transferred will land there,
   and naming it produces "not scanned" - which is true and actionable.
3. Any remaining scanner-enabled repository, targets before sources.

`regclient.SecurityRepositoryFor` holds the ordering; the API supplies the one
fact it cannot know, which is where the release has actually been.

## 6. Sync, and why reading is not retrieving

**Exactly one thing talks to a scanner:** `POST …:syncSecurity`. Everything else
- the listing, the release view, the comparison, the search, the exports - reads
what that stored.

It did not start that way. Every read went to Xray, so a listing of twenty
releases was twenty scanner-backed reads to draw one column, and that column
shipped behind a toggle. A toggle is a design apologising for itself.

| | Before | Now |
|---|---|---|
| Listing column | 20 scanner reads | one join, always on |
| Release view | scanner, tens of seconds | database, instant |
| Comparison | two live retrievals | two indexed reads |
| Search | only what a page happened to cache | the index a sync wrote |

A sync is an explicit act with a durable result, and that is also what makes
search answerable at all: there is a table to search.

### The claim

Starting a sync is a **conditional UPDATE**, not a read-then-write. Two people
pressing the button, or a page that retries, would both read "not syncing" and
both start; the conflict target and the `WHERE` clause make exactly one caller
win, and the other is told "already running" - which is not a failure, because
the thing they wanted is happening.

`started_at` makes the claim **recoverable**. A Coordinator that dies mid-sync
would otherwise leave a release marked syncing forever, and a release that can
never be synced again is a worse outcome than a rare duplicate that converges on
the same rows. The maintenance loop releases claims older than 30 minutes.

### The heartbeat, and why an age was not enough

An age answers "has this been going too long". It cannot answer the question a
reader actually has, which is **"is anything running?"** - because a killed
process leaves exactly the row a healthy one leaves. So a release whose
Coordinator had just been restarted reported *"this sync is running on another
Coordinator; the result will appear once it completes"*, on a deployment with
one Coordinator, and refused a new sync for half an hour.

A running sync therefore **beats**: it renews `heartbeat_at` every 15 seconds
and the claim is honoured for 90. Three things follow, and each of them was
impossible before:

- **A stopped process is visible.** `sync.stalled` is a claim that stopped
  beating, and the interface says the sync was interrupted rather than
  inventing work happening elsewhere.
- **The next claim is not refused.** `Claim` takes a row whose heartbeat has
  expired, so "sync again" works immediately instead of waiting out the sweep.
- **A sync can be stopped from anywhere.** `POST …:cancelSecuritySync` releases
  the claim; the run notices at its next beat that it no longer holds one and
  stands down. That is what makes Stop work against a sync on another replica,
  where there is no goroutine to cancel.

`claimed_by` names the process holding a claim - host, pid and a random suffix,
so a restart is never mistaken for its predecessor.

A **stopped** sync is not a failure and is not recorded as one. The release goes
back to the state it was in before the run started - `synced` with its previous
result, or never synced - because a sync somebody stopped is a sync that did not
happen.

### Four states, not a timestamp

`package_security.state` is `'' | syncing | synced | failed`. "Has this been
synced" has two answers and needs four: never, running, done, failed are four
situations with four different things to offer, and three of them look identical
to a timestamp. Same argument migration 00021 makes for `analysis_state`.

A **failed** sync keeps the last good counts. A release that synced cleanly last
week and whose scanner is unreachable today still knows what it knew, and
showing nothing when something dated is known is the worse of the two answers.

## 7. Storage, and why nothing is deleted on a clock

**Xray is the source of truth for detailed findings.** It re-grades issues,
learns new fixed versions and re-scans continuously, and this platform must not
become a second copy of it.

For a long time the answer to that was expiry: every tier carried `expires_at`,
every read filtered on it, and a sweeper deleted what had passed. **That was a
correct cache and the wrong policy for a security index**, and it failed in the
way that is hardest to notice. `package_security` never expired, so a release
synced on Monday still said 90,808 vulnerabilities on Wednesday; the rows behind
that number lived in `security_findings` and had aged out. The reader was
looking at a release that had been scanned, had counts, and had nothing behind
them - and there is no sentence an interface can honestly put on that screen.

Worse, the deletion did not do the job it was there for. "Do not quote a stale
finding" is a good argument. Silently deleting the finding is not how you honour
it; it is how you lose the ability to say how stale it is.

**So rows carry `evictable_at` - the point after which they MAY be reclaimed -
and `last_used_at`. Reads serve them whatever their age**, with `retrieved_at`
alongside so the interface says "from the sync 3 days ago". Eviction is a SIZE
decision, taken by the sweeper, in this order:

1. Anything **unreferenced** - a payload whose scan row is gone, which no read
   path can reach - goes immediately.
2. Everything else is kept while the store is **inside its byte budget**.
3. Over budget, the **least recently read** evictable rows go, heaviest tier
   first, until it is back inside.

`coordinator.security.cacheBudgetBytes` is that budget, and **zero - the default
- means no ceiling**. Forgetting a security answer is the surprising behaviour
and should have to be asked for.

Five tables, split by what they cost and how expensive they are to rebuild:

| Table | Holds | Pinned for | Rebuilt by | Read by |
|---|---|---|---|---|
| `package_security` | One row per RELEASE: state, counts, coverage, when it was synced | Forever - it is the result of a sync | A sync | The listing, the release view |
| `package_security_sources` | One row per (release, scanner): its counts, and what only it reported | With the release | A sync | The source toggle, the comparison |
| `security_scans` | One row per ARTIFACT: status, counts by severity, fixability | `indexRetention`, 30 days | A sync (minutes) | The artifact table, comparisons |
| `security_findings` | Identifiers only - CVE, component, version, severity, fixed version. **No prose.** | With its scan | A sync (minutes) | Search, comparison, navigation |
| `security_details` | The complete normalized report, gzipped | `detailRetention`, 7 days | One request per image | Descriptions, references, CVSS vectors, malware, violations |
| `security_documents` | The scanner's OWN bodies - vulnerability response, SBOM, policy verdict, malware - gzipped | `documentRetention`, 30 days | One request per image | Downloads, the evidence bundle |

The order the budget is spent in follows the "rebuilt by" column, and that is
the whole argument. A document is megabytes and one request rebuilds it. A
detail payload is kilobytes and one request rebuilds it. A scan row plus its
findings is the durable result of a whole sync, and rebuilding it is minutes of
somebody else's scanner - so **the index tier is not in the budget at all**.

When the prose has gone the findings are still complete enough to list, filter,
compare and export - they simply lack the paragraph.

### The row identity that made one sync report two totals

A release reported **90,808 findings on its listing row and 86,085 on its own
security tab, from the same sync**. Neither page was wrong about its own
arithmetic: the listing quotes what the sync summed in memory, the tab counts
the rows that reached `security_findings`, and 4,723 findings did not survive
the trip.

The table's unique key was `(scan, CVE, issue, component_id)`, and
`component_id` deliberately carries **no version** - `alpine://libcrypto3`,
never `alpine://libcrypto3:3.5.5-r0`. That is the right identity for comparing
two releases (see §5) and the wrong one for a row: an image holding two builds
of one package, which any multi-stage build does routinely, has two things to
upgrade and wrote one. `ON CONFLICT DO NOTHING` discarded the second silently.

Two identities, then, for two questions. `Finding.Key()` answers "is this the
same problem as the one in the other release" and must not carry the version.
`Finding.StorageKey()` answers "is this the same row" and must. And the provider
collapses exact duplicates before handing findings over, so **the total and the
rows are computed from one list** - a page can quote two numbers for one sync
only if two lists exist.

The same confusion had a second half. `distinct_total` counts (CVE, package)
PAIRS - openssl and libssl3 carrying one advisory are two - and the interface
printed it under the label "unique CVEs", where a reader counts one. Both
numbers are worth having; what was not worth having was one wearing the other's
name, so `distinct_cves` is stored beside it and each is labelled for what it
counts.

**Every row carries `product`, `repository` and `provider`, and every statement
filters on all three.** That is an authorization boundary, not a filing
convention: findings were retrieved with one repository's credential under that
repository's Xray permissions. Two products pointing at the same digest get two
rows even though the bytes are identical, because serving one product's cached
findings to the other would disclose a security posture the asker was never
entitled to. **A cache keyed by digest alone would be a cross-tenant leak with
good performance.**

Other rules that are easy to get wrong and were:

- **A counts-only write must not clear the detail tier.** A package listing asks
  for counts; if that cleared the details, the next person to open a release's
  findings would re-query the scanner for every artifact, caused by a listing
  that renders one column.
- **A re-scan that resolved a finding must remove its index row.** A merge that
  only upserted would leave a search naming an image that no longer has the
  problem, which is worse than not having a search.
- **A disabled report is never cached.** It is a fact about configuration and
  would outlive the change that fixes it.
- **Age is never a read filter.** A summary row is the durable result of a sync,
  and hiding it because a clock passed turns "synced three days ago" into "never
  synced" - the one distinction this whole feature exists to keep.
- **A read touches `last_used_at` and nothing else.** Eviction is least
  recently USED, and a cache that cannot tell which rows anybody looks at evicts
  the hot ones first. Batched per chunk, and best-effort: losing a touch costs a
  row its place in the queue, never its contents.
- **Refresh drops every tier**, documents included. Leaving last week's raw
  scanner payload beside this minute's findings would hand somebody an export
  whose two halves disagree - and the raw half is the one they forward on.

Browser caching is `private, max-age=60, must-revalidate` with an **ETag over
the findings themselves** rather than over a timestamp, so a re-scan that
produced identical results does not invalidate anybody's copy. `private` because
these are one repository's findings and a shared cache must never hold them.

## 8. Retrieval

Batched and parallel, and **both numbers are ceilings rather than settings**.
They are system configuration under `coordinator.security`, not product
configuration:

- **`batchSize` (50)** is the largest batch. It bounds the blast radius of one
  failure: a failed call costs fifty artifacts' results, not a release's.
- **`concurrency` (10)** is the most that may be in flight. It bounds what we do
  to Xray, whose summary endpoint is expensive server-side and rate-limited on
  hosted JFrog. A budget PER SYNC, so two releases syncing at once are two.

### The pacer: why they are ceilings and not constants

A real sync, 260 artifacts:

```
2:00:06  Sync started. 260 artifacts.
2:00:08  Requesting scan results for 157 images.
2:13:18  JFrog Xray timed out on 13 artifacts. Retrying as two smaller
         requests. (x24)
2:14:28  Sync finished.
```

Fourteen minutes, thirteen of them spent discovering the same fact over and
over. Every batch went out at fifty, every batch waited its full sixty-second
timeout, and every batch then halved itself - **sequentially**, inside the
concurrency slot it already held, so a batch needing four splits paid four
timeouts one after another while nine other workers sat idle. Twenty-four of
those messages is twenty-four independent rediscoveries that this Xray, right
now, cannot answer fifty checksums in a minute.

Nothing in that design could learn. The batch size was a constant, the
concurrency was a constant, and the only feedback path threw its knowledge away
when it returned.

So a **pacer** (`internal/registry/artifactory/pacer.go`), shared by every
request to one scanner and outliving any single sync, runs additive-increase /
multiplicative-decrease on both dials:

- A **timeout** halves the batch. The first timeout still costs a minute; the
  twentieth does not happen, because by then the batch is twelve.
- A **429** halves the concurrency instead. "Too many at once" and "too much at
  once" want opposite corrections, and answering one with the other makes a sync
  slower without making it quieter.
- A **run of clean requests** grows the batch back, then the concurrency, after
  a short backoff - so one bad minute does not slow every later sync against a
  scanner that has recovered.

Both floors are well above one. A scanner having a bad minute should get slower,
not stop: a pacer that collapses to one artifact per request turns a slow sync
into one that cannot finish inside the claim window at all.

Two mechanics make that work. **Work is handed out by a queue** rather than
partitioned up front, so a worker asking for its next batch gets the size the
scanner has just proved it can answer - the old shape cut every batch at fifty
before the first answer arrived. And **a split batch goes back on the queue**
instead of recursing: the halves are picked up by whichever worker is free, in
parallel, inside the same global allowance. The load on the scanner is unchanged
- the semaphore decides that, not the call graph - and the wall clock is not.

The allowance is a semaphore whose capacity moves, not `errgroup.SetLimit`:
shrinking is acquiring ballast and growing is releasing it, so **no in-flight
request is ever cancelled to make a limit true**.

When the pacer had to move, the sync log says so once, at the end: "JFrog Xray
was slow, so requests were made smaller: 12 images per request and 4 requests at
a time by the end." That line is the answer to "why did that take eleven
minutes", and without it the only evidence is a wall clock and a shrug.

### The probe that follows a scan

Xray answers "Artifact doesn't exist or not indexed/cached in Xray" for two
situations with nothing in common: an image it has not looked at, and an image
that was never replicated there. The first is a scan waiting to happen; the
second is a TRANSFER waiting to happen, and reporting it as a scanning gap sends
somebody to the wrong team. Artifactory knows which, so it is asked.

**In bulk.** One AQL query answers a hundred images, so a release costs about
three requests. It was one request per image - the only phase whose request
count scaled with the number of artifacts rather than the number of batches, and
it ran in exactly the situation somebody is already waiting on a slow answer: a
release that has not been replicated yet, where *every* image needs the probe.

AQL is administrator-only on some platforms. A refusal is recorded once per
client and the per-image search runs instead - the same question asked the slow
way, not a degraded answer.

**A per-artifact failure is a report with `unavailable`, never an error.** One
image the scanner would not answer for must not lose the other hundred - and,
critically, must not silently become an image with no findings. An error return
is reserved for a cancelled context.

**A batch that times out is halved and asked again, down to one artifact.**
Xray's summary cost is superlinear in the batch: fifty checksums can exceed
`requestTimeout` where two lots of twenty-five each finish comfortably, and on a
258-artifact release that difference was 209 artifacts reported `unavailable`
for no reason but batch size. The halves go back on the queue rather than being
re-sent by the goroutine that failed - see the pacer above.

Only a failure that splitting can plausibly fix earns the retry: a client-side
timeout, a rate limit, or a 504/408 from the platform. An authentication
failure, a 404, or a cancelled context is re-asked in halves forever for
nothing, so those record and stop.

The last resort is honest words. `describeXrayFailure` turns each class into a
sentence naming the fix - the missing Xray permission, the `requestTimeout` to
raise, the `concurrency` to lower, the `xrayEndpoint` to correct - rather than
echoing a URL and a Go phrase. A body that will not parse is called out as an
answer we could not read, because saying "could not be reached" about a scanner
that replied sends somebody to check a network path that is demonstrably fine.

Progress travels **inside the security response**, not on a channel of its own.
The interface polls one cheap endpoint while a sync runs and gets both the live
position and whatever is already stored in the same answer; two endpoints would
be two requests that can disagree.

Live progress is present only on the replica running the sync. The stored state
is authoritative, so a sync running elsewhere still reports "syncing" - reading
it the other way round is how a two-replica deployment shows a spinner that
resets on every second request.

A spinner says the same thing for thirty seconds of scanner queries as it says
for a request that has silently stopped, and "is this working?" is the only
question anybody has while waiting. A user who cannot tell reloads the page,
which starts the whole retrieval again.

## 9. API

| Method | Path | Answers |
|---|---|---|
| `POST` | `/products/{p}/packages/{pkg}:syncSecurity` | **The only route that talks to a scanner.** Claims the release and returns immediately |
| `POST` | `/products/{p}/packages/{pkg}:cancelSecuritySync` | Releases the claim, wherever the sync is running. The release keeps its last completed result |
| `GET` | `/products/{p}/packages/{pkg}/security` | This release's stored posture and its sync state. `?detail=true` for findings |
| `POST` | `/products/{p}/packages/{pkg}:compareSecurity` | How the posture changed to `against`, from both sides' stored data |
| `GET` | `/products/{p}/security/search` | `?kind=cve\|package\|image&q=` |
| `GET` | `/products/{p}/packages/{pkg}/security/documents/{kind}` | One image's SBOM, raw vulnerability response, policy verdict or malware list. `?digest=` names the image |
| `GET` | `/products/{p}/packages/{pkg}/security/export` | CSV, Excel, JSON, ZIP |
| `GET` | `/products/{p}/packages/{pkg}/security/compare/export` | The comparison, same formats |
| `GET` | `/products/{p}/security/search/export` | Search results, same formats |
| `GET` | `/security/progress/{token}` | What a retrieval is doing |

Every route is registered **only when the dependency exists**, so a deployment
with no scanner answers an honest 404 rather than a route that always fails.

Search reads the index a sync wrote and **never the scanner**. It therefore
answers "is this CVE in a release somebody has synced", not "is it anywhere in
my estate", and the response says so on every result - including a full one,
naming the remedy. A search that silently returned nothing would be read as
"this does not affect us", which is the most dangerous thing this feature could
say wrongly.

It is one indexed SQL query over identifiers, so it is fast at any catalogue
size, and it is exactly as complete as the set of releases somebody has synced.

Exports are `GET` because a download is a link: a browser cannot follow a `POST`
to a file, and an export a user cannot bookmark is an export they screenshot.
Filters are applied **server-side**, because a client filtering its own copy
would export the first fifty of 1,286 rows into a file that looked complete.
Every row carries its whole address - release, artifact, package, CVE, and for a
comparison its classification - because a spreadsheet row is read out of order,
filtered, and pasted into a ticket.

### An export is the data, not a summary of it

A detailed export used to be a twenty-six-row field/value "Summary" sheet and
one flat findings sheet, and the summary VIEW was that first sheet on its own.
Nobody exports a spreadsheet to read a headline. So a detailed export is now the
**tables the interface shows**, in the shape a reader has already learned:

| Sheet | One row per |
|---|---|
| Unique CVEs | advisory, with every image and package it turns up in, and how many |
| All findings | (image, advisory, package) |
| Images | image, with its counts, its status and the sentence explaining it |
| Malware | malicious package |
| Policy violations | violation, with the watch, policy and rule that raised it |
| By source | scanner, present only where more than one contributed |
| Problems | reason the scanner gave, with the images it gave it for |

The field/value grid survives only under `?view=summary`, for the person pasting
one number into a release note.

A workbook carries every sheet. A **CSV carries one**, and which one is the
caller's choice (`?table=unique|findings|images|malware|policy|problems`),
defaulting to All findings - the interface passes whichever tab is open, because
a download that came back with a different table from the one on screen is the
complaint the parameter answers.

### The bundle

`?format=zip` is the evidence bundle: the tables as CSV **beside the scanner's
own responses**, laid out one directory per kind, then per image, then per tag.

```
README.txt
tables/unique-cves.csv, all-findings.csv, images.csv, ...
vulnerabilities/<image>/<tag>/jfrog-xray.json
malware/<image>/<tag>/jfrog-xray.json
policy/<image>/<tag>/jfrog-xray.json
sbom/<image>/<tag>/jfrog-xray.json
```

Kind first, because that is how it is consumed: somebody forwarding a
vulnerability report to a customer sends `vulnerabilities/`, and somebody asking
"is there malware in this release" opens one directory. Image first would put
the answer to that question in 157 places.

It reads storage and **never a scanner**: a download link that starts a
fifteen-minute retrieval is a link that times out somewhere between here and the
browser, and the user's only evidence is a truncated file. The bodies were
captured by the sync that was making those requests anyway. A release whose
documents were never retrieved gets the tables and a README naming the empty
directories - a worse bundle, honestly labelled, rather than a
`vulnerabilities/` directory holding four files of a hundred and fifty-seven.

The `xlsx` and `zip` writers are `internal/export`, written directly over
`archive/zip` and `encoding/xml`. The subset needed for a grid of strings and
numbers is one file; a dependency that renders charts and pivot tables is a
large supply-chain surface on a product whose purpose is telling people what is
in their supply chain.

## 10. Interface

Three rules, and they are the difference between a page somebody acts on and a
wall of red:

1. **Colour is never the only signal.** Every severity carries its word, every
   verdict a sentence, every state an icon whose *shape* differs. The pages read
   correctly in greyscale.
2. **An absence of findings is never rendered as safety.** Every component that
   can show an empty list takes a status and says which kind of empty it is.
3. **The simple view uses no jargon**, and the detailed view keeps all of it.

The security panel on a release is ordered by *decreasing trust*: whether these
numbers cover the whole release, then the numbers. The same panel with the
caveat underneath is a panel whose totals get quoted without it.

The listing's vulnerability column is **always on** and costs nothing: the
counts arrive with the listing itself. Where a release has never been synced the
row offers the sync rather than showing a blank cell, because a blank cell in a
vulnerability column reads as "nothing wrong with this one".

The release page is **tabbed** - Overview, Components, Security, Downloads -
with the tab in the URL, so "the security of this release" is a link somebody
can send. Stacked as cards, the security section lived below three screens of
manifest tree, and a reader who came to check a release's security should not
scroll past its contents to reach it.

## 11. More than vulnerabilities: SBOMs, policy and malware

Three things a scanner knows about an image that are not a list of CVEs. They
arrive from different endpoints, mean different things and are read on different
tabs; what they have in common is the only thing this platform needs, which is
that each is **a body the scanner produced about one artifact** that somebody
eventually wants to read, keep, and forward unaltered.

Modelling them as one concept (`security.Document`) is what makes the storage,
the retention, the eviction, the export tree and the download route one
implementation instead of four. The alternative - an sbom table, a violations
table, a malware table, three cache policies and three export writers - is the
version that never gets a fourth document type added to it.

**The raw body is kept**, even though everything else here is normalized,
because normalization is lossy on purpose and the person asking for an SBOM is
asking for the SBOM. A CycloneDX document regenerated from this platform's
component model would be a different document with the same names in it, and
handing that to somebody's compliance team is worse than handing them nothing.

Where each comes from, and what it costs:

| Kind | Retrieved | Why |
|---|---|---|
| `vulnerabilities` | Free, during a sync | It is the response the scan was already making. Captured through a sink so it never touches a `Report`, which is serialized into the cache on every sync |
| `policy` | One request per image, during a sync | `coordinator.security.documents`, on by default |
| `malware` | Free, with `policy` | It is the malicious subset of the same response, plus the scan's own malicious issues |
| `sbom` | On demand, behind the download | Minutes and tens of megabytes per image. Generating one for every image on every sync would turn a two-minute job into an hour, for a file somebody downloads occasionally |

**Malware is separated from vulnerabilities at the boundary**, not filtered
downstream. Ninety thousand vulnerabilities are a backlog somebody works through
over quarters; one malicious package is a release that does not ship tonight,
and returned in the same list it is row 43,712 of a table sorted by severity.
Xray has no `malicious` issue type - it reports a malicious package as a
security issue whose summary begins "Malicious Package" - so the match is
deliberately narrow: a false negative leaves a hit in the vulnerabilities table
where it is still visible, and a loose match moves ordinary CVEs into a tab that
means "stop".

**A policy violation is not a finding with a policy field.** A finding is "this
image contains CVE-2026-31789". A violation is "your Production watch forbids
critical fixable issues and this image has four" - it exists because somebody
wrote a rule, it disappears when the rule changes, and it can be raised against
a licence with no CVE anywhere near it. Folding them together produced a
vulnerabilities table with rows that were not vulnerabilities.

Every failure here is a **message, not an error**. `exportDetails` is absent on
older Xray versions, `violations` needs a permission a read-only scan token
often lacks, and neither is a reason to fail an export of vulnerabilities that
were retrieved perfectly well. An unavailable document is a document with no
payload and a sentence saying which of those happened.

## 12. Adding a second scanner

Everything above is scanner-agnostic except one directory. Concretely:

1. Implement `security.Provider` wherever that scanner's credentials already
   live - inside a repository plugin if it is scoped by repository, in its own
   package if it genuinely is not.
2. Normalize into `security.Finding`, keeping the component identity
   version-free (§4). Implement `security.DocumentProvider` too if it produces
   SBOMs or policy verdicts; a scanner that does not is asked for none.
3. Return it from `regclient.SecurityResolver.ProviderFor`.

Nothing in `internal/security`, `internal/store`, `internal/api`, `internal/export`
or the web application changes. That is what the boundary bought, and it is the
only thing it had to buy.

### What the second scanner turns on

The shape for comparing scanners is already in place, and deliberately so: a
field added the day Anchore is switched on is a field every stored row, every
export column and every table lacks for the releases synced before it.

- `Finding.Sources` names **every** scanner that reported a finding, where
  `Provider` names the one this row came from. They are different questions:
  which one said it, and who agrees.
- `Posture.BySource` and `package_security_sources` carry per-scanner counts and
  `only_cves` - the advisories that scanner reported and no other did. Stored
  rather than derived, because the listing that most wants the number is the one
  rendering twenty releases with none of their findings loaded.
- Everything above renders **nothing** while one scanner answers. A segmented
  control with a single position, and a "Reported by" column reading "JFrog
  Xray" on every row of three thousand, are chrome that teaches a reader to
  expect a comparison that does not exist.

The interface, once there are two, is two controls and no query builder. A
segmented switch answers the common question in one click ("what does Xray
say"), and a single select holds the **whole truth table as named sentences** -
only in Astra, in Anchore and Astra but not Xray, found by every scanner -
generated from whichever scanners answered, with counts beside each. For three
scanners that is seven rows, which is a list somebody reads.

The alternative is a filter builder with AND, OR and NOT: the answer that gets
built, demoed, and never used, because the person who needs it most is the one
who does not think in set algebra.
