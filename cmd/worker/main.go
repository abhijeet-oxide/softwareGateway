// Command worker is the softwareGateway data plane.
//
// Workers are stateless: they hold no database credentials and contain no SQL.
// They lease jobs from the Coordinator over HTTP and stream blobs directly
// between registries. See docs/design/00-overview.md section 5.2.
//
// It leases jobs from the Coordinator, streams the bytes, and reports what
// happened. Product configuration and projected secrets are read from the same
// mounts the Coordinator reads, which is what lets a lease response carry a
// credential REFERENCE rather than a credential.
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

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/config"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/health"
	plog "github.com/abhijeet-oxide/softwareGateway/internal/platform/log"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/metrics"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/tlscompat"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/tracing"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/version"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/regclient"
	"github.com/abhijeet-oxide/softwareGateway/internal/worker"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

const component = "worker"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "worker: %v\n", err)
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

	workerID := cfg.Worker.WorkerID
	if workerID == "" {
		// Defaults to the pod name in Kubernetes, the hostname elsewhere.
		if h, err := os.Hostname(); err == nil {
			workerID = h
		} else {
			workerID = "worker-unknown"
		}
	}

	logger := plog.New(plog.Config{
		Level:  cfg.Observability.Log.Level,
		Format: cfg.Observability.Log.Format,
	}, os.Stdout, component).With(plog.KeyWorkerID, workerID)

	info := version.Get(component)
	logger.Info("starting",
		"version", info.Version,
		"commit", info.Commit,
		"coordinator", cfg.Worker.CoordinatorEndpoint,
		"max_concurrent_jobs", cfg.Worker.MaxConcurrentJobs,
	)

	// Workers pull and push blobs, so they hit the same registries the
	// Coordinator discovers on. The setting has to be applied in both processes
	// or discovery succeeds and every transfer fails at the handshake.
	tlscompat.Apply(tlscompat.Options{
		AllowNegativeSerialNumbers: cfg.TLS.AllowNegativeSerialNumbers,
	}, logger)
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

	// Product configuration, for resolving a job's endpoints into clients.
	// A worker with no products configured can lease nothing it could execute,
	// so this is fatal rather than a warning.
	secrets := product.NewSecretResolver(cfg.SecretsDir())
	products := product.NewRegistry()
	loaded, err := product.NewLoader(cfg.ProductsDir(), secrets).
		WithConcurrency(product.Concurrency{
			PerRegistry:       cfg.Concurrency.PerRegistry,
			RequestsPerSecond: cfg.Concurrency.RequestsPerSecond,
		}).Load()
	if err != nil {
		return fmt.Errorf("load product configuration from %s: %w", cfg.ProductsDir(), err)
	}
	products.Swap(loaded)

	// A worker with no products can lease nothing it could execute. Refusing to
	// start is the whole point: the alternative is what happened on the first
	// real run - the worker came up, leased two and a half thousand jobs, and
	// failed every one of them, consuming an attempt each time because attempts
	// are counted at LEASE time. A worker that cannot do the work must not take
	// it.
	//
	// The loader returns no error for a missing directory, which is right for
	// the Coordinator - configuration can arrive later via the watcher - and
	// wrong here, so the check belongs at this end.
	if products.Count() == 0 {
		return fmt.Errorf(
			"no product configuration in %s: this worker would lease jobs it cannot execute"+
				"\npoint it at the same configuration the Coordinator uses, with --config",
			cfg.ProductsDir())
	}
	logger.Info("product configuration loaded",
		"products", products.Count(), "invalid", len(products.Invalid()),
		"dir", cfg.ProductsDir())

	coordinator := v1.NewClient(cfg.Worker.CoordinatorEndpoint,
		v1.WithUserAgent("softwaregateway-worker/"+info.Version))

	loop := worker.NewLoop(
		coordinator,
		regclient.NewClients(products, secrets, cfg.ProductsDir(), logger),
		worker.Options{
			WorkerID:          workerID,
			Version:           info.Version,
			MaxConcurrentJobs: cfg.Worker.MaxConcurrentJobs,
			CopyBufferSize:    int(cfg.Worker.CopyBufferSize),
			HeartbeatInterval: cfg.Worker.HeartbeatInterval,
			StallTimeout:      cfg.Worker.StallTimeout,
		},
		logger,
	)

	hreg := health.New()
	// Liveness is deliberately NOT a check on the Coordinator. A control-plane
	// blip must not crash-loop the fleet: a worker that cannot reach the
	// Coordinator keeps transferring what it already holds, which is the
	// correct behaviour (docs/design/09 §9.1).
	hreg.AddLiveness("process", func() error { return nil })
	hreg.AddReadiness("coordinator", func(ctx context.Context) health.Result {
		if _, err := coordinator.Version(ctx); err != nil {
			return health.Down(err)
		}
		return health.OK("leasing from " + cfg.Worker.CoordinatorEndpoint)
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if err := hreg.Live(); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		rep := hreg.Ready(r.Context())
		if rep.Status != health.StatusHealthy {
			http.Error(w, string(rep.Status), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.Handle("/metrics", promhttp.HandlerFor(
		mreg.Prometheus(), promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError}))

	httpServer := &http.Server{
		Addr:              cfg.Worker.Address,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		logger.Info("probe server listening", "address", cfg.Worker.Address)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("probe server: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		return loop.Run(gctx)
	})

	g.Go(func() error {
		<-gctx.Done()
		// Stopping is not a drain protocol. In-flight blobs are abandoned and
		// their leases expire, which the Coordinator's reaper handles as the
		// ordinary case - it is the same path a SIGKILL takes, so it is the
		// one that has to work anyway.
		logger.Info("draining")

		shutdownCtx, cancel := context.WithTimeout(
			context.WithoutCancel(gctx), cfg.Server.ShutdownGracePeriod)
		defer cancel()

		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Warn("probe server shutdown", "error", err)
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		return err
	}
	logger.Info("stopped")
	return nil
}
