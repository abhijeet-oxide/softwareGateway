// Package discovery answers one question, repeatedly: has this vendor
// published something we have not seen?
//
// See docs/design/07-discovery.md.
//
// Every scan is a FULL scan. There is no cursor, no "tags since" watermark and
// no cached tag set, because the OCI tag list has no ordering guarantee and no
// change feed — a cursor is a position in an arbitrary, registry-defined order
// that can change between calls. Any incremental scheme would need
// reconciliation against reality to avoid permanently missing a tag, and that
// reconciliation is a full scan.
//
// The property that earns it: a full scan is SELF-HEALING. Discovery that was
// down for a day, crashed mid-scan, or ran against a stale replica simply
// catches up on the next pass. There is no divergent state to detect and no
// repair path to write, because there is no state.
//
// A source covers ONE REGISTRY and one or more repositories on it. The
// repository set is re-resolved on every scan for the same reason the tag set
// is: a repository published since the last pass should be found without a
// restart or a configuration reload.
package discovery

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/oci"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/vendors"
)

// tagPageSize is the page size requested from tags/list.
const tagPageSize = 200

// ClientFactory builds a repository client for one repository on the source's
// registry.
//
// A factory rather than a prepared map because the repository set is not known
// until a scan resolves it — catalog enumeration can return repositories that
// did not exist when the loop started.
type ClientFactory func(repositoryPath string) (registry.Source, error)

// Scanner scans one source: one registry, one or more repositories on it.
type Scanner struct {
	packages *store.Packages
	log      *slog.Logger

	product    *product.Product
	productID  int64
	sourceName string
	sourceCfg  product.Source

	newClient ClientFactory
	catalog   CatalogLister

	repoFilter filter
	tagFilter  filter
	rules      ruleSet

	// layout is how this vendor lays packages out. Never nil — an unset
	// configuration resolves to the standard layout, so the scanner has no
	// "does this source have a plugin?" branch anywhere.
	layout vendors.Layout

	// targetIDs maps configured TARGET names to catalog row IDs, for
	// auto-download rule resolution. Read-only after construction.
	targetIDs map[string]int64

	// clients caches one client per repository path.
	//
	// Rebuilding per scan would discard the connection pool and the bearer
	// token cache every fifteen minutes, turning a warm keep-alive into a fresh
	// TLS handshake and a token exchange per repository — the cost the token
	// cache exists to avoid.
	mu      sync.Mutex
	clients map[string]registry.Source

	// writeMu serialises package-recording transactions. See recordPackage.
	writeMu sync.Mutex

	// progress is the live counter for the scan currently running, read by the
	// status endpoint while the scan is in flight.
	progress progressTracker
}

// Progress returns a snapshot of the scan currently running, or the zero value
// when none is.
func (s *Scanner) Progress() ScanProgress { return s.progress.snapshot() }

// ScannerConfig builds a Scanner.
type ScannerConfig struct {
	Packages  *store.Packages
	Logger    *slog.Logger
	Product   *product.Product
	ProductID int64
	// SourceName selects which of the product's sources this scans.
	SourceName string
	// NewClient builds a client for a repository on this source's registry.
	NewClient ClientFactory
	// Catalog enumerates the registry. May be nil when repositoryDiscovery is
	// off, which is the default.
	Catalog CatalogLister
	// RepoIDs maps configured repository NAMES to catalog row IDs, used to
	// resolve auto-download rule targets.
	RepoIDs map[string]int64
	// Layout groups scanned tags into packages. Nil means the standard layout.
	Layout vendors.Layout
}

// NewScanner compiles a source's filters and rules.
//
// Compilation happens here, once, rather than per scan: a bad pattern should be
// one loud complaint at startup, not a failure that recurs every fifteen
// minutes forever.
func NewScanner(cfg ScannerConfig) (*Scanner, error) {
	src, ok := cfg.Product.Source(cfg.SourceName)
	if !ok {
		return nil, fmt.Errorf("product %q has no source %q", cfg.Product.Metadata.Name, cfg.SourceName)
	}

	where := fmt.Sprintf("product %q source %q", cfg.Product.Metadata.Name, cfg.SourceName)

	tagFilter, err := compileFilters("discovery.tagFilters", src.Discovery.TagFilters)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}
	repoFilter, err := compileFilters("discovery.repositoryFilters", src.Discovery.RepositoryFilters)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}
	rules, err := compileRules(cfg.Product.Spec.AutoDownload)
	if err != nil {
		return nil, fmt.Errorf("product %q: %w", cfg.Product.Metadata.Name, err)
	}

	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	layout := cfg.Layout
	if layout == nil {
		layout = vendors.Standard{}
	}

	return &Scanner{
		packages:   cfg.Packages,
		log:        log.With("product", cfg.Product.Metadata.Name, "source", cfg.SourceName),
		product:    cfg.Product,
		productID:  cfg.ProductID,
		sourceName: cfg.SourceName,
		sourceCfg:  src,
		newClient:  cfg.NewClient,
		catalog:    cfg.Catalog,
		repoFilter: repoFilter,
		tagFilter:  tagFilter,
		rules:      rules,
		targetIDs:  cfg.RepoIDs,
		layout:     layout,
		clients:    map[string]registry.Source{},
	}, nil
}

