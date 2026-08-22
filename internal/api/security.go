package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/regclient"
	"github.com/abhijeet-oxide/softwareGateway/internal/security"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// Security, as a job and a stored result.
//
// # Why nothing here queries a scanner
//
// It did, once: every read of a release's security went to Xray, and a listing
// of twenty releases was twenty scanner-backed reads to draw one column. That
// column shipped behind a toggle, which is the shape of a design apologising
// for itself.
//
// Now there is exactly one thing that talks to a scanner - `:syncSecurity` -
// and it is an explicit act with a durable result. Everything else in this file
// reads what that stored. A listing is a join, a comparison is two reads, a
// search is an indexed query, and none of the three can be slow because a
// vendor's scanner is busy.

// SecuritySyncer starts and reports on vulnerability syncs.
//
// A consumer-defined interface, like Comparer and Discoverer beside it: three
// calls, not the provider resolver, the cache and the parallel retrieval behind
// them (docs/design/15 §6).
type SecuritySyncer interface {
	Start(ctx context.Context, req security.SyncRequest) (security.SyncStatus, error)
	Progress(packageID int64) (*security.SyncProgress, bool)
	Running(packageID int64) bool
}

// SecurityStore is the stored result of syncs, which is what every read serves.
type SecurityStore interface {
	Get(ctx context.Context, packageID int64) (store.PackageSecurityRow, bool, error)
	ForPackages(ctx context.Context, ids []int64) (map[int64]store.PackageSecurityRow, error)
	ReportsFor(ctx context.Context, scope security.Scope, refs []security.ArtifactRef) ([]security.Report, error)
}

// SecurityIndex is the searchable record of what syncs have recorded.
//
// Separate from SecurityStore because it answers across releases rather than
// about one, and because it is readable on a Coordinator that cannot currently
// reach a scanner at all - which is exactly when somebody is searching for a
// CVE.
type SecurityIndex interface {
	Search(ctx context.Context, f store.SearchFilter) ([]store.SearchHit, error)
	ReleasesFor(ctx context.Context, productName string, artifactRefs []string) (map[string][]store.ReleaseRef, error)
}

// packageVerbSyncSecurity is the custom method that talks to the scanner.
const (
	packageVerbSyncSecurity    = "syncSecurity"
	packageVerbCompareSecurity = "compareSecurity"
)

// maxSecurityArtifacts bounds one sync.
//
// A release in this system is a few hundred artifacts. The bound exists for the
// pathological case - a mis-analysed tree, a release that swept in a whole
// catalogue - where the cost is not ours but the scanner's, and an unbounded
// request is how one button press becomes a rate-limit ban for everybody.
const maxSecurityArtifacts = 2000

// ---------------------------------------------------------------------------
// Reading
// ---------------------------------------------------------------------------

