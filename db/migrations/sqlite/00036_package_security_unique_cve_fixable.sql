-- +goose Up

ALTER TABLE package_security ADD COLUMN unique_cve_fixable INTEGER NOT NULL DEFAULT 0;

-- +goose Down

ALTER TABLE package_security DROP COLUMN unique_cve_fixable;