// ScanResult reports what one scan did, across every repository.
type ScanResult struct {
	// Repositories is how many were scanned.
	Repositories int
	// RepositoriesFromCatalog is how many of those came from `/v2/_catalog`
	// rather than from configuration.
	RepositoriesFromCatalog int
	// RepositoriesFiltered is how many candidates repositoryFilters rejected.
	RepositoriesFiltered int

	TagsListed   int
	TagsAdmitted int
	New          int
	Superseded   int
	Requests     int

	// TagErrors are per-tag failures that did not stop the scan.
	TagErrors []TagError
	// RepositoryErrors are per-repository failures that did not stop the scan.
	// One unreachable repository must not hide the other nineteen.
	RepositoryErrors []RepositoryError

	Duration time.Duration

	// Collapsed reports that this result came from a scan ALREADY RUNNING when
	// the request arrived, rather than one it started.
	//
	// Reported rather than hidden. The numbers are real either way — the caller
	// waited for that scan to finish — but "a scan ran for you" and "you were
	// shown a scan that was already under way" are different facts, and an
	// operator watching a count they expect to change deserves to know which
	// one they are looking at.
	Collapsed bool
}

// TagError is a single tag's failure.
type TagError struct {
	Repository string
	Tag        string
	Err        error
}

func (e TagError) Error() string {
	if e.Repository == "" {
		return "tag " + e.Tag + ": " + e.Err.Error()
	}
	return e.Repository + " tag " + e.Tag + ": " + e.Err.Error()
}
func (e TagError) Unwrap() error { return e.Err }

// RepositoryError is a single repository's failure.
type RepositoryError struct {
	Repository string
	Err        error
}

func (e RepositoryError) Error() string { return "repository " + e.Repository + ": " + e.Err.Error() }
func (e RepositoryError) Unwrap() error { return e.Err }

