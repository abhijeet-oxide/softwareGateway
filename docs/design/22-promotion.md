# 22. Promotion

> **A promotion has always worked. This is about it being FAST, and about the
> shape that keeps it fast for the second registry vendor as well as the
> first.**

Related: [01](01-domain-model.md) §3.4 (promotion is a transfer whose origin is
a target) · [05](05-transfer-engine.md) §6 (the engine) ·
[18](18-quay-replication.md) §7 (the strategy seam this extends) ·
[02](02-configuration.md) §5.2 (`promotionOnly`)

## 1. What was already true

Promotion is not a subsystem. A promotion is a `transfer_requests` row with
`operation = 'promote'` whose `source_repo_id` happens to reference a
TargetRepository, and the engine has never cared about roles
([01](01-domain-model.md) §3.4). No separate planner, queue, state machine or
retry logic; the same `Repository` interface on both ends.

It is also already cheap. Lab and production commonly share a registry, so
every blob relocates by cross-repository mount and no bytes cross the wire -
`internal/transfer/promote_test.go` asserts exactly that, and a 45 GB promotion
moving 0 bytes is the normal case rather than the exception.

## 2. What was still expensive

TALK.

A 260-artifact release is a manifest walk, a plan, several thousand `jobs`
rows, and a mount request per blob per destination repository. Tens of
thousands of round trips - to relocate content the registry could relocate in
one call per name.

The registry knows this. JFrog exposes
`POST /api/docker/{repo}/v2/promote`, which moves one image - manifest, layers
and all - between two repositories of the same Artifactory, server-side. A
release becomes one call per NAME rather than one per blob, and the planner is
skipped entirely.

## 3. Why it is a plugin and not an `if`

The obvious implementation is a branch in the expander:

```go
if bothEndsAreJFrog && sameHost { callJFrog() }   // correct exactly once
```

Quay has its own copy API. ACR has import. A site with shared storage behind
two registries has something cheaper than either. Every one of them is the same
shape - *a hop somebody else can do better than we can* - and every one would
add a clause to a branch that lives in the expander, which has no business
knowing what Artifactory is.

So a promoter is a plugin registered by name, exactly as a registry backend is
([06](06-registry-abstraction.md) §5):

```go
promote.Register("jfrog", jfrog.New)   // one line, in the composition root
```

`grep -rn "promote/jfrog"` finding only `cmd/coordinator/main.go` is the
mechanical form of "the engine does not know what Artifactory is", and depguard
enforces it. **Deleting `internal/promote/jfrog` must leave everything building
and passing, with every promotion falling back to a copy.**

### 3.1 Claim and Promote are separate calls

```go
type Promoter interface {
    Name() string
    Claim(h Hop) Verdict                            // configuration only
    Promote(ctx context.Context, h Hop) (Outcome, error)
}
```

The chain constructs *every* registered plugin in order to ask, so a plugin
that resolved credentials before it could say "not mine" would make a hop
between two Quay repositories fail on a missing JFrog password. Claim reads
configuration and nothing else; it is asked on every promotion and from a GET.

### 3.2 A refusal is a SENTENCE

`Verdict.Reason` is populated whether it claimed or not, and every caller shows
it. "JFrog could not promote this" is useless. This is the whole diagnosis:

```
lab is on artifactory.internal.example.com:9444 and jfrog-dr is on
registry.ericsson.example.com:9443: JFrog can only relocate within one
Artifactory, so the content will be copied instead
```

An operator who configured two hosts by mistake reads that in the promotion
dialog rather than discovering it as a 45 GB transfer.

### 3.3 Two claims is a configuration error

`promote.Resolve` asks in NAME ORDER - not registration order, so the answer
cannot depend on which composition root imported what first - and refuses when
two plugins claim one hop, naming both. A precedence rule would silently pick
one, and the operator whose hop went the slower way would have nothing to read.

## 4. Execution: a leader tick, not the expander

The expander CLAIMS and records. It does not promote.

