package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// Package catalog keeps the database catalog in step with loaded
// configuration.
//
// It sits in the domain layer, above persistence: it imports both `product`
// and `store`, which `store` itself may not do. Persistence takes and returns
// rows; deciding what a product means belongs here. See docs/design/15 §3 -
// the rule is enforced by depguard, and this package exists because of it.

// Catalog reconciles loaded configuration into the products and repositories
// tables.
//
// This exists because configuration lives in Git and memory, but `packages`
// carries foreign keys to `products` and `repositories` - so discovery cannot
// record anything until those rows exist. Reconciliation runs at startup and
// after every configuration reload.
type Catalog struct {
	store   store.Store
	dialect store.Dialect
}

// NewCatalog builds a reconciler.
func NewCatalog(s store.Store) *Catalog {
	return &Catalog{store: s, dialect: store.DialectFor(s.Driver())}
}

// ProductRef is the resolved database identity of a configured product.
type ProductRef struct {
	ID   int64
	Name string
	// Repositories maps the configured repository name to its row ID, for both
	// roles. Discovery uses it to resolve a source name to source_repo_id.
	Repositories map[string]int64
}

// ReconcileResult reports what a reconciliation did.
type ReconcileResult struct {
	Products     map[string]ProductRef
	Deactivated  int
	ProductsSeen int
	ReposSeen    int
	// Disabled counts rows switched off by `enabled: false` rather than by
	// having been removed from configuration. Worth separating in the log: one
	// is a deliberate pause, the other is a deletion.
	Disabled int
}

// Reconcile makes the catalog tables match the loaded configuration.
//
// Rows are never deleted. A product or repository removed from configuration
// is marked inactive, because discovery history, transfers and audit records
// reference these rows by ID - deleting one would orphan the very history the
// audit trail exists to preserve.
func (c *Catalog) Reconcile(ctx context.Context, products []*product.Product) (ReconcileResult, error) {
	res := ReconcileResult{Products: make(map[string]ProductRef, len(products))}

	tx, err := c.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("begin catalog reconcile: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	activeProducts := make(map[int64]bool)
	activeRepos := make(map[int64]bool)

	for _, p := range products {
		productID, err := c.upsertProduct(ctx, tx, p)
		if err != nil {
			return res, err
		}
		res.ProductsSeen++

		// A DISABLED product keeps its row - its packages and transfer history
		// reference it - but is left out of the active set, so deactivation
		// below switches it off. Deleting the row instead would orphan exactly
		// the history somebody disabled the product to preserve.
		if !p.IsEnabled() {
			res.Disabled++
			continue
		}
		activeProducts[productID] = true

		ref := ProductRef{ID: productID, Name: p.Metadata.Name, Repositories: map[string]int64{}}

		for _, r := range p.Repositories() {
			repoID, err := c.upsertRepository(ctx, tx, productID, p.Metadata.Name, r)
			if err != nil {
				return res, err
			}
			ref.Repositories[r.Name] = repoID
			res.ReposSeen++

			if !r.Enabled {
				res.Disabled++
				continue
			}
			activeRepos[repoID] = true
		}

		res.Products[p.Metadata.Name] = ref
	}

	deactivated, err := c.deactivateMissing(ctx, tx, activeProducts, activeRepos)
	if err != nil {
		return res, err
	}
	res.Deactivated = deactivated

	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("commit catalog reconcile: %w", err)
	}
	return res, nil
}

// upsertProduct inserts or updates the product row and returns its ID.
//
// The unique key is (name, config_hash), so an unchanged configuration reuses
// its row and an edited one creates a new version - which is what lets an
// audit record from March still resolve to the configuration in force then.
func (c *Catalog) upsertProduct(ctx context.Context, tx *sql.Tx, p *product.Product) (int64, error) {
	cfg, err := json.Marshal(p)
	if err != nil {
		return 0, fmt.Errorf("marshal product %q: %w", p.Metadata.Name, err)
	}

	// Older rows for the same product are deactivated first, so the partial
	// unique index on (name) WHERE active holds at most one row per product.
	deactivate := c.dialect.Rewrite(`
		UPDATE products SET active = ` + c.dialect.Bool(false) + `, updated_at = ` + c.dialect.Now() + `
		 WHERE name = ? AND config_hash <> ?`)
	if _, err := tx.ExecContext(ctx, deactivate, p.Metadata.Name, p.ConfigHash); err != nil {
		return 0, fmt.Errorf("deactivate prior versions of product %q: %w", p.Metadata.Name, err)
	}

	upsert := c.dialect.Rewrite(`
		INSERT INTO products (name, display_name, owner, config_hash, config, active, updated_at)
		VALUES (?, ?, ?, ?, ?, ` + c.dialect.Bool(true) + `, ` + c.dialect.Now() + `)
		ON CONFLICT (name, config_hash) DO UPDATE SET
			display_name = excluded.display_name,
			owner        = excluded.owner,
			config       = excluded.config,
			active       = ` + c.dialect.Bool(true) + `,
			updated_at   = ` + c.dialect.Now() + `
		RETURNING id`)

	var id int64
	err = tx.QueryRowContext(ctx, upsert,
		p.Metadata.Name, p.Metadata.DisplayName, p.Metadata.Owner, p.ConfigHash, string(cfg),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert product %q: %w", p.Metadata.Name, err)
	}
	return id, nil
}