// Scan performs one full scan of every repository this source covers.
//
// Returning an error means the scan could not proceed at all — the repository
// set could not be resolved. Per-repository and per-tag failures are collected
// and do not stop the scan (docs/design/07 §7).
func (s *Scanner) Scan(ctx context.Context) (ScanResult, error) {
	started := time.Now()
	var res ScanResult

	// Progress is published from here on. The enumeration below is the first
	// thing a caller waits on and, against a slow registry, often the longest —
	// so the tracker starts before it, not after.
	s.progress.begin()
	defer s.progress.end()

	// Re-resolved every scan, for the same reason the tag list is: a repository
	// published since the last pass should be found without a restart.
	set := resolveRepositories(ctx, s.sourceCfg, s.repoFilter, s.catalog)
	res.RepositoriesFromCatalog = set.FromCatalog
	res.RepositoriesFiltered = set.Filtered

	if set.CatalogErr != nil {
		// Not fatal. A vendor forbidding `_catalog` is normal — the credential
		// is usually good for pulling named repositories, not for enumerating
		// the registry — and the repositories we WERE told about must still be
		// scanned.
		s.log.WarnContext(ctx, "could not list the registry's repositories",
			"error", describeCatalogError(set.CatalogErr))
		res.RepositoryErrors = append(res.RepositoryErrors,
			RepositoryError{Repository: "_catalog", Err: set.CatalogErr})
	}
	if set.Truncated {
		s.log.WarnContext(ctx, "repository enumeration hit its cap; the set is partial",
			"max", s.sourceCfg.Discovery.EffectiveMaxRepositories(),
			"hint", "narrow discovery.repositoryFilters, or raise discovery.maxRepositories")
	}

	if len(set.Repositories) == 0 {
		res.Duration = time.Since(started)
		if set.CatalogErr != nil {
			return res, fmt.Errorf("no repositories to scan: %s", describeCatalogError(set.CatalogErr))
		}
		s.log.WarnContext(ctx, "no repositories to scan after filtering",
			"filtered", set.Filtered)
		return res, nil
	}

	s.progress.update(func(p *ScanProgress) {
		p.Phase = PhaseListingTags
		p.RepositoriesTotal = len(set.Repositories)
	})

	// ONE bound, applied to both phases, sized to the connection pool.
	//
	// This used to be two nested semaphores — repositories outside, tags inside
	// — with separately configured limits. The trouble is that every request
	// they gate goes through ONE connection pool, so the real ceiling was
	// min(pool, R×T), and the defaults hid it by agreeing: 4 × 8 = 32 =
	// maxConnections. Change any one of the three and the system silently
	// becomes either socket-starved or over-provisioned, with no error and no
	// way to tell from the outside which.
	//
	// So the semaphore is sized to the pool, and there is one of it. It cannot
	// be nested — the outer holders would deadlock waiting for slots the inner
	// work needs — which is why the scan is now two flat phases rather than a
	// tree. That fell out of the fix and is an improvement on its own: the total
	// tag count is known before any manifest is fetched, so progress reports a
	// real denominator instead of one that grows as it goes.
	limit := s.sourceCfg.Concurrency.PerRegistry
	if limit <= 0 {
		limit = product.DefaultPerRegistry
	}

	listed := s.listPhase(ctx, set.Repositories, limit)
	for _, l := range listed.failures {
		res.RepositoryErrors = append(res.RepositoryErrors,
			RepositoryError{Repository: l.path, Err: l.err})
		s.log.WarnContext(ctx, "repository scan failed", "repository", l.path, "error", l.err)
	}
	res.Repositories = listed.scanned
	res.TagsListed = listed.tagsListed
	res.TagsAdmitted = len(listed.work)

	if err := ctx.Err(); err != nil {
		return res, err
	}

	resolved := s.resolvePhase(ctx, listed.work, limit)
	res.New = resolved.New
	res.Superseded = resolved.Superseded
	res.Requests = resolved.Requests
	res.TagErrors = resolved.TagErrors

	// Retire discovery-managed rows for repositories that have left the
	// catalog. Only attempted when enumeration actually succeeded: a failed
	// catalog call must not be read as "everything disappeared".
	if s.sourceCfg.EnumeratesRepositories() && set.CatalogErr == nil {
		if n, err := s.packages.DeactivateDiscoveredRepositories(
			ctx, s.productID, s.sourceCfg.Registry, set.Repositories); err != nil {
			s.log.WarnContext(ctx, "could not retire vanished repositories", "error", err)
		} else if n > 0 {
			s.log.InfoContext(ctx, "retired repositories no longer in the catalog", "count", n)
		}
	}

	res.Duration = time.Since(started)

	// A scan where EVERY repository failed is a failed scan, not a successful
	// one that found nothing.
	//
	// This distinction is load-bearing. Per-repository errors are collected so
	// one unreachable repository cannot hide the other nineteen — but if none
	// succeeded, the registry is down, and reporting success would keep
	// `discovery_last_success_timestamp_seconds` advancing right through the
	// outage. That gauge is the thing to alert on precisely because it catches
	// "discovery quietly stopped finding anything" (docs/design/07 §7); letting
	// a total outage refresh it would defeat it. It would also leave the loop's
	// backoff disengaged, hammering a dead registry on the normal interval.
	if res.Repositories == 0 && len(res.RepositoryErrors) > 0 {
		return res, fmt.Errorf("every repository failed (%d of %d): %w",
			len(res.RepositoryErrors), len(set.Repositories), res.RepositoryErrors[0].Err)
	}

	return res, nil
}

// tagWork is one tag to resolve, with everything needed to resolve it.
//
// The repository client and row ID are carried rather than looked up again:
// phase one already paid for both, and re-deriving them per tag would put a
// mutex acquisition and a database round trip in front of every manifest.
type tagWork struct {
	repoPath string
	repoID   int64
	client   registry.Source
	tag      string
}

// listOutcome is what phase one produced.
type listOutcome struct {
	work       []tagWork
	tagsListed int
	scanned    int
	failures   []repoFailure
}

type repoFailure struct {
	path string
	err  error
}

