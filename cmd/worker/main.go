// Command worker is the softwareGateway data plane.
//
// Workers are stateless: they hold no database credentials and contain no SQL.
// They lease jobs from the Coordinator over HTTP and stream blobs directly
// between registries. See docs/design/00-overview.md section 5.2.
//
// M1 SCOPE: this binary starts, registers its identity, serves its own probes
// and metrics, and idles. The lease loop, the transfer engine and heartbeating
// arrive in M3 with the vertical slice.
//
// It deliberately does not pretend to do more than it does: there is no fake
// lease loop and no stub that reports progress it never made.
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
	logger.Warn("M1: the lease loop is not implemented; this worker will idle")

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

	hreg := health.New()
	// Liveness: the main loop is ticking. In M3 this becomes a staleness check
	// on the lease loop, which is what catches a wedged worker holding leases
	// it will never progress — see docs/design/11 section 2.1.
	hreg.AddLiveness("process", func() error { return nil })
	// Readiness in M1 is "the process is up". In M3 it becomes "registered
	// with the Coordinator", so a worker that cannot reach the control plane
	// leaves rotation instead of appearing healthy.
	hreg.AddReadiness("process", func(context.Context) health.Result {
		return health.OK("worker running; lease loop lands in M3")
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
		<-gctx.Done()
		// In M3 this is where the worker stops leasing and drains in-flight
		// blobs. A blob that outlives the grace period is killed and its lease
		// simply expires, which is also correct — just less efficient.
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
