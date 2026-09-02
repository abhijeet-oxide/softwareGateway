package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/api"
	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
	"github.com/abhijeet-oxide/softwareGateway/internal/compliance/baseline"
	celc "github.com/abhijeet-oxide/softwareGateway/internal/compliance/cel"
	"github.com/abhijeet-oxide/softwareGateway/internal/compliance/render"
	"github.com/abhijeet-oxide/softwareGateway/internal/compliance/source"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/config"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/vendors"
)

// Wiring compliance into the Coordinator.
//
// The composition root again, and for the same reason blobsImpl and compareImpl
// live here: a run reaches a vendor registry with credentials and shells out to
// a binary on this host, and nothing under internal/api may do either.

// policyCatalogue holds the loaded checks and swaps them atomically on reload.
//
// # Why the swap is atomic and the pointer is read per request
//
// A run in flight keeps the catalogue it started with, so its bundle digest
// still describes what produced it. A run started after a reload gets the new
// one. Neither ever sees a half-loaded catalogue, which is what a mutex-free
// field assignment would eventually produce on a busy Coordinator.
type policyCatalogue struct {
	current atomic.Pointer[compliance.Catalog]
}

func (p *policyCatalogue) get() *compliance.Catalog  { return p.current.Load() }
func (p *policyCatalogue) set(c *compliance.Catalog) { p.current.Store(c) }

// loadPolicies compiles the built-in baseline plus every operator pack.
//
// # Why the baseline goes through the same loader as an operator's pack
//
// It is written to a temporary directory and loaded from disk exactly as a
// mounted pack is. A shipped check that would fail an operator's loader is a
// shipped check that lies about the contract, and the only way to be sure it
// would not is to put it through the same code.
func loadPolicies(cfg config.ComplianceConfig, log *slog.Logger) (*compliance.Catalog, error) {
	comp, err := celc.NewCompiler()
	if err != nil {
		return nil, fmt.Errorf("building the policy compiler: %w", err)
	}

	files, err := baseline.Files()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "sgw-baseline-")
	if err != nil {
		return nil, fmt.Errorf("staging the built-in policy pack: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			return nil, fmt.Errorf("staging %s: %w", name, err)
		}
	}

	paths := append([]string{dir}, cfg.PolicyPaths...)
	cat, err := (&compliance.Loader{Compiler: comp}).Load(paths...)
	if err != nil {
		return nil, err
	}

	// A broken pack is reported and kept, never silently dropped: the checks it
	// owns will report `error`, and an operator has to be told which pack and
	// why. Logged at WARN here and served in the API, because a log line alone
	// is not somewhere anybody looks.
	for _, p := range cat.Packs() {
		if p.OK() {
			continue
		}
		for _, e := range p.Errors {
			log.Warn("policy pack did not load",
				slog.String("pack", p.Name), slog.String("path", p.Path), slog.String("error", e))
		}
	}
	log.Info("compliance policies loaded",
		slog.Int("checks", cat.Len()), slog.Int("packs", len(cat.Packs())),
		slog.String("bundle", compliance.ShortDigest(cat.BundleDigest)))
	return cat, nil
}

// complianceHelm builds the renderer from configuration.
func complianceHelm(cfg config.ComplianceConfig) render.Helm {
	return render.Helm{
		Binary:      cfg.HelmBinary,
		KubeVersion: cfg.KubeVersion,
		APIVersions: cfg.APIVersions,
		Timeout:     cfg.RenderTimeout,
	}.WithDefaults()
}

// complianceSweeper releases claims held by Coordinators that stopped.
//
// Without it a release whose Coordinator was killed mid-run is stuck "running"
// forever and can never be checked again - the state nobody can leave without a
// database console.
type complianceSweeper struct {
	packages   *store.Packages
	staleAfter time.Duration
	log        *slog.Logger
	leader     atomic.Bool
}

// SetLeader follows the same contract the other sweepers do: only the leader
// sweeps, so two Coordinators do not both release the same claim and race to
// write the reason.
func (c *complianceSweeper) SetLeader(isLeader bool) { c.leader.Store(isLeader) }

func (c *complianceSweeper) sweep(ctx context.Context) {
	if !c.leader.Load() {
		return
	}
	n, err := c.packages.ReleaseStaleComplianceRuns(ctx, c.staleAfter)
	if err != nil {
		c.log.Warn("could not release stale compliance runs", slog.String("error", err.Error()))
		return
	}
	if n > 0 {
		c.log.Info("released stale compliance runs", slog.Int("runs", n))
	}
}

