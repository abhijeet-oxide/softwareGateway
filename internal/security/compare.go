package security

import (
	"fmt"
	"sort"
	"strings"
)

// Verdict is the one-word answer to "is the new release better or worse".
//
// Four values, and the fourth is the one that earns this type its place.
// Better/worse/unchanged over incomplete data is a guess wearing a badge, and
// the person reading it has no way to tell. Inconclusive says so.
type Verdict string

const (
	// VerdictBetter means the new release's security posture improved.
	VerdictBetter Verdict = "better"
	// VerdictWorse means it deteriorated.
	VerdictWorse Verdict = "worse"
	// VerdictUnchanged means there is no meaningful security difference, over
	// data complete enough to say so.
	VerdictUnchanged Verdict = "unchanged"
	// VerdictInconclusive means the data does not support an answer: a scanner
	// disabled, a release unscanned, coverage too partial for "no difference"
	// to mean anything.
	VerdictInconclusive Verdict = "inconclusive"
)

// Label is the verdict in the words the interface shows.
func (v Verdict) Label() string {
	switch v {
	case VerdictBetter:
		return "Better"
	case VerdictWorse:
		return "Worse"
	case VerdictUnchanged:
		return "Unchanged"
	default:
		return "Inconclusive"
	}
}

// ChangeType is what happened to one finding between two releases.
type ChangeType string

const (
	// ChangeIntroduced is present in the new release and not the old one.
	ChangeIntroduced ChangeType = "introduced"
	// ChangeResolved is present in the old release and not the new one.
	ChangeResolved ChangeType = "resolved"
	// ChangeUnchanged is present in both, at the same severity and the same
	// remediation status.
	ChangeUnchanged ChangeType = "unchanged"
	// ChangeSeverityIncreased and ChangeSeverityDecreased are present in both
	// and re-graded. Two types rather than one with a direction, because they
	// are read as different events and filtered separately.
	ChangeSeverityIncreased ChangeType = "severity_increased"
	ChangeSeverityDecreased ChangeType = "severity_decreased"
	// ChangeRemediation is present in both at the same severity, but a fix
	// became available - or stopped being available. Not a severity change and
	// not nothing: a critical that just became fixable is this afternoon's work.
	ChangeRemediation ChangeType = "remediation_changed"
	// ChangeRemovedArtifact is present in the old release on an artifact the
	// new one does not ship, where the rules did NOT confirm the removal as an
	// improvement. Reported separately rather than counted as resolved.
	ChangeRemovedArtifact ChangeType = "removed_artifact"
)

// Improvement reports whether this change makes the release better.
func (c ChangeType) Improvement() bool {
	return c == ChangeResolved || c == ChangeSeverityDecreased
}

// Deterioration reports whether this change makes the release worse.
func (c ChangeType) Deterioration() bool {
	return c == ChangeIntroduced || c == ChangeSeverityIncreased
}

// ArtifactChange is what happened to an artifact between two releases.
type ArtifactChange string

const (
	// ArtifactCommon is the same artifact, same digest, in both releases.
	ArtifactCommon ArtifactChange = "common"
	// ArtifactUpgraded is the same artifact by name at a different digest -
	// the case a patch release is made of.
	ArtifactUpgraded ArtifactChange = "upgraded"
	// ArtifactAdded is in the new release only.
	ArtifactAdded ArtifactChange = "added"
	// ArtifactRemoved is in the old release only.
	ArtifactRemoved ArtifactChange = "removed"
)