// upsertRepository inserts or updates a repository row and returns its ID.
//
// Keyed by PHYSICAL identity (registry_host, repository_path), matching the
// unique index. Combined with product_id NOT NULL, that means one repository
// belongs to exactly one product - enforced at configuration load, within a
// product and across the directory (internal/product, docs/design/03 section 4).
//
// The ownership check below is defence in depth: if a clash ever reached this
// point it would otherwise flip product_id on every reload, which is a silent
// and genuinely baffling failure. Better to refuse and name both products.
func (c *Catalog) upsertRepository(
	ctx context.Context, tx *sql.Tx, productID int64, productName string, r product.RepositoryRef,
) (int64, error) {
	// Ownership is compared by product NAME, not by row ID. Editing a product
	// mints a new products row (identity is name + config_hash), so an ID
	// comparison would flag every legitimate config edit as an ownership
	// conflict. The name is the stable identity across config versions.
	var ownerName string
	err := tx.QueryRowContext(ctx, c.dialect.Rewrite(`
		SELECT p.name FROM repositories r
		  JOIN products p ON p.id = r.product_id
		 WHERE r.registry_host = ? AND r.repository_path = ?`),
		r.Registry, r.Repository,
	).Scan(&ownerName)
	switch {
	case err == nil && ownerName != productName:
		return 0, fmt.Errorf(
			"repository %s/%s is already owned by product %q: a physical repository "+
				"belongs to exactly one product",
			r.Registry, r.Repository, ownerName)
	case err != nil && err != sql.ErrNoRows:
		return 0, fmt.Errorf("check ownership of %s/%s: %w", r.Registry, r.Repository, err)
	}

	// managed_by is set to 'config' on both insert and update: a repository a
	// human has now declared is a declaration, even if discovery found it first.
	// Reconciliation then owns its lifecycle, which is what the operator means
	// by writing it down.
	upsert := c.dialect.Rewrite(`
		INSERT INTO repositories
			(product_id, role, name, registry_host, repository_path, registry_type,
			 managed_by, active, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 'config', ` + c.dialect.Bool(true) + `, ` + c.dialect.Now() + `)
		ON CONFLICT (registry_host, repository_path) DO UPDATE SET
			product_id    = excluded.product_id,
			role          = excluded.role,
			name          = excluded.name,
			registry_type = excluded.registry_type,
			managed_by    = 'config',
			active        = ` + c.dialect.Bool(true) + `,
			updated_at    = ` + c.dialect.Now() + `
		RETURNING id`)

	var id int64
	if err := tx.QueryRowContext(ctx, upsert,
		productID, string(r.Role), r.Name, r.Registry, r.Repository, string(r.Type),
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("upsert repository %q (%s/%s): %w", r.Name, r.Registry, r.Repository, err)
	}
	return id, nil
}

// deactivateMissing marks rows absent from the current configuration inactive.
func (c *Catalog) deactivateMissing(
	ctx context.Context, tx *sql.Tx, activeProducts, activeRepos map[int64]bool,
) (int, error) {
	total := 0

	n, err := deactivateExcept(ctx, tx, c.dialect, "repositories", activeRepos)
	if err != nil {
		return 0, err
	}
	total += n

	n, err = deactivateExcept(ctx, tx, c.dialect, "products", activeProducts)
	if err != nil {
		return 0, err
	}
	return total + n, nil
}

// deactivateExcept marks every active row not in keep as inactive.
//
// The table name is a compile-time constant from the caller, never user input;
// the IDs go through placeholders. Building the IN list this way avoids a
// dialect-specific array type for what is a handful of integers.
func deactivateExcept(
	ctx context.Context, tx *sql.Tx, d store.Dialect, table string, keep map[int64]bool,
) (int, error) {
	query := "UPDATE " + table + " SET active = " + d.Bool(false) +
		", updated_at = " + d.Now() + " WHERE active = " + d.Bool(true)

	// Repositories discovery found in the registry catalog are not in
	// configuration and must survive reconciliation. Without this clause every
	// reload would deactivate them and the next scan would revive them, flapping
	// the rows and churning the audit trail for no reason. Their lifecycle
	// belongs to discovery, which deactivates them when they leave the catalog.
	if table == "repositories" {
		query += " AND managed_by = 'config'"
	}

	args := make([]any, 0, len(keep))
	if len(keep) > 0 {
		placeholders := make([]byte, 0, len(keep)*2)
		for id := range keep {
			if len(placeholders) > 0 {
				placeholders = append(placeholders, ',')
			}
			placeholders = append(placeholders, '?')
			args = append(args, id)
		}
		query += " AND id NOT IN (" + string(placeholders) + ")"
	}

	res, err := tx.ExecContext(ctx, d.Rewrite(query), args...)
	if err != nil {
		return 0, fmt.Errorf("deactivate stale %s: %w", table, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil // not all drivers report this; not worth failing over
	}
	return int(n), nil
}

// ResolveRepository returns the row ID for a product's named repository.
func (c *Catalog) ResolveRepository(ctx context.Context, productName, repoName string) (int64, error) {
	query := c.dialect.Rewrite(`
		SELECT r.id FROM repositories r
		  JOIN products p ON p.id = r.product_id
		 WHERE p.name = ? AND r.name = ? AND r.active = ` + c.dialect.Bool(true) + `
		 LIMIT 1`)

	var id int64
	if err := c.store.DB().QueryRowContext(ctx, query, productName, repoName).Scan(&id); err != nil {
		if err == sql.ErrNoRows {
			return 0, fmt.Errorf("product %q has no active repository %q", productName, repoName)
		}
		return 0, fmt.Errorf("resolve repository %q/%q: %w", productName, repoName, err)
	}
	return id, nil
}
