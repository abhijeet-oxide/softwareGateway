// Package api owns the Coordinator's HTTP surface: routing, DTOs, middleware
// and HTTP semantics.
//
// It owns no business logic. A handler parses, calls a domain package, and
// serializes. See docs/design/15-code-layout.md section 2.
package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/abhijeet-oxide/softwareGateway/internal/api/middleware"
	"github.com/abhijeet-oxide/softwareGateway/internal/calibrate"
	"github.com/abhijeet-oxide/softwareGateway/internal/compare"
	"github.com/abhijeet-oxide/softwareGateway/internal/discovery"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/health"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/metrics"
	"github.com/abhijeet-oxide/softwareGateway/internal/preflight"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/queue"
	"github.com/abhijeet-oxide/softwareGateway/internal/security"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/vendors"
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
	// activeJobs is what the worker reports it is already running. It is a
	// recovery signal as well as an accounting one - see queue.Lease.
	Lease(ctx context.Context, workerID string, capacity, activeJobs int) (queue.LeaseResult, error)
	Progress(ctx context.Context, jobID int64, workerID string, bytes int64) error
	Complete(ctx context.Context, c store.Completion) (store.CompletionResult, error)
	// Heartbeat renews what a worker holds AND tells it what to drop: a job
	// whose transfer somebody stopped is not renewed, it is cancelled, and the
	// heartbeat is the only channel that reaches a worker mid-blob.
	Heartbeat(ctx context.Context, workerID string, activeJobs []int64) (renewed []int64, cancelled []int64, err error)
	// Retry returns one transfer's failed jobs to the queue. Here rather than
	// on a separate interface because it is the same queue and the same
	// invariants - a requeue that bypassed this type could reopen a wave the
	// scheduler believes is closed.
	Retry(ctx context.Context, transferID string) (store.RetryResult, error)
	// Pause, Resume and Stop are somebody intervening in a transfer that is
	// under way. Here for the same reason as Retry: they move jobs between
	// states the scheduler reasons about, and a path that bypassed this type
	// could leave a wave open that the scheduler believes is closed.
	Pause(ctx context.Context, transferID string) (store.ControlResult, error)
	Resume(ctx context.Context, transferID string) (store.ControlResult, error)
	Stop(ctx context.Context, transferID string) (store.ControlResult, error)
	// Delete removes a settled transfer's RECORD. Nothing at the destination
	// is touched: what a transfer put there is content-addressed and shared,
	// and a delete that unpicked it would be the most dangerous operation in
	// this system. This is bookkeeping.
	Delete(ctx context.Context, transferID string) (store.ControlResult, error)
	// SetPriority reorders what a transfer has left to do. Here for the same
	// reason as the rest: it writes `jobs.priority`, which is the first key the
	// dequeue orders by.
	SetPriority(ctx context.Context, transferID string, priority int) (store.ControlResult, error)

	// RecordWorker notes what a worker reported about itself. No error: the
	// fleet view must never be the reason a lease fails.
	RecordWorker(ctx context.Context, id string, capacity, active int, version string)
	// Workers reports the fleet.
	Workers(ctx context.Context) ([]store.WorkerSummary, error)
}

// ComparePoint is one end of a comparison: which version, in which place.
//
// Two of these rather than "a package and a destination", because the ends are
// symmetric. Source against target, target against target, and one place at two
// versions are the same request with different arguments, and a shape that
// privileged one end would need a second shape for each of the others.
type ComparePoint struct {
	Package store.PackageRow
	// Endpoint is a configured source or target name. Empty means the
	// repository the package was discovered in.
	Endpoint string
}

// Comparer walks two places and reports what is different.
//
// A consumer-defined interface: the API needs the one call, not the client
// factory, the registry walker and the product registry behind it
// (docs/design/15 §6).
type Comparer interface {
	// fileBudget is how many bytes of layer content the comparison may
	// download in order to say which FILES changed rather than which layers.
	// Zero means the server's default; negative means none.
	// progress is called as the comparison proceeds, and may be nil. It is a
	// parameter rather than a field on the implementation because a comparison
	// is a request, and two of them in flight report separately.
	Compare(ctx context.Context, productName string, a, b ComparePoint,
		fileBudget int64, progress compare.ProgressFunc) (compare.Report, error)
}

