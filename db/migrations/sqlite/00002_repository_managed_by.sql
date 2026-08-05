-- +goose Up
-- Records WHO put a repository row here.
--
-- A source may now name several repositories, or enumerate them from the
-- registry catalog (docs/design/07 section 2.1). That creates two populations
-- of rows with different lifecycles:
--
--   config     a human declared it in YAML. Reconciliation owns it and
--              deactivates it when the declaration is removed.
--   discovery  we found it in the catalog. Reconciliation must NOT touch it,
--              or every configuration reload would deactivate every
--              discovered repository and the next scan would revive them —
--              a flap that would churn the audit trail for no reason.
--
-- Without this column the two are indistinguishable and reconciliation cannot
-- do its job without breaking discovery.

ALTER TABLE repositories
    ADD COLUMN managed_by TEXT NOT NULL DEFAULT 'config'
        CHECK (managed_by IN ('config', 'discovery'));

CREATE INDEX repositories_managed_by_idx ON repositories (managed_by, active);

-- +goose Down
DROP INDEX IF EXISTS repositories_managed_by_idx;
ALTER TABLE repositories DROP COLUMN managed_by;
