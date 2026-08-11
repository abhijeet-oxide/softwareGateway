package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Package persistence: rows in, rows out.
//
// Nothing here decides what a package MEANS — whether a tag should be
// discovered, which rule matches it, whether a re-push is interesting. That
// belongs in internal/discovery. This file owns the SQL and nothing else.

// PackageRow is a row in `packages`.
type PackageRow struct {
	ID             int64
	ProductID      int64
	SourceRepoID   int64
	Tag            string
	ManifestDigest string
	MediaType      string
	// TotalBytes and BlobCount are NIL when not yet measured, which is the case
	// for a package whose root is an index: discovery records what the index
	// lists without fetching it, so the layer bytes underneath are unknown
	// until a transfer walks the tree. Nil rather than zero — a wrong size is
	// worse than an absent one, because nobody questions a number.
	TotalBytes    *int64
	ArtifactCount int
	BlobCount     *int
	State         string
	DiscoveredAt  string
	// PublishedAt is when the VENDOR says the artifact was built, from the
	// standard org.opencontainers.image.created annotation. Nil when the
	// publisher set none, which the spec permits.
	//
	// Deliberately separate from DiscoveredAt: one is a claim we were handed,
	// the other an observation we made. Merging them would lose the ability to
	// say which — and "published in March, we only noticed in July" is exactly
	// the sort of thing worth being able to see.
	PublishedAt  *string
	SupersededBy *int64

	// SignatureStatus is signed | unsigned | unknown. Three values, because
	// "we looked and found none" and "nobody looked" are different facts and a
	// boolean cannot tell them apart — which matters when the question being
	// answered is whether to trust something.
	SignatureStatus string
	// DisplayTag is Tag with the vendor's structural noise removed, or empty
	// when none applies. Cosmetic; Tag is the identity.
	DisplayTag string
	// TransferRootDigest is what a transfer plans from when that is NOT this
	// package's own manifest.
	//
	// Empty for the ordinary case. Set where a vendor bundles the payload and
	// its signature under a wrapper index: only the wrapper reaches both, so
	// planning from the payload alone would move the bytes and leave the
	// signature behind.
	TransferRootDigest string
	TransferRootTag    string
	// SourceRepository is the repository path this was discovered in. Joined
	// on read rather than denormalised: a product may span several
	// repositories, and a listing that does not say which is ambiguous.
	SourceRepository string
	// DisplayRepository is SourceRepository with the vendor's structural noise
	// removed, or empty when none applies. Cosmetic; SourceRepository is the
	// identity. Both spellings resolve as input.
	DisplayRepository string

	// AccessoryOf names the package this row turned out to be PART OF — a
	// signature or a wrapper the vendor publishes as its own tag.
	//
	// Nil for an ordinary package. Set only by a re-grouping pass over rows
	// recorded before their source declared a vendor: a tag discovered after
	// that never becomes a package at all, because grouping absorbs it.
	//
	// The row keeps its history and stays reachable by explicit reference; it
	// simply stops being listed as a release of its own.
	AccessoryOf *int64

	// ExpandedAt is when this package's manifest tree was last fully walked,
	// or nil if it never has been.
	//
	// The difference between the two states is what `packages describe` reports
	// and what `packages inspect` changes: a package that has only been
	// discovered knows what its own manifest lists and NOT what those artifacts
	// contain, so its size is unknown rather than zero.
	ExpandedAt *string
}

// ArtifactRow is a row in `package_artifacts`: one manifest in the tree.
type ArtifactRow struct {
	ID           int64
	PackageID    int64
	ParentID     *int64
	Digest       string
	MediaType    string
	ArtifactType string
	SizeBytes    int64
	Platform     string
	Depth        int
	// Annotations is the artifact's annotation map, stored verbatim as JSON.
	//
	// Kept whole rather than picked apart so a vendor's own keys survive
	// without this project knowing they exist. The alternative is a column per
	// vendor, which does not end.
	Annotations map[string]string
	// Raw is the manifest exactly as served, when it is still CACHED.
	//
	// Nil means one of two different things, and Fetched is what tells them
	// apart: either this artifact was only LISTED by its parent index and never
	// fetched, or it was fetched and its bytes have since been evicted. See
	// migration 00007 — the bytes are a cache in front of the source registry
	// and the fetch is a fact, and conflating them made the cache impossible to
	// reclaim.
	Raw []byte
	// Fetched is `fetched_at IS NOT NULL`: this manifest was pulled from the
	// registry and verified against its digest, so its blobs and its size are
	// known. Read back without the bytes themselves — a listing must not load
	// every manifest body to report which ones we walked.
	Fetched bool
	// Cached is `raw IS NOT NULL`: the bytes are still here, so pushing this
	// manifest needs no round trip to the source.
	Cached bool
}

// BlobRef links an artifact to a blob it references.
type BlobRef struct {
	Digest    string
	MediaType string
	SizeBytes int64
	// Kind is "config" or "layer", matching the CHECK constraint.
	Kind string
	// Ordinal preserves layer order, which matters: layer order is part of the
	// image, and a manifest reassembled with layers transposed is a different
	// image that happens to contain the same bytes.
	Ordinal int
}

// Packages is the persistence surface discovery needs.
//
// Every method takes an explicit *sql.Tx rather than opening its own. Discovery
// writes a package, its artifact tree, an audit event, a notification and
// possibly a transfer request as ONE atomic fact — a package that exists
// without the notification that announces it is precisely the failure the
// outbox pattern exists to prevent (docs/design/07 §6).
type Packages struct {
	db      *sql.DB
	dialect Dialect
}

// NewPackages builds the package store.
func NewPackages(s Store) *Packages {
	return &Packages{db: s.DB(), dialect: DialectFor(s.Driver())}
}

// DB exposes the handle so callers can open the transaction they will pass
// back in.
func (p *Packages) DB() *sql.DB { return p.db }

// Dialect exposes the dialect for callers assembling their own statements.
func (p *Packages) Dialect() Dialect { return p.dialect }

// ErrAlreadyExists reports that a unique constraint absorbed the write.
//
// Not an error condition anywhere in discovery — it is the expected result of
// re-scanning a repository, which happens every fifteen minutes forever. It is
// an error VALUE rather than a bool return so it cannot be silently ignored by
// a caller that forgot to check.
var ErrAlreadyExists = errors.New("row already exists")

// InsertPackage inserts a discovered package.
//
// Returns ErrAlreadyExists when (source_repo_id, tag, manifest_digest) is
// already recorded. THIS is the idempotency mechanism for discovery — not an
// application-level "have I seen this?" lookup, which would have a race between
// the check and the insert. A repeated scan, two overlapping scans, or a scan
// that crashed halfway and restarted all converge here (docs/design/07 §2).
func (p *Packages) InsertPackage(ctx context.Context, tx *sql.Tx, row PackageRow) (int64, error) {
	state := row.State
	if state == "" {
		state = "discovered"
	}

	// A package discovery could already measure IS fully known, and saying so
	// here saves an inspect that would fetch nothing.
	//
	// That is the case whenever the root is a plain image manifest: its config
	// and layer descriptors are inside the one manifest discovery fetched, so
	// there is no deeper tree to walk. An index-rooted package leaves
	// total_bytes nil, because the index states the size of each child manifest
	// and not of the layers beneath it — and that is exactly what `inspect`
	// exists to resolve.
	expanded := "NULL"
	if row.TotalBytes != nil {
		expanded = p.dialect.Now()
	}

	query := p.dialect.Rewrite(`
		INSERT INTO packages
			(product_id, source_repo_id, tag, manifest_digest, media_type,
			 total_bytes, artifact_count, blob_count, published_at, state,
			 signature_status, transfer_root_digest, transfer_root_tag, display_tag,
			 expanded_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ` + expanded + `, ` + p.dialect.Now() + `)
		ON CONFLICT (source_repo_id, tag, manifest_digest) DO NOTHING
		RETURNING id`)

	status := row.SignatureStatus
	if status == "" {
		status = "unknown"
	}

	var id int64
	err := tx.QueryRowContext(ctx, query,
		row.ProductID, row.SourceRepoID, row.Tag, row.ManifestDigest, row.MediaType,
		row.TotalBytes, row.ArtifactCount, row.BlobCount, row.PublishedAt, state,
		status, nullIfEmpty(row.TransferRootDigest), nullIfEmpty(row.TransferRootTag),
		nullIfEmpty(row.DisplayTag),
	).Scan(&id)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		// DO NOTHING suppressed the insert, so RETURNING produced no row. Both
		// dialects behave this way, and it is the signal we want.
		return 0, ErrAlreadyExists
	case err != nil:
		return 0, fmt.Errorf("insert package %s:%s: %w", row.Tag, row.ManifestDigest, err)
	}
	return id, nil
}

