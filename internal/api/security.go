package api

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/security"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// SecurityAnalyzer is what the API needs from the security engine.
//
// A consumer-defined interface, like Comparer and Discoverer beside it: two
// calls, not the provider resolver, the cache and the parallel retrieval behind
// them (docs/design/15 §6).
type SecurityAnalyzer interface {
	Posture(ctx context.Context, req security.Request) (security.Result, error)
	Compare(ctx context.Context, req security.CompareRequest) (security.CompareResult, error)
}

// SecurityIndex is the searchable record of what has already been retrieved.
//
// Separate from SecurityAnalyzer because it answers a different question from a
// different place: the analyzer asks a scanner, and this reads what previous
// asks recorded. A search that reached a scanner would take a round trip per
// artifact per keystroke.
type SecurityIndex interface {
	Search(ctx context.Context, f store.SearchFilter) ([]store.SearchHit, error)
	ReleasesFor(ctx context.Context, productName string, artifactRefs []string) (map[string][]store.ReleaseRef, error)
	RepositoryByID(ctx context.Context, id int64) (store.RepositoryIdentity, error)
}

// Security custom methods on a package.
const (
	packageVerbSecurity        = "security"
	packageVerbCompareSecurity = "compareSecurity"
)

// maxSecurityArtifacts bounds one retrieval.
//
// A release in this system is a few hundred artifacts. The bound exists for the
// pathological case - a mis-analysed tree, a release that swept in a whole
// catalogue - where the cost is not ours but the scanner's, and an unbounded
// request is how one page render becomes a rate-limit ban for everybody.
const maxSecurityArtifacts = 2000

// securityDeadline bounds one security retrieval end to end.
//
// Generous because the work is real - hundreds of artifacts against a scanner
// that answers in hundreds of milliseconds - and bounded because an unbounded
// one is indistinguishable from a hang. The progress channel is what makes the
// wait legible; this is what makes it finite.
var securityDeadline = 5 * time.Minute

