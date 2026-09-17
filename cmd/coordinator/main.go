// Command coordinator is the softwareGateway control plane.
//
// It owns all state and is the sole writer to the database. Two replicas run
// for API availability; one holds a pg_advisory_lock and runs the background
// loops. See docs/design/00-overview.md section 5.1.
//
// This binary contains wiring only - construct, inject, run. Logic in main is
// untestable. See docs/design/15-code-layout.md section 3.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/abhijeet-oxide/softwareGateway/internal/api"
	"github.com/abhijeet-oxide/softwareGateway/internal/api/middleware"
	"github.com/abhijeet-oxide/softwareGateway/internal/calibrate"
	"github.com/abhijeet-oxide/softwareGateway/internal/catalog"
	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
	"github.com/abhijeet-oxide/softwareGateway/internal/discovery"
	"github.com/abhijeet-oxide/softwareGateway/internal/download"
	"github.com/abhijeet-oxide/softwareGateway/internal/maintenance"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/config"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/health"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/leader"
	plog "github.com/abhijeet-oxide/softwareGateway/internal/platform/log"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/metrics"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/tlscompat"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/tracing"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/version"
	"github.com/abhijeet-oxide/softwareGateway/internal/preflight"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/promotion"
	"github.com/abhijeet-oxide/softwareGateway/internal/queue"
	"github.com/abhijeet-oxide/softwareGateway/internal/regclient"
	"github.com/abhijeet-oxide/softwareGateway/internal/replication"
	"github.com/abhijeet-oxide/softwareGateway/internal/security"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/transfer"
	"github.com/abhijeet-oxide/softwareGateway/internal/vendors"
	"github.com/abhijeet-oxide/softwareGateway/internal/vendors/near"
	"github.com/abhijeet-oxide/softwareGateway/pkg/authz"

	// Registers the JFrog promoter with the plugin registry, via its init.
	//
	// THE ONE PLACE A PROMOTER IS NAMED, and it is the same arrangement the
	// vendor layout above uses. Everything downstream depends on the seam in
	// internal/transfer and is forbidden by depguard from importing an
	// implementation, so `grep -rn "promote/jfrog"` finding only this file is
	// the mechanical form of "the engine does not know what Artifactory is".
	// Deleting internal/promoter/jfrog must leave the rest building and
	// passing, with every promotion falling back to a copy.
	_ "github.com/abhijeet-oxide/softwareGateway/internal/promoter/jfrog"
)

const component = "coordinator"

