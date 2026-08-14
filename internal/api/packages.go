package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/abhijeet-oxide/softwareGateway/internal/compare"
	"github.com/abhijeet-oxide/softwareGateway/internal/discovery"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// defaultPageSize and maxPageSize bound a list response.
const (
	defaultPageSize = 50
	maxPageSize     = 500
)

// handleListUnavailable serves GET /api/v1/products/{product}/unavailable.
//
// What a source would not serve us, and why. Its own route rather than a filter
// on the packages listing, because these are the ABSENCE of packages: giving
// them a state on the package list would make every consumer of that list
// remember to exclude them.
func (s *Server) handleListUnavailable(w http.ResponseWriter, r *http.Request) {
	productName := chi.URLParam(r, "product")
	if !s.productExists(w, r, productName) {
		return
	}
	if s.deps.Packages == nil {
		Error(w, r, v1.CodeUnavailable, "package storage is not configured")
		return
	}

	pageSize, err := parsePageSize(r.URL.Query().Get("pageSize"))
	if err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}

	rows, err := s.deps.Packages.ListUnavailable(r.Context(), productName, pageSize)
	if err != nil {
		Error(w, r, v1.CodeUnavailable, "could not list unavailable packages: "+err.Error())
		return
	}

	out := v1.ListUnavailableResponse{Packages: make([]v1.UnavailablePackage, 0, len(rows))}
	for _, u := range rows {
		out.Packages = append(out.Packages, v1.UnavailablePackage{
			Repository:        u.Repository,
			Tag:               u.Tag,
			DisplayRepository: u.DisplayRepository,
			DisplayTag:        u.DisplayTag,
			Reason:            u.Reason,
			Detail:            u.Detail,
			FirstSeenAt:       u.FirstSeenAt,
			LastSeenAt:        u.LastSeenAt,
		})
	}
	WriteJSON(w, r, http.StatusOK, out)
}

// handleListPackages serves GET /api/v1/products/{product}/packages.
func (s *Server) handleListPackages(w http.ResponseWriter, r *http.Request) {
	productName := chi.URLParam(r, "product")
	if !s.productExists(w, r, productName) {
		return
	}
	if s.deps.Packages == nil {
		Error(w, r, v1.CodeUnavailable, "package storage is not configured")
		return
	}

	q := r.URL.Query()
	pageSize, err := parsePageSize(q.Get("pageSize"))
	if err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}
	offset, err := parseOffset(q.Get("pageToken"))
	if err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}

	state, err := parsePackageState(q.Get("state"))
	if err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}

	// One row over the page size, so "is there another page" is answered
	// without a second COUNT query — and without claiming a next page that
	// turns out to be empty.
	rows, err := s.deps.Packages.ListPackages(r.Context(), store.ListPackagesFilter{
		ProductName:        productName,
		Repository:         q.Get("repository"),
		Tag:                q.Get("tag"),
		State:              state,
		IncludeAccessories: q.Get("includeAccessories") == "true",
		Limit:              pageSize + 1,
		Offset:             offset,
	})
	if err != nil {
		s.internal(w, r, "list packages", err)
		return
	}

	resp := v1.ListPackagesResponse{Packages: []v1.Package{}}
	if len(rows) > pageSize {
		rows = rows[:pageSize]
		resp.NextPageToken = strconv.Itoa(offset + pageSize)
	}
	for _, row := range rows {
		resp.Packages = append(resp.Packages, toAPIPackage(productName, row))
	}

	WriteJSON(w, r, http.StatusOK, resp)
}