// handlePackageSecurity serves
// GET /api/v1/products/{product}/packages/{package}/security.
//
// # Two depths, one endpoint
//
// `?detail=true` returns every finding; without it, counts alone. They are one
// endpoint rather than two because they are one question asked at two depths,
// and because the cheap read is what a listing needs and the expensive one is
// what a person needs after clicking - splitting them would let the two answer
// differently about the same release.
func (s *Server) handlePackageSecurity(w http.ResponseWriter, r *http.Request) {
	productName := chi.URLParam(r, "product")
	ref := chi.URLParam(r, "package")

	if !s.productExists(w, r, productName) {
		return
	}
	if s.deps.Packages == nil {
		Error(w, r, v1.CodeUnavailable, "package storage is not configured")
		return
	}
	if s.deps.Security == nil {
		Error(w, r, v1.CodeUnavailable, "security scanning is not configured on this Coordinator")
		return
	}

	pkg, ok := s.resolvePackage(w, r, productName, ref)
	if !ok {
		return
	}

	q := r.URL.Query()
	detail := q.Get("detail") == "true"
	refresh := q.Get("refresh") == "true"

	tracker := s.securityProgress.start(q.Get("progressToken"))
	defer s.securityProgress.finish(q.Get("progressToken"))

	ctx, cancel := context.WithTimeout(r.Context(), securityDeadline)
	defer cancel()

	req, problem := s.securityRequestFor(ctx, productName, pkg, detail, refresh, tracker)
	if problem != "" {
		Error(w, r, v1.CodeUnavailable, problem)
		return
	}

	res, err := s.deps.Security.Posture(ctx, req)
	if err != nil {
		s.internal(w, r, "retrieve security posture", err)
		return
	}

	out := toAPIPackageSecurity(productName, pkg, req.Scope, res, detail)

	// An ETag over the FINDINGS, not over a timestamp. A re-scan that produced
	// identical results must not invalidate a client's copy - that is the
	// difference between a page that re-renders once an hour and one that
	// re-downloads two megabytes every time somebody presses refresh.
	if out.Fingerprint != "" {
		etag := `"` + out.Fingerprint + `"`
		w.Header().Set("ETag", etag)
		if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	// private, because these are one repository's findings under one
	// repository's permissions and a shared cache must never hold them.
	// must-revalidate, because a stale security answer is the one kind of
	// stale answer that changes a decision.
	w.Header().Set("Cache-Control", "private, max-age=60, must-revalidate")
	w.Header().Set("Vary", "Authorization")

	WriteJSON(w, r, http.StatusOK, out)
}

// handleCompareSecurity serves POST
// /api/v1/products/{product}/packages/{package}:compareSecurity.
//
// The package in the path is the OLD release and `against` names the new one,
// so every word in the answer - resolved, introduced - is written from the new
// release's point of view. That is the question being asked: not "how do these
// differ" but "is what I am about to ship better than what I am running".
func (s *Server) handleCompareSecurity(w http.ResponseWriter, r *http.Request) {
	productName := chi.URLParam(r, "product")
	ref := chi.URLParam(r, "package")

	if !s.productExists(w, r, productName) {
		return
	}
	if s.deps.Packages == nil {
		Error(w, r, v1.CodeUnavailable, "package storage is not configured")
		return
	}
	if s.deps.Security == nil {
		Error(w, r, v1.CodeUnavailable, "security scanning is not configured on this Coordinator")
		return
	}

	var body v1.SecurityCompareRequest
	if err := decodeOptionalJSON(r, &body); err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}
	if strings.TrimSpace(body.Against) == "" {
		Error(w, r, v1.CodeInvalidArgument,
			"a security comparison needs a second release: set `against` to a tag or digest")
		return
	}

	base, ok := s.resolvePackage(w, r, productName, ref)
	if !ok {
		return
	}
	other, ok := s.resolveSecondPackage(w, r, productName, body)
	if !ok {
		return
	}

	tracker := s.securityProgress.start(body.ProgressToken)
	defer s.securityProgress.finish(body.ProgressToken)

	ctx, cancel := context.WithTimeout(r.Context(), securityDeadline)
	defer cancel()

	// Details on both sides, always. A comparison is a classification of
	// individual findings; counts alone cannot tell "three resolved and three
	// introduced" from "nothing changed", and those are opposite answers.
	reqA, problem := s.securityRequestFor(ctx, productName, base, true, body.Refresh, tracker)
	if problem != "" {
		Error(w, r, v1.CodeUnavailable, problem)
		return
	}
	reqB, problem := s.securityRequestFor(ctx, productName, other, true, body.Refresh, tracker)
	if problem != "" {
		Error(w, r, v1.CodeUnavailable, problem)
		return
	}

	res, err := s.deps.Security.Compare(ctx, security.CompareRequest{
		A: reqA, B: reqB,
		NameA: releaseLabel(base),
		NameB: releaseLabel(other),
	})
	if err != nil {
		s.internal(w, r, "compare security", err)
		return
	}

	WriteJSON(w, r, http.StatusOK, toAPISecurityComparison(productName, base, other, reqA.Scope, reqB.Scope, res))
}

