---
name: interface-copy
description: Use before writing or editing any user-visible string in this repository - a label, a button, a card title, a column header, a tooltip, an empty state, an error, a log line the interface renders, or the Go text behind one (Stage.Label, event text, API `detail`). Enforces this product's register: every label names its subject and its verb, nothing is conversational, and a number always says what it counts. Also use when reviewing an interface for copy defects.
user-invocable: true
---

# Interface copy

This is an operations tool used by release managers, platform engineers and
vendors. Its text is read under time pressure, quoted into tickets, and pasted
into mail to a vendor who does not have access to it. That is the register:
**precise, complete, and impersonal.** Not chatty, not cute, not a caption on a
screenshot.

The failures below are not style preferences. Each one was found in review of
shipped screens in this repository, and each cost a round trip.

## The four rules

### 1. A label names its subject AND its verb

A heading over a list of things that happened must say what happened to what.
"What has happened" is a fragment with no subject: it prompts the question
"happened to *what*?" and answers nothing. A person scanning a card should be
able to read the label alone and know what the block under it contains.

| Wrong | Right | Why |
|---|---|---|
| `What has happened` | `Run log` | Names the thing, not the sensation of watching it |
| `What was found` | `Finding` | The column holds a finding, so it is called one |
| `What to do` | `Remediation` | The reader is not being spoken to |
| `Whose to fix` | `Owner` / `Determinacy` | Same |
| `Charts found` | `Charts discovered` | A found thing is passive; discovery is the stage that produced it |

A bare noun is fine when the noun *is* the content (`Remediation`, `Run log`,
`Coverage`). A bare noun is wrong when it describes an experience
(`What has happened`, `Still going`, `Almost there`).

### 2. Nothing is phrased as speech

The tool states facts. It does not narrate, reassure, or estimate aloud.

| Wrong | Right |
|---|---|
| `4 at a time` | `4 charts in parallel` |
| `about 2s left on this stage` | `Estimated 2s remaining in this stage` |
| `Analyzed in a moment` | `Analyzed in under a second` |
| `starting` | `Starting` (or a real count) |
| `Reading it…` | `Loading manifest` |
| `Open whole` | `Open full manifest` |
| `not kept` | `Not retained` |
| `nothing rendered` | `No output` |
| `is as far as that path goes` | `is the deepest resolved element of this path` |
| `Somebody is already doing it` | `A walk is already in progress` |

Test: read the string in a monotone. If it sounds like one colleague telling
another what the screen is doing, rewrite it as the fact itself.

Hedges are the same defect: `about`, `roughly`, `a moment`, `shortly`,
`should be`, `probably`. Either the number is known - state it - or it is not,
and the field is absent.

### 3. A number states its unit and what it counts

`50` alone is a defect. `50 charts`, `50 of 95 rendered`, `50 objects` are not.
This applies to tile values whose label supplies the noun (`Rendered` / `50` is
acceptable *because the label is directly above*), and never applies to a number
in running text.

Percentages and durations follow the shared formatters
(`web/src/domain/format.ts`) so the same quantity reads identically everywhere.
Never hand-format a byte count or a duration.

### 4. The reader is never addressed, and the tool never refers to itself

No `you`, `your`, `we`, `our`, `let's`, `sorry`, `oops`, `please`. State the
condition and the remedy in the third person.

| Wrong | Right |
|---|---|
| `We couldn't reach the registry` | `The registry did not respond` |
| `You need to analyse this release first` | `This release must be analysed first` |
| `Sorry, nothing to show` | `No results` |

The one deliberate exception is a **consequence sentence on a destructive or
expensive control**, where the second person is clearer than a passive
construction and the sentence is doing real work. Prefer the impersonal form
even there when it reads as well.

## Applies to Go as much as to TSX

Strings that reach a screen are not only in `web/src/`. These are in scope and
are missed most often:

- `Stage.Label()` / `Stage.Detail()` and any `Label()` on a domain enum
- progress event text (`rep.Event(...)`)
- RFC 9457 `detail` in `Error(w, r, code, "…")` - it is rendered verbatim
- `Skipped`/`RenderError` strings recorded on a run, which end up in a table
- CLI output that mirrors a screen

An error `detail` is held to the same rules **plus one**: it must say what was
attempted, what happened, and what to do. `"compliance is not configured on this
Coordinator"` is complete. `"could not do that"` is not.

## Before you finish

Run this over the diff. Every hit is either a defect or a deliberate exception
you can defend:

```sh
git diff -U0 | grep -nE '^\+' | grep -inE \
  "what (has|was|is) |whose |you |your |we |our |let'?s |sorry|oops|
   \bat a time\b|about [0-9]|a moment|shortly|probably|just now,|
   ^\+.*['\"][a-z][a-z ]{0,20}…['\"]" || echo "clean"
```

Then read every new string aloud once. A string that cannot be read in a flat
voice without sounding like conversation is not finished.

## Where the vocabulary comes from

Product nouns are fixed by `docs/design/19-user-interface.md` §3.1 - **Software**,
**Download**, **Location**, **Download Rule** - and the interface must not leak
the domain model's words (`package`, `transfer`, `job`, `blob`, `placement`).
This skill governs the *grammar* of a label; that section governs the *noun*.
Both apply.