// handleGetPackage serves GET /api/v1/products/{product}/packages/{package}.
//
// The package reference is a tag or a digest.
func (s *Server) handleGetPackage(w http.ResponseWriter, r *http.Request) {
	productName := chi.URLParam(r, "product")
	ref := chi.URLParam(r, "package")

	if !s.productExists(w, r, productName) {
		return
	}
	if s.deps.Packages == nil {
		Error(w, r, v1.CodeUnavailable, "package storage is not configured")
		return
	}

	row, ok := s.resolvePackage(w, r, productName, ref)
	if !ok {
		return
	}

	pkg := toAPIPackage(productName, row)

	// Related artifacts are loaded on the SINGLE-package read only, never in a
	// listing: a page of fifty packages would be fifty extra queries to render
	// a column that shows a count. `signatureStatus` is on the row itself and
	// is what a listing actually needs.
	rels, err := s.deps.Packages.ListRelations(r.Context(), row.ID)
	if err != nil {
		s.internal(w, r, "list related artifacts", err)
		return
	}
	for _, rel := range rels {
		related := v1.RelatedArtifact{
			Role:        strings.ToUpper(rel.Role),
			Digest:      rel.Digest,
			Tag:         rel.Tag,
			MediaType:   rel.MediaType,
			SizeBytes:   v1.Int64String(strconv.FormatInt(rel.SizeBytes, 10)),
			Annotations: rel.Annotations,
			ResolvedAt:  rel.ResolvedAt,
		}
		if rel.BlobDigest != "" {
			related.BlobDigest = rel.BlobDigest
			related.BlobMediaType = rel.BlobMediaType
			related.BlobSize = v1.Int64String(strconv.FormatInt(rel.BlobSize, 10))
		}
		pkg.Related = append(pkg.Related, related)
	}

	WriteJSON(w, r, http.StatusOK, pkg)
}

// handleListArtifacts serves
// GET /api/v1/products/{product}/packages/{package}/artifacts.
func (s *Server) handleListArtifacts(w http.ResponseWriter, r *http.Request) {
	productName := chi.URLParam(r, "product")
	ref := chi.URLParam(r, "package")

	if !s.productExists(w, r, productName) {
		return
	}
	if s.deps.Packages == nil {
		Error(w, r, v1.CodeUnavailable, "package storage is not configured")
		return
	}

	pkg, ok := s.resolvePackage(w, r, productName, ref)
	if !ok {
		return
	}

	rows, err := s.deps.Packages.ListArtifacts(r.Context(), pkg.ID)
	if err != nil {
		s.internal(w, r, "list artifacts", err)
		return
	}

	resp := v1.ListArtifactsResponse{Artifacts: []v1.Artifact{}}
	for _, a := range rows {
		artifact := v1.Artifact{
			ArtifactID:   strconv.FormatInt(a.ID, 10),
			Digest:       a.Digest,
			MediaType:    a.MediaType,
			ArtifactType: a.ArtifactType,
			SizeBytes:    v1.Int64String(strconv.FormatInt(a.SizeBytes, 10)),
			Platform:     a.Platform,
			Depth:        a.Depth,
			Fetched:      a.Fetched,
			Cached:       a.Cached,
			Annotations:  a.Annotations,
		}
		if a.ParentID != nil {
			artifact.ParentID = strconv.FormatInt(*a.ParentID, 10)
		}
		resp.Artifacts = append(resp.Artifacts, artifact)
	}

	WriteJSON(w, r, http.StatusOK, resp)
}