// handleSecuritySearch serves
// GET /api/v1/products/{product}/security/search?kind=&q=.
//
// Reads what has already been retrieved and never asks a scanner. That is a
// deliberate limit and the response says so: a search cannot answer "is this
// CVE anywhere in my estate", only "is it anywhere I have looked", and the two
// are different sentences. Answering the first would mean scanning every
// release in the catalogue on every search.
func (s *Server) handleSecuritySearch(w http.ResponseWriter, r *http.Request) {
	productName := chi.URLParam(r, "product")
	if !s.productExists(w, r, productName) {
		return
	}
	if s.deps.SecurityIndex == nil {
		Error(w, r, v1.CodeUnavailable, "security search is not configured on this Coordinator")
		return
	}

	q := r.URL.Query()
	query := strings.TrimSpace(q.Get("q"))
	if query == "" {
		Error(w, r, v1.CodeInvalidArgument, "q is required")
		return
	}

	kind, err := parseSearchKind(q.Get("kind"), query)
	if err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}

	limit := 200
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > 1000 {
			Error(w, r, v1.CodeInvalidArgument, "limit must be between 1 and 1000")
			return
		}
		limit = n
	}

	hits, err := s.deps.SecurityIndex.Search(r.Context(), store.SearchFilter{
		Product: productName,
		Kind:    kind,
		Query:   query,
		Exact:   q.Get("exact") == "true",
		// One over the limit, so "there is more" is answered without a second
		// count query and without claiming more that turns out not to exist.
		Limit: limit + 1,
	})
	if err != nil {
		s.internal(w, r, "security search", err)
		return
	}

	truncated := false
	if len(hits) > limit {
		hits = hits[:limit]
		truncated = true
	}

	releases := map[string][]store.ReleaseRef{}
	if len(hits) > 0 {
		refs := make([]string, 0, len(hits))
		seen := map[string]bool{}
		for _, h := range hits {
			if !seen[h.ArtifactRef] {
				seen[h.ArtifactRef] = true
				refs = append(refs, h.ArtifactRef)
			}
		}
		if found, err := s.deps.SecurityIndex.ReleasesFor(r.Context(), productName, refs); err == nil {
			releases = found
		} else {
			// The relationship is an enrichment. Losing it should cost the
			// navigation links, not the search.
			s.deps.Logger.Warn("security search: could not resolve releases", "error", err)
		}
	}

	WriteJSON(w, r, http.StatusOK, toAPISecuritySearch(productName, string(kind), query,
		q.Get("exact") == "true", hits, releases, truncated))
}

