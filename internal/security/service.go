package security

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Scope is the authorization boundary a cached result belongs to.
//
// Every cache read and every cache write is keyed by it, and that is a security
// property rather than a filing convention. Findings are retrieved with one
// repository's credential, under that repository's Xray permissions; serving
// them to a request scoped to a different product, repository or provider would
// disclose the security posture of something the asker was never entitled to
// see. Two artifacts with the same digest in two products are two cache rows,
// deliberately, even though the bytes are identical.
type Scope struct {
	Product string
	// Repository is the CONFIGURED repository name - "vendor-jfrog" - not the
	// registry path. Which credential answered is what the boundary is drawn
	// around.
	Repository string
	Role       string
	Provider   string
}

// Key renders the scope as a stable cache key component.
func (s Scope) Key() string {
	return s.Product + "|" + s.Role + "|" + s.Repository + "|" + s.Provider
}

// Cache is the platform's storage for security results.
//
// Two tiers, and they are not the same data with different timeouts:
//
//   - The SUMMARY tier is lightweight and kept long: status, counts by
//     severity, fixability, scan time, and the normalized identifiers needed to
//     search. It is what a package listing, a dashboard and a quick comparison
//     read, and it is cheap enough to keep for every artifact of every release.
//   - The DETAIL tier is the complete normalized response and is kept briefly.
//     It exists so that reopening a finding, repeating a comparison or
//     generating an export does not re-query Xray - not so the platform can
//     answer without Xray at all.
//
// The platform is not a system of record for security findings. Xray is, and
// the detail tier's short retention is what keeps that true.
type Cache interface {
	// LoadSummaries returns cached summary rows for the given artifacts, keyed
	// by ArtifactRef.Ref(). Expired rows are not returned.
	LoadSummaries(ctx context.Context, scope Scope, refs []ArtifactRef) (map[string]Report, error)
	// LoadDetails returns cached COMPLETE reports, keyed by ArtifactRef.Ref().
	LoadDetails(ctx context.Context, scope Scope, refs []ArtifactRef) (map[string]Report, error)
	// Save records both tiers for one retrieval. detail says whether the
	// reports carry findings; a counts-only retrieval writes the summary tier
	// only, and must not overwrite a detail row with an empty one.
	Save(ctx context.Context, scope Scope, reports []Report, detail bool, ttl CacheTTL) error
	// Invalidate drops every tier for the given artifacts, for an explicit
	// refresh.
	Invalidate(ctx context.Context, scope Scope, refs []ArtifactRef) error
}

// CacheTTL is how long each tier is good for.
type CacheTTL struct {
	Summary time.Duration
	Detail  time.Duration
}

// Request is one release's security read.
type Request struct {
	Scope Scope
	// Artifacts is what to ask about. The caller assembles these from the
	// release's own artifact tree, because what a release CONTAINS is the
	// core's knowledge and not the provider's.
	Artifacts []ArtifactRef
	// Detail asks for full findings rather than counts.
	Detail bool
	// Refresh bypasses the cache and re-queries the provider. Only a person
	// pressing refresh should set it.
	Refresh bool
	// TTL governs what is written back.
	TTL CacheTTL
	// Progress reports what is happening, and may be nil.
	Progress Progress
}

// Result is a release's security state and how it was obtained.
//
// The provenance fields are not decoration. A user looking at a number needs to
// know whether it was measured a minute ago or served from a cache filled
// yesterday, and an interface that cannot say loses the ability to offer a
// meaningful refresh button.
type Result struct {
	Posture Posture
	// Fingerprint is a stable hash of the findings, served as an ETag.
	Fingerprint string
	// FromCache is how many artifacts were answered without asking the
	// provider, and Fetched how many were asked about.
	FromCache int
	Fetched   int
	// RetrievedAt is when this answer was assembled.
	RetrievedAt time.Time
	// Provider names the scanner, or is empty when none is configured.
	Provider string
	// Enabled reports whether the scanner is switched on for this repository.
	Enabled bool
	// Unavailable, when set, is why nothing could be retrieved at all.
	Unavailable string
}