// probeReadiness calls /readyz on the local listener. Used by -health-check.
func probeReadiness() error {
	addr := os.Getenv("SWGW_SERVER_ADDRESS")
	if addr == "" {
		addr = ":8080"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	// NewRequestWithContext rather than Client.Get: the linter forbids the
	// context-less helpers, and a probe that cannot be cancelled is exactly
	// the kind that hangs a container healthcheck.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// #nosec G704 -- the address is this process's own listener, read from the
	// same variable it binds. There is no request-scoped input on this path.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/readyz", nil)
	if err != nil {
		return fmt.Errorf("not ready: %w", err)
	}
	resp, err := http.DefaultClient.Do(req) // #nosec G704 -- see the request above.
	if err != nil {
		return fmt.Errorf("not ready: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("not ready: %s", resp.Status)
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "coordinator: %v\n", err)
		os.Exit(1)
	}
}

// orUnset labels an empty setting in a log line, because `tenant=""` reads as
// a bug in the logging rather than as the fact it is reporting.
func orUnset(v string) string {
	if v == "" {
		return "(not set)"
	}
	return v
}

func run() error {
	var (
		configPath  = flag.String("config", config.DefaultPath, "path to the system configuration file")
		showVersion = flag.Bool("version", false, "print version and exit")
		healthCheck = flag.Bool("health-check", false,
			"probe this process's own readiness endpoint and exit 0 or 1")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Get(component))
		return nil
	}

	// A container healthcheck that needs no shell and no curl.
	//
	// The runtime image is distroless: there is no /bin/sh, no wget and no
	// curl, so the only thing that can probe this process is this process.
	// Without it a compose or Kubernetes healthcheck has nothing to call and
	// dependent services cannot wait on readiness.
	if *healthCheck {
		return probeReadiness()
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}

	// A copy of every line to a log store when one is configured, ALONGSIDE
	// stdout rather than instead of it - see internal/platform/log.Shipper for
	// why the application ships its own, and why an unreachable store costs
	// this process nothing.
	logShipper, err := plog.NewShipper(plog.ShipConfig{
		Endpoint:  cfg.Observability.Log.Ship.Endpoint,
		Component: component,
		MaxLines:  cfg.Observability.Log.Ship.MaxLines,
		MaxBytes:  cfg.Observability.Log.Ship.MaxBytes,
	})
	if err != nil {
		return fmt.Errorf("configure log shipping: %w", err)
	}

	logger := plog.New(plog.Config{
		Level:  cfg.Observability.Log.Level,
		Format: cfg.Observability.Log.Format,
	}, plog.Writer(os.Stdout, logShipper), component)

	info := version.Get(component)
	logger.Info("starting", "version", info.Version, "commit", info.Commit, "go", info.GoVersion)

	// SQLite is a development convenience and is explicitly not supported in
	// production. Say so loudly rather than letting someone discover it during
	// an incident. See docs/design/03-persistence.md section 2.
	if !cfg.IsProduction() {
		logger.Warn("using the SQLite driver - DEVELOPMENT ONLY, not supported in production",
			"driver", cfg.Database.Driver, "dsn", cfg.Database.DSN)
	}

	// Before any TLS connection is made, including the database's.
	tlscompat.Apply(tlscompat.Options{
		AllowNegativeSerialNumbers: cfg.TLS.AllowNegativeSerialNumbers,
	}, logger)

	// Signals cancel the root context; every loop below observes it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mreg := metrics.New(component)

	_, shutdownTracing, err := tracing.Init(ctx, tracing.Config{
		Enabled:     cfg.Observability.Tracing.Enabled,
		Endpoint:    cfg.Observability.Tracing.Endpoint,
		SampleRatio: cfg.Observability.Tracing.SampleRatio,
	}, component)
	if err != nil {
		return fmt.Errorf("initialise tracing: %w", err)
	}
	defer func() {
		if err := shutdownTracing(context.WithoutCancel(ctx)); err != nil {
			logger.Warn("tracing shutdown", "error", err)
		}
	}()
	// LAST, so it carries the shutdown lines the defers above write - which
	// are the ones that say why the process is stopping. Bounded, because a
	// log store that has stopped answering must not stop this process exiting.
	defer func() {
		flush, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := logShipper.Close(flush); err != nil {
			logger.Warn("log shipping shutdown", "error", err)
		}
	}()

	// ---- store ----
	st, err := store.Open(ctx, store.Config{
		Driver:          store.Driver(cfg.Database.Driver),
		DSN:             cfg.Database.DSN,
		MaxOpenConns:    cfg.Database.MaxOpenConns,
		MaxIdleConns:    cfg.Database.MaxIdleConns,
		ConnMaxLifetime: cfg.Database.ConnMaxLifetime,
	})
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = st.Close() }()

	// The pool's own numbers, read at scrape time rather than copied on a
	// timer - see metrics.Registry.BindDatabase. A saturated pool is what
	// turns one slow query into a slow page, and nothing else looks like it.
	mreg.BindDatabase(st.Stats)

	if err := store.Migrate(ctx, st, logger); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	// ---- product configuration ----
	resolver := product.NewSecretResolver(cfg.SecretsDir())
	// The application-level concurrency reaches products HERE, at load, so every
	// consumer downstream reads a number that is already resolved rather than
	// each deciding for itself what an unset value means.
	loader := product.NewLoader(cfg.ProductsDir(), resolver).
		WithConcurrency(product.Concurrency{
			PerRegistry:       cfg.Concurrency.PerRegistry,
			RequestsPerSecond: cfg.Concurrency.RequestsPerSecond,
		})
	products := product.NewRegistry()
	cat := catalog.NewCatalog(st)

	packages := store.NewPackages(st)

	// Discovery is leader-gated and configuration-driven. The controller owns
	// both inputs; here we only report changes to it.
	// THE ONE PLACE A VENDOR IS NAMED.
	//
	// Everything downstream depends on vendors.Layout and is forbidden by
	// depguard from importing an implementation, so `grep -rn "vendor/near"`
	// finding only this file is the mechanical form of "the core is generic".
	// Deleting internal/vendors/near must leave the rest building and passing.
	layouts := vendors.NewRegistry()
	near.Register(layouts)

	discoveryCtl := discovery.NewController(packages, resolver, layouts, logger, mreg)

	// Delegated replication: what we last wrote to a registry's own
	// configuration, and what it did next. Only the Coordinator holds this -
	// a worker moves bytes and has no business writing a mirror config.
	replicationStore := store.NewReplication(st)

	// ---- the queue ----
	//
	// The Coordinator is the SOLE database writer, so everything a worker does
	// to the queue passes through here: leases out, results back. Bytes do not.
	jobQueue := queue.New(packages, cfg.Coordinator.Reaper.LeaseDuration, logger).
		WithMetrics(mreg)

	registryClients := regclient.NewClients(products, resolver, cfg.ProductsDir(), logger)
	transferResolver := &resolverImpl{
		products: products,
		catalog:  cat,
		packages: packages,
		clients:  registryClients,
		log:      logger,
	}

	// ---- security ----
	//
	// The scanner is reached through the SAME client configuration a transfer
	// gets - same credential, same CA bundle, same proxy - because it is the
	// same JFrog. A security path that resolved its own would be reaching that
	// host by a different route from the one that replicates from it, and the
	// day the two disagree is the day Xray reports nothing while replication
	// works perfectly.
	securityCache := store.NewSecurity(st)
	packageSecurity := store.NewPackageSecurity(st)
	// Which releases have been replicated to a scanner that has to be told
	// about them. Its own table, because "have we told Anchore this release
	// exists" is not a fact about a sync. See security/replicate.go.
	securityRegistrations := store.NewSecurityRegistrations(st)
	securityTuning := regclient.SecurityTuning{
		Concurrency:    cfg.Coordinator.Security.Concurrency,
		BatchSize:      cfg.Coordinator.Security.BatchSize,
		RequestTimeout: cfg.Coordinator.Security.RequestTimeout,
		Anchore:        anchoreTuning(cfg.Coordinator.Security.Anchore, resolver, logger),
	}
	securityRetention := security.CacheTTL{
		Summary:   cfg.Coordinator.Security.IndexRetention,
		Detail:    cfg.Coordinator.Security.DetailRetention,
		Documents: cfg.Coordinator.Security.DocumentRetention,
	}
	// When an answer stops being called current. Deliberately not a retention
	// and deliberately not a schedule - see security.Freshness.
	securityFreshness := security.Freshness{
		Vulnerabilities: cfg.Coordinator.Security.MaxAge,
		SBOM:            cfg.Coordinator.Security.SBOMMaxAge,
	}
	// The document kinds a sync retrieves beyond the vulnerability response,
	// which is captured for free from the request the scan already makes. Each
	// named kind is a request per image, so this is an operator's decision -
	// see config.SecurityConfig.Documents.
	securityDocuments := security.DocumentKindsFrom(
		cfg.Coordinator.Security.SecurityDocumentKinds())
	securityService := security.NewService(
		regclient.NewSecurityResolver(registryClients, securityTuning),
		securityCache, logger).
		WithDocuments(securityCache).
		// Replicating a release to a scanner that has to be told about it is
		// its own act, recorded in its own table. See security/replicate.go.
		WithRegistrations(securityRegistrations)
	securitySyncer := security.NewSyncer(securityService, packageSecurity, logger).
		WithDocuments(securityDocuments)
	// The requester turns `transfers create` and `transfers promote` into
	// rows; the expander plans the transfers those rows opened. Two halves of
	// one path, sharing the resolver so an origin the API accepted is one the
	// planner can read from.
	requester := transfer.NewRequester(packages, transferResolver)

	// Delegated replication: the service, the seam the expander branches on,
	// and the watcher that settles a transfer once the registry is done.
	replicationAudit := replication.NewStoreAuditor(packages)
	replicationMetric := replicationMetrics{m: mreg}
	replicationSvc := replication.NewService(
		replication.NewResolver(resolver, logger, "softwaregateway/"+info.Version),
		replicationStore, logger).
		WithObservability(replicationAudit, replicationMetric)
	replicationWatcher := replication.NewWatcher(
		replicationSvc, replicationStore, products, transferResolver, logger).
		WithObservability(replicationAudit, replicationMetric)

	// Native promotion: the seam the expander asks before planning a hop, the
	// store that carries what was claimed, and the loop that carries it out.
	//
	// It is what makes lab -> production on one Artifactory the seconds it
	// ought to be rather than a manifest walk and several thousand mounts. An
	// estate whose targets do not share a registry never claims, and every
	// promotion is the copy it always was.
	promotionStore := store.NewPromotions(st)
	promotionSvc := promotion.NewService(products, resolver, logger)
	promotionRunner := promotion.NewRunner(
		promotionSvc, promotionStore, packages, transferResolver, logger)

	queueCtl := queue.NewController(jobQueue, expanderAdapter{
		e: transfer.NewExpander(
			packages,
			transfer.NewPlanner(packages, cfg.Concurrency.PerRegistry, logger),
			transferResolver,
			0, logger,
		).WithDelegation(
			replication.NewDelegation(replicationSvc, products, replicationStore),
			replicationStore,
		).WithPromotion(promotionSvc, promotionStore),
	}, queue.ControllerOptions{
		ReapInterval:   cfg.Coordinator.Reaper.TickInterval,
		ExpandInterval: cfg.Coordinator.Scheduler.TickInterval,
	}, logger).WithStepper(replicationStore).
		WithPromoter(promoterAdapter{r: promotionRunner})

	// A package's manifest BODIES are the only thing recorded here that grows
	// without limit and can be discarded without losing a fact - they are a
	// cache in front of the source registry, and the tree they describe is kept
	// whatever happens to them. This is what bounds them. See
	// internal/store/manifestcache.go.
	cacheSweeper := maintenance.NewManifestCacheSweeper(packages,
		store.ManifestCachePolicy{
			BudgetBytes: cfg.Coordinator.ManifestCache.BudgetBytes,
			TTL:         cfg.Coordinator.ManifestCache.TTL,
		},
		cfg.Coordinator.ManifestCache.SweepInterval, logger, mreg)

	// The rendered charts a compliance run reuses. Derived data with a
	// deterministic recipe: an evicted entry costs one render to rebuild and can
	// never be WRONG, which is what makes an LRU acceptable here and not for
	// anything else in the schema. See internal/store/rendercache.go.
	renderSweeper := maintenance.NewRenderCacheSweeper(packages,
		store.RenderCachePolicy{
			TTL:    cfg.Coordinator.Compliance.RenderCacheTTL,
			Budget: cfg.Coordinator.Compliance.RenderCacheBytes,
		},
		cfg.Coordinator.Compliance.RenderCacheSweep, logger)

	// The security store keeps what it is told until the disk says otherwise.
	// Nothing here expires on a clock: rows past their retention become
	// evictable, and the sweep removes the least recently read ones only while
	// the store is over its budget. A budget of zero - the default - means it
	// never is. See internal/maintenance/security.go.
	securitySweeper := maintenance.NewSecurityCacheSweeper(
		securityCache, packageSecurity, cfg.Coordinator.Security.SweepInterval,
		store.CacheBudget{Bytes: cfg.Coordinator.Security.CacheBudgetBytes}, logger).
		WithRegistrations(securityRegistrations)

	// Compliance: does a release follow the organization's own Kubernetes and
	// CNF standards. Built here for the same reason security is - a run reaches
	// a vendor registry with credentials and shells out to helm on this host,
	// and nothing under internal/api may do either.
	//
	// A failure to load the policy catalogue is fatal, and deliberately so: a
	// Coordinator that started with no rules would report every release as
	// having nothing wrong with it, which is the one answer this feature must
	// never give by accident.
	var (
		policyCat        *policyCatalogue
		complianceRunner *compliance.Runner
		complianceSweep  *complianceSweeper
	)
	if cfg.Coordinator.Compliance.Enabled {
		var cerr error
		policyCat, complianceRunner, complianceSweep, cerr = buildCompliance(
			cfg.Coordinator.Compliance, packages, blobsImpl{transferResolver},
			complianceClassifier(products, layouts), logger)
		if cerr != nil {
			return fmt.Errorf("compliance: %w", cerr)
		}
	} else {
		logger.Info("compliance is disabled in configuration")
	}

	retentionSweeper := maintenance.NewRetentionSweeper(packages,
		store.RetentionPolicy{
			Transfers:   cfg.Coordinator.GC.Transfers,
			WorkerLogs:  cfg.Coordinator.GC.WorkerLogs,
			AuditEvents: cfg.Coordinator.GC.AuditEvents,
			Placements:  cfg.Coordinator.GC.Placements,
			// A COUNT, not a duration: what a release's compliance history is
			// for is "what did this look like the last few times", and a
			// release checked once eight months ago must keep that one run.
			ComplianceRuns: cfg.Coordinator.GC.ComplianceRuns,
			BatchSize:      cfg.Coordinator.GC.BatchSize,
		},
		cfg.Coordinator.GC.TickInterval, logger)

	watcher := product.NewWatcher(cfg.ProductsDir(), loader, products, product.WatchOptions{
		Logger: logger,
		OnReload: func(res product.LoadResult) {
			mreg.ConfigProductsLoaded.WithLabelValues().Set(float64(len(res.Valid)))
			mreg.ConfigLoadErrors.Reset()
			for _, bad := range res.Invalid {
				name := bad.Name
				if name == "" {
					name = bad.File
				}
				mreg.ConfigLoadErrors.WithLabelValues(name).Set(1)
			}
			mreg.ConfigLastReload.SetToCurrentTime()

			// Give the loaded configuration database identity. `packages`
			// carries foreign keys to `products` and `repositories`, so
			// discovery cannot record anything until these rows exist.
			//
			// A reconcile failure is logged, not fatal: the API and the
			// already-loaded configuration keep working, and the next reload
			// retries. Refusing to serve because one product's repository is
			// contested would be a worse outcome than continuing.
			reconcileCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			r, err := cat.Reconcile(reconcileCtx, res.Valid)
			if err != nil {
				logger.Error("catalog: reconcile failed; database catalog may be stale", "error", err)
				// Discovery is deliberately NOT updated: it keeps polling with
				// the last configuration that reconciled successfully. Handing
				// it row IDs from a failed reconcile would be worse than
				// running slightly stale.
				return
			}
			logger.Info("catalog: reconciled",
				"products", r.ProductsSeen, "repositories", r.ReposSeen, "deactivated", r.Deactivated)

			refs := make(map[string]discovery.ProductRef, len(r.Products))
			for name, p := range r.Products {
				refs[name] = discovery.ProductRef{ID: p.ID, Repositories: p.Repositories}
			}
			discoveryCtl.SetConfig(res.Valid, refs)
		},
	})

	// A directory-level failure is fatal; individual invalid products are not.
	// The Coordinator must stay up and serve the API even when every product
	// is invalid - a crash-looping process cannot tell anyone why it is
	// unhappy. See docs/design/02-configuration.md section 7.
	if err := watcher.LoadOnce(); err != nil {
		return fmt.Errorf("load products: %w", err)
	}

	// ---- health ----
	hreg := health.New()
	// Liveness is process-local ONLY. The probe signature makes a dependency
	// call impossible; see internal/platform/health.
	hreg.AddLiveness("process", func() error { return nil })
	hreg.AddReadiness("database", func(ctx context.Context) health.Result {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if err := st.Ping(ctx); err != nil {
			return health.Down(err)
		}
		return health.OK(string(st.Driver()))
	})
	hreg.AddReadiness("configuration", func(context.Context) health.Result {
		if bad := products.Invalid(); len(bad) > 0 {
			return health.Degraded(fmt.Sprintf("%d product(s) failed to load", len(bad)))
		}
		return health.OK(fmt.Sprintf("%d product(s) loaded", products.Count()))
	})
	// The database being REACHABLE is not the same question as it being the
	// shape this binary was built against, and only the second one decides
	// whether this replica can serve. A replica running ahead of its schema
	// connects, reports itself ready, and then fails on the first query naming
	// a column nobody has added yet - which reaches an operator as a scatter
	// of 500s across unrelated endpoints rather than as a replica that never
	// went ready. During a rolling deployment that is the normal case, not an
	// exotic one.
	wantSchema, schemaErr := store.ExpectedSchemaVersion(st.Driver())
	hreg.AddReadiness("schema", func(ctx context.Context) health.Result {
		if schemaErr != nil {
			return health.Down(schemaErr)
		}
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		have, err := st.SchemaVersion(ctx)
		if err != nil {
			return health.Down(err)
		}
		if have < wantSchema {
			return health.Down(fmt.Errorf(
				"database is at migration %d, this build expects %d", have, wantSchema))
		}
		// AHEAD is degraded rather than down, and the distinction is a rolling
		// deployment: the new replicas migrate first and the old ones keep
		// serving against a schema with columns they do not know about, which
		// is exactly what a forward-only migration policy is FOR. Refusing
		// traffic here would take the old replicas out during every upgrade.
		if have > wantSchema {
			return health.Degraded(fmt.Sprintf(
				"database is at migration %d, ahead of this build's %d", have, wantSchema))
		}
		return health.OK(fmt.Sprintf("migration %d", have))
	})

	// ---- leader election ----
	// THE SERVICE'S OWN RECORD OF WHEN IT WAS SERVING, which is what the
	// Overview page's availability panel reads. Not leader-gated: each replica
	// records its own runs, because a recorder that went quiet when the leader
	// died would miss the one event it exists to capture. See
	// internal/maintenance/availability.go.
	availabilityRecorder := maintenance.NewAvailabilityRecorder(
		store.NewAvailability(st), hreg,
		"coordinator", coordinatorInstance(), version.Version,
		store.DefaultAvailabilityInterval, logger)

	var elector leader.Interface
	if cfg.Coordinator.LeaderElection.Enabled && st.SupportsAdvisoryLocks() {
		elector = leader.New(st.DB(), leader.Options{
			LockID:        cfg.Coordinator.LeaderElection.LockID,
			RetryInterval: cfg.Coordinator.LeaderElection.RetryInterval,
			Logger:        logger,
			OnChange: func(isLeader bool) {
				if isLeader {
					mreg.LeaderElected.Set(1)
				} else {
					mreg.LeaderElected.Set(0)
				}
				discoveryCtl.SetLeader(isLeader)
				cacheSweeper.SetLeader(isLeader)
				renderSweeper.SetLeader(isLeader)
				renderSweeper.SetLeader(isLeader)
				retentionSweeper.SetLeader(isLeader)
				securitySweeper.SetLeader(isLeader)
				if complianceSweep != nil {
					complianceSweep.SetLeader(isLeader)
				}
				replicationWatcher.SetLeader(isLeader)
				// Recording is not gated; trimming the history is.
				availabilityRecorder.SetLeader(isLeader)
			},
		})
	} else {
		// SQLite, or election disabled: a single process has nothing to
		// contend with, so leadership is unconditional.
		elector = leader.NewAlwaysLeader(func(isLeader bool) {
			if isLeader {
				mreg.LeaderElected.Set(1)
			} else {
				mreg.LeaderElected.Set(0)
			}
			discoveryCtl.SetLeader(isLeader)
			cacheSweeper.SetLeader(isLeader)
			retentionSweeper.SetLeader(isLeader)
			securitySweeper.SetLeader(isLeader)
			if complianceSweep != nil {
				complianceSweep.SetLeader(isLeader)
			}
			replicationWatcher.SetLeader(isLeader)
			queueCtl.SetLeader(isLeader)
			availabilityRecorder.SetLeader(isLeader)
		})
	}

	// ---- HTTP ----
	// Authentication. Off by default so an existing deployment upgrades
	// unchanged; the shipped compose file turns it on. Failing here rather
	// than at the first request is deliberate: a Coordinator that starts with
	// broken auth configuration looks healthy while refusing everyone.
	var authenticator middleware.Authenticator
	var policyEngine authz.Engine
	if cfg.Auth.Enabled {
		a, err := middleware.NewOIDCAuthenticator(ctx, middleware.OIDCOptions{
			Issuer:       cfg.Auth.Issuer,
			DiscoveryURL: cfg.Auth.DiscoveryURL,
			HostHeader:   cfg.Auth.HostHeader,
			Audience:     cfg.Auth.Audience,
			Tenant:       cfg.Auth.Tenant,
			CerbosAddr:   cfg.Auth.CerbosAddr,
			SkipIssuer:   cfg.Auth.SkipIssuerCheck,
		})
		if err != nil {
			return fmt.Errorf("authentication is enabled but not usable: %w", err)
		}
		authenticator = a
		policyEngine = a.Engine
		logger.Info("authentication enabled",
			"issuer", cfg.Auth.Issuer,
			"tenant", orUnset(cfg.Auth.Tenant),
			"authorization", map[bool]string{true: "cerbos", false: "roles only"}[cfg.Auth.CerbosAddr != ""])

		// TWO BOUNDARIES THIS PROCESS IS NOT ENFORCING, said at startup rather
		// than left to be discovered.
		//
		// Neither is visible from inside a request: every token still verifies,
		// every screen still loads, and what is missing only shows up as a
		// caller who should not have been let in at all. An issuer signs for
		// every application and every organization it hosts with the same keys,
		// so a signature proves who MINTED the token and nothing about who it
		// was minted for.
		if cfg.Auth.Tenant == "" {
			logger.Warn("no tenant boundary: a token from any organization at this issuer "+
				"is accepted, and its roles are read as if they had been granted here",
				"remedy", "set SWGW_AUTH_TENANT to this deployment's organization name")
		}
		if cfg.Auth.Audience == "" {
			logger.Warn("no audience check: a token minted for a different application at "+
				"this issuer is accepted",
				"remedy", "set SWGW_AUTH_AUDIENCE to this deployment's OIDC client or project id")
		}

		// DEEP, never readiness, and the reason is the same for both of them:
		// each one is a dependency this process TOLERATES losing for a while.
		// The key set is cached, so an identity provider that goes away breaks
		// nothing until a key rotates; the policy engine already fails closed,
		// so a Coordinator without one is refusing exactly what it cannot
		// authorize. Gating readiness on either would convert a blip in a
		// dependency into every replica leaving the endpoints at once, which
		// removes the service that was still half working AND the page that
		// would have explained why.
		//
		// They belong in the deep check because that is where an operator
		// asking "why is nobody able to sign in" looks, and because without
		// them the answer to that question was a token failure hours after the
		// network change that caused it.
		hreg.AddDeep("identity", func(ctx context.Context) health.Result {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err := a.Verifier.Health(ctx); err != nil {
				return health.Down(err)
			}
			return health.OK("signing keys from " + a.Verifier.KeysURL())
		})
		if prober, ok := a.Engine.(authz.Prober); ok {
			hreg.AddDeep("authorization", func(ctx context.Context) health.Result {
				ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				if err := prober.Health(ctx); err != nil {
					return health.Down(err)
				}
				return health.OK("cerbos at " + cfg.Auth.CerbosAddr)
			})
		}
	} else {
		logger.Warn("AUTHENTICATION IS DISABLED - every caller holds admin. " +
			"Safe only behind a NetworkPolicy; see docs/design/24.")
	}

	srv := api.NewServer(api.Deps{
		Authenticator: authenticator,
		// The policy engine, if one is configured. Handed to the API rather
		// than kept on the authenticator: establishing WHO somebody is and
		// deciding WHAT they may do are two jobs, and only the second belongs
		// in front of every route.
		Engine:   policyEngine,
		Logger:   logger,
		Metrics:  mreg,
		Health:   hreg,
		Products: products,
		Store:    st,
		Packages: packages,
		// The vendor layouts, so an artifact listing can report a vendor's
		// Helm charts as charts rather than as images. See
		// Server.artifactClassifier.
		Vendors:   layouts,
		Discovery: discoveryCtl.Loop(),
		Queue:     jobQueue,
		Requests:  requester,
		// Connectivity checking is deliberately NOT part of the health
		// registry: health must not depend on third-party registries, or a
		// vendor's outage pulls this replica out of the Service.
		Preflight: preflight.NewChecker(resolver),
		// Calibration runs here for the same reason preflight does: transferctl
		// is a pure API client and must not open a connection to a registry
		// itself. Every report says which host measured it, because that is the
		// caveat - a Coordinator on a different network from the workers
		// measures a path no transfer takes.
		Calibrator: calibrate.NewCalibrator(resolver),
		// Comparison runs here for the same reason: it opens connections to the
		// DESTINATION registry, and transferctl is a pure API client.
		Comparer: compareImpl{transferResolver, layouts},
		// Security runs here for the same reason as the three above: the sync
		// needs a credentialed client and the API layer holds none.
		//
		// Split in three because the parts fail differently. The syncer needs a
		// reachable scanner; the store and the index need only the database. A
		// release's stored findings stay readable, comparable and searchable
		// while the scanner is down - which is exactly when somebody looks at
		// them.
		SecuritySync:      securitySyncer,
		SecurityStore:     securitySecurityStore{packageSecurity, securityCache},
		SecurityIndex:     securityCache,
		SecurityRetention: securityRetention,
		SecurityFreshness: securityFreshness,
		// The on-demand half: an SBOM a sync deliberately did not fetch,
		// generated when somebody presses the button beside an image.
		SecurityDocuments: securityService,
		// Replicating a release to a scanner that has to be told about it, and
		// reading what that scanner holds. Two dependencies because they fail
		// differently: running one needs a reachable scanner, reading the state
		// needs only the database.
		SecurityReplicate:     securityService,
		SecurityRegistrations: securityRegistrations,
		// Compliance, split the same three ways and for the same reason. The
		// runner needs a reachable registry and a helm binary; the store and
		// the catalogue need neither. A release's findings and the rulebook
		// stay readable when a run could not happen - which is exactly when
		// somebody is working out why a release was blocked.
		ComplianceRunner: complianceAPIRunner(complianceRunner),
		ComplianceStore:  packages,
		// The manifests a run judged, so a finding can be SHOWN. Same store as
		// the results, and a separate seam because it is separately absent: a
		// deployment can turn the keeping of them off, and a run recorded
		// before they were kept has none.
		ComplianceEvidence:  packages,
		ComplianceCatalogue: complianceAPICatalogue(policyCat),
		ComplianceHelm:      complianceAPIHelm(cfg.Coordinator.Compliance),
		// Reading one file out of a release, for somebody looking at it. Here
		// for the third time for the first reason: it needs a credentialed
		// client, and the API layer holds none.
		Blobs:                blobsImpl{transferResolver},
		FileDownloadsEnabled: cfg.Coordinator.Files.DownloadEnabled,
		// How a promotion would be carried out, which the dialog asks before
		// anybody commits to anything. Here for the same reason again: the
		// plugin claim reads a target's resolved credential and registry type,
		// and the API layer holds neither.
		//
		// The STORE is separate, and deliberately: reading what a promotion
		// did is a database query, and it must keep answering on a replica
		// that cannot resolve a credential at all.
		Promotions:     promotionSvc,
		PromotionStore: promotionStore,
		// Delegated replication runs here for the same reason again: it speaks
		// to Quay's MANAGEMENT api, which needs a credential from a projected
		// Secret, and transferctl holds neither.
		Replication: replicationSvc,
		// Downloads and auto-download rules read configuration; running a
		// download also needs catalog rows, which is why the two are separate
		// dependencies.
		Downloads:        download.NewService(packages, replicationStore, logger),
		TargetRows:       transferResolver,
		ReplicationStore: replicationStore,
		Leader:           elector,
		Component:        component,
		// The same record the recorder above writes, read back for the
		// Overview page's availability panel.
		Availability: store.NewAvailability(st),
	})

	httpServer := &http.Server{
		Addr:              cfg.Server.Address,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: it would cap long-poll and streaming responses in
		// later milestones. Per-handler timeouts are the right granularity.
		IdleTimeout: 120 * time.Second,
	}

	g, gctx := errgroup.WithContext(ctx)

	// THE PROFILER, on a listener of its own and only when asked for.
	//
	// Never on the API server: a profiler hands the heap - registry
	// credentials included - to anybody who can reach it, and a CPU profile is
	// a denial of service on request. See config.ProfilingConfig for the whole
	// argument, and internal/compliance/cel for the NET-06 check that fails a
	// deployment which publishes one of these paths.
	var profileServer *http.Server
	if cfg.Observability.Profiling.Enabled {
		addr := cfg.Observability.Profiling.Address
		// Only the pprof handlers, on a mux of their own: http.DefaultServeMux
		// is where net/http/pprof registers itself, and serving that mux would
		// also serve whatever any other package has registered on it.
		mux := http.NewServeMux()
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

		profileServer = &http.Server{
			Addr: addr, Handler: mux,
			ReadHeaderTimeout: 10 * time.Second,
			// No WriteTimeout: a CPU profile is a thirty-second response by
			// default and a deadline here would truncate every one of them.
		}
		if !loopbackAddr(addr) {
			logger.Warn("THE PROFILER IS NOT ON LOOPBACK - anything that can reach "+
				"this address can read the heap of this process, which holds registry "+
				"credentials", "address", addr,
				"setting", "observability.profiling.address")
		}
		g.Go(func() error {
			logger.Info("profiler listening", "address", addr, "path", "/debug/pprof/")
			if err := profileServer.ListenAndServe(); err != nil &&
				!errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("profile server: %w", err)
			}
			return nil
		})
	}

	g.Go(func() error {
		logger.Info("http listening", "address", cfg.Server.Address)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	})

	// The change feed behind GET /api/v1/events. It queries nothing while
	// nobody is subscribed, so an estate nobody is watching costs nothing.
	g.Go(func() error { srv.WatchTransfers(gctx); return nil })

	g.Go(func() error { return elector.Run(gctx) })
	g.Go(func() error { return watcher.Run(gctx) })
	g.Go(func() error { return discoveryCtl.Run(gctx) })
	g.Go(func() error { return cacheSweeper.Run(gctx) })
	g.Go(func() error { return renderSweeper.Run(gctx) })
	g.Go(func() error { return retentionSweeper.Run(gctx) })
	g.Go(func() error { return securitySweeper.Run(gctx) })
	if complianceSweep != nil {
		g.Go(func() error { complianceSweep.Run(gctx); return nil })
	}
	g.Go(func() error { return queueCtl.Run(gctx) })
	g.Go(func() error { return replicationWatcher.Run(gctx) })
	g.Go(func() error { return availabilityRecorder.Run(gctx) })

	// The queue's own gauges. Every other metric in this process measures the
	// service that fronts the work; this is the work.
	g.Go(func() error {
		return newQueueSampler(packages, mreg, logger).
			withLogShipper(logShipper).Run(gctx)
	})

	// Graceful shutdown: stop accepting, drain in-flight requests, then exit.
	g.Go(func() error {
		<-gctx.Done()
		logger.Info("shutting down", "grace", cfg.Server.ShutdownGracePeriod)

		shutdownCtx, cancel := context.WithTimeout(
			context.WithoutCancel(gctx), cfg.Server.ShutdownGracePeriod)
		defer cancel()

		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Warn("http shutdown", "error", err)
		}
		if profileServer != nil {
			if err := profileServer.Shutdown(shutdownCtx); err != nil {
				logger.Warn("profiler shutdown", "error", err)
			}
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		return err
	}
	logger.Info("stopped")
	return nil
}