// Change is one finding's fate between two releases.
type Change struct {
	Type ChangeType `json:"type"`

	CVE string `json:"cve,omitempty"`
	ID  string `json:"id,omitempty"`

	// Severity is the severity that matters for this change: the new one for
	// anything present in B, the old one for anything only in A.
	Severity Severity `json:"severity"`
	// FromSeverity and ToSeverity are set on a re-grading, and empty otherwise.
	FromSeverity Severity `json:"fromSeverity,omitempty"`
	ToSeverity   Severity `json:"toSeverity,omitempty"`

	Fixable bool     `json:"fixable"`
	FixedIn []string `json:"fixedIn,omitempty"`

	Summary     string    `json:"summary,omitempty"`
	Description string    `json:"description,omitempty"`
	Component   Component `json:"component"`

	// Artifact is where this change is observed: the new release's artifact
	// when there is one, the old release's otherwise.
	Artifact ArtifactRef `json:"artifact"`
	// ArtifactChange says what happened to that artifact, so a reader can tell
	// "this CVE arrived in a new image" from "this CVE arrived in an image that
	// was already there".
	ArtifactChange ArtifactChange `json:"artifactChange"`

	// ViaRemoval marks a resolution that happened because the artifact carrying
	// it is no longer shipped, rather than because anything was patched. Both
	// are genuine improvements and they are not the same event.
	ViaRemoval bool `json:"viaRemoval,omitempty"`

	Provider string `json:"provider,omitempty"`
}

// ArtifactDelta is one artifact's fate, with its security arithmetic.
type ArtifactDelta struct {
	// Key is the name both releases know the artifact by.
	Key    string         `json:"key"`
	Change ArtifactChange `json:"change"`

	A *ArtifactRef `json:"a,omitempty"`
	B *ArtifactRef `json:"b,omitempty"`

	StatusA Status `json:"statusA,omitempty"`
	StatusB Status `json:"statusB,omitempty"`

	CountsA Counts `json:"countsA"`
	CountsB Counts `json:"countsB"`

	Introduced      int `json:"introduced"`
	Resolved        int `json:"resolved"`
	Unchanged       int `json:"unchanged"`
	SeverityChanged int `json:"severityChanged"`

	// Comparable is false when one side of this pair has no scan result, so
	// nothing about it could be classified. The count columns are then zero
	// because nothing was computed, not because nothing changed.
	Comparable bool `json:"comparable"`
}

// Comparison is the whole answer: one word, one sentence, and every row behind
// them.
//
// The simple view and the detailed view are the same object read at two depths,
// deliberately. Two objects would drift, and the day they drift is the day the
// headline says "better" over a table that says otherwise.
type Comparison struct {
	Verdict Verdict `json:"verdict"`
	// Headline is the one-line answer: "Release 25.7_mp2604_2131 is better than
	// release 25.7_mp2603_2100".
	Headline string `json:"headline"`
	// Explanation is the plain-language paragraph a person with no security
	// background can act on. No CVE terminology, no jargon, no percentages.
	Explanation string `json:"explanation"`
	// Caveats are the things that qualify the answer - unscanned artifacts, a
	// disabled scanner, findings that could not be classified. Separate from
	// Explanation so the interface can render them as warnings rather than
	// burying them in a sentence.
	Caveats []string `json:"caveats,omitempty"`

	Introduced         Counts `json:"introduced"`
	Resolved           Counts `json:"resolved"`
	Unchanged          Counts `json:"unchanged"`
	SeverityIncreased  Counts `json:"severityIncreased"`
	SeverityDecreased  Counts `json:"severityDecreased"`
	RemediationChanged Counts `json:"remediationChanged"`
	// RemovedArtifact counts findings that left with an artifact whose removal
	// the rules would not confirm as an improvement. Not in Resolved.
	RemovedArtifact Counts `json:"removedArtifact"`

	// NetScore is the severity-weighted difference. Negative is better.
	// Exposed because "why did it say worse" is the second question, and a
	// number that can be checked is a better answer than a rule that cannot.
	NetScore int `json:"netScore"`

	Changes   []Change        `json:"changes"`
	Artifacts []ArtifactDelta `json:"artifacts"`

	CoverageA Coverage `json:"coverageA"`
	CoverageB Coverage `json:"coverageB"`

	// ArtifactSummary is how many artifacts were common, added, removed and
	// upgraded - the delta the requirement calls out, because two releases do
	// not contain the same artifacts and a comparison that assumes they do is
	// wrong before it starts.
	ArtifactSummary ArtifactSummary `json:"artifactSummary"`

	NameA string `json:"nameA,omitempty"`
	NameB string `json:"nameB,omitempty"`
}

