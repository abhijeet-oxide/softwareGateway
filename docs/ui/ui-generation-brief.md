# softwareGateway — UI generation brief

> **What this file is.** A prompt to hand to a UI-generating agent (v0, Lovable, Figma AI, or a coding agent) to produce screen mockups for softwareGateway. It is written to be pasted whole. The output is images/mockups for review — not production code.
>
> **Design direction and the API gates that must exist first:** [19 — User Interface](../design/19-user-interface.md). Replication modes referenced here: [18 — Quay Replication Strategies](../design/18-quay-replication.md).

---

## 0. How to use this prompt

Paste everything from §1 onward into your UI agent. Ask for one page at a time, in the order listed, and ask for the **empty**, **loading**, **busy** and **error** state of each page rather than only the happy path — the happy path is the one state that never teaches you anything about the design.

When a page comes back, check it against §10 before iterating.

---

## 1. What the product is

softwareGateway moves large software packages from vendor registries into internal registries, continuously and with a record of what moved.

A vendor publishes a **package** — one tag that resolves to an OCI index containing container images, Helm charts, config bundles, signatures and SBOMs, routinely **30–60 GB across hundreds of blobs**. The tool:

1. **Discovers** — polls vendor registries and records every new package.
2. **Replicates** — copies a package into one or more internal registries, streaming blob by blob, skipping anything already there, resuming after failures.
3. **Promotes** — moves a validated package between internal registries (lab → production).
4. **Verifies** — proves the bytes are what the vendor signed (cosign).
5. **Delegates, where it makes sense** — a Red Hat Quay destination can instead be configured to *mirror* from an upstream on a schedule, or to act as a *proxy cache* that fills on demand. In those modes the tool configures and observes rather than moves.

It is a control plane (Coordinator), a fleet of stateless workers, and a CLI. **The UI is a second client of the same API the CLI uses.** Configuration lives in Git (GitOps) and is read-only in the product — the UI shows configuration and any drift from it, and never edits it.

**Scale to design for:** 5–50 products, hundreds to thousands of packages, tens of concurrent transfers, thousands of jobs inside one transfer, a fleet of 2–20 workers, a year of audit history.

## 2. Who uses it

| Persona | Comes here to | Frequency |
|---|---|---|
| **Release engineer** | Get a specific release into lab before a demo; promote to production in a window | Daily, task-driven |
| **Platform operator** | See what is stuck and why; unstick it; keep the fleet healthy | Daily, interrupt-driven |
| **Auditor / manager** | Answer "what did we ship in March, and was it signed?" | Rarely, question-driven |

None of them will read documentation, and none of them will be trained. **A first-time user must complete "get release v2.14.0 into lab" without asking anyone.**

## 3. Design principles — the rules that make training unnecessary

1. **Every page answers one question in its first screenful.** The question is stated in the page title area, and the answer is the largest thing on the page. Detail is below the fold, never above it.
2. **Nouns are pages, verbs are buttons.** Users never navigate to perform an action — they find the thing, then act on it. Actions are always attached to the object they affect.
3. **Nothing is more than two clicks from Home.** Home → list → detail. There is no third level.
4. **Every object is the same chip everywhere.** A package, a target, a transfer, a digest look identical on every page and are always clickable to their detail page.
5. **Never show a number you cannot defend.** No estimated percentages, no invented ETAs, no progress bar on a timer. Where a number is genuinely unknown, show `—` with a one-line reason on hover. This is a hard rule; see §7.
6. **Destructive and delegated actions confirm with consequences, not with "Are you sure?"** The dialog states what will change, in the same words the page uses.
7. **Empty states teach.** Every empty state contains one sentence explaining what will appear there and one button that makes it appear.
8. **Colour carries meaning and nothing else.** Grey = idle, blue = running, green = done, amber = attention, red = failed. Never decorative.
9. **The UI never lies about who did what.** Actions taken in the UI look identical in the activity trail to the same action taken from the CLI.

## 4. Global shell

Present on every page:

- **Left sidebar, 6 items only** (icon + label, collapsible to icons): **Home · Catalog · Transfers · Products · Compare · Activity**. Below a divider: **System**. Nothing else, ever. Detail pages are drill-downs, not nav items.
- **Top bar**: global search (`/` focuses it) that searches packages, transfers, products and digests in one box and shows typed results grouped by kind; a **⌘K command palette** exposing every action in the product ("Replicate…", "Promote…", "Scan now", "Pause all"); an environment/instance label; a status dot that turns amber when anything needs attention and links to System; an identity slot in the far right (v1 shows "Unauthenticated — network restricted", greyed; the slot exists so adding sign-in later moves nothing).
- **Primary action button, top right of every list page** — the one thing you most likely came to do. On Catalog and Home that is **Replicate**.
- **Toasts** for accepted actions, with an **Undo** where the action is reversible (cancel a queued transfer) and a **View** link where it is not.
- **A single "needs attention" band** directly under the top bar, shown only when something is failing, listing at most three items with a link to the rest. It is the same band on every page, so users learn one place to look.

Layout: 1440 × 900 baseline, content max-width 1400 px, 12-column grid, 8 px spacing scale. Responsive down to 1024 px (tablet); below that, read-only.

## 5. The pages

Ten pages. Six are top-level; four are detail pages reached by clicking an object. **Every page supports more than one job**, as noted under "also does".

---

### Page 1 — Home ("What is happening right now?")

**For:** everyone, every morning. The page a browser tab is left open on.

**Layout, top to bottom:**
- **Four stat tiles**: Active transfers · Queue depth (waiting jobs) · Throughput now (bytes/sec across the fleet) · Failures needing attention. Each tile is clickable and filters the relevant page. Each carries a sparkline of the last hour — no axes, no legend.
- **Active transfers strip**: up to five live rows, each showing package chip, target chip, a real progress bar, bytes done / bytes planned, current speed, ETA, and inline pause/cancel. Rows animate their progress; they do not re-sort while you look at them.
- **Two columns below:**
  - *Newly discovered* — packages found in the last 24 h, each with a one-click **Replicate** and a "not replicated anywhere" marker where true.
  - *Needs attention* — failed transfers grouped by cause ("Vendor registry unreachable — 3 transfers"), each with the fix action inline (**Retry all**).
- **Fleet ribbon** at the bottom: workers online, their aggregate load, and coordinator health as one green/amber line linking to System.

**Primary action:** Replicate (opens the action drawer, §6).
**Also does:** triggers a scan (**Scan now** in the discovery card), retries failures in bulk, pauses everything (a deliberate, confirm-with-consequences **Pause all** hidden behind the ⋯ menu).
**Empty state:** "Nothing is running. 128 packages are known across 6 products." + **Browse catalog** and **Scan now**.

---

### Page 2 — Catalog ("What exists, and where is it?")

**For:** release engineers finding the thing they came for. The busiest page in the product.

The core of this page is a table where **the columns are the answer**: for each package, one column per target showing presence.

| Package | Product | Discovered | Size | Signed | jfrog-store | ocp-prod | ocp-dev-cache |
|---|---|---|---|---|---|---|---|
| `v2.14.0` | vendor-a-platform | 2 h ago | 44.8 GB | ✓ | ● present | ◐ syncing | ○ not cached |

- Presence cells are the whole point: **● present · ◐ in progress · ○ absent · ⚠ diverged** (destination holds a different digest) · **— n/a** (target cannot answer, e.g. a proxy cache that has not been asked). Hovering a cell gives one line of detail; clicking it acts on that one cell (replicate into that target).
- **Filters as chips above the table**, not in a sidebar drawer: Product, Target, Signed, State, Discovered-within. Selected filters appear as removable chips, and the URL carries them so a filtered view can be pasted into chat.
- **Row selection enables bulk actions** in a bar that slides up from the bottom: Replicate to…, Promote to…, Verify, Compare, Inspect.
- **Search** at the top of the table accepts a tag, a partial tag or a digest.