// BlobReader opens one blob of a release from where it was published.
//
// A consumer-defined interface, and a deliberately narrow one: the API needs
// bytes for a digest it has already ESTABLISHED belongs to the release being
// read, not the client factory and the credential store behind it. The handler
// does the establishing; this does the fetching, and cannot be asked for
// anything else.
type BlobReader interface {
	ReadBlob(ctx context.Context, productName string, pkg store.PackageRow,
		digest string) (io.ReadCloser, error)
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
	// Comparer is optional on the same terms: it reaches a destination
	// registry through the configured client factory, which only a
	// composition root holds.
	Comparer Comparer
	// Blobs reads a file's content out of the source registry. Optional on the
	// same terms: without it the file-content route is absent and a caller
	// gets an honest 404 rather than a route that always fails.
	Blobs BlobReader
	// FileDownloadsEnabled gates the raw-bytes download route separately from
	// Blobs: viewing a file's text and saving its bytes are the same read, but
	// a deployment may want the first without the second - see
	// FilesConfig.DownloadEnabled.
	FileDownloadsEnabled bool
	// Replication is optional: it needs a registry management client and the
	// secrets behind it, which only a composition root holds. Without it the
	// replication routes are absent and a caller gets an honest 404 rather
	// than a route that always fails.
	Replication Replicator
	// Downloads is the download surface. Optional on the same terms: without
	// it the routes are absent and a caller gets an honest 404.
	Downloads Downloader
	// TargetRows resolves configured target names to catalog rows, which only
	// running a rule needs.
	TargetRows TargetRows
	// Promotions answers how a promotion would be carried out. Optional on
	// the same terms as Comparer: it reaches the promoter plugins through
	// resolved credentials, which only a composition root holds. Without it
	// every hop is reported as a copy, which is what it would be.
	Promotions Promotions
	// PromotionStore backs the promotion progress on a transfer. Separate from
	// Promotions because reading what a promotion DID is a database query and
	// must keep working on a replica that cannot resolve a credential at all.
	PromotionStore *store.Promotions
	// ReplicationStore backs the sync history. Separate from Replication
	// because the history is readable on a Coordinator that cannot currently
	// reach the registry at all, and that is exactly when it is wanted.
	ReplicationStore *store.Replication
	Leader           Leadership
	// Vendors resolves a source's configured layout, which is what lets an
	// artifact listing report a vendor's Helm charts as charts. Optional: a
	// deployment without it classifies on the OCI fields alone, which is
	// correct for a conformant registry and under-reports charts for one that
	// relies on its own annotations.
	Vendors   *vendors.Registry
	Component string

	// SecuritySync runs vulnerability syncs. Optional on the same terms as
	// Comparer: it reaches a scanner through the configured client factory and
	// the secrets behind it, which only a composition root holds. Without it
	// the sync route is absent and a caller gets an honest 404 rather than a
	// route that always fails.
	SecuritySync SecuritySyncer
	// SecurityStore serves every security READ. Separate from SecuritySync
	// because the two fail differently: the sync needs a reachable scanner and
	// this needs only the database, so a release's stored findings stay
	// readable while the scanner is down - which is exactly when somebody looks
	// at them.
	SecurityStore SecurityStore
	// SecurityIndex is the searchable record of what syncs have recorded.
	SecurityIndex SecurityIndex
	// SecurityReplicate registers a release with a scanner that has to be TOLD
	// about it before it can answer - Anchore, today.
	//
	// Set only on a Coordinator that may reach such a scanner. Without it the
	// replicate route is absent and a caller gets an honest 404 rather than a
	// route that always fails.
	SecurityReplicate SecurityReplicator
	// SecurityRegistrations serves the STORED replication state. Separate from
	// SecurityReplicate because the two fail differently: running one needs a
	// reachable scanner and this needs only the database, so a release's state
	// stays readable while the scanner is down - which is when somebody looks.
	SecurityRegistrations SecurityRegistrationStore
	// SecurityRetention is how long a sync's tiers are PINNED, from the system
	// configuration. Past it a row is evictable rather than gone. The zero
	// value means the store's own defaults.
	SecurityRetention security.CacheTTL
	// SecurityDocuments generates a scanner body that is not held - which in
	// practice means an SBOM, the one document a sync deliberately does not
	// fetch. Nil disables the on-demand path and leaves the download serving
	// only what a sync captured.
	SecurityDocuments SecurityDocuments
	// SecurityFreshness is when an answer stops being presented as current.
	//
	// Not a retention and not a schedule: nothing is deleted and nothing is
	// refetched because of it. It is the number the interface uses to put an
	// age in words beside a count and a Refresh beside the age, and it is on
	// the wire so that the rule lives in one place rather than being guessed
	// at by every page that draws a date.
	SecurityFreshness security.Freshness

	// ComplianceRunner starts a check of a release against the organization's
	// own Kubernetes and CNF standards.
	//
	// Set only on a Coordinator that may reach a vendor registry and shell out
	// to helm. Without it the run route is absent, and a caller gets an honest
	// 404 rather than a route that always fails.
	ComplianceRunner ComplianceRunner
	// ComplianceStore serves every compliance READ. Separate from the runner
	// because the two fail differently: a run needs a reachable registry and a
	// helm binary, and this needs only the database - so a release's findings
	// stay readable when neither is available, which is exactly when somebody
	// is asking why a release was blocked.
	ComplianceStore ComplianceStore
	// ComplianceEvidence serves the manifests a run judged. Separate from the
	// store above because it is separately absent: a deployment can turn the
	// keeping of them off, and a run recorded before they were kept has none.
	ComplianceEvidence ComplianceEvidence
	// ComplianceCatalogue serves the rulebook: what will be checked and why.
	//
	// A function rather than a value, because the loader swaps the catalogue
	// when a policy directory changes and a handler holding the old one would
	// serve a rulebook nobody is being checked against.
	ComplianceCatalogue ComplianceCatalogue
	// ComplianceHelm reports whether charts can be rendered at all. On screen
	// this is the difference between a tab full of "could not be checked" with
	// no explanation and one that says the helm binary is missing.
	ComplianceHelm ComplianceHelm
}

