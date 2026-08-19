// Package discovery answers one question, repeatedly: has this vendor
// published something we have not seen?
//
// See docs/design/07-discovery.md.
//
// Every scan is a FULL scan. There is no cursor, no "tags since" watermark and
// no cached tag set, because the OCI tag list has no ordering guarantee and no
// change feed - a cursor is a position in an arbitrary, registry-defined order
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
	"net/http"
	"sync"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/download"
	"github.com/abhijeet-oxide/softwareGateway/internal/oci"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/transfer"
	"github.com/abhijeet-oxide/softwareGateway/internal/vendors"
)

// tagPageSize is the page size requested from tags/list.
const tagPageSize = 200

// ClientFactory builds a repository client for one repository on the source's
// registry.
//
// A factory rather than a prepared map because the repository set is not known
// until a scan resolves it - catalog enumeration can return repositories that
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

	// layout is how this vendor lays packages out. Never nil - an unset
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
	// TLS handshake and a token exchange per repository - the cost the token
	// cache exists to avoid.
	mu      sync.Mutex
	clients map[string]registry.Source

	// writeMu serialises package-recording transactions. See recordPackage.
	writeMu sync.Mutex

	// progress is the live counter for the scan currently running, read by the
	// status endpoint while the scan is in flight.
	progress progressTracker

	// analyseWake asks the background analyser for a pass. One slot: it looks
	// for everything outstanding when it wakes, so a second request while one
	// is pending is the same request. See analyse.go.
	analyseWake chan struct{}
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
	rules, err := compileRules(cfg.Product)
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
		packages:    cfg.Packages,
		log:         log.With("product", cfg.Product.Metadata.Name, "source", cfg.SourceName),
		product:     cfg.Product,
		productID:   cfg.ProductID,
		sourceName:  cfg.SourceName,
		sourceCfg:   src,
		newClient:   cfg.NewClient,
		catalog:     cfg.Catalog,
		repoFilter:  repoFilter,
		tagFilter:   tagFilter,
		rules:       rules,
		targetIDs:   cfg.RepoIDs,
		layout:      layout,
		clients:     map[string]registry.Source{},
		analyseWake: make(chan struct{}, 1),
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

	// Renamed is how many EXISTING packages had their display name corrected
	// because the source's `vendor` changed.
	//
	// Reported rather than left to a log line, because it is the answer to the
	// question a person actually has after editing that field: "did it take?".
	// Zero on every steady-state scan.
	Renamed int
	// Regrouped is how many EXISTING packages were re-grouped under a newly
	// declared vendor - gaining their signature status, their related artifacts
	// and their transfer root.
	//
	// Also zero on every steady-state scan, and also non-zero exactly once, on
	// the first scan after the vendor changes.
	Regrouped int

	// TagErrors are per-tag failures that did not stop the scan.
	TagErrors []TagError
	// RepositoryErrors are per-repository failures that did not stop the scan.
	// One unreachable repository must not hide the other nineteen.
	RepositoryErrors []RepositoryError

	// Vocabulary is what this source's vendor calls a repository and a tag, so
	// a summary can be read without translating every line.
	Vocabulary vendors.Vocabulary

	Duration time.Duration

	// Collapsed reports that this result came from a scan ALREADY RUNNING when
	// the request arrived, rather than one it started.
	//
	// Reported rather than hidden. The numbers are real either way - the caller
	// waited for that scan to finish - but "a scan ran for you" and "you were
	// shown a scan that was already under way" are different facts, and an
	// operator watching a count they expect to change deserves to know which
	// one they are looking at.
	Collapsed bool
}

// TagError is a single tag's failure.
type TagError struct {
	Repository string
	Tag        string
	// DisplayRepository and DisplayTag are the VENDOR's names for the same two
	// things - `cfx-5000-k8s` and `24.7.1186` where the paths are
	// `orbs/cfx-5000-k8s` and `orb_24.7.1186`. Empty means no shortening
	// applies, which is what a conformant registry gets.
	DisplayRepository string
	DisplayTag        string
	// Class says what KIND of failure this is, where the kind changes what
	// should be done about it. Empty means an ordinary failure.
	Class string
	Err   error
}

func (e TagError) Error() string {
	if e.Repository == "" {
		return "tag " + e.Tag + ": " + e.Err.Error()
	}
	return e.Repository + " tag " + e.Tag + ": " + e.Err.Error()
}
func (e TagError) Unwrap() error { return e.Err }

