package api

import (
	"strconv"
	"strings"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/security"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// Domain to wire. One direction only: nothing here parses a request body.
//
// The labels - StatusLabel, SeverityLabel, VerdictLabel - are computed HERE
// rather than in the browser, and that is the point of the "simple view". A
// client deriving "Release B is better than Release A" from counts is a client
// that will eventually derive it differently from the next client, over the
// same data, and one of the two will be wrong in a release meeting.

func toAPISeverityCounts(c security.SeverityCounts) v1.SecuritySeverityCounts {
	return v1.SecuritySeverityCounts{
		Critical: c.Critical, High: c.High, Medium: c.Medium, Low: c.Low, Unknown: c.Unknown,
	}
}

func toAPICounts(c security.Counts) v1.SecurityCounts {
	return v1.SecurityCounts{
		Total:             c.Total,
		Fixable:           c.Fixable,
		NonFixable:        c.NonFixable,
		KEV:               c.KEV,
		KEVFixable:        c.KEVFixable,
		BySeverity:        toAPISeverityCounts(c.BySeverity),
		FixableBySeverity: toAPISeverityCounts(c.FixableBySeverity),
		KEVBySeverity:     toAPISeverityCounts(c.KEVBySeverity),
	}
}

func toAPICoverage(c security.Coverage) v1.SecurityCoverage {
	return v1.SecurityCoverage{
		Artifacts:   c.Artifacts,
		Scanned:     c.Scanned,
		NotScanned:  c.NotScanned,
		Unsupported: c.Unsupported,
		Unavailable: c.Unavailable,
		Disabled:    c.Disabled,
		Missing:     c.Missing,
		Scannable:   c.Scannable(),
		Complete:    c.Complete(),
	}
}

func toAPIComponent(c security.Component) v1.SecurityComponent {
	return v1.SecurityComponent{
		ID: c.ID, Name: c.Name, Version: c.Version, Type: c.Type, Path: c.Path,
	}
}

func toAPIArtifact(a security.ArtifactRef) v1.SecurityArtifact {
	return v1.SecurityArtifact{
		Name:       a.ArtifactKey(),
		Tag:        a.Tag,
		Digest:     a.Digest,
		Repository: a.Repository,
		Kind:       a.Kind,
		MediaType:  a.MediaType,
		Platform:   a.Platform,
		Display:    a.Display(),
	}
}

func toAPIFinding(f security.Finding) v1.SecurityFinding {
	out := v1.SecurityFinding{
		CVE:           f.CVE,
		ID:            f.ID,
		Severity:      string(f.Severity),
		SeverityLabel: f.Severity.Label(),
		Summary:       f.Summary,
		Description:   f.Description,
		Component:     toAPIComponent(f.Component),
		FixedIn:       f.FixedIn,
		Fixable:       f.Fixable,
		CVSSScore:     f.CVSSScore,
		CVSSVector:    f.CVSSVector,
		References:    f.References,
		KEV:           f.KEV,
		KEVSource:     f.KEVSource,
		WillNotFix:    f.WillNotFix,
		Provider:      f.Provider,
		Policy:        f.Policy,
		Sources:       f.SourceSet(),
	}
	if f.EPSS != nil {
		out.EPSS = &v1.SecurityEPSS{Score: f.EPSS.Score, Percentile: f.EPSS.Percentile}
	}
	for _, o := range f.Observations {
		out.Observations = append(out.Observations, v1.SecurityObservation{
			Provider:      o.Provider,
			ProviderLabel: providerLabel(o.Provider),
			Source:        o.Source,
			Severity:      string(o.Severity),
			SeverityLabel: severityLabelOrEmpty(o.Severity),
			Score:         o.Score,
			Vector:        o.Vector,
		})
	}
	if f.Published != nil {
		out.Published = f.Published.UTC().Format(time.RFC3339)
	}
	return out
}

// severityLabelOrEmpty leaves an ungraded observation unlabelled.
//
// An observation that carries a CVSS score and no severity - which is every
// NVD entry in an Anchore response - would otherwise render as "Unknown", and a
// row reading "NVD: Unknown, 9.8" invites the reader to believe NVD had no
// opinion when it plainly did.
func severityLabelOrEmpty(s security.Severity) string {
	if s == "" {
		return ""
	}
	return s.Label()
}

func toAPIReport(r security.Report) v1.SecurityReport {
	status, label := string(r.Status), r.Status.Label()
	// "Not scanned" and "not in the repository at all" are one status in the
	// scanner's vocabulary and two different jobs for two different people, so
	// the interface is given the distinction the store keeps as a flag.
	if r.Status == security.StatusNotScanned && r.Missing {
		// "Not in registry", not "Not in JFrog". The fact is that the image is
		// not where the scanner that answered pulls from - which for Anchore
		// need not be JFrog at all, and naming the wrong system sends somebody
		// to look in a registry that was never involved. The report's own
		// message names the actual repository.
		status, label = v1.SecurityStatusNotFound, "Not in registry"
	}
	out := v1.SecurityReport{
		Artifact:    toAPIArtifact(r.Artifact),
		Status:      status,
		StatusLabel: label,
		Provider:    r.Provider,
		Message:     r.Message,
		Counts:      toAPICounts(r.Counts),
		FromCache:   r.FromCache,
	}
	for _, f := range r.Findings {
		out.Findings = append(out.Findings, toAPIFinding(f))
	}
	for _, f := range r.Malware {
		out.Malware = append(out.Malware, toAPIFinding(f))
	}
	for _, v := range r.Violations {
		out.Violations = append(out.Violations, toAPIViolation(v))
	}
	for _, d := range r.Documents {
		out.Documents = append(out.Documents, toAPIDocumentRef(d))
	}
	if r.ScannedAt != nil {
		out.ScannedAt = r.ScannedAt.UTC().Format(time.RFC3339)
	}
	if !r.RetrievedAt.IsZero() {
		out.RetrievedAt = r.RetrievedAt.UTC().Format(time.RFC3339)
	}
	return out
}

func toAPIViolation(v security.Violation) v1.SecurityViolation {
	out := v1.SecurityViolation{
		ID:            v.ID,
		Type:          v.Type,
		Severity:      string(v.Severity),
		SeverityLabel: v.Severity.Label(),
		Watch:         v.Watch,
		Policy:        v.Policy,
		Rule:          v.Rule,
		Summary:       v.Summary,
		Description:   v.Description,
		CVE:           v.CVE,
		Component:     toAPIComponent(v.Component),
		FixedIn:       v.FixedIn,
		Provider:      v.Provider,
	}
	if v.Created != nil {
		out.Created = v.Created.UTC().Format(rfc3339)
	}
	return out
}

func toAPIDocumentRef(d security.DocumentSummary) v1.SecurityDocumentRef {
	return v1.SecurityDocumentRef{
		Kind:        string(d.Kind),
		Label:       d.Kind.Label(),
		Provider:    d.Provider,
		Available:   d.Available,
		ContentType: d.ContentType,
		Bytes:       d.SourceBytes,
		FetchedAt:   d.FetchedAt,
		Message:     d.Message,
	}
}

// toAPISourceCounts renders one scanner's contribution.
func toAPISourceCounts(src security.SourceCounts) v1.SecuritySourceCounts {
	return v1.SecuritySourceCounts{
		Provider:   src.Provider,
		Label:      providerLabel(src.Provider),
		Counts:     toAPICounts(src.Counts),
		UniqueCVEs: src.UniqueCVEs,
		OnlyHere:   src.OnlyHere,
		Artifacts:  src.Artifacts,
		KEVs:       src.KEVs,
	}
}

// maxComparedCVEs bounds each list in a source comparison.
//
// # Why a cap rather than everything
//
// Because "only in Anchore" on a first cross-scanner sync is routinely four
// thousand advisories, and a page that ships four thousand identifiers to
// render a panel somebody glances at is a page that takes a second to open. The
// count is exact; the list is a sample somebody reads, and the export carries
// all of them.
const maxComparedCVEs = 200

// toAPISourceComparison renders the set arithmetic between scanners.
func toAPISourceComparison(cmp security.SourceComparison) *v1.SecuritySourceComparison {
	if len(cmp.Providers) < 2 {
		return nil
	}
	out := &v1.SecuritySourceComparison{
		Providers: cmp.Providers,
		Shared:    cmp.SharedCount,
	}
	out.SharedCVEs, out.Truncated = capList(cmp.Shared, out.Truncated)
	if len(cmp.OnlyIn) > 0 {
		out.OnlyIn = map[string][]string{}
		for provider, ids := range cmp.OnlyIn {
			out.OnlyIn[provider], out.Truncated = capList(ids, out.Truncated)
		}
	}
	if len(cmp.KEVOnlyIn) > 0 {
		out.KEVOnlyIn = map[string][]string{}
		for provider, ids := range cmp.KEVOnlyIn {
			// NOT capped. An exploited advisory only one scanner reported is
			// the whole reason to run two, and there are never four thousand
			// of them.
			out.KEVOnlyIn[provider] = ids
		}
	}
	return out
}

func capList(ids []string, truncated bool) ([]string, bool) {
	if len(ids) <= maxComparedCVEs {
		return ids, truncated
	}
	return ids[:maxComparedCVEs], true
}

// securityState is the one-word summary of whether a release's numbers can be
// trusted, and the sentence that goes with it.
//
// Five states, and the reason they are not three is the whole feature: a clean
// release and an unsynced one both have zero findings, and only this field
// tells them apart. `not_synced` is the common state in a fresh estate and gets
// a word of its own rather than being rounded to "clean".
func securityState(row store.PackageSecurityRow, target securityTarget) (state, message string) {
	switch {
	case !target.Available:
		return "disabled", target.Reason
	case row.State == store.PackageSecuritySyncing:
		return "syncing", "A vulnerability sync is running for this release."
	case row.State == store.PackageSecurityFailed && row.SyncedAt == nil:
		return "unavailable", failureMessage(row)
	case row.State == store.PackageSecurityFailed:
		return "stale", failureMessage(row) + " The numbers below are from the last successful sync."
	case row.State == store.PackageSecurityNever:
		return "not_synced",
			"This release has not been scanned for vulnerabilities. Run a sync to retrieve its results."
	case row.Coverage.Scannable() == 0:
		return "unavailable", "This release contains no artifacts the scanner can analyse."
	case !row.Coverage.Any():
		return "unavailable", "The scanner returned no results for any artifact in this release."
	case row.Coverage.Complete():
		return "ok", ""
	default:
		return "partial", coverageSentence(row.Coverage)
	}
}

// coverageSentence names each REASON an artifact has no result, separately.
//
// # Why they cannot be one number
//
// They were, and the page contradicted itself: a banner said "209 artifacts
// have no scan result", the panel under it said "1 artifact has not been
// scanned yet", and the scan-status card said "1 not scanned" while quietly
// omitting the 209. All three were reading the same coverage and summing
// different parts of it.
//
// They are different facts with different fixes. An artifact the scanner has
// never indexed will be there after the next sync too - somebody has to index
// it. An artifact the scanner would not ANSWER for is a scanner or a network
// problem, and syncing again may well fix it. Rolling them into one number
// tells a reader neither.
func coverageSentence(c security.Coverage) string {
	var parts []string
	if c.Missing > 0 {
		parts = append(parts, plural(c.Missing, "image is", "images are")+" not found in the repository")
	}
	if c.NotScanned > 0 {
		parts = append(parts, plural(c.NotScanned, "image has", "images have")+" not been scanned yet")
	}
	if c.Unavailable > 0 {
		parts = append(parts, plural(c.Unavailable, "image", "images")+" could not be retrieved from the scanner")
	}
	if c.Disabled > 0 {
		parts = append(parts, plural(c.Disabled, "image is", "images are")+" in a repository with no scanner")
	}
	if len(parts) == 0 {
		return "The totals below cover only the artifacts that were scanned."
	}

	return joinClauses(parts) + ". The totals below cover only the " +
		plural(c.Scanned, "image", "images") + " that were scanned."
}

// joinClauses reads a list the way a person says it.
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

func failureMessage(row store.PackageSecurityRow) string {
	if strings.TrimSpace(row.Error) != "" {
		return row.Error
	}
	return "The most recent vulnerability sync did not complete."
}

func plural(n int, singular, pluralWord string) string {
	if n == 1 {
		return "1 " + singular
	}
	return strconv.Itoa(n) + " " + pluralWord
}

func toAPIPackageSecurity(
	productName string, pkg store.PackageRow,
	row store.PackageSecurityRow, target securityTarget, detail bool,
	fresh security.Freshness,
) v1.PackageSecurityResponse {
	state, message := securityState(row, target)

	out := v1.PackageSecurityResponse{
		Product:         productName,
		Package:         packageReferenceOf(pkg),
		Provider:        providerOr(row.Provider, target),
		Enabled:         target.Available,
		Repository:      repositoryOr(row.Repository, target),
		State:           state,
		Message:         message,
		Counts:          toAPICounts(row.Counts),
		UniqueCounts:    toAPICounts(row.DistinctCounts),
		UniqueCVECounts: toAPICounts(row.UniqueCVECounts),
		DistinctTotal:   row.DistinctTotal,
		DistinctCVEs:    row.DistinctCVEs,
		Coverage:        toAPICoverage(row.Coverage),
		Fingerprint:     row.Fingerprint,
		Detail:          detail,
		Reports:         []v1.SecurityReport{},
	}
	if row.ScannedAt != nil {
		out.ScannedAt = row.ScannedAt.UTC().Format(rfc3339)
	}
	if row.SyncedAt != nil {
		out.SyncedAt = row.SyncedAt.UTC().Format(rfc3339)
	}
	out.Freshness = toAPIFreshness(fresh, row.SyncedAt)
	out.KEVs = row.KEVs
	out.KEVFixable = row.KEVFixable
	// Which scanners are CONFIGURED, not which the last sync used.
	//
	// The difference matters on the day somebody switches a second one on: the
	// stored row names one scanner and the release now has two, and a page
	// listing only what the last sync recorded would hide the very control that
	// fills the gap.
	out.Providers = target.Providers
	if len(out.Providers) == 0 && out.Provider != "" {
		out.Providers = []string{out.Provider}
	}
	out.KEVCapable = anyKEVCapable(out.Providers)
	for _, src := range row.Sources {
		out.Sources = append(out.Sources, toAPISourceCounts(src))
	}
	return out
}

// kevCapableProviders is the scanners that report a known-exploited catalogue.
//
// # Why this is a list rather than a field on the finding
//
// Because the question it answers is about ABSENCE. A finding says whether it
// is exploited; nothing on a set of findings says whether anybody checked, and
// "0 known-exploited vulnerabilities" is a very good result on a deployment
// running Anchore and a meaningless one on a deployment running only Xray -
// whose versions here carry no KEV feed. The interface needs to tell those
// apart, and only the list of who answered can.
//
// Named rather than probed, because a scanner that HAPPENED to report no KEVs
// in this release would otherwise look like one that cannot report them at all.
var kevCapableProviders = map[string]bool{"anchore": true}

func anyKEVCapable(providers []string) bool {
	for _, p := range providers {
		if kevCapableProviders[p] {
			return true
		}
	}
	return false
}

func providerOr(stored string, target securityTarget) string {
	if stored != "" {
		return stored
	}
	return target.Scope.Provider
}

func repositoryOr(stored string, target securityTarget) string {
	if stored != "" {
		return stored
	}
	return target.Scope.Repository
}

// changeTypeLabel is the change in the words the interface shows.
//
// Written out rather than title-cased from the enum, because "Removed artifact"
// is not what that change means: it means "this left with an artifact that is
// no longer shipped, and we are not calling that a fix".
func changeTypeLabel(t security.ChangeType) string {
	switch t {
	case security.ChangeIntroduced:
		return "Introduced"
	case security.ChangeResolved:
		return "Resolved"
	case security.ChangeUnchanged:
		return "Unchanged"
	case security.ChangeSeverityIncreased:
		return "More severe"
	case security.ChangeSeverityDecreased:
		return "Less severe"
	case security.ChangeRemediation:
		return "Fix availability changed"
	case security.ChangeRemovedArtifact:
		return "On a removed artifact"
	default:
		return string(t)
	}
}

func toAPIChange(c security.Change) v1.SecurityChange {
	return v1.SecurityChange{
		Type:           string(c.Type),
		TypeLabel:      changeTypeLabel(c.Type),
		CVE:            c.CVE,
		ID:             c.ID,
		Severity:       string(c.Severity),
		SeverityLabel:  c.Severity.Label(),
		FromSeverity:   string(c.FromSeverity),
		ToSeverity:     string(c.ToSeverity),
		Fixable:        c.Fixable,
		FixedIn:        c.FixedIn,
		KEV:            c.KEV,
		Sources:        c.Sources,
		Summary:        c.Summary,
		Description:    c.Description,
		Component:      toAPIComponent(c.Component),
		Artifact:       toAPIArtifact(c.Artifact),
		ArtifactChange: string(c.ArtifactChange),
		ViaRemoval:     c.ViaRemoval,
		Provider:       c.Provider,
	}
}

func toAPIArtifactDelta(d security.ArtifactDelta) v1.SecurityArtifactDelta {
	out := v1.SecurityArtifactDelta{
		Key:             d.Key,
		Change:          string(d.Change),
		StatusA:         string(d.StatusA),
		StatusB:         string(d.StatusB),
		CountsA:         toAPICounts(d.CountsA),
		CountsB:         toAPICounts(d.CountsB),
		Introduced:      d.Introduced,
		Resolved:        d.Resolved,
		Unchanged:       d.Unchanged,
		SeverityChanged: d.SeverityChanged,
		Comparable:      d.Comparable,
	}
	if d.A != nil {
		a := toAPIArtifact(*d.A)
		out.A = &a
	}
	if d.B != nil {
		b := toAPIArtifact(*d.B)
		out.B = &b
	}
	return out
}

func toAPIComparisonEnd(pkg store.PackageRow, side securitySide) v1.SecurityComparisonEnd {
	state, message := securityState(side.row, side.target)
	end := v1.SecurityComparisonEnd{
		Label:           releaseLabel(pkg),
		Package:         packageReferenceOf(pkg),
		Tag:             pkg.Tag,
		Digest:          pkg.ManifestDigest,
		Repository:      repositoryOr(side.row.Repository, side.target),
		Provider:        providerOr(side.row.Provider, side.target),
		Enabled:         side.target.Available,
		Counts:          toAPICounts(side.row.Counts),
		UniqueCVECounts: toAPICounts(side.row.UniqueCVECounts),
		Coverage:        toAPICoverage(side.row.Coverage),
		Sync: v1.SecuritySyncStatus{
			State:      string(orNever(side.row.State)),
			Label:      syncStateLabel(orNever(side.row.State)),
			Error:      side.row.Error,
			CanSync:    side.target.Available,
			Reason:     side.target.Reason,
			Repository: repositoryOr(side.row.Repository, side.target),
			Provider:   providerOr(side.row.Provider, side.target),
		},
	}
	_ = state
	_ = message
	if side.row.ScannedAt != nil {
		end.ScannedAt = side.row.ScannedAt.UTC().Format(rfc3339)
	}
	if side.row.SyncedAt != nil {
		end.Sync.SyncedAt = side.row.SyncedAt.UTC().Format(rfc3339)
	}
	return end
}

func orNever(state store.PackageSecurityState) store.PackageSecurityState {
	if state == "" {
		return store.PackageSecurityNever
	}
	return state
}

// shortenChanges trims a comparison to the rows worth sending to a page,
// leaving ChangesTotal to say how many there were.
//
// Applied by the page's handler and NOT by the export, which exists to be
// complete - see maxListedChanges for why the page is not.
func shortenChanges(out *v1.SecurityComparisonResponse) {
	if len(out.Changes) > maxListedChanges {
		out.Changes = out.Changes[:maxListedChanges]
	}
}

// maxListedChanges bounds how many classified findings a comparison RESPONSE
// enumerates. It does not bound the comparison, which is computed over all of
// them, nor the export, which writes all of them.
//
// # Why there is a bound at all
//
// Because two neighbouring releases of a large product are almost entirely
// identical, and "identical" is a row here. Comparing two 84,000-finding
// releases produced 85,715 classified findings of which 82,285 said nothing
// happened - 96% of a response whose only reader is a table that opens on a
// different tab. Sent in full it is a browser holding eighty-five thousand
// objects to show twenty-five of them, and a filter box that walks all of them
// on every keystroke.
//
// # Why a prefix is the right thing to drop
//
// Because security.Compare has already sorted the list by how much each row
// matters: severity increases, then what is new, then what left, then what was
// resolved, then remediation changes, and unchanged findings last - each group
// worst-severity first. A prefix is therefore exactly "the rows that matter
// most", and what falls off the end is the tail of what carried over
// identically. Every genuine change survives the cut unless there are more
// than #maxListedChanges of them, in which case the ones kept are the worst.
//
// The counts are unaffected: Introduced, Resolved, Unchanged and the rest are
// computed over everything and are what a client must count from. ChangesTotal
// says how many there were, so an interface can say the list is shortened
// rather than quietly showing a fraction as if it were the whole.
//
// Five thousand: more rows than anybody pages through at twenty-five a page,
// and small enough that the response stays a few megabytes.
const maxListedChanges = 5000

func toAPISecurityComparison(
	productName string, base, other store.PackageRow,
	sideA, sideB securitySide, c security.Comparison,
) v1.SecurityComparisonResponse {
	out := v1.SecurityComparisonResponse{
		Product:            productName,
		A:                  toAPIComparisonEnd(base, sideA),
		B:                  toAPIComparisonEnd(other, sideB),
		Verdict:            string(c.Verdict),
		VerdictLabel:       c.Verdict.Label(),
		Headline:           c.Headline,
		Explanation:        c.Explanation,
		Caveats:            c.Caveats,
		Introduced:         toAPICounts(c.Introduced),
		Resolved:           toAPICounts(c.Resolved),
		Unchanged:          toAPICounts(c.Unchanged),
		SeverityIncreased:  toAPICounts(c.SeverityIncreased),
		SeverityDecreased:  toAPICounts(c.SeverityDecreased),
		RemediationChanged: toAPICounts(c.RemediationChanged),
		RemovedArtifact:    toAPICounts(c.RemovedArtifact),
		NetScore:           c.NetScore,
		ArtifactSummary: v1.SecurityArtifactSummary{
			Common:        c.ArtifactSummary.Common,
			Upgraded:      c.ArtifactSummary.Upgraded,
			Added:         c.ArtifactSummary.Added,
			Removed:       c.ArtifactSummary.Removed,
			NotComparable: c.ArtifactSummary.NotComparable,
		},
		ChangesTotal: len(c.Changes),
		Artifacts:    make([]v1.SecurityArtifactDelta, 0, len(c.Artifacts)),
		RetrievedAt:  time.Now().UTC().Format(rfc3339),
		Fingerprint:  sideA.row.Fingerprint + "-" + sideB.row.Fingerprint,
	}
	out.Changes = make([]v1.SecurityChange, 0, len(c.Changes))
	for _, ch := range c.Changes {
		out.Changes = append(out.Changes, toAPIChange(ch))
	}
	for _, d := range c.Artifacts {
		out.Artifacts = append(out.Artifacts, toAPIArtifactDelta(d))
	}
	return out
}

func toAPISecuritySearch(
	productName, kind, query string, exact bool,
	hits []store.SearchHit, releases map[string][]store.ReleaseRef, truncated bool,
	kevOnly bool, provider string,
) v1.SecuritySearchResponse {
	out := v1.SecuritySearchResponse{
		Product:   productName,
		Kind:      kind,
		Query:     query,
		Exact:     exact,
		KEVOnly:   kevOnly,
		Provider:  provider,
		Truncated: truncated,
		Hits:      make([]v1.SecuritySearchHit, 0, len(hits)),
	}

	artifacts := map[string]bool{}
	releaseIDs := map[string]bool{}

	for _, h := range hits {
		sev := security.ParseSeverity(h.Severity)
		hit := v1.SecuritySearchHit{
			CVE:           h.CVE,
			IssueID:       h.IssueID,
			Severity:      h.Severity,
			SeverityLabel: sev.Label(),
			Fixable:       h.Fixable,
			KEV:           h.KEV,
			Summary:       h.Summary,
			Component: v1.SecurityComponent{
				ID: h.ComponentID, Name: h.ComponentName,
				Version: h.ComponentVersion, Type: h.ComponentType,
			},
			FixedIn: h.FixedIn,
			Artifact: v1.SecurityArtifact{
				Name: h.ArtifactKey, Tag: h.ArtifactTag, Digest: h.ArtifactRef,
				Repository: h.ArtifactRepo, Kind: h.ArtifactKind,
				Display: displayArtifact(h.ArtifactKey, h.ArtifactTag, h.ArtifactRef),
			},
			Provider:   h.Provider,
			Repository: h.Repository,
			ScannedAt:  h.ScannedAt,
		}
		artifacts[h.ArtifactRef] = true
		for _, rel := range releases[h.ArtifactRef] {
			id := strconv.FormatInt(rel.PackageID, 10)
			releaseIDs[id] = true
			hit.Releases = append(hit.Releases, v1.SecurityRelease{
				PackageID: id, Tag: rel.Tag, DisplayTag: rel.DisplayTag, Digest: rel.Digest,
			})
		}
		out.Hits = append(out.Hits, hit)
	}

	out.Searched = v1.SecuritySearchScope{
		Artifacts: len(artifacts),
		Releases:  len(releaseIDs),
		// Said out loud on every search, because the alternative sentence -
		// silence - reads as "this CVE is not in your estate", which is a
		// dangerous thing to say when what is true is "not in what has been
		// synced". The remedy is named, so a reader who finds nothing knows
		// what to do rather than concluding they are safe.
		Note: "Search covers releases whose vulnerabilities have been synced. " +
			"Run a sync on a release to include it.",
	}
	return out
}

func displayArtifact(name, tag, digest string) string {
	switch {
	case name != "" && tag != "":
		return name + ":" + tag
	case name != "":
		return name
	default:
		return shortDigest(digest)
	}
}

// packageReferenceOf is how a release is addressed in a URL: its tag where it
// has one, its digest otherwise.
func packageReferenceOf(pkg store.PackageRow) string {
	if pkg.Tag != "" {
		return pkg.Tag
	}
	return pkg.ManifestDigest
}

// toAPIFreshness states the deployment's rule and where this release sits
// against it.
//
// Measured from the SYNC rather than from the scan. They are different facts -
// the scanner's result can be older than our retrieval of it - and the one a
// reader can act on is ours: "we last asked five days ago" has a button under
// it, and "Xray graded this eleven days ago" does not.
func toAPIFreshness(f security.Freshness, syncedAt *time.Time) v1.SecurityFreshness {
	out := v1.SecurityFreshness{
		MaxAgeSeconds:     int(f.Vulnerabilities / time.Second),
		SBOMMaxAgeSeconds: int(f.SBOM / time.Second),
	}
	if syncedAt == nil {
		return out
	}
	if at := f.StaleAt(syncedAt.UTC(), security.DocumentVulnerabilities); !at.IsZero() {
		out.StaleAt = at.Format(rfc3339)
		out.Stale = time.Now().UTC().After(at)
	}
	return out
}
