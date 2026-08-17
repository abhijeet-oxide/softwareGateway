# 19 — User Interface

> **Prerequisites:** [09 — API](09-api.md), [13 — CLI](13-cli.md)
> **Status: DIRECTION. Nothing here is implemented, and v1 does not include it.** Scheduled at [M10](17-delivery-plan.md#m10--web-ui), behind the authentication gate in [17](17-delivery-plan.md) Q6. Recorded now because decisions taken today either make it cheap or make it expensive, and the difference is invisible until someone tries.

---

## 1. The statement

**The first release is CLI-centric. The design is API-first. A web UI follows.**

That is not a hedge. [09](09-api.md) already says the Coordinator's REST API is the *only* interface — `transferctl` is a pure API client that never contacts a registry and never opens a database connection ([00](00-overview.md) §5.3). The CLI is therefore not the product's interface; it is the product's **first client**, and its existence is the evidence that a second client is buildable without moving anything behind the API.

What the UI will be, stated so it can be held to: **clean, minimal, fast, fully optimised, enterprise-grade, and capable of everything the tool can do.** Deployable in-cluster, air-gapped, with no external dependency at runtime.

It is called **Software Lifecycle Manager** on screen. `softwareGateway` is the engineering name of the system and appears nowhere in the interface — §3.1 explains why that is a design decision rather than branding.

## 2. Why say it now rather than at M10

Because three classes of decision are cheap today and expensive later, and all three are decisions we are otherwise making by accident:

| Decision | Made well today | Discovered at M10 |
|---|---|---|
| **Identity** | The `actor` field, its plumbing and its recording already exist and write `"anonymous"` ([12](12-observability-and-audit.md) §4.2) | An audit trail with a year of unattributable history |
| **Every capability behind the API** | Already true, and enforced by the CLI being a pure client | A UI that needs an endpoint the CLI never needed, added under time pressure |
| **Machine-readable errors and pagination** | RFC 9457 problem details, `pageSize`/`pageToken`, `filter`, `validateOnly` ([09](09-api.md) §1) | A UI parsing human-readable strings |

The first row is the one that cannot be retrofitted. The other two are already right, and this document exists partly to say **keep them right** — an endpoint added for the CLI's convenience that returns rendered text rather than data is a small, invisible step away from a UI that has to scrape it.

## 3. What it must do

Everything `transferctl` does ([13](13-cli.md) §2), because a UI that can do most of it forces users to keep a terminal open and then they never come back to the UI. Concretely: browse products and their health; see and trigger discovery; browse packages and inspect them; request replications and promotions with a dry-run preview; watch transfers live; read failures grouped by cause; control the queue (pause, resume, cancel, retry, priority); manage schedules; verify signatures; read worker logs; compare two registries; and query the audit trail.

Plus the one thing the CLI does awkwardly and a UI does naturally: **comprehension at a glance** — which releases are where, which have diverged, and what is failing right now, without composing a query first.

Screen-by-screen intent, layout and the visual system live in the [UI generation brief](../ui/ui-generation-brief.md), which is a working document for producing mockups rather than a specification. **Ten pages, eight of them navigable:**

```
Home · Products · Software · Download Rules · Repositories · Activity · Reports · Settings
```

with Software detail and Download reached from them. The lifecycle those pages exist to make obvious is **Discover → Review → Verify → Download & Replicate → Compare → Promote**.

### 3.1 The UI speaks the user's language, not the engine's

> **Decision — the interface uses product vocabulary, and the domain model stays behind the API.**
>
> A Product Owner looking at SBC 25.8.1 should think "this release is new and signed, I'll download it". They should not meet *package*, *transfer*, *job*, *blob*, *placement*, *wave* or *replication mode* — every one of which is load-bearing in [01](01-domain-model.md) and none of which is their problem.
>
> The mapping is fixed, one term to one term, in the brief's vocabulary table: **Software** is a Package, **Download** is a TransferRequest and its Transfers, **Location** is a target repository, **Saved (already present)** is deduplication, **Download Rule** is a rule in the product's `download` block together with the chain its targets declare ([20](20-download-rules.md)), and **Configure Mirror to Quay** is a step of that chain, from `replication.mode: mirror` ([18](18-quay-replication.md)).
>
> *Why write it down rather than let it emerge:* two clients of one API will otherwise invent two vocabularies, and the day someone reads a CLI transcript next to a UI screenshot, neither of them can be trusted. The CLI keeps the domain words — its users are operators and the words are precise. The UI keeps the product words. The mapping is the contract between them.

**The most consequential consequence of this: there is no Replicate action in the UI.** Downloading a release into JFrog and configuring the Quay mirror that OpenShift pulls from are one operation with several steps, presented as one operation with several steps. Internally they remain a transfer and a replication-mode apply ([18](18-quay-replication.md) §7); the user is not asked to know that, and is never asked to perform the second half themselves.

## 4. What it must never do

> **Decision — the UI is a read-and-operate client. It never edits configuration.**
>
> *Alternative considered:* let the UI edit products — the field every operator asks for within a week of seeing a read-only YAML pane.
>
> *Rejected because* configuration is GitOps ([02](02-configuration.md) §2): Git is the source of truth and Flux reconciles it. A UI that wrote configuration would create a second source of truth that Flux would silently revert, producing the worst failure mode in the product — a change that appears to work, works for a few minutes, and then undoes itself with nothing in any log to say why. The API already refuses this for the same reason ([09](09-api.md) §2, "Products are read-only over the API").
>
> *What the UI does instead:* shows the loaded configuration with its config hash and load time, shows **drift** between Git and any registry-side state it manages ([18](18-quay-replication.md) §8), and links to the repository. Requesting work — downloads, promotions, syncs, applies — is not configuration and is fully available.
>
> *Where this is hardest, and therefore where it must be designed properly:* the **Download Rules** page. A rule is the most natural thing in the product to want to edit, and the page will be judged on whether "managed in Git" reads as obvious or as broken. So a rule is a first-class object with its YAML on the page, an **Open in Git** link, a **Drift** banner when the registry disagrees with it, and an **Apply** that states its consequences first. Enabling, disabling and running a rule now are actions; changing what a rule *says* is a commit. [20](20-download-rules.md) §9 is where that line is drawn precisely: the toggle writes an audited **suspension**, and the page shows both facts — *"Suspended by alice@example.com — configuration says enabled"*.
>
> *If that proves too slow in practice*, the escape hatch is a UI that opens a pull request rather than one that writes configuration — Git stays the source of truth and the change stays reviewable. That is a post-M10 question, deliberately not designed now.

Also out of scope, permanently: it is not a registry browser, not a replacement for Grafana, and it serves no artifact bytes. The data path stays where [00](00-overview.md) §5 puts it.

## 5. What must exist before it ships

Gates, not aspirations. Each is a prerequisite with a named owner elsewhere in this document set.

| # | Gate | Where it is specified |
|---|---|---|
| G1 | **API authentication and identity.** A browser application cannot front an unauthenticated API on a NetworkPolicy — the policy *is* the security model today, and a UI exists to be reachable | [09](09-api.md) §10, [17](17-delivery-plan.md) Q6 |
| G2 | **Authorization with roles.** Read-only for auditors, operate for engineers, apply for operators. The audit trail already records the actor; it needs one that means something | [09](09-api.md) §10.2 |
| G3 | **An OpenAPI document generated from the router**, not hand-written, so the client cannot drift from the server | new at M10 |
| G4 | **A live-progress channel.** Server-sent events over the existing progress endpoints; polling a 2 000-job transfer from a browser is not acceptable | [09](09-api.md) §6 |
| G5 | **CORS and CSRF posture**, decided with G1 rather than after it | with G1 |
| G6 | **Stable error semantics** — RFC 9457 problem details with machine-readable `type` values the UI can branch on without matching prose | [09](09-api.md) §8 |

G1 is absolute: shipping a UI in front of an unauthenticated API would expose transfer control and the full audit trail to anything that can route to it, which is precisely the risk [17](17-delivery-plan.md) Q6 exists to prevent.

## 6. Non-functional requirements, and what they actually oblige

"Clean, minimal, fast, enterprise-grade" is a sentence anyone can write. Here is what it commits us to:

- **Air-gapped by construction.** No CDN, no external fonts, no analytics, no third-party embeds. The bundle ships everything it needs — the same constraint the rest of the system already meets.
- **Deployable as a static bundle**, served either by the Coordinator or by a small separate Deployment, with no server-side rendering tier and therefore no fourth binary.
- **Fast on real data**: virtualised tables, because a transfer has thousands of job rows; server-side pagination and filtering, which the API already provides; no client-side aggregation of anything the API can aggregate.
- **Accessible**: WCAG AA, full keyboard operation, no meaning carried by colour alone.
- **Honest**: the rule from [18](18-quay-replication.md) §6.1 is a UI rule above all — no progress bar, percentage or ETA for work whose bytes we are not counting. In the download flow this is visible as a real asymmetry the design must preserve: the JFrog step reports measured bytes, speed and ETA, and the **Configure Mirror to Quay** step reports configured-at, sync-completed-at and whether the content matches. Two steps of one operation, two different kinds of truth, shown differently on purpose.
- **AT&T enterprise visual language**, light theme first: AT&T blue for primary actions, dark navy navigation, white and light-grey content, green/red/amber carrying only success, failure and warning. A mature internal operations product rather than a generic dashboard.
- **Boring technology**, chosen at M10 against the same criteria as [ADR-001](16-technology-choices.md#adr-001): one framework, one build, no exotic runtime, and a dependency footprint an air-gapped estate can vendor.

## 7. Delivery

M10, after M9. It depends on G1, which is a deployment gate rather than a milestone, so the ordering is: authentication ships, then the UI is buildable. The [UI generation brief](../ui/ui-generation-brief.md) exists now so the information architecture can be reviewed — on paper, at zero cost — long before any of that.