// ClassNotEntitled is a source refusing content this customer has not bought.
//
// It is a separate class because it is not a failure of anything. A vendor
// registry serves a catalogue spanning every customer, and answering 403 for
// the products this one has no licence to is the correct behaviour - the
// entitlement check working, not a broken credential, an outage, or a mistake
// in configuration.
//
// Treating it as an error made every scheduled scan of a real catalogue exit
// non-zero forever, with thirty-seven lines of URL and status code attached,
// which is precisely how a monitoring signal gets ignored. What an operator
// needs is the fact, once, in the vendor's own nouns, and a scan that reports
// success because it succeeded.
const ClassNotEntitled = "not_entitled"

// tagError records one tag's failure with everything needed to report it in the
// vendor's own words, rather than in ours.
//
// The names are resolved HERE, at the moment of failure, because this is the
// only place that holds both the paths and the Layout. Doing it later would put
// the vendor's convention somewhere it is forbidden to be.
func (s *Scanner) tagError(repoPath, tag string, err error) TagError {
	return TagError{
		Repository:        repoPath,
		Tag:               tag,
		DisplayRepository: s.layout.DisplayRepository(repoPath),
		DisplayTag:        s.layout.DisplayTag(tag),
		Class:             classifyTagError(err),
		Err:               err,
	}
}

// classifyTagError decides whether a failure is a fact about entitlement.
//
// A 403 on a READ, from a registry that fronts an entitlement system, means
// one thing: this account may not have this product. 401 is deliberately NOT
// included - that is a credential that did not authenticate at all, which is a
// real fault affecting everything, and folding it in here would silence an
// outage.
func classifyTagError(err error) string {
	var rerr *registry.Error
	if errors.As(err, &rerr) && rerr.StatusCode == http.StatusForbidden {
		return ClassNotEntitled
	}
	return ""
}

// RepositoryError is a single repository's failure.
type RepositoryError struct {
	Repository string
	Err        error
}

func (e RepositoryError) Error() string { return "repository " + e.Repository + ": " + e.Err.Error() }
func (e RepositoryError) Unwrap() error { return e.Err }

