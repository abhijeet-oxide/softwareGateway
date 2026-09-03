package security

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Asking every scanner, and answering as one.
//
// # Why the fan-out is here rather than inside a composite Provider
//
// A composite provider - one Provider that calls two and returns the union -
// was the obvious shape and it is the wrong one, for a storage reason rather
// than a stylistic one. Scope.Provider is part of every storage key: the scan
// row, the detail payload, the raw document. A composite would have to write
// under one name, which means Anchore's vulnerability body and Xray's would
// collide on the same (scope, artifact, kind) row, only one of them would be
// downloadable, and "what did Xray alone say" would stop being answerable the
// day the second scanner was switched on.
//
// So each scanner is asked in its own scope, stores in its own rows, and the
// union happens above the storage - on every read, from rows that are still
// separately readable. The cost is a merge per read. What it buys is that
// switching a scanner off removes its contribution without a migration, and
// that a vendor can still be handed the exact bytes their scanner produced.

// MultiRequest is one release's security read, across every configured scanner.
type MultiRequest struct {
	// Request carries everything a single-provider read needs. Its Scope's
	// Provider field is IGNORED - the providers below say who is asked, and
	// each gets a scope of its own.
	Request
	// Providers names the scanners to ask, in the order to ask them.
	//
	// Empty means none are configured, which is answered without any I/O and
	// with a report per artifact saying so - not with an empty finding list,
	// which is what a release with no scanner would otherwise look like.
	Providers []string
}

// MultiResult is what every scanner said, merged and separately.
type MultiResult struct {
	// Posture is the merged view: one report per artifact, each finding
	// carrying every scanner that reported it.
	Posture Posture
	// Sources is each scanner's own answer, untouched by the merge.
	//
	// Kept because "what does Anchore alone say" cannot be answered by
	// filtering the merged rows: a merged row carries the WORST severity of
	// the sources that reported it, so a filtered view would show Anchore's
	// rows wearing Xray's grade - which is exactly the disagreement somebody
	// opened the source view to see.
	Sources []SourceReports
	// Comparison is the set arithmetic between the scanners: what each found
	// that the others did not, and which of those are known-exploited.
	//
	// Zero-valued on a single-scanner deployment, where there is nothing to
	// compare and every finding is trivially unique to the only answerer.
	Comparison SourceComparison

	Fingerprint string
	RetrievedAt time.Time

	// FromCache and Fetched are summed across scanners.
	FromCache int
	Fetched   int

	// Failures maps a scanner to why it could not be asked at all - a
	// credential the scanner rejected outright, a resolver that could not build
	// it. Per-artifact failures are not here; they are unavailable reports.
	//
	// A map rather than an error, because one scanner refusing must not lose
	// the other's answer. That is the same rule the provider boundary applies
	// per artifact, applied one level up.
	Failures map[string]error

	// enabled is whether ANY scanner was switched on for this repository.
	//
	// Not derivable from the reports: a scanner that is switched off answers
	// with a report per artifact saying so, so "there are reports" is true
	// either way. The distinction is what stops a release whose scanners are
	// all off being recorded as a clean scan.
	enabled bool
}

// Enabled reports whether any scanner is switched on for this repository.
func (r MultiResult) Enabled() bool { return r.enabled }

// Providers names the scanners that contributed, sorted.
func (r MultiResult) ProviderNames() []string {
	out := make([]string, 0, len(r.Sources))
	for _, s := range r.Sources {
		out = append(out, s.Provider)
	}
	sort.Strings(out)
	return out
}

