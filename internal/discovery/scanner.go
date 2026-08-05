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
package discovery

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// tagPageSize is the page size requested from tags/list. Registries may return
// fewer, and some ignore it entirely.
const tagPageSize = 200

// Scanner scans one source repository.
//
// One per source, holding its compiled filters and rules so a scan does no
// configuration work.
type Scanner struct {
	source   registry.Source
	packages *store.Packages
	log      *slog.Logger

	product      *product.Product
	productID    int64
	sourceName   string
	sourceRepoID int64
	// repoIDs maps a configured repository name to its catalog row ID.
	repoIDs map[string]int64

	filter tagFilter
	rules  ruleSet
}

// ScannerConfig builds a Scanner.
type ScannerConfig struct {
	Source       registry.Source
	Packages     *store.Packages
	Logger       *slog.Logger
	Product      *product.Product
	ProductID    int64
	SourceName   string
	SourceRepoID int64
	RepoIDs      map[string]int64
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

	filter, err := compileTagFilter(src.Discovery.TagFilters)
	if err != nil {
		return nil, fmt.Errorf("product %q source %q: %w", cfg.Product.Metadata.Name, cfg.SourceName, err)
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
		source:       cfg.Source,
		packages:     cfg.Packages,
		log:          log.With("product", cfg.Product.Metadata.Name, "source", cfg.SourceName),
		product:      cfg.Product,
		productID:    cfg.ProductID,
		sourceName:   cfg.SourceName,
		sourceRepoID: cfg.SourceRepoID,
		repoIDs:      cfg.RepoIDs,
		filter:       filter,
		rules:        rules,
	}, nil
}

// ScanResult reports what one scan did.
type ScanResult struct {
	TagsListed   int
	TagsAdmitted int
	New          int
	Superseded   int
	Requests     int
	// TagErrors are per-tag failures that did not stop the scan. A malformed
	// manifest on one tag must not hide the other forty-nine.
	TagErrors []TagError
	Duration  time.Duration
}

// TagError is a single tag's failure.
type TagError struct {
	Tag string
	Err error
}

func (e TagError) Error() string { return "tag " + e.Tag + ": " + e.Err.Error() }
func (e TagError) Unwrap() error { return e.Err }

// Scan performs one full scan.
//
// Returning an error means the scan could not proceed at all — the registry was
// unreachable or refused the tag list. Per-tag failures are collected in
// ScanResult.TagErrors and do not stop the scan (docs/design/07 §7).
func (s *Scanner) Scan(ctx context.Context) (ScanResult, error) {
	started := time.Now()
	var res ScanResult

	tags, err := s.listTags(ctx)
	if err != nil {
		return res, err
	}
	res.TagsListed = len(tags)

	for _, tag := range tags {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if !s.filter.admits(tag) {
			continue
		}
		res.TagsAdmitted++

		outcome, err := s.scanTag(ctx, tag)
		if err != nil {
			// Collected, not returned. One bad artifact must not stop discovery
			// of the rest — that is how a single vendor mistake would otherwise
			// stall every release behind it.
			res.TagErrors = append(res.TagErrors, TagError{Tag: tag, Err: err})
			s.log.WarnContext(ctx, "tag scan failed", "tag", tag, "error", err)
			continue
		}
		if outcome.isNew {
			res.New++
			res.Superseded += outcome.superseded
			res.Requests += outcome.requests
		}
	}

	res.Duration = time.Since(started)
	return res, nil
}

// listTags pages through the whole tag list.
func (s *Scanner) listTags(ctx context.Context) ([]string, error) {
	var all []string
	last := ""

	// Bounded so a registry with a broken pagination cursor cannot loop
	// forever. At 200 tags a page this admits 100k tags, far past the point
	// where tagFilters should be in use.
	const maxPages = 500

	for page := 0; page < maxPages; page++ {
		tags, next, err := s.source.ListTags(ctx, last, tagPageSize)
		if err != nil {
			return nil, fmt.Errorf("list tags for %s: %w", s.source.Name(), err)
		}
		all = append(all, tags...)
		if next == "" || next == last {
			return all, nil
		}
		last = next
	}
	return all, fmt.Errorf("list tags for %s: exceeded %d pages", s.source.Name(), maxPages)
}

