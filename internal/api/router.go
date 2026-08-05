// Package api owns the Coordinator's HTTP surface: routing, DTOs, middleware
// and HTTP semantics.
//
// It owns no business logic. A handler parses, calls a domain package, and
// serializes. See docs/design/15-code-layout.md section 2.
package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/abhijeet-oxide/softwareGateway/internal/api/middleware"
	"github.com/abhijeet-oxide/softwareGateway/internal/discovery"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/health"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/metrics"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// Leadership reports whether this replica holds the leader lock. Satisfied by
// platform/leader; declared here so api does not import leader directly.
type Leadership interface{ IsLeader() bool }

// Discoverer triggers scans and reports whether discovery is running.
//
// A consumer-defined interface rather than *discovery.Loop, so the API package
// depends on the two methods it calls instead of the whole engine (docs/design
// /15 §6).
type Discoverer interface {
	Running() bool
	Trigger(ctx context.Context, productName, sourceName string) (discovery.ScanResult, error)
	TriggerProduct(ctx context.Context, productName string) (discovery.ScanResult, error)
}

// Deps are the Coordinator's dependencies.
type Deps struct {
	Logger    *slog.Logger
	Metrics   *metrics.Registry
	Health    *health.Registry
	Products  *product.Registry
	Store     store.Store
	Packages  *store.Packages
	Discovery Discoverer
	Leader    Leadership
	Component string
}

// Server wires the router.
type Server struct {
	deps   Deps
	router chi.Router
}

// NewServer builds the HTTP surface.
//
// ONLY IMPLEMENTED ROUTES ARE REGISTERED. Routes specified in
// docs/design/09-api.md but not yet built (transfers, packages, the worker
// plane) are deliberately absent, so a caller receives an honest 404 rather
// than a stub returning fabricated data. They arrive with the features that
// back them, in M2 and M3.
func NewServer(deps Deps) *Server {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	s := &Server{deps: deps}
	s.router = s.routes()
	return s
}

// Handler returns the root handler.
func (s *Server) Handler() http.Handler { return s.router }

func (s *Server) routes() chi.Router {
	r := chi.NewRouter()

	// Order is load-bearing — see internal/api/middleware.
	r.Use(middleware.RequestID)
	r.Use(middleware.Logging(s.deps.Logger))
	r.Use(func(next http.Handler) http.Handler {
		// otelhttp names spans by route template, matching the metric labels.
		return otelhttp.NewHandler(next, "coordinator",
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
					return r.Method + " " + rc.RoutePattern()
				}
				return r.Method + " " + r.URL.Path
			}))
	})
	if s.deps.Metrics != nil {
		r.Use(middleware.Metrics(s.deps.Metrics))
	}
	r.Use(middleware.Recovery(internalErrorWriter(s.deps.Logger)))
	r.Use(middleware.Auth(middleware.AnonymousAuthenticator{}, unauthenticatedWriter))

	r.NotFound(s.handleNotFound)
	r.MethodNotAllowed(s.handleMethodNotAllowed)

	// ---- Probes and metrics (unversioned; scraped by infrastructure) ----
	r.Get("/healthz", s.handleLiveness)
	r.Get("/readyz", s.handleReadiness)
	if s.deps.Metrics != nil {
		r.Handle("/metrics", promhttp.HandlerFor(
			s.deps.Metrics.Prometheus(),
			promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError},
		))
	}

	// ---- API v1 ----
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/system/version", s.handleVersion)
		// AIP-136 custom method: a colon, because a deep health check is a
		// verb with side effects (it makes outbound calls), not a resource.
		r.Get("/system:healthCheck", s.handleDeepHealth)

		r.Get("/products", s.handleListProducts)
		r.Get("/products/{product}", s.handleGetProduct)

		// Packages. Registered only when there is a store behind them, so a
		// deployment without persistence returns an honest 404 rather than a
		// route that always fails.
		if s.deps.Packages != nil {
			r.Get("/products/{product}/packages", s.handleListPackages)
			r.Get("/products/{product}/packages/{package}", s.handleGetPackage)
			r.Get("/products/{product}/packages/{package}/artifacts", s.handleListArtifacts)
		}
		// AIP-136 custom method. Registered whenever discovery is wired, so a
		// follower can answer with the reason it is not scanning rather than
		// with a 404 that reads like a missing feature.
		if s.deps.Discovery != nil {
			r.Post("/products/{product}/packages:discover", s.handleDiscoverPackages)
		}
	})

	return r
}

// Shutdown releases server-held resources. The HTTP server's own lifecycle is
// owned by cmd/coordinator.
func (s *Server) Shutdown(context.Context) error { return nil }