// SupersedePrior marks earlier packages carrying THE SAME TAG as superseded.
//
// Note `tag = ?`: the statement cannot touch a package with a different tag.
// This is the point stressed in docs/design/07 §4 — v2.13.0 and v2.14.0 are
// independent software versions that coexist indefinitely, and discovering one
// does nothing to the other. Supersession is exactly one situation: the same
// tag re-pushed with different content.
//
// The old row's history — what was replicated, when, to where, whether it
// verified — is preserved. Overwriting in place would be simpler and would
// destroy the ability to answer "which bytes did we actually ship in March".
func (p *Packages) SupersedePrior(
	ctx context.Context, tx *sql.Tx, sourceRepoID int64, tag string, newPackageID int64,
) (int64, error) {
	query := p.dialect.Rewrite(`
		UPDATE packages
		   SET state = 'superseded', superseded_by = ?, updated_at = ` + p.dialect.Now() + `
		 WHERE source_repo_id = ?
		   AND tag = ?
		   AND id <> ?
		   AND state <> 'superseded'`)

	res, err := tx.ExecContext(ctx, query, newPackageID, sourceRepoID, tag, newPackageID)
	if err != nil {
		return 0, fmt.Errorf("supersede prior packages for tag %q: %w", tag, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil // not every driver reports this; not worth failing over
	}
	return n, nil
}

// InsertArtifact records one manifest in a package's tree.
func (p *Packages) InsertArtifact(ctx context.Context, tx *sql.Tx, a ArtifactRow) (int64, error) {
	// fetched_at and the cache stamps move together with the bytes: a row
	// written WITH raw was fetched and is cached, a row written without was
	// merely listed by its parent index and is neither.
	//
	// Written as a literal in the statement rather than as a parameter, because
	// "now" is a dialect expression and a Go time would be formatted by the
	// driver — which for SQLite means a string that does not sort against the
	// ones the schema's own DEFAULT writes.
	query := p.dialect.Rewrite(`
		INSERT INTO package_artifacts
			(package_id, parent_id, digest, media_type, artifact_type,
			 size_bytes, platform, depth, raw, annotations,
			 fetched_at, raw_bytes, raw_used_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ` + p.fetchStamps(a.Raw) + `)
		ON CONFLICT (package_id, digest) DO NOTHING
		RETURNING id`)

	var id int64
	err := tx.QueryRowContext(ctx, query,
		a.PackageID, a.ParentID, a.Digest, a.MediaType, nullable(a.ArtifactType),
		a.SizeBytes, nullable(a.Platform), a.Depth, rawOrNull(a.Raw), annotationsJSON(a.Annotations),
	).Scan(&id)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		// The same manifest can legitimately appear twice in one tree — an index
		// listing the same digest under two platforms, say. Recording it once is
		// correct, so resolve to the existing row rather than failing.
		return p.artifactID(ctx, tx, a.PackageID, a.Digest)
	case err != nil:
		return 0, fmt.Errorf("insert artifact %s: %w", a.Digest, err)
	}
	return id, nil
}

// fetchStamps renders the `fetched_at, raw_bytes, raw_used_at` triple for a
// write that either carries manifest bytes or does not.
//
// One place, because the three must agree: a row claiming fetched_at with no
// raw_bytes would make the cache sweeper's accounting wrong, and a row carrying
// bytes with no fetched_at would be evicted and then re-walked forever.
func (p *Packages) fetchStamps(raw []byte) string {
	if len(raw) == 0 {
		return "NULL, 0, NULL"
	}
	now := p.dialect.Now()
	return now + ", " + strconv.Itoa(len(raw)) + ", " + now
}

// rawOrNull writes SQL NULL for an absent manifest body.
//
// A zero-length []byte is not the same as nil to every driver, and the
// distinction here is load-bearing: NULL means "not held", and an empty blob
// would be a manifest of nothing.
func rawOrNull(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	return raw
}

func (p *Packages) artifactID(ctx context.Context, tx *sql.Tx, packageID int64, digest string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx,
		p.dialect.Rewrite(`SELECT id FROM package_artifacts WHERE package_id = ? AND digest = ?`),
		packageID, digest,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("resolve existing artifact %s: %w", digest, err)
	}
	return id, nil
}

// LinkBlobs records the blobs an artifact references.
//
// `blobs` is content-addressed and global: the same layer shared by fifty
// packages is one row. That is what makes the placement model in M3 able to
// answer "is this blob already at the destination" without knowing which
// package asked.
func (p *Packages) LinkBlobs(ctx context.Context, tx *sql.Tx, artifactID int64, refs []BlobRef) error {
	blobUpsert := p.dialect.Rewrite(`
		INSERT INTO blobs (digest, size_bytes, media_type)
		VALUES (?, ?, ?)
		ON CONFLICT (digest) DO NOTHING`)

	linkInsert := p.dialect.Rewrite(`
		INSERT INTO artifact_blobs (artifact_id, digest, kind, ordinal)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (artifact_id, digest, kind) DO NOTHING`)

	for _, r := range refs {
		if _, err := tx.ExecContext(ctx, blobUpsert, r.Digest, r.SizeBytes, nullable(r.MediaType)); err != nil {
			return fmt.Errorf("upsert blob %s: %w", r.Digest, err)
		}
		if _, err := tx.ExecContext(ctx, linkInsert, artifactID, r.Digest, r.Kind, r.Ordinal); err != nil {
			return fmt.Errorf("link blob %s to artifact %d: %w", r.Digest, artifactID, err)
		}
	}
	return nil
}

// TransferRequestRow is a row in `transfer_requests`.
type TransferRequestRow struct {
	ID             string
	ProductID      int64
	PackageID      int64
	Operation      string
	SourceRepoID   int64
	Priority       int
	IdempotencyKey string
	RequestedBy    string
	RequestOrigin  string
	AutoRuleName   string
}

// CreateTransferRequest inserts a request, or returns the existing one's ID.
//
// UNIQUE (idempotency_key) is what makes auto-download safe to run in a loop.
// This matters more here than anywhere else in the system: an auto-download
// rule is the one path that creates tens of gigabytes of work with no human in
// the loop (docs/design/07 §5).
//
// The bool reports whether this call created the row, so a caller can tell a
// genuinely new request from a replay — the API surface needs it to answer 201
// versus 200.
func (p *Packages) CreateTransferRequest(
	ctx context.Context, tx *sql.Tx, row TransferRequestRow,
) (string, bool, error) {
	query := p.dialect.Rewrite(`
		INSERT INTO transfer_requests
			(id, product_id, package_id, operation, source_repo_id, priority,
			 idempotency_key, requested_by, request_origin, auto_rule_name, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ` + p.dialect.Now() + `)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id`)

	var id string
	err := tx.QueryRowContext(ctx, query,
		row.ID, row.ProductID, row.PackageID, row.Operation, row.SourceRepoID,
		row.Priority, row.IdempotencyKey, row.RequestedBy, row.RequestOrigin,
		nullable(row.AutoRuleName),
	).Scan(&id)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		existing, err := p.requestByKey(ctx, tx, row.IdempotencyKey)
		if err != nil {
			return "", false, err
		}
		return existing, false, nil
	case err != nil:
		return "", false, fmt.Errorf("create transfer request for package %d: %w", row.PackageID, err)
	}
	return id, true, nil
}

func (p *Packages) requestByKey(ctx context.Context, tx *sql.Tx, key string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx,
		p.dialect.Rewrite(`SELECT id FROM transfer_requests WHERE idempotency_key = ?`), key,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("resolve existing transfer request: %w", err)
	}
	return id, nil
}

// AuditRow is a row in `audit_events`.
type AuditRow struct {
	EventType   string
	Actor       string
	ActorKind   string
	ProductName string
	SubjectKind string
	SubjectID   string
	Outcome     string
	Detail      string
}