// ArtifactSummary counts the artifact delta.
type ArtifactSummary struct {
	Common   int `json:"common"`
	Upgraded int `json:"upgraded"`
	Added    int `json:"added"`
	Removed  int `json:"removed"`
	// NotComparable is pairs where one side has no scan result.
	NotComparable int `json:"notComparable"`
}

// CompareInput is two releases' security state, plus what to call them.
type CompareInput struct {
	A     []Report
	B     []Report
	NameA string
	NameB string
}

// Compare classifies two releases' findings and reaches a verdict.
//
// # The order of the two arguments
//
// A is the OLD release and B the NEW one. Every word in the output is written
// from B's point of view - "resolved" means B no longer has it - because that
// is the question being asked: not "how do these differ" but "is what I am
// about to ship better than what I am running".
func Compare(in CompareInput) Comparison {
	a := indexReports(in.A)
	b := indexReports(in.B)

	postureA := Summarize(in.A)
	postureB := Summarize(in.B)

	cmp := Comparison{
		NameA:     in.NameA,
		NameB:     in.NameB,
		CoverageA: postureA.Coverage,
		CoverageB: postureB.Coverage,
	}

	pairs := pairArtifacts(a, b)

	// Every (CVE, component) B holds anywhere. A finding that left one artifact
	// and appeared on another has not been resolved, it has moved, and counting
	// it as resolved would let a repackaging read as a security improvement.
	presentInB := map[string]bool{}
	for _, r := range b {
		if r.Status != StatusScanned {
			continue
		}
		for _, f := range r.Findings {
			presentInB[f.Key()] = true
		}
	}

	// Whether B's coverage is complete enough to trust a removal as a genuine
	// improvement. If B has artifacts nobody scanned, "the CVE is gone" may
	// only mean "the CVE is somewhere nobody looked".
	removalConclusive := postureB.Coverage.Complete() && postureA.Coverage.Any()

	for _, p := range pairs {
		delta := ArtifactDelta{Key: p.key, Change: p.change}
		if p.a != nil {
			ref := p.a.Artifact
			delta.A = &ref
			delta.StatusA = p.a.Status
			delta.CountsA = p.a.Counts
		}
		if p.b != nil {
			ref := p.b.Artifact
			delta.B = &ref
			delta.StatusB = p.b.Status
			delta.CountsB = p.b.Counts
		}

		switch p.change {
		case ArtifactAdded:
			if p.b.Status == StatusScanned {
				delta.Comparable = true
				for _, f := range p.b.Findings {
					cmp.append(newChange(ChangeIntroduced, f, p.b.Artifact, ArtifactAdded))
					delta.Introduced++
				}
			}
		case ArtifactRemoved:
			if p.a.Status == StatusScanned {
				delta.Comparable = true
				for _, f := range p.a.Findings {
					if presentInB[f.Key()] {
						// It moved. Not gone.
						cmp.append(newChange(ChangeUnchanged, f, p.a.Artifact, ArtifactRemoved))
						delta.Unchanged++
						continue
					}
					if removalConclusive {
						c := newChange(ChangeResolved, f, p.a.Artifact, ArtifactRemoved)
						c.ViaRemoval = true
						cmp.append(c)
						delta.Resolved++
						continue
					}
					cmp.append(newChange(ChangeRemovedArtifact, f, p.a.Artifact, ArtifactRemoved))
				}
			}
		default: // common or upgraded
			if p.a.Status != StatusScanned || p.b.Status != StatusScanned {
				break
			}
			delta.Comparable = true
			classifyPair(&cmp, &delta, p)
		}

		if !delta.Comparable && (p.change == ArtifactCommon || p.change == ArtifactUpgraded) {
			cmp.ArtifactSummary.NotComparable++
		}
		switch p.change {
		case ArtifactCommon:
			cmp.ArtifactSummary.Common++
		case ArtifactUpgraded:
			cmp.ArtifactSummary.Upgraded++
		case ArtifactAdded:
			cmp.ArtifactSummary.Added++
		case ArtifactRemoved:
			cmp.ArtifactSummary.Removed++
		}
		cmp.Artifacts = append(cmp.Artifacts, delta)
	}

	sortChanges(cmp.Changes)
	sort.SliceStable(cmp.Artifacts, func(i, j int) bool { return cmp.Artifacts[i].Key < cmp.Artifacts[j].Key })

	cmp.NetScore = netScore(cmp)
	cmp.Verdict, cmp.Caveats = decide(cmp, postureA, postureB)
	cmp.Headline, cmp.Explanation = explain(cmp)
	return cmp
}

