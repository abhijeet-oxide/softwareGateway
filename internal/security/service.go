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
	// Invalidate drops every tier for the given artifacts.
	//
	// Deliberately NOT part of a refresh: a refresh skips the read and
	// overwrites what it finds, so that a release keeps its results while
	// another release sharing the same images is being re-scanned. This is for
	// forgetting on purpose - a repository removed, a scanner replaced - where
	// the old answer is not stale but wrong.
	Invalidate(ctx context.Context, scope Scope, refs []ArtifactRef) error
}

// CacheTTL is how long each tier is PINNED for - not how long it lives.
//
// Past its retention a row becomes evictable and is still served; it goes only
// when the store is over its byte budget and it is the least recently read
// thing in it. See db/migrations/*/00033_security_cache.sql for why that
// replaced expiry, and internal/store/security.go for what enforces it.
type CacheTTL struct {
	Summary time.Duration
	Detail  time.Duration
	// Documents pins the raw scanner bodies: the vulnerability response as it
	// arrived, the SBOM, the policy verdict. The largest tier and the first
	// evicted, because it is the only one a single request rebuilds.
	Documents time.Duration
}

// Request is one release's security read.
type Request struct {
	Scope Scope
	// Artifacts is what to ask about. The caller assembles these from the
	// release's own artifact tree, because what a release CONTAINS is the
	// core's knowledge and not the provider's.
	Artifacts []ArtifactRef
	// Release names the release being read, for the providers that group by
	// one. See ScanOptions.Release.
	Release ReleaseRef
	// Detail asks for full findings rather than counts.
	Detail bool
	// Refresh bypasses the cache and re-queries the provider for EVERY
	// artifact. Only a person asking for exactly that should set it.
	//
	// It is not what a sync does. See MaxAge.
	Refresh bool
	// MaxAge is how old a stored answer may be before this retrieval asks the
	// scanner about it again. Zero accepts anything held.
	//
	// # Why a sync uses this instead of Refresh
	//
	// Because stored answers are keyed by ARTIFACT, not by release, and
	// releases of one product overwhelmingly share their images. Syncing the
	// November release of a product whose October release was synced this
	// morning used to ask the scanner about all 157 images; 150 of them were
	// the same bytes, already answered for, an hour old. That is 150 requests
	// and ten minutes spent re-learning something already known - and it is
	// paid by whoever is waiting, against somebody else's rate limit.
	//
	// So a sync asks only about what it does not have or what has aged out.
	// The saving is the whole point of storing by artifact in the first place.
	MaxAge time.Duration
	// Rescan names the artifacts to ask about regardless of age, for the case
	// where the caller knows something the store does not - an image whose scan
	// failed last time, an SBOM a reader has just asked to generate.
	Rescan map[string]bool
	// TTL governs what is written back.
	TTL CacheTTL
	// Documents are the extra scanner bodies to retrieve alongside the
	// findings - policy violations, malware, an SBOM.
	//
	// Empty is the common case and costs nothing. Naming a kind here is what
	// turns a sync from "ask about the CVEs" into "ask about the CVEs and the
	// gate", and each kind is a request per artifact, which is why the caller
	// chooses rather than the provider assuming.
	//
	// DocumentVulnerabilities never needs naming: that body is captured from
	// the request the scan was making anyway.
	Documents []DocumentKind
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
	// documents is where raw scanner bodies are kept. Nil is legal and means
	// they are retrieved, used, and thrown away - correct for a deployment
	// without storage, and an export that costs a fresh fetch every time.
	documents DocumentStore
	log       *slog.Logger
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

// WithDocuments attaches the raw-body store.
//
// A setter rather than a constructor argument because the store is optional and
// arrived later than the constructor's other three, and adding a fourth
// positional argument to every call site to pass nil at most of them is how a
// constructor becomes something people copy without reading.
func (s *Service) WithDocuments(store DocumentStore) *Service {
	s.documents = store
	return s
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
	// A refresh does NOT delete what is held first. It skips the read above and
	// overwrites on the way out.
	//
	// # Why deleting first was wrong
	//
	// The stored rows are keyed by artifact, not by release, because that is
	// what makes two releases of one product share the scan of a base image
	// they both carry. Deleting them at the START of a sync therefore emptied
	// every OTHER release holding those images, for the ten minutes the sync
	// took - somebody with a release open watched its vulnerabilities vanish
	// because a colleague pressed Sync on a different one.
	//
	// It also fought the store's own rule that an artifact the scanner would
	// not answer for keeps its previous result: there was no previous result
	// left to keep, so a sync interrupted by a scanner outage turned a release
	// with ninety thousand findings into a release with none.
	//
	// Save upserts, so nothing needs clearing: an artifact's row is replaced by
	// its new answer the moment there is one, and until then the old answer is
	// the best thing anybody can be shown. Artifacts that have left a release
	// are collected by the sweep, which is where that belongs.

	var missing []ArtifactRef
	var reused int
	reports := make([]Report, 0, len(refs))
	now := time.Now().UTC()
	for _, ref := range refs {
		r, ok := cached[ref.Ref()]
		switch {
		case !ok:
			missing = append(missing, ref)
			continue
		case req.Rescan[ref.Ref()]:
			missing = append(missing, ref)
			continue
		case stale(r, req.MaxAge, now):
			missing = append(missing, ref)
			continue
		}
		r.Artifact = ref
		r.FromCache = true
		reports = append(reports, r)
		reused++
	}
	res.FromCache = len(reports)
	if reused > 0 && len(missing) > 0 {
		// Said out loud, because a sync that finishes in twenty seconds after
		// a colleague's took ten minutes looks broken otherwise. The saving is
		// the reason answers are stored by artifact rather than by release.
		ReportInfo(req.Progress, fmt.Sprintf(
			"%d of %d images already had a stored result within the age limit and were not "+
				"asked about again. Asking the scanner about the other %d.",
			reused, len(refs), len(missing)))
	}

	if len(reports) > 0 {
		ReportStage(req.Progress, StageCached, len(reports), len(refs))
	}

	if len(missing) > 0 {
		// The raw bodies ride out on the request that was going to happen
		// anyway. Collected into a slice rather than written straight through,
		// because this callback runs on the scanner's own request path and a
		// database write there would slow every batch down to the speed of the
		// slowest INSERT.
		var (
			sinkMu sync.Mutex
			raw    []Document
		)
		var sink DocumentSink
		if s.documents != nil {
			sink = DocumentSinkFunc(func(d Document) {
				sinkMu.Lock()
				raw = append(raw, d)
				sinkMu.Unlock()
			})
		}

		fetched, err := provider.Scan(ctx, missing, ScanOptions{
			Detail:   req.Detail,
			Refresh:  req.Refresh,
			Release:  req.Release,
			Progress: req.Progress,
			Sink:     sink,
		})
		if err != nil {
			return res, err
		}
		res.Fetched = len(fetched)

		// The extra bodies, where the caller asked for them and the scanner can
		// produce them. Before the cache write, because the normalized halves -
		// the policy verdict and the malware list - belong on the reports that
		// are about to be stored, and a second write to add them later would be
		// a window in which the page shows findings and no gate.
		docs := s.fetchDocuments(ctx, provider, req, fetched)
		raw = append(raw, docs...)
		attachDocuments(fetched, raw)

		reports = append(reports, fetched...)

		if s.cache != nil {
			if err := s.cache.Save(ctx, req.Scope, fetched, req.Detail, req.TTL); err != nil {
				// Failing to cache is a performance problem, never a
				// correctness one: the answer in hand is the answer.
				s.log.Warn("security: cache write failed", "error", err)
			}
		}
		if s.documents != nil && len(raw) > 0 {
			if err := s.documents.SaveDocuments(ctx, req.Scope, raw, req.TTL.Documents); err != nil {
				s.log.Warn("security: document write failed", "error", err)
			}
		}
	}

	ReportStage(req.Progress, StageCorrelating, len(reports), len(refs))
	sortReports(reports)
	res.Posture = Summarize(reports)
	res.Fingerprint = FingerprintReports(reports)
	return res, nil
}

// fetchDocuments asks the provider for the extra bodies the caller wants.
//
// Returns nothing rather than failing when the provider cannot produce them.
// An Xray without the SBOM endpoint, or a token without violation permission,
// is a deployment whose vulnerability sync works perfectly - and failing the
// whole sync over the tab it cannot fill would be the tail wagging the dog.
func (s *Service) fetchDocuments(
	ctx context.Context, provider Provider, req Request, reports []Report,
) []Document {
	if len(req.Documents) == 0 || len(reports) == 0 {
		return nil
	}
	docProvider, ok := provider.(DocumentProvider)
	if !ok {
		ReportNote(req.Progress, fmt.Sprintf(
			"%s does not produce SBOMs or policy verdicts, so only vulnerabilities were retrieved.",
			provider.Name()))
		return nil
	}

	// Only for artifacts the scanner actually answered about. Asking for the
	// SBOM of an image that is not in the repository is a request that can only
	// fail, once per image, on exactly the release where somebody is already
	// waiting.
	refs := make([]ArtifactRef, 0, len(reports))
	for _, r := range reports {
		if r.Status == StatusScanned {
			refs = append(refs, r.Artifact)
		}
	}
	if len(refs) == 0 {
		return nil
	}

	docs, err := docProvider.Documents(ctx, refs, req.Documents, ScanOptions{
		Refresh:  req.Refresh,
		Release:  req.Release,
		Progress: req.Progress,
	})
	if err != nil {
		s.log.Warn("security: could not retrieve scanner documents",
			"product", req.Scope.Product, "repository", req.Scope.Repository, "error", err)
		return docs
	}
	return docs
}

// attachDocuments puts the normalized halves of the fetched bodies onto the
// reports they belong to.
//
// The raw halves are stored separately and are not put on a Report at all: a
// Report is serialized into the cache on every sync, and a forty-megabyte SBOM
// riding on one would multiply the stored size of every release to serve a
// download somebody presses once a month.
func attachDocuments(reports []Report, docs []Document) {
	if len(docs) == 0 {
		return
	}
	byRef := make(map[string]int, len(reports))
	for i, r := range reports {
		byRef[r.Artifact.Ref()] = i
	}
	for _, doc := range docs {
		i, ok := byRef[doc.Artifact.Ref()]
		if !ok {
			continue
		}
		switch doc.Kind {
		case DocumentPolicy:
			reports[i].Violations = doc.Violations
		case DocumentMalware:
			// Malware arrives twice on a full sync - once from the scan's own
			// findings and once from the policy verdict - and the two name the same
			// package by different routes. The scan's list wins because it is
			// the one that exists on every Xray version; the violations only
			// fill a gap.
			if len(reports[i].Malware) == 0 {
				reports[i].Malware = malwareFromViolations(doc.Violations)
			}
		}
		reports[i].Documents = append(reports[i].Documents, DocumentSummary{
			Kind:        doc.Kind,
			Available:   doc.Available,
			ContentType: doc.ContentType,
			SourceBytes: doc.SourceBytes,
			Message:     doc.Message,
		})
	}
}

// malwareFromViolations turns malicious policy violations into findings, so the
// malware tab has rows on a platform whose Xray grades malware only through its
// watches.
func malwareFromViolations(violations []Violation) []Finding {
	if len(violations) == 0 {
		return nil
	}
	out := make([]Finding, 0, len(violations))
	for _, v := range violations {
		out = append(out, Finding{
			CVE:       v.CVE,
			ID:        v.ID,
			Severity:  v.Severity,
			Summary:   v.Summary,
			Component: v.Component,
			FixedIn:   v.FixedIn,
			Fixable:   len(v.FixedIn) > 0,
			Provider:  v.Provider,
			Policy:    v.Watch,
		})
	}
	return DedupeFindings(out)
}

// DocumentRequest asks for the scanner's own bodies about some artifacts.
type DocumentRequest struct {
	Scope     Scope
	Artifacts []ArtifactRef
	Kinds     []DocumentKind
	// Release names the release these artifacts belong to, for the providers
	// that group by one.
	Release ReleaseRef
	// Refresh re-asks the scanner even for a body already held. Only a person
	// pressing refresh should set it.
	Refresh bool
	// TTL governs what is written back.
	TTL      CacheTTL
	Progress Progress
}

// Documents serves the scanner's own bodies, fetching what is not held.
//
// # Why this fetches at all, when the sync already captured what it needs
//
// Because the SBOM is not one of the things the sync captures. It is tens of
// megabytes and minutes per image, so a deployment that generated one for every
// image on every sync would turn a two-minute job into an hour - and the SBOM
// is wanted for a download somebody presses occasionally, not for a page.
//
// So it is fetched here, for the artifacts asked about, when somebody asks. One
// image's SBOM is one request; a release's is a bounded parallel fan-out
// through the same pacer as everything else.
func (s *Service) Documents(ctx context.Context, req DocumentRequest) ([]Document, error) {
	if len(req.Artifacts) == 0 || len(req.Kinds) == 0 {
		return nil, nil
	}

	refs := dedupeRefs(req.Artifacts)
	held := map[string]map[DocumentKind]Document{}
	if s.documents != nil && !req.Refresh {
		var err error
		held, err = s.documents.LoadDocuments(ctx, req.Scope, refs, req.Kinds)
		if err != nil {
			// A store that will not answer is a slow read, not a failed one.
			s.log.Warn("security: document read failed; asking the scanner",
				"product", req.Scope.Product, "error", err)
			held = map[string]map[DocumentKind]Document{}
		}
	}

	var out []Document
	// missing is per KIND, because an image whose vulnerability body is held
	// and whose SBOM is not needs one request rather than none or two.
	missing := map[DocumentKind][]ArtifactRef{}
	for _, ref := range refs {
		for _, kind := range req.Kinds {
			if doc, ok := held[ref.Ref()][kind]; ok && doc.Available {
				doc.Artifact = ref
				out = append(out, doc)
				continue
			}
			missing[kind] = append(missing[kind], ref)
		}
	}
	if len(missing) == 0 {
		return out, nil
	}

	provider, err := s.provider(ctx, req.Scope)
	if err != nil {
		return out, err
	}
	if !provider.Enabled() {
		return out, nil
	}
	docProvider, ok := provider.(DocumentProvider)
	if !ok {
		return out, nil
	}

	// Grouped back into one call per KIND SET, so an export asking for four
	// kinds across a release is four fan-outs rather than four hundred.
	byRef := map[string][]DocumentKind{}
	order := []ArtifactRef{}
	seen := map[string]bool{}
	for kind, list := range missing {
		for _, ref := range list {
			byRef[ref.Ref()] = append(byRef[ref.Ref()], kind)
			if !seen[ref.Ref()] {
				seen[ref.Ref()] = true
				order = append(order, ref)
			}
		}
	}

	fetched, err := docProvider.Documents(ctx, order, req.Kinds, ScanOptions{
		Refresh: req.Refresh, Release: req.Release, Progress: req.Progress,
	})
	if err != nil {
		return out, err
	}
	out = append(out, fetched...)

	if s.documents != nil && len(fetched) > 0 {
		if err := s.documents.SaveDocuments(ctx, req.Scope, fetched, req.TTL.Documents); err != nil {
			// Failing to store is a slower next time, never a wrong answer now.
			s.log.Warn("security: document write failed", "error", err)
		}
	}
	return out, nil
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
	p, err := s.resolver.ProviderFor(ctx, scope)
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

// stale reports whether a stored answer is too old to reuse, or is not an
// answer at all.
//
// A report the scanner would not answer for is ALWAYS retried. It is stored so
// that a page has something to show - "Xray did not respond" beats a blank -
// but it is not a result, and treating it as one would mean an image that
// failed once during an outage stays unanswered until somebody notices and
// forces a refresh by hand.
func stale(r Report, maxAge time.Duration, now time.Time) bool {
	switch r.Status {
	case StatusUnavailable, StatusNotScanned:
		return true
	}
	if maxAge <= 0 {
		return false
	}
	if r.RetrievedAt.IsZero() {
		// Stored before this platform recorded retrieval times. Ask again once
		// rather than trusting an answer whose age is unknown.
		return true
	}
	return now.Sub(r.RetrievedAt) > maxAge
}