// listPhase resolves each repository and lists its tags, bounded.
//
// It produces a FLAT work list. Nothing is fetched here beyond the tag lists,
// so the phase is cheap relative to the one that follows and finishes knowing
// the exact size of the work remaining — which is what lets progress report a
// denominator that does not move.
func (s *Scanner) listPhase(ctx context.Context, repos []string, limit int) listOutcome {
	type result struct {
		path     string
		repoID   int64
		client   registry.Source
		admitted []string
		listed   int
		err      error
	}

	results := make([]result, len(repos))
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup

	for i, repoPath := range repos {
		if ctx.Err() != nil {
			break
		}

		wg.Add(1)
		go func(i int, repoPath string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			s.progress.update(func(p *ScanProgress) {
				p.Phase = PhaseListingTags
				p.CurrentRepository = repoPath
				p.RepositoriesInFlight++
			})
			defer s.progress.update(func(p *ScanProgress) {
				p.RepositoriesInFlight--
				p.RepositoriesDone++
			})

			r := result{path: repoPath}
			defer func() { results[i] = r }()

			if r.client, r.err = s.clientFor(repoPath); r.err != nil {
				return
			}
			if r.repoID, r.err = s.ensureRepositoryRow(ctx, repoPath); r.err != nil {
				return
			}

			var tags []string
			if tags, r.err = s.listTags(ctx, r.client); r.err != nil {
				return
			}
			r.listed = len(tags)
			for _, tag := range tags {
				if s.tagFilter.admits(tag) {
					r.admitted = append(r.admitted, tag)
				}
			}

			admitted := len(r.admitted)
			listed := r.listed
			s.progress.update(func(p *ScanProgress) {
				p.TagsListed += listed
				p.TagsTotal += admitted
			})
		}(i, repoPath)
	}
	wg.Wait()

	// Assembled in the ORIGINAL order, after the fact, so the work list — and
	// therefore the logs — come out identical run to run even though the listing
	// did not.
	var out listOutcome
	for _, r := range results {
		if r.err != nil {
			out.failures = append(out.failures, repoFailure{path: r.path, err: r.err})
			s.progress.update(func(p *ScanProgress) { p.Errors++ })
			continue
		}
		out.scanned++
		out.tagsListed += r.listed
		for _, tag := range r.admitted {
			out.work = append(out.work, tagWork{
				repoPath: r.path, repoID: r.repoID, client: r.client, tag: tag,
			})
		}
	}
	return out
}

// resolveResult is phase two's contribution to a scan.
type resolveResult struct {
	New        int
	Superseded int
	Requests   int
	TagErrors  []TagError
}