// securitySecurityStore joins the two halves of a security read behind the one
// interface the API asks for.
//
// The per-release row and the per-artifact reports live in different tables
// with different lifetimes - one is the durable result of a sync, the other the
// index behind it - and the API needs both to answer one request. Composing
// them here rather than merging the tables keeps each store owning its own
// retention.
type securitySecurityStore struct {
	*store.PackageSecurity
	reports *store.Security
}

func (s securitySecurityStore) ReportsFor(
	ctx context.Context, scope security.Scope,
	refs []security.ArtifactRef, detail security.Detail,
) ([]security.Report, error) {
	return s.reports.ReportsFor(ctx, scope, refs, detail)
}

func (s securitySecurityStore) LoadDocuments(
	ctx context.Context, scope security.Scope,
	refs []security.ArtifactRef, kinds []security.DocumentKind,
) (map[string]map[security.DocumentKind]security.Document, error) {
	return s.reports.LoadDocuments(ctx, scope, refs, kinds)
}

func (s securitySecurityStore) LoadSources(
	ctx context.Context, packageID int64,
) ([]security.SourceCounts, error) {
	return s.PackageSecurity.LoadSources(ctx, packageID)
}

func (s securitySecurityStore) DocumentSummaries(
	ctx context.Context, scope security.Scope, refs []security.ArtifactRef,
) (map[string][]security.DocumentSummary, error) {
	return s.reports.DocumentSummaries(ctx, scope, refs)
}

