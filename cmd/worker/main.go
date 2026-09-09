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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
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
	"github.com/abhijeet-oxide/softwareGateway/pkg/authz"
)

const component = "worker"

// probeReadiness calls /readyz on the local listener. Used by -health-check.
func probeReadiness() error {
	addr := os.Getenv("SWGW_WORKER_ADDRESS")
	if addr == "" {
		addr = ":8081"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	// NewRequestWithContext rather than Client.Get: the linter forbids the
	// context-less helpers, and a probe that cannot be cancelled is exactly
	// the kind that hangs a container healthcheck.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/readyz", nil)
	if err != nil {
		return fmt.Errorf("not ready: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
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
		fmt.Fprintf(os.Stderr, "worker: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "", "path to the system configuration file")
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

	workerID := workerName(cfg.Worker.WorkerID, cfg.Worker.Name)

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
	// The loader is kept, not discarded after the first Load: the watcher below
	// reuses THIS one, so a reload applies the same concurrency defaults the
	// startup load did. A second loader built with different arguments would
	// silently change a product's connection ceiling the first time somebody
	// touched an unrelated file.
	loader := product.NewLoader(cfg.ProductsDir(), secrets).
		WithConcurrency(product.Concurrency{
			PerRegistry:       cfg.Concurrency.PerRegistry,
			RequestsPerSecond: cfg.Concurrency.RequestsPerSecond,
		})
	loaded, err := loader.Load()
	if err != nil {
		return fmt.Errorf("load product configuration from %s: %w", cfg.ProductsDir(), err)
	}
	products.Swap(loaded)

	// A WORKER WITH NO PRODUCTS DOES NOT LEASE. It still starts.
	//
	// The rule worth keeping is that a worker which cannot do the work must not
	// take it: on the first real run one came up with no configuration, leased
	// two and a half thousand jobs and failed every one, consuming an attempt
	// each time because attempts are counted at LEASE time.
	//
	// That was implemented as refusing to START, and those are different
	// things. Refusing to start makes the data plane depend on configuration
	// arriving first, which is the opposite of what a deployment wants: a
	// product added at ten past three should be picked up by a fleet that has
	// been running since Tuesday, and a fleet that cannot boot until somebody
	// writes a product file is a fleet that cannot be rolled out ahead of one.
	//
	// So the loop gates instead - see CanLease below. The worker starts,
	// reports the condition in its readiness for as long as it lasts, leases
	// nothing, and begins working the moment the watcher loads a product. No
	// restart, and nothing to do on the worker when a product is added.
	if products.Count() == 0 {
		logger.Warn("no product configuration yet; this worker will not lease until there is some",
			"dir", cfg.ProductsDir())
	} else {
		logger.Info("product configuration loaded",
			"products", products.Count(), "invalid", len(products.Invalid()),
			"dir", cfg.ProductsDir())
	}

	/* ---- who this worker is, to the Coordinator ----------------------------
	 *
	 * A worker leases jobs over the same authenticated API a person uses, so
	 * with authentication on it needs a credential of its own or it gets
	 * `UNAUTHENTICATED: no bearer token` every five seconds forever: a process
	 * that is up, probes green, and never does any work.
	 *
	 * It authenticates as a MACHINE ACCOUNT in the identity provider, using the
	 * client credentials grant, and receives a short-lived token the
	 * Coordinator verifies with exactly the same keys and the same code as a
	 * person's. Nothing is configured here by hand: the credentials are written
	 * by the deployment's seeder and mounted read-only, so `replicas: 20` needs
	 * no conversation with anybody. See pkg/authz/workload.go.
	 *
	 * ABSENT CREDENTIALS ARE NOT FATAL. A Coordinator with authentication
	 * switched off wants no token at all, and that is a supported deployment;
	 * refusing to start would make this the one component that cannot run in
	 * it. The condition is logged once, in the words that name the fix, and the
	 * worker's own deep health reports it for as long as it lasts. */
	clientOpts := []v1.ClientOption{
		v1.WithUserAgent("softwaregateway-worker/" + info.Version),
	}
	var tokens *authz.TokenSource
	if path := cfg.Worker.CredentialsFile; path != "" {
		// READ ON DEMAND, not here. Whether the file exists at this instant
		// says nothing about whether it will exist in ten seconds: the seeder
		// writes it, and a worker is perfectly capable of winning that race.
		// Compose declares the ordering as a dependency and podman-compose does
		// not implement the key, so on that runtime it is not declared at all -
		// and a worker that read this once would then spend its whole life
		// unauthenticated with the credentials sitting beside it. It is also
		// what makes rotation a seeder run rather than a fleet restart.
		tokens = authz.NewFileTokenSource(path, nil)
		clientOpts = append(clientOpts, v1.WithTokenSource(tokens))

		// Said once, at startup, in whichever of the two states this is. Not a
		// failure either way: an installation with authentication off has no
		// such file and never will.
		if creds, err := authz.LoadWorkloadCredentials(path); err == nil {
			logger.Info("authenticating to the Coordinator",
				"issuer", creds.Issuer, "client_id", creds.ClientID)
		} else {
			logger.Warn("no workload credentials yet: calling the Coordinator with no "+
				"token, which only works where authentication is off. They are picked "+
				"up as soon as they appear, without a restart",
				"path", path, "reason", err)
		}
	}

	coordinator := v1.NewClient(cfg.Worker.CoordinatorEndpoint, clientOpts...)

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
			// Asked on every pass rather than captured once, because the
			// watcher below swaps the registry in place: this is what turns a
			// deployed product into a working fleet with no restart.
			CanLease: func() (bool, string) {
				if products.Count() == 0 {
					return false, "no product configuration in " + cfg.ProductsDir() +
						"; a job's repositories cannot be resolved into a registry client"
				}
				return true, ""
			},
		},
		logger,
	)

	hreg := health.New()

	// ---- liveness: is this process wedged? Nothing external. ----
	//
	// A worker SERVES NOTHING, which is what makes its liveness probe
	// different from the Coordinator's. Answering the probe proves the probe
	// server is alive and says nothing whatever about the lease loop, so a
	// loop that has deadlocked or whose goroutine has died leaves a container
	// that reports itself perfectly healthy and does no work for as long as
	// anybody leaves it running.
	hreg.AddLiveness("process", func() error { return nil })
	hreg.AddLiveness("lease-loop", loop.Wedged)

	// ---- readiness: is this worker actually part of the fleet? ----
	//
	// READY MEANS REGISTERED. Not "the process started", not "the Coordinator
	// answers a probe" - the Coordinator has accepted this worker's lease call,
	// so it is reachable, this worker is authenticated, and it is in the fleet.
	//
	// The narrower reading cost a deployment. With authentication on and no
	// credentials to present, two workers were refused on every lease, five
	// seconds apart, indefinitely - and reported HEALTHY throughout, because
	// the process was up, the control plane was reachable and the products had
	// loaded. `podman ps` showed a green fleet over a deployment that had never
	// worked once. Green must mean the container is doing its job, because
	// that is what it will be read as.
	//
	// The checks are ordered as a diagnosis: can it reach the Coordinator at
	// all, has it work it could execute, and is it being accepted. They run
	// together, so the report names every failing one and the first is the
	// cause rather than the symptom.
	hreg.AddReadiness("control-plane", func(ctx context.Context) health.Result {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		// The Coordinator's PUBLIC liveness endpoint, not its version API.
		// Reachability and authorization are different questions, and probing
		// an authenticated endpoint answers them both at once: with
		// authentication on, this check reported a Coordinator that was up,
		// answering and two feet away as DOWN, because the probe was refused
		// rather than unanswered.
		endpoint := strings.TrimRight(cfg.Worker.CoordinatorEndpoint, "/") + "/healthz"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return health.Down(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return health.Down(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return health.Down(fmt.Errorf("%s answered %s", endpoint, resp.Status))
		}
		return health.OK("reachable at " + cfg.Worker.CoordinatorEndpoint)
	})
	hreg.AddReadiness("configuration", func(context.Context) health.Result {
		// The SAME judgement the Coordinator makes about the SAME directory,
		// and it has to be: a worker whose view of the products differs from
		// the Coordinator's leases jobs it then cannot execute.
		if bad := products.Invalid(); len(bad) > 0 {
			return health.Degraded(fmt.Sprintf("%d product(s) failed to load", len(bad)))
		}
		if products.Count() == 0 {
			return health.Degraded("no products loaded")
		}
		return health.OK(fmt.Sprintf("%d product(s) loaded", products.Count()))
	})

	// ---- diagnostics: can this worker prove who it is at all ----
	//
	// Beside registration rather than inside it, because they fail differently
	// and the difference is the whole diagnosis. A Coordinator that is refusing
	// this worker and an identity provider that will not issue it a token look
	// identical from the lease loop - "could not lease work" - and have nothing
	// in common to do about them.
	//
	// A diagnostic and not a gate: registration below is the gate, and it
	// already fails when this does. Two checks reporting the same outage would
	// only make the report say it twice.
	if tokens != nil {
		hreg.AddDeep("credentials", func(context.Context) health.Result {
			ok, detail := tokens.Health()
			if ok {
				return health.OK(detail)
			}
			return health.Degraded(detail)
		})
	}

	// ---- readiness: the Coordinator is accepting this worker ----
	//
	// The check the two above exist to lead up to, and the one that makes a
	// green container mean a working one. See Loop.Registered for why this
	// gates readiness rather than sitting in the diagnostics, and for why that
	// is safe for a component that serves no traffic and dangerous only if it
	// were confused with liveness.
	//
	// DOWN rather than DEGRADED, and the distinction is the whole point: a
	// degraded report answers the probe 200, so the container is still green in
	// `docker ps` and `podman ps`. A worker the Coordinator will not accept is
	// not a degraded worker. It is a worker doing nothing.
	hreg.AddReadiness("registration", func(context.Context) health.Result {
		// NOT LEASING ON PURPOSE is not a failure. A worker with no product
		// configuration deliberately asks for no work, so it never registers -
		// and reporting that as DOWN would paint every worker red on a
		// deployment that is simply ahead of its configuration, which is the
		// ordinary order of a rollout. Degraded says it, keeps the container
		// green, and clears itself the moment a product lands.
		if products.Count() == 0 {
			return health.Degraded("waiting for product configuration in " + cfg.ProductsDir())
		}
		ok, _, detail := loop.Registered()
		if ok {
			return health.OK(detail)
		}
		return health.Down(errors.New(detail))
	})

	mux := http.NewServeMux()
	// Both spellings of liveness, exactly as the Coordinator serves them, so
	// one probe configuration works against either component.
	mux.HandleFunc("/healthz", liveHandler(hreg))
	mux.HandleFunc("/livez", liveHandler(hreg))
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		writeReport(w, hreg.Ready(r.Context()))
	})
	// The deep report, at the path the Coordinator serves its own at. A worker
	// is not an API server and this is the only route it shares, which is the
	// point: `curl <host>/api/v1/system:healthCheck` answers for either.
	mux.HandleFunc("/api/v1/system:healthCheck", func(w http.ResponseWriter, r *http.Request) {
		// Always 200. This is a report, and its body carries the verdict; a
		// 503 would leave a caller that checks status codes unable to see
		// WHICH check is unhappy.
		writeJSON(w, http.StatusOK, hreg.Deep(r.Context()))
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

	/*
	  WATCH THE PRODUCTS, exactly as the Coordinator does.

	  The worker read this directory once, at startup, and never again. So a
	  product added or corrected while it was running did not exist as far as
	  it was concerned: the Coordinator reloaded, planned the work and handed it
	  out, and every job for that product came back

	      product "…" is not configured on this worker

	  until somebody restarted the process. The two planes were reading the same
	  files and disagreeing about what was in them, which is the one thing this
	  configuration model is supposed to make impossible.

	  It is worse than a delay. A job that fails this way is classified
	  `configuration`, which is TERMINAL - one attempt, by deliberate policy,
	  because a worker that cannot execute a job should surface that rather than
	  burn the attempts a real retry would need. So a single lease taken during
	  the gap does not merely wait for the restart; it kills the job, and the
	  transfer needs an explicit retry afterwards even once the worker can see
	  the product.

	  A watch failure is not fatal - the loaded configuration keeps working and
	  only hot reload is lost - so this returns nil on everything except the
	  context ending, and never takes the fleet down over an inotify limit.
	*/
	g.Go(func() error {
		watcher := product.NewWatcher(cfg.ProductsDir(), loader, products, product.WatchOptions{
			Logger: logger,
			OnReload: func(res product.LoadResult) {
				// Said at INFO because it is the answer to "why did this worker
				// start working" as well as to "why did it stop": a reload that
				// drops a product is as operationally interesting as one that
				// adds it, and the count is what makes either visible.
				logger.Info("product configuration reloaded",
					"products", products.Count(), "invalid", len(res.Invalid),
					"dir", cfg.ProductsDir())
			},
		})
		if err := watcher.Run(gctx); err != nil {
			logger.Warn("product configuration is not being watched; "+
				"a change will need a restart to take effect", "error", err)
		}
		return nil
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

// liveHandler answers a liveness probe from the registry's process-local
// checks. Shared by /healthz and /livez, which are the same question.
func liveHandler(hreg *health.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if err := hreg.Live(); err != nil {
			// A wedged lease loop is a restart, and this is what asks for one.
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"status": string(health.StatusDown),
				"error":  err.Error(),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": string(health.StatusHealthy)})
	}
}

// writeReport answers a readiness probe with the whole report.
//
// The body NAMES THE FAILING CHECK, which the previous bare {"status":"ok"}
// did not: a worker that would not go ready said only that it was not ready,
// and the reason was in a log somebody had to go and find. Degraded is still
// ready - the same rule the Coordinator applies - because a worker with one
// unloadable product can still transfer the others.
func writeReport(w http.ResponseWriter, rep health.Report) {
	status := http.StatusOK
	if rep.Status == health.StatusDown {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, rep)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// workerName is what this process calls itself in the fleet.
//
// The hostname alone is not a name. Under Kubernetes it is the POD name and
// reads perfectly - `swgw-worker-7d9c5f8b6d-x2k9p` says what it is and which
// replica. Under Docker and Podman it is the CONTAINER ID, so the same fleet
// list reads `16b81c38de5c`: unique, and nothing on the screen says which
// container that is or even that it is a worker.
//
// So the deployment supplies a name, and the host keeps replicas apart:
//
//	--worker-id            wins outright. An operator naming one worker.
//	name + "-" + host      `worker-16b81c38de5c` under compose.
//	host                   when the name already contains it, which is what
//	                       Kubernetes produces by setting the name from the
//	                       downward API - appending the pod name to itself
//	                       would be worse than the id ever was.
//	"worker-unknown"       when there is no hostname at all.
func workerName(explicit, name string) string {
	if explicit != "" {
		return explicit
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		if name != "" {
			return name
		}
		return "worker-unknown"
	}
	if name == "" || strings.Contains(name, host) {
		return firstNonEmpty(name, host)
	}
	return name + "-" + host
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