// resolvePhase turns the flat work list into recorded packages.
//
// Four steps, and the split is what lets a vendor's several tags become the one
// release they represent:
//
//	head    one HEAD per tag; which are new?
//	fetch   one GET per NEW tag
//	group   the source's Layout turns those tags into packages
//	record  write each package, its relations, and anything that follows
//
// Grouping runs only over NEW tags, which is what keeps the steady state as
// cheap as it was before: a re-scan where nothing changed still costs one HEAD
// per tag and transfers no manifest bodies at all.
//
// Tags across ALL repositories share one pool, rather than one pool per
// repository. That matters on a real catalogue: a repository with three tags no
// longer leaves most of the budget idle while the repository with three hundred
// waits its turn behind it.
func (s *Scanner) resolvePhase(ctx context.Context, work []tagWork, limit int) resolveResult {
	var res resolveResult
	if len(work) == 0 {
		return res
	}

	s.progress.update(func(p *ScanProgress) { p.Phase = PhaseResolving })

	resolved := s.headPhase(ctx, work, limit)
	for _, r := range resolved {
		s.progress.update(func(p *ScanProgress) { p.TagsResolved++ })
		if r.err != nil {
			// Collected, not returned. One bad artifact must not stop discovery
			// of the rest — that is how a single vendor mistake would otherwise
			// stall every release behind it.
			res.TagErrors = append(res.TagErrors,
				TagError{Repository: r.work.repoPath, Tag: r.work.tag, Err: r.err})
			s.log.WarnContext(ctx, "tag resolve failed",
				"repository", r.work.repoPath, "tag", r.work.tag, "error", r.err)
			s.progress.update(func(p *ScanProgress) { p.Errors++ })
		}
	}

	items := s.fetchPhase(ctx, resolved, limit)
	for _, f := range items {
		if f.err != nil {
			res.TagErrors = append(res.TagErrors,
				TagError{Repository: f.work.repoPath, Tag: f.work.tag, Err: f.err})
			s.log.WarnContext(ctx, "manifest fetch failed",
				"repository", f.work.repoPath, "tag", f.work.tag, "error", f.err)
			s.progress.update(func(p *ScanProgress) { p.Errors++ })
		}
	}

	// Grouped PER REPOSITORY. A vendor's relationships are between tags of one
	// repository, and grouping across repositories would let `orb_1.0` in one
	// be claimed by a `signed_orb_1.0` in another.
	byRepo := map[string][]fetched{}
	order := []string{}
	for _, f := range items {
		if f.err != nil {
			continue
		}
		if _, seen := byRepo[f.work.repoPath]; !seen {
			order = append(order, f.work.repoPath)
		}
		byRepo[f.work.repoPath] = append(byRepo[f.work.repoPath], f)
	}

	for _, repoPath := range order {
		group := byRepo[repoPath]
		pkgs := s.groupPhase(ctx, group[0].work.client, scannedTagsFor(group))

		// The Layout names packages by tag; this maps back to the fetched oci.Tree
		// so recording still writes the artifacts we actually verified.
		trees := make(map[string]fetched, len(group))
		for _, f := range group {
			trees[f.work.tag] = f
		}

		for _, pkg := range pkgs {
			f, ok := trees[pkg.Tag]
			if !ok {
				// The Layout named a tag we did not fetch. Possible when it
				// groups onto a payload discovered by an earlier scan — the
				// relations still belong on that existing package.
				if err := s.attachRelations(ctx, group[0].work, pkg); err != nil {
					s.log.WarnContext(ctx, "could not attach related artifacts",
						"repository", repoPath, "tag", pkg.Tag, "error", err)
				}
				continue
			}

			outcome, err := s.recordPackage(ctx, f.work.client, f.work.repoID,
				f.work.repoPath, pkg.Tag, f.tree.Artifacts[0].Descriptor, f.tree, pkg)
			if err != nil {
				res.TagErrors = append(res.TagErrors,
					TagError{Repository: repoPath, Tag: pkg.Tag, Err: err})
				s.log.WarnContext(ctx, "package record failed",
					"repository", repoPath, "tag", pkg.Tag, "error", err)
				s.progress.update(func(p *ScanProgress) { p.Errors++ })
				continue
			}
			if outcome.isNew {
				res.New++
				res.Superseded += outcome.superseded
				res.Requests += outcome.requests
				s.progress.update(func(p *ScanProgress) { p.New++ })
			}
		}
	}

	return res
}

// clientFor returns the cached client for a repository, building it on first
// use.
func (s *Scanner) clientFor(repoPath string) (registry.Source, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if c, ok := s.clients[repoPath]; ok {
		return c, nil
	}
	if s.newClient == nil {
		return nil, fmt.Errorf("no client factory configured")
	}
	c, err := s.newClient(repoPath)
	if err != nil {
		return nil, err
	}
	s.clients[repoPath] = c
	return c, nil
}

// ensureRepositoryRow resolves the repositories row for a path, creating it if
// discovery found it rather than configuration declaring it.
func (s *Scanner) ensureRepositoryRow(ctx context.Context, repoPath string) (int64, error) {
	tx, err := s.packages.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin repository transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The row NAME is the source name for a single-repository source, and
	// "<source>/<path>" when the source covers several. That keeps the common
	// case reading exactly as it did before this feature existed, while the
	// multi-repository case stays unambiguous under the (product, role, name)
	// unique constraint.
	name := s.sourceName
	if declared := s.sourceCfg.DeclaredRepositories(); len(declared) != 1 || declared[0] != repoPath {
		name = s.sourceName + "/" + repoPath
	}

	managedBy := "discovery"
	for _, declared := range s.sourceCfg.DeclaredRepositories() {
		if declared == repoPath {
			managedBy = "config"
			break
		}
	}

	// The shortened spelling for listings, from the source's vendor plugin —
	// empty for every conformant registry, which is the point. It travels with
	// the repository row rather than being inferred at render time, so a listing
	// says the same thing about a repository whatever else is on the page, and
	// so the short form can be looked up as input.
	id, err := s.packages.EnsureRepository(ctx, tx,
		s.productID, string(product.RoleSource), name,
		s.sourceCfg.Registry, repoPath, string(s.sourceCfg.Type), managedBy,
		s.layout.DisplayRepository(repoPath))
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit repository row: %w", err)
	}
	return id, nil
}

