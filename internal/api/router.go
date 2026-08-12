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
	"github.com/abhijeet-oxide/softwareGateway/internal/calibrate"
	"github.com/abhijeet-oxide/softwareGateway/internal/discovery"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/health"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/metrics"
	"github.com/abhijeet-oxide/softwareGateway/internal/preflight"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/queue"
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
	// StartProduct begins a scan without waiting for it.
	StartProduct(productName string) (started, alreadyRunning int, err error)
	// InspectPackage expands one package's manifest tree on demand.
	InspectPackage(ctx context.Context, packages *store.Packages, pkg store.PackageRow,
		productName string) (discovery.InspectResult, error)
	// Progress reports what every source is doing right now.
	Progress(productName string) []discovery.SourceProgress
	// Products lists the products being polled, so a global scan knows what
	// "everything" means without the caller enumerating config a second time.
	Products() []string
}

// Worker is the queue as the API needs it: hand out work, take back results.
//
// A consumer-defined interface rather than *queue.Queue, so this package
// depends on the four calls the worker plane makes rather than on the reaper,
// the wave logic and everything else the queue owns (docs/design/15 §6).
type Worker interface {
	Lease(ctx context.Context, workerID string, capacity int) (queue.LeaseResult, error)
	Progress(ctx context.Context, jobID int64, workerID string, bytes int64) error
	Complete(ctx context.Context, c store.Completion) (store.CompletionResult, error)
	Heartbeat(ctx context.Context, workerID string, activeJobs []int64) ([]int64, error)
	// Retry returns one transfer's failed jobs to the queue. Here rather than
	// on a separate interface because it is the same queue and the same
	// invariants — a requeue that bypassed this type could reopen a wave the
	// scheduler believes is closed.
	Retry(ctx context.Context, transferID string) (store.RetryResult, error)

	// RecordWorker notes what a worker reported about itself. No error: the
	// fleet view must never be the reason a lease fails.
	RecordWorker(ctx context.Context, id string, capacity, active int, version string)
	// Workers reports the fleet.
	Workers(ctx context.Context) ([]store.WorkerSummary, error)
}

// Calibrator measures one source-to-target path and recommends settings.
//
// A consumer-defined interface for the same reason as ConnectivityChecker: the
// API needs one method, not the probe machinery behind it.
type Calibrator interface {
	Run(ctx context.Context, p *product.Product, opts calibrate.Options) (calibrate.Report, error)
}

// ConnectivityChecker probes configured registries.
//
// A consumer-defined interface: the API needs one method, not the checker's
// construction (docs/design/15 §6).
type ConnectivityChecker interface {
	CheckProduct(ctx context.Context, p *product.Product) preflight.ProductResult
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
	Queue     Worker
	Requests  Requests
	Preflight ConnectivityChecker
	// Calibrator is optional: without it the calibrate route is not
	// registered, and a caller is told so by an honest 404.
	Calibrator Calibrator
	Leader     Leadership
	Component  string
}

// Server wires the router.
type Server struct {
	deps   Deps
	router chi.Router
}

