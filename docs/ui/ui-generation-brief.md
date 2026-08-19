# Software Gateway - UI generation brief

> **What this file is.** A prompt to hand to a UI-generating agent (v0, Lovable, Figma AI, a coding agent) to produce screen mockups. Paste it whole. The output is images for review, not production code.
>
> **Product name in the UI:** *Software Gateway*.
>
> **Authoritative on structure, language and layout:** this file, derived from the reviewed AT&T reference design. **Authoritative on what the system can actually do:** [19 - User Interface](../design/19-user-interface.md) (gates and constraints), [18 - Quay Replication Strategies](../design/18-quay-replication.md) (what a Quay destination does), [09 - API](../design/09-api.md) (where every number comes from).
>
> Ask for one page at a time, in order, and for each page ask for the **populated**, **empty**, **in-progress** and **error** state. The happy path is the one state that teaches you nothing.

---

## 1. What the product does

**Software Gateway discovers vendor software releases, verifies them, downloads them into internal repositories, and tracks them through to production.**

A vendor (Nokia, for example) publishes a release - SBC 25.8.1 - as a set of container images, Helm charts and files, routinely **15–60 GB across dozens to hundreds of artifacts**. The application:

1. **Discovers** - polls vendor registries and records every new release, automatically or on demand.
2. **Verifies** - checks the vendor's signature before and after the release is brought inside.
3. **Downloads** - brings the complete release into internal repositories. Existing artifacts are detected and skipped, so a download moves far less than the release weighs.
4. **Replicates onward as part of that same download** - the release lands in JFrog Artifactory (storage) and the Quay registry that OpenShift pulls from is configured to mirror it. **This is one operation, not two.**
5. **Compares** - any two versions or locations, down to the file diff.
6. **Promotes** - moves an approved release to production.

The user is a **Product Owner** at AT&T. They think in releases and products, not in blobs, queues or registries.

**The lifecycle every screen must make obvious:**

```
Discover → Review → Verify → Download & Replicate → Compare → Promote
```

**Scale to design for:** 5–50 products, hundreds of releases, a handful of concurrent downloads, dozens to hundreds of artifacts per release, a year of activity history.

## 2. Design language

- **AT&T enterprise visual language.** AT&T blue as the primary colour. Dark navy sidebar and header areas. White and light-grey content surfaces. Blue for primary actions. Green for success, verified, completed. Red for errors and verification failures. Amber for warnings. Purple sparingly, for lifecycle-state differentiation only.
- **Mature internal operations product, not a generic SaaS dashboard.** Compact spacing, clear hierarchy, subtle borders, restrained rounded corners, clean enterprise typography. Nothing playful, no gradients, no illustration-led empty states.
- **Light theme is the default and the one to design first.** A dark theme may follow; it is not part of this exercise.
- **Full-width desktop.** 1440 × 900 baseline, and important pages use the full available height rather than floating a narrow column in the middle. Responsive down to 1280 px.
- **Icons are meaningful and consistent**: product icons, artifact-type icons (image, Helm chart, file), repository icons (JFrog, Quay, vendor), verification, download, production. **Every action carries a visible text label** - no bare icon buttons without a tooltip, and no ambiguous icon standing in for a verb.
- Identifiers - versions, digests, repository paths, URLs, tags - are always monospace.

## 3. Navigation

A dark navy left sidebar, eight items, in this order:

**Home · Products · Software · Downloads · Repositories · Activity · Reports · Settings**

Nothing else, ever. Detail views are drill-downs from these, not extra nav entries. The sidebar footer shows the signed-in user with their role (`Abhijeet Prasad - Product Owner`).

Header, on every page: page title, one-line description, the contextual right-hand cluster (last discovery timestamp, **Run Discovery**, help, notifications with a count, avatar).

## 4. Rules that make training unnecessary

1. **Every page answers one question in its first screenful**, and the answer is the largest thing on it.
2. **Nouns are pages, verbs are buttons.** Users find the thing, then act on it. Actions live on the object they affect.
3. **One primary action per page**, top right, blue.
4. **The user never manages implementation.** They think "SBC 25.8.1 is new and signed, I'll download it". The system handles JFrog, existing-artifact detection, the Quay mirror, verification and completion, and *reports* each step without asking the user to drive it.
5. **A release is downloaded whole.** Individual artifacts are never selectable for download. Show the composition; do not offer to cherry-pick it.
6. **Never show a number you cannot defend.** No invented percentages, no ETA on work whose bytes are not being counted, no progress bar driven by a timer. Where a value is genuinely unknown, render `-` with a one-line reason on hover. §11 says exactly where this applies.
7. **Every status is stated in words**, not conveyed by colour alone. Colour reinforces; it never carries the only meaning.
8. **Empty states explain and act** - one sentence about what will appear, one button that makes it appear.
9. **External repositories are always reachable.** Any repository URL is clickable and opens in a new tab.

