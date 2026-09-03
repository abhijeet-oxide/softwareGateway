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
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/abhijeet-oxide/softwareGateway/internal/api"
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

	// Registers the JFrog promoter with the plugin registry, via its init.
	//
	// THE ONE PLACE A PROMOTER IS NAMED, and it is the same arrangement the
	// vendor layout above uses. Everything downstream depends on the seam in
	// internal/transfer and is forbidden by depguard from importing an
	// implementation, so `grep -rn "promote/jfrog"` finding only this file is
	// the mechanical form of "the engine does not know what Artifactory is".
	// Deleting internal/promote/jfrog must leave the rest building and
	// passing, with every promotion falling back to a copy.
	_ "github.com/abhijeet-oxide/softwareGateway/internal/promote/jfrog"
)

const component = "coordinator"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "coordinator: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "", "path to the system configuration file")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Get(component))
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}

	logger := plog.New(plog.Config{
		Level:  cfg.Observability.Log.Level,
		Format: cfg.Observability.Log.Format,
	}, os.Stdout, component)

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
	jobQueue := queue.New(packages, cfg.Coordinator.Reaper.LeaseDuration, logger)

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

	// ---- leader election ----
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
		})
	}

	// ---- HTTP ----
	srv := api.NewServer(api.Deps{
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

	g.Go(func() error {
		logger.Info("http listening", "address", cfg.Server.Address)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	})

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
		Endpoint:       cfg.Endpoint,
		Account:        cfg.Account,
		Concurrency:    cfg.Concurrency,
		RequestTimeout: cfg.RequestTimeout,
		Submit:         cfg.SubmitImages(),
		Grouping:       cfg.Grouping(),
		SBOMFormat:     cfg.SBOMFormat,
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
