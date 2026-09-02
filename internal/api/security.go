package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"golang.org/x/sync/errgroup"

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
	// Cancel stops a sync, whether or not this replica is the one running it.
	Cancel(ctx context.Context, packageID int64) (bool, error)
}

// SecurityStore is the stored result of syncs, which is what every read serves.
type SecurityStore interface {
	Get(ctx context.Context, packageID int64) (store.PackageSecurityRow, bool, error)
	ForPackages(ctx context.Context, ids []int64) (map[int64]store.PackageSecurityRow, error)
	// ReportsFor reads stored reports. The Detail argument says whether the
	// prose tier is wanted: a reader that only counts and classifies must not
	// pay to decompress it (see security.Detail).
	ReportsFor(
		ctx context.Context, scope security.Scope,
		refs []security.ArtifactRef, detail security.Detail,
	) ([]security.Report, error)
	// LoadDocuments returns the scanner's own bodies, for a bundle export and
	// for the download beside an image.
	//
	// Kinds are a filter rather than a fetch-everything: the SBOM is the
	// largest thing this store holds, and a page that only wants to know
	// whether one exists must not pull tens of megabytes per image to find out.
	LoadDocuments(
		ctx context.Context, scope security.Scope,
		refs []security.ArtifactRef, kinds []security.DocumentKind,
	) (map[string]map[security.DocumentKind]security.Document, error)
	// DocumentSummaries answers "what is held" without reading any payload.
	DocumentSummaries(
		ctx context.Context, scope security.Scope, refs []security.ArtifactRef,
	) (map[string][]security.DocumentSummary, error)
	// LoadSources reads a release's per-scanner rows.
	//
	// A separate call rather than a field on the row, because a listing renders
	// twenty releases and joining a second table for a breakdown that is empty
	// on every single-scanner deployment would be twenty joins for nothing.
	LoadSources(ctx context.Context, packageID int64) ([]security.SourceCounts, error)
}

// SecurityDocuments retrieves the scanner's own bodies, fetching what is not
// already held.
//
// Separate from SecurityStore because it can reach a scanner and the store
// cannot: an SBOM is not captured by a sync (tens of megabytes and minutes per
// image) and is generated when somebody presses the button.
type SecurityDocuments interface {
	Documents(ctx context.Context, req security.DocumentRequest) ([]security.Document, error)
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
	packageVerbCancelSecurity  = "cancelSecuritySync"
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
	// Add rather than Set, because this is not the only reason the answer
	// varies: the compression middleware lists Accept-Encoding here too, and a
	// handler that replaced the header would tell a shared cache it may serve
	// a gzipped body to a client that asked for plain text.
	w.Header().Add("Vary", "Authorization")

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

	out := toAPIPackageSecurity(productName, pkg, row, target, detail, s.deps.SecurityFreshness)
	out.Sync = s.syncStatusFor(pkg.ID, row, target)

	// The per-scanner breakdown, which is empty on a single-scanner deployment
	// and is what the source toggle is drawn from when it is not.
	if sources, err := s.deps.SecurityStore.LoadSources(ctx, pkg.ID); err == nil {
		for _, src := range sources {
			out.Sources = append(out.Sources, toAPISourceCounts(src))
		}
	} else {
		// A breakdown that will not load costs a control, never the page.
		s.deps.Logger.Warn("security: could not read the per-scanner breakdown",
			"package", pkg.ID, "error", err)
	}

	// Served DURING a sync as well as after one.
	//
	// This used to require a settled row, on the premise that a sync cleared
	// the per-artifact rows before refilling them - so mid-sync there was
	// nothing to send. That premise is gone: a sync overwrites each artifact as
	// its answer arrives and never deletes first, so what is stored throughout
	// is the previous sync's complete result.
	//
	// Withholding it left a reader who pressed Sync looking at three spinners
	// and an empty table for as long as the scanner took, on a release whose
	// findings were sitting in the database the whole time. The last answer is
	// the best answer until there is a better one; the interface says how old
	// it is and that a refresh is running.
	if detail && (row.Synced() || row.State == store.PackageSecuritySyncing) {
		refs := s.securityArtifactsFor(productName, pkg, ctx)
		reports, err := s.deps.SecurityStore.ReportsFor(ctx, target.Scope, refs, security.WithProse)
		if err != nil {
			return v1.PackageSecurityResponse{}, err
		}
		// What bodies are held, WITHOUT reading any of them. A release is 157
		// images times four kinds of document, and reading them to decide
		// whether to draw a download button would be hundreds of megabytes to
		// render a row of icons.
		docs, err := s.deps.SecurityStore.DocumentSummaries(ctx, target.Scope, refs)
		if err != nil {
			s.deps.Logger.Warn("security: could not list stored scanner documents",
				"package", pkg.ID, "error", err)
			docs = map[string][]security.DocumentSummary{}
		}

		posture := security.Summarize(reports)
		out.Counts = toAPICounts(posture.Counts)
		out.UniqueCounts = toAPICounts(posture.UniqueCounts)
		out.UniqueCVECounts = toAPICounts(posture.UniqueCVECounts)
		out.DistinctTotal = posture.UniqueCounts.Total
		out.DistinctCVEs = posture.UniqueCVEs
		out.Coverage = toAPICoverage(posture.Coverage)
		for _, rep := range posture.Reports {
			item := toAPIReport(rep)
			item.ScanURL = scanURL(target, rep.Artifact)
			item.Documents = documentRefsFor(productName, pkg, rep, docs[rep.Artifact.Ref()])
			out.Reports = append(out.Reports, item)
		}
	}
	return out, nil
}

