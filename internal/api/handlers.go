package api

import (
	"net/http"

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

// handleReadiness answers "should this replica receive traffic?" — the
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
// database — configuration is GitOps-managed and read-only over the API.
func (s *Server) handleListProducts(w http.ResponseWriter, r *http.Request) {
	if s.deps.Products == nil {
		WriteJSON(w, r, http.StatusOK, v1.ListProductsResponse{Products: []v1.Product{}})
		return
	}

	loaded := s.deps.Products.List()
	out := make([]v1.Product, 0, len(loaded))
	for _, p := range loaded {
		out = append(out, toAPIProduct(p))
	}
	// Pagination is a no-op at this scale (products number in the tens) but
	// the field is present from the start so adding it later is not a breaking
	// change for clients.
	WriteJSON(w, r, http.StatusOK, v1.ListProductsResponse{Products: out})
}

func (s *Server) handleGetProduct(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "product")

	if s.deps.Products == nil {
		NotFound(w, r, "product", name)
		return
	}

	p, ok := s.deps.Products.Get(name)
	if !ok {
		// A product whose configuration failed to load is reported as a 404
		// with the reason, rather than a bare 404 — otherwise an operator
		// cannot tell "misspelled" from "your YAML is broken".
		for _, bad := range s.deps.Products.Invalid() {
			if bad.Name == name {
				pr := v1.NewProblem(v1.CodeNotFound,
					"product "+name+" is configured but failed to load: "+bad.Err.Error())
				WriteProblem(w, r, pr)
				return
			}
		}
		NotFound(w, r, "product", name)
		return
	}
	WriteJSON(w, r, http.StatusOK, toAPIProduct(p))
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
