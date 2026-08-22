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
		BySeverity:        toAPISeverityCounts(c.BySeverity),
		FixableBySeverity: toAPISeverityCounts(c.FixableBySeverity),
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
		Provider:      f.Provider,
		Policy:        f.Policy,
	}
	if f.Published != nil {
		out.Published = f.Published.UTC().Format(time.RFC3339)
	}
	return out
}

func toAPIReport(r security.Report) v1.SecurityReport {
	out := v1.SecurityReport{
		Artifact:    toAPIArtifact(r.Artifact),
		Status:      string(r.Status),
		StatusLabel: r.Status.Label(),
		Provider:    r.Provider,
		Message:     r.Message,
		Counts:      toAPICounts(r.Counts),
		FromCache:   r.FromCache,
	}
	for _, f := range r.Findings {
		out.Findings = append(out.Findings, toAPIFinding(f))
	}
	if r.ScannedAt != nil {
		out.ScannedAt = r.ScannedAt.UTC().Format(time.RFC3339)
	}
	if !r.RetrievedAt.IsZero() {
		out.RetrievedAt = r.RetrievedAt.UTC().Format(time.RFC3339)
	}
	return out
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
			"This release has not been scanned for vulnerabilities yet. Run a sync to find out what is in it."
	case row.Coverage.Scannable() == 0:
		return "unavailable", "There is nothing in this release that the scanner can look at."
	case !row.Coverage.Any():
		return "unavailable", "The scanner returned no results for any artifact in this release."
	case row.Coverage.Complete():
		return "ok", ""
	default:
		missing := row.Coverage.NotScanned + row.Coverage.Unavailable + row.Coverage.Disabled
		return "partial", plural(missing, "artifact", "artifacts") +
			" in this release have no scan result, so these numbers cover only what was scanned."
	}
}

func failureMessage(row store.PackageSecurityRow) string {
	if strings.TrimSpace(row.Error) != "" {
		return row.Error
	}
	return "The last vulnerability sync did not finish."
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
) v1.PackageSecurityResponse {
	state, message := securityState(row, target)

	out := v1.PackageSecurityResponse{
		Product:       productName,
		Package:       packageReferenceOf(pkg),
		Provider:      providerOr(row.Provider, target),
		Enabled:       target.Available,
		Repository:    repositoryOr(row.Repository, target),
		State:         state,
		Message:       message,
		Counts:        toAPICounts(row.Counts),
		UniqueCounts:  toAPICounts(distinctCounts(row)),
		DistinctTotal: row.DistinctTotal,
		Coverage:      toAPICoverage(row.Coverage),
		Fingerprint:   row.Fingerprint,
		Detail:        detail,
		Reports:       []v1.SecurityReport{},
	}
	if row.ScannedAt != nil {
		out.ScannedAt = row.ScannedAt.UTC().Format(rfc3339)
	}
	if row.SyncedAt != nil {
		out.SyncedAt = row.SyncedAt.UTC().Format(rfc3339)
	}
	if out.Provider != "" {
		out.Providers = []string{out.Provider}
	}
	return out
}

// distinctCounts renders the distinct total in the counts shape the interface
// already knows.
//
// Only the total is stored: a per-severity breakdown of DISTINCT findings would
// be a second set of five columns answering a question nobody has asked, and
// the severity breakdown people do want is the per-artifact one above.
func distinctCounts(row store.PackageSecurityRow) security.Counts {
	return security.Counts{Total: row.DistinctTotal}
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
		Label:      releaseLabel(pkg),
		Package:    packageReferenceOf(pkg),
		Tag:        pkg.Tag,
		Digest:     pkg.ManifestDigest,
		Repository: repositoryOr(side.row.Repository, side.target),
		Provider:   providerOr(side.row.Provider, side.target),
		Enabled:    side.target.Available,
		Counts:     toAPICounts(side.row.Counts),
		Coverage:   toAPICoverage(side.row.Coverage),
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
		Changes:     make([]v1.SecurityChange, 0, len(c.Changes)),
		Artifacts:   make([]v1.SecurityArtifactDelta, 0, len(c.Artifacts)),
		RetrievedAt: time.Now().UTC().Format(rfc3339),
		Fingerprint: sideA.row.Fingerprint + "-" + sideB.row.Fingerprint,
	}
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
) v1.SecuritySearchResponse {
	out := v1.SecuritySearchResponse{
		Product:   productName,
		Kind:      kind,
		Query:     query,
		Exact:     exact,
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
