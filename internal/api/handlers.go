package api

import (
	"errors"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/health"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/version"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// handleLiveness answers "is this process wedged?" and NOTHING ELSE.
//
// It must never touch the database, a registry, or any other dependency. A
// liveness probe that checked Postgres would restart every Coordinator during
// a brief database blip, converting a recoverable hiccup into a fleet-wide
// crash-loop at exactly the moment the process needs to stay alive and retry.
// The health.Registry enforces this structurally: liveness probes take no
// context and no arguments, so they have nothing to make a call with.
func (s *Server) handleLiveness(w http.ResponseWriter, r *http.Request) {
	if s.deps.Health != nil {
		if err := s.deps.Health.Live(); err != nil {
			Error(w, r, v1.CodeUnavailable, err.Error())
			return
		}
	}
	WriteJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadiness answers "should this replica receive traffic?" - the
// database and configuration, and nothing slow.
func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	if s.deps.Health == nil {
		WriteJSON(w, r, http.StatusOK, v1.ReadyResponse{Status: v1.HealthHealthy})
		return
	}

	rep := s.deps.Health.Ready(r.Context())
	resp := v1.ReadyResponse{
		Status: v1.HealthStatus(rep.Status),
		Checks: toAPIChecks(rep.Results),
	}

	status := http.StatusOK
	if rep.Status != health.StatusHealthy {
		// 503 pulls the replica out of the Service endpoints without killing
		// it, which is the whole point of separating readiness from liveness.
		status = http.StatusServiceUnavailable
	}
	WriteJSON(w, r, status, resp)
}

// handleDeepHealth validates connectivity to every configured dependency.
//
// Deliberately not what Kubernetes polls: it may be slow and may fail for
// reasons that should not affect whether we serve traffic. It backs
// `transferctl health`.
func (s *Server) handleDeepHealth(w http.ResponseWriter, r *http.Request) {
	resp := v1.HealthCheckResponse{
		Status:    v1.HealthHealthy,
		Component: s.deps.Component,
		Version:   version.Version,
	}
	if s.deps.Leader != nil {
		resp.Leader = s.deps.Leader.IsLeader()
	}
	if s.deps.Health != nil {
		rep := s.deps.Health.Deep(r.Context())
		resp.Status = v1.HealthStatus(rep.Status)
		resp.Checks = toAPIChecks(rep.Results)
	}

	// The fleet, on the report an operator already opens. A failure to read it
	// does not change the verdict: the workers are a thing this check
	// DESCRIBES, and a database hiccup listing them is already covered by the
	// database check above.
	if s.deps.Queue != nil {
		if workers, err := s.deps.Queue.Workers(r.Context()); err == nil {
			resp.Workers = toAPIWorkers(workers)
		}
	}

	// Always 200: this is a diagnostic report, and its body carries the
	// verdict. Returning 503 would make a CLI that checks status codes unable
	// to show the operator WHICH dependency is unhappy.
	WriteJSON(w, r, http.StatusOK, resp)
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	info := version.Get(s.deps.Component)
	resp := v1.VersionResponse{
		Version:    info.Version,
		Commit:     info.Commit,
		BuildDate:  info.BuildDate,
		GoVersion:  info.GoVersion,
		APIVersion: info.APIVersion,
		Component:  info.Component,
	}
	if s.deps.Store != nil {
		if v, err := s.deps.Store.SchemaVersion(r.Context()); err == nil {
			resp.SchemaVersion = v
		}
	}
	WriteJSON(w, r, http.StatusOK, resp)
}

// handleListProducts serves the loaded configuration.
//
// This works in M1 because it reads the in-memory product registry, not the
// database - configuration is GitOps-managed and read-only over the API.
func (s *Server) handleListProducts(w http.ResponseWriter, r *http.Request) {
	if s.deps.Products == nil {
		WriteJSON(w, r, http.StatusOK, v1.ListProductsResponse{Products: []v1.Product{}})
		return
	}

	// A REJECTED PRODUCT IS LISTED, NOT OMITTED.
	//
	// It used to be dropped here, which meant a product somebody had configured
	// simply was not on the screen - no row, no name, no reason, and the only
	// record of it a line in the Coordinator's log. An operator looking at that
	// page cannot tell a misconfigured product from one they never wrote, and
	// the product they are looking for is the one thing the page will not
	// mention. Every other failure in this system is a STATE with a reason
	// attached; this one was an absence.
	//
	// So the two sets are merged. The registry is fail-closed per product, so a
	// name can legitimately appear in both: a bad edit to a working product
	// leaves the previous good version running and records the rejection
	// alongside it. That is a different fact from a product that never loaded,
	// and `ConfigError.Loaded` is what carries the difference through.
	failed := map[string]product.InvalidProduct{}
	if inv := s.deps.Products.Invalid(); len(inv) > 0 {
		for _, bad := range inv {
			failed[invalidKey(bad)] = bad
		}
	}

	loaded := s.deps.Products.List()
	out := make([]v1.Product, 0, len(loaded)+len(failed))
	for _, p := range loaded {
		api := toAPIProduct(p)
		if bad, rejected := failed[p.Metadata.Name]; rejected {
			// Running, but not on what the file now says. `Enabled` is left
			// alone deliberately: this product IS replicating.
			api.ConfigError = toAPIConfigError(bad, true)
			delete(failed, p.Metadata.Name)
		}
		out = append(out, api)
	}
	for _, bad := range failed {
		out = append(out, rejectedAPIProduct(bad))
	}
	// Merging appends the rejected ones after the loaded ones, so the whole set
	// is re-sorted: a product's position must not depend on whether its
	// document happens to parse today.
	sort.Slice(out, func(i, j int) bool { return out[i].ProductID < out[j].ProductID })

	// Pagination is a no-op at this scale (products number in the tens) but
	// the field is present from the start so adding it later is not a breaking
	// change for clients.
	WriteJSON(w, r, http.StatusOK, v1.ListProductsResponse{Products: out})
}

// invalidKey is the name a rejected document is filed under.
//
// The registry keys by product name where it knows one and by file path where
// it does not - a document that fails to PARSE never yields a name. Falling
// back to the file's base name gives the UI something to show in the one case
// where the product has no identity of its own yet, which is exactly the case
// an operator most needs to see: a file they have just created and got wrong.
func invalidKey(bad product.InvalidProduct) string {
	if bad.Name != "" {
		return bad.Name
	}
	return strings.TrimSuffix(filepath.Base(bad.File), filepath.Ext(bad.File))
}

// toAPIConfigError renders a rejection for the API, keeping the structure when
// there is structure to keep.
//
// A validation failure is `product.Errors` - a list, each naming its own field
// - and flattening it to one string throws away the only thing that lets a
// reader go straight to the offending line. A parse error has no such shape and
// is reported as the message alone.
func toAPIConfigError(bad product.InvalidProduct, loaded bool) *v1.ConfigError {
	out := &v1.ConfigError{
		Message: bad.Err.Error(),
		File:    filepath.Base(bad.File),
		Loaded:  loaded,
	}
	var errs product.Errors
	if errors.As(bad.Err, &errs) {
		out.Details = make([]v1.ConfigIssue, 0, len(errs))
		for _, e := range errs {
			out.Details = append(out.Details, v1.ConfigIssue{
				Field:   e.Field,
				Message: e.Message,
				Hint:    e.Hint,
			})
		}
	}
	return out
}

// rejectedAPIProduct is the view of a product that has never loaded.
//
// Almost every field is empty because almost nothing is known: the document did
// not survive validation, so its sources, targets and policies cannot be
// reported as facts. What IS known is that somebody configured a product of
// this name, that it is not running, and why - which is the whole content of
// the row.
func rejectedAPIProduct(bad product.InvalidProduct) v1.Product {
	name := invalidKey(bad)
	return v1.Product{
		Name:      "products/" + name,
		ProductID: name,
		// Not enabled, and not as a guess: nothing about this product runs.
		// Whatever `enabled` its document may claim was never read.
		Enabled:     false,
		Sources:     []v1.Repository{},
		Targets:     []v1.Repository{},
		ConfigError: toAPIConfigError(bad, false),
	}
}

func (s *Server) handleGetProduct(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "product")

	if s.deps.Products == nil {
		NotFound(w, r, "product", name)
		return
	}

	p, ok := s.deps.Products.Get(name)
	if !ok {
		// A product whose document was REJECTED is returned, not refused.
		//
		// This used to be a 404 carrying the reason in its detail, which was
		// better than a bare 404 and still wrong in two ways: a resource the
		// List method returns cannot be missing from the Get method, and a
		// reader deep-linked to a broken product got an error page instead of
		// the product and its error. "Not found" is also simply untrue - it is
		// configured, it is just not running.
		for _, bad := range s.deps.Products.Invalid() {
			if invalidKey(bad) == name {
				WriteJSON(w, r, http.StatusOK, rejectedAPIProduct(bad))
				return
			}
		}
		NotFound(w, r, "product", name)
		return
	}

	api := toAPIProduct(p)
	// Loaded AND rejected: an edit to a working product failed validation, so
	// what is running is the previous version. The product is fine; the change
	// did not take effect. Reporting only the first half is how somebody
	// concludes their edit landed.
	for _, bad := range s.deps.Products.Invalid() {
		if invalidKey(bad) == name {
			api.ConfigError = toAPIConfigError(bad, true)
			break
		}
	}
	WriteJSON(w, r, http.StatusOK, api)
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	Error(w, r, v1.CodeNotFound, "No route matches "+r.Method+" "+r.URL.Path)
}

