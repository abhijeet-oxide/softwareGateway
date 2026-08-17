# 19 — User Interface

> **Prerequisites:** [09 — API](09-api.md), [13 — CLI](13-cli.md)
> **Status: DIRECTION. Nothing here is implemented, and v1 does not include it.** Scheduled at [M9](17-delivery-plan.md#m9--web-ui), behind the authentication gate in [17](17-delivery-plan.md) Q6. Recorded now because decisions taken today either make it cheap or make it expensive, and the difference is invisible until someone tries.

---

## 1. The statement

**The first release is CLI-centric. The design is API-first. A web UI follows.**

That is not a hedge. [09](09-api.md) already says the Coordinator's REST API is the *only* interface — `transferctl` is a pure API client that never contacts a registry and never opens a database connection ([00](00-overview.md) §5.3). The CLI is therefore not the product's interface; it is the product's **first client**, and its existence is the evidence that a second client is buildable without moving anything behind the API.

What the UI will be, stated so it can be held to: **clean, minimal, fast, fully optimised, enterprise-grade, and capable of everything the tool can do.** Deployable in-cluster, air-gapped, with no external dependency at runtime.

## 2. Why say it now rather than at M9

Because three classes of decision are cheap today and expensive later, and all three are decisions we are otherwise making by accident:

| Decision | Made well today | Discovered at M9 |
|---|---|---|
| **Identity** | The `actor` field, its plumbing and its recording already exist and write `"anonymous"` ([12](12-observability-and-audit.md) §4.2) | An audit trail with a year of unattributable history |
| **Every capability behind the API** | Already true, and enforced by the CLI being a pure client | A UI that needs an endpoint the CLI never needed, added under time pressure |
| **Machine-readable errors and pagination** | RFC 9457 problem details, `pageSize`/`pageToken`, `filter`, `validateOnly` ([09](09-api.md) §1) | A UI parsing human-readable strings |

The first row is the one that cannot be retrofitted. The other two are already right, and this document exists partly to say **keep them right** — an endpoint added for the CLI's convenience that returns rendered text rather than data is a small, invisible step away from a UI that has to scrape it.

## 3. What it must do

Everything `transferctl` does ([13](13-cli.md) §2), because a UI that can do most of it forces users to keep a terminal open and then they never come back to the UI. Concretely: browse products and their health; see and trigger discovery; browse packages and inspect them; request replications and promotions with a dry-run preview; watch transfers live; read failures grouped by cause; control the queue (pause, resume, cancel, retry, priority); manage schedules; verify signatures; read worker logs; compare two registries; and query the audit trail.

Plus the one thing the CLI does awkwardly and a UI does naturally: **comprehension at a glance** — which packages are where, which are diverged, and what is failing right now, without composing a query first.

Screen-by-screen intent, page count, information architecture and the visual system live in the [UI generation brief](../ui/ui-generation-brief.md), which is a working document for producing mockups rather than a specification. Ten pages, six of them top-level.

## 4. What it must never do

> **Decision — the UI is a read-and-operate client. It never edits configuration.**
>
> *Alternative considered:* let the UI edit products — the field every operator asks for within a week of seeing a read-only YAML pane.
>
> *Rejected because* configuration is GitOps ([02](02-configuration.md) §2): Git is the source of truth and Flux reconciles it. A UI that wrote configuration would create a second source of truth that Flux would silently revert, producing the worst failure mode in the product — a change that appears to work, works for a few minutes, and then undoes itself with nothing in any log to say why. The API already refuses this for the same reason ([09](09-api.md) §2, "Products are read-only over the API").
>
> *What the UI does instead:* shows the loaded configuration with its config hash and load time, shows **drift** between Git and any registry-side state it manages ([18](18-quay-replication.md) §8), and links to the repository. Requesting work — transfers, promotions, syncs, applies — is not configuration and is fully available.

Also out of scope, permanently: it is not a registry browser, not a replacement for Grafana, and it serves no artifact bytes. The data path stays where [00](00-overview.md) §5 puts it.

## 5. What must exist before it ships

Gates, not aspirations. Each is a prerequisite with a named owner elsewhere in this document set.

| # | Gate | Where it is specified |
|---|---|---|
| G1 | **API authentication and identity.** A browser application cannot front an unauthenticated API on a NetworkPolicy — the policy *is* the security model today, and a UI exists to be reachable | [09](09-api.md) §10, [17](17-delivery-plan.md) Q6 |
| G2 | **Authorization with roles.** Read-only for auditors, operate for engineers, apply for operators. The audit trail already records the actor; it needs one that means something | [09](09-api.md) §10.2 |
| G3 | **An OpenAPI document generated from the router**, not hand-written, so the client cannot drift from the server | new at M9 |
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
- **Honest**: the rule from [18](18-quay-replication.md) §6.1 is a UI rule above all — no progress bar, percentage or ETA for work whose bytes we are not counting. A delegated mirror shows a state.
- **Boring technology**, chosen at M9 against the same criteria as [ADR-001](16-technology-choices.md#adr-001): one framework, one build, no exotic runtime, and a dependency footprint an air-gapped estate can vendor.

## 7. Delivery

M9, after M7. It depends on G1, which is a deployment gate rather than a milestone, so the ordering is: authentication ships, then the UI is buildable. The [UI generation brief](../ui/ui-generation-brief.md) exists now so the information architecture can be reviewed — on paper, at zero cost — long before any of that.