// handleDiscoverPackages serves
// POST /api/v1/products/{product}/packages:discover.
//
// An AIP-136 custom method: a colon, because triggering a scan is a verb with
// side effects rather than a resource. Idempotent and safe — it is the same
// scan the loop runs, and concurrent triggers collapse into the running scan
// rather than starting a second one.
func (s *Server) handleDiscoverPackages(w http.ResponseWriter, r *http.Request) {
	productName := chi.URLParam(r, "product")
	if !s.productExists(w, r, productName) {
		return
	}

	if s.deps.Discovery == nil || !s.deps.Discovery.Running() {
		// Not an error in the caller's request. Discovery runs on the leader
		// only, so a follower answering "not here" with the reason is more
		// useful than a 500 or a silent no-op.
		Error(w, r, v1.CodeFailedPrecondition,
			"discovery is not running on this replica: it runs on the leader, "+
				"and this replica is a follower or discovery is disabled")
		return
	}

	var body v1.DiscoverPackagesRequest
	if err := decodeOptionalJSON(r, &body); err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}

	if !body.ShouldWait() {
		// Start and return. The caller polls GET .../discovery for progress.
		started, running, err := s.deps.Discovery.StartProduct(productName)
		if errors.Is(err, discovery.ErrNoSuchSource) {
			Error(w, r, v1.CodeNotFound, err.Error())
			return
		}
		if err != nil {
			Error(w, r, v1.CodeUnavailable, err.Error())
			return
		}
		WriteJSON(w, r, http.StatusOK, v1.DiscoverPackagesResponse{
			Started: &v1.DiscoverStarted{Sources: started, AlreadyRunning: running},
		})
		return
	}

	var (
		res discovery.ScanResult
		err error
	)
	if body.Source != "" {
		res, err = s.deps.Discovery.Trigger(r.Context(), productName, body.Source)
	} else {
		res, err = s.deps.Discovery.TriggerProduct(r.Context(), productName)
	}

	if errors.Is(err, discovery.ErrNoSuchSource) {
		Error(w, r, v1.CodeNotFound, err.Error())
		return
	}
	if err != nil {
		// The scan failed against the registry. That is a real outcome worth
		// reporting as such: the source is still enabled and will retry.
		Error(w, r, v1.CodeUnavailable, "discovery scan failed: "+err.Error())
		return
	}

	resp := v1.DiscoverPackagesResponse{
		Repositories:            res.Repositories,
		RepositoriesFromCatalog: res.RepositoriesFromCatalog,
		RepositoriesFiltered:    res.RepositoriesFiltered,

		TagsListed:         res.TagsListed,
		TagsAdmitted:       res.TagsAdmitted,
		PackagesDiscovered: res.New,
		Superseded:         res.Superseded,
		RequestsCreated:    res.Requests,
		Renamed:            res.Renamed,
		Regrouped:          res.Regrouped,
		DurationMs:         res.Duration.Milliseconds(),
		Collapsed:          res.Collapsed,
	}
	if v := res.Vocabulary; v.Units != "" {
		resp.Vocabulary = &v1.ScanVocabulary{
			Unit: v.Unit, Units: v.Units, Version: v.Version, Versions: v.Versions,
		}
	}
	for _, te := range res.TagErrors {
		resp.TagErrors = append(resp.TagErrors, v1.ScanIssue{
			Repository:        te.Repository,
			Tag:               te.Tag,
			DisplayRepository: te.DisplayRepository,
			DisplayTag:        te.DisplayTag,
			Class:             te.Class,
			Message:           te.Error(),
		})
	}
	for _, re := range res.RepositoryErrors {
		resp.RepositoryErrors = append(resp.RepositoryErrors, re.Error())
	}

	WriteJSON(w, r, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// productExists reports whether the named product is configured, writing a 404
// if not.
func (s *Server) productExists(w http.ResponseWriter, r *http.Request, name string) bool {
	if s.deps.Products == nil {
		NotFound(w, r, "product", name)
		return false
	}
	if _, ok := s.deps.Products.Get(name); !ok {
		// A product that FAILED TO LOAD is not the same as one that does not
		// exist, and reporting both as NOT_FOUND sent operators looking for a
		// typo in the name. It happens routinely while someone is editing the
		// document: the watcher reloads a half-written file, validation fails,
		// and the product drops out of the registry until the next save.
		for _, bad := range s.deps.Products.Invalid() {
			if bad.Name != name {
				continue
			}
			Error(w, r, v1.CodeFailedPrecondition,
				fmt.Sprintf("product %q exists but failed to load, so it is not running: %s "+
					"(from %s). Fix the document and it will be picked up on the next reload",
					name, bad.Err, bad.File))
			return false
		}
		NotFound(w, r, "product", name)
		return false
	}
	return true
}

// internal logs the cause and returns a generic problem.
//
// The detail is deliberately not the error string: a database error can carry
// a query, a hostname or a constraint name, none of which belong in a response
// to a caller who may not be an operator.
func (s *Server) internal(w http.ResponseWriter, r *http.Request, op string, err error) {
	s.deps.Logger.ErrorContext(r.Context(), "request failed", "op", op, "error", err)
	Error(w, r, v1.CodeInternal, "the request could not be completed")
}

func parsePageSize(v string) (int, error) {
	if v == "" {
		return defaultPageSize, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, errors.New("pageSize must be a non-negative integer")
	}
	if n == 0 {
		return defaultPageSize, nil
	}
	if n > maxPageSize {
		return maxPageSize, nil
	}
	return n, nil
}

// parseOffset reads the page token.
//
// The token is an offset rendered as a string, and it is opaque BY CONTRACT
// even though it is currently readable: clients must round-trip it rather than
// compute it, so the scheme can become a keyset cursor later without a breaking
// change.
func parseOffset(token string) (int, error) {
	if token == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(token)
	if err != nil || n < 0 {
		return 0, errors.New("pageToken is not valid: pass back the nextPageToken from the previous response")
	}
	return n, nil
}

// parsePackageState converts a wire enum to the stored value.
func parsePackageState(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	// AIP-126: the wire form is SCREAMING_SNAKE_CASE; the column is lower.
	lower := strings.ToLower(v)
	for _, valid := range []string{
		"discovered", "queued", "transferring", "transferred",
		"verifying", "verified", "verification_failed", "failed", "superseded",
	} {
		if lower == valid {
			return lower, nil
		}
	}
	return "", errors.New("state " + v + " is not a valid package state")
}

// decodeOptionalJSON decodes a request body that may legitimately be absent.
func decodeOptionalJSON(r *http.Request, out any) error {
	if r.Body == nil {
		return nil
	}
	// Bounded: this body is a handful of fields, and an unbounded read is a
	// trivial way to exhaust memory.
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		return errors.New("request body could not be read")
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}

	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return errors.New("request body is not valid JSON: " + err.Error())
	}
	return nil
}