// documentRefsFor is what an image's download menu offers.
//
// # Why the SBOM is offered even when nothing is held
//
// Because it is generated on demand: a sync does not fetch one (tens of
// megabytes and minutes per image), so "not held" is the ordinary state and a
// menu that hid the button would hide the feature. The other three are offered
// only when something IS held, because those ARE captured by a sync - so their
// absence means the sync did not retrieve them, and a button that fetched them
// individually would be a page that quietly re-runs part of a sync.
func documentRefsFor(
	productName string, pkg store.PackageRow, report security.Report,
	held []security.DocumentSummary,
) []v1.SecurityDocumentRef {
	byKind := map[security.DocumentKind]security.DocumentSummary{}
	for _, d := range held {
		byKind[d.Kind] = d
	}
	// The report's own list carries the messages from the sync - "this Xray
	// has no SBOM endpoint" - which the store does not keep.
	for _, d := range report.Documents {
		if existing, ok := byKind[d.Kind]; !ok || (!existing.Available && d.Message != "") {
			byKind[d.Kind] = d
		}
	}

	var out []v1.SecurityDocumentRef
	for _, kind := range security.AllDocumentKinds {
		summary, ok := byKind[kind]
		if !ok {
			if kind != security.DocumentSBOM {
				continue
			}
			summary = security.DocumentSummary{Kind: kind}
		}
		ref := toAPIDocumentRef(summary)
		ref.URL = documentURL(productName, pkg, report.Artifact, kind)
		out = append(out, ref)
	}
	return out
}

