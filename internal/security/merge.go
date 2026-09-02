package security

import (
	"sort"
	"time"
)

// Merging what several scanners said about one release.
//
// # The problem, in one sentence
//
// Xray and Anchore both look at the same image and hand back two lists that
// overlap without agreeing, and a page that shows them one after the other
// makes the reader do the join.
//
// # Why the join is done here and not in the store
//
// Because it is a judgement, not a query. "The same finding" is a decision
// about identity (§ MergeKey), "the severity to show" is a decision about which
// source to believe, and "reported by" is a decision about what to keep when
// two rows become one. Those belong in the vocabulary the whole platform
// reasons in, beside the model they are about - not in SQL, where a second
// scanner would mean a second query nobody can test, and not in the browser,
// where every consumer would implement it slightly differently.
//
// # Why the sources stay separately stored
//
// Merging is done on READ, over rows each scanner wrote into its own scope
// (Scope.Provider is part of every storage key). Nothing overwrites anything:
// Anchore's raw body and Xray's raw body are both downloadable, "what did Xray
// alone say" is still answerable a month later, and switching a scanner off
// removes its contribution without a migration. A merged row written at sync
// time would have destroyed all three of those to save one join.

// MergeReports combines several scanners' reports into one set, one report per
// artifact, with each finding carrying every source that reported it.
//
// The input is one slice per provider - what each of them said about the same
// release. Order does not matter: the result is deterministic because the
// merge is commutative on everything it keeps (worst severity, union of facts)
// and because the output is sorted.
func MergeReports(bySource ...[]Report) []Report {
	// The common case by far, and it must cost nothing: one scanner's reports
	// are already the answer.
	nonEmpty := make([][]Report, 0, len(bySource))
	for _, list := range bySource {
		if len(list) > 0 {
			nonEmpty = append(nonEmpty, list)
		}
	}
	switch len(nonEmpty) {
	case 0:
		return nil
	case 1:
		return nonEmpty[0]
	}

	order := []string{}
	merged := map[string]*Report{}
	for _, list := range nonEmpty {
		for _, r := range list {
			key := r.Artifact.Ref()
			existing, ok := merged[key]
			if !ok {
				clone := r
				clone.Findings = append([]Finding(nil), r.Findings...)
				clone.Malware = append([]Finding(nil), r.Malware...)
				clone.Violations = append([]Violation(nil), r.Violations...)
				clone.Documents = append([]DocumentSummary(nil), r.Documents...)
				stampSource(&clone)
				merged[key] = &clone
				order = append(order, key)
				continue
			}
			mergeInto(existing, r)
		}
	}

	out := make([]Report, 0, len(order))
	for _, key := range order {
		r := merged[key]
		r.Findings = MergeFindings(r.Findings)
		r.Malware = MergeFindings(r.Malware)
		r.Recount()
		r.Sort()
		out = append(out, *r)
	}
	sortReports(out)
	return out
}

// stampSource makes sure every finding on a report knows which scanner
// produced it, before it can be merged with somebody else's.
//
// A provider that already filled Sources is left alone. One that did not - and
// none of them need to, because a single-source provider stating its own name
// twice per finding is noise - gets the report's provider written onto each
// row, because after the merge the report no longer has one.
func stampSource(r *Report) {
	for i := range r.Findings {
		stampFinding(&r.Findings[i], r.Provider)
	}
	for i := range r.Malware {
		stampFinding(&r.Malware[i], r.Provider)
	}
}

func stampFinding(f *Finding, provider string) {
	if f.Provider == "" {
		f.Provider = provider
	}
	if len(f.Sources) == 0 && f.Provider != "" {
		f.Sources = []string{f.Provider}
	}
}

// mergeInto folds one scanner's report about an artifact into the running
// merge of the others'.
//
// # Status, and the rule that matters most here
//
// The BEST status wins, and only a scanned one contributes findings. Two
// scanners looking at one image routinely disagree about whether they have
// looked: Anchore has analysed it and Xray has not indexed it yet, or the other
// way round. Taking the worse status would report an image nobody scanned when
// one scanner has a complete answer sitting in the response; taking the better
// one silently would lose the fact that the other scanner has nothing to say.
//
// So: the status is the best any scanner reached, the findings are the union of
// what the scanners that DID reach it found, and Sources on the report says who
// answered - which is what lets the interface say "Anchore scanned this; Xray
// has not indexed it yet" instead of picking one of those to be true.
func mergeInto(dst *Report, src Report) {
	other := src
	stampSource(&other)

	dst.Findings = append(dst.Findings, other.Findings...)
	dst.Malware = append(dst.Malware, other.Malware...)
	dst.Violations = append(dst.Violations, other.Violations...)
	dst.Documents = append(dst.Documents, other.Documents...)

	if statusRank(other.Status) > statusRank(dst.Status) {
		dst.Status = other.Status
		dst.Message = other.Message
		dst.Provider = other.Provider
	} else if dst.Message == "" {
		dst.Message = other.Message
	}
	// Missing means "not in the repository the scanner answers for", and two
	// scanners answer for two different places. It is only still true of the
	// release when EVERY scanner said it.
	dst.Missing = dst.Missing && other.Missing

	// The OLDEST scan time, for the same reason a posture reports the oldest:
	// it is the age of the weakest link, and quoting the fresher of two makes a
	// stale half of the answer look current.
	if other.ScannedAt != nil && (dst.ScannedAt == nil || other.ScannedAt.Before(*dst.ScannedAt)) {
		t := *other.ScannedAt
		dst.ScannedAt = &t
	}
	if other.RetrievedAt.Before(dst.RetrievedAt) || dst.RetrievedAt.IsZero() {
		dst.RetrievedAt = other.RetrievedAt
	}
	dst.FromCache = dst.FromCache && other.FromCache
}