// handlePackageSecurity serves
// GET /api/v1/products/{product}/packages/{package}/security.
//
// Reads stored data and nothing else, so it is fast enough to poll - which is
// what the interface does while a sync runs, because the sync's live position
// travels in this same response rather than on a channel of its own.
func (s *Server) handlePackageSecurity(w http.ResponseWriter, r *http.Request) {
	productName := chi.URLParam(r, "product")
	ref := chi.URLParam(r, "package")

	if !s.productExists(w, r, productName) {
		return
	}
	if s.deps.Packages == nil || s.deps.SecurityStore == nil {
		Error(w, r, v1.CodeUnavailable, "security storage is not configured on this Coordinator")
		return
	}

	pkg, ok := s.resolvePackage(w, r, productName, ref)
	if !ok {
		return
	}

	detail := r.URL.Query().Get("detail") == "true"
	out, err := s.packageSecurity(r.Context(), productName, pkg, detail)
	if err != nil {
		s.internal(w, r, "read package security", err)
		return
	}

	// An ETag over the FINDINGS, not over a timestamp. A re-sync that produced
	// identical results must not invalidate a client's copy - that is the
	// difference between a page that re-renders when something changed and one
	// that re-downloads megabytes on every poll.
	//
	// Never while a sync is running: the answer is changing by definition, and
	// a 304 would freeze the progress the caller is polling for.
	if out.Fingerprint != "" && out.Sync.State != string(store.PackageSecuritySyncing) {
		etag := `"` + out.Fingerprint + `"`
		w.Header().Set("ETag", etag)
		if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	// private, because these are one repository's findings under one
	// repository's permissions and a shared cache must never hold them.
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("Vary", "Authorization")

	WriteJSON(w, r, http.StatusOK, out)
}

// packageSecurity assembles one release's stored security state.
func (s *Server) packageSecurity(
	ctx context.Context, productName string, pkg store.PackageRow, detail bool,
) (v1.PackageSecurityResponse, error) {
	row, _, err := s.deps.SecurityStore.Get(ctx, pkg.ID)
	if err != nil {
		return v1.PackageSecurityResponse{}, err
	}

	target := s.securityTargetFor(ctx, productName, pkg)

	out := toAPIPackageSecurity(productName, pkg, row, target, detail)
	out.Sync = s.syncStatusFor(pkg.ID, row, target)

	if detail && row.Synced() {
		refs := s.securityArtifactsFor(productName, pkg, ctx)
		reports, err := s.deps.SecurityStore.ReportsFor(ctx, target.Scope, refs)
		if err != nil {
			return v1.PackageSecurityResponse{}, err
		}
		posture := security.Summarize(reports)
		for _, rep := range posture.Reports {
			out.Reports = append(out.Reports, toAPIReport(rep))
		}
	}
	return out, nil
}

// handleSyncPackageSecurity serves POST
// /api/v1/products/{product}/packages/{package}:syncSecurity.
//
// Starts the work and answers immediately. The alternative - holding the
// request open for the minutes a real release takes - puts every intermediary's
// idle timeout into the control plane and gives the user a spinner that cannot
// say anything. Same argument discovery's `wait: false` makes.
func (s *Server) handleSyncPackageSecurity(w http.ResponseWriter, r *http.Request) {
	productName := chi.URLParam(r, "product")
	ref := chi.URLParam(r, "package")

	if !s.productExists(w, r, productName) {
		return
	}
	if s.deps.Packages == nil || s.deps.SecuritySync == nil {
		Error(w, r, v1.CodeUnavailable, "security scanning is not configured on this Coordinator")
		return
	}

	pkg, ok := s.resolvePackage(w, r, productName, ref)
	if !ok {
		return
	}

	target := s.securityTargetFor(r.Context(), productName, pkg)
	if !target.Available {
		// A precondition rather than an internal error: nothing is broken, the
		// product simply has no repository with a scanner switched on, and the
		// message says which knob turns it on.
		Error(w, r, v1.CodeFailedPrecondition, target.Reason)
		return
	}

	artifacts := s.securityArtifactsFor(productName, pkg, r.Context())
	if len(artifacts) == 0 {
		Error(w, r, v1.CodeFailedPrecondition,
			"this release has not been analysed yet, so there is nothing to scan: analyse it first")
		return
	}

	status, err := s.deps.SecuritySync.Start(r.Context(), security.SyncRequest{
		PackageID: pkg.ID,
		Label:     releaseLabel(pkg),
		Scope:     target.Scope,
		Artifacts: artifacts,
		TTL:       s.securityTTL(productName, product.Role(target.Scope.Role), target.Scope.Repository),
	})
	switch {
	case errors.Is(err, store.ErrSyncInFlight):
		// Not a failure. The thing the caller wanted is already happening, and
		// saying so beats a 409 that reads like a refusal.
		status = security.SyncAlreadyRunning
	case err != nil:
		s.internal(w, r, "start security sync", err)
		return
	}

	row, _, err := s.deps.SecurityStore.Get(r.Context(), pkg.ID)
	if err != nil {
		s.internal(w, r, "read package security", err)
		return
	}

	WriteJSON(w, r, http.StatusAccepted, v1.SyncSecurityResponse{
		Product:   productName,
		Package:   packageReferenceOf(pkg),
		Status:    string(status),
		Started:   status == security.SyncStarted,
		Sync:      s.syncStatusFor(pkg.ID, row, target),
		Artifacts: len(artifacts),
	})
}

// syncStatusFor renders what is happening to a release right now.
//
// The stored state is authoritative and the live progress is an enrichment: a
// sync running on ANOTHER replica has no in-memory progress here, and the
// answer must still be "syncing" rather than "never synced". Reading it the
// other way round is how a two-replica deployment shows a spinner that resets
// on every second request.
func (s *Server) syncStatusFor(
	packageID int64, row store.PackageSecurityRow, target securityTarget,
) v1.SecuritySyncStatus {
	out := v1.SecuritySyncStatus{
		State:      string(row.State),
		Error:      row.Error,
		CanSync:    target.Available,
		Reason:     target.Reason,
		Repository: target.Scope.Repository,
		Provider:   target.Scope.Provider,
	}
	if out.State == "" {
		out.State = string(store.PackageSecurityNever)
	}
	out.Label = syncStateLabel(store.PackageSecurityState(out.State))

	if row.SyncedAt != nil {
		out.SyncedAt = row.SyncedAt.UTC().Format(rfc3339)
	}
	if row.StartedAt != nil {
		out.StartedAt = row.StartedAt.UTC().Format(rfc3339)
	}

	if s.deps.SecuritySync == nil {
		return out
	}
	if progress, ok := s.deps.SecuritySync.Progress(packageID); ok {
		stages, notes, _, _ := progress.Snapshot()
		for _, st := range stages {
			out.Stages = append(out.Stages, v1.SecurityProgressStage{
				Name: st.Name, Label: securityStageLabel(st.Name), Done: st.Done, Total: st.Total,
			})
		}
		out.Notes = notes
	}
	return out
}

func syncStateLabel(state store.PackageSecurityState) string {
	switch state {
	case store.PackageSecuritySyncing:
		return "Syncing vulnerabilities"
	case store.PackageSecuritySynced:
		return "Vulnerabilities synced"
	case store.PackageSecurityFailed:
		return "Vulnerability sync failed"
	default:
		return "Vulnerabilities not synced"
	}
}

// ---------------------------------------------------------------------------
// Comparison
// ---------------------------------------------------------------------------

// handleCompareSecurity serves POST
// /api/v1/products/{product}/packages/{package}:compareSecurity.
//
// Both sides come from storage, so this is two indexed reads and an in-memory
// classification - fast enough to run on a page load. A release that has never
// been synced is reported as such rather than triggering a scan: a comparison
// that quietly started two multi-minute retrievals would be a button that
// appears to hang.
func (s *Server) handleCompareSecurity(w http.ResponseWriter, r *http.Request) {
	productName := chi.URLParam(r, "product")
	ref := chi.URLParam(r, "package")

	if !s.productExists(w, r, productName) {
		return
	}
	if s.deps.Packages == nil || s.deps.SecurityStore == nil {
		Error(w, r, v1.CodeUnavailable, "security storage is not configured on this Coordinator")
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

	out, err := s.compareSecurity(r.Context(), productName, base, other)
	if err != nil {
		s.internal(w, r, "compare security", err)
		return
	}
	WriteJSON(w, r, http.StatusOK, out)
}

// compareSecurity classifies two stored releases.
func (s *Server) compareSecurity(
	ctx context.Context, productName string, base, other store.PackageRow,
) (v1.SecurityComparisonResponse, error) {
	sideA, err := s.securitySide(ctx, productName, base)
	if err != nil {
		return v1.SecurityComparisonResponse{}, err
	}
	sideB, err := s.securitySide(ctx, productName, other)
	if err != nil {
		return v1.SecurityComparisonResponse{}, err
	}

	cmp := security.Compare(security.CompareInput{
		A: sideA.reports, B: sideB.reports,
		NameA: releaseLabel(base), NameB: releaseLabel(other),
	})
	return toAPISecurityComparison(productName, base, other, sideA, sideB, cmp), nil
}

// securitySide is one end of a comparison, read from storage.
type securitySide struct {
	row     store.PackageSecurityRow
	target  securityTarget
	reports []security.Report
	posture security.Posture
}

func (s *Server) securitySide(
	ctx context.Context, productName string, pkg store.PackageRow,
) (securitySide, error) {
	row, _, err := s.deps.SecurityStore.Get(ctx, pkg.ID)
	if err != nil {
		return securitySide{}, err
	}
	side := securitySide{row: row, target: s.securityTargetFor(ctx, productName, pkg)}

	// A release nobody synced contributes NO reports, which the comparison
	// engine reads as "no coverage" and answers inconclusive. That is the
	// honest outcome: inventing empty scanned reports for it would let an
	// unsynced release read as a clean one.
	if !row.Synced() {
		return side, nil
	}

	refs := s.securityArtifactsFor(productName, pkg, ctx)
	reports, err := s.deps.SecurityStore.ReportsFor(ctx, side.target.Scope, refs)
	if err != nil {
		return securitySide{}, err
	}
	side.reports = reports
	side.posture = security.Summarize(reports)
	return side, nil
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

// handleSecuritySearch serves
// GET /api/v1/products/{product}/security/search?kind=&q=.
//
// Reads the index a sync wrote and never a scanner. It therefore answers "is
// this CVE in a release somebody has synced", not "is it anywhere in my
// estate", and the response says so on every result - including a full one. A
// search that silently returned nothing would be read as "this does not affect
// us", which is the most dangerous thing this feature could say wrongly.
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

// ---------------------------------------------------------------------------
// Assembling a request
// ---------------------------------------------------------------------------

// securityTarget is the repository a release's scanner answers for.
type securityTarget struct {
	Scope security.Scope
	// Available is false when no configured repository has a scanner. Reason
	// then says which knob turns one on.
	Available bool
	Reason    string
}

// securityTargetFor decides which configured repository to ask about a release.
//
// # Why this is not "the repository it was discovered in"
//
// Because that is the VENDOR'S registry, and the vendor does not run your
// scanner. A release is discovered on a vendor registry - Nokia NEAR, say - and
// replicated into JFrog, and it is the JFrog copy Xray has indexed. Scoping to
// the source finds no scanner at all and reports every release as "not
// configured", on an estate where Xray is switched on and working.
//
// That was the first shape of this and it was wrong on the only topology that
// matters. regclient.SecurityRepositoryFor holds the ordering; this supplies the
// one fact it cannot know, which is where the release has actually been.
func (s *Server) securityTargetFor(
	ctx context.Context, productName string, pkg store.PackageRow,
) securityTarget {
	p, ok := s.deps.Products.Get(productName)
	if !ok {
		return securityTarget{Reason: "This product is not configured on this Coordinator."}
	}

	chosen, ok := regclient.SecurityRepositoryFor(p, s.reachedTargets(ctx, pkg))
	if !ok {
		return securityTarget{Reason: fmt.Sprintf(
			"No repository of %q has vulnerability scanning switched on. "+
				"Set `type: jfrog` and `xray.enabled: true` on the JFrog repository this release lands in.",
			productName)}
	}

	return securityTarget{
		Available: true,
		Scope: security.Scope{
			Product:    productName,
			Repository: chosen.Name,
			Role:       string(chosen.Role),
			Provider:   "jfrog-xray",
		},
	}
}

// reachedTargets names the destinations a release has actually been transferred
// to, so the scanner is asked about a copy that exists.
func (s *Server) reachedTargets(ctx context.Context, pkg store.PackageRow) []string {
	if s.deps.Packages == nil {
		return nil
	}
	transfers, err := s.deps.Packages.ListTransfers(ctx, store.ListTransfersFilter{
		PackageID: pkg.ID, Limit: 50,
	})
	if err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, t := range transfers {
		// Only a transfer that finished put anything there. A failed one left
		// the destination empty, and asking a scanner about an artifact that
		// was never pushed produces "not indexed" - which reads as a scanning
		// problem rather than as a transfer that did not happen.
		if !strings.EqualFold(t.State, "succeeded") {
			continue
		}
		name := targetName(t)
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// securityArtifactsFor turns a release's stored artifact rows into scannable
// references.
//
// # Why the release's own manifest is added last, and only if unclaimed
//
// A single-artifact release's root IS its one artifact, so the root and the row
// carry the same digest. Sending both asks the scanner about it twice and
// counts every finding twice. Deduplicating alone is not enough, because which
// of the two survives decides what the artifact is CALLED - and the root is
// named after the release, whose name changes every release. Aligning two
// releases on that name reports every artifact as removed and every artifact as
// added, which is a comparison containing no information. So the named row wins.
func (s *Server) securityArtifactsFor(
	productName string, pkg store.PackageRow, ctx context.Context,
) []security.ArtifactRef {
	rows, err := s.deps.Packages.ListArtifacts(ctx, pkg.ID)
	if err != nil {
		return nil
	}

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

	if len(out) > maxSecurityArtifacts {
		out = out[:maxSecurityArtifacts]
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

// securityTTL resolves the retention the repository's configuration asks for.
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
		// search box has said what they mean, and asking them to choose a radio
		// button first is a worse interface than reading it.
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
		return "Recording findings"
	case security.StageComparing:
		return "Comparing the two releases"
	case security.StageExporting:
		return "Preparing the export"
	default:
		if name == "" {
			return "Working"
		}
		return strings.ToUpper(name[:1]) + name[1:]
	}
}

const rfc3339 = "2006-01-02T15:04:05Z07:00"
