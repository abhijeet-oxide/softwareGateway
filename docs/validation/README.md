# Custom Software Validation

Checking a vendor's release against this organization's own Kubernetes and CNF
standards - automatically, at ingest, with every finding addressed to one
Kubernetes object inside one chart inside one release.

**Building it? Read [design/23 - Custom Software Validation](../design/23-validation.md).**
That is the implementation plan: architecture, rendering, data model, API, UI,
report, milestones. This directory is the ground truth it rests on.

---

## Document set

| # | Document | Covers |
|---|---|---|
| — | [custom-validation.md](custom-validation.md) | **The source catalog.** 118 assertions across 13 categories, written from lab and production experience. Unchanged, and the reason everything else exists |
| 00 | [The Validation Model](00-validation-model.md) | What a check is allowed to say. Outcomes, severity, determinacy, the address, verdicts, waivers, and what is deliberately out of scope |
| 01 | [Check Catalog](01-check-catalog.md) | Every one of the 118 assertions triaged - automatable now, needs a values file, or needs a human reading a document - plus 8 proposed additions and the six checks that are easy to get wrong |
| 02 | [Authoring Checks](02-authoring-checks.md) | The extension contract: the pack manifest, the Rego input and output, ID rules, and what a new check must prove before it can fail anybody's release |
| 03 | [Review of the Existing Policies](03-sample-policy-review.md) | An honest read of the sixteen `.rego` files in [sample-policies/](sample-policies/): what is sound, what is broken, what each becomes |
| — | [sample-policies/](sample-policies/) | The policies as they were handed over. Kept as-is; see 03 before adopting any of them |

## Reading order

| If you are… | Read |
|---|---|
| Deciding whether this is worth building | 00 → 01 §4 → design/23 §1-2 |
| Implementing the engine | 00 → design/23 → 01 §3 |
| Writing a new check | 02 → 01 §3 → 00 §5 |
| Wondering what happened to the existing Rego | 03 |
| Explaining a finding to a vendor | 00 §3 → 01, the row for that ID |

## The model in ten lines

- **A result is about one resource**, never about "the chart". A finding that
  cannot be pasted into a ticket is not a finding.
- **A pass is a result, and so is a skip.** An engine that emits only violations
  reports "compliant", "not applicable" and "the traversal never got there"
  identically - and the third is happening in the existing policies today.
- **`error` is an outcome.** A check that could not be decided never becomes a
  pass, and a run containing one is *inconclusive*, not green.
- **Severity belongs to the check; outcome belongs to the result.** Two fields,
  because a policy that can pick its own severity has no severity.
- **Determinacy is measured, not assumed.** Rendering twice - once with the
  chart's values, once with them perturbed - says whether a value is fixed by the
  template or merely defaulted. That is what lets tier 1 block.
- **The address is handed to the check and echoed back**, so no policy can
  construct one wrongly and none can forget it.
- **Passes are derived** from a declared applicability set, so the denominator is
  always right and a violation-style rule still produces a complete report.
- **Every run records what produced it** - policy bundle digest, engine, helm and
  Kubernetes version - because a report nobody can reproduce is an opinion.
- **Waivers live in Git**, are scoped, and expire. An approval that can be
  granted through a UI is an approval with no reviewer.
- **A check ships with fixtures or it does not ship.** A meta-test fails CI when a
  registered check has no positive and negative case, and a shared good chart
  makes "no false positives" a CI assertion rather than a claim.