// listTags pages through the whole tag list of one repository.
func (s *Scanner) listTags(ctx context.Context, client registry.Source) ([]string, error) {
	var all []string
	last := ""

	// Bounded so a registry with a broken pagination cursor cannot loop
	// forever. At 200 tags a page this admits 100k tags, far past the point
	// where tagFilters should be in use.
	const maxPages = 500

	for page := 0; page < maxPages; page++ {
		tags, next, err := client.ListTags(ctx, last, tagPageSize)
		if err != nil {
			// A repository with no tags yet is a normal state, and so is one
			// that vanished between the catalog listing and this scan. Neither
			// should back off the source. The CLIENT reports the 404 faithfully
			// so `products check` can call a typo'd path what it is; deciding
			// that discovery tolerates it belongs here.
			if errors.Is(err, registry.ErrNotFound) {
				return all, nil
			}
			return nil, fmt.Errorf("list tags for %s: %w", client.Name(), err)
		}
		all = append(all, tags...)
		if next == "" || next == last {
			return all, nil
		}
		last = next
	}
	return all, fmt.Errorf("list tags for %s: exceeded %d pages", client.Name(), maxPages)
}

// tagOutcome reports what one tag produced.
type tagOutcome struct {
	isNew      bool
	packageID  int64
	superseded int
	requests   int
}

// recordPackage writes a new package and everything that follows from it, in
// one transaction.
//
// The package, its artifact oci.Tree, the audit event, the notification and any
// auto-download request are ONE atomic fact. A package that exists without the
// notification announcing it, or a transfer request pointing at a package that
// was rolled back, are precisely the states the outbox pattern exists to make
// impossible (docs/design/07 §6).
func (s *Scanner) recordPackage(
	ctx context.Context, client registry.Source, repoID int64,
	repoPath, tag string, desc registry.Descriptor, t oci.Tree, pkg vendors.Package,
) (tagOutcome, error) {
	// One writer at a time within a source.
	//
	// Everything expensive — the HEAD, the existence check, the manifest oci.Tree
	// fetch — already happened outside this call and runs fully in parallel.
	// What is left is a short local transaction, and serialising it costs
	// nothing measurable while removing a whole class of problem: SQLite
	// serialises writers anyway and returns SQLITE_BUSY rather than queueing,
	// and on Postgres concurrent inserts into the same repository's rows would
	// contend on the unique index for no gain.
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.packages.DB().BeginTx(ctx, nil)
	if err != nil {
		return tagOutcome{}, fmt.Errorf("begin package transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	packageID, err := s.packages.InsertPackage(ctx, tx, store.PackageRow{
		ProductID:      s.productID,
		SourceRepoID:   repoID,
		Tag:            tag,
		ManifestDigest: desc.Digest.String(),
		MediaType:      desc.MediaType,
		TotalBytes:     t.TotalBytes,
		ArtifactCount:  len(t.Artifacts),
		BlobCount:      t.BlobCount,
		PublishedAt:    t.PublishedAt,

		// What the Layout concluded. `unknown` where the layout does not look
		// for signatures at all, which is honest — claiming `unsigned` there
		// would be a confident answer nobody checked.
		SignatureStatus: string(pkg.Status(s.layout.LooksForSignatures())),
		// Empty unless the vendor bundles the payload with its signature, in
		// which case this is the wrapper a transfer must plan from.
		TransferRootDigest: rootDigestOf(pkg),
		TransferRootTag:    pkg.RootTag,
		DisplayTag:         pkg.DisplayTag,
	})
	if errors.Is(err, store.ErrAlreadyExists) {
		// A concurrent scan won the race. Nothing to do, and nothing wrong:
		// this is the constraint doing its job.
		return tagOutcome{}, nil
	}
	if err != nil {
		return tagOutcome{}, err
	}

	if err := s.writeTree(ctx, tx, packageID, t); err != nil {
		return tagOutcome{}, err
	}

	if err := s.packages.ReplaceRelations(ctx, tx, packageID, relationRows(pkg)); err != nil {
		return tagOutcome{}, err
	}

	// Supersede any earlier package carrying THE SAME TAG in THIS repository.
	// Different tags are independent versions and are never touched, and
	// neither is the same tag in a different repository (docs/design/07 §4).
	superseded, err := s.packages.SupersedePrior(ctx, tx, repoID, tag, packageID)
	if err != nil {
		return tagOutcome{}, err
	}

	if err := s.writeAudit(ctx, tx, client, packageID, repoPath, tag, desc, superseded); err != nil {
		return tagOutcome{}, err
	}
	if err := s.notify(ctx, tx, client, packageID, repoPath, tag, desc, superseded); err != nil {
		return tagOutcome{}, err
	}

	requests, err := s.applyRules(ctx, tx, packageID, repoID, tag)
	if err != nil {
		return tagOutcome{}, err
	}

	if err := tx.Commit(); err != nil {
		return tagOutcome{}, fmt.Errorf("commit package %s: %w", tag, err)
	}

	s.log.InfoContext(ctx, "discovered package",
		"repository", repoPath,
		"tag", tag,
		"digest", desc.Digest.Short(),
		"artifacts", len(t.Artifacts),
		"blobs", t.BlobCount,
		"bytes", t.TotalBytes,
		"superseded", superseded,
		"requests", requests,
	)

	return tagOutcome{
		isNew:      true,
		packageID:  packageID,
		superseded: int(superseded),
		requests:   requests,
	}, nil
}