// toAPIPackage converts a stored row to the wire view.
func toAPIPackage(productName string, row store.PackageRow) v1.Package {
	p := v1.Package{
		Name:              "products/" + productName + "/packages/" + strconv.FormatInt(row.ID, 10),
		PackageID:         strconv.FormatInt(row.ID, 10),
		Product:           productName,
		Tag:               row.Tag,
		ManifestDigest:    row.ManifestDigest,
		MediaType:         row.MediaType,
		ArtifactCount:     row.ArtifactCount,
		BlobCount:         row.BlobCount,
		State:             v1.PackageState(strings.ToUpper(row.State)),
		DiscoveredAt:      row.DiscoveredAt,
		SourceRepository:  row.SourceRepository,
		DisplayRepository: row.DisplayRepository,
		DisplayTag:        row.DisplayTag,
		SignatureStatus:   v1.SignatureStatus(strings.ToUpper(row.SignatureStatus)),
		TransferRootTag:   row.TransferRootTag,
	}
	if row.ExpandedAt != nil {
		p.ExpandedAt = *row.ExpandedAt
	}
	if p.SignatureStatus == "" {
		p.SignatureStatus = v1.SignatureUnknown
	}
	if row.PublishedAt != nil {
		p.PublishedAt = *row.PublishedAt
	}
	if row.TotalBytes != nil {
		v := v1.Int64String(strconv.FormatInt(*row.TotalBytes, 10))
		p.TotalBytes = &v
	}
	if row.SupersededBy != nil {
		p.SupersededBy = strconv.FormatInt(*row.SupersededBy, 10)
	}
	if row.AccessoryOf != nil {
		p.AccessoryOf = strconv.FormatInt(*row.AccessoryOf, 10)
	}
	return p
}

// GET /api/v1/products/{product}/discovery.
//
// What discovery is doing right now, for this product. A read, not a verb, so
// it is safe to poll at one-second intervals while a scan runs — which is
// exactly what transferctl does, because a synchronous scan against a slow
// registry otherwise shows a blank terminal for minutes and then a timeout.
func (s *Server) handleDiscoveryStatus(w http.ResponseWriter, r *http.Request) {
	productName := chi.URLParam(r, "product")
	if !s.productExists(w, r, productName) {
		return
	}

	resp := v1.DiscoveryStatusResponse{
		Running: s.deps.Discovery != nil && s.deps.Discovery.Running(),
		Sources: []v1.DiscoverySourceState{},
	}
	if !resp.Running {
		// Not an error: discovery runs on the leader, and a follower answering
		// "not here" is more useful than a 503.
		WriteJSON(w, r, http.StatusOK, resp)
		return
	}

	for _, sp := range s.deps.Discovery.Progress(productName) {
		state := v1.DiscoverySourceState{
			Product:         sp.Product,
			Source:          sp.Source,
			IntervalSeconds: int(sp.Interval.Seconds()),
		}

		if p := sp.Progress; p.Running() {
			state.Scanning = true
			state.Phase = string(p.Phase)
			state.ElapsedMs = p.Elapsed().Milliseconds()
			state.RepositoriesTotal = p.RepositoriesTotal
			state.RepositoriesDone = p.RepositoriesDone
			state.RepositoriesInFlight = p.RepositoriesInFlight
			state.CurrentRepository = p.CurrentRepository
			state.TagsTotal = p.TagsTotal
			state.TagsResolved = p.TagsResolved
			state.Artifacts = p.Artifacts
			state.NewPackages = p.New
			state.Errors = p.Errors
		}

		if !sp.LastRun.IsZero() {
			state.LastRunAt = sp.LastRun.UTC().Format(time.RFC3339)
			state.LastRepositories = sp.Last.Repositories
			state.LastTagsListed = sp.Last.TagsListed
			state.LastNewPackages = sp.Last.New
			state.LastDurationMs = sp.Last.Duration.Milliseconds()
		}
		state.LastError = sp.LastErr

		resp.Sources = append(resp.Sources, state)
	}

	WriteJSON(w, r, http.StatusOK, resp)
}

