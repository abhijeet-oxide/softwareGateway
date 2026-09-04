package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// Compliance: does this release follow the organization's own Kubernetes and
// CNF standards.
//
// # Why starting a check and reading one are different endpoints
//
// Reading is a database query and must work on a Coordinator that cannot reach
// a registry at all - which is exactly the state somebody is in when they are
// trying to find out why a release was blocked. Starting reaches a vendor
// registry, unpacks charts and shells out to helm, and takes minutes.
//
// Registering the read on the STORE and the start on the RUNNER is what keeps
// the first working when the second cannot, and it is the same split the
// security endpoints already make.

// ComplianceRunner starts and reports on compliance runs.
//
// A consumer-defined interface: four calls, not the fetcher, the renderer and
// the catalogue behind them.
type ComplianceRunner interface {
	Start(ctx context.Context, req compliance.Request) (compliance.Progress, error)
	Progress(packageID int64) (compliance.Progress, bool)
	Cancel(packageID int64) bool
}

// ComplianceStore is the stored result of runs, which is what every read
// serves.
type ComplianceStore interface {
	LatestComplianceRun(ctx context.Context, packageID int64) (store.ComplianceRunRow, error)
	ComplianceRun(ctx context.Context, id string) (store.ComplianceRunRow, error)
	ComplianceRuns(ctx context.Context, packageID int64, limit int) ([]store.ComplianceRunRow, error)
	ComplianceUniqueChecks(ctx context.Context, runID string) (store.ComplianceUniqueCounts, error)
	ComplianceCharts(ctx context.Context, runID string) ([]store.ComplianceChartRow, error)
	ComplianceResults(ctx context.Context, runID string, f store.ComplianceFilter) ([]store.ComplianceResultRow, int, error)
	PackageCompliance(ctx context.Context, ids []int64) (map[int64]store.PackageComplianceRow, error)
}

// ComplianceCatalogue serves the rulebook.
//
// A function rather than a value, because the loader swaps the catalogue when a
// policy directory changes and a handler holding the old one would serve a
// rulebook nobody is being checked against.
type ComplianceCatalogue func() *compliance.Catalog

// ComplianceHelm reports whether this Coordinator can render charts.
type ComplianceHelm func() (version string, err error)

// handlePackageCompliance serves
// GET /api/v1/products/{product}/packages/{package}/compliance.
//
// Reads stored data and nothing else, so it is fast enough to poll - which is
// what the tab does while a run is going, because the run's live position
// travels in this same response rather than on a channel of its own.
func (s *Server) handlePackageCompliance(w http.ResponseWriter, r *http.Request) {
	productName := chi.URLParam(r, "product")
	ref := chi.URLParam(r, "package")

	if !s.productExists(w, r, productName) {
		return
	}
	if s.deps.ComplianceStore == nil {
		Error(w, r, v1.CodeUnavailable, "compliance is not configured on this Coordinator")
		return
	}
	pkg, ok := s.resolvePackage(w, r, productName, ref)
	if !ok {
		return
	}

	out := PackageComplianceView{
		Product: productName,
		Release: pkg.Tag,
		Helm:    s.complianceHelm(),
	}
	// Whether a run is possible at all, before offering it. A release whose
	// manifest tree has not been walked has no layer digests to fetch, and
	// finding that out by pressing a button and reading a recorded failure is
	// a worse answer than being told.
	if s.deps.Packages != nil {
		if analysed, err := s.deps.Packages.PackageAnalysed(r.Context(), pkg.ID); err == nil {
			out.Analysed = analysed
		}
	}

	// The live position first, so a tab polling this endpoint sees the run
	// start even before the first result exists.
	if s.deps.ComplianceRunner != nil {
		if p, live := s.deps.ComplianceRunner.Progress(pkg.ID); live {
			progress := p
			out.Progress = &progress
		}
	}

	run, err := s.deps.ComplianceStore.LatestComplianceRun(r.Context(), pkg.ID)
	if errors.Is(err, store.ErrNotFound) {
		// Never checked. The absence is the answer, and the interface renders
		// it as "not checked" - never as a pass.
		WriteJSON(w, r, http.StatusOK, out)
		return
	}
	if err != nil {
		s.internal(w, r, "read compliance run", err)
		return
	}

	view := complianceRunView(run)
	// The distinct checks behind the severity counts. One extra aggregate over
	// an indexed column, and it is what the tab leads with - see
	// store.ComplianceUniqueCounts for why it cannot come from the page.
	if unique, err := s.deps.ComplianceStore.ComplianceUniqueChecks(r.Context(), run.ID); err == nil {
		view.Counts.UniqueBlocking = unique.Blocking
		view.Counts.UniqueWarning = unique.Warning
		view.Counts.UniqueInfo = unique.Info
		view.Counts.UniquePassed = unique.Passed
	}
	out.Run = &view

	charts, err := s.deps.ComplianceStore.ComplianceCharts(r.Context(), run.ID)
	if err != nil {
		s.internal(w, r, "read compliance charts", err)
		return
	}
	out.Charts = complianceChartViews(charts)

	results, total, err := s.deps.ComplianceStore.ComplianceResults(
		r.Context(), run.ID, complianceFilterFrom(r))
	if err != nil {
		s.internal(w, r, "read compliance results", err)
		return
	}
	out.Results = complianceResultViews(results)
	out.Total = total

	// private, because these are one vendor's findings under one product's
	// permissions and a shared cache must never hold them.
	w.Header().Set("Cache-Control", "private, no-store")
	WriteJSON(w, r, http.StatusOK, out)
}