## 5. The vocabulary - use these words, and only these

The UI has one vocabulary and it is the user's, not the engine's. This table is also the implementation bridge: the right-hand column is what the API calls the same thing.

| On screen | Never say | API / internal concept |
|---|---|---|
| **Software** / release | package, artifact bundle | Package |
| **Version** (`25.8.1`) | tag | Package tag, pinned to a manifest digest |
| **Product** (SBC, BGW, NTAS) | - | Product |
| **Vendor** (Nokia) | source registry | Source repository |
| **Location** (Vendor · JFrog · Quay · Production) | target | Target repository |
| **Download** (includes replication) | transfer, replicate, sync, copy | TransferRequest → Transfer(s) |
| **Download** | replication, transfer chain | a `spec.download` entry + the chain its targets declare |
| **Auto-download rule** | trigger, schedule, filter | a `spec.autoDownload` rule - a version pattern and the download it fires |
| **Saved (already present)** | dedupe, placement hit, mount | Blob placements, cross-repository mount |
| **Configure Mirror to Quay** | mirror mode, delegated replication | Quay `replication.mode: mirror` |
| **Verified / Signed** | cosign, sigstore, referrers | Verification |
| **Promote to Production** | promotion transfer | Promotion (target → target) |
| **Activity** | audit trail, audit events | Audit events |
| **Artifacts** - Images, Helm Charts, Files | manifests, blobs, layers | Artifacts and blobs |

"Downloading" and "replicating" are not two words for the user to distinguish. **There is no Replicate button anywhere in this product.**

## 6. The pages

Ten. Eight are nav entries; Software detail and Download are drill-downs from them.

---

### Page 1 - Home / Overview

**Answers:** *What new software is available, what is downloading, and what needs my attention?*

- **Header** - title `Overview`, description "Track and manage software from vendor to production", last-discovery timestamp with a refresh control, auto-discovery on/off indicator, and **Run Discovery** as the primary action.
- **Five KPI cards**, each with the count, a 24-hour delta and an arrow: **New Software · Downloading · Downloaded · Production Ready · Verification Issues**. Each card is clickable and lands on the relevant page pre-filtered. **Do not put "Saved" here** - it belongs to a release, not to the estate (Page 3).
- **Latest Software table** - the centre of the page. Columns: Product (icon + name) · Version · Published · **Verified** · Status · **Location** · Elapsed / Download Time · Actions.
  - **Verified** shows one of three unmistakable states: `✓ Signed`, `⚠ Not Signed`, `✕ Verification Failed`.
  - **Status** is one of `NEW`, `DOWNLOADING`, `DOWNLOADED`, `READY FOR PRODUCTION`, `PRODUCTION`, `VERIFICATION FAILED`.
  - **Location** uses repository icons and reads as a chain where more than one applies - `Vendor: Nokia`, `JFrog + Quay`, `Production`.
  - **Download time** is the *actual* time taken for a completed download (`1h 56m`), `-` for a release that has not been downloaded, and live elapsed time while one is running.
  - **Actions**: `Download` for a NEW release, `View Details` for anything in flight or complete, plus a `⋮` menu (Verify, Compare, Promote, View in vendor registry).
  - Paginated, with a `View all software` link.
- **Right column, stacked:**
  - **Products** summary table - Product · Latest Version · New · Downloaded · Production · Last Discovery · Status, with `View all`.
  - **Download Performance (Last 7 Days)** - **Average Download Speed** (`142 MB/s`) and **Total Data Downloaded** (`1.42 TB`), each with a 7-day delta, plus a small speed trend line. **Do not show "average download time"** - it measures release size, not the system.
- **Attention band** directly under the header, shown only when something is failing: at most three items, each with its fix action inline. The same band on every page, so users learn one place to look.

**Empty state:** "No software discovered yet. Discovery runs every 15 minutes." + **Run Discovery**.

---

### Page 2 - Products

**Answers:** *For each product, what is the newest release, what have we got, and what is in production?*

- **Product list**, one row or card each: product icon and name, latest vendor version, number of new releases (badge), latest downloaded version, current production version, last discovery, discovery status.
- Clicking a product opens its **detail view in place**: a chronological version history, newest first - `25.8.1 → 25.8.0 → 25.7.2 → …`.
- Each version row shows a **compact lifecycle indicator** - `Vendor → Downloaded → Production` with the reached stages filled - plus published date, verification status, download status, download duration, current location and its available actions.
- Product header carries the vendor, the source repository, the owner, and a **Run Discovery** for just this product.