// writeTree persists the artifact oci.Tree and its blob references.
func (s *Scanner) writeTree(ctx context.Context, tx *sql.Tx, packageID int64, t oci.Tree) error {
	// Parents precede children in the slice — fetchTree walks breadth-first —
	// so a child's parent row ID is always already assigned.
	ids := make([]int64, len(t.Artifacts))

	for i, a := range t.Artifacts {
		var parentID *int64
		if a.Parent >= 0 {
			parentID = &ids[a.Parent]
		}

		id, err := s.packages.InsertArtifact(ctx, tx, store.ArtifactRow{
			PackageID:    packageID,
			ParentID:     parentID,
			Digest:       a.Descriptor.Digest.String(),
			MediaType:    a.Descriptor.MediaType,
			ArtifactType: a.Descriptor.ArtifactType,
			SizeBytes:    a.Descriptor.Size,
			Platform:     a.Descriptor.Platform.String(),
			Depth:        a.Depth,
			Raw:          a.Raw,
			Annotations:  a.Descriptor.Annotations,
		})
		if err != nil {
			return err
		}
		ids[i] = id

		if len(a.Blobs) == 0 {
			continue
		}
		refs := make([]store.BlobRef, 0, len(a.Blobs))
		for _, b := range a.Blobs {
			refs = append(refs, store.BlobRef{
				Digest:    b.Descriptor.Digest.String(),
				MediaType: b.Descriptor.MediaType,
				SizeBytes: b.Descriptor.Size,
				Kind:      b.Kind,
				Ordinal:   b.Ordinal,
			})
		}
		if err := s.packages.LinkBlobs(ctx, tx, id, refs); err != nil {
			return err
		}
	}
	return nil
}

// writeAudit records the discovery, and the supersession if there was one.
func (s *Scanner) writeAudit(
	ctx context.Context, tx *sql.Tx, client registry.Source,
	packageID int64, repoPath, tag string, desc registry.Descriptor, superseded int64,
) error {
	detail, _ := json.Marshal(map[string]any{
		"tag":        tag,
		"digest":     desc.Digest.String(),
		"source":     s.sourceName,
		"repository": client.Name(),
	})

	if err := s.packages.InsertAudit(ctx, tx, store.AuditRow{
		EventType:   "PackageDiscovered",
		ActorKind:   "system",
		ProductName: s.product.Metadata.Name,
		SubjectKind: "package",
		SubjectID:   fmt.Sprint(packageID),
		Detail:      string(detail),
	}); err != nil {
		return err
	}

	if superseded == 0 {
		return nil
	}

	// A vendor silently changing a released tag is something an operator should
	// be able to find later, so it gets its own event rather than being a field
	// on the discovery one.
	supersededDetail, _ := json.Marshal(map[string]any{
		"tag":            tag,
		"newDigest":      desc.Digest.String(),
		"packagesMarked": superseded,
		"source":         s.sourceName,
		"repository":     repoPath,
	})
	return s.packages.InsertAudit(ctx, tx, store.AuditRow{
		EventType:   "PackageSuperseded",
		ActorKind:   "system",
		ProductName: s.product.Metadata.Name,
		SubjectKind: "package",
		SubjectID:   fmt.Sprint(packageID),
		Detail:      string(supersededDetail),
	})
}