// anchoreTuning resolves the deployment's Anchore stanza into the values the
// security resolver needs, including its credential.
//
// # Why a missing credential is a warning rather than a failed start
//
// Because an unreachable scanner must not be an outage. A Coordinator that
// refused to start over a secret that has not been projected yet takes down
// replication, discovery, promotion and every read of everything already
// scanned - to protect a feature whose absence is one tab. So the endpoint is
// dropped, the reason is logged once at startup, and a repository asking for
// Anchore is told this Coordinator has none configured.
func anchoreTuning(
	cfg config.AnchoreConfig, secrets *product.SecretResolver, log *slog.Logger,
) regclient.AnchoreTuning {
	if !cfg.Available() {
		return regclient.AnchoreTuning{}
	}

	tuning := regclient.AnchoreTuning{
		Endpoint:           cfg.Endpoint,
		Account:            cfg.Account,
		InsecureSkipVerify: cfg.SkipsTLSVerification(),
		Concurrency:        cfg.Concurrency,
		RequestTimeout:     cfg.RequestTimeout,
		Submit:             cfg.SubmitImages(),
		Grouping:           cfg.Grouping(),
		SBOMFormat:         cfg.SBOMFormat,
	}

	if cfg.SecretName == "" {
		log.Warn("anchore is configured with no credential and will not be used",
			"endpoint", cfg.Endpoint,
			"fix", "set coordinator.security.anchore.secretName to a projected secret holding "+
				"a username and password, or an API key in the password key")
		return regclient.AnchoreTuning{}
	}
	creds, err := secrets.Credentials(product.CredentialsRef{
		SecretName:  cfg.SecretName,
		UsernameKey: cfg.UsernameKey,
		PasswordKey: cfg.PasswordKey,
	})
	if err != nil {
		log.Warn("anchore credential could not be read; anchore will not be used",
			"endpoint", cfg.Endpoint, "secret", cfg.SecretName, "error", err)
		return regclient.AnchoreTuning{}
	}
	tuning.Username = creds.Username
	tuning.Password = creds.Password.Reveal()

	log.Info("anchore is available for products that enable it",
		"endpoint", cfg.Endpoint, "submit", tuning.Submit, "grouping", tuning.Grouping)
	return tuning
}

// loopbackAddr reports whether a listen address is reachable only from this
// pod, which is what decides whether the profiler needs a warning.
//
// The HOST half only: a port says nothing about who can reach it. An empty
// host is the case that matters most - ":6060" binds every interface, and it
// is the shortest thing somebody types.
func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// Not a host:port at all. Warning is the safe answer for something
		// nobody can read, and the listener itself will fail on it anyway.
		return false
	}
	switch host {
	case "":
		// ":6060" - every interface, including the pod network.
		return false
	case "localhost":
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// coordinatorInstance names this process in the availability record.
//
// It MUST differ between replicas and stay the same across a beat: two
// replicas sharing a name interleave their beats into one run, which would
// report a rolling restart as uninterrupted service - the one claim that
// record must never make falsely. A hostname is what Kubernetes gives each pod
// and what Docker gives each container, so it is both by default.
//
// The fallback is deliberately not a random id. A process that restarts under
// a generated name leaves a run nothing will ever extend, and the record fills
// with orphans that look like a fleet.
func coordinatorInstance() string {
	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}
	return "coordinator"
}