**Also does:** discovery per product, downloading a specific older version, and jumping to any release.

---

### Page 3 - Software (release detail)

**Answers:** *Everything about this one release, and what can I do with it?*

Example: **SBC 25.8.1**

- **Header** - product icon, `SBC 25.8.1`, status badge (`NEW`), verification badge (`✓ Signed`). Beneath, a metadata strip: Vendor · Published · Discovered · Artifacts · Total Size. Primary action **Download**; secondary **Compare**; `⋮` for Verify, Promote, View in vendor registry, Copy pull reference.
- **Lifecycle indicator** - `Vendor → Downloading → Downloaded / Replicated → Production`, with the current position unmistakable, and each reached stage carrying its timestamp.
- **Contents** - the composition as three counted tiles: **Images 18 · Helm Charts 7 · Files 14**, with total artifacts and total size. Expandable to a list per type showing name, tag, size and digest. **Nothing here is selectable for download** - the release downloads whole.
- **Saved (already present)** - this is where it lives, not on Home. Total size, already-present size, percentage, artifacts skipped, and one plain sentence: *"Existing artifacts are automatically detected and skipped to reduce download time."* Example: **Saved: 6.2 GB (40%) · 14 artifacts skipped**. Before a download this is an estimate and must be labelled as one; after it, it is measured.
- **Verification** - signed or not, signature type, signing identity or key, timestamp, and result. When verification failed, the reason in plain language and what it means for using the release.
- **Release notes** - a short vendor summary with a link out.
- **Locations** - where this release currently exists, one row per location, with clickable repository URLs.

---

### Page 4 - Download

**Answers:** *What is happening to this release right now, and what did it cost?*

Header: **Downloading SBC 25.8.1**, with **Elapsed · ETA · Speed** stated prominently (`Elapsed: 1h 24m · ETA: 2h 10m · Speed: 148 MB/s`) and overall progress.

**A four-step stepper representing real backend work:**

1. **Downloading to JFrog**
2. **Configuring Mirror to Quay**
3. **Verification**
4. **Completed**

**Step 1 - Downloading to JFrog.** Repository name and icon, clickable repository URL opening in a new tab, artifacts total and completed, progress, current speed. Below it, **aggregate progress by type** - Images, Helm Charts, Files - each with total, completed, a progress bar and a status. And a persistent note that existing artifacts were skipped: **"6.2 GB already present - download skipped"**, phrased as the system optimising on the user's behalf.

**Step 2 - Configuring Mirror to Quay.** Quay icon, mirror/repository name, clickable URL, configuration progress, completion status. **This step configures Quay and confirms its first sync; it does not move bytes through us**, so it shows *state and timestamps* - `Configured 10:12 · First sync completed 10:31 · Content matches` - and **never** a byte count, percentage, speed or ETA. If the first sync fails, Quay's own error text is shown verbatim, because "our side is fine" is exactly the confusion to pre-empt.

**Step 3 - Verification.** What was checked, where, against which identity, and the result.

**Step 4 - Completed.** A **Download Summary** panel, which is also what the page becomes once the work is done:

- A checklist of the completed steps with their timestamps and durations.
- **Total Size · Downloaded · Saved (already present) · Total Time · Average Speed.**
- **Locations** - JFrog repository and Quay mirror, both clickable, with **View Details**.
- Verification result.

**Failure state:** the failing step is red and expands to state what failed, in plain language, grouped if it affected many artifacts, with a **Retry** that resumes rather than restarts and says so.

---

### Page 5 - Compare

**Answers:** *What is different between these two, exactly?*

Supports vendor vs vendor, vendor vs downloaded, vendor vs production, downloaded vs production - any two available versions or locations of the same software.

- **Selection area** - two clearly separated panels, **Left (Source)** and **Right (Target)**, each with Location, Product and Version selectors, a swap control between them, and a **Compare** button. Recent comparisons are one click away.
- **High-level summary first, always** - Total Artifacts (`39 vs 37`), **New · Removed · Changed · Unchanged** with counts and percentages, and a **Size Comparison** bar (`15.6 GB` vs `13.9 GB`).
- **Broken down by type** - a small table with rows Images, Helm Charts, Files and columns New, Removed, Changed, Unchanged, Total.
- **Tabs** - Overview · Images · Helm Charts · Files · Differences.
- **Detailed differences** - Name · Type · Change (`Added`, `Removed`, `Changed`, `Unchanged`) · Version · Action, each changed item with **View**.
- **File diff** - **View** opens the comparison without leaving the workflow: side-by-side diff with added, removed and changed lines and syntax highlighting for text files; metadata, digest, tag and size comparison for images, charts and binaries.

