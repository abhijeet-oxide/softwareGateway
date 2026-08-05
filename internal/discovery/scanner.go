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

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
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
}

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

	for _, repoPath := range set.Repositories {
		if err := ctx.Err(); err != nil {
			return res, err
		}

		sub, err := s.scanRepository(ctx, repoPath)
		if err != nil {
			res.RepositoryErrors = append(res.RepositoryErrors,
				RepositoryError{Repository: repoPath, Err: err})
			s.log.WarnContext(ctx, "repository scan failed", "repository", repoPath, "error", err)
			continue
		}

		res.Repositories++
		res.TagsListed += sub.TagsListed
		res.TagsAdmitted += sub.TagsAdmitted
		res.New += sub.New
		res.Superseded += sub.Superseded
		res.Requests += sub.Requests
		res.TagErrors = append(res.TagErrors, sub.TagErrors...)
	}

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

// repoResult is one repository's contribution to a scan.
type repoResult struct {
	TagsListed   int
	TagsAdmitted int
	New          int
	Superseded   int
	Requests     int
	TagErrors    []TagError
}

// scanRepository scans every admitted tag in one repository.
func (s *Scanner) scanRepository(ctx context.Context, repoPath string) (repoResult, error) {
	var res repoResult

	client, err := s.clientFor(repoPath)
	if err != nil {
		return res, err
	}

	repoID, err := s.ensureRepositoryRow(ctx, repoPath)
	if err != nil {
		return res, err
	}

	tags, err := s.listTags(ctx, client)
	if err != nil {
		return res, err
	}
	res.TagsListed = len(tags)

	for _, tag := range tags {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if !s.tagFilter.admits(tag) {
			continue
		}
		res.TagsAdmitted++

		outcome, err := s.scanTag(ctx, client, repoID, repoPath, tag)
		if err != nil {
			// Collected, not returned. One bad artifact must not stop discovery
			// of the rest — that is how a single vendor mistake would otherwise
			// stall every release behind it.
			res.TagErrors = append(res.TagErrors, TagError{Repository: repoPath, Tag: tag, Err: err})
			s.log.WarnContext(ctx, "tag scan failed",
				"repository", repoPath, "tag", tag, "error", err)
			continue
		}
		if outcome.isNew {
			res.New++
			res.Superseded += outcome.superseded
			res.Requests += outcome.requests
		}
	}
	return res, nil
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

	id, err := s.packages.EnsureRepository(ctx, tx,
		s.productID, string(product.RoleSource), name,
		s.sourceCfg.Registry, repoPath, string(s.sourceCfg.Type), managedBy)
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

// scanTag resolves one tag and records it if it is new.
func (s *Scanner) scanTag(
	ctx context.Context, client registry.Source, repoID int64, repoPath, tag string,
) (tagOutcome, error) {
	// HEAD only: the body is not fetched. Discovery calls this for every tag on
	// every scan, so the common case — nothing changed — costs one small
	// request per tag and transfers no manifest bodies.
	desc, err := client.ResolveTag(ctx, tag)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			// The tag was deleted between the list and the resolve. Not an
			// error: the next scan will not list it.
			return tagOutcome{}, nil
		}
		return tagOutcome{}, err
	}
	if err := desc.Digest.Validate(); err != nil {
		return tagOutcome{}, err
	}

	// An optimisation, not the correctness mechanism. It skips the expensive
	// part — the manifest-tree fetch — for content already recorded. The unique
	// constraint inside recordPackage is what actually prevents a duplicate, so
	// a scan racing us between this check and that insert is harmless.
	known, err := s.packages.PackageExists(ctx, repoID, tag, desc.Digest.String())
	if err != nil {
		return tagOutcome{}, err
	}
	if known {
		return tagOutcome{}, nil
	}

	// Fetched BEFORE the transaction opens: this is network I/O of unbounded
	// duration, and holding a database transaction across it would pin a
	// connection and a snapshot for as long as the vendor takes to answer.
	t, err := fetchTree(ctx, client, desc)
	if err != nil {
		return tagOutcome{}, err
	}

	return s.recordPackage(ctx, client, repoID, repoPath, tag, desc, t)
}

// recordPackage writes a new package and everything that follows from it, in
// one transaction.
//
// The package, its artifact tree, the audit event, the notification and any
// auto-download request are ONE atomic fact. A package that exists without the
// notification announcing it, or a transfer request pointing at a package that
// was rolled back, are precisely the states the outbox pattern exists to make
// impossible (docs/design/07 §6).
func (s *Scanner) recordPackage(
	ctx context.Context, client registry.Source, repoID int64,
	repoPath, tag string, desc registry.Descriptor, t tree,
) (tagOutcome, error) {
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

// writeTree persists the artifact tree and its blob references.
func (s *Scanner) writeTree(ctx context.Context, tx *sql.Tx, packageID int64, t tree) error {
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