// handleRunCompliance serves
// POST /api/v1/products/{product}/packages/{package}/compliance:run.
//
// Returns as soon as the claim is taken. The work continues in the background
// and the caller polls the read endpoint, because a run takes minutes and no
// browser will hold a request open for it.
func (s *Server) handleRunCompliance(w http.ResponseWriter, r *http.Request) {
	productName := chi.URLParam(r, "product")
	ref := chi.URLParam(r, "package")

	if !s.productExists(w, r, productName) {
		return
	}
	if s.deps.ComplianceRunner == nil {
		Error(w, r, v1.CodeUnavailable,
			"this Coordinator cannot run compliance checks; it is configured for reads only")
		return
	}
	pkg, ok := s.resolvePackage(w, r, productName, ref)
	if !ok {
		return
	}

	progress, err := s.deps.ComplianceRunner.Start(r.Context(), compliance.Request{
		RunID:     uuid.NewString(),
		PackageID: pkg.ID,
		Product:   productName,
		Release:   pkg.Tag,
		Digest:    pkg.ManifestDigest,
		Trigger:   "api",
	})
	switch {
	case errors.Is(err, compliance.ErrRunInFlight):
		// Not a failure to show as one: it is the honest answer to "start a
		// check" when one is running. The caller shows the run in progress.
		WriteJSON(w, r, http.StatusOK, progress)
		return
	case err != nil:
		s.internal(w, r, "start compliance run", err)
		return
	}
	WriteJSON(w, r, http.StatusAccepted, progress)
}

// handleCancelCompliance serves
// POST /api/v1/products/{product}/packages/{package}/compliance:cancel.
func (s *Server) handleCancelCompliance(w http.ResponseWriter, r *http.Request) {
	productName := chi.URLParam(r, "product")
	ref := chi.URLParam(r, "package")

	if !s.productExists(w, r, productName) {
		return
	}
	if s.deps.ComplianceRunner == nil {
		Error(w, r, v1.CodeUnavailable, "compliance is not configured on this Coordinator")
		return
	}
	pkg, ok := s.resolvePackage(w, r, productName, ref)
	if !ok {
		return
	}
	// A cancel that found nothing is not an error: the run may have finished
	// between the button and the request, which is the common case.
	WriteJSON(w, r, http.StatusOK, map[string]bool{
		"cancelled": s.deps.ComplianceRunner.Cancel(pkg.ID),
	})
}

// handleComplianceRuns serves the history of a release's runs.
//
// A failed run is in this list. "The last attempt failed at 14:22 because the
// registry refused us" is the thing an operator needs, and it is not the same
// as "this release has never been checked".
func (s *Server) handleComplianceRuns(w http.ResponseWriter, r *http.Request) {
	productName := chi.URLParam(r, "product")
	ref := chi.URLParam(r, "package")

	if !s.productExists(w, r, productName) {
		return
	}
	if s.deps.ComplianceStore == nil {
		Error(w, r, v1.CodeUnavailable, "compliance is not configured on this Coordinator")
		return
	}
	pkg, ok := s.resolvePackage(w, r, productName, ref)
	if !ok {
		return
	}

	rows, err := s.deps.ComplianceStore.ComplianceRuns(r.Context(), pkg.ID, intParam(r, "limit", 20))
	if err != nil {
		s.internal(w, r, "list compliance runs", err)
		return
	}
	out := make([]ComplianceRunView, 0, len(rows))
	for _, row := range rows {
		out = append(out, complianceRunView(row))
	}
	WriteJSON(w, r, http.StatusOK, map[string]any{"runs": out})
}

// handlePolicies serves the rulebook.
//
// Deliberately not scoped to a product or a release: it is what WILL be
// checked, and a vendor asking before they ship has no release to point at
// yet. It is also what a reviewer reads when settling an argument about a
// finding, which is why the rationale travels with it.
func (s *Server) handlePolicies(w http.ResponseWriter, r *http.Request) {
	if s.deps.ComplianceCatalogue == nil {
		Error(w, r, v1.CodeUnavailable, "no policy catalogue is loaded on this Coordinator")
		return
	}
	cat := s.deps.ComplianceCatalogue()
	if cat == nil {
		Error(w, r, v1.CodeUnavailable, "the policy catalogue has not finished loading")
		return
	}
	WriteJSON(w, r, http.StatusOK, policyCatalogueView(cat))
}