The path **high-level difference → artifact → actual file diff** must be traversable in three clicks and reversible with a breadcrumb.

**Also does:** promotion planning - compare downloaded against production, then act on the gap from the summary.

---

### Page 6 - Downloads

**Answers:** *How does software get in, and what comes in automatically?*

**Two things on one page, and the page must keep them apart** - this is the distinction the whole feature is built on:

- A **download** is *what happens*: where software goes, in what order, and what has to verify on the way. It carries no pattern.
- An **auto-download rule** is *when it happens by itself*: a version pattern, and the name of the download it triggers. It performs nothing of its own.

A rule fires the same download a person runs by hand. If the page makes that look like two different mechanisms, it has failed.

- **Download panel (top)** - usually one, named or not. Show the chain as a visual flow (**Vendor → JFrog → Quay Mirror**), the verification gates on it, and a **Download…** action that asks for the software by name and then runs. The chain is *derived* from what the targets declare, so where it is longer than what was configured, say which target pulled the extra hop in.
- **Rule list (below)** - name, what versions it matches, which sources it watches, which download it triggers, enabled state, last fired, and what it last produced. A rule row shows its pattern; a download panel never does.
- **Downloading by hand takes software, not a filter.** The action asks for versions, offers what has been discovered, and has no pattern field anywhere. A **Preview** shows the resolved steps before anything runs.
- **Configuration is managed in Git, and this page cannot change it.** Each object carries a `Managed in Git` badge, a **View YAML** panel and an **Open in Git** link; when the registry's actual configuration has drifted from what Git says, a clear **Drift** banner names what differs and offers **Apply**, with the consequences stated before it is pressed. **There is no enable/disable toggle on a rule** - a rule that is off reads *"Disabled in configuration"* and links to the line. What the page *does* is run a download and, when a rule is misbehaving, **Pause the queue**: an action on work, not on Git.
- **Show the chain as steps, and show what stops it.** A download that requires verification before Quay is configured should say so on the row, because that is the sentence a Product Owner needs when asked "could a bad build reach the cluster?" - the answer is visible, not documented.
- **Design the read/drift/apply affordances properly** - this is the page where "the UI shows configuration but Git owns it" either reads as obvious or reads as broken.

---

### Page 7 - Repositories

**Answers:** *Where is our software stored, and are those places healthy?*

- Cards or a table of repositories: name, type, icon, clickable URL, what is stored there (products and release count, total size), health and status, last activity.
- Examples: **JFrog Artifactory** (storage), **Quay** (what OpenShift pulls from), vendor registries (read-only, discovery source), internal file repository.
- Each entry expands to its recent activity, credential status (never a credential value), and a connectivity result - green, or a red line naming what failed: credential, TLS, DNS, proxy.

**Also does:** connectivity checking, and answering "is it us or is it them" at the start of an incident.

---

### Page 8 - Activity

**Answers:** *What happened, when, and who did it?*

- A reverse-chronological trail. Each entry: timestamp · product and version · action · result · user or system · a link to the software it concerns.
- Realistic entries, in this voice: `Discovery completed - 3 new versions found`, `New SBC version 25.8.1 discovered`, `SBC 25.8.1 download started`, `6.2 GB detected as already present`, `Download to JFrog completed`, `Quay mirror configured`, `Signature verified`, `SBC 25.8.1 promoted to production`, `Comparison performed: 25.8.1 vs 25.7.2`.
- **Filters** - product, version, action type, result, actor, date range - composing into the URL so a filtered view can be pasted to a colleague.
- Entries expand to the full record: digests, repositories, durations, request identifiers. **Export** to CSV/JSON.

**Also does:** doubles as per-release history when opened from a software page.

---

### Page 9 - Reports

**Answers:** *How is this operating over time?*

Operational metrics, not decorative charts. Every figure states its period.

- **Download volume** and **total data downloaded**.
- **Average download speed** - the headline performance metric. **Not average download time**, which measures how big the releases were.
- **Download success and failure rate**, with failures grouped by cause.
- **Saved data from existing artifacts**, absolute and as a percentage - the system's clearest demonstration of value.
- **Verification failures** over time.
- **Software promoted to production**.
- **Download duration trends** and **repository usage**.
- Period selector, per-product filter, and export.

---

### Page 10 - Settings

**Answers:** *How is this instance configured, and is it healthy?*

- **Discovery** - global interval, auto-discovery on/off, per-product overrides (read-only where Git owns them).
- **Verification** - default policy: required or advisory, and what a failure does.
- **Notifications** - channels and which events go to whom, with delivery status so "why did nobody get told" has an answer.
- **Users and roles** - who can view, download, and promote.
- **System health** - application version, service status, database, background workers with their load, and one row per external dependency with a real check result.
- **Managed in Git** badges wherever that is the truth, each with a link to the repository.

---

## 7. Components used everywhere

Design once, reuse literally.

- **Product chip** - icon + name, click → Page 2 detail.
- **Version chip** - monospace `25.8.1`, click → Page 3.
- **Verification badge** - `✓ Signed` / `⚠ Not Signed` / `✕ Verification Failed`.
- **Status badge** - the six statuses from Page 1 and no others.
- **Location chip** - repository icon + name, reading as a chain (`JFrog + Quay`), click → Page 7 or the external URL.
- **Lifecycle indicator** - `Vendor → Downloading → Downloaded/Replicated → Production`, compact in tables, expanded with timestamps on Page 3.
- **Artifact-type tile** - Images / Helm Charts / Files with count and size.
- **Progress bar** - real bytes only. The Quay mirror step uses a visually distinct *state* strip that can never be mistaken for a measured bar.
- **Saved panel** - size, percentage, artifacts skipped, and the one-sentence explanation.
- **Repository link** - monospace URL with an external-link icon, new tab.
- **Consequence dialog** - for Apply, Promote and Disable. States what will change in the page's own words, then a single confirming verb (`Promote to production`), never `OK`.

## 8. Language and microcopy

- Sentence case. No exclamation marks. No "Oops".
- Errors state what happened, what it means, and what to do: *"The vendor registry refused our credential (401). The download is paused. Check the Nokia registry credential in Repositories, then retry."*
- Sizes in binary units to one decimal (`15.6 GB`), durations as `1h 56m`, speeds as `142 MB/s`, timestamps relative with an absolute tooltip (`2h ago` → `16 Aug 2026 08:30 AM`).
- Products, versions, repositories and digests are monospace everywhere.

## 9. What to produce

For each of the ten pages, 1440 × 900, light theme, using consistent realistic data: products SBC, BGW, NTAS, CFX, DAS; vendor Nokia; versions in the `25.x.y` family; JFrog `jfrog-releases`; Quay `quay-mirror`; one release mid-download, one verification failure, one release in production.

1. **Populated state** for every page.
2. **Empty state** for Home, Products, Software, Activity.
3. **In-progress state** for Download (step 1 running, step 2 configuring).
4. **Error states** - Download with a failed step and its retry; Home with the attention band showing a verification failure; Downloads with drift detected.
5. **Compare** at three depths: summary, differences list, and an open file diff.
6. **Products** with a product's version history expanded.

Annotate each screen with the question it answers and its primary action.

## 10. Non-negotiables - check every screen

1. One primary action per page.
2. Eight nav items, exactly the eight in §3.
3. **No Replicate button, anywhere.** Download includes replication.
4. **No artifact-level download selection.** A release downloads whole.
5. **No byte progress, percentage, speed or ETA on the Quay mirror step** - it shows state and timestamps (§11).
6. **"Saved" never appears as a top-level KPI**; it lives on the software and download pages.
7. **"Average download time" never appears.** Download speed is the performance metric.
8. No rule or configuration edited in place without the `Managed in Git` treatment and an Apply that states consequences.
9. No credential value on any screen, ever - status only.
10. No bare icon button without a label or tooltip; no status conveyed by colour alone.
11. No empty state without an explanation and an action.
12. No horizontal page scrolling at 1440 px - wide tables scroll inside their own container with the first column pinned.
13. Nothing on any screen a first-time Product Owner would need to be told the meaning of.

## 11. Why the Quay step is different, and must stay different

The Quay mirror step is the one place where the honest-numbers rule has teeth, so it is worth stating why rather than leaving it as an arbitrary constraint.

For the JFrog step, **we move the bytes**, so we count them: progress, speed, ETA and elapsed time are all measured facts. For the Quay step, **Quay pulls the content itself** once we have configured it - our work is the configuration, and the transfer is Quay's. We can see *that* a sync started, *that* it finished, and *what* it produced, but not how many bytes moved or how many remain.

So that step reports what is true: configured at, first sync completed at, content matches or diverged, and Quay's own message when it fails. A progress bar there would be a number derived from a timer, and someone would make a decision from it.

The same rule applies wherever a Quay destination is involved: state and timestamps, never synthesised progress. The engineering reasoning is [18](../design/18-quay-replication.md) §6.1; the user-facing consequence is this paragraph.
