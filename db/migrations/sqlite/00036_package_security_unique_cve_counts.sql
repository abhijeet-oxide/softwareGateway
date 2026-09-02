-- +goose Up

ALTER TABLE package_security ADD COLUMN unique_cve_critical INTEGER NOT NULL DEFAULT 0;
ALTER TABLE package_security ADD COLUMN unique_cve_high INTEGER NOT NULL DEFAULT 0;
ALTER TABLE package_security ADD COLUMN unique_cve_medium INTEGER NOT NULL DEFAULT 0;
ALTER TABLE package_security ADD COLUMN unique_cve_low INTEGER NOT NULL DEFAULT 0;
ALTER TABLE package_security ADD COLUMN unique_cve_unknown INTEGER NOT NULL DEFAULT 0;

-- +goose Down

ALTER TABLE package_security DROP COLUMN unique_cve_unknown;
ALTER TABLE package_security DROP COLUMN unique_cve_low;
ALTER TABLE package_security DROP COLUMN unique_cve_medium;
ALTER TABLE package_security DROP COLUMN unique_cve_high;
ALTER TABLE package_security DROP COLUMN unique_cve_critical;