// classifyPair aligns the findings of one artifact present in both releases.
func classifyPair(cmp *Comparison, delta *ArtifactDelta, p pair) {
	old := map[string]Finding{}
	for _, f := range p.a.Findings {
		// Worst wins on a duplicate key: the same CVE reported twice for one
		// component at two paths is one finding, and taking the worse grade
		// keeps a re-grading from being invented by deduplication order.
		if prev, ok := old[f.Key()]; !ok || f.Severity.Rank() > prev.Severity.Rank() {
			old[f.Key()] = f
		}
	}
	seen := map[string]bool{}

	for _, f := range p.b.Findings {
		k := f.Key()
		if seen[k] {
			continue
		}
		seen[k] = true

		prev, existed := old[k]
		if !existed {
			cmp.append(newChange(ChangeIntroduced, f, p.b.Artifact, p.change))
			delta.Introduced++
			continue
		}
		switch {
		case f.Severity.Rank() > prev.Severity.Rank():
			c := newChange(ChangeSeverityIncreased, f, p.b.Artifact, p.change)
			c.FromSeverity, c.ToSeverity = prev.Severity, f.Severity
			cmp.append(c)
			delta.SeverityChanged++
		case f.Severity.Rank() < prev.Severity.Rank():
			c := newChange(ChangeSeverityDecreased, f, p.b.Artifact, p.change)
			c.FromSeverity, c.ToSeverity = prev.Severity, f.Severity
			cmp.append(c)
			delta.SeverityChanged++
		case f.Fixable != prev.Fixable:
			cmp.append(newChange(ChangeRemediation, f, p.b.Artifact, p.change))
			delta.Unchanged++
		default:
			cmp.append(newChange(ChangeUnchanged, f, p.b.Artifact, p.change))
			delta.Unchanged++
		}
	}

	for k, f := range old {
		if seen[k] {
			continue
		}
		cmp.append(newChange(ChangeResolved, f, p.a.Artifact, p.change))
		delta.Resolved++
	}
}

// append records a change and folds it into the right total.
func (c *Comparison) append(ch Change) {
	c.Changes = append(c.Changes, ch)
	switch ch.Type {
	case ChangeIntroduced:
		c.Introduced.Add(ch.Severity, ch.Fixable)
	case ChangeResolved:
		c.Resolved.Add(ch.Severity, ch.Fixable)
	case ChangeUnchanged:
		c.Unchanged.Add(ch.Severity, ch.Fixable)
	case ChangeSeverityIncreased:
		c.SeverityIncreased.Add(ch.ToSeverity, ch.Fixable)
	case ChangeSeverityDecreased:
		c.SeverityDecreased.Add(ch.ToSeverity, ch.Fixable)
	case ChangeRemediation:
		c.RemediationChanged.Add(ch.Severity, ch.Fixable)
	case ChangeRemovedArtifact:
		c.RemovedArtifact.Add(ch.Severity, ch.Fixable)
	}
}