// Scan performs one full scan of every repository this source covers.
//
// Returning an error means the scan could not proceed at all - the repository
// set could not be resolved. Per-repository and per-tag failures are collected
// and do not stop the scan (docs/design/07 §7).
func (s *Scanner) Scan(ctx context.Context) (ScanResult, error) {
	started := time.Now()
	res := ScanResult{Vocabulary: s.layout.Vocabulary().Or(vendors.StandardVocabulary())}

	// Progress is published from here on. The enumeration below is the first
	// thing a caller waits on and, against a slow registry, often the longest -
	// so the tracker starts before it, not after.
	s.progress.begin()
	defer s.progress.end()

	// Re-resolved every scan, for the same reason the tag list is: a repository
	// published since the last pass should be found without a restart.
	set := resolveRepositories(ctx, s.sourceCfg, s.repoFilter, s.catalog)
	res.RepositoriesFromCatalog = set.FromCatalog
	res.RepositoriesFiltered = set.Filtered

	if set.CatalogErr != nil {
		// Not fatal. A vendor forbidding `_catalog` is normal - the credential
		// is usually good for pulling named repositories, not for enumerating
		// the registry - and the repositories we WERE told about must still be
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
	// This used to be two nested semaphores - repositories outside, tags inside
	// - with separately configured limits. The trouble is that every request
	// they gate goes through ONE connection pool, so the real ceiling was
	// min(pool, R×T), and the defaults hid it by agreeing: 4 × 8 = 32 =
	// maxConnections. Change any one of the three and the system silently
	// becomes either socket-starved or over-provisioned, with no error and no
	// way to tell from the outside which.
	//
	// So the semaphore is sized to the pool, and there is one of it. It cannot
	// be nested - the outer holders would deadlock waiting for slots the inner
	// work needs - which is why the scan is now two flat phases rather than a
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

	resolved := s.resolvePhase(ctx, listed.work, listed.tags, limit)
	res.New = resolved.New
	res.Superseded = resolved.Superseded
	res.Requests = resolved.Requests
	res.Regrouped = resolved.Regrouped
	res.TagErrors = resolved.TagErrors

	// Display names are RECONCILED on every scan, not written once at discovery.
	//
	// Without this, `vendor: near` would only affect tags discovered after it
	// was set - because the phases above skip a tag already recorded, by design:
	// one HEAD, no fetch, no grouping. A source whose packages predate the
	// setting would keep showing `orb_23.8.1076` forever, and re-scanning would
	// never fix it, which is exactly what a person would try.
	//
	// It costs one query per repository and no registry traffic, and it corrects
	// in both directions: removing the vendor clears the names again.
	res.Renamed = s.reconcileDisplayNames(ctx, listed.work)

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

	// Content the vendor would not serve is written down rather than reported
	// and forgotten. On a catalogue spanning every customer this is dozens of
	// orbs on every pass, forever, and the question "which ones are we not
	// entitled to?" has no other answer.
	s.recordUnavailable(ctx, res.TagErrors)

	res.Duration = time.Since(started)

	// A scan where EVERY repository failed is a failed scan, not a successful
	// one that found nothing.
	//
	// This distinction is load-bearing. Per-repository errors are collected so
	// one unreachable repository cannot hide the other nineteen - but if none
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

// recordUnavailable persists the tags a source refused to serve.
//
// Only the classified ones. An ordinary failure - a timeout, a malformed
// manifest - is transient or is a bug, and writing it here would turn a table
// of "what we are not entitled to" into a table of everything that ever went
// wrong, which is what the log is for.
//
// A write failure is logged and swallowed. This is bookkeeping about a scan
// that has already happened; losing it must not cost the caller the scan.
func (s *Scanner) recordUnavailable(ctx context.Context, failures []TagError) {
	var rows []store.UnavailablePackage
	for _, e := range failures {
		if e.Class != ClassNotEntitled {
			continue
		}
		rows = append(rows, store.UnavailablePackage{
			Repository:        e.Repository,
			Tag:               e.Tag,
			DisplayRepository: e.DisplayRepository,
			DisplayTag:        e.DisplayTag,
			Reason:            store.ReasonNotEntitled,
			Detail:            entitlementDetail(e.Err),
		})
	}
	if len(rows) == 0 {
		return
	}
	if err := s.packages.RecordUnavailable(ctx, s.productID, rows); err != nil {
		s.log.WarnContext(ctx, "could not record unentitled content", "error", err)
	}
}

// entitlementDetail is what the registry itself said, when it said anything.
//
// Preferred over our own rendering because the vendor's sentence names the
// customer and the product - "No valid entitlement found for End User: 215952
// and Product sales items: CFXC24STD03.00" - which is what somebody takes to
// their account manager. Our rendering is a status code.
func entitlementDetail(err error) string {
	var rerr *registry.Error
	if errors.As(err, &rerr) && rerr.Detail != "" {
		return rerr.Detail
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

// reconcileDisplayNames brings stored display tags in line with what the
// source's vendor plugin says today.
//
// Reads once per repository and writes only what disagrees, so the steady state
// is one cheap SELECT and no writes at all. It runs against every package in the
// repository, including superseded ones: a superseded row is still listed, and
// it rendering under a different name from its replacement would be worse than
// either name alone.
//
// Failures are logged and swallowed. A display name is cosmetic, and failing a
// scan over one - losing the packages that scan discovered - would be a bad
// trade in every direction.
func (s *Scanner) reconcileDisplayNames(ctx context.Context, work []tagWork) int {
	repos := make(map[int64]string, len(work))
	for _, w := range work {
		repos[w.repoID] = w.repoPath
	}

	renamed := 0
	for repoID, repoPath := range repos {
		if ctx.Err() != nil {
			return renamed
		}
		rows, err := s.packages.ListPackageDisplayNames(ctx, repoID)
		if err != nil {
			s.log.WarnContext(ctx, "could not read display names", "repository", repoPath, "error", err)
			continue
		}
		for _, r := range rows {
			want := s.layout.DisplayTag(r.Tag)
			if want == r.DisplayTag {
				continue
			}
			if err := s.packages.SetDisplayTag(ctx, r.ID, want); err != nil {
				s.log.WarnContext(ctx, "could not update a display name",
					"repository", repoPath, "tag", r.Tag, "error", err)
				continue
			}
			renamed++
		}
	}

	if renamed > 0 {
		s.log.InfoContext(ctx, "corrected package display names after a vendor change",
			"source", s.sourceName, "vendor", s.layout.Name(), "packages", renamed)
	}
	return renamed
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
	// accessory marks a tag pulled in by the Layout rather than admitted by the
	// filters - NEAR's `signed_orb_X` for an admitted `orb_X`. It is fetched and
	// handed to Group, and it must never become a package of its own.
	accessory bool
}

// listOutcome is what phase one produced.
type listOutcome struct {
	work       []tagWork
	tagsListed int
	scanned    int
	failures   []repoFailure
	// tags is every tag each repository actually has, before filtering, so a
	// Layout's accessory tags can be intersected with reality rather than
	// probed for with requests that mostly 404.
	tags map[int64]map[string]bool
}

type repoFailure struct {
	path string
	err  error
}

// listPhase resolves each repository and lists its tags, bounded.
//
// It produces a FLAT work list. Nothing is fetched here beyond the tag lists,
// so the phase is cheap relative to the one that follows and finishes knowing
// the exact size of the work remaining - which is what lets progress report a
// denominator that does not move.
func (s *Scanner) listPhase(ctx context.Context, repos []string, limit int) listOutcome {
	type result struct {
		path     string
		repoID   int64
		client   registry.Source
		admitted []string
		// all is every tag the repository has, so a Layout's accessory tags can
		// be checked against it without a request each.
		all    map[string]bool
		listed int
		err    error
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
			r.all = make(map[string]bool, len(tags))
			for _, tag := range tags {
				r.all[tag] = true
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

	// Assembled in the ORIGINAL order, after the fact, so the work list - and
	// therefore the logs - come out identical run to run even though the listing
	// did not.
	out := listOutcome{tags: map[int64]map[string]bool{}}
	for _, r := range results {
		if r.err != nil {
			out.failures = append(out.failures, repoFailure{path: r.path, err: r.err})
			s.progress.update(func(p *ScanProgress) { p.Errors++ })
			continue
		}
		out.scanned++
		out.tagsListed += r.listed
		out.tags[r.repoID] = r.all
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
	// Regrouped counts existing packages corrected by a re-grouping pass.
	Regrouped int
	TagErrors []TagError
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
func (s *Scanner) resolvePhase(
	ctx context.Context, work []tagWork, repoTags map[int64]map[string]bool, limit int,
) resolveResult {
	var res resolveResult
	if len(work) == 0 {
		return res
	}

	s.progress.update(func(p *ScanProgress) { p.Phase = PhaseResolving })

	resolved := s.headPhase(ctx, work, limit)

	// A repository grouped under a DIFFERENT convention from the one configured
	// now has to be grouped again, and the head phase has just marked all its
	// tags `known` - which is exactly what would skip it. So the marks are
	// cleared for those repositories only.
	//
	// This is what makes a source's `vendor` retroactive. Without it, declaring
	// a vendor after a repository was scanned leaves every existing package
	// reading `unknown`, carrying no signature relation, and - the part that
	// actually matters - with no transfer root, so moving one would leave its
	// signature behind. Re-scanning could never fix it, because re-scanning is
	// the path that skips known tags.
	regrouping := s.repositoriesToRegroup(ctx, resolved)
	if len(regrouping) > 0 {
		for i := range resolved {
			if regrouping[resolved[i].work.repoID] {
				resolved[i].known = false
			}
		}
	}

	// Pull in the tags the Layout needs but the filters did not admit - NEAR's
	// `signed_orb_X` and `signature_orb_X` for an admitted `orb_X`.
	//
	// Only for releases that are actually being grouped: a repository where
	// nothing is new costs nothing extra, which is what keeps the steady state
	// one HEAD per tag. See accessoryPhase.
	resolved = append(resolved, s.accessoryPhase(ctx, resolved, repoTags, limit)...)

	for _, r := range resolved {
		s.progress.update(func(p *ScanProgress) { p.TagsResolved++ })
		if r.err != nil {
			// Collected, not returned. One bad artifact must not stop discovery
			// of the rest - that is how a single vendor mistake would otherwise
			// stall every release behind it.
			res.TagErrors = append(res.TagErrors,
				s.tagError(r.work.repoPath, r.work.tag, r.err))
			s.log.WarnContext(ctx, "tag resolve failed",
				"repository", r.work.repoPath, "tag", r.work.tag, "error", r.err)
			s.progress.update(func(p *ScanProgress) { p.Errors++ })
		}
	}

	items := s.fetchPhase(ctx, resolved, limit)
	for _, f := range items {
		if f.err != nil {
			res.TagErrors = append(res.TagErrors,
				s.tagError(f.work.repoPath, f.work.tag, f.err))
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
		repoID := group[0].work.repoID

		accessory := map[string]bool{}
		for _, f := range group {
			if f.work.accessory {
				accessory[f.work.tag] = true
			}
		}
		pkgs := s.groupPhase(ctx, group[0].work.client, scannedTagsFor(group), accessory)

		// A re-grouping pass starts by forgetting what the previous convention
		// concluded, so removing a vendor genuinely undoes it rather than
		// leaving marks derived from a rule that no longer applies.
		if regrouping[repoID] {
			if err := s.packages.ClearAccessories(ctx, repoID); err != nil {
				s.log.WarnContext(ctx, "could not clear accessory marks",
					"repository", repoPath, "error", err)
			}
		}

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
				// groups onto a payload discovered by an earlier scan - the
				// relations still belong on that existing package.
				if err := s.attachRelations(ctx, repoID, pkg, regrouping[repoID]); err != nil {
					s.log.WarnContext(ctx, "could not attach related artifacts",
						"repository", repoPath, "tag", pkg.Tag, "error", err)
				}
				continue
			}

			outcome, err := s.recordPackage(ctx, f.work.client, f.work.repoID,
				f.work.repoPath, pkg.Tag, f.tree.Artifacts[0].Descriptor, f.tree, pkg)
			if err != nil {
				res.TagErrors = append(res.TagErrors,
					s.tagError(repoPath, pkg.Tag, err))
				s.log.WarnContext(ctx, "package record failed",
					"repository", repoPath, "tag", pkg.Tag, "error", err)
				s.progress.update(func(p *ScanProgress) { p.Errors++ })
				continue
			}
			s.progress.update(func(p *ScanProgress) { p.Packages++ })
			if outcome.isNew {
				res.New++
				res.Superseded += outcome.superseded
				res.Requests += outcome.requests
				s.progress.update(func(p *ScanProgress) { p.New++ })
				continue
			}

			// The package already existed. On an ordinary scan that means a
			// concurrent one won the race and there is nothing to do - but on a
			// re-grouping pass it is the WHOLE POINT: this is a package recorded
			// under the previous convention, and its relations, signature status
			// and transfer root are what we came to correct.
			if regrouping[repoID] {
				if err := s.attachRelations(ctx, repoID, pkg, true); err != nil {
					s.log.WarnContext(ctx, "could not regroup an existing package",
						"repository", repoPath, "tag", pkg.Tag, "error", err)
					continue
				}
				res.Regrouped++
			}
		}

		// Recorded only after the pass succeeded for this repository. A failure
		// must leave the old value so the next scan retries, rather than
		// concluding the work was done.
		if regrouping[repoID] {
			if err := s.packages.SetGroupedLayout(ctx, repoID, s.layout.Name()); err != nil {
				s.log.WarnContext(ctx, "could not record the grouping convention",
					"repository", repoPath, "error", err)
			}
		}
	}

	return res
}

// accessoryPhase resolves the tags a Layout needs but the filters did not
// admit.
//
// THE BUG THIS FIXES. `discovery.tagFilters` is how an operator says which
// RELEASES to track, and `include: ['^orb_']` is a perfectly reasonable way to
// say "the release tags, not the noise". But under a vendor Layout, NEAR's
// `signed_orb_X` is neither noise nor a release - it is the plumbing of the
// release that WAS admitted, and it is where the signature lives. With the two
// conflated, that filter produced a catalogue in which every package read
// `unsigned`, with nothing in any output to suggest the filter was why.
//
// Only for tags actually being grouped - new ones, or a repository being
// regrouped. A repository where nothing changed adds nothing, which is what
// keeps the steady state at one HEAD per admitted tag.
//
// Intersected with the repository's real tag list rather than probed for: a
// release the vendor did not sign has no signature tag, and asking would be a
// 404 per release per scan.
func (s *Scanner) accessoryPhase(
	ctx context.Context, resolved []resolvedTag, repoTags map[int64]map[string]bool, limit int,
) []resolvedTag {
	// Tags already in the work list, so an accessory that the filters happened
	// to admit as well is not fetched twice.
	claimed := map[int64]map[string]bool{}
	for _, r := range resolved {
		if claimed[r.work.repoID] == nil {
			claimed[r.work.repoID] = map[string]bool{}
		}
		claimed[r.work.repoID][r.work.tag] = true
	}

	var extra []tagWork
	for _, r := range resolved {
		if r.err != nil || r.gone || r.known {
			continue
		}
		for _, tag := range s.layout.AccessoryTags(r.work.tag) {
			if !repoTags[r.work.repoID][tag] || claimed[r.work.repoID][tag] {
				continue
			}
			claimed[r.work.repoID][tag] = true
			extra = append(extra, tagWork{
				repoPath:  r.work.repoPath,
				repoID:    r.work.repoID,
				client:    r.work.client,
				tag:       tag,
				accessory: true,
			})
		}
	}
	if len(extra) == 0 {
		return nil
	}

	s.log.InfoContext(ctx, "resolving vendor accessory tags the filters did not admit",
		"vendor", s.layout.Name(), "tags", len(extra))

	out := s.headPhase(ctx, extra, limit)
	for i := range out {
		// An accessory is never "known": it has no package row of its own, by
		// design. Forcing the flag keeps it out of the existence check, whose
		// answer would be a permanent false.
		out[i].known = false
	}
	return out
}

// repositoriesToRegroup reports which repositories were grouped under a
// different convention from the one configured now.
//
// Cheap: one query per repository in the scan, and it answers "no" for every
// repository on every scan after the one that reconciles it. That termination
// is the reason this keys off the RECORDED LAYOUT NAME rather than off a
// symptom such as "some package still reads unknown" - a repository can
// legitimately contain unsigned packages forever, and a symptom-based trigger
// would re-fetch every tag of it on every scan for the rest of time.
func (s *Scanner) repositoriesToRegroup(ctx context.Context, resolved []resolvedTag) map[int64]bool {
	want := s.layout.Name()
	seen := map[int64]bool{}
	out := map[int64]bool{}

	for _, r := range resolved {
		id := r.work.repoID
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true

		got, err := s.packages.GroupedLayout(ctx, id)
		if err != nil {
			s.log.WarnContext(ctx, "could not read the recorded grouping convention",
				"repository", r.work.repoPath, "error", err)
			continue
		}
		if got == want {
			continue
		}
		out[id] = true
		s.log.InfoContext(ctx, "regrouping a repository under a new vendor convention",
			"repository", r.work.repoPath, "was", dashIfEmpty(got), "now", want)
	}
	return out
}

// dashIfEmpty renders "never grouped" for a log line.
func dashIfEmpty(s string) string {
	if s == "" {
		return "(never grouped)"
	}
	return s
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

	// The shortened spelling for listings, from the source's vendor plugin -
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
	// Everything expensive - the HEAD, the existence check, the manifest oci.Tree
	// fetch - already happened outside this call and runs fully in parallel.
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
		// for signatures at all, which is honest - claiming `unsigned` there
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
	// Parents precede children in the slice - fetchTree walks breadth-first -
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

	// The rule says WHICH software; the download says what happens to it.
	dl, err := s.product.DownloadFor(rule)
	if err == nil {
		var steps []download.ResolvedStep
		steps, err = download.Resolve(s.product, dl, s.targetIDs)
		if err == nil {
			return s.openRun(ctx, tx, packageID, sourceRepoID, tag, rule, dl, steps)
		}
	}
	if err != nil {
		// A misconfigured rule must not fail the discovery - the package is
		// real and worth recording either way. Logged loudly instead.
		s.log.ErrorContext(ctx, "auto-download rule could not be applied",
			"rule", rule.Name, "tag", tag, "error", err)
		return 0, nil
	}
	return 0, nil
}

// openRun records the request and the ordered steps a rule's download needs.
func (s *Scanner) openRun(
	ctx context.Context, tx *sql.Tx, packageID, sourceRepoID int64, tag string,
	rule product.Rule, dl product.Download, steps []download.ResolvedStep,
) (int, error) {
	targetNames := download.Names(steps)

	priority := dl.EffectivePriority()
	// The key covers the DOWNLOAD, not the rule that triggered it: two rules
	// pointing at one download for one package are asking for one piece of
	// work, and keying them apart would move the bytes twice.
	key := transfer.IdempotencyKey("replicate", packageID, sourceRepoID,
		download.RepoIDs(steps), download.Revision(dl), priority)

	id, created, err := download.Open(ctx, tx, s.packages, download.Request{
		ProductID:      s.productID,
		ProductName:    s.product.Metadata.Name,
		PackageID:      packageID,
		SourceRepoID:   sourceRepoID,
		Tag:            tag,
		DownloadName:   dl.Name,
		RuleName:       rule.Name,
		Trigger:        download.TriggerDiscovery,
		Origin:         "auto_download",
		RequestedBy:    "auto_download:" + rule.Name,
		Priority:       priority,
		IdempotencyKey: key,
		Steps:          steps,
	})
	if err != nil {
		return 0, err
	}
	if !created {
		return 0, nil
	}

	s.log.InfoContext(ctx, "auto-download rule matched",
		"rule", rule.Name, "tag", tag, "chain", targetNames, "request", id, "priority", priority)

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