// resolvePackage finds one package, or explains why it cannot.
//
// Centralised because every package endpoint needs the same three outcomes and
// a handler that quietly picked the first match would be wrong in a way nobody
// would report — the caller gets a real package and believes it asked for that
// one.
func (s *Server) resolvePackage(w http.ResponseWriter, r *http.Request, productName, ref string) (store.PackageRow, bool) {
	// `?repository=` scopes the lookup, as does the `repo:tag` form of the
	// reference itself. Either is enough; both agreeing is fine.
	parsed := store.ParsePackageRef(ref)
	if repo := strings.TrimSpace(r.URL.Query().Get("repository")); repo != "" {
		parsed.Repository = strings.Trim(repo, "/")
	}

	pkg, err := s.deps.Packages.GetPackageRef(r.Context(), productName, parsed)

	var (
		ambiguous  *store.AmbiguousReferenceError
		ambiguousR *store.AmbiguousRepositoryError
	)
	switch {
	case errors.As(err, &ambiguousR):
		Error(w, r, v1.CodeInvalidArgument, fmt.Sprintf(
			"%s. Name one in full, for example: %s:%s",
			ambiguousR.Error(), ambiguousR.Paths[0], parsed.Tag))
		return store.PackageRow{}, false
	case errors.As(err, &ambiguous):
		// The scoped form is quoted rather than the `?repository=` query
		// parameter, because this message is read at a terminal far more often
		// than in a response body, and a CLI user has nowhere to put a query
		// parameter. Both work; only one of them is advice a person can follow
		// without translating it first.
		Error(w, r, v1.CodeInvalidArgument, fmt.Sprintf(
			"%s. Name one, for example: %s:%s",
			ambiguous.Error(), ambiguous.Repositories[0], parsed.Tag))
		return store.PackageRow{}, false
	case errors.Is(err, store.ErrNotFound):
		// "package not found" on its own reads as "the package is gone", when
		// the overwhelmingly likelier cause is a reference that was never going
		// to match — a product name typed where a tag belongs, or a version
		// without the vendor's prefix. Saying what a reference IS, and where the
		// list is, turns three attempts into one.
		Error(w, r, v1.CodeNotFound, fmt.Sprintf(
			"no package of product %q matches %q. A package is identified by a TAG, "+
				"a digest, or repository:tag — for example orb_23.8.1076, "+
				"sha256:ccbd…, or orbs/cfx-5000-k8s:orb_23.8.1076. "+
				"`transferctl packages list %s` shows what there is",
			productName, ref, productName))
		return store.PackageRow{}, false
	case err != nil:
		s.internal(w, r, "get package", err)
		return store.PackageRow{}, false
	}
	return pkg, true
}

// The custom methods a package accepts.
const (
	packageVerbInspect = "inspect"
	packageVerbCompare = "compare"
)