// notify enqueues PackageDiscovered to every subscribed channel.
func (s *Scanner) notify(
	ctx context.Context, tx *sql.Tx, client registry.Source,
	packageID int64, repoPath, tag string, desc registry.Descriptor, superseded int64,
) error {
	n := s.product.Spec.Notifications
	if !n.Enabled {
		return nil
	}

	payload, err := json.Marshal(map[string]any{
		"product":    s.product.Metadata.Name,
		"source":     s.sourceName,
		"repository": client.Name(),
		"tag":        tag,
		"digest":     desc.Digest.String(),
		"packageId":  packageID,
		"superseded": superseded > 0,
	})
	if err != nil {
		return fmt.Errorf("marshal notification payload: %w", err)
	}

	for _, channelName := range subscribedChannels(n, "PackageDiscovered") {
		channel, ok := channelByName(n, channelName)
		if !ok {
			// Validation rejects a subscription naming an unknown channel, so
			// reaching here would mean configuration and catalog disagree.
			// Skipping is right: a missing channel must not block the package.
			s.log.WarnContext(ctx, "subscription names unknown channel", "channel", channelName)
			continue
		}

		if err := s.packages.EnqueueNotification(ctx, tx, store.NotificationRow{
			ProductID:   s.productID,
			EventType:   "PackageDiscovered",
			ChannelName: channel.Name,
			ChannelType: string(channel.Type),
			SubjectKind: "package",
			SubjectID:   fmt.Sprint(packageID),
			Payload:     string(payload),
			// Keyed by package and channel, so the same package never notifies
			// the same channel twice however many times discovery re-runs.
			DedupeKey: fmt.Sprintf("PackageDiscovered|%d|%s", packageID, channel.Name),
		}); err != nil {
			return err
		}
	}
	return nil
}

// applyRules evaluates auto-download rules against a new package.
func (s *Scanner) applyRules(
	ctx context.Context, tx *sql.Tx, packageID, sourceRepoID int64, tag string,
) (int, error) {
	rule, ok := s.rules.match(tag)
	if !ok {
		return 0, nil
	}

	targetIDs, targetNames, err := resolveTargets(s.product, rule, s.targetIDs)
	if err != nil {
		// A misconfigured rule must not fail the discovery — the package is
		// real and worth recording either way. Logged loudly instead.
		s.log.ErrorContext(ctx, "auto-download rule could not be applied",
			"rule", rule.Name, "tag", tag, "error", err)
		return 0, nil
	}

	priority := rule.EffectivePriority()
	key := idempotencyKey("replicate", packageID, targetIDs, "", priority)

	id, created, err := s.packages.CreateTransferRequest(ctx, tx, store.TransferRequestRow{
		ID:             requestID(key),
		ProductID:      s.productID,
		PackageID:      packageID,
		Operation:      "replicate",
		SourceRepoID:   sourceRepoID,
		Priority:       priority,
		IdempotencyKey: key,
		RequestedBy:    "auto_download:" + rule.Name,
		RequestOrigin:  "auto_download",
		AutoRuleName:   rule.Name,
	})
	if err != nil {
		return 0, err
	}
	if !created {
		return 0, nil
	}

	s.log.InfoContext(ctx, "auto-download rule matched",
		"rule", rule.Name, "tag", tag, "targets", targetNames, "request", id, "priority", priority)

	detail, _ := json.Marshal(map[string]any{
		"rule": rule.Name, "tag": tag, "targets": targetNames, "priority": priority,
	})
	if err := s.packages.InsertAudit(ctx, tx, store.AuditRow{
		EventType:   "TransferRequested",
		Actor:       rule.Name,
		ActorKind:   "auto_rule",
		ProductName: s.product.Metadata.Name,
		SubjectKind: "transfer_request",
		SubjectID:   id,
		Detail:      string(detail),
	}); err != nil {
		return 0, err
	}

	return 1, nil
}

// subscribedChannels returns the channels subscribed to an event.
func subscribedChannels(n product.Notifications, event string) []string {
	var out []string
	seen := map[string]bool{}
	for _, sub := range n.Subscriptions {
		for _, e := range sub.Events {
			if e != event {
				continue
			}
			for _, c := range sub.Channels {
				if !seen[c] {
					seen[c] = true
					out = append(out, c)
				}
			}
		}
	}
	return out
}

func channelByName(n product.Notifications, name string) (product.Channel, bool) {
	for _, c := range n.Channels {
		if c.Name == name {
			return c, true
		}
	}
	return product.Channel{}, false
}