// Postures retrieves one release's security state from every configured
// scanner, concurrently, and merges it.
//
// # Concurrently, and what that costs
//
// The scanners are independent - different hosts, different credentials,
// different rate limits - and asking them in sequence would make a release's
// sync the SUM of their times rather than the longest of them. On a first sync
// that is the difference between ten minutes and twenty, because Anchore's
// phase is dominated by waiting for analysis that is happening whether we watch
// it or not.
//
// The progress reporter is shared, which is deliberate: a reader watching a
// sync wants one transcript of what is happening, not two interleaved ones
// they have to demultiplex. Each provider names itself in its own lines.
func (s *Service) Postures(ctx context.Context, req MultiRequest) (MultiResult, error) {
	out := MultiResult{RetrievedAt: time.Now().UTC(), Failures: map[string]error{}}

	providers := req.Providers
	if len(providers) == 0 {
		// No scanner is configured. Answered with a disabled report per
		// artifact rather than an empty list, because an empty list is what a
		// clean release looks like.
		res, err := s.Posture(ctx, req.Request)
		if err != nil {
			return out, err
		}
		out.Posture = res.Posture
		out.Fingerprint = res.Fingerprint
		out.enabled = res.Enabled
		if res.Enabled && len(res.Posture.Reports) > 0 {
			out.Sources = []SourceReports{{
				Provider: res.Provider, Reports: res.Posture.Reports,
				Posture: res.Posture, RetrievedAt: res.RetrievedAt,
			}}
		}
		return out, nil
	}

	type answer struct {
		provider string
		result   Result
		err      error
	}
	answers := make([]answer, len(providers))

	var wg sync.WaitGroup
	for i, provider := range providers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			one := req.Request
			one.Scope.Provider = provider
			res, err := s.Posture(ctx, one)
			answers[i] = answer{provider: provider, result: res, err: err}
		}()
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return out, err
	}

	lists := make([][]Report, 0, len(answers))
	for _, a := range answers {
		if a.err != nil {
			// One scanner refusing must not lose the other's answer. Recorded
			// and reported; the caller decides whether a release with one of
			// two scanners is a success (it is) or none (it is not).
			out.Failures[a.provider] = a.err
			s.log.Warn("security: a scanner could not be asked",
				"provider", a.provider, "product", req.Scope.Product,
				"repository", req.Scope.Repository, "error", a.err)
			continue
		}
		out.FromCache += a.result.FromCache
		out.Fetched += a.result.Fetched
		out.enabled = out.enabled || a.result.Enabled
		reports := a.result.Posture.Reports
		// A scanner that is switched OFF still answers, with a disabled report
		// per artifact - and those must not join the merge. Merged in, they
		// would contribute a "disabled" status to every artifact the OTHER
		// scanner answered for, and the merge's best-status rule would then
		// have to defend against its own inputs.
		if len(reports) == 0 || !a.result.Enabled {
			continue
		}
		lists = append(lists, reports)
		out.Sources = append(out.Sources, SourceReports{
			Provider:    a.provider,
			Reports:     reports,
			Posture:     a.result.Posture,
			RetrievedAt: a.result.RetrievedAt,
		})
	}

	// Nothing was switched on. The disabled reports from the first scanner
	// asked are the honest answer - "Anchore is not enabled for this
	// repository", per artifact - and they are what the caller renders rather
	// than an empty list, which is what a clean release looks like.
	if len(lists) == 0 {
		for _, a := range answers {
			if a.err == nil && len(a.result.Posture.Reports) > 0 {
				out.Posture = a.result.Posture
				out.Fingerprint = a.result.Fingerprint
				break
			}
		}
		return out, nil
	}

	merged := MergeReports(lists...)
	out.Posture = Summarize(merged)
	out.Fingerprint = FingerprintReports(merged)
	if len(out.Sources) > 1 {
		out.Comparison = CompareSources(out.Sources)
		// The per-source KEV counts come from the comparison rather than from
		// each posture, because "advisories only this scanner reported" is the
		// number that needs every scanner's answer in one place - and having
		// walked them once for that, walking them again for the rest would be
		// a second pass over a release's findings.
		applyComparison(&out.Posture, out.Comparison)
	}
	return out, nil
}

// applyComparison writes the cross-scanner numbers onto the merged posture.
//
// Summarize computes BySource from the merged findings' Sources lists, which is
// correct and cannot see a scanner that answered for an artifact and found
// nothing in it. CompareSources can, because it walks each scanner's own
// reports - so where the two disagree, the comparison wins.
func applyComparison(p *Posture, cmp SourceComparison) {
	if len(cmp.Counts) == 0 {
		return
	}
	byProvider := make(map[string]int, len(p.BySource))
	for i, sc := range p.BySource {
		byProvider[sc.Provider] = i
	}
	for provider, agreement := range cmp.Counts {
		i, ok := byProvider[provider]
		if !ok {
			p.BySource = append(p.BySource, SourceCounts{Provider: provider})
			i = len(p.BySource) - 1
			byProvider[provider] = i
		}
		p.BySource[i].UniqueCVEs = agreement.Total
		p.BySource[i].OnlyHere = agreement.Only
		p.BySource[i].KEVs = agreement.KEVOnly + sharedKEVs(p.BySource[i].KEVs, agreement)
	}
	sort.Slice(p.BySource, func(i, j int) bool {
		return p.BySource[i].Provider < p.BySource[j].Provider
	})
}

// sharedKEVs keeps whichever KEV count is larger.
//
// Summarize's figure counts exploited advisories this scanner reported,
// including ones others reported too; the comparison's KEVOnly counts only the
// exclusive ones. The first is the honest answer to "how many KEVs did this
// scanner find", so it wins where it is larger - and the max is what makes this
// safe to apply whichever way the two were computed.
func sharedKEVs(summarized int, agreement SourceAgreement) int {
	if summarized > agreement.KEVOnly {
		return summarized - agreement.KEVOnly
	}
	return 0
}

// ProvidersFor names the scanners configured for one repository.
func (s *Service) ProvidersFor(ctx context.Context, product, repository string) ([]string, error) {
	if s.resolver == nil {
		return nil, nil
	}
	return s.resolver.ProvidersFor(ctx, product, repository)
}

// ProviderLabel names a scanner in the words the interface shows.
//
// Here rather than in the interface, so a transcript line, an export column and
// a badge cannot disagree about what a scanner is called. A name this build
// does not know is returned as it was stored, which is the honest answer for a
// provider added after this Coordinator was built.
func ProviderLabel(provider string) string {
	switch provider {
	case "jfrog-xray":
		return "JFrog Xray"
	case "anchore":
		return "Anchore"
	case "":
		return "the configured scanner"
	default:
		return provider
	}
}

// DescribeAll names several scanners as a sentence: "JFrog Xray and Anchore".
func DescribeAll(providers []string) string {
	names := make([]string, 0, len(providers))
	for _, p := range providers {
		names = append(names, ProviderLabel(p))
	}
	switch len(names) {
	case 0:
		return "no scanner"
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	default:
		return fmt.Sprintf("%s and %s",
			joinAll(names[:len(names)-1]), names[len(names)-1])
	}
}

func joinAll(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}
