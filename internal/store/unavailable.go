package store

import (
	"context"
	"fmt"
)

// What the vendor will not serve us, kept as a fact rather than an error.
//
// See db/migrations/*/00015_unavailable_packages.sql for why this is a table.
// In one sentence: a 403 from a registry fronting an entitlement system is the
// entitlement check working, it recurs on every scan forever, and it is
// therefore both wrong to report as a failure and wrong to discard.

// ReasonNotEntitled is a source refusing content this customer has not bought.
const ReasonNotEntitled = "not_entitled"

// UnavailablePackage is one artifact a source would not serve.
type UnavailablePackage struct {
	Repository string
	Tag        string
	// DisplayRepository and DisplayTag are the vendor's own names for the two
	// above. Stored rather than derived so a listing still reads correctly for
	// a source whose `vendor` has since been changed or removed.
	DisplayRepository string
	DisplayTag        string
	Reason            string
	Detail            string
	FirstSeenAt       string
	LastSeenAt        string
}

// RecordUnavailable upserts what a scan could not read.
//
// last_seen_at moves on every scan and first_seen_at does not, which is the
// pair that makes a row falsifiable: entitlement changes when somebody buys
// something, so a row that stops being refreshed is one that came back.
func (p *Packages) RecordUnavailable(
	ctx context.Context, productID int64, rows []UnavailablePackage,
) error {
	if len(rows) == 0 {
		return nil
	}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin unavailable record: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt := p.dialect.Rewrite(`
		INSERT INTO unavailable_packages
		       (product_id, repository_path, tag, display_repository, display_tag,
		        reason, detail, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ` + p.dialect.Now() + `, ` + p.dialect.Now() + `)
		ON CONFLICT (product_id, repository_path, tag) DO UPDATE SET
		    display_repository = EXCLUDED.display_repository,
		    display_tag        = EXCLUDED.display_tag,
		    reason             = EXCLUDED.reason,
		    detail             = EXCLUDED.detail,
		    last_seen_at       = EXCLUDED.last_seen_at`)

	for _, r := range rows {
		if _, err := tx.ExecContext(ctx, stmt, productID, r.Repository, r.Tag,
			r.DisplayRepository, r.DisplayTag, r.Reason, r.Detail); err != nil {
			return fmt.Errorf("record unavailable %s:%s: %w", r.Repository, r.Tag, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit unavailable record: %w", err)
	}
	return nil
}

// ClearUnavailable forgets rows a scan has since read successfully.
//
// Called with everything the scan DID read, so an orb that becomes entitled
// disappears from the listing on the next pass rather than lingering as a
// permanent accusation. Deleting is right rather than marking resolved: the
// question the table answers is "what can we not get right now", and history of
// what we once could not get is what the audit log is for.
func (p *Packages) ClearUnavailable(
	ctx context.Context, productID int64, repository string, tags []string,
) error {
	if len(tags) == 0 {
		return nil
	}

	stmt := p.dialect.Rewrite(
		`DELETE FROM unavailable_packages
		  WHERE product_id = ? AND repository_path = ? AND tag = ?`)
	for _, tag := range tags {
		if _, err := p.db.ExecContext(ctx, stmt, productID, repository, tag); err != nil {
			return fmt.Errorf("clear unavailable %s:%s: %w", repository, tag, err)
		}
	}
	return nil
}

// ListUnavailable returns what a product's sources would not serve, most
// recently confirmed first.
func (p *Packages) ListUnavailable(
	ctx context.Context, productName string, limit int,
) ([]UnavailablePackage, error) {
	if limit <= 0 {
		limit = 200
	}

	query := p.dialect.Rewrite(`
		SELECT u.repository_path, u.tag, u.display_repository, u.display_tag,
		       u.reason, u.detail, u.first_seen_at, u.last_seen_at
		  FROM unavailable_packages u
		  JOIN products pr ON pr.id = u.product_id
		 WHERE (? = '' OR pr.name = ?)
		 ORDER BY u.last_seen_at DESC, u.repository_path, u.tag
		 LIMIT ?`)

	rows, err := p.db.QueryContext(ctx, query, productName, productName, limit)
	if err != nil {
		return nil, fmt.Errorf("list unavailable packages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []UnavailablePackage
	for rows.Next() {
		var u UnavailablePackage
		if err := rows.Scan(&u.Repository, &u.Tag, &u.DisplayRepository, &u.DisplayTag,
			&u.Reason, &u.Detail, &u.FirstSeenAt, &u.LastSeenAt); err != nil {
			return nil, fmt.Errorf("scan unavailable package: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