// handlePackageCustomMethod dispatches POST /packages/{package}:<verb>.
//
// The router binds `{package}` to the WHOLE segment — reference and verb — and
// the split happens here. See the registration in router.go for why: a chi
// partial-segment pattern splits on the first colon, and a digest reference
// contains one, so `sha256:ccbd…:inspect` never reached the inspect handler and
// came back as "POST is not supported".
//
// Splitting at the LAST colon is unambiguous for every reference form we
// accept. A tag may not contain a colon; a digest contains exactly one, and it
// is not the last when a verb follows. The repository half of a scoped
// reference never appears in the path at all — it travels as `?repository=`,
// because a slash cannot survive a path segment.
func (s *Server) handlePackageCustomMethod(w http.ResponseWriter, r *http.Request) {
	segment := chi.URLParam(r, "package")

	i := strings.LastIndex(segment, ":")
	if i <= 0 || i == len(segment)-1 {
		Error(w, r, v1.CodeInvalidArgument, fmt.Sprintf(
			"POST on a package requires a custom method: %s:%s", segment, packageVerbInspect))
		return
	}
	ref, verb := segment[:i], segment[i+1:]

	if verb != packageVerbInspect && verb != packageVerbCompare {
		Error(w, r, v1.CodeInvalidArgument, fmt.Sprintf(
			"%q is not a custom method on a package (known: %s, %s)",
			verb, packageVerbInspect, packageVerbCompare))
		return
	}

	// Put the reference back where the rest of the handler chain expects it, so
	// resolvePackage sees `sha256:ccbd…` rather than `sha256:ccbd…:inspect`.
	if rc := chi.RouteContext(r.Context()); rc != nil {
		for k := range rc.URLParams.Keys {
			if rc.URLParams.Keys[k] == "package" {
				rc.URLParams.Values[k] = ref
			}
		}
	}
	if verb == packageVerbCompare {
		s.handleComparePackage(w, r)
		return
	}
	s.handleInspectPackage(w, r)
}

// POST /api/v1/products/{product}/packages/{package}:compare.
//
// Walks TWO places and reports what is different. The package in the path is
// the first end; the body names the second.
//
// It reads no transfer record, and that is the whole point: a transfer can
// report every job successful and leave the destination wrong — a tag that
// failed to apply, content deleted afterwards, a bundle assembled by two
// transfers of which one was stopped. Each of those was seen in practice.
//
// Both ends are symmetric, so source-against-target, target-against-target and
// one-place-at-two-versions are the same request with different arguments.
func (s *Server) handleComparePackage(w http.ResponseWriter, r *http.Request) {
	productName := chi.URLParam(r, "product")
	if !s.productExists(w, r, productName) {
		return
	}
	if s.deps.Comparer == nil {
		Error(w, r, v1.CodeUnavailable,
			"comparing reads both registries, and the client for them is not "+
				"configured on this replica")
		return
	}

	var req v1.CompareRequest
	if r.ContentLength > 0 && !decodeJSON(w, r, &req) {
		return
	}

	pkg, ok := s.resolvePackage(w, r, productName, chi.URLParam(r, "package"))
	if !ok {
		return
	}

	// The second end's package. The same one unless `against` names another,
	// which is what turns a place-to-place comparison into a version-to-version
	// one without a second endpoint or a second shape.
	against := pkg
	if req.Against != "" {
		if against, ok = s.resolvePackage(w, r, productName, req.Against); !ok {
			return
		}
	}

	report, err := s.deps.Comparer.Compare(r.Context(), productName,
		ComparePoint{Package: pkg, Endpoint: req.From},
		ComparePoint{Package: against, Endpoint: req.To})
	if err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}

	out := v1.CompareResponse{
		Product:    productName,
		A:          compareEndDTO(report.A),
		B:          compareEndDTO(report.B),
		Rows:       make([]v1.CompareRow, 0, len(report.Rows)),
		Same:       report.Same,
		Changed:    report.Changed,
		OnlyA:      report.OnlyA,
		OnlyB:      report.OnlyB,
		ExtraTagsA: report.ExtraTagsA,
		ExtraTagsB: report.ExtraTagsB,
	}
	for _, row := range report.Rows {
		out.Rows = append(out.Rows, v1.CompareRow{
			Type:         row.Type,
			Name:         row.Name,
			Verdict:      string(row.Verdict),
			A:            compareSideDTO(row.A),
			B:            compareSideDTO(row.B),
			Differences:  row.Differences,
			FilesAdded:   row.FilesAdded,
			FilesRemoved: row.FilesRemoved,
		})
	}
	WriteJSON(w, r, http.StatusOK, out)
}

func compareEndDTO(s compare.SideSpec) v1.CompareEnd {
	return v1.CompareEnd{Label: s.Label, Reference: s.String()}
}