// Service answers the platform's security questions.
//
// It owns the orchestration - which provider, what is cached, what must be
// fetched, in what order, and what to tell the user while it happens - and no
// scanner-specific knowledge whatsoever.
type Service struct {
	resolver Resolver
	cache    Cache
	log      *slog.Logger
}

// NewService builds the service. A nil cache is legal and means every read goes
// to the provider, which is correct for a deployment that has not configured
// storage and wrong for one that has.
func NewService(resolver Resolver, cache Cache, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{resolver: resolver, cache: cache, log: log}
}

// Posture retrieves one release's security state.
//
// # The order of operations, and why
//
//  1. Resolve the provider. A repository with no scanner, or one with Xray
//     switched off, is answered here without any I/O and without any pretence
//     that the release is clean.
//  2. Read the cache. A release of 157 artifacts whose findings were fetched
//     two minutes ago must not cost 157 Xray queries to render a page.
//  3. Fetch what is missing, in parallel, inside the provider.
//  4. Write back both tiers.
//
// Step 2 is skipped entirely on a refresh, and step 4 is skipped when there is
// no cache.
func (s *Service) Posture(ctx context.Context, req Request) (Result, error) {
	res := Result{RetrievedAt: time.Now().UTC()}
	if len(req.Artifacts) == 0 {
		res.Posture = Summarize(nil)
		return res, nil
	}

	// The denominator, before anything has been asked. Without it the bar has
	// no total until the first batch lands, which on a slow scanner is the
	// first thirty seconds - exactly the window somebody is watching it in.
	ReportStage(req.Progress, StageResolving, len(req.Artifacts), len(req.Artifacts))

	provider, err := s.provider(ctx, req.Scope)
	if err != nil {
		return res, err
	}
	res.Provider = provider.Name()
	res.Enabled = provider.Enabled()

	// A disabled scanner answers immediately, says so on every artifact, and
	// is never cached - the answer is a fact about configuration, and caching
	// it would outlive the configuration change that fixes it.
	if !provider.Enabled() {
		reports, err := provider.Scan(ctx, req.Artifacts, ScanOptions{Progress: req.Progress})
		if err != nil {
			return res, err
		}
		res.Posture = Summarize(reports)
		res.Fingerprint = FingerprintReports(reports)
		return res, nil
	}

	refs := dedupeRefs(req.Artifacts)
	cached := map[string]Report{}
	if s.cache != nil && !req.Refresh {
		cached, err = s.load(ctx, req.Scope, refs, req.Detail)
		if err != nil {
			// A cache that will not answer is a slow read, not a failed one.
			s.log.Warn("security: cache read failed; asking the provider",
				"product", req.Scope.Product, "repository", req.Scope.Repository, "error", err)
			cached = map[string]Report{}
		}
	}
	if s.cache != nil && req.Refresh {
		if err := s.cache.Invalidate(ctx, req.Scope, refs); err != nil {
			s.log.Warn("security: cache invalidation failed", "error", err)
		}
	}

	var missing []ArtifactRef
	reports := make([]Report, 0, len(refs))
	for _, ref := range refs {
		if r, ok := cached[ref.Ref()]; ok {
			r.Artifact = ref
			r.FromCache = true
			reports = append(reports, r)
			continue
		}
		missing = append(missing, ref)
	}
	res.FromCache = len(reports)

	if len(reports) > 0 {
		ReportStage(req.Progress, StageCached, len(reports), len(refs))
	}

	if len(missing) > 0 {
		fetched, err := provider.Scan(ctx, missing, ScanOptions{
			Detail:   req.Detail,
			Refresh:  req.Refresh,
			Progress: req.Progress,
		})
		if err != nil {
			return res, err
		}
		res.Fetched = len(fetched)
		reports = append(reports, fetched...)

		if s.cache != nil {
			if err := s.cache.Save(ctx, req.Scope, fetched, req.Detail, req.TTL); err != nil {
				// Failing to cache is a performance problem, never a
				// correctness one: the answer in hand is the answer.
				s.log.Warn("security: cache write failed", "error", err)
			}
		}
	}

	ReportStage(req.Progress, StageCorrelating, len(reports), len(refs))
	sortReports(reports)
	res.Posture = Summarize(reports)
	res.Fingerprint = FingerprintReports(reports)
	return res, nil
}

