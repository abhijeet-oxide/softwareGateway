package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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
	// SourceRepository is the repository path this was discovered in. Joined
	// on read rather than denormalised: a product may span several
	// repositories, and a listing that does not say which is ambiguous.
	SourceRepository string
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
	// Raw is the manifest exactly as served. NIL means this artifact was LISTED
	// by its parent index and not fetched, so we have the vendor's word for its
	// digest rather than bytes we hashed ourselves.
	Raw []byte
	// Fetched is `raw IS NOT NULL`, read back without the bytes themselves — a
	// listing must not load every manifest body to report which ones we hold.
	Fetched bool
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

	query := p.dialect.Rewrite(`
		INSERT INTO packages
			(product_id, source_repo_id, tag, manifest_digest, media_type,
			 total_bytes, artifact_count, blob_count, published_at, state, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ` + p.dialect.Now() + `)
		ON CONFLICT (source_repo_id, tag, manifest_digest) DO NOTHING
		RETURNING id`)

	var id int64
	err := tx.QueryRowContext(ctx, query,
		row.ProductID, row.SourceRepoID, row.Tag, row.ManifestDigest, row.MediaType,
		row.TotalBytes, row.ArtifactCount, row.BlobCount, row.PublishedAt, state,
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
	query := p.dialect.Rewrite(`
		INSERT INTO package_artifacts
			(package_id, parent_id, digest, media_type, artifact_type,
			 size_bytes, platform, depth, raw, annotations)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (package_id, digest) DO NOTHING
		RETURNING id`)

	var id int64
	err := tx.QueryRowContext(ctx, query,
		a.PackageID, a.ParentID, a.Digest, a.MediaType, nullable(a.ArtifactType),
		a.SizeBytes, nullable(a.Platform), a.Depth, a.Raw, annotationsJSON(a.Annotations),
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
func (p *Packages) EnsureRepository(
	ctx context.Context, tx *sql.Tx,
	productID int64, role, name, registryHost, repositoryPath, registryType, managedBy string,
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
			 managed_by, active, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ` + p.dialect.Bool(true) + `, ` + p.dialect.Now() + `)
		ON CONFLICT (registry_host, repository_path) DO UPDATE SET
			active     = ` + p.dialect.Bool(true) + `,
			updated_at = ` + p.dialect.Now() + `
		RETURNING id`)

	var id int64
	err := tx.QueryRowContext(ctx, upsert,
		productID, role, name, registryHost, repositoryPath, registryType, managedBy,
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
	Limit      int
	Offset     int
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
		       COALESCE(sr.repository_path, '')
		  FROM packages pk
		  JOIN products pr ON pr.id = pk.product_id
		  LEFT JOIN repositories sr ON sr.id = pk.source_repo_id
		 WHERE pr.name = ?`
	args := []any{f.ProductName}

	if f.Repository != "" {
		query += " AND sr.repository_path = ?"
		args = append(args, f.Repository)
	}
	if f.Tag != "" {
		query += " AND pk.tag = ?"
		args = append(args, f.Tag)
	}
	if f.State != "" {
		query += " AND pk.state = ?"
		args = append(args, f.State)
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
			&r.State, &r.DiscoveredAt, &r.PublishedAt, &r.SupersededBy, &r.SourceRepository,
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
		       COALESCE(sr.repository_path, '')
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
		query += " AND pk.tag = ?"
		args = append(args, ref.Tag)
	}
	if ref.Repository != "" {
		query += " AND sr.repository_path = ?"
		args = append(args, ref.Repository)
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
			&r.State, &r.DiscoveredAt, &r.PublishedAt, &r.SupersededBy, &r.SourceRepository,
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
		       raw IS NOT NULL, annotations
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
			&a.ArtifactType, &a.SizeBytes, &a.Platform, &a.Depth, &a.Fetched,
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
// already hold.
func (p *Packages) upsertArtifact(ctx context.Context, tx *sql.Tx, a ArtifactRow) (int64, error) {
	query := p.dialect.Rewrite(`
		INSERT INTO package_artifacts
			(package_id, parent_id, digest, media_type, artifact_type,
			 size_bytes, platform, depth, raw, annotations)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (package_id, digest) DO UPDATE SET
			raw           = COALESCE(EXCLUDED.raw, package_artifacts.raw),
			media_type    = EXCLUDED.media_type,
			artifact_type = COALESCE(EXCLUDED.artifact_type, package_artifacts.artifact_type),
			size_bytes    = EXCLUDED.size_bytes,
			platform      = COALESCE(EXCLUDED.platform, package_artifacts.platform),
			annotations   = COALESCE(EXCLUDED.annotations, package_artifacts.annotations)
		RETURNING id`)

	var id int64
	err := tx.QueryRowContext(ctx, query,
		a.PackageID, a.ParentID, a.Digest, a.MediaType, nullable(a.ArtifactType),
		a.SizeBytes, nullable(a.Platform), a.Depth, a.Raw, annotationsJSON(a.Annotations),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert artifact %s: %w", a.Digest, err)
	}
	return id, nil
}

// setPackageMeasurement records what the walk measured.
func (p *Packages) setPackageMeasurement(
	ctx context.Context, tx *sql.Tx, packageID int64, artifactCount int, totalBytes *int64, blobCount *int,
) error {
	_, err := tx.ExecContext(ctx, p.dialect.Rewrite(`
		UPDATE packages
		   SET artifact_count = ?, total_bytes = ?, blob_count = ?,
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
		       COALESCE(sr.repository_path, '')
		  FROM packages pk
		  LEFT JOIN repositories sr ON sr.id = pk.source_repo_id
		 WHERE pk.id = ?`)

	var r PackageRow
	err := p.db.QueryRowContext(ctx, query, id).Scan(
		&r.ID, &r.ProductID, &r.SourceRepoID, &r.Tag, &r.ManifestDigest,
		&r.MediaType, &r.TotalBytes, &r.ArtifactCount, &r.BlobCount,
		&r.State, &r.DiscoveredAt, &r.PublishedAt, &r.SupersededBy, &r.SourceRepository)
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