// compareSideDTO renders one end's account of a component, or nothing when that
// end does not have it.
//
// Nil rather than a zero struct with `present: false`, because "this side does
// not have this component" is the finding, and a client should not have to read
// a boolean to discover it.
func compareSideDTO(item *compare.Item) *v1.CompareSide {
	if item == nil {
		return nil
	}
	out := &v1.CompareSide{
		Digest:     item.Digest,
		Tag:        item.Tag,
		Size:       v1.Int64String(strconv.FormatInt(item.Size, 10)),
		Repository: item.Repository,
	}
	if item.Named != nil {
		out.NamedRepository = item.Named.Repository
		out.NamedPresent = item.Named.Present
		out.NamedTagDigest = item.Named.TagDigest
	}
	return out
}

// POST /api/v1/products/{product}/packages/{package}:inspect.
//
// Expands one package's manifest tree: fetches the artifacts discovery only
// listed, records their blobs, and measures the transfer size.
//
// Discovery deliberately stops at the tag's own manifest — it answers "what is
// new", and the root digest immutably determines everything beneath it, so the
// rest is recoverable whenever it is wanted (docs/design/07 §12). This is where
// it is wanted, and it is the same walk a transfer performs, so these numbers
// are the numbers a transfer will move.
//
// Idempotent: the tree under a digest cannot change, so a second call fetches
// nothing and says so.
func (s *Server) handleInspectPackage(w http.ResponseWriter, r *http.Request) {
	productName := chi.URLParam(r, "product")
	if !s.productExists(w, r, productName) {
		return
	}
	ref := chi.URLParam(r, "package")

	pkg, ok := s.resolvePackage(w, r, productName, ref)
	if !ok {
		return
	}

	if s.deps.Discovery == nil || !s.deps.Discovery.Running() {
		// Inspect reaches the registry through the source's configured client,
		// which only the leader holds. Saying so beats a 500.
		Error(w, r, v1.CodeFailedPrecondition,
			"inspecting a package reads from its source registry, and the client for "+
				"that source runs on the leader: this replica is a follower, or "+
				"discovery is disabled")
		return
	}

	res, err := s.deps.Discovery.InspectPackage(r.Context(), s.deps.Packages, pkg, productName)
	if errors.Is(err, discovery.ErrNotInspectable) {
		Error(w, r, v1.CodeFailedPrecondition, err.Error())
		return
	}
	if err != nil {
		Error(w, r, v1.CodeUnavailable, "could not expand the package: "+err.Error())
		return
	}

	WriteJSON(w, r, http.StatusOK, v1.InspectPackageResponse{
		Package:         toAPIPackage(productName, res.Package),
		Fetched:         res.Fetched,
		AlreadyExpanded: res.AlreadyExpanded,
		Artifacts:       res.Artifacts,
		Blobs:           res.Blobs,
		TotalBytes:      v1.Int64String(strconv.FormatInt(res.TotalBytes, 10)),
		CachedManifests: res.CachedManifests,
		CachedBytes:     v1.Int64String(strconv.FormatInt(res.CachedBytes, 10)),

		SignatureResolved: res.SignatureResolved,
	})
}

// POST /api/v1/products:discover.
//
// Scans every product discovery is polling. The definition of "everything"
// comes from the loop rather than from the caller, so it cannot disagree with
// which products are actually enabled.
//
// Always non-blocking. A fleet-wide scan against slow registries is minutes to
// hours of work, and holding an HTTP request open for it would make every
// intermediary's idle timeout part of the control plane. Progress comes from
// GET .../discovery per product.
func (s *Server) handleDiscoverAll(w http.ResponseWriter, r *http.Request) {
	if s.deps.Discovery == nil || !s.deps.Discovery.Running() {
		Error(w, r, v1.CodeFailedPrecondition,
			"discovery is not running on this replica: it runs on the leader, "+
				"and this replica is a follower or discovery is disabled")
		return
	}

	products := s.deps.Discovery.Products()
	resp := v1.DiscoverAllResponse{Products: []v1.DiscoverAllProduct{}}

	for _, name := range products {
		started, running, err := s.deps.Discovery.StartProduct(name)
		entry := v1.DiscoverAllProduct{
			Product: name, Sources: started, AlreadyRunning: running,
		}
		if err != nil {
			// Reported per product rather than failing the whole call. One
			// product's broken source must not stop the other thirty being
			// scanned — the same rule discovery itself follows.
			entry.Error = err.Error()
		}
		resp.Products = append(resp.Products, entry)
		resp.Started += started
		resp.AlreadyRunning += running
	}

	WriteJSON(w, r, http.StatusOK, resp)
}
