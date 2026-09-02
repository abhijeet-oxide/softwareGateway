# 19 - User Interface

> **Prerequisites:** [09 - API](09-api.md), [13 - CLI](13-cli.md)
> **Status: DIRECTION. Nothing here is implemented, and v1 does not include it.** Scheduled at [M10](17-delivery-plan.md#m10--web-ui), behind the authentication gate in [17](17-delivery-plan.md) Q6. Recorded now because decisions taken today either make it cheap or make it expensive, and the difference is invisible until someone tries.

---

## 1. The statement

**The first release is CLI-centric. The design is API-first. A web UI follows.**

That is not a hedge. [09](09-api.md) already says the Coordinator's REST API is the *only* interface - `transferctl` is a pure API client that never contacts a registry and never opens a database connection ([00](00-overview.md) §5.3). The CLI is therefore not the product's interface; it is the product's **first client**, and its existence is the evidence that a second client is buildable without moving anything behind the API.

What the UI will be, stated so it can be held to: **clean, minimal, fast, fully optimised, enterprise-grade, and capable of everything the tool can do.** Deployable in-cluster, air-gapped, with no external dependency at runtime.

It is called **Software Gateway** on screen - the product owner's choice, and the same name as the system itself. An earlier draft of this document used *Software Lifecycle Manager* and kept `softwareGateway` off the interface entirely; that distinction bought nothing and is dropped. §3.1 still holds and is the part that mattered: the interface avoids the ENGINE's nouns - package, transfer, blob, wave - which is a statement about vocabulary rather than about branding.

## 2. Why say it now rather than at M10

Because three classes of decision are cheap today and expensive later, and all three are decisions we are otherwise making by accident:

| Decision | Made well today | Discovered at M10 |
|---|---|---|
| **Identity** | The `actor` field, its plumbing and its recording already exist and write `"anonymous"` ([12](12-observability-and-audit.md) §4.2) | An audit trail with a year of unattributable history |
| **Every capability behind the API** | Already true, and enforced by the CLI being a pure client | A UI that needs an endpoint the CLI never needed, added under time pressure |
| **Machine-readable errors and pagination** | RFC 9457 problem details, `pageSize`/`pageToken`, `filter`, `validateOnly` ([09](09-api.md) §1) | A UI parsing human-readable strings |

The first row is the one that cannot be retrofitted. The other two are already right, and this document exists partly to say **keep them right** - an endpoint added for the CLI's convenience that returns rendered text rather than data is a small, invisible step away from a UI that has to scrape it.

## 3. What it must do

Everything `transferctl` does ([13](13-cli.md) §2), because a UI that can do most of it forces users to keep a terminal open and then they never come back to the UI. Concretely: browse products and their health; see and trigger discovery; browse packages and inspect them; request replications and promotions with a dry-run preview (the promotion dialog states where the release is, every destination it can reach, and whether each hop would be relocated by the registry or copied - see [22](22-promotion.md) §8); watch transfers live; read failures grouped by cause; control the queue (pause, resume, cancel, retry, priority); manage schedules; verify signatures; read worker logs; compare two registries; and query the audit trail.

Plus the one thing the CLI does awkwardly and a UI does naturally: **comprehension at a glance** - which releases are where, which have diverged, and what is failing right now, without composing a query first.

Screen-by-screen intent, layout and the visual system live in the [UI generation brief](../ui/ui-generation-brief.md), which is a working document for producing mockups rather than a specification. **Ten pages, eight of them navigable:**

```
Home · Products · Software · Downloads · Repositories · Activity · Reports · Settings
```

with Software detail and Download reached from them. The lifecycle those pages exist to make obvious is **Discover → Review → Verify → Download & Replicate → Compare → Promote**.

### 3.0 Choosing happens where the data is

> **Decision - a comparison is selected on the Software listing, not on a page of its own.**
>
> Comparing two releases used to open a page whose entire content was a form: a
> product select, a two-position mode switch, two release dropdowns, a swap
> button and sometimes a fourth select for the source. Six controls to express
> "these two".
>
> The two dropdowns were the worst of it. A list of two hundred releases,
> rendered as a name and a version in a box 320 pixels wide, with no status, no
> date, no repository and no vulnerability counts - so everything a person needs
> in order to DECIDE which two releases to compare was on the listing they had
> just left, and none of it was where the decision was made.
>
> So the listing IS the selection surface. "Compare packages" turns it into one
> in place: the title becomes *Select two packages to compare*, a numbered pick
> column appears, and a bar above the table states what has been chosen. The
> reader keeps the search box, the status filter, the product filter, the
> sorting and every column they were already reading.
>
> *Three rules that make it work, and each was a failure first:*
>
> - **The selection lives in the URL.** It has to survive the search box, the
>   product filter and the round trip to the report - all of which destroy
>   component state. "Pick one, search for the other, lose the first" is the
>   exact thing that makes a two-step selection feel broken.
> - **The names resolve against every loaded release, not the filtered ones.**
>   Missing this reintroduced the same failure one layer up: the selection was
>   intact and the bar said *select the first package*, which is worse than
>   losing it.
> - **A change is a function of the LIVE URL, not of the last render.** Two
>   ticks 400 ms apart lost one; the second click decided against a selection
>   that had already moved. React Router's functional setter does not fix this -
>   it passes the render's own params.
>
> The report keeps a route of its own, because it is a page-sized answer somebody
> waits minutes for and sends to a colleague. What it no longer keeps is any way
> to choose: arriving there without a pair returns the reader to the listing.
>
> *The one form that survives* compares a release across two LOCATIONS - "did
> this arrive intact" - which is a question about one release rather than two
> and cannot be expressed by ticking two rows. It is reached from that release's
> own row menu, and is two selects about a release already named.

> **Decision - the intent is asked before the two releases, because it decides
> which releases are worth offering.**
>
> A comparison answers two questions - what the releases HOLD, and how their
> VULNERABILITIES differ - and they do not have the same candidates. A
> vulnerability comparison against a release nobody has scanned cannot say
> anything, so offering those rows is offering a choice that does not work, and
> the reader only finds out two clicks later on a page explaining its own
> refusal.
>
> So selection mode opens with a two-position switch, **Contents** or
> **Vulnerabilities**. Contents shows everything, because every release can
> answer it. Vulnerabilities hides the releases with no findings, says how many
> in one line, and moves the vulnerability counts to the second column - the
> table is wider than a laptop window, so column order is priority order, and
> the column somebody is deciding on should not be the one the pinned actions
> cover.
>
> The intent then travels with the pair, so the report opens on the answer that
> was asked for, and **only that comparison runs**. The two cost wildly
> different things: contents walks two manifest trees against their registries
> and takes minutes, vulnerabilities is two indexed reads of what a sync already
> stored. Somebody who asked about vulnerabilities should not wait on a registry
> walk they did not ask for.
>
> *Where the answer cannot be given*, the tab is disabled and names the release
> that has not been scanned - "25.10.1 has not been scanned yet". Both ends are
> required, because a difference needs two sides: one release scanned and the
> other not produces a verdict where every finding reads as introduced, which is
> a fact about what nobody scanned rather than about the release, and it is the
> most dangerous thing this page could say wrongly. A link asking for that view
> falls back to contents rather than erroring.

### 3.1 The UI speaks the user's language, not the engine's

> **Decision - the interface uses product vocabulary, and the domain model stays behind the API.**
>
> A Product Owner looking at SBC 25.8.1 should think "this release is new and signed, I'll download it". They should not meet *package*, *transfer*, *job*, *blob*, *placement*, *wave* or *replication mode* - every one of which is load-bearing in [01](01-domain-model.md) and none of which is their problem.
>
> The mapping is fixed, one term to one term, in the brief's vocabulary table: **Software** is a Package, **Download** is a TransferRequest and its Transfers, **Location** is a target repository, **Saved (already present)** is deduplication, **Download Rule** is a rule in the product's `download` block together with the chain its targets declare ([20](20-download-rules.md)), and **Configure Mirror to Quay** is a step of that chain, from `replication.mode: mirror` ([18](18-quay-replication.md)).
>
> *Why write it down rather than let it emerge:* two clients of one API will otherwise invent two vocabularies, and the day someone reads a CLI transcript next to a UI screenshot, neither of them can be trusted. The CLI keeps the domain words - its users are operators and the words are precise. The UI keeps the product words. The mapping is the contract between them.

**The most consequential consequence of this: there is no Replicate action in the UI.** Downloading a release into JFrog and configuring the Quay mirror that OpenShift pulls from are one operation with several steps, presented as one operation with several steps. Internally they remain a transfer and a replication-mode apply ([18](18-quay-replication.md) §7); the user is not asked to know that, and is never asked to perform the second half themselves.

### 3.2 How a label is written, not only which noun it uses

§3.1 fixes the **vocabulary** - which noun the interface uses for a domain
object. It does not fix the **grammar**, and every copy defect found in review
so far has been a grammar defect rather than a vocabulary one: `What has
happened` over a log (a fragment with no subject), `4 at a time` beside a
concurrency (a count with no unit), `about 2s left` (a hedge), `Analyzed in a
moment` (a duration in prose).

The rules are in `.claude/skills/interface-copy/SKILL.md`, which is enforceable
rather than aspirational - it carries the wrong/right table these came from and
a grep to run over a diff. In summary:

1. **A label names its subject and its verb.** `Run log`, not `What has
   happened`. `Remediation`, not `What to do`.
2. **Nothing is phrased as speech.** `4 charts in parallel`, not `4 at a time`.
   No hedges: either the number is known or the field is absent.
3. **A number states its unit and what it counts**, except where the label
   directly above supplies the noun.
4. **The reader is never addressed and the tool never refers to itself.** No
   *you*, *we*, *sorry*.

This applies to the Go strings that reach a screen as much as to the TSX -
`Stage.Label()`, progress event text, and an RFC 9457 `detail`, which is
rendered verbatim and is where the rule is broken most often.

### 3.3 Two long jobs, one panel

The vulnerability sync and the compliance check are the two things in this
product that take minutes against somebody else's registry. They are watched by
the same person, on the same release, one tab apart, and they answered the same
question - *is this working, and what is the answer going to be missing* - in
two different visual languages. The compliance run drew counters of real things,
a stage route with what each finished stage cost, and a timeline of what had
happened; the sync drew a bar and a row of grey sentences separated by middots,
in which the number that changes what the answer means (the artifacts the
scanner did not answer for) was drawn identically to the number that does not.

They now share the counters (`components/runtiles.tsx`) and the transcript
shape, and each kept what it did better:

| Taken from | What | Now on |
|---|---|---|
| Compliance | Counters of real things, failures coloured | Both |
| Compliance | The transcript on the panel, not only behind a button | Both |
| Security | A stored log with a button that reopens it after the job ends | Both - compliance kept its transcript on the run (see [23](23-compliance.md) §9.1) |
| Security | A travelling stripe until the first item lands, rather than a determinate bar at zero | Security; compliance has a real denominator from its first stage |
| Compliance | An estimate derived from the job's own rate, withheld until there is a rate | Both |
| Compliance | While the job runs, the job is the WHOLE tab | Both - Security drew its progress panel above the previous sync's summary band and findings tables, every number of which was real and none of which was the answer being fetched |

This is the same rule as §3.1 one level up: a reader who has learned a shape on
one tab should not have to learn it again on the next one.

### 3.4 One layout for a release's two verdicts

Security and Compliance answer the same shape of question about the same
release, one tab apart, and they were drawn as different products. They now
share the layout, top to bottom:

| Band | What it holds |
|---|---|
| A header row, outside any card | What produced the answer, as muted text on the left. The controls that change it as buttons on the right - `Sync log` / `Sync again`, `Rulebook` / `Run log` / `Re-check`. |
| One card, three hairline-separated zones | How bad it is (a headline number and the shape of it), what it is made of (a meter per severity), and whether the answer can be trusted (coverage meters, and the exceptions named individually). |
| One card: a Segmented view switch and the export beside it, a row of filters under it, then the table | |

The third zone is where the two differ and where the difference matters:
Security's confidence is *how many images were scanned*, compliance's is *how
many charts rendered, and how many checks that left undecided.* Same question,
different denominator.

**The findings table groups by default, in both.** Security groups occurrences
onto the CVE; compliance groups them onto the CHECK. It is the same fact about
both datasets: a release repeats one problem across everything it ships. 171
critical compliance findings on a real orb are **five rules** - "containers do
not run as root", "every image is pinned by digest" - each broken in twenty to
forty-four places, and the flat list spread that over 171 rows in which SEC-01
appeared forty-four times with a different chart name. It read as 171 problems
and it is five pieces of work. A row expands into a table of every occurrence,
and the identifier opens a panel with the rule and all of them.

Because compliance's grouping is done in the browser, over the page the server
returned, a group's "broken in 44 places" is only true if all 44 arrived. The
query asks for enough rows that the slices a reader works in - Critical,
Warning, Info, Unchecked - arrive whole; Passed and All are still a page, and
the view says it grouped from one rather than quoting a count assembled from it.

## 4. What it must never do

> **Decision - the UI is a read-and-operate client. It never edits configuration.**
>
> *Alternative considered:* let the UI edit products - the field every operator asks for within a week of seeing a read-only YAML pane.
>
> *Rejected because* configuration is GitOps ([02](02-configuration.md) §2): Git is the source of truth and Flux reconciles it. A UI that wrote configuration would create a second source of truth that Flux would silently revert, producing the worst failure mode in the product - a change that appears to work, works for a few minutes, and then undoes itself with nothing in any log to say why. The API already refuses this for the same reason ([09](09-api.md) §2, "Products are read-only over the API").
>
> *What the UI does instead:* shows the loaded configuration with its config hash and load time, shows **drift** between Git and any registry-side state it manages ([18](18-quay-replication.md) §8), and links to the repository. Requesting work - downloads, promotions, syncs, applies - is not configuration and is fully available.
>
> *Where this is hardest, and therefore where it must be designed properly:* the **Downloads** page. A rule is the most natural thing in the product to want to edit, and the page will be judged on whether "managed in Git" reads as obvious or as broken. So a download and a rule are first-class objects with their YAML on the page, an **Open in Git** link, a **Drift** banner when the registry disagrees with the target configuration, and an **Apply** that states its consequences first. What the page *does* is run a download - naming the software, the way a person always did. What it does not do is change whether a rule fires: there is no toggle, because there is nothing behind one ([20](20-download-rules.md) §9). A rule that is off reads **"Disabled in configuration"** with a link to the line, and the honest control next to a rule that is misbehaving is **Pause the queue**, which acts on work rather than on Git.
>
> *If that proves too slow in practice*, the escape hatch is a UI that opens a pull request rather than one that writes configuration - Git stays the source of truth and the change stays reviewable. That is a post-M10 question, deliberately not designed now.

Also out of scope, permanently: it is not a registry browser, not a replacement for Grafana, and it serves no artifact bytes. The data path stays where [00](00-overview.md) §5 puts it.

## 5. What must exist before it ships

Gates, not aspirations. Each is a prerequisite with a named owner elsewhere in this document set.

| # | Gate | Where it is specified |
|---|---|---|
| G1 | **API authentication and identity.** A browser application cannot front an unauthenticated API on a NetworkPolicy - the policy *is* the security model today, and a UI exists to be reachable | [09](09-api.md) §10, [17](17-delivery-plan.md) Q6 |
| G2 | **Authorization with roles.** Read-only for auditors, operate for engineers, apply for operators. The audit trail already records the actor; it needs one that means something | [09](09-api.md) §10.2 |
| G3 | **An OpenAPI document generated from the router**, not hand-written, so the client cannot drift from the server | new at M10 |
| G4 | **A live-progress channel.** Server-sent events over the existing progress endpoints; polling a 2 000-job transfer from a browser is not acceptable | [09](09-api.md) §6 |
| G5 | **CORS and CSRF posture**, decided with G1 rather than after it | with G1 |
| G6 | **Stable error semantics** - RFC 9457 problem details with machine-readable `type` values the UI can branch on without matching prose | [09](09-api.md) §8 |

G1 is absolute: shipping a UI in front of an unauthenticated API would expose transfer control and the full audit trail to anything that can route to it, which is precisely the risk [17](17-delivery-plan.md) Q6 exists to prevent.

## 6. Non-functional requirements, and what they actually oblige

"Clean, minimal, fast, enterprise-grade" is a sentence anyone can write. Here is what it commits us to:

- **Air-gapped by construction.** No CDN, no external fonts, no analytics, no third-party embeds. The bundle ships everything it needs - the same constraint the rest of the system already meets.
- **Deployable as a static bundle**, served either by the Coordinator or by a small separate Deployment, with no server-side rendering tier and therefore no fourth binary.
- **Fast on real data**: virtualised tables, because a transfer has thousands of job rows; server-side pagination and filtering, which the API already provides; no client-side aggregation of anything the API can aggregate.
- **Accessible**: WCAG AA, full keyboard operation, no meaning carried by colour alone.
- **Honest**: the rule from [18](18-quay-replication.md) §6.1 is a UI rule above all - no progress bar, percentage or ETA for work whose bytes we are not counting. In the download flow this is visible as a real asymmetry the design must preserve: the JFrog step reports measured bytes, speed and ETA, and the **Configure Mirror to Quay** step reports configured-at, sync-completed-at and whether the content matches. Two steps of one operation, two different kinds of truth, shown differently on purpose.
- **AT&T enterprise visual language**, light theme first: AT&T blue for primary actions, dark navy navigation, white and light-grey content, green/red/amber carrying only success, failure and warning. A mature internal operations product rather than a generic dashboard.
- **Boring technology**, chosen at M10 against the same criteria as [ADR-001](16-technology-choices.md#adr-001): one framework, one build, no exotic runtime, and a dependency footprint an air-gapped estate can vendor.

## 7. Delivery

M10, after M9. It depends on G1, which is a deployment gate rather than a milestone, so the ordering is: authentication ships, then the UI is buildable. The [UI generation brief](../ui/ui-generation-brief.md) exists now so the information architecture can be reviewed - on paper, at zero cost - long before any of that.