// statusRank orders statuses by how much they tell us, best first.
//
// Scanned outranks everything because it is the only one carrying an answer.
// Below it, "the scanner cannot scan this kind of thing" is a settled fact and
// outranks "it has not got round to it", which outranks "it would not answer",
// which outranks "it is switched off".
func statusRank(s Status) int {
	switch s {
	case StatusScanned:
		return 5
	case StatusUnsupported:
		return 4
	case StatusNotScanned:
		return 3
	case StatusUnavailable:
		return 2
	case StatusDisabled:
		return 1
	default:
		return 0
	}
}

// MergeFindings collapses findings that several scanners reported as the same
// thing, unioning what each of them knew.
//
// # The identity, and why it is not the CVE
//
// The Anchore integration guide is explicit about this and it is right: a
// single CVE affects several packages in one image, and each occurrence is its
// own remediation. Collapsing on the advisory alone would turn "openssl and
// libssl3 both need upgrading" into one row and lose one of the two upgrades.
//
// So the key is StorageKey - (identifier, component, component version) - the
// same identity the store uses for a row. Two scanners naming the same package
// at the same version for the same advisory are reporting one thing, and
// anything else is two.
//
// # What this cannot fix
//
// Scanners that spell a package differently produce two rows rather than one.
// That is under-merging, and it is the safe direction to fail in: an
// unmerged pair shows a reader two rows and lets them see the disagreement,
// where an over-merged pair silently attributes one scanner's fix version to
// another scanner's package. Providers normalize component identity into the
// same vocabulary (Component.ID) precisely so this stays rare.
func MergeFindings(in []Finding) []Finding {
	if len(in) < 2 {
		return in
	}
	at := make(map[string]int, len(in))
	out := make([]Finding, 0, len(in))
	for _, f := range in {
		k := f.StorageKey()
		i, seen := at[k]
		if !seen {
			at[k] = len(out)
			out = append(out, f)
			continue
		}
		out[i] = enrich(out[i], f)
	}
	for i := range out {
		sort.Strings(out[i].Sources)
	}
	return out
}

// SourceReports is one scanner's answer for a release, kept whole.
//
// # Why the unmerged answers survive the merge
//
// Because "what does Anchore alone say" is a question the interface has to
// answer, and answering it by filtering the merged rows is not the same thing.
// A merged row carries the WORST severity of the sources that reported it, so
// filtering to Anchore would show Anchore's rows wearing Xray's grade - which
// is precisely the disagreement somebody opened the source view to see.
type SourceReports struct {
	// Provider is the scanner - "jfrog-xray", "anchore".
	Provider string `json:"provider"`
	// Reports is what that scanner said, untouched by the merge.
	Reports []Report `json:"reports"`
	// Posture is that scanner's own arithmetic over its own reports.
	Posture Posture `json:"posture"`
	// RetrievedAt is when this scanner was last asked.
	RetrievedAt time.Time `json:"retrievedAt"`
}

// SourceComparison is what two scanners said about one release, as a set.
//
// # Why this is a type and not three filters in the browser
//
// Because the numbers have to agree with the export, the summary line and the
// stored per-source rows, and four implementations of "only in Anchore" is four
// chances for them not to. It is also the answer to the question the second
// scanner exists to make askable - did it find anything the first one missed -
// and that answer should not depend on which page you are on.
type SourceComparison struct {
	// Providers is every scanner that answered, sorted.
	Providers []string `json:"providers"`
	// Shared is the advisories every scanner that answered reported.
	Shared []string `json:"shared"`
	// OnlyIn maps a scanner to the advisories only it reported.
	OnlyIn map[string][]string `json:"onlyIn"`
	// Counts is the same, as numbers, for a summary that draws no lists.
	Counts map[string]SourceAgreement `json:"counts"`
	// SharedCount is how many advisories every scanner agreed on.
	SharedCount int `json:"sharedCount"`
	// KEVOnlyIn maps a scanner to the KNOWN-EXPLOITED advisories only it
	// reported.
	//
	// Its own field because it is the finding that decides whether a scanner
	// stays switched on. Two thousand unique lows is a difference in feed
	// coverage; one unique KEV is a vulnerability that was being exploited and
	// that the other scanner did not mention.
	KEVOnlyIn map[string][]string `json:"kevOnlyIn"`
}