// Run sweeps on a ticker until the context ends.
func (c *complianceSweeper) Run(ctx context.Context) {
	// A third of the stale window, so a claim is released within one sweep of
	// becoming stale rather than up to a full window later.
	interval := c.staleAfter / 3
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.sweep(ctx)
		}
	}
}

// buildCompliance assembles everything the API needs, or reports why it cannot.
func buildCompliance(
	cfg config.ComplianceConfig, packages *store.Packages,
	blobs source.BlobReader, classify func(string) vendors.Classifier, log *slog.Logger,
) (*policyCatalogue, *compliance.Runner, *complianceSweeper, error) {
	cat, err := loadPolicies(cfg, log)
	if err != nil {
		return nil, nil, nil, err
	}
	catalogue := &policyCatalogue{}
	catalogue.set(cat)

	helm := complianceHelm(cfg)
	if version, herr := helm.Version(context.Background()); herr != nil {
		// Not fatal. Chart acquisition and the API still work; every rendered
		// check reports `error` and the run is inconclusive. A Coordinator that
		// refused to start over this would take the whole platform down for a
		// feature that degrades honestly.
		log.Warn("helm is not available, so charts cannot be rendered; "+
			"compliance runs will report every rendered check as undecided",
			slog.String("error", herr.Error()))
	} else {
		log.Info("compliance renderer ready",
			slog.String("helm", version), slog.String("kubeVersion", helm.KubeVersion))
	}

	preparer := &source.Preparer{
		Fetcher: source.Fetcher{
			Blobs: blobs,
			Budgets: source.Budgets{
				PerChart:   cfg.MaxChartBytes,
				PerRelease: cfg.MaxReleaseBytes,
			},
		},
		Helm:     helm,
		Probe:    cfg.ProbeDeterminacy(),
		Packages: packages,
		// The SAME classifier the artifact listing uses. A compliance run with
		// its own opinion about what a chart is would disagree with the page
		// somebody was looking at when they pressed the button.
		Classify: classify,
		Config: func() map[string]any {
			registries := make([]any, 0, len(cfg.ApprovedRegistries))
			for _, r := range cfg.ApprovedRegistries {
				registries = append(registries, r)
			}
			return map[string]any{"approvedRegistries": registries}
		},
	}

	runner := &compliance.Runner{
		Catalog:    catalogue.get,
		Source:     preparer,
		Recorder:   packages,
		Log:        log,
		MaxResults: cfg.MaxResults,
		StaleAfter: cfg.StaleAfter,
	}
	sweeper := &complianceSweeper{
		packages:   packages,
		staleAfter: orDuration(cfg.StaleAfter, compliance.DefaultStaleAfter),
		log:        log,
	}
	return catalogue, runner, sweeper, nil
}

func orDuration(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}

// The three adapters below exist so a disabled or unbuilt feature reaches the
// API as a nil INTERFACE rather than a non-nil interface holding a nil pointer.
//
// That distinction is not pedantry: the router checks `s.deps.X != nil` to
// decide whether to register a route, and a typed nil passes that check. The
// route would then exist and panic on the first request - a 500 where an honest
// 404 belongs.

func complianceAPIRunner(r *compliance.Runner) api.ComplianceRunner {
	if r == nil {
		return nil
	}
	return r
}

func complianceAPICatalogue(p *policyCatalogue) api.ComplianceCatalogue {
	if p == nil {
		return nil
	}
	return p.get
}

func complianceAPIHelm(cfg config.ComplianceConfig) api.ComplianceHelm {
	if !cfg.Enabled {
		return nil
	}
	helm := complianceHelm(cfg)
	return func() (string, error) {
		// Probed when somebody looks rather than cached at start-up: an
		// operator who installs helm and restarts nothing should see the tab
		// start working, and the probe is one subprocess printing a version.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return helm.Version(ctx)
	}
}

// complianceClassifier builds the per-product artifact classifier.
//
// # Why this is here and not in the compliance package
//
// It reads the product's configured vendor layouts, and only the composition
// root may name a vendor. It is the same construction internal/api makes for
// the artifact listing, deliberately: one product, one answer to "is this a
// chart", or a run disagrees with the page that sent somebody to it.
func complianceClassifier(products *product.Registry, layouts *vendors.Registry) func(string) vendors.Classifier {
	return func(productName string) vendors.Classifier {
		if products == nil || layouts == nil {
			return vendors.OCIOnly
		}
		p, ok := products.Get(productName)
		if !ok {
			return vendors.OCIOnly
		}
		names := make([]string, 0, len(p.Spec.Sources))
		for _, src := range p.Spec.Sources {
			names = append(names, src.VendorLayout())
		}
		return vendors.ClassifierFor(layouts, names)
	}
}
