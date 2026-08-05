package discovery

import (
	"context"
	"log/slog"
	"sync"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/metrics"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// Controller decides WHEN discovery should be running.
//
// Two inputs move independently and either can change at any moment:
// leadership, which flaps when a Coordinator restarts or a lock is lost, and
// configuration, which reloads whenever Flux applies a commit. Discovery must
// be running exactly when we are the leader AND we have a reconciled
// configuration.
//
// Kept out of cmd/coordinator deliberately: this is a state machine with real
// failure modes, and logic in main is logic that cannot be tested
// (docs/design/15 §3).
type Controller struct {
	loop     *Loop
	packages *store.Packages
	secrets  *product.SecretResolver
	log      *slog.Logger

	// reconcileMu serialises whole reconciles.
	//
	// Separate from mu, which guards the fields below and is released while the
	// loop starts and stops. Without it two reconciles can interleave: SetLeader
	// runs on the leader-election goroutine and SetConfig on the config watcher,
	// so a reload landing as leadership is acquired had both observe
	// started=false and both call Start — the second failing with "discovery
	// loop is already running". The worse ordering is silent: one reconcile
	// stops the loop while the other is mid-Start, leaving started=true with
	// nothing polling.
	reconcileMu sync.Mutex

	mu       sync.Mutex
	ctx      context.Context
	leader   bool
	products []*product.Product
	catalog  map[string]ProductRef
	started  bool
}

// NewController builds the supervisor.
func NewController(
	packages *store.Packages,
	secrets *product.SecretResolver,
	log *slog.Logger,
	m *metrics.Registry,
) *Controller {
	if log == nil {
		log = slog.Default()
	}
	return &Controller{
		loop:     NewLoop(log, m),
		packages: packages,
		secrets:  secrets,
		log:      log,
	}
}

// Loop exposes the underlying loop, which satisfies the API's Discoverer.
func (c *Controller) Loop() *Loop { return c.loop }

// Run holds the context discovery goroutines inherit, and stops the loop on
// shutdown.
func (c *Controller) Run(ctx context.Context) error {
	c.mu.Lock()
	c.ctx = ctx
	c.mu.Unlock()

	c.reconcile()

	<-ctx.Done()
	c.loop.Stop()
	return nil
}

// SetLeader records a leadership change.
//
// Losing leadership stops discovery promptly. Two Coordinators scanning the
// same sources would double vendor load and race every insert; the unique
// constraints would hold, but the wasted requests would not be free.
func (c *Controller) SetLeader(isLeader bool) {
	c.mu.Lock()
	changed := c.leader != isLeader
	c.leader = isLeader
	c.mu.Unlock()

	if changed {
		c.log.Info("discovery: leadership changed", "leader", isLeader)
		c.reconcile()
	}
}

// SetConfig records a newly loaded and reconciled configuration.
//
// Always restarts the loop when leading, rather than diffing the source set: a
// product's interval, tag filters, credentials or rules may all have changed,
// and a restart is a few milliseconds against a fifteen-minute interval.
// Diffing here would be an optimisation whose bugs are silent — a source
// quietly polling with stale filters.
func (c *Controller) SetConfig(products []*product.Product, catalog map[string]ProductRef) {
	c.mu.Lock()
	c.products = products
	c.catalog = catalog
	c.mu.Unlock()

	c.reconcile()
}

// reconcile brings the loop in line with the desired state.
func (c *Controller) reconcile() {
	c.reconcileMu.Lock()
	defer c.reconcileMu.Unlock()

	c.mu.Lock()
	ctx, leader, products, cat, started := c.ctx, c.leader, c.products, c.catalog, c.started
	c.mu.Unlock()

	if ctx == nil {
		// Run has not been called yet. Whatever was recorded will be applied
		// when it is.
		return
	}

	want := leader && len(products) > 0 && cat != nil

	if started {
		c.loop.Stop()
		c.mu.Lock()
		c.started = false
		c.mu.Unlock()
	}
	if !want {
		return
	}

	specs, errs := SourceSpecs(products, cat, c.secrets, c.log)
	for _, err := range errs {
		// A source that cannot be built is omitted and reported. One product's
		// missing secret must not stop discovery for the rest of the fleet.
		c.log.Error("discovery: source unavailable", "error", err)
	}
	if len(specs) == 0 {
		c.log.Info("discovery: no enabled sources to poll")
		return
	}

	for _, s := range specs {
		if !s.InsecureTLS {
			continue
		}
		// Repeated on every reconcile, not just the first. A warning logged once
		// at startup scrolls away; one that reappears whenever configuration
		// changes is one somebody eventually removes the cause of.
		c.log.Warn("discovery: TLS CERTIFICATE VERIFICATION IS DISABLED for this source — "+
			"the connection is encrypted but not authenticated",
			"product", s.Product.Metadata.Name,
			"source", s.SourceName,
			"setting", "network.tls.insecureSkipVerify")
	}

	if err := c.loop.Start(ctx, specs, c.packages); err != nil {
		c.log.Error("discovery: failed to start", "error", err)
		return
	}

	c.mu.Lock()
	c.started = true
	c.mu.Unlock()
}