A promotion is a call into somebody else's registry, and Artifactory
materialises the copy synchronously - a release with a few hundred large layers
on a busy instance genuinely takes minutes. Doing that inline would hold the
expander tick, and one slow Artifactory would stall every other product's
planning behind it. So:

| | |
|---|---|
| `planning` | the expander asks the seam |
| `promoting` | claimed; the names are recorded; no jobs exist |
| `succeeded` / `failed` | the runner finished and the destination was read back |

`promoting` is its own state and not a reuse of `syncing`, and the difference
is the SETTLE PATH. A mirror pulls from an upstream on its own schedule, so its
distinctive outcome is `diverged`: the tag moved, and what arrived is not what
was asked for - a mirror working as designed. A promotion is a copy *we* asked
one registry to make between two of its own repositories. There is no upstream
and nothing to diverge from, so a destination holding a different digest is a
**fault**. There is no `PromotionDiverged` event, and the absence is the point.

### 4.1 Names, not bytes

`promotions` records what the hop has to publish; `promotion_names` records
each name and whether it landed.

A registry relocating within itself already holds every blob. What it publishes
is NAMES - the release's tag, and the name each bundled component answers to -
so names are the only denominator a promotion has. Every byte column on such a
transfer is structurally zero, which means *"we did not move those and cannot
count them"*, never *"nothing happened"*. A client rendering a percentage from
them is inventing one ([18](18-quay-replication.md) §6.1, same rule).

Only TAGGED sites are published. Our engine moves CONTENT, so it must visit
every manifest and blob including the ones nothing names - an index's
per-platform children are reached by digest and carry no tag. The registry
already holds all of that.

### 4.2 Resumability

Names are marked off as they land, in document order, root first. A Coordinator
killed half way through a 260-name release resumes at the exact name rather
than re-issuing two hundred calls to discover they were already done - and
because the root goes first, an interrupted promotion has published a
consistent prefix rather than an arbitrary subset.

The claim carries `claimed_by` and a heartbeat, for the reason
[00027](../../db/migrations/postgres/00027_security_sync_heartbeat.sql) gives
about a security sync: a stopped heartbeat is the one honest signal that the
holder is gone, and on a single-Coordinator deployment "running on another
replica" is simply false.

### 4.3 Settled on what the registry SERVES

A 200 says Artifactory did something, not that it did *this*. After the names
are published the runner resolves the ROOT tag at the destination and compares
the digest. The failure worth catching is the one where a release is reported
promoted and the tag resolves to the previous version - nobody notices until a
cluster pulls it.

The root only. Verifying every name would be a HEAD per component - two hundred
round trips to re-prove what the same call already reported - and the
components are reached *through* the root. What a promotion cannot prove about
the tree, the destination verification stage proves for every transfer alike.

### 4.4 An unanalysed release is not claimed

The names underneath a release nobody has walked are not known, because
discovery records the root manifest and a list of what it references without
fetching it. Claiming on a partial tree would promote the root and silently
leave a bundle's components behind, which is worse than being slow. So the
expander declines, the copy path derives the names for itself by walking the
registry, and the dialog says which button makes the fast path available.

## 5. The JFrog promoter

`internal/promote/jfrog`. It claims when, and only when:

1. **Both ends are declared `type: jfrog` (or `artifactory`).** A target
   configured `generic` against an Artifactory host is deliberately not
   claimed: configuration says what this deployment intends to speak, and
   inferring JFrog from a hostname would make the fast path depend on DNS.
2. **One host.** The real constraint, and the reason the fast path exists at
   all: promotion is an internal relocation, and there is nothing internal
   about two hosts.
3. **Both paths yield a repository key** (§5.2).
4. **The hop moves.** Two targets resolving to one JFrog coordinate are one
   place under two names.

### 5.1 `copy: true`, always, and never configurable

> **JFrog's promote MOVES by default.**

`copy: false` deletes the image from the source repository - which for a
promotion means lab is emptied the instant production is filled, silently, on a
successful-looking request. It is the single most destructive thing this system
could do.

So `copy` is set explicitly on every call, is never read from configuration,
and there is no option to turn it off. A "move" mode would be a footgun with no
user.