func newChange(t ChangeType, f Finding, artifact ArtifactRef, ac ArtifactChange) Change {
	return Change{
		Type:           t,
		CVE:            f.CVE,
		ID:             f.ID,
		Severity:       f.Severity,
		Fixable:        f.Fixable,
		FixedIn:        f.FixedIn,
		Summary:        f.Summary,
		Description:    f.Description,
		Component:      f.Component,
		Artifact:       artifact,
		ArtifactChange: ac,
		Provider:       f.Provider,
	}
}

// pair is one artifact key's two sides.
type pair struct {
	key    string
	change ArtifactChange
	a      *Report
	b      *Report
}

// indexReports groups reports by cross-release artifact key, merging the
// findings of artifacts that share one - an index and its per-platform
// manifests both answer to the same name.
func indexReports(reports []Report) map[string]*Report {
	out := map[string]*Report{}
	for i := range reports {
		r := reports[i]
		k := r.Artifact.ArtifactKey()
		existing, ok := out[k]
		if !ok {
			cp := r
			cp.Findings = append([]Finding(nil), r.Findings...)
			out[k] = &cp
			continue
		}
		existing.Findings = append(existing.Findings, r.Findings...)
		// A group is scanned only if something in it was: an index whose
		// children are scanned is scanned, and one nobody touched is not.
		if r.Status == StatusScanned {
			existing.Status = StatusScanned
		}
		existing.Recount()
	}
	return out
}

// pairArtifacts aligns two releases' artifacts by name, then rescues renames by
// digest.
//
// The rescue matters more than it sounds. An artifact renamed between releases
// looks like one removal and one addition, which double-counts every finding it
// carries: once as resolved and once as introduced. The digests are identical
// in that case, and that is a fact worth using.
func pairArtifacts(a, b map[string]*Report) []pair {
	pairs := make([]pair, 0, len(a)+len(b))
	usedB := map[string]bool{}

	keys := make([]string, 0, len(a))
	for k := range a {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		ra := a[k]
		if rb, ok := b[k]; ok {
			usedB[k] = true
			change := ArtifactUpgraded
			if ra.Artifact.Ref() == rb.Artifact.Ref() {
				change = ArtifactCommon
			}
			pairs = append(pairs, pair{key: k, change: change, a: ra, b: rb})
			continue
		}
		// Same bytes under another name is the same artifact.
		if rb, key := findByDigest(b, ra.Artifact.Digest, usedB); rb != nil {
			usedB[key] = true
			pairs = append(pairs, pair{key: k, change: ArtifactCommon, a: ra, b: rb})
			continue
		}
		pairs = append(pairs, pair{key: k, change: ArtifactRemoved, a: ra})
	}

	bkeys := make([]string, 0, len(b))
	for k := range b {
		bkeys = append(bkeys, k)
	}
	sort.Strings(bkeys)
	for _, k := range bkeys {
		if usedB[k] {
			continue
		}
		pairs = append(pairs, pair{key: k, change: ArtifactAdded, b: b[k]})
	}
	return pairs
}

func findByDigest(m map[string]*Report, digest string, used map[string]bool) (*Report, string) {
	if digest == "" {
		return nil, ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if used[k] {
			continue
		}
		if m[k].Artifact.Digest == digest {
			return m[k], k
		}
	}
	return nil, ""
}

// netScore is the severity-weighted difference. Negative is an improvement.
//
// Re-gradings contribute the DIFFERENCE in weight rather than the whole of the
// new severity: a medium that became high is 90 points worse, not 100, because
// the medium was already there.
func netScore(c Comparison) int {
	score := c.Introduced.BySeverity.Weight() - c.Resolved.BySeverity.Weight()
	for _, ch := range c.Changes {
		switch ch.Type {
		case ChangeSeverityIncreased, ChangeSeverityDecreased:
			score += ch.ToSeverity.Weight() - ch.FromSeverity.Weight()
		}
	}
	return score
}