// NewServer builds the HTTP surface.
//
// ONLY IMPLEMENTED ROUTES ARE REGISTERED. Routes specified in
// docs/design/09-api.md but not yet built are deliberately absent, so a caller
// receives an honest 404 rather than a stub returning fabricated data. They
// arrive with the features that back them. The worker plane and the transfer
// read routes landed with M3; pause, resume, cancel and retry have not, and
// are therefore still absent rather than present and inert.
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

		// AIP-136 custom methods. Connectivity checking is separate from the
		// health check on purpose: health must not depend on third-party
		// registries, or a vendor outage pulls this replica out of service.
		if s.deps.Preflight != nil {
			r.Post("/products:checkConnectivity", s.handleCheckConnectivity)
			r.Post("/products/{product}:checkConnectivity", s.handleCheckConnectivity)
		}

		// Calibration is per product and never fleet-wide. There is no
		// `/products:calibrate`: measuring every product would mean saturating
		// every vendor link this deployment has, in sequence, and the answer
		// for one path says nothing about another.
		if s.deps.Calibrator != nil {
			r.Post("/products/{product}:calibrate", s.handleCalibrate)
		}

		// Packages. Registered only when there is a store behind them, so a
		// deployment without persistence returns an honest 404 rather than a
		// route that always fails.
		if s.deps.Packages != nil {
			r.Get("/products/{product}/packages", s.handleListPackages)
			r.Get("/products/{product}/packages/{package}", s.handleGetPackage)
			r.Get("/products/{product}/packages/{package}/artifacts", s.handleListArtifacts)

			// AIP-136 custom method. Registered whenever discovery is wired, so a
			// follower can answer with the reason it is not scanning rather than
			// with a 404 that reads like a missing feature.
			if s.deps.Discovery != nil {
				r.Post("/products/{product}/packages:discover", s.handleDiscoverPackages)
				// Fleet-wide: scan every product being polled. The common operator
				// action after a maintenance window is "go and look at everything",
				// and making that a shell loop over `products list` puts the
				// definition of "everything" in the wrong place.
				r.Post("/products:discover", s.handleDiscoverAll)

				// Read-only, and deliberately outside the group above: a caller
				// polling progress while a scan runs must not be blocked by
				// whatever gates the write path.
				r.Get("/products/{product}/discovery", s.handleDiscoveryStatus)

				// AIP-136 custom method: expanding a package has side effects — it
				// writes artifacts, blobs and a measured size — so it is a POST verb
				// rather than a GET that quietly mutates.
				//
				// Registered as the PLAIN package pattern, with the `:verb` suffix
				// split off by the handler rather than by the router. The obvious
				// spelling — `/packages/{package}:inspect` — is a chi partial-segment
				// pattern whose delimiter is `:`, and it matches on the FIRST colon in
				// the segment. That works for a tag and silently fails for a digest:
				// `sha256:ccbd…:inspect` binds `{package}` to `sha256` and then cannot
				// match `:inspect` against `:ccbd…`, so the request falls through to
				// the GET-only route and comes back
				// `INVALID_ARGUMENT: POST is not supported on …`. Splitting here
				// costs six lines and makes every reference form work.
				if s.deps.Packages != nil {
					r.Post("/products/{product}/packages/{package}", s.handlePackageCustomMethod)
				}
			}

			// Transfers, read-only. Registered with the store rather than with
			// the queue: a follower replica serves these perfectly well, and an
			// operator asking "did it work" should not need to find the leader.
			// Creating a request needs the resolver and the planner behind it,
			// so it is registered only when those are wired. The read routes
			// below need neither and a follower replica serves them fine.
			if s.deps.Requests != nil {
				r.Post("/transfers", s.handleCreateTransfer)
			}
			r.Get("/transfers", s.handleListTransfers)
			r.Get("/transfers/{transfer}", s.handleGetTransfer)
			r.Get("/transfers/{transfer}/jobs", s.handleListTransferJobs)
			r.Get("/transfers/{transfer}/failures", s.handleListTransferFailures)

			// Retry needs the queue behind it, so it is registered with the
			// queue rather than with the read routes — a follower replica
			// serves the reads and honestly 404s the write.
			if s.deps.Queue != nil {
				// The verb is split by the handler, not the router. See
				// handleTransferCustomMethod.
				r.Post("/transfers/{transfer}", s.handleTransferCustomMethod)
				r.Post("/transfers:retry", s.handleRetryTransfers)
			}
		}

		// ---- The worker plane (docs/design/09 §7) ----
		//
		// Registered only when a queue is wired. A Coordinator without one is
		// a control plane with no data plane, and a worker calling it should
		// be told that plainly rather than have its jobs accepted and lost.
		if s.deps.Queue != nil {
			// The fleet, which until now was invisible: the workers table was
			// created in the first migration and nothing ever wrote to it.
			r.Get("/workers", s.handleListWorkers)

			r.Post("/jobs:lease", s.handleLeaseJobs)
			// The verb is split by the handler, not the router — see
			// handleJobCustomMethod.
			r.Post("/jobs/{job}", s.handleJobCustomMethod)
			r.Post("/workers/{worker}", s.handleWorkerHeartbeat)
		}
	})

	return r
}

// Shutdown releases server-held resources. The HTTP server's own lifecycle is
// owned by cmd/coordinator.
func (s *Server) Shutdown(context.Context) error { return nil }