### 5.2 The repository key

JFrog serves Docker two ways and only one puts the key in the path:

| deployment | address | key |
|---|---|---|
| repository-path | `acme.jfrog.io/docker-prod/nokia/orbs` | first path segment |
| subdomain | `acme-docker-prod.jfrog.io/nokia/orbs` | in the hostname |

There is no reliable way to tell them apart from a hostname, so the derivation
covers the common case and `jfrogRepositoryKey` covers the other - the same
arrangement `xrayEndpoint` uses, and for the same reason. Getting it wrong is
cheap to diagnose because a 404 names the URL it asked and the key it derived.

`jfrogEndpoint` does the same for the platform base URL, falling back to
`xrayEndpoint`: they are the same host reached for the same reason in every
ordinary estate, and a second field to fill in is a second field to forget.

Both are rejected on a non-JFrog target. A `jfrogRepositoryKey` on a target
typed `generic` is well-formed, parses, and would be silently ignored - which
is exactly the failure `xrayEnabled` validation exists to prevent, arriving
through configuration instead of through code.

### 5.3 Credentials

The same `registry.ClientConfig` a transfer gets - credential, CA bundle,
proxy, timeouts - resolved through `internal/regclient`. A promotion path that
resolved its own would reach a host by a different route from the one that
replicates to it, and the day the two disagree is the day promotion fails
against a registry every transfer reaches perfectly.

Basic auth, not the OCI token exchange: this is the Artifactory REST API, and a
bearer token from the docker token realm gets a 401 that reads like a
credential problem. An access token in the password field works unchanged.

A 403 here is nearly always a credential that can pull and push over the docker
endpoint and still lacks the Artifactory permission promotion needs, so the
message says so rather than saying "forbidden".

## 6. Choosing an origin

`promote` with no `--from` means "from where it is". In order:

1. **The product's promotion path**, when its source environment names exactly
   one target. That is what the operator wrote down, and it holds even for a
   release that has not landed yet.
2. **The target that actually HOLDS the release**, when configuration cannot
   settle it. `lab-eu` and `lab-us` are indistinguishable as configuration and
   completely distinguishable as history, and asking somebody to type `--from`
   when only one of them has ever received the release is asking them to
   re-state what the system knows.
3. **Nothing** - refused, naming every candidate. A release in two labs is two
   different promotions and picking one would be a guess.

Step 2 only ever NARROWS: a hop configuration already resolves is unaffected.

## 7. The one question, answered on the server

`GET /api/v1/products/{product}/packages/{package}/promotionOptions`

"Where can this release go, and what will happen if I send it there" reads like
a client-side join. It is not: the answer needs the promotion path,
`promotionOnly`, which targets already hold the release, whether its tree has
been walked, and which plugin would claim each hop. A client assembling that
would be re-implementing `internal/transfer`'s resolution rules in TypeScript,
and the copy that drifted would be the one people clicked.

It is a GET and stays one - every input is configuration or a row, nothing
leaves the process - so a dialog nobody can safely reopen does not exist.

`defaultOrigin` mirrors §6 step for step, and `method` comes from the same
plugin claim the expander will make, so **the dialog cannot promise a fast path
the transfer then declines to take.**

`defaultDestinations` is empty rather than guessed when several targets share
the destination environment: `production-eu` and `production-us` are two
promotions, and pre-ticking both would make sending a release to a region
nobody asked about the path of least resistance.

## 8. Surfaces

| | |
|---|---|
| `transferctl promote <product> <package>` | top level, beside `download`. With no `--to` and nothing to deduce it LISTS the destinations and what each would do, rather than guessing |
| `transferctl transfers promote` | the same command, unchanged |
| `--dry-run` | resolution, plus whether each destination would be relocated or copied |
| `transferctl transfers describe` | a `relocate` transfer renders names published, never a byte account |
| Package page → **Promote** | beside Download, because those two are the release's whole life on that page |
| Packages listing → **Promote** | the row's one verb once the release has landed. `View download` moves into the row menu: promoting it is what somebody is about to do, looking at the download that brought it is what they might, and a row has space for one of those |
| Downloads page → **Promotions** | its own table, not a filter on the downloads one - see §8.1 |