// documentURL is where one image's document is downloaded from.
//
// # The `repository` this needs, and the one it was sending
//
// It sent `target.Scope.Repository` - the CONFIGURED repository whose scanner
// answered, "cfx-jfrog-external". Every route under /packages/{package} reads
// `?repository=` as the OCI REPOSITORY PATH the release was discovered in,
// "orbs/cfx-5000-k8s-215952-ncp". They are different namespaces that happen to
// share a parameter name, so the lookup failed for every product whose releases
// span more than one repository - which is every product this feature is for -
// and the download opened a page reading "no package of product X matches Y".
//
// The package's own path is the right answer and the row is right here. The
// scanner's repository is not needed at all: the handler re-derives it from the
// release, which is also what stops a caller naming somebody else's.
func documentURL(
	productName string, pkg store.PackageRow, ref security.ArtifactRef,
	kind security.DocumentKind,
) string {
	q := url.Values{}
	q.Set("digest", ref.Digest)
	if pkg.SourceRepository != "" {
		q.Set("repository", pkg.SourceRepository)
	}
	return fmt.Sprintf("/api/v1/products/%s/packages/%s/security/documents/%s?%s",
		url.PathEscape(productName), url.PathEscape(packageReferenceOf(pkg)),
		url.PathEscape(string(kind)), q.Encode())
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

	var body v1.SyncSecurityRequest
	if err := decodeOptionalJSON(r, &body); err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}

	status, err := s.deps.SecuritySync.Start(r.Context(), security.SyncRequest{
		PackageID: pkg.ID,
		Label:     releaseLabel(pkg),
		Scope:     target.Scope,
		Artifacts: artifacts,
		// Retention is a deployment's business, not a product's - see
		// config.SecurityConfig. The zero value means "use the defaults", which
		// is right for a Coordinator that has not been told otherwise.
		TTL: s.deps.SecurityRetention,
		// Reuse what is already held and inside the age limit. Releases of one
		// product share most of their images, so this is the difference between
		// a sync that asks about 157 and one that asks about seven.
		MaxAge: s.deps.SecurityFreshness.Vulnerabilities,
		Force:  body.Force,
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

// handleCancelPackageSecuritySync serves POST
// /api/v1/products/{product}/packages/{package}:cancelSecuritySync.
//
// # Why a sync needs a stop at all
//
// Because it is minutes of somebody else's scanner, started by one button, and
// until now the only way out of one started by mistake - the wrong release, a
// scanner that is plainly down, a sync that has to make way for a more urgent
// one - was to wait it out. A job a user can start and cannot stop is a job
// they learn not to start.
//
// The claim is released here rather than in the goroutine, so a sync running on
// another replica stops too: that run notices at its next heartbeat that its
// claim has gone and stands down.
func (s *Server) handleCancelPackageSecuritySync(w http.ResponseWriter, r *http.Request) {
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

	stopped, err := s.deps.SecuritySync.Cancel(r.Context(), pkg.ID)
	if err != nil {
		s.internal(w, r, "stop security sync", err)
		return
	}

	row, _, err := s.deps.SecurityStore.Get(r.Context(), pkg.ID)
	if err != nil {
		s.internal(w, r, "read package security", err)
		return
	}

	WriteJSON(w, r, http.StatusOK, v1.CancelSecuritySyncResponse{
		Product: productName,
		Package: packageReferenceOf(pkg),
		// False is not a failure: the sync finished between the reader deciding
		// to stop it and the request arriving, which is a thing to say rather
		// than an error to raise.
		Stopped: stopped,
		Sync:    s.syncStatusFor(pkg.ID, row, s.securityTargetFor(r.Context(), productName, pkg)),
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
	if row.HeartbeatAt != nil {
		out.HeartbeatAt = row.HeartbeatAt.UTC().Format(rfc3339)
	}

	if s.deps.SecuritySync == nil {
		out.Log = toAPISyncLog(row.Log)
		out.Stalled = row.Stalled(security.StaleClaimAfter)
		return out
	}
	/*
	 * WHOSE sync this is, said plainly.
	 *
	 * A row marked `syncing` used to mean one of three things and the interface
	 * could only say the first: a sync running here, a sync running on another
	 * Coordinator, and a Coordinator that was killed mid-sync and left the row
	 * exactly as a healthy one leaves it. A reader who had just restarted their
	 * only Coordinator was told the work was happening somewhere else, and the
	 * release refused a new sync for half an hour.
	 *
	 * The heartbeat separates the second from the third, and `here` separates
	 * the first from both.
	 */
	out.Here = s.deps.SecuritySync.Running(packageID)
	out.Stalled = !out.Here && row.Stalled(security.StaleClaimAfter)
	if out.Stalled {
		out.Label = "Vulnerability sync interrupted"
	}
	if progress, ok := s.deps.SecuritySync.Progress(packageID); ok {
		stages, notes, _, _ := progress.Snapshot()
		for _, st := range stages {
			out.Stages = append(out.Stages, v1.SecurityProgressStage{
				Name: st.Name, Label: securityStageLabel(st.Name), Done: st.Done, Total: st.Total,
			})
		}
		out.Notes = notes
		// The RUNNING sync's transcript, not the last one's: a reader watching a
		// sync that is failing right now needs this minute's lines, and the
		// stored log still describes the run before it.
		out.Log = toAPISyncLog(progress.Entries())
		return out
	}
	out.Log = toAPISyncLog(row.Log)
	return out
}

func toAPISyncLog(entries []security.SyncLogEntry) []v1.SecurityLogEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]v1.SecurityLogEntry, 0, len(entries))
	for _, e := range entries {
		item := v1.SecurityLogEntry{Level: e.Level, Message: e.Message, Repeat: e.Repeat}
		if !e.At.IsZero() {
			item.At = e.At.UTC().Format(rfc3339)
		}
		out = append(out, item)
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
	// IndexOnly: a comparison is decided by identity and grade - which CVE, on
	// which component, at what severity, fixable or not. It never reads a
	// description, and reading them anyway is what made this endpoint answer in
	// minutes (see security.Detail).
	//
	// The two sides are read CONCURRENTLY. They share nothing - two disjoint
	// sets of artifacts, two independent reads - and each is most of a second
	// for a release of this size, so running them one after the other spent
	// half the endpoint's time waiting for a database that was idle.
	var sideA, sideB securitySide
	group, gctx := errgroup.WithContext(ctx)
	group.Go(func() error {
		var err error
		sideA, err = s.securitySide(gctx, productName, base, security.IndexOnly)
		return err
	})
	group.Go(func() error {
		var err error
		sideB, err = s.securitySide(gctx, productName, other, security.IndexOnly)
		return err
	})
	if err := group.Wait(); err != nil {
		return v1.SecurityComparisonResponse{}, err
	}

	cmp := security.Compare(security.CompareInput{
		A: sideA.reports, B: sideB.reports,
		NameA: releaseLabel(base), NameB: releaseLabel(other),
	})
	out := toAPISecurityComparison(productName, base, other, sideA, sideB, cmp)
	shortenChanges(&out)
	return out, nil
}

// securitySide is one end of a comparison, read from storage.
type securitySide struct {
	row     store.PackageSecurityRow
	target  securityTarget
	reports []security.Report
	posture security.Posture
}

func (s *Server) securitySide(
	ctx context.Context, productName string, pkg store.PackageRow, detail security.Detail,
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
	reports, err := s.deps.SecurityStore.ReportsFor(ctx, side.target.Scope, refs, detail)
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
	// Registry is the docker host, RepositoryKey the Artifactory repository the
	// release lands in, and Endpoint the platform base URL where one is
	// configured. Together they are what a link into JFrog's own scan view is
	// built from - from configuration, so a second deployment is not a code
	// change.
	Registry      string
	RepositoryKey string
	Endpoint      string
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
				"Set `type: jfrog` and `xrayEnabled: true` on the JFrog repository this release is replicated to.",
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
		Registry:      chosen.Registry,
		RepositoryKey: chosen.Repository,
		Endpoint:      chosen.XrayEndpoint,
	}
}

// scanURL links an image to JFrog's own scan view.
//
// Assembled from the configured platform host and repository key rather than
// written down anywhere: a second deployment is a different host and a
// different repository, and a hardcoded link would be right on exactly one
// estate. Empty when the release has not landed anywhere with a scanner, which
// is the case where there would be nothing to link to.
func scanURL(target securityTarget, ref security.ArtifactRef) string {
	base := strings.TrimSuffix(strings.TrimSpace(target.Endpoint), "/")
	if base == "" {
		if target.Registry == "" {
			return ""
		}
		base = "https://" + target.Registry
	}
	if target.RepositoryKey == "" || ref.Repository == "" || ref.Tag == "" {
		return ""
	}

	path := ref.Repository + "/" + ref.Tag
	q := url.Values{}
	q.Set("version", ref.Tag)
	q.Set("package_id", "docker://"+ref.Repository)
	q.Set("path", target.RepositoryKey+"/"+path+"/manifest.json")
	q.Set("page_type", "overview")

	return base + "/ui/scans-list/repositories/" + url.PathEscape(target.RepositoryKey) +
		"/scan-descendants/" + url.PathEscape(path) + "?" + q.Encode()
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

// securityStageLabel names a phase.
//
// Kept short and factual, because the interface renders the NUMBERS beside
// them: a label that also tried to say how far along it was would disagree
// with the counter next to it the moment one of the two was updated.
//
// And named for the OPERATION, not for the platform's side of a conversation.
// "Asking the scanner" and "Working out what to ask about" describe a piece of
// infrastructure as though it were a colleague thinking aloud; a release
// manager reading a compliance screen wants the noun for the step.
func securityStageLabel(name string) string {
	switch name {
	case security.StageResolving:
		return "Resolving artifacts"
	case security.StageFetching:
		return "Retrieving scan results"
	case security.StageCached:
		return "Reading stored results"
	case security.StageCorrelating:
		return "Recording findings"
	case security.StageFailing:
		return "Failed"
	case security.StageComparing:
		return "Comparing releases"
	case security.StageExporting:
		return "Generating export"
	default:
		if name == "" {
			return "In progress"
		}
		return strings.ToUpper(name[:1]) + name[1:]
	}
}

const rfc3339 = "2006-01-02T15:04:05Z07:00"