// SourceAgreement is one scanner's position in a comparison.
type SourceAgreement struct {
	// Total is every advisory this scanner reported.
	Total int `json:"total"`
	// Only is the advisories no other scanner reported.
	Only int `json:"only"`
	// KEVOnly is how many of those are known-exploited.
	KEVOnly int `json:"kevOnly"`
	// Enriched is advisories this scanner reported that another also did, and
	// where this scanner supplied a fact the other did not - a fix version, a
	// description, a CVSS vector, a KEV flag.
	//
	// It is the answer to "what did the second scanner add, apart from rows",
	// and it is the honest defence of a scanner whose Only count is zero.
	Enriched int `json:"enriched"`
}

// CompareSources classifies what each scanner reported against the others.
//
// Takes the per-source reports rather than the merged ones, because the whole
// question is who said what and a merged row has already forgotten.
func CompareSources(sources []SourceReports) SourceComparison {
	out := SourceComparison{
		OnlyIn:    map[string][]string{},
		KEVOnlyIn: map[string][]string{},
		Counts:    map[string]SourceAgreement{},
	}
	if len(sources) == 0 {
		return out
	}

	// Which scanners saw each advisory, and what each of them knew about it.
	sawBy := map[string]map[string]bool{}
	kev := map[string]bool{}
	// facts[id][provider] is what that provider supplied for that advisory, so
	// "who added something the other lacked" is answerable without a re-walk.
	facts := map[string]map[string]factSet{}

	for _, src := range sources {
		out.Providers = append(out.Providers, src.Provider)
		agreement := out.Counts[src.Provider]
		seen := map[string]bool{}
		for _, r := range src.Reports {
			if r.Status != StatusScanned {
				continue
			}
			for _, f := range r.Findings {
				id := f.Identifier()
				if id == "" {
					continue
				}
				if !seen[id] {
					seen[id] = true
					agreement.Total++
				}
				if sawBy[id] == nil {
					sawBy[id] = map[string]bool{}
				}
				sawBy[id][src.Provider] = true
				if f.KEV {
					kev[id] = true
				}
				if facts[id] == nil {
					facts[id] = map[string]factSet{}
				}
				facts[id][src.Provider] = facts[id][src.Provider].plus(f)
			}
		}
		out.Counts[src.Provider] = agreement
	}
	sort.Strings(out.Providers)

	answered := len(out.Providers)
	for id, srcs := range sawBy {
		if len(srcs) == answered && answered > 1 {
			out.Shared = append(out.Shared, id)
			out.SharedCount++
		}
		if len(srcs) == 1 {
			for provider := range srcs {
				out.OnlyIn[provider] = append(out.OnlyIn[provider], id)
				agreement := out.Counts[provider]
				agreement.Only++
				if kev[id] {
					out.KEVOnlyIn[provider] = append(out.KEVOnlyIn[provider], id)
					agreement.KEVOnly++
				}
				out.Counts[provider] = agreement
			}
			continue
		}
		// Reported by more than one. Which of them brought something the
		// others did not?
		for provider, mine := range facts[id] {
			added := false
			for other, theirs := range facts[id] {
				if other == provider {
					continue
				}
				if mine.adds(theirs) {
					added = true
					break
				}
			}
			if added {
				agreement := out.Counts[provider]
				agreement.Enriched++
				out.Counts[provider] = agreement
			}
		}
	}

	sort.Strings(out.Shared)
	for k := range out.OnlyIn {
		sort.Strings(out.OnlyIn[k])
	}
	for k := range out.KEVOnlyIn {
		sort.Strings(out.KEVOnlyIn[k])
	}
	return out
}

// factSet is which facts one scanner supplied about one advisory.
//
// Booleans rather than the values, because the question is "did this source
// know something the other did not", and comparing the values would call two
// different phrasings of one description an enrichment.
type factSet struct {
	fix, prose, vector, kev, epss bool
}

func (s factSet) plus(f Finding) factSet {
	return factSet{
		fix:    s.fix || len(f.FixedIn) > 0,
		prose:  s.prose || f.Description != "",
		vector: s.vector || f.CVSSVector != "",
		kev:    s.kev || f.KEV,
		epss:   s.epss || f.EPSS != nil,
	}
}

// adds reports whether s knows something other does not.
func (s factSet) adds(other factSet) bool {
	return (s.fix && !other.fix) ||
		(s.prose && !other.prose) ||
		(s.vector && !other.vector) ||
		(s.kev && !other.kev) ||
		(s.epss && !other.epss)
}