// handlePolicy serves one check in full.
func (s *Server) handlePolicy(w http.ResponseWriter, r *http.Request) {
	if s.deps.ComplianceCatalogue == nil {
		Error(w, r, v1.CodeUnavailable, "no policy catalogue is loaded on this Coordinator")
		return
	}
	cat := s.deps.ComplianceCatalogue()
	if cat == nil {
		Error(w, r, v1.CodeUnavailable, "the policy catalogue has not finished loading")
		return
	}
	id := strings.ToUpper(chi.URLParam(r, "check"))
	check, ok := cat.Check(id)
	if !ok {
		Error(w, r, v1.CodeNotFound, "no check with ID "+id+" is loaded")
		return
	}
	view := policyCatalogueView(cat)
	for _, c := range view.Checks {
		if c.ID == check.ID {
			WriteJSON(w, r, http.StatusOK, c)
			return
		}
	}
	Error(w, r, v1.CodeNotFound, "no check with ID "+id+" is loaded")
}

// complianceHelm reports whether charts can be rendered at all.
//
// On screen this is the difference between a tab full of "could not be
// checked" with no explanation and one that says helm is missing. The check is
// cheap - one subprocess that prints a version - and it is only made when
// somebody is looking.
func (s *Server) complianceHelm() ComplianceHelmView {
	if s.deps.ComplianceHelm == nil {
		return ComplianceHelmView{}
	}
	version, err := s.deps.ComplianceHelm()
	if err != nil {
		return ComplianceHelmView{Reason: err.Error()}
	}
	return ComplianceHelmView{Available: true, Version: version}
}

// complianceFilterFrom reads the result filter off the query string.
//
// The default is FAILURES AND UNDECIDED, not everything: a release produces ten
// to fifteen thousand results and the tab opens on what needs attention. The
// coverage view asks for the rest explicitly, and the counts on the run say how
// many there are either way - so the short list is never mistaken for a small
// denominator.
func complianceFilterFrom(r *http.Request) store.ComplianceFilter {
	q := r.URL.Query()
	f := store.ComplianceFilter{
		Outcomes:   commaList(q.Get("outcome")),
		Severities: commaList(q.Get("severity")),
		Checks:     commaList(q.Get("check")),
		Charts:     commaList(q.Get("chart")),
		Kinds:      commaList(q.Get("kind")),
		// The mechanism a finding is about, which is the split an engineer
		// makes first and the one the category is too coarse for.
		Subcategories: commaList(q.Get("subcategory")),
		Determinacy:   commaList(q.Get("determinacy")),
		Search:        strings.TrimSpace(q.Get("q")),
		Limit:         intParam(r, "limit", 500),
		Offset:        intParam(r, "offset", 0),
	}
	if len(f.Outcomes) == 0 && q.Get("all") != "true" {
		f.Outcomes = []string{
			string(compliance.OutcomeFail),
			string(compliance.OutcomeError),
			string(compliance.OutcomeWaived),
		}
	}
	return f
}

func commaList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func intParam(r *http.Request, name string, def int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// attachCompliance fills in each listed release's stored compliance summary.
//
// Silent on failure, deliberately, and for the same reason attachSecurity is:
// compliance is a column, and a listing that 500s because one table is
// unreadable would take the whole page down for a decoration. A missing summary
// renders as "not checked", which is honest - and which is NOT the same as
// compliant, a distinction the client is required to keep.
func (s *Server) attachCompliance(ctx context.Context, rows []store.PackageRow, out []v1.Package) {
	if s.deps.ComplianceStore == nil || len(rows) == 0 {
		return
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	found, err := s.deps.ComplianceStore.PackageCompliance(ctx, ids)
	if err != nil {
		if s.deps.Logger != nil {
			s.deps.Logger.Warn("could not read package compliance for listing", "error", err)
		}
		return
	}

	// Whether a check can be started at all is a property of this COORDINATOR,
	// not of a release, so it is resolved once rather than per row.
	canRun := s.deps.ComplianceRunner != nil
	reason := ""
	if !canRun {
		reason = "this Coordinator is configured for compliance reads only"
	}

	for i, row := range rows {
		summary := &v1.PackageComplianceSummary{CanRun: canRun, Reason: reason}
		if c, ok := found[row.ID]; ok {
			view := complianceListingView(c)
			summary.State = view.State
			summary.Verdict = view.Verdict
			summary.Label = view.Label
			summary.Blocking = view.Blocking
			summary.Warning = view.Warning
			summary.Error = view.Error
			summary.Pass = view.Pass
			summary.UniqueBlocking = view.UniqueBlocking
			summary.UniqueWarning = view.UniqueWarning
			if view.CheckedAt != nil {
				summary.CheckedAt = view.CheckedAt.UTC().Format(rfc3339)
			}
		}
		// A summary is sent even for a release nobody has checked, because the
		// listing has to offer the action and a nil would leave the cell with
		// nothing to render.
		out[i].Compliance = summary
	}
}