// tagOutcome reports what one tag produced.
type tagOutcome struct {
	isNew      bool
	packageID  int64
	superseded int
	requests   int
}

// scanTag resolves one tag and records it if it is new.
func (s *Scanner) scanTag(ctx context.Context, tag string) (tagOutcome, error) {
	// HEAD only: the body is not fetched. Discovery calls this for every tag on
	// every scan, so the common case — nothing changed — costs one small
	// request per tag and transfers no manifest bodies.
	desc, err := s.source.ResolveTag(ctx, tag)
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
	known, err := s.packages.PackageExists(ctx, s.sourceRepoID, tag, desc.Digest.String())
	if err != nil {
		return tagOutcome{}, err
	}
	if known {
		return tagOutcome{}, nil
	}

	// Fetched BEFORE the transaction opens: this is network I/O of unbounded
	// duration, and holding a database transaction across it would pin a
	// connection and a snapshot for as long as the vendor takes to answer.
	t, err := fetchTree(ctx, s.source, desc)
	if err != nil {
		return tagOutcome{}, err
	}

	return s.recordPackage(ctx, tag, desc, t)
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
	ctx context.Context, tag string, desc registry.Descriptor, t tree,
) (tagOutcome, error) {
	tx, err := s.packages.DB().BeginTx(ctx, nil)
	if err != nil {
		return tagOutcome{}, fmt.Errorf("begin package transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	packageID, err := s.packages.InsertPackage(ctx, tx, store.PackageRow{
		ProductID:      s.productID,
		SourceRepoID:   s.sourceRepoID,
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

	// Supersede any earlier package carrying THE SAME TAG. Different tags are
	// independent versions and are never touched (docs/design/07 §4).
	superseded, err := s.packages.SupersedePrior(ctx, tx, s.sourceRepoID, tag, packageID)
	if err != nil {
		return tagOutcome{}, err
	}

	if err := s.writeAudit(ctx, tx, packageID, tag, desc, superseded); err != nil {
		return tagOutcome{}, err
	}
	if err := s.notify(ctx, tx, packageID, tag, desc, superseded); err != nil {
		return tagOutcome{}, err
	}

	requests, err := s.applyRules(ctx, tx, packageID, tag)
	if err != nil {
		return tagOutcome{}, err
	}

	if err := tx.Commit(); err != nil {
		return tagOutcome{}, fmt.Errorf("commit package %s: %w", tag, err)
	}

	s.log.InfoContext(ctx, "discovered package",
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
	ctx context.Context, tx *sql.Tx, packageID int64, tag string, desc registry.Descriptor, superseded int64,
) error {
	detail, _ := json.Marshal(map[string]any{
		"tag":        tag,
		"digest":     desc.Digest.String(),
		"source":     s.sourceName,
		"repository": s.source.Name(),
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
	ctx context.Context, tx *sql.Tx, packageID int64, tag string, desc registry.Descriptor, superseded int64,
) error {
	n := s.product.Spec.Notifications
	if !n.Enabled {
		return nil
	}

	payload, err := json.Marshal(map[string]any{
		"product":    s.product.Metadata.Name,
		"source":     s.sourceName,
		"repository": s.source.Name(),
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
func (s *Scanner) applyRules(ctx context.Context, tx *sql.Tx, packageID int64, tag string) (int, error) {
	rule, ok := s.rules.match(tag)
	if !ok {
		return 0, nil
	}

	targetIDs, targetNames, err := resolveTargets(s.product, rule, s.repoIDs)
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
		SourceRepoID:   s.sourceRepoID,
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