// decide reaches the verdict and states what qualifies it.
//
// # Why "no difference" is not automatically "unchanged"
//
// A comparison over two releases where a fifth of the artifacts were never
// scanned can produce a zero score for two entirely different reasons: nothing
// changed, or the changes are in the part nobody looked at. Those must not
// share a word. A DIFFERENCE found over partial data is still a difference -
// the findings are real - so partial coverage downgrades "unchanged" to
// inconclusive and leaves better/worse standing, with a caveat.
func decide(c Comparison, a, b Posture) (Verdict, []string) {
	var caveats []string

	switch {
	case !a.Coverage.Any() && !b.Coverage.Any():
		return VerdictInconclusive, []string{coverageCaveat("Neither release", a.Coverage, b.Coverage)}
	case !a.Coverage.Any():
		return VerdictInconclusive, []string{fmt.Sprintf(
			"There are no scan results for %s, so there is nothing to compare against.", displayName(c.NameA, "the base release"))}
	case !b.Coverage.Any():
		return VerdictInconclusive, []string{fmt.Sprintf(
			"There are no scan results for %s, so its security cannot be judged.", displayName(c.NameB, "the new release"))}
	}

	if n := a.Coverage.NotScanned + a.Coverage.Unavailable + a.Coverage.Disabled; n > 0 {
		caveats = append(caveats, fmt.Sprintf("%d of %d artifacts in %s have no scan result.",
			n, a.Coverage.Scannable(), displayName(c.NameA, "the base release")))
	}
	if n := b.Coverage.NotScanned + b.Coverage.Unavailable + b.Coverage.Disabled; n > 0 {
		caveats = append(caveats, fmt.Sprintf("%d of %d artifacts in %s have no scan result.",
			n, b.Coverage.Scannable(), displayName(c.NameB, "the new release")))
	}
	if c.ArtifactSummary.NotComparable > 0 {
		caveats = append(caveats, fmt.Sprintf(
			"%d artifacts present in both releases could not be compared because one side has no scan result.",
			c.ArtifactSummary.NotComparable))
	}
	if !c.RemovedArtifact.Empty() {
		caveats = append(caveats, fmt.Sprintf(
			"%d findings belong to artifacts that are no longer present in this release. They are listed separately rather than counted as resolved, because the scan data does not confirm the removal as an improvement.",
			c.RemovedArtifact.Total))
	}
	if c.Introduced.BySeverity.Unknown+c.Resolved.BySeverity.Unknown > 0 {
		caveats = append(caveats, "Some findings carry no severity grade and do not affect the verdict.")
	}

	complete := a.Coverage.Complete() && b.Coverage.Complete() && c.ArtifactSummary.NotComparable == 0

	switch {
	case c.NetScore < 0:
		return VerdictBetter, caveats
	case c.NetScore > 0:
		return VerdictWorse, caveats
	case !complete:
		return VerdictInconclusive, caveats
	default:
		return VerdictUnchanged, caveats
	}
}

func coverageCaveat(who string, a, b Coverage) string {
	if a.Disabled > 0 || b.Disabled > 0 {
		return who + " has security scanning enabled, so there is nothing to compare."
	}
	return who + " has been scanned, so there is nothing to compare."
}