// handleSecurityProgress serves GET /api/v1/security/progress/{token}.
func (s *Server) handleSecurityProgress(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	entry, ok := s.securityProgress.read(token)
	if !ok {
		// A normal answer. Progress lives in one replica's memory and is
		// dropped shortly after the work finishes.
		Error(w, r, v1.CodeNotFound, "no security retrieval is in flight for that token")
		return
	}

	out := v1.SecurityProgressResponse{
		Done:      entry.Done,
		Notes:     entry.Notes,
		StartedAt: entry.StartedAt.UTC().Format(time.RFC3339),
		UpdatedAt: entry.UpdatedAt.UTC().Format(time.RFC3339),
	}
	for _, st := range entry.Stages {
		out.Stages = append(out.Stages, v1.SecurityProgressStage{
			Name: st.Name, Label: securityStageLabel(st.Name), Done: st.Done, Total: st.Total,
		})
	}
	WriteJSON(w, r, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// Assembling a request
// ---------------------------------------------------------------------------

// securityRequestFor turns a stored release into a security request.
//
// The scope comes from the repository the release was DISCOVERED IN, resolved
// by row id rather than by matching a registry host against the product
// document. A product with two sources on one host would otherwise resolve to
// whichever the guess happened to find first, and the two may have different
// Xray settings and different credentials.
//
// Returns a non-empty second value when the request cannot be assembled, with a
// sentence for the user rather than an error for a log.
func (s *Server) securityRequestFor(
	ctx context.Context, productName string, pkg store.PackageRow,
	detail, refresh bool, progress security.Progress,
) (security.Request, string) {
	rows, err := s.deps.Packages.ListArtifacts(ctx, pkg.ID)
	if err != nil {
		return security.Request{}, "could not read this release's artifacts: " + err.Error()
	}

	role := string(product.RoleSource)
	repoName := ""
	if s.deps.SecurityIndex != nil {
		if id, err := s.deps.SecurityIndex.RepositoryByID(ctx, pkg.SourceRepoID); err == nil {
			repoName, role = id.Name, id.Role
		}
	}

	scope := security.Scope{
		Product:    productName,
		Repository: repoName,
		Role:       role,
		Provider:   "jfrog-xray",
	}

	artifacts := s.securityArtifactsFor(productName, pkg, rows)
	if len(artifacts) > maxSecurityArtifacts {
		artifacts = artifacts[:maxSecurityArtifacts]
	}

	return security.Request{
		Scope:     scope,
		Artifacts: artifacts,
		Detail:    detail,
		Refresh:   refresh,
		TTL:       s.securityTTL(productName, product.Role(role), repoName),
		Progress:  progress,
	}, ""
}

// securityArtifactsFor turns stored artifact rows into scannable references.
//
// # Why the release's own manifest is included
//
// A release's root manifest is what a person means by "the release", and some
// JFrog deployments index the index rather than each child. Including it costs
// one lookup and is the difference between a release that reports its posture
// and one that reports nothing while every child says "not scanned".
//
// # Why it is added LAST, and only if nothing else claimed its digest
//
// A single-artifact release's root IS its one artifact, so the root and the
// row carry the same digest. Sending both asks the scanner about it twice and
// counts every finding twice. Deduplicating is not enough on its own, because
// which of the two survives decides what the artifact is CALLED - and the root
// is called after the release, whose name changes every release. Aligning two
// releases on that name reports every artifact as removed and every artifact as
// added, which is a comparison containing no information. So the named row
// wins, and the root fills in only where nothing else did.
func (s *Server) securityArtifactsFor(
	productName string, pkg store.PackageRow, rows []store.ArtifactRow,
) []security.ArtifactRef {
	classify := s.artifactClassifier(productName)

	out := make([]security.ArtifactRef, 0, len(rows)+1)
	seen := make(map[string]bool, len(rows)+1)

	for _, a := range rows {
		if a.Digest == "" || seen[a.Digest] {
			continue
		}
		seen[a.Digest] = true
		out = append(out, security.ArtifactRef{
			Name:       artifactName(a),
			Tag:        artifactTag(a),
			Digest:     a.Digest,
			Repository: artifactRepository(a, pkg.SourceRepository),
			MediaType:  a.MediaType,
			Kind:       classify(a.MediaType, a.ArtifactType, "", a.Annotations),
			SizeBytes:  a.SizeBytes,
			Platform:   a.Platform,
		})
	}

	if !seen[pkg.ManifestDigest] && pkg.ManifestDigest != "" {
		out = append(out, security.ArtifactRef{
			Name:       releaseLabel(pkg),
			Tag:        pkg.Tag,
			Digest:     pkg.ManifestDigest,
			Repository: pkg.SourceRepository,
			MediaType:  pkg.MediaType,
			Kind:       "index",
		})
	}
	return out
}

// artifactName is what a release calls one of its artifacts.
//
// `org.opencontainers.image.ref.name` is the publisher naming the component, and
// it is the only name that survives a release: digests change, and the position
// in the tree changes when a vendor reorganises. Falling back to the digest is
// correct but useless for alignment - a release whose artifacts are unnamed
// compares as entirely new every time, which the coverage numbers then say.
func artifactName(a store.ArtifactRow) string {
	if a.Annotations == nil {
		return shortDigest(a.Digest)
	}
	for _, key := range []string{
		"org.opencontainers.image.ref.name",
		"org.opencontainers.image.title",
	} {
		if v := strings.TrimSpace(a.Annotations[key]); v != "" {
			// A ref.name is "repository:tag"; the repository half is the name
			// that is stable across releases and the tag half is what changes.
			if i := strings.LastIndex(v, ":"); i > 0 && !strings.Contains(v[i+1:], "/") {
				return path.Base(v[:i])
			}
			return path.Base(v)
		}
	}
	return shortDigest(a.Digest)
}

func artifactTag(a store.ArtifactRow) string {
	if a.Annotations == nil {
		return ""
	}
	v := strings.TrimSpace(a.Annotations["org.opencontainers.image.ref.name"])
	if i := strings.LastIndex(v, ":"); i > 0 && !strings.Contains(v[i+1:], "/") {
		return v[i+1:]
	}
	if ver := strings.TrimSpace(a.Annotations["org.opencontainers.image.version"]); ver != "" {
		return ver
	}
	return ""
}

func artifactRepository(a store.ArtifactRow, fallback string) string {
	if a.Annotations != nil {
		v := strings.TrimSpace(a.Annotations["org.opencontainers.image.ref.name"])
		if i := strings.LastIndex(v, ":"); i > 0 && !strings.Contains(v[i+1:], "/") {
			return v[:i]
		}
		if v != "" {
			return v
		}
	}
	return fallback
}

func shortDigest(d string) string {
	if i := strings.Index(d, ":"); i >= 0 && len(d) > i+13 {
		return d[i+1 : i+13]
	}
	return d
}

// releaseLabel is what to call a release in a sentence.
func releaseLabel(pkg store.PackageRow) string {
	if pkg.DisplayTag != "" {
		return pkg.DisplayTag
	}
	if pkg.Tag != "" {
		return pkg.Tag
	}
	return shortDigest(pkg.ManifestDigest)
}

// securityTTL resolves the cache retention the repository's configuration asks
// for, falling back to the defaults when the product cannot be read.
func (s *Server) securityTTL(productName string, role product.Role, repository string) security.CacheTTL {
	var x *product.Xray
	if s.deps.Products != nil {
		if p, ok := s.deps.Products.Get(productName); ok {
			x, _ = p.XrayFor(role, repository)
		}
	}
	return security.CacheTTL{
		Summary: x.SummaryTTLOrDefault(),
		Detail:  x.DetailTTLOrDefault(),
	}
}

// resolveSecondPackage resolves the `against` reference of a comparison.
func (s *Server) resolveSecondPackage(
	w http.ResponseWriter, r *http.Request, productName string, body v1.SecurityCompareRequest,
) (store.PackageRow, bool) {
	ref := strings.TrimSpace(body.Against)
	if body.Repository != "" {
		ref = body.Repository + ":" + ref
	}
	return s.resolvePackage(w, r, productName, ref)
}

func parseSearchKind(raw, query string) (store.SearchKind, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(store.SearchCVE):
		return store.SearchCVE, nil
	case string(store.SearchComponent), "component":
		return store.SearchComponent, nil
	case string(store.SearchImage), "artifact":
		return store.SearchImage, nil
	case "":
		// Guessing beats refusing here: somebody who pasted a CVE id into a
		// search box has said what they mean, and asking them to choose a
		// radio button first is a worse interface than reading it.
		if strings.HasPrefix(strings.ToUpper(query), "CVE-") {
			return store.SearchCVE, nil
		}
		return store.SearchComponent, nil
	default:
		return "", fmt.Errorf("kind must be one of cve, package, image (got %q)", raw)
	}
}

// etagMatches implements the If-None-Match comparison, including `*` and a
// comma-separated list.
func etagMatches(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "*" {
		return true
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == etag {
			return true
		}
	}
	return false
}

func securityStageLabel(name string) string {
	switch name {
	case security.StageResolving:
		return "Working out what to scan"
	case security.StageFetching:
		return "Fetching from JFrog Xray"
	case security.StageCached:
		return "Reading the platform cache"
	case security.StageCorrelating:
		return "Correlating findings"
	case security.StageComparing:
		return "Comparing the two releases"
	case security.StageExporting:
		return "Preparing the export"
	default:
		return strings.ToUpper(name[:1]) + name[1:]
	}
}

// sortStages keeps the progress list in the order the work happens, so a bar
// does not jump backwards as concurrent stages report.
func sortStages(in []securityStage) []securityStage {
	order := map[string]int{
		security.StageResolving:   0,
		security.StageCached:      1,
		security.StageFetching:    2,
		security.StageCorrelating: 3,
		security.StageComparing:   4,
		security.StageExporting:   5,
	}
	out := append([]securityStage(nil), in...)
	sort.SliceStable(out, func(i, j int) bool { return order[out[i].Name] < order[out[j].Name] })
	return out
}