**Primary action:** Replicate.
**Also does:** promote, verify, inspect (measure a package's real size), compare, and one-click replicate-into-one-target from a presence cell.
**Empty state:** "No packages discovered yet for these filters." + **Clear filters** / **Scan now**.

---

### Page 3 — Package detail ("Everything about this one release")

Reached by clicking a package anywhere.

- **Header**: tag, product, source repository, full digest with a copy button, size, discovered-at, signature status as a plain sentence ("Signed by the vendor — verified 2 h ago at ocp-prod").
- **Presence panel** — the row from Catalog, expanded: one card per target showing state, when it got there, which transfer put it there, and the digest the target actually holds (highlighted when it differs).
- **Contents** — the artifact tree (index → image manifests → Helm chart → config bundles), collapsible, with per-node media type, platform and size. Shows blob counts and how much of it is already deduplicated at each target. If the package has never been measured, this area shows a single **Inspect** button and explains that inspection contacts the vendor registry and may take minutes.
- **Timeline** — discovered → requested → transferred → verified → promoted, with timestamps and the actor for each, as a vertical thread. This *is* the audit trail for this package; there is no separate place to look.

**Primary action:** Replicate (pre-filled with this package).
**Also does:** promote, verify now, inspect, warm a proxy cache, copy the pull reference for any target, jump to the transfer that moved it.

---

### Page 4 — Transfers ("What is moving, and what got stuck?")

- **Two tabs**: **Active** (running, queued, paused) and **History** (completed, failed, cancelled). A third tab, **Scheduled**, lists requests that have not fired yet with their due time and a cancel action.
- Each row: package chip · direction (source chip → target chip) · state pill · progress bar with real bytes · speed · ETA · priority · started-at · inline ⋯ (pause, resume, cancel, retry, change priority).
- **Group-by control**: none / product / target / failure cause. Grouping by failure cause is what turns forty red rows into three real problems.
- **Bulk select** for pause, resume, retry, cancel and priority.
- A quiet line at the top states the honest aggregate: "12 running · 340 GB planned · 88 GB moved · 41 GB deduplicated".

**Primary action:** Replicate.
**Also does:** the entire queue control surface, plus schedules, without a separate page.

---

### Page 5 — Transfer detail ("Why is this one behaving like this?")

- **Header**: package → target, state, elapsed, bytes moved / planned / deduplicated / mounted, speed, ETA. For a **delegated (mirror) transfer** this header changes shape: state only, with the line "Delegated to Quay mirror — byte-level progress is not available", and every byte field renders `—`. This is required behaviour, not an edge case.
- **Progress by wave**: blobs → manifests → tag, as three segmented bars, making it obvious that a tag is only applied at the end.
- **Jobs table** (virtualised, thousands of rows): digest, size, state, worker, attempts, outcome (`transferred`, `skipped — already present`, `mounted`, `failed`). Filterable to failures in one click.
- **Failures panel**, grouped by cause, each group stating the cause in plain language, the count, one example digest, and a **Retry these** button.
- **Logs** — worker log lines for this transfer, streaming while it runs, with a level filter and a copy button.

**Primary action:** Retry failed jobs.
**Also does:** pause/resume/cancel/priority, log reading, and jumping to the worker or the package.

---

### Page 6 — Products ("How is this configured, and is it healthy?")

- One expandable card per product: name, owner, enabled state, and a health dot.
- Expanded, the card shows **Sources** (registry, repositories, discovery interval, last scan, packages found) and **Targets** (registry, repository, environment, and a **replication mode badge**: `COPY` / `MIRROR` / `PROXY`), each row with a connectivity result — a green check, or a red line naming what failed (credential, TLS, DNS, proxy).
- A **Configuration** tab shows the product's YAML, read-only, with a banner: "Configuration comes from Git. Edit it there — this view is what the Coordinator has loaded." Include the config hash and when it was loaded.
- A **Drift** marker appears on any target whose replication configuration in the registry differs from the configuration in Git, linking to Page 7.

**Primary action:** Scan now (per product or all).
**Also does:** connectivity check, calibration ("measure this source→target path and recommend settings" — a modal with a live progress readout and a plain-language recommendation), viewing discovery status, and opening any target's replication page.

---

### Page 7 — Target & replication ("How does content get into this destination?")

Reached by clicking a target. **This is the page that makes the three replication modes comprehensible**, and it is mostly a page about honesty.

- **Header**: target name, registry, repository, environment, mode badge.
- **A one-sentence explanation of the mode, in plain language, always visible:**
  - *Copy* — "We move the bytes. Content arrives when you ask for it."
  - *Mirror* — "Quay pulls from `artifactory.example.com/docker-local/vendor-a/platform` every 6 hours. We configure it and watch it."
  - *Proxy cache* — "Nothing is stored until someone pulls it. This target fronts `artifactory.example.com/docker-local`."
- **Mode-specific body:**
  - **Copy** — recent transfers into this target, dedupe rate, and the pull reference.
  - **Mirror** — the rule (tag patterns) shown as chips with a live count of what currently matches; the schedule and next due time; the robot account; a **sync history table** (started, finished, result, digest observed, message from Quay verbatim on failure); and a **Sync now** button. A prominent warning card appears when the tag rules cannot match signature tags (`sha256-*.sig`), stating exactly what will happen: "Signatures will not be mirrored, and verification at this target will fail."
  - **Proxy cache** — the upstream, the staleness window, what is currently cached, quota use, and a **Warm** action explaining plainly that it pulls the package through the cache and discards the bytes.
- **Desired vs applied** — a two-column diff of the configuration in Git against the configuration in the registry, with unchanged fields collapsed. When they differ, a banner says so and offers **Apply**. **Apply always shows the diff and its consequences first** — in particular that putting a repository into mirror mode makes it read-only to everyone except the mirror robot.

**Primary action:** Sync now (mirror) / Warm (proxy) / Replicate (copy).
**Also does:** apply configuration, resolve drift, pause a mirror without deleting it, read sync history, and see the effective pull reference.

---

### Page 8 — Compare ("What is different between these two places?")

- **Two selectors at the top** — left and right, each any source or target — and a single **Compare** button. Recent comparisons are one click away.
- Result is a three-column diff: **Only on the left · On both · Only on the right**, per package, with the digest shown when both sides have the same tag but different content (the case that matters most and is easiest to miss).
- Every row on the left that is missing on the right has an inline **Replicate →** button; the header has **Replicate all missing**, with a count and total size stated before you press it.

**Primary action:** Replicate the difference.
**Also does:** promotion planning (compare lab against production, then promote the gap), drift investigation, and pre-window verification that production has what it should.

---

### Page 9 — Activity ("What happened, and who did it?")

- A reverse-chronological feed of every event: discovered, requested, planned, started, completed, failed, verified, promoted, configuration applied, mirror synced, configuration reloaded.
- **Filter bar**: kind, product, package, target, actor, outcome, time range. Filters compose and live in the URL.
- Each entry is one readable line — "Aisha promoted `v2.14.0` from lab to production" — expandable to the full record (digests, request id, trace id, the exact detail payload).
- **Export** to CSV/JSON for whatever the audit actually needs.
- A **Notifications** tab shows what was sent, to whom, through which channel, and whether it was delivered — so "why did nobody get told" has an answer.

**Also does:** doubles as the per-object history when opened pre-filtered from a package, transfer or target.

---

### Page 10 — System ("Is the machine healthy?")

- **Coordinator** — version, uptime, leader status, database connectivity, config load status with any product that failed to load and why.
- **Workers** — a table of the fleet: id, version, state, jobs in flight vs its granted ceiling, throughput, last heartbeat, and a link to that worker's logs. A worker that has stopped heartbeating is visibly different, not just a stale timestamp.
- **Throughput and limits** — current fleet throughput, the per-registry concurrency budget in force, and whether adaptive backpressure has reduced it (and against which registry).
- **Dependencies** — one row per external dependency (database, each registry, SMTP, Sigstore), each with a real check result and its latency.

**Also does:** worker log reading, calibration entry point, and the "is it us or is it them" question that every incident starts with.

---

## 6. Components used across pages

Design these once; reuse them literally everywhere.

- **Package chip** — `v2.14.0` with product name beneath, monospace tag, click → Page 3.
- **Target chip** — name + mode badge (`COPY`/`MIRROR`/`PROXY`), click → Page 7.
- **Digest chip** — `sha256:9f86d081…` truncated to 12 chars, monospace, click to copy, tooltip shows full value.
- **State pill** — one vocabulary for the entire product: `queued`, `running`, `paused`, `succeeded`, `diverged`, `failed`, `cancelled`, `skipped`.
- **Presence cell** — the ●/◐/○/⚠/— glyph set from Page 2, used in any table that answers "is it there".
- **Progress bar** — always real bytes. A delegated transfer shows an indeterminate *state* strip that is visually distinct from a measured bar, so the two are never confused.
- **Action drawer** — the one place actions are configured. Slides in from the right; used for Replicate, Promote, Verify, Warm and Apply. Contents: what (pre-filled), where (multi-select targets showing which already have it), when (now / schedule), priority, verify-before-transfer, and a **dry-run preview** that states what will move and how much is already there. The preview is part of the drawer, not a separate page or mode.
- **Consequence dialog** — for apply, pause-all, cancel and mode changes. States what will change in the page's own words, then a single confirming verb ("Put repository into mirror mode"), never "OK".

## 7. Language and honest numbers

- Sentence case everywhere. No exclamation marks. No "Oops".
- Errors say what happened, what it means and what to do: *"Vendor registry refused the credential (401). The transfer is paused. Check the `vendor-a-registry` secret, then retry."*
- Bytes in binary units to one decimal (`44.8 GB`), durations as `4m 12s`, timestamps as relative with an absolute tooltip.
- **The honest-numbers rule, restated because it is the easiest thing to get wrong:** show ETA and speed only where the system is measuring bytes. A mirror sync gives a *state*, so a mirror transfer shows a state — never a percentage derived from elapsed time, never a progress bar that moves because a timer moved. Where a value is unknowable, render `—` and explain on hover.
- Never say "syncing" for a copy or "transferring" for a mirror. The three modes keep their own verbs everywhere in the product: **replicate / mirror / cache**.

## 8. Visual system

- **Enterprise-clean, dense but calm.** Closer to Linear or Vercel's dashboard than to a consumer app. Flat surfaces, one elevation level, hairline borders, generous vertical rhythm inside dense tables.
- **Type**: one sans (Inter or the system stack) for UI; one mono (JetBrains Mono / ui-monospace) for tags, digests, repository paths and log output. Everything that is an identifier is monospace, always.
- **Colour**: a neutral grey scale doing 90% of the work, one blue accent for interactive elements, and the four semantic colours from §3.8. Both **light and dark themes**, equal quality, dark is the default.
- **Density**: table rows 40 px, comfortable option at 48 px. No card grids where a table would do — this is a data product.
- **Motion**: 120–200 ms, only on state change. Progress bars animate; nothing else slides for decoration.
- **Accessibility**: WCAG AA contrast in both themes, full keyboard operation including the command palette, visible focus rings, no meaning carried by colour alone (the presence glyphs are shapes as well as colours).
- **Air-gapped by construction**: no external fonts, no CDN assets, no telemetry, no third-party embeds. Anything the page needs, it ships with.

## 9. What to produce

For each of the ten pages, at 1440 × 900, dark theme first and light theme for pages 1, 2 and 7:

1. **The populated state** — realistic data: 6 products, ~40 packages, 12 transfers of which 2 are failing, one target in each replication mode.
2. **The empty state** — first run, nothing discovered.
3. **The busy/error state** — Home with three failures in the attention band; Transfer detail with a failure group expanded; Target & replication with drift detected and the signature-rule warning showing.
4. **The action drawer** open over Catalog, with dry-run preview populated.
5. **The command palette** open.

Annotate each screen with the question it answers and the primary action, so the review can check §3.1 directly.

## 10. Non-negotiables — check every screen against this list

1. No page has more than one primary action.
2. No navigation item beyond the seven in §4.
3. No progress percentage, ETA or speed on a delegated (mirror/proxy) object. Ever.
4. No configuration editing anywhere. Configuration is read-only with a "comes from Git" banner.
5. No modal that asks "Are you sure?" without stating the consequence in the product's own words.
6. No empty state without an explanation and an action.
7. No identifier (tag, digest, repository, worker id) in a proportional font.
8. No colour used decoratively.
9. No page that requires horizontal scrolling at 1440 px; wide tables scroll inside their own container with the first column pinned.
10. Nothing on any screen that a first-time user would have to be told the meaning of.