func (s *Server) handleMethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	pr := v1.NewProblem(v1.CodeInvalidArgument, r.Method+" is not supported on "+r.URL.Path)
	pr.Status = http.StatusMethodNotAllowed
	WriteProblem(w, r, pr)
}

func toAPIChecks(in []health.Result) []v1.HealthCheck {
	out := make([]v1.HealthCheck, 0, len(in))
	for _, c := range in {
		out = append(out, v1.HealthCheck{
			Name:      c.Name,
			Status:    v1.HealthStatus(c.Status),
			LatencyMs: c.LatencyMillis(),
			Detail:    c.Detail,
			Error:     c.Error,
		})
	}
	return out
}

// toAPIProduct converts the internal document to the public view.
//
// This mapping is deliberate rather than a struct embed: the internal type
// carries resolved credentials and source file paths that must never leave
// the process, and a future field added internally must not silently appear
// on the wire.
func toAPIProduct(p *product.Product) v1.Product {
	out := v1.Product{
		Name:        "products/" + p.Metadata.Name,
		ProductID:   p.Metadata.Name,
		DisplayName: p.Metadata.DisplayName,
		Description: p.Metadata.Description,
		Owner:       p.Metadata.Owner,
		Labels:      p.Metadata.Labels,
		Enabled:     p.IsEnabled(),
		ConfigHash:  p.ConfigHash,
		Sources:     make([]v1.Repository, 0, len(p.Spec.Sources)),
		Targets:     make([]v1.Repository, 0, len(p.Spec.Targets)),
	}

	for _, s := range p.Spec.Sources {
		r := v1.Repository{
			Name:                s.Name,
			Enabled:             s.IsEnabled(),
			Registry:            s.Registry,
			Repositories:        s.DeclaredRepositories(),
			RepositoryDiscovery: s.EnumeratesRepositories(),
			Type:                string(s.Type),
			Vendor:              s.VendorLayout(),
			Role:                string(product.RoleSource),
			Concurrency:         toAPIConcurrency(s.Concurrency),
			Discovery: &v1.Discovery{
				Enabled:         s.Discovery.IsEnabled(),
				IntervalSeconds: int(s.Discovery.Interval.Duration().Seconds()),
				IncludePatterns: s.Discovery.TagFilters.Include,
				ExcludePatterns: s.Discovery.TagFilters.Exclude,
			},
		}
		// The singular field stays populated for the single-repository case, so
		// a client written before sources could span repositories keeps working.
		if declared := s.DeclaredRepositories(); len(declared) == 1 {
			r.Repository = declared[0]
		}
		if f := s.Discovery.RepositoryFilters; len(f.Include) > 0 || len(f.Exclude) > 0 {
			r.RepositoryFilters = &v1.Filters{Include: f.Include, Exclude: f.Exclude}
		}
		out.Sources = append(out.Sources, r)
	}

	for _, t := range p.Spec.Targets {
		out.Targets = append(out.Targets, v1.Repository{
			Name:          t.Name,
			Enabled:       t.IsEnabled(),
			Registry:      t.Registry,
			Repository:    t.Repository,
			Repositories:  []string{t.Repository},
			Type:          string(t.Type),
			Environment:   t.Environment,
			Role:          string(product.RoleTarget),
			Default:       t.Default,
			PromotionOnly: t.PromotionOnly,
			Concurrency:   toAPIConcurrency(t.Concurrency),
		})
	}

	out.AutoDownload = v1.AutoDownloadSummary{Enabled: p.Spec.AutoDownload.Enabled}
	for _, rule := range p.Spec.AutoDownload.Rules {
		// Targets and priority come from the DOWNLOAD the rule triggers, not
		// from the rule: a rule holds a pattern, and where things go is the
		// download's business. Resolving here keeps this summary answering the
		// question it always answered.
		summary := v1.AutoDownloadRule{Name: rule.Name, TagPattern: rule.TagPattern}
		if d, err := p.DownloadFor(rule); err == nil {
			summary.Targets = d.Targets
			summary.Priority = d.EffectivePriority()
		}
		out.AutoDownload.Rules = append(out.AutoDownload.Rules, summary)
	}

	out.Verification = v1.VerificationSummary{
		Enabled:       p.Spec.Verification.Enabled,
		Policy:        string(p.Spec.Verification.Policy),
		Mode:          string(p.Spec.Verification.Cosign.Mode),
		AtSource:      p.Spec.Verification.AtSource,
		AtDestination: p.Spec.Verification.AtDestination,
	}

	return out
}

func toAPIConcurrency(c product.Concurrency) v1.Concurrency {
	return v1.Concurrency{
		PerRegistry:       c.PerRegistry,
		RequestsPerSecond: c.RequestsPerSecond,
	}
}