// Server wires the router.
type Server struct {
	deps   Deps
	router chi.Router
	// comparisons is progress for comparisons in flight - see
	// compareprogress.go for why it lives in memory.
	comparisons *compareTracker
	// analyses are the manifest-tree walks THIS replica is running, so one can
	// be stopped rather than only disowned. See internal/api/analysis.go.
	analyses *analysisRunner
}

// NewServer builds the HTTP surface.
//
// ONLY IMPLEMENTED ROUTES ARE REGISTERED. Routes specified in
// docs/design/09-api.md but not yet built are deliberately absent, so a caller
// receives an honest 404 rather than a stub returning fabricated data. They
// arrive with the features that back them. The worker plane and the transfer
// read routes landed with M3, and retry, pause, resume and stop with it;
// setPriority has not, and is therefore still absent rather than present and
// inert.
func NewServer(deps Deps) *Server {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	s := &Server{
		deps:        deps,
		comparisons: newCompareTracker(),
		analyses:    newAnalysisRunner(),
	}
	s.router = s.routes()
	return s
}

// Handler returns the root handler.
func (s *Server) Handler() http.Handler { return s.router }

func (s *Server) routes() chi.Router {
	r := chi.NewRouter()

	// Order is load-bearing - see internal/api/middleware.
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
	r.Use(middleware.Compress)

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
		// Who is calling. Registered unconditionally and with no dependency:
		// a caller must always be able to discover that they are anonymous,
		// including on a Coordinator with no store behind it.
		r.Get("/whoami", s.handleWhoAmI)
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

		// Target replication. These do NOT contradict "products are read-only
		// over the API": configuration still comes from Git, and `:apply`
		// pushes what Git already says into a third-party registry's own
		// configuration store. Nothing here edits a product.
		//
		// Registered only when a composition root supplied a replicator, since
		// the routes need a management client and the secrets behind it.
		if s.deps.Replication != nil {
			r.Get("/products/{product}/replication", s.handleListReplication)
			r.Get("/products/{product}/targets/{target}/replication", s.handleGetReplication)
			r.Post("/products/{product}/targets/{target}/replication:apply", s.handleApplyReplication)
			r.Post("/products/{product}/targets/{target}/replication:sync", s.handleSyncReplication)
			r.Post("/products/{product}/targets/{target}/replication:cancelSync", s.handleCancelSyncReplication)
		}
		// Downloads and auto-download rules, as two resources because they are
		// two things. Both collections are read-only - they are configuration
		// - and the one verb takes SOFTWARE rather than a pattern: a pattern
		// decides what to download when nobody is asking, and `:run` is
		// somebody asking.
		if s.deps.Downloads != nil {
			r.Get("/products/{product}/downloads", s.handleListDownloads)
			r.Post("/products/{product}/downloads:run", s.handleRunDownload)
			r.Get("/products/{product}/autoDownloadRules", s.handleListAutoDownloadRules)
			r.Get("/products/{product}/autoDownloadRules/{rule}/matches", s.handleRuleMatches)
		}

		// The sync HISTORY is readable without a management client, and that
		// separation is deliberate: "what has this mirror been doing" is most
		// wanted precisely when the registry cannot be reached.
		if s.deps.ReplicationStore != nil {
			r.Get("/products/{product}/targets/{target}/syncs", s.handleListSyncs)
		}

		// Packages. Registered only when there is a store behind them, so a
		// deployment without persistence returns an honest 404 rather than a
		// route that always fails.
		if s.deps.Packages != nil {
			r.Get("/products/{product}/packages", s.handleListPackages)
			r.Get("/products/{product}/packages/{package}", s.handleGetPackage)
			r.Get("/products/{product}/packages/{package}/artifacts", s.handleListArtifacts)
			// WHERE THIS RELEASE CAN GO NEXT, and what sending it there would
			// do. A GET because every input is configuration or a row and
			// nothing leaves the process - a dialog that could not be opened
			// without a side effect would be one nobody could safely reopen.
			//
			// Registered on Packages rather than on Promotions: the answer is
			// useful without a promoter configured, where it correctly reports
			// that every hop is a copy.
			r.Get("/products/{product}/packages/{package}/promotionOptions",
				s.handlePromotionOptions)
			// What is INSIDE the release, as files rather than as layers.
			// Its own route rather than a field on the artifact listing: a
			// release has thousands of files and dozens of artifacts, and one
			// page wants one of those and not the other.
			r.Get("/products/{product}/packages/{package}/files", s.handleListPackageFiles)
			if s.deps.Blobs != nil {
				// ONE file's content, for a reader who wants to look at it.
				// Registered only where a client factory exists, because
				// unlike the listing above this one leaves the database.
				r.Get("/products/{product}/packages/{package}/files/content",
					s.handleGetPackageFileContent)
				if s.deps.FileDownloadsEnabled {
					// ONE file's bytes, for a reader who wants to save it - an
					// archive, an image, a file too large to look at inline.
					// Gated separately so a deployment can turn saving off
					// without also losing the ability to look.
					r.Get("/products/{product}/packages/{package}/files/download",
						s.handleDownloadPackageFile)
				}
			}
			// What the source would not serve. A sibling of the packages listing
			// rather than part of it: these are not packages, they are the
			// absence of packages, and folding them in would make every consumer
			// of the listing filter them out.
			r.Get("/products/{product}/unavailable", s.handleListUnavailable)

			// Security. Its own sub-resource rather than fields on the package,
			// because it is answered by a different system with a different
			// failure mode: a release whose scanner is unreachable must still
			// render as a release.
			//
			// Registered on the STORE rather than on the syncer: reading a
			// release's security is a database query, and it must keep working
			// on a Coordinator that cannot currently reach a scanner at all.
			if s.deps.SecurityStore != nil {
				r.Get("/products/{product}/packages/{package}/security",
					s.handlePackageSecurity)
				// One scanner body for one image: the SBOM behind the button
				// beside the Xray link, and the raw responses somebody
				// forwards to a customer.
				r.Get("/products/{product}/packages/{package}/security/documents/{kind}",
					s.handleSecurityDocument)
				r.Get("/products/{product}/packages/{package}/security/export",
					s.handleExportPackageSecurity)
				r.Get("/products/{product}/packages/{package}/security/compare/export",
					s.handleExportSecurityComparison)
			}
			// Search reads what has already been retrieved, so it is registered
			// on the INDEX rather than on the analyzer: it answers on a
			// Coordinator that cannot currently reach a scanner at all, which
			// is exactly when somebody is searching for a CVE.
			if s.deps.SecurityIndex != nil {
				r.Get("/products/{product}/security/search", s.handleSecuritySearch)
				r.Get("/products/{product}/security/search/export", s.handleExportSecuritySearch)
			}

			// Compliance. Registered on the STORE rather than on the runner,
			// for the same reason security is: reading a release's compliance
			// is a database query, and it must keep working on a Coordinator
			// that cannot currently reach a registry or run helm - which is
			// exactly the state somebody is in when they are working out why a
			// release was blocked.
			if s.deps.ComplianceStore != nil {
				r.Get("/products/{product}/packages/{package}/compliance",
					s.handlePackageCompliance)
				r.Get("/products/{product}/packages/{package}/compliance/runs",
					s.handleComplianceRuns)
				// THE REPORT. Registered on the reads-only condition beside the
				// results, because it is the same answer for a reader who is
				// never going to open this platform - a vendor engineer sent a
				// spreadsheet, an auditor asked to show the release was checked.
				r.Get("/products/{product}/packages/{package}/compliance/export",
					s.handleExportPackageCompliance)
			}
			// THE MANIFESTS THE RUN JUDGED. Registered beside the results and
			// on the same reads-only condition, because that is what they are:
			// a finding and the lines it is about are one answer, and a
			// Coordinator that can serve the first and not the second sends a
			// vendor a claim with the evidence missing.
			if s.deps.ComplianceStore != nil && s.deps.ComplianceEvidence != nil {
				r.Get("/products/{product}/packages/{package}/compliance/rendered",
					s.handleComplianceRendered)
				r.Get("/products/{product}/packages/{package}/compliance/rendered/content",
					s.handleComplianceRenderedContent)
				r.Get("/products/{product}/packages/{package}/compliance/rendered/excerpt",
					s.handleComplianceExcerpt)
			}
			// Running one reaches a vendor registry and shells out to helm, so
			// it is registered only where those are possible.
			if s.deps.ComplianceRunner != nil {
				r.Post("/products/{product}/packages/{package}/compliance:run",
					s.handleRunCompliance)
				r.Post("/products/{product}/packages/{package}/compliance:cancel",
					s.handleCancelCompliance)
			}
			// The rulebook is not scoped to a product: it is what WILL be
			// checked, and a vendor asking before they ship has no release to
			// point at yet.
			if s.deps.ComplianceCatalogue != nil {
				r.Get("/policies", s.handlePolicies)
				r.Get("/policies/{check}", s.handlePolicy)
			}

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

				// AIP-136 custom method: expanding a package has side effects - it
				// writes artifacts, blobs and a measured size - so it is a POST verb
				// rather than a GET that quietly mutates.
				//
				// Registered as the PLAIN package pattern, with the `:verb` suffix
				// split off by the handler rather than by the router. The obvious
				// spelling - `/packages/{package}:inspect` - is a chi partial-segment
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
			// The audit trail, and the rollups over it. Registered with the
			// store rather than the queue: both are records, and reading
			// them must not require finding the leader.
			r.Get("/auditEvents", s.handleListAuditEvents)
			r.Get("/reports/summary", s.handleReportSummary)

			// A comparison's progress while its own request is open. Not under
			// /products, because the token identifies it and the caller
			// polling it already knows which product it asked about - and a
			// path that repeated the product would be a second place for the
			// two to disagree.
			r.Get("/comparisons/{comparison}", s.handleCompareProgress)

			r.Get("/transfers", s.handleListTransfers)
			// Registered BEFORE the parameterised route, and spelled with a
			// colon rather than as a path segment, so it cannot ever be read as
			// a transfer called "activity". Same shape as
			// `GET /system:healthCheck`.
			r.Get("/transfers:activity", s.handleTransferActivity)
			r.Get("/transfers/{transfer}", s.handleGetTransfer)
			r.Get("/transfers/{transfer}/jobs", s.handleListTransferJobs)
			r.Get("/transfers/{transfer}/failures", s.handleListTransferFailures)
			// WHAT the destination already held, by name. Its own route
			// because the transfer is polled and this is not.
			r.Get("/transfers/{transfer}/present", s.handleListPresentComponents)

			// Retry needs the queue behind it, so it is registered with the
			// queue rather than with the read routes - a follower replica
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
			// The verb is split by the handler, not the router - see
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
