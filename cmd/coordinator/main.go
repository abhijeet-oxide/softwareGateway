// Command coordinator is the softwareGateway control plane.
//
// It owns all state and is the sole writer to the database. Two replicas run
// for API availability; one holds a pg_advisory_lock and runs the background
// loops. See docs/design/00-overview.md section 5.1.
//
// This binary contains wiring only — construct, inject, run. Logic in main is
// untestable. See docs/design/15-code-layout.md section 3.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/abhijeet-oxide/softwareGateway/internal/api"
	"github.com/abhijeet-oxide/softwareGateway/internal/calibrate"
	"github.com/abhijeet-oxide/softwareGateway/internal/catalog"
	"github.com/abhijeet-oxide/softwareGateway/internal/discovery"
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
	"github.com/abhijeet-oxide/softwareGateway/internal/queue"
	"github.com/abhijeet-oxide/softwareGateway/internal/regclient"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/transfer"
	"github.com/abhijeet-oxide/softwareGateway/internal/vendors"
	"github.com/abhijeet-oxide/softwareGateway/internal/vendors/near"
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
		logger.Warn("using the SQLite driver — DEVELOPMENT ONLY, not supported in production",
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

	// ---- the queue ----
	//
	// The Coordinator is the SOLE database writer, so everything a worker does
	// to the queue passes through here: leases out, results back. Bytes do not.
	jobQueue := queue.New(packages, cfg.Coordinator.Reaper.LeaseDuration, logger)

	transferResolver := &resolverImpl{
		products: products,
		catalog:  cat,
		packages: packages,
		clients:  regclient.NewClients(products, resolver, cfg.ProductsDir(), logger),
		log:      logger,
	}
	// The requester turns `transfers create` and `transfers promote` into
	// rows; the expander plans the transfers those rows opened. Two halves of
	// one path, sharing the resolver so an origin the API accepted is one the
	// planner can read from.
	requester := transfer.NewRequester(packages, transferResolver)

	queueCtl := queue.NewController(jobQueue, expanderAdapter{
		e: transfer.NewExpander(
			packages,
			transfer.NewPlanner(packages, cfg.Concurrency.PerRegistry, logger),
			transferResolver,
			0, logger),
	}, queue.ControllerOptions{
		ReapInterval:   cfg.Coordinator.Reaper.TickInterval,
		ExpandInterval: cfg.Coordinator.Scheduler.TickInterval,
	}, logger)

	// A package's manifest BODIES are the only thing recorded here that grows
	// without limit and can be discarded without losing a fact — they are a
	// cache in front of the source registry, and the tree they describe is kept
	// whatever happens to them. This is what bounds them. See
	// internal/store/manifestcache.go.
	cacheSweeper := maintenance.NewManifestCacheSweeper(packages,
		store.ManifestCachePolicy{
			BudgetBytes: cfg.Coordinator.ManifestCache.BudgetBytes,
			TTL:         cfg.Coordinator.ManifestCache.TTL,
		},
		cfg.Coordinator.ManifestCache.SweepInterval, logger, mreg)

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
	// is invalid — a crash-looping process cannot tell anyone why it is
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
			queueCtl.SetLeader(isLeader)
		})
	}

	// ---- HTTP ----
	srv := api.NewServer(api.Deps{
		Logger:    logger,
		Metrics:   mreg,
		Health:    hreg,
		Products:  products,
		Store:     st,
		Packages:  packages,
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
		// caveat — a Coordinator on a different network from the workers
		// measures a path no transfer takes.
		Calibrator: calibrate.NewCalibrator(resolver),
		Leader:     elector,
		Component:  component,
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
	g.Go(func() error { return queueCtl.Run(gctx) })

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