A release's TIMELINE gains a third moment - `Published → Downloaded →
Promoted` - by the same rule the first two follow: only facts with an instant
attached. A promotion has a completion timestamp, and it is the one that
matters most to anybody asking what production is running. Each stage carries
its own mark and colour rather than a third identical dot: the three are
different KINDS of event, and the colours run cool to warm in the order the
release travels.

**Promote is the only orange verb in the application.** It is the one action
that changes what production pulls - download brings bytes into a lab and is
reversible by ignoring them, this is the step somebody books a window for.
Green would file it with the safe verbs and red would read as a failure. The
Promoted stage on the timeline wears the same colour, so somebody who pressed
the button recognises where its result landed.

**The button disappears once there is nowhere left to send the release.** Every
target already holds it, so the only honest outcome is a dialog saying so - and
a control whose whole job is to explain that it does nothing is worse than no
control. Decided from configuration and the transfer history rather than by
asking the server, because a page that fetched promotion options for every
release just to decide whether to draw a button would pay for the dialog on
every visit that never opens it.

`operation` is what makes any of this possible. It comes from the REQUEST -
`replicate` or `promote` - and reaches the transfer listing, a package's
history and the join that gives a listed release its state. A field dropped
anywhere on that path is a field the surface cannot see however carefully it
asks, which is how a promoted release first showed a menu with no promotion in
it.

Two states follow from it: **PROMOTING** and **PROMOTION FAILED**, distinct
from their download equivalents. A release being promoted is not downloading -
it arrived days ago - and a page saying otherwise sends somebody to look at a
vendor link for a problem between two internal registries.

### 8.1 Why promotions are their own table

Not a filter on the downloads listing, because the columns differ:

- A download has ONE route, the vendor to wherever it lands, so the downloads
  table never shows it. A promotion is defined BY its route, and `lab →
  production` beside `lab → dr` is the comparison somebody opened the page for.
- A promotion has a METHOD, which no download has. Relocated in seconds or
  copied is most of why anybody looks.

Both listings gained the ROUTE, though - `vendor → jfrog-lab` for a download.
"Which of my three targets did this actually land in" was a question the
listing could not answer at all; the reader had to open the transfer.

They also gained the right PACKAGE NAME. A transfer's `packageName` came from
its ORIGIN repository, which for a promotion is a target - so the listing named
a promoted release after the lab it was being promoted out of. It comes from
the package's own source repository now. A release is called what the vendor
published it as, whatever it has been copied to since, and the origin is not
lost: it is the left half of the route.

One table rather than ongoing and finished, because a native promotion is
normally over before the page refreshes: splitting it would give one empty card
and one with everything in it.

The page asks the API TWICE - `operation=replicate` and `operation=promote` -
rather than splitting one page of a hundred rows in the browser. A busy
estate's hundred most recent transfers are all downloads, so a client-side
split would empty the promotions table on exactly the deployments that promote
the most.

The dialog states where the release is (a sentence when it is not a choice, a
control only when it is), lists every destination with its method and the
reason for it, and disables the ones that already hold the release rather than
hiding them - "production already has 23.8.1076" is the answer somebody came
for as often as the promotion is.

## 9. What is deliberately absent

| | |
|---|---|
| A "move" mode | §5.1. Nobody promoting lab to production wants lab emptied |
| A `--method` flag | The method is a property of the pair, not of the request. Forcing a copy where a relocation is available is asking for a slower, identical result; forcing a relocation where it is impossible is a request that cannot be honoured |
| `diverged` for a promotion | §4. There is no upstream to diverge from |
| Per-name verification | §4.3 |
| Promotion of an unanalysed release, natively | §4.4. It still promotes - by copy |
| A promoter that also decides WHETHER to promote | Authorisation and destination resolution are the requester's, and a plugin sees a hop that has already been resolved and allowed |