// InsertAudit appends an audit event.
func (p *Packages) InsertAudit(ctx context.Context, tx *sql.Tx, row AuditRow) error {
	actor, actorKind, outcome := row.Actor, row.ActorKind, row.Outcome
	if actor == "" {
		actor = "system"
	}
	if actorKind == "" {
		actorKind = "system"
	}
	if outcome == "" {
		outcome = "success"
	}

	query := p.dialect.Rewrite(`
		INSERT INTO audit_events
			(event_type, actor, actor_kind, product_name, subject_kind, subject_id, outcome, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)

	if _, err := tx.ExecContext(ctx, query,
		row.EventType, actor, actorKind, nullable(row.ProductName),
		nullable(row.SubjectKind), nullable(row.SubjectID), outcome, nullable(row.Detail),
	); err != nil {
		return fmt.Errorf("insert audit event %s: %w", row.EventType, err)
	}
	return nil
}

// NotificationRow is a row in the `notifications` outbox.
type NotificationRow struct {
	ProductID   int64
	EventType   string
	ChannelName string
	ChannelType string
	SubjectKind string
	SubjectID   string
	Payload     string
	DedupeKey   string
}

// EnqueueNotification writes to the outbox.
//
// Written in the caller's transaction, which is the entire reason the outbox
// exists: it is impossible to insert the package and fail to enqueue the
// notification, or to notify about a package that was rolled back. Delivery is
// a separate retried concern; DECIDING to notify is atomic with the fact that
// caused it (docs/design/07 §6).
func (p *Packages) EnqueueNotification(ctx context.Context, tx *sql.Tx, row NotificationRow) error {
	query := p.dialect.Rewrite(`
		INSERT INTO notifications
			(product_id, event_type, channel_name, channel_type,
			 subject_kind, subject_id, payload, dedupe_key)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (dedupe_key) DO NOTHING`)

	if _, err := tx.ExecContext(ctx, query,
		row.ProductID, row.EventType, row.ChannelName, row.ChannelType,
		row.SubjectKind, row.SubjectID, row.Payload, row.DedupeKey,
	); err != nil {
		return fmt.Errorf("enqueue notification %s: %w", row.EventType, err)
	}
	return nil
}

// EnsureRepository returns a repository row's ID, creating it if absent.
//
// Discovery calls this for every repository it resolves — including ones found
// in the registry catalog, which by definition are not in configuration and so
// have no row yet. `packages` has a foreign key to `repositories`, so the row
// must exist before anything discovered there can be recorded.
//
// managedBy is 'config' or 'discovery'. It exists so reconciliation can
// deactivate declarations a human removed without touching rows discovery
// found — otherwise every configuration reload would deactivate every
// discovered repository and the next scan would revive it, flapping the row
// and churning the audit trail for no reason.
// displayPath is the vendor-shortened spelling, or empty where none applies. It
// is refreshed on every upsert rather than written once, so turning `vendor:
// near` on — or off — takes effect on the next scan instead of only for
// repositories discovered afterwards.
func (p *Packages) EnsureRepository(
	ctx context.Context, tx *sql.Tx,
	productID int64, role, name, registryHost, repositoryPath, registryType, managedBy, displayPath string,
) (int64, error) {
	if registryType == "" {
		registryType = "generic"
	}
	if managedBy == "" {
		managedBy = "discovery"
	}

	// Physical identity is the conflict target, matching the unique index: one
	// registry host plus repository path is one row, whoever created it.
	upsert := p.dialect.Rewrite(`
		INSERT INTO repositories
			(product_id, role, name, registry_host, repository_path, registry_type,
			 managed_by, display_path, active, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ` + p.dialect.Bool(true) + `, ` + p.dialect.Now() + `)
		ON CONFLICT (registry_host, repository_path) DO UPDATE SET
			active       = ` + p.dialect.Bool(true) + `,
			display_path = EXCLUDED.display_path,
			updated_at   = ` + p.dialect.Now() + `
		RETURNING id`)

	var id int64
	err := tx.QueryRowContext(ctx, upsert,
		productID, role, name, registryHost, repositoryPath, registryType, managedBy,
		nullIfEmpty(displayPath),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("ensure repository %s/%s: %w", registryHost, repositoryPath, err)
	}
	return id, nil
}

// DeactivateDiscoveredRepositories marks discovery-managed rows for a product
// inactive when they are no longer in the registry.
//
// Only rows this source discovered are eligible: `managed_by = 'discovery'`
// protects a human's declaration from being switched off because a catalog
// call failed or a filter changed.
func (p *Packages) DeactivateDiscoveredRepositories(
	ctx context.Context, productID int64, registryHost string, keep []string,
) (int64, error) {
	query := `
		UPDATE repositories
		   SET active = ` + p.dialect.Bool(false) + `, updated_at = ` + p.dialect.Now() + `
		 WHERE product_id = ?
		   AND registry_host = ?
		   AND managed_by = 'discovery'
		   AND active = ` + p.dialect.Bool(true)

	args := []any{productID, registryHost}
	if len(keep) > 0 {
		placeholders := make([]byte, 0, len(keep)*2)
		for _, r := range keep {
			if len(placeholders) > 0 {
				placeholders = append(placeholders, ',')
			}
			placeholders = append(placeholders, '?')
			args = append(args, r)
		}
		query += " AND repository_path NOT IN (" + string(placeholders) + ")"
	}

	res, err := p.db.ExecContext(ctx, p.dialect.Rewrite(query), args...)
	if err != nil {
		return 0, fmt.Errorf("deactivate discovered repositories on %s: %w", registryHost, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil // not every driver reports this
	}
	return n, nil
}

// PackageExists reports whether this exact content is already recorded.
//
// An OPTIMISATION, not the correctness mechanism. It lets a scan where nothing
// changed skip the manifest-tree fetch — the expensive part — for tags already
// known. The unique constraint in InsertPackage is what actually guarantees no
// duplicate, so a racing scan between this check and that insert is harmless.
func (p *Packages) PackageExists(ctx context.Context, sourceRepoID int64, tag, digest string) (bool, error) {
	query := p.dialect.Rewrite(`
		SELECT 1 FROM packages
		 WHERE source_repo_id = ? AND tag = ? AND manifest_digest = ?
		 LIMIT 1`)

	var one int
	err := p.db.QueryRowContext(ctx, query, sourceRepoID, tag, digest).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("check package %s@%s: %w", tag, digest, err)
	}
	return true, nil
}

// ListPackages returns a product's packages, newest first.
type ListPackagesFilter struct {
	ProductName string
	// Repository narrows to one repository path. A product may span several.
	Repository string
	Tag        string
	State      string
	// IncludeAccessories lists the signature and wrapper rows a vendor
	// publishes as their own tags, which are hidden by default.
	//
	// Hidden because they are not releases: showing them triples the length of a
	// NEAR listing with rows nobody asked about, which is most of the noise the
	// vendor plugin exists to remove. Available at all because a row that exists
	// and cannot be seen is worse — somebody eventually has to find out where a
	// signature went.
	IncludeAccessories bool
	Limit              int
	Offset             int
}

// ListPackages backs the packages list API and CLI.
func (p *Packages) ListPackages(ctx context.Context, f ListPackagesFilter) ([]PackageRow, error) {
	query := `
		SELECT pk.id, pk.product_id, pk.source_repo_id, pk.tag, pk.manifest_digest,
		       -- total_bytes and blob_count are NOT coalesced: NULL is a real
		       -- value here, meaning "not yet measured", and folding it to 0
		       -- would put a wrong number in front of an operator. artifact_count
		       -- IS coalesced because it is always known once a package exists.
		       pk.media_type, pk.total_bytes, COALESCE(pk.artifact_count, 0),
		       pk.blob_count, pk.state, pk.discovered_at, pk.published_at, pk.superseded_by,
		       pk.signature_status, COALESCE(pk.transfer_root_digest,''), COALESCE(pk.transfer_root_tag,''),
		       COALESCE(pk.display_tag,''), pk.expanded_at, pk.accessory_of,
		       COALESCE(sr.repository_path, ''), COALESCE(sr.display_path, '')
		  FROM packages pk
		  JOIN products pr ON pr.id = pk.product_id
		  LEFT JOIN repositories sr ON sr.id = pk.source_repo_id
		 WHERE pr.name = ?`
	args := []any{f.ProductName}

	// BOTH SPELLINGS FILTER, for the same reason both spellings resolve in
	// GetPackageRef: a listing renders the shortened form, and a filter that
	// accepted only the long one would reject the value the user just copied off
	// their own screen. `--tag 23.8.1076` and `--tag orb_23.8.1076` are the same
	// request; so are `--repository cfx-5000-k8s` and `orbs/cfx-5000-k8s`.
	if f.Repository != "" {
		full, err := p.resolveRepositoryPath(ctx, f.ProductName, f.Repository)
		if err != nil {
			return nil, err
		}
		query += " AND sr.repository_path = ?"
		args = append(args, full)
	}
	if f.Tag != "" {
		query += " AND (pk.tag = ? OR pk.display_tag = ?)"
		args = append(args, f.Tag, f.Tag)
	}
	if f.State != "" {
		query += " AND pk.state = ?"
		args = append(args, f.State)
	}
	if !f.IncludeAccessories {
		query += " AND pk.accessory_of IS NULL"
	}

	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	// Newest RELEASE first, by the vendor's own declared build date — which is
	// the order a person thinks about releases in, and not the same as the
	// order we happened to notice them.
	//
	// The CASE is there because the two dialects DISAGREE about where NULLs go.
	// Measured, not assumed: with a plain `published_at DESC`, SQLite sorts
	// NULLs last and PostgreSQL sorts them FIRST. So on Postgres the packages
	// whose publisher set no date would head the list — the least informative
	// rows first — and nobody would notice until production, because the
	// development default is SQLite and it looks correct there.
	//
	// The CASE makes both dialects agree explicitly rather than relying on a
	// default neither of them documents as portable.
	//
	// Packages with no published date fall to the end and are then ordered by
	// when we found them, which is the best available answer for a publisher
	// that sets no annotations.
	query += `
		 ORDER BY CASE WHEN pk.published_at IS NULL THEN 1 ELSE 0 END,
		          pk.published_at DESC,
		          pk.discovered_at DESC,
		          pk.id DESC
		 LIMIT ? OFFSET ?`
	args = append(args, limit, max(f.Offset, 0))

	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(query), args...)
	if err != nil {
		return nil, fmt.Errorf("list packages for product %q: %w", f.ProductName, err)
	}
	defer func() { _ = rows.Close() }()

	var out []PackageRow
	for rows.Next() {
		var r PackageRow
		if err := rows.Scan(
			&r.ID, &r.ProductID, &r.SourceRepoID, &r.Tag, &r.ManifestDigest,
			&r.MediaType, &r.TotalBytes, &r.ArtifactCount, &r.BlobCount,
			&r.State, &r.DiscoveredAt, &r.PublishedAt, &r.SupersededBy,
			&r.SignatureStatus, &r.TransferRootDigest, &r.TransferRootTag, &r.DisplayTag,
			&r.ExpandedAt, &r.AccessoryOf, &r.SourceRepository, &r.DisplayRepository,
		); err != nil {
			return nil, fmt.Errorf("scan package row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AmbiguousReferenceError reports that a reference matched packages in more
// than one REPOSITORY.
//
// A product routinely spans dozens of repositories, and a vendor's version tag
// —`orb_23.8.1076` — appears in many of them. Picking one silently is the worst
// available behaviour: the caller gets a real package, believes it asked for
// that package, and is wrong. This makes the caller say which.
type AmbiguousReferenceError struct {
	Ref string
	// Repositories are the ones the reference matched, sorted.
	Repositories []string
}

func (e *AmbiguousReferenceError) Error() string {
	return fmt.Sprintf("%q matches packages in %d repositories (%s)",
		e.Ref, len(e.Repositories), strings.Join(e.Repositories, ", "))
}

// PackageRef is a parsed package reference.
type PackageRef struct {
	// Repository scopes the lookup. Empty means "any, but refuse if ambiguous".
	Repository string
	Tag        string
	Digest     string
}

// ParsePackageRef reads the reference forms a person actually types.
//
//	orb_23.8.1076                      a tag
//	sha256:ccbd37…                     a digest
//	orbs/cfx-5000-k8s:orb_23.8.1076    a repository and a tag
//
// The last form exists because a bare tag is ambiguous across a product's
// repositories, and making someone pass a separate flag to disambiguate is
// worse than letting them paste the thing they already have.
//
// A digest is recognised by its `algorithm:hex` shape BEFORE the repository
// split is attempted, so `sha256:…` is never mistaken for a repository called
// "sha256".
func ParsePackageRef(s string) PackageRef {
	s = strings.TrimSpace(s)

	if algo, hex, ok := strings.Cut(s, ":"); ok && isDigestAlgorithm(algo) && hex != "" {
		return PackageRef{Digest: s}
	}
	// Split at the LAST colon: a repository path may contain slashes but a tag
	// may not contain a colon.
	if i := strings.LastIndex(s, ":"); i > 0 && i < len(s)-1 {
		return PackageRef{Repository: strings.Trim(s[:i], "/"), Tag: s[i+1:]}
	}
	return PackageRef{Tag: s}
}

func isDigestAlgorithm(a string) bool {
	switch a {
	case "sha256", "sha512":
		return true
	default:
		return false
	}
}

// GetPackage returns one package by product and reference.
//
// A tag matching several rows in ONE repository is the re-push case, and the
// newest non-superseded row wins: asking for "v2.14.0" means the current one.
//
// A tag matching rows in SEVERAL repositories is a different situation
// entirely, and returns AmbiguousReferenceError rather than choosing. Scope it
// with a repository, either in the reference or in ref.Repository.
func (p *Packages) GetPackage(ctx context.Context, productName, refStr string) (PackageRow, error) {
	return p.GetPackageRef(ctx, productName, ParsePackageRef(refStr))
}

// GetPackageRef is GetPackage with an already-parsed reference.
func (p *Packages) GetPackageRef(ctx context.Context, productName string, ref PackageRef) (PackageRow, error) {
	rows, err := p.matchPackages(ctx, productName, ref)
	if err != nil {
		return PackageRow{}, err
	}
	if len(rows) == 0 {
		return PackageRow{}, ErrNotFound
	}

	repos := map[string]bool{}
	for _, r := range rows {
		repos[r.SourceRepository] = true
	}
	if len(repos) > 1 {
		names := make([]string, 0, len(repos))
		for r := range repos {
			names = append(names, r)
		}
		sort.Strings(names)
		return PackageRow{}, &AmbiguousReferenceError{Ref: ref.String(), Repositories: names}
	}

	// One repository: the ordering already put the current row first.
	return rows[0], nil
}

// String renders a reference the way a person would type it back.
func (r PackageRef) String() string {
	switch {
	case r.Digest != "":
		return r.Digest
	case r.Repository != "":
		return r.Repository + ":" + r.Tag
	default:
		return r.Tag
	}
}

// matchPackages returns every row a reference matches, current ones first.
func (p *Packages) matchPackages(ctx context.Context, productName string, ref PackageRef) ([]PackageRow, error) {
	query := `
		SELECT pk.id, pk.product_id, pk.source_repo_id, pk.tag, pk.manifest_digest,
		       -- total_bytes and blob_count are NOT coalesced: NULL is a real
		       -- value here, meaning "not yet measured", and folding it to 0
		       -- would put a wrong number in front of an operator. artifact_count
		       -- IS coalesced because it is always known once a package exists.
		       pk.media_type, pk.total_bytes, COALESCE(pk.artifact_count, 0),
		       pk.blob_count, pk.state, pk.discovered_at, pk.published_at, pk.superseded_by,
		       pk.signature_status, COALESCE(pk.transfer_root_digest,''), COALESCE(pk.transfer_root_tag,''),
		       COALESCE(pk.display_tag,''), pk.expanded_at, pk.accessory_of,
		       COALESCE(sr.repository_path, ''), COALESCE(sr.display_path, '')
		  FROM packages pk
		  JOIN products pr ON pr.id = pk.product_id
		  LEFT JOIN repositories sr ON sr.id = pk.source_repo_id
		 WHERE pr.name = ?`

	args := []any{productName}
	switch {
	case ref.Digest != "":
		query += " AND pk.manifest_digest = ?"
		args = append(args, ref.Digest)
	default:
		// EITHER SPELLING RESOLVES.
		//
		// A listing renders the display tag — `23.8.1076` for a vendor whose
		// real tag is `orb_23.8.1076` — and the shortened form has to be
		// typeable back, or the abbreviation is a trap: someone copies what
		// they see, gets "not found", and reasonably concludes the package is
		// gone.
		//
		// A stored column rather than a pattern match, because the transform is
		// the VENDOR's and this package must not know it. The Layout computed
		// it once at discovery; here it is just another equality.
		query += " AND (pk.tag = ? OR pk.display_tag = ?)"
		args = append(args, ref.Tag, ref.Tag)
	}
	if ref.Repository != "" {
		// Resolved first, so the shortened form a listing shows also works.
		full, err := p.resolveRepositoryPath(ctx, productName, ref.Repository)
		if err != nil {
			return nil, err
		}
		query += " AND sr.repository_path = ?"
		args = append(args, full)
	}
	query += `
		 ORDER BY CASE WHEN pk.state = 'superseded' THEN 1 ELSE 0 END,
		          pk.discovered_at DESC, pk.id DESC`

	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(query), args...)
	if err != nil {
		return nil, fmt.Errorf("match package %q: %w", ref, err)
	}
	defer func() { _ = rows.Close() }()

	var out []PackageRow
	for rows.Next() {
		var r PackageRow
		if err := rows.Scan(
			&r.ID, &r.ProductID, &r.SourceRepoID, &r.Tag, &r.ManifestDigest,
			&r.MediaType, &r.TotalBytes, &r.ArtifactCount, &r.BlobCount,
			&r.State, &r.DiscoveredAt, &r.PublishedAt, &r.SupersededBy,
			&r.SignatureStatus, &r.TransferRootDigest, &r.TransferRootTag, &r.DisplayTag,
			&r.ExpandedAt, &r.AccessoryOf, &r.SourceRepository, &r.DisplayRepository,
		); err != nil {
			return nil, fmt.Errorf("scan package row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ErrNotFound reports that no row matched.
var ErrNotFound = errors.New("not found")

// ListArtifacts returns a package's artifact tree, parents before children.
func (p *Packages) ListArtifacts(ctx context.Context, packageID int64) ([]ArtifactRow, error) {
	query := p.dialect.Rewrite(`
		SELECT id, package_id, parent_id, digest, media_type,
		       COALESCE(artifact_type, ''), size_bytes, COALESCE(platform, ''), depth,
		       fetched_at IS NOT NULL, raw IS NOT NULL, annotations
		  FROM package_artifacts
		 WHERE package_id = ?
		 ORDER BY depth, id`)

	rows, err := p.db.QueryContext(ctx, query, packageID)
	if err != nil {
		return nil, fmt.Errorf("list artifacts for package %d: %w", packageID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []ArtifactRow
	for rows.Next() {
		var a ArtifactRow
		var annotations []byte
		if err := rows.Scan(&a.ID, &a.PackageID, &a.ParentID, &a.Digest, &a.MediaType,
			&a.ArtifactType, &a.SizeBytes, &a.Platform, &a.Depth, &a.Fetched, &a.Cached,
			&annotations); err != nil {
			return nil, fmt.Errorf("scan artifact row: %w", err)
		}
		if len(annotations) > 0 {
			// A malformed map is dropped rather than failing the listing: it is
			// descriptive metadata, and an unreadable annotation must not make a
			// package impossible to look at.
			_ = json.Unmarshal(annotations, &a.Annotations)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListTransferRequests returns the requests raised for a package.
func (p *Packages) ListTransferRequests(ctx context.Context, packageID int64) ([]TransferRequestRow, error) {
	query := p.dialect.Rewrite(`
		SELECT id, product_id, package_id, operation, source_repo_id, priority,
		       idempotency_key, requested_by, request_origin, COALESCE(auto_rule_name, '')
		  FROM transfer_requests
		 WHERE package_id = ?
		 ORDER BY created_at, id`)

	rows, err := p.db.QueryContext(ctx, query, packageID)
	if err != nil {
		return nil, fmt.Errorf("list transfer requests for package %d: %w", packageID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []TransferRequestRow
	for rows.Next() {
		var r TransferRequestRow
		if err := rows.Scan(&r.ID, &r.ProductID, &r.PackageID, &r.Operation, &r.SourceRepoID,
			&r.Priority, &r.IdempotencyKey, &r.RequestedBy, &r.RequestOrigin, &r.AutoRuleName); err != nil {
			return nil, fmt.Errorf("scan transfer request row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// nullable renders an empty string as SQL NULL.
//
// The columns involved are nullable and semantically optional; storing "" would
// make "absent" and "empty" indistinguishable in every later query.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ExpandedArtifact is one manifest of a fully walked tree.
type ExpandedArtifact struct {
	Row ArtifactRow
	// Parent indexes into the tree slice; -1 for the root. Resolved to a row ID
	// as the tree is written, which is why parents must be written first.
	Parent int
	Blobs  []BlobRef
}

// ExpandedTree is a package's complete contents.
type ExpandedTree struct {
	Artifacts []ExpandedArtifact
	// TotalBytes and BlobCount are nil when still unmeasurable, which after a
	// full walk should not happen — but the type carries the possibility rather
	// than asserting it, because a walk that was truncated must not write a
	// confident number.
	TotalBytes *int64
	BlobCount  *int
}

// RecordExpandedTree writes a fully walked tree over whatever was recorded
// before, in one transaction.
//
// Discovery records a package's root manifest and lists its children without
// fetching them. This fills those in: the same rows gain their raw bytes, any
// deeper artifacts appear for the first time, blobs are linked, and the
// package's size stops being unknown.
//
// One transaction because a half-expanded package is the worst outcome
// available — it has a size that omits most of its bytes, and nothing marks it
// as partial. Either the whole tree is known or none of it is.
func (p *Packages) RecordExpandedTree(ctx context.Context, packageID int64, t ExpandedTree) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin expand transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	ids := make([]int64, len(t.Artifacts))

	for i, a := range t.Artifacts {
		row := a.Row
		row.PackageID = packageID
		if a.Parent >= 0 {
			row.ParentID = &ids[a.Parent]
		}

		id, err := p.upsertArtifact(ctx, tx, row)
		if err != nil {
			return err
		}
		ids[i] = id

		if err := p.LinkBlobs(ctx, tx, id, a.Blobs); err != nil {
			return err
		}
	}

	if err := p.setPackageMeasurement(ctx, tx, packageID, len(t.Artifacts), t.TotalBytes, t.BlobCount); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit expanded tree: %w", err)
	}
	return nil
}

// upsertArtifact inserts, or fills in an artifact discovery only listed.
//
// The ON CONFLICT branch is the point: discovery wrote these rows from an
// index's descriptors with no raw bytes, and this is where they gain them. The
// raw bytes are written with COALESCE so a re-run cannot blank a manifest we
// already hold, and fetched_at with COALESCE so a re-run cannot RESET the
// moment we learned this artifact's contents — which is what an evicted-then-
// refetched row would otherwise do to the record.
func (p *Packages) upsertArtifact(ctx context.Context, tx *sql.Tx, a ArtifactRow) (int64, error) {
	query := p.dialect.Rewrite(`
		INSERT INTO package_artifacts
			(package_id, parent_id, digest, media_type, artifact_type,
			 size_bytes, platform, depth, raw, annotations,
			 fetched_at, raw_bytes, raw_used_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ` + p.fetchStamps(a.Raw) + `)
		ON CONFLICT (package_id, digest) DO UPDATE SET
			raw           = COALESCE(EXCLUDED.raw, package_artifacts.raw),
			-- parent_id and depth are taken from the incoming tree rather than
			-- preserved, because a tree can legitimately be RE-ROOTED: discovery
			-- records the payload's tree, and expanding from the vendor's wrapper
			-- makes that same payload a child at depth 1. Keeping the old values
			-- would leave two artifacts claiming depth 0 and a parent link
			-- pointing at nothing.
			parent_id     = EXCLUDED.parent_id,
			depth         = EXCLUDED.depth,
			media_type    = EXCLUDED.media_type,
			artifact_type = COALESCE(EXCLUDED.artifact_type, package_artifacts.artifact_type),
			size_bytes    = EXCLUDED.size_bytes,
			platform      = COALESCE(EXCLUDED.platform, package_artifacts.platform),
			annotations   = COALESCE(EXCLUDED.annotations, package_artifacts.annotations),
			fetched_at    = COALESCE(package_artifacts.fetched_at, EXCLUDED.fetched_at),
			raw_bytes     = CASE WHEN EXCLUDED.raw IS NULL
			                     THEN package_artifacts.raw_bytes ELSE EXCLUDED.raw_bytes END,
			raw_used_at   = COALESCE(EXCLUDED.raw_used_at, package_artifacts.raw_used_at)
		RETURNING id`)

	var id int64
	err := tx.QueryRowContext(ctx, query,
		a.PackageID, a.ParentID, a.Digest, a.MediaType, nullable(a.ArtifactType),
		a.SizeBytes, nullable(a.Platform), a.Depth, rawOrNull(a.Raw), annotationsJSON(a.Annotations),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert artifact %s: %w", a.Digest, err)
	}
	return id, nil
}

// setPackageMeasurement records what the walk measured.
//
// expanded_at is set only when the measurement is a REAL one. A truncated walk
// leaves total_bytes nil, and stamping the package as expanded in that state
// would tell every later caller the tree is known when it is not — which is
// worse than the unmeasured row it replaced, because nobody questions a
// timestamp.
func (p *Packages) setPackageMeasurement(
	ctx context.Context, tx *sql.Tx, packageID int64, artifactCount int, totalBytes *int64, blobCount *int,
) error {
	expanded := "expanded_at"
	if totalBytes != nil {
		expanded = p.dialect.Now()
	}
	_, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE packages
		   SET artifact_count = ?, total_bytes = ?, blob_count = ?,
		       expanded_at = `+expanded+`,
		       updated_at = `+p.dialect.Now()+`
		 WHERE id = ?`),
		artifactCount, totalBytes, blobCount, packageID)
	if err != nil {
		return fmt.Errorf("record package measurement: %w", err)
	}
	return nil
}

// GetPackageByID returns one package row.
func (p *Packages) GetPackageByID(ctx context.Context, id int64) (PackageRow, error) {
	query := p.dialect.Rewrite(`
		SELECT pk.id, pk.product_id, pk.source_repo_id, pk.tag, pk.manifest_digest,
		       pk.media_type, pk.total_bytes, COALESCE(pk.artifact_count, 0),
		       pk.blob_count, pk.state, pk.discovered_at, pk.published_at, pk.superseded_by,
		       pk.signature_status, COALESCE(pk.transfer_root_digest,''), COALESCE(pk.transfer_root_tag,''),
		       COALESCE(pk.display_tag,''), pk.expanded_at, pk.accessory_of,
		       COALESCE(sr.repository_path, ''), COALESCE(sr.display_path, '')
		  FROM packages pk
		  LEFT JOIN repositories sr ON sr.id = pk.source_repo_id
		 WHERE pk.id = ?`)

	var r PackageRow
	err := p.db.QueryRowContext(ctx, query, id).Scan(
		&r.ID, &r.ProductID, &r.SourceRepoID, &r.Tag, &r.ManifestDigest,
		&r.MediaType, &r.TotalBytes, &r.ArtifactCount, &r.BlobCount,
		&r.State, &r.DiscoveredAt, &r.PublishedAt, &r.SupersededBy,
		&r.SignatureStatus, &r.TransferRootDigest, &r.TransferRootTag, &r.DisplayTag,
		&r.ExpandedAt, &r.AccessoryOf, &r.SourceRepository, &r.DisplayRepository)
	if errors.Is(err, sql.ErrNoRows) {
		return PackageRow{}, ErrNotFound
	}
	if err != nil {
		return PackageRow{}, fmt.Errorf("get package %d: %w", id, err)
	}
	return r, nil
}

// annotationsJSON encodes an annotation map for storage, or NULL when empty.
//
// NULL rather than "{}" so the COALESCE in upsertArtifact can tell "this write
// carries no annotations" from "this artifact genuinely has none", and a
// re-inspection cannot blank what discovery recorded.
func annotationsJSON(m map[string]string) any {
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// related artifacts
// ---------------------------------------------------------------------------

// RelationRow is one artifact that belongs to a package without living inside
// its manifest tree — a signature, an SBOM, an attestation, or the wrapper that
// bundles them.
//
// Role is vendor-neutral by construction: `signature`, never
// `nokia_signature`. Which mechanism found it stays in the plugin that produced
// the row, so a vendor switching from a wrapper index to the referrers API
// changes no stored data.
type RelationRow struct {
	Role      string
	Digest    string
	Tag       string
	MediaType string
	SizeBytes int64

	// The SIGNATURE MATERIAL: the blob a verifier actually reads.
	//
	// Digest above names the MANIFEST that carries the signature. These name
	// what is inside it — for NEAR, one layer of `application/pkcs7-signature`.
	// Empty until the package has been inspected, because the manifest has to be
	// fetched before its layers are known.
	BlobDigest    string
	BlobMediaType string
	BlobSize      int64
	// Annotations is the signature manifest's own annotation map, verbatim.
	// A verifier reads vendor keys from it — `com.nokia.ncd.orb.type` to know
	// what kind of signature this is, `com.nokia.rb.*` to tie it to a release —
	// without this package knowing any of them exist.
	Annotations map[string]string
	// ResolvedAt separates "inspected, and this signature carries no blob" from
	// "nobody has inspected this package yet". Empty blob digest means both, and
	// only one of them is worth acting on.
	ResolvedAt string
}

// ReplaceRelations writes a package's related artifacts.
//
// Insert-or-ignore rather than delete-then-insert: re-deriving the same
// relationship on a later scan must be a no-op, and deleting first would open a
// window where a package briefly appears to have no signature — which is
// exactly the fact a security decision reads.
func (p *Packages) ReplaceRelations(
	ctx context.Context, tx *sql.Tx, packageID int64, rows []RelationRow,
) error {
	if len(rows) == 0 {
		return nil
	}

	query := p.dialect.Rewrite(`
		INSERT INTO package_relations (package_id, role, digest, tag, media_type, size_bytes)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (package_id, role, digest) DO NOTHING`)

	for _, r := range rows {
		if _, err := tx.ExecContext(ctx, query,
			packageID, r.Role, r.Digest, nullIfEmpty(r.Tag), nullIfEmpty(r.MediaType), r.SizeBytes,
		); err != nil {
			return fmt.Errorf("insert %s relation %s: %w", r.Role, r.Digest, err)
		}
	}
	return nil
}

// RecordRelationMaterial writes what a relation's manifest turned out to
// contain — for a signature, the blob a verifier reads.
//
// Idempotent and safe to repeat: the tree under a digest cannot change, so a
// second inspection resolves the same material. `resolved_at` is refreshed each
// time, which is the honest reading — it says when we last confirmed it, not
// when we first guessed.
func (p *Packages) RecordRelationMaterial(
	ctx context.Context, packageID int64, role, digest string, m RelationRow,
) error {
	query := p.dialect.Rewrite(`
		UPDATE package_relations
		   SET blob_digest     = ?,
		       blob_media_type = ?,
		       blob_size       = ?,
		       annotations     = COALESCE(?, annotations),
		       resolved_at     = ` + p.dialect.Now() + `
		 WHERE package_id = ? AND role = ? AND digest = ?`)

	if _, err := p.db.ExecContext(ctx, query,
		nullIfEmpty(m.BlobDigest), nullIfEmpty(m.BlobMediaType), m.BlobSize,
		annotationsJSON(m.Annotations), packageID, role, digest); err != nil {
		return fmt.Errorf("record %s material for package %d: %w", role, packageID, err)
	}
	return nil
}

// ListRelations returns a package's related artifacts.
func (p *Packages) ListRelations(ctx context.Context, packageID int64) ([]RelationRow, error) {
	query := p.dialect.Rewrite(`
		SELECT role, digest, COALESCE(tag,''), COALESCE(media_type,''), size_bytes,
		       COALESCE(blob_digest,''), COALESCE(blob_media_type,''), blob_size,
		       annotations, resolved_at
		  FROM package_relations
		 WHERE package_id = ?
		 ORDER BY role, digest`)

	rows, err := p.db.QueryContext(ctx, query, packageID)
	if err != nil {
		return nil, fmt.Errorf("list relations for package %d: %w", packageID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []RelationRow
	for rows.Next() {
		var (
			r           RelationRow
			annotations []byte
			resolvedAt  *string
		)
		if err := rows.Scan(&r.Role, &r.Digest, &r.Tag, &r.MediaType, &r.SizeBytes,
			&r.BlobDigest, &r.BlobMediaType, &r.BlobSize, &annotations, &resolvedAt); err != nil {
			return nil, fmt.Errorf("scan relation: %w", err)
		}
		if len(annotations) > 0 {
			// Dropped rather than fatal if malformed: it is descriptive
			// metadata, and it must not make a package impossible to look at.
			_ = json.Unmarshal(annotations, &r.Annotations)
		}
		if resolvedAt != nil {
			r.ResolvedAt = *resolvedAt
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateSignatureState records what a later scan learned about an EXISTING
// package.
//
// The case this exists for: a vendor publishes a release, then signs it
// afterwards. The payload was recorded on an earlier scan, so the scan that
// finds the signature has no new package to attach it to — only an old one to
// correct. Without this the package would read `unsigned` forever, which is
// worse than `unknown` because it looks like an answer.
func (p *Packages) UpdateSignatureState(
	ctx context.Context, tx *sql.Tx, packageID int64, status, rootDigest, rootTag string,
) error {
	query := p.dialect.Rewrite(`
		UPDATE packages
		   SET signature_status     = ?,
		       transfer_root_digest = COALESCE(?, transfer_root_digest),
		       transfer_root_tag    = COALESCE(?, transfer_root_tag),
		       updated_at           = ` + p.dialect.Now() + `
		 WHERE id = ?`)

	if _, err := tx.ExecContext(ctx, query,
		status, nullIfEmpty(rootDigest), nullIfEmpty(rootTag), packageID); err != nil {
		return fmt.Errorf("update signature state for package %d: %w", packageID, err)
	}
	return nil
}

// GroupedLayout returns the layout name a repository's packages were last
// grouped under, or empty when they never were.
func (p *Packages) GroupedLayout(ctx context.Context, sourceRepoID int64) (string, error) {
	var name string
	err := p.db.QueryRowContext(ctx,
		p.dialect.Rewrite(`SELECT COALESCE(grouped_layout, '') FROM repositories WHERE id = ?`),
		sourceRepoID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read grouped layout for repository %d: %w", sourceRepoID, err)
	}
	return name, nil
}

// SetGroupedLayout records which convention a repository's packages are grouped
// under.
//
// Written only after a grouping pass SUCCEEDS. A failed pass must leave the old
// value, so the next scan retries rather than concluding the work was done.
func (p *Packages) SetGroupedLayout(ctx context.Context, sourceRepoID int64, layout string) error {
	_, err := p.db.ExecContext(ctx, p.dialect.Rewrite(
		`UPDATE repositories SET grouped_layout = ?, updated_at = `+p.dialect.Now()+` WHERE id = ?`),
		nullIfEmpty(layout), sourceRepoID)
	if err != nil {
		return fmt.Errorf("record grouped layout for repository %d: %w", sourceRepoID, err)
	}
	return nil
}

// MarkAccessory records that a package is part of another one — a signature or
// a wrapper that a vendor publishes as its own tag.
//
// The row survives with all its history; it simply stops being listed as a
// release in its own right. Passing 0 clears the mark, which is what removing a
// source's vendor does.
//
// A package can never be its own accessory: that would hide a real release
// behind a self-reference, and it is the shape a Layout bug would produce.
func (p *Packages) MarkAccessory(ctx context.Context, tx *sql.Tx, packageID, partOf int64) error {
	if packageID == partOf {
		return fmt.Errorf("package %d cannot be an accessory of itself", packageID)
	}
	var owner any
	if partOf != 0 {
		owner = partOf
	}
	_, err := tx.ExecContext(ctx, p.dialect.Rewrite(
		`UPDATE packages SET accessory_of = ? WHERE id = ?`), owner, packageID)
	if err != nil {
		return fmt.Errorf("mark package %d as an accessory of %d: %w", packageID, partOf, err)
	}
	return nil
}

// ClearAccessories un-marks every accessory in a repository.
//
// Run at the start of a re-grouping pass, so a repository whose vendor was
// REMOVED gets its packages back rather than keeping marks derived from a
// convention that no longer applies.
func (p *Packages) ClearAccessories(ctx context.Context, sourceRepoID int64) error {
	_, err := p.db.ExecContext(ctx, p.dialect.Rewrite(
		`UPDATE packages SET accessory_of = NULL
		  WHERE source_repo_id = ? AND accessory_of IS NOT NULL`), sourceRepoID)
	if err != nil {
		return fmt.Errorf("clear accessories in repository %d: %w", sourceRepoID, err)
	}
	return nil
}

// DeleteRelations removes a package's related artifacts.
//
// Paired with ReplaceRelations inside ONE transaction on the re-grouping path,
// where the relations are being deliberately re-derived under a different
// convention. Never on the discovery path: there the insert-or-ignore behaviour
// is what keeps a package from briefly appearing to have no signature, which is
// exactly the fact a security decision reads.
func (p *Packages) DeleteRelations(ctx context.Context, tx *sql.Tx, packageID int64) error {
	_, err := tx.ExecContext(ctx,
		p.dialect.Rewrite(`DELETE FROM package_relations WHERE package_id = ?`), packageID)
	if err != nil {
		return fmt.Errorf("delete relations of package %d: %w", packageID, err)
	}
	return nil
}

// PackageDisplayRow is a package's identity and its stored display name.
type PackageDisplayRow struct {
	ID         int64
	Tag        string
	DisplayTag string
}

// ListPackageDisplayNames returns every package in a repository with its stored
// display tag, so a scan can check them against what the vendor plugin now says.
//
// Every package, not just current ones: a superseded row is still listed and
// still has to render with the same name as its replacement.
func (p *Packages) ListPackageDisplayNames(ctx context.Context, sourceRepoID int64) ([]PackageDisplayRow, error) {
	query := p.dialect.Rewrite(`
		SELECT id, tag, COALESCE(display_tag, '')
		  FROM packages
		 WHERE source_repo_id = ?`)

	rows, err := p.db.QueryContext(ctx, query, sourceRepoID)
	if err != nil {
		return nil, fmt.Errorf("list display names for repository %d: %w", sourceRepoID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []PackageDisplayRow
	for rows.Next() {
		var r PackageDisplayRow
		if err := rows.Scan(&r.ID, &r.Tag, &r.DisplayTag); err != nil {
			return nil, fmt.Errorf("scan display name row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetDisplayTag rewrites one package's display tag.
//
// `updated_at` is deliberately NOT touched. This is a cosmetic correction to a
// row nothing about the package has changed in — the digest, the contents and
// the transfer history are all untouched — and bumping the timestamp would make
// a configuration edit look like a re-push in every audit and every diff.
func (p *Packages) SetDisplayTag(ctx context.Context, packageID int64, displayTag string) error {
	query := p.dialect.Rewrite(`UPDATE packages SET display_tag = ? WHERE id = ?`)
	if _, err := p.db.ExecContext(ctx, query, nullIfEmpty(displayTag), packageID); err != nil {
		return fmt.Errorf("set display tag for package %d: %w", packageID, err)
	}
	return nil
}

// rowQueryer is satisfied by both *sql.DB and *sql.Tx.
//
// It exists so a lookup can be run INSIDE a caller's transaction. On SQLite
// that is not a stylistic preference: a write transaction holds the database
// lock, and the same lookup issued on a pooled connection blocks until the
// transaction ends — which, if the transaction is waiting for the lookup, is
// never. That deadlock is easy to write and invisible on Postgres, which is
// where the development default not being the production database bites.
type rowQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// FindPackageByTag returns the current package for a (repository, tag), or
// ErrNotFound.
//
// Used when a later scan learns something about a package discovered earlier —
// see UpdateSignatureState.
func (p *Packages) FindPackageByTag(
	ctx context.Context, sourceRepoID int64, tag string,
) (int64, error) {
	return p.findPackageByTag(ctx, p.db, sourceRepoID, tag)
}

// FindPackageByTagTx is FindPackageByTag inside an open transaction. See
// rowQueryer for why the distinction is load-bearing rather than cosmetic.
func (p *Packages) FindPackageByTagTx(
	ctx context.Context, tx *sql.Tx, sourceRepoID int64, tag string,
) (int64, error) {
	return p.findPackageByTag(ctx, tx, sourceRepoID, tag)
}

func (p *Packages) findPackageByTag(
	ctx context.Context, q rowQueryer, sourceRepoID int64, tag string,
) (int64, error) {
	query := p.dialect.Rewrite(`
		SELECT id FROM packages
		 WHERE source_repo_id = ? AND tag = ? AND superseded_by IS NULL
		 ORDER BY id DESC
		 LIMIT 1`)

	var id int64
	err := q.QueryRowContext(ctx, query, sourceRepoID, tag).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("find package %s: %w", tag, err)
	}
	return id, nil
}

// nullIfEmpty writes SQL NULL for an empty string.
//
// The distinction is load-bearing for transfer_root_digest: NULL means "plan
// from the package's own manifest", and an empty string would be a digest that
// matches nothing.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// AmbiguousRepositoryError reports that a repository reference matched more
// than one path.
type AmbiguousRepositoryError struct {
	Ref   string
	Paths []string
}

func (e *AmbiguousRepositoryError) Error() string {
	return fmt.Sprintf("repository %q matches %d paths (%s)",
		e.Ref, len(e.Paths), strings.Join(e.Paths, ", "))
}

// resolveRepositoryPath turns a repository reference into a full path.
//
// A listing shortens `orbs/cfx-5000-db` to `cfx-5000-db` where the source's
// vendor plugin says the prefix is structural, so the short form has to resolve
// or the abbreviation is a trap — someone copies what they see and gets "not
// found" for a package that is plainly on the screen.
//
// Two things match. The stored `display_path`, which is the exact string a
// listing showed. And any WHOLE TRAILING SEGMENT, never a substring:
// `cfx-5000-db` matches `orbs/cfx-5000-db` and `a/b/cfx-5000-db`, and never
// `orbs/x-cfx-5000-db`. The second is deliberately kept even now that display
// paths are stored — it costs nothing, and someone typing the last segment of a
// path they can see is making a perfectly clear request whether or not a vendor
// plugin agrees. Two matches is a real ambiguity and is refused with both,
// exactly as an ambiguous tag is.
//
// Done in Go over the product's repository rows rather than as a LIKE, because
// a repository path may legally contain `_`, which LIKE would treat as a
// wildcard — quietly matching `cfx_db` against `cfx-db`.
func (p *Packages) resolveRepositoryPath(
	ctx context.Context, productName, ref string,
) (string, error) {
	ref = strings.Trim(strings.TrimSpace(ref), "/")
	if ref == "" {
		return "", nil
	}

	query := p.dialect.Rewrite(`
		SELECT DISTINCT r.repository_path, COALESCE(r.display_path, '')
		  FROM repositories r
		  JOIN products pr ON pr.id = r.product_id
		 WHERE pr.name = ? AND r.role = 'source'
		 ORDER BY r.repository_path`)

	rows, err := p.db.QueryContext(ctx, query, productName)
	if err != nil {
		return "", fmt.Errorf("resolve repository %q: %w", ref, err)
	}
	defer func() { _ = rows.Close() }()

	var matches []string
	for rows.Next() {
		var path, display string
		if err := rows.Scan(&path, &display); err != nil {
			return "", fmt.Errorf("scan repository path: %w", err)
		}
		if path == ref || display == ref || strings.HasSuffix(path, "/"+ref) {
			matches = append(matches, path)
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	switch len(matches) {
	case 0:
		// Unknown to us. Returned as given so the package lookup fails with
		// "not found" for the thing that was asked for, rather than for a path
		// this function invented.
		return ref, nil
	case 1:
		return matches[0], nil
	default:
		return "", &AmbiguousRepositoryError{Ref: ref, Paths: matches}
	}
}

// ReadExpandedTree returns a package's recorded tree, and whether it is
// COMPLETE.
//
// This is what makes the walk happen once. Discovery records a package's own
// manifest and lists what it references without fetching it; expanding fetches
// the rest. Whichever asks first — `packages describe --expand`, or a transfer
// planning its jobs — pays for the walk, and everyone after reads this.
//
// It is a CACHE, not a second source of truth. The tree under a digest is
// immutable, so a recorded tree can never be stale: content addressing is what
// makes reuse safe here and would not make it safe for anything mutable.
//
// complete is false when any artifact row was never FETCHED, which is exactly
// the state discovery leaves behind. Deliberately not "still holds its bytes":
// the manifest bodies are an evictable cache (migration 00007) and a package
// whose bytes were reclaimed is still fully known — its artifacts, blobs and
// sizes are all recorded. Tying completeness to the bytes would mean every
// eviction bought back a full registry walk, and the cache could never actually
// be reclaimed.
//
// A caller that needs the whole tree and finds it incomplete walks the registry
// and records the result; a caller that only needs sizes uses what is here.
func (p *Packages) ReadExpandedTree(
	ctx context.Context, packageID int64,
) (tree ExpandedTree, complete bool, err error) {
	query := p.dialect.Rewrite(`
		SELECT id, parent_id, digest, media_type, COALESCE(artifact_type,''),
		       size_bytes, COALESCE(platform,''), depth, raw, fetched_at IS NOT NULL,
		       annotations
		  FROM package_artifacts
		 WHERE package_id = ?
		 ORDER BY depth, id`)

	rows, err := p.db.QueryContext(ctx, query, packageID)
	if err != nil {
		return ExpandedTree{}, false, fmt.Errorf("read tree of package %d: %w", packageID, err)
	}
	defer func() { _ = rows.Close() }()

	// index maps an artifact's row ID to its position, so a parent row ID
	// becomes the slice index the tree uses. Parents come first because the
	// query orders by depth.
	index := map[int64]int{}
	complete = true

	for rows.Next() {
		var (
			id          int64
			parentID    *int64
			annotations []byte
			a           ExpandedArtifact
		)
		if err := rows.Scan(&id, &parentID, &a.Row.Digest, &a.Row.MediaType,
			&a.Row.ArtifactType, &a.Row.SizeBytes, &a.Row.Platform, &a.Row.Depth,
			&a.Row.Raw, &a.Row.Fetched, &annotations); err != nil {
			return ExpandedTree{}, false, fmt.Errorf("scan artifact: %w", err)
		}
		// Annotations are what the planner derives a destination from: an
		// artifact naming itself with org.opencontainers.image.ref.name says
		// which repository and tag it must land under. Dropping them here made
		// every artifact in a bundle look anonymous.
		if len(annotations) > 0 {
			_ = json.Unmarshal(annotations, &a.Row.Annotations)
		}
		a.Row.ID = id
		a.Row.Cached = len(a.Row.Raw) > 0
		if !a.Row.Fetched {
			// Listed by an index but never fetched. The tree is incomplete and
			// the caller must walk.
			complete = false
		}

		a.Parent = -1
		if parentID != nil {
			if pos, ok := index[*parentID]; ok {
				a.Parent = pos
			}
		}
		index[id] = len(tree.Artifacts)
		tree.Artifacts = append(tree.Artifacts, a)
	}
	if err := rows.Err(); err != nil {
		return ExpandedTree{}, false, err
	}
	if len(tree.Artifacts) == 0 {
		return ExpandedTree{}, false, nil
	}

	if err := p.attachBlobs(ctx, packageID, tree.Artifacts, index); err != nil {
		return ExpandedTree{}, false, err
	}

	// Recomputed rather than read from the package row, so this returns the
	// same numbers a fresh walk would and the two can be compared.
	total, count := measureExpanded(tree.Artifacts)
	tree.TotalBytes, tree.BlobCount = total, count
	return tree, complete, nil
}

// attachBlobs loads the config and layer descriptors each artifact references.
func (p *Packages) attachBlobs(
	ctx context.Context, packageID int64, artifacts []ExpandedArtifact, index map[int64]int,
) error {
	query := p.dialect.Rewrite(`
		SELECT ab.artifact_id, ab.digest, COALESCE(b.media_type,''), b.size_bytes,
		       ab.kind, ab.ordinal
		  FROM artifact_blobs ab
		  JOIN package_artifacts pa ON pa.id = ab.artifact_id
		  JOIN blobs b ON b.digest = ab.digest
		 WHERE pa.package_id = ?
		 ORDER BY ab.artifact_id, ab.kind, ab.ordinal`)

	rows, err := p.db.QueryContext(ctx, query, packageID)
	if err != nil {
		return fmt.Errorf("read blobs of package %d: %w", packageID, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			artifactID int64
			b          BlobRef
		)
		if err := rows.Scan(&artifactID, &b.Digest, &b.MediaType, &b.SizeBytes,
			&b.Kind, &b.Ordinal); err != nil {
			return fmt.Errorf("scan blob reference: %w", err)
		}
		pos, ok := index[artifactID]
		if !ok {
			continue
		}
		artifacts[pos].Blobs = append(artifacts[pos].Blobs, b)
	}
	return rows.Err()
}

// measureExpanded sums the transfer cost of a recorded tree.
//
// Each distinct digest counted ONCE: a fat index whose platforms share a base
// layer does not transfer that layer per platform, and summing naively would
// overstate the cost — sometimes several times over — making every size shown
// to an operator, and every estimate derived from one, wrong.
func measureExpanded(artifacts []ExpandedArtifact) (*int64, *int) {
	seen := map[string]bool{}
	var total int64

	for _, a := range artifacts {
		if !a.Row.Fetched {
			// An unfetched manifest's own blobs are unknown, so no honest total
			// exists. Nil rather than a number that is quietly too small.
			return nil, nil
		}
		if !seen[a.Row.Digest] {
			seen[a.Row.Digest] = true
			total += a.Row.SizeBytes
		}
		for _, b := range a.Blobs {
			if seen[b.Digest] {
				continue
			}
			seen[b.Digest] = true
			total += b.SizeBytes
		}
	}

	distinct := map[string]bool{}
	for _, a := range artifacts {
		for _, b := range a.Blobs {
			distinct[b.Digest] = true
		}
	}
	n := len(distinct)
	return &total, &n
}

// ---------------------------------------------------------------------------
// transfers and jobs
// ---------------------------------------------------------------------------

// JobRow is one unit of work: ONE blob to move, or ONE manifest to push.
//
// Never a package. That single choice produces most of the system's good
// properties — a thousand independent jobs distribute across the whole fleet, a
// network blip costs a retry of one blob rather than a restart of sixty
// gigabytes, workers are stateless because a job is self-contained, and
// deduplication is natural because the unit of work IS the unit of content
// addressing.
type JobRow struct {
	ID           int64
	TransferID   string
	Kind         string // blob | manifest
	Digest       string
	SizeBytes    int64
	MediaType    string
	ArtifactID   *int64
	SourceRepoID int64
	TargetRepoID int64
	State        string
	Wave         int
	Priority     int
	Attempts     int
	SkipReason   string

	// TargetTags are the tags this manifest must carry once committed.
	//
	// Resolved at PLANNING time from the artifact's own
	// org.opencontainers.image.ref.name, not derived at lease time from the
	// package row — which could only ever produce the one tag a person asked
	// for, and so lost every component's own name.
	TargetTags []string
	// TargetRepository is the destination path, denormalised for diagnostics.
	// repositories.repository_path via TargetRepoID is authoritative.
	TargetRepository string
}

// InsertJob creates a job, reporting whether it was new.
//
// ON CONFLICT DO NOTHING against (transfer_id, kind, digest, target_repo_id) is
// what makes PLANNING IDEMPOTENT: a Coordinator that dies mid-plan leaves a partial job
// set, and the replan on restart finds the existing rows free rather than
// duplicating them.
func (p *Packages) InsertJob(ctx context.Context, tx *sql.Tx, row JobRow) (bool, error) {
	state := row.State
	if state == "" {
		state = "pending"
	}

	query := p.dialect.Rewrite(`
		INSERT INTO jobs
			(transfer_id, kind, digest, size_bytes, media_type, artifact_id,
			 source_repo_id, target_repo_id, state, wave, priority,
			 target_tags, target_repository, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ` + p.dialect.Now() + `)
		ON CONFLICT (transfer_id, kind, digest, target_repo_id) DO NOTHING
		RETURNING id`)

	var id int64
	err := tx.QueryRowContext(ctx, query,
		row.TransferID, row.Kind, row.Digest, row.SizeBytes, nullIfEmpty(row.MediaType),
		row.ArtifactID, row.SourceRepoID, row.TargetRepoID, state, row.Wave, row.Priority,
		tagsJSON(row.TargetTags), nullIfEmpty(row.TargetRepository),
	).Scan(&id)

	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("insert %s job %s: %w", row.Kind, row.Digest, err)
	}
	return true, nil
}

// PlanTotals are what a planning run recorded about a transfer.
type PlanTotals struct {
	JobCount           int
	PlannedBytes       int64
	DedupeSkippedBytes int64
	MountableBytes     int64
	MaxWave            int
}

// RecordPlan writes a transfer's plan totals and opens it for work.
//
// The totals are what every progress figure and every estimate is derived from,
// so they are written in the SAME transaction as the jobs. A transfer whose
// totals disagreed with its jobs would report a percentage that never reaches
// a hundred.
//
// It also opens the transfer's first WORKABLE wave, which is not always wave 0.
// A plan whose blobs were entirely deduplicated creates no wave-0 jobs at all,
// and every manifest is created `blocked`. Leaving current_wave at 0 in that
// case is a deadlock: wave advancement is driven by job completion, and there
// is no wave-0 job left to complete and drive it. The transfer would sit in
// `ready` with a queue full of blocked manifests, forever.
//
// That is not a hypothetical — it is the SECOND transfer of any package, which
// is the case deduplication exists to make fast.
func (p *Packages) RecordPlan(
	ctx context.Context, tx *sql.Tx, transferID string, t PlanTotals,
) error {
	query := p.dialect.Rewrite(`
		UPDATE transfers
		   SET planned_job_count    = ?,
		       planned_bytes        = ?,
		       dedupe_skipped_bytes = ?,
		       mountable_bytes      = ?,
		       max_wave             = ?,
		       state                = 'ready',
		       updated_at           = ` + p.dialect.Now() + `
		 WHERE id = ?`)

	if _, err := tx.ExecContext(ctx, query,
		t.JobCount, t.PlannedBytes, t.DedupeSkippedBytes, t.MountableBytes,
		t.MaxWave, transferID); err != nil {
		return fmt.Errorf("record plan for transfer %s: %w", transferID, err)
	}
	return p.OpenFirstWave(ctx, tx, transferID)
}

// OpenFirstWave sets a transfer's current wave to the earliest one that still
// has work, and promotes that wave's jobs from `blocked` to `pending`.
//
// A transfer with no outstanding work at all goes straight to `succeeded`.
// That is the honest answer for a replan of something already transferred:
// there is nothing to do, and parking it in `ready` would mean an operator
// watching a transfer that will never move.
func (p *Packages) OpenFirstWave(ctx context.Context, tx *sql.Tx, transferID string) error {
	var first sql.NullInt64
	if err := tx.QueryRowContext(ctx, p.dialect.Rewrite(
		`SELECT MIN(wave) FROM jobs
		  WHERE transfer_id = ? AND state IN ('pending','blocked','leased')`),
		transferID).Scan(&first); err != nil {
		return fmt.Errorf("find the first workable wave of transfer %s: %w", transferID, err)
	}

	if !first.Valid {
		if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(
			`UPDATE transfers
			    SET state = 'succeeded', current_wave = max_wave,
			        completed_at = `+p.dialect.Now()+`, updated_at = `+p.dialect.Now()+`
			  WHERE id = ?`), transferID); err != nil {
			return fmt.Errorf("finish empty transfer %s: %w", transferID, err)
		}
		return nil
	}

	if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(
		`UPDATE transfers SET current_wave = ?, updated_at = `+p.dialect.Now()+
			` WHERE id = ?`), first.Int64, transferID); err != nil {
		return fmt.Errorf("open wave %d of transfer %s: %w", first.Int64, transferID, err)
	}

	if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(
		`UPDATE jobs SET state = 'pending', updated_at = `+p.dialect.Now()+`
		  WHERE transfer_id = ? AND wave = ? AND state = 'blocked'`),
		transferID, first.Int64); err != nil {
		return fmt.Errorf("unblock wave %d of transfer %s: %w", first.Int64, transferID, err)
	}
	return nil
}

// PlacedDigests reports which of these digests are already in a repository.
//
// A DATABASE lookup, not a network call — that is what makes planning a
// thousand-blob package fast. Registry HEADs are deferred to the worker, where
// they run in parallel and where a stale record is caught anyway.
//
// A placement is STRONG EVIDENCE, not proof: a registry's garbage collector can
// remove content underneath us. Two defences make the optimism safe — entries
// past their TTL are not trusted, and a manifest push failing with BLOB_UNKNOWN
// invalidates the placements for that manifest's blobs and requeues them. The
// registry itself tells us when the cache is wrong.
func (p *Packages) PlacedDigests(
	ctx context.Context, repositoryID int64, digests []string,
) (map[string]bool, error) {
	return p.placedDigests(ctx, p.db, repositoryID, digests)
}

// PlacedDigestsTx is the same lookup inside a caller's transaction.
//
// Not a convenience. SQLite is opened with SetMaxOpenConns(1), so a read
// issued on the pool while the caller holds a write transaction waits for a
// connection that transaction will not release until it commits — a deadlock
// that presents as a hung planner rather than an error. The planner reads
// placements between writing destination rows and writing jobs, so it must
// read on its own transaction.
//
// It is also more correct: a plan should see one consistent snapshot, not the
// placements as they were between two of its own writes.
func (p *Packages) PlacedDigestsTx(
	ctx context.Context, tx *sql.Tx, repositoryID int64, digests []string,
) (map[string]bool, error) {
	return p.placedDigests(ctx, tx, repositoryID, digests)
}

// rowQuerier is the read surface *sql.DB and *sql.Tx have in common.
type rowQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func (p *Packages) placedDigests(
	ctx context.Context, q rowQuerier, repositoryID int64, digests []string,
) (map[string]bool, error) {
	out := map[string]bool{}
	if len(digests) == 0 {
		return out, nil
	}

	// Chunked, because a package with thousands of blobs would otherwise build
	// a statement with thousands of placeholders — which SQLite refuses outright
	// and Postgres merely plans badly.
	const chunk = 500
	for start := 0; start < len(digests); start += chunk {
		end := min(start+chunk, len(digests))
		batch := digests[start:end]

		placeholders := make([]byte, 0, len(batch)*2)
		args := make([]any, 0, len(batch)+2)
		args = append(args, repositoryID)
		for i, d := range batch {
			if i > 0 {
				placeholders = append(placeholders, ',')
			}
			placeholders = append(placeholders, '?')
			args = append(args, d)
		}
		args = append(args, placementTTLSeconds)

		query := p.dialect.Rewrite(`
			SELECT digest FROM blob_placements
			 WHERE repository_id = ?
			   AND digest IN (` + string(placeholders) + `)
			   AND verified_at > ` + p.dialect.TimeAgo("?"))

		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("read placements for repository %d: %w", repositoryID, err)
		}
		for rows.Next() {
			var d string
			if err := rows.Scan(&d); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan placement: %w", err)
			}
			out[d] = true
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}
	return out, nil
}

// placementTTLSeconds is how long a placement record is trusted.
//
// Twenty-four hours. Long enough that a repeated transfer of a product line is
// nearly free, short enough that a registry garbage collector cannot leave us
// confidently wrong for long. The BLOB_UNKNOWN backstop is what makes any value
// here safe rather than merely optimistic.
const placementTTLSeconds = 24 * 60 * 60

// RecordPlacement notes that a digest is present in a repository.
//
// source distinguishes how we know: `transferred` (we put it there),
// `mounted` (the registry relocated it), `observed` (a HEAD found it). The
// distinction is worth keeping — an observed placement is the weakest evidence
// and the first thing to doubt when something is missing.
func (p *Packages) RecordPlacement(
	ctx context.Context, tx *sql.Tx, repositoryID int64, digest string, size int64, source string,
) error {
	if _, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
		INSERT INTO blobs (digest, size_bytes) VALUES (?, ?)
		ON CONFLICT (digest) DO NOTHING`), digest, size); err != nil {
		return fmt.Errorf("record blob %s: %w", digest, err)
	}

	query := p.dialect.Rewrite(`
		INSERT INTO blob_placements (repository_id, digest, size_bytes, source, verified_at)
		VALUES (?, ?, ?, ?, ` + p.dialect.Now() + `)
		ON CONFLICT (repository_id, digest)
		DO UPDATE SET verified_at = ` + p.dialect.Now() + `, source = EXCLUDED.source`)

	if _, err := tx.ExecContext(ctx, query, repositoryID, digest, size, source); err != nil {
		return fmt.Errorf("record placement %s: %w", digest, err)
	}
	return nil
}

// CreateTransfer opens a transfer for one request and one target.
//
// UNIQUE (request_id, target_repo_id) makes this idempotent: a request expanded
// twice produces one transfer per target, not two.
func (p *Packages) CreateTransfer(ctx context.Context, tx *sql.Tx, row TransferRow) (bool, error) {
	query := p.dialect.Rewrite(`
		INSERT INTO transfers
			(id, request_id, package_id, source_repo_id, target_repo_id, state, priority, updated_at)
		VALUES (?, ?, ?, ?, ?, 'planning', ?, ` + p.dialect.Now() + `)
		ON CONFLICT (request_id, target_repo_id) DO NOTHING
		RETURNING id`)

	var id string
	err := tx.QueryRowContext(ctx, query, row.ID, row.RequestID, row.PackageID,
		row.SourceRepoID, row.TargetRepoID, row.Priority).Scan(&id)

	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("create transfer %s: %w", row.ID, err)
	}
	return true, nil
}

// TransferRow is one package moving to one target.
type TransferRow struct {
	ID           string
	RequestID    string
	PackageID    int64
	SourceRepoID int64
	TargetRepoID int64
	Priority     int
	State        string
}