// load reads whichever cache tier the request needs.
func (s *Service) load(ctx context.Context, scope Scope, refs []ArtifactRef, detail bool) (map[string]Report, error) {
	if detail {
		return s.cache.LoadDetails(ctx, scope, refs)
	}
	return s.cache.LoadSummaries(ctx, scope, refs)
}

// provider resolves the scanner for one repository, turning "there is none"
// into a disabled provider rather than an error.
func (s *Service) provider(ctx context.Context, scope Scope) (Provider, error) {
	if s.resolver == nil {
		return Disabled{Reason: "No security scanner is configured for this deployment."}, nil
	}
	p, err := s.resolver.ProviderFor(ctx, scope.Product, scope.Repository)
	switch {
	case err == nil:
		return p, nil
	case errors.Is(err, ErrNoProvider):
		return Disabled{Reason: "This repository has no security scanner configured."}, nil
	default:
		return nil, err
	}
}

// CompareRequest asks for two releases' postures and the delta between them.
type CompareRequest struct {
	A, B  Request
	NameA string
	NameB string
}

// CompareResult is the comparison plus both sides' provenance.
type CompareResult struct {
	Comparison Comparison
	A, B       Result
}

// Compare retrieves both releases and classifies the difference.
//
// The two retrievals run CONCURRENTLY. They are independent - different
// artifacts, possibly different repositories - and running them in sequence
// doubles the wait for no reason, which on a 157-artifact release is the
// difference between a page that opens and one that is abandoned.
func (s *Service) Compare(ctx context.Context, req CompareRequest) (CompareResult, error) {
	var (
		wg         sync.WaitGroup
		resA, resB Result
		errA, errB error
	)

	wg.Add(2)
	go func() {
		defer wg.Done()
		resA, errA = s.Posture(ctx, req.A)
	}()
	go func() {
		defer wg.Done()
		resB, errB = s.Posture(ctx, req.B)
	}()
	wg.Wait()

	if errA != nil {
		return CompareResult{}, fmt.Errorf("retrieve security for the base release: %w", errA)
	}
	if errB != nil {
		return CompareResult{}, fmt.Errorf("retrieve security for the new release: %w", errB)
	}

	ReportStage(req.A.Progress, StageComparing, 0, 0)
	cmp := Compare(CompareInput{
		A:     resA.Posture.Reports,
		B:     resB.Posture.Reports,
		NameA: req.NameA,
		NameB: req.NameB,
	})
	return CompareResult{Comparison: cmp, A: resA, B: resB}, nil
}

// dedupeRefs collapses artifacts that resolve to the same concrete identity.
//
// A release's tree lists the same image under an index and again under its own
// tag, and asking Xray about one digest twice is a wasted round trip and a
// double-counted finding.
func dedupeRefs(in []ArtifactRef) []ArtifactRef {
	seen := map[string]bool{}
	out := make([]ArtifactRef, 0, len(in))
	for _, ref := range in {
		k := ref.Ref()
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, ref)
	}
	return out
}

// sortReports puts the artifacts with the most to answer for first, so a
// truncated view shows the ones that matter. Ties break on name, so the order
// is stable across runs.
func sortReports(rs []Report) {
	sort.SliceStable(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		aw, bw := a.Counts.BySeverity.Weight(), b.Counts.BySeverity.Weight()
		if aw != bw {
			return aw > bw
		}
		return a.Artifact.ArtifactKey() < b.Artifact.ArtifactKey()
	})
}