// explain writes the headline and the paragraph.
//
// Written for somebody who does not know what a CVE is. No identifiers, no
// percentages, no scanner names: counts, severities, and a direction. The
// detailed view is where the vocabulary lives.
func explain(c Comparison) (headline, explanation string) {
	newName := displayName(c.NameB, "The new release")
	oldName := displayName(c.NameA, "the previous release")

	switch c.Verdict {
	case VerdictBetter:
		headline = fmt.Sprintf("%s is better than %s.", newName, oldName)
	case VerdictWorse:
		headline = fmt.Sprintf("%s is worse than %s.", newName, oldName)
	case VerdictUnchanged:
		headline = fmt.Sprintf("%s is unchanged from %s.", newName, oldName)
	default:
		headline = fmt.Sprintf("Whether %s is better or worse than %s cannot be determined.", newName, oldName)
	}

	var clauses []string
	if !c.Resolved.Empty() {
		clauses = append(clauses, "resolves "+phrase(c.Resolved.BySeverity, "vulnerability", "vulnerabilities"))
	}
	if !c.Introduced.Empty() {
		clauses = append(clauses, "introduces "+phrase(c.Introduced.BySeverity, "vulnerability", "vulnerabilities"))
	}
	if n := c.SeverityIncreased.Total; n > 0 {
		clauses = append(clauses, fmt.Sprintf("has %s that became more severe", countNoun(n, "vulnerability", "vulnerabilities")))
	}
	if n := c.SeverityDecreased.Total; n > 0 {
		clauses = append(clauses, fmt.Sprintf("has %s that became less severe", countNoun(n, "vulnerability", "vulnerabilities")))
	}

	switch {
	case len(clauses) == 0 && c.Verdict == VerdictInconclusive:
		explanation = headline + " " + firstCaveat(c)
	case len(clauses) == 0:
		explanation = headline + fmt.Sprintf(" No vulnerabilities were resolved or introduced, and %s remain in both.",
			countNoun(c.Unchanged.Total, "vulnerability", "vulnerabilities"))
	default:
		explanation = headline + " It " + joinClauses(clauses) + "."
	}

	if n := c.Unchanged.Total; n > 0 && len(clauses) > 0 {
		explanation += fmt.Sprintf(" %s carry over unchanged.", capitalize(countNoun(n, "vulnerability", "vulnerabilities")))
	}
	return headline, explanation
}

func firstCaveat(c Comparison) string {
	if len(c.Caveats) > 0 {
		return c.Caveats[0]
	}
	return "The available scan data is not enough to compare the two releases."
}

// phrase renders a severity breakdown the way a person would say it:
// "3 high and 5 medium vulnerabilities".
func phrase(counts SeverityCounts, singular, plural string) string {
	var parts []string
	for _, s := range Severities {
		n := counts.Get(s)
		if n == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%d %s", n, strings.ToLower(s.Label())))
	}
	if len(parts) == 0 {
		return "no " + plural
	}
	noun := plural
	if counts.Total() == 1 {
		noun = singular
	}
	return joinClauses(parts) + " " + noun
}

func countNoun(n int, singular, plural string) string {
	if n == 1 {
		return "1 " + singular
	}
	return fmt.Sprintf("%d %s", n, plural)
}

func joinClauses(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
	}
}

func displayName(name, fallback string) string {
	if strings.TrimSpace(name) == "" {
		return fallback
	}
	return name
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// sortChanges orders the detailed table: the events that matter first, worst
// severity first within each, then by identifier so the order is stable.
func sortChanges(cs []Change) {
	order := map[ChangeType]int{
		ChangeSeverityIncreased: 0,
		ChangeIntroduced:        1,
		ChangeRemovedArtifact:   2,
		ChangeResolved:          3,
		ChangeSeverityDecreased: 4,
		ChangeRemediation:       5,
		ChangeUnchanged:         6,
	}
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		if order[a.Type] != order[b.Type] {
			return order[a.Type] < order[b.Type]
		}
		if a.Severity.Rank() != b.Severity.Rank() {
			return a.Severity.Rank() > b.Severity.Rank()
		}
		if a.CVE != b.CVE {
			return a.CVE < b.CVE
		}
		if a.Component.ComponentKey() != b.Component.ComponentKey() {
			return a.Component.ComponentKey() < b.Component.ComponentKey()
		}
		return a.Artifact.ArtifactKey() < b.Artifact.ArtifactKey()
	})
}
