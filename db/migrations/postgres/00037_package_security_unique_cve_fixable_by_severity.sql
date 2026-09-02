-- +goose Up

ALTER TABLE package_security ADD COLUMN unique_cve_fix_critical INTEGER NOT NULL DEFAULT 0;
ALTER TABLE package_security ADD COLUMN unique_cve_fix_high INTEGER NOT NULL DEFAULT 0;
ALTER TABLE package_security ADD COLUMN unique_cve_fix_medium INTEGER NOT NULL DEFAULT 0;
ALTER TABLE package_security ADD COLUMN unique_cve_fix_low INTEGER NOT NULL DEFAULT 0;
ALTER TABLE package_security ADD COLUMN unique_cve_fix_unknown INTEGER NOT NULL DEFAULT 0;

-- +goose Down

ALTER TABLE package_security DROP COLUMN unique_cve_fix_unknown;
ALTER TABLE package_security DROP COLUMN unique_cve_fix_low;
ALTER TABLE package_security DROP COLUMN unique_cve_fix_medium;
ALTER TABLE package_security DROP COLUMN unique_cve_fix_high;
ALTER TABLE package_security DROP COLUMN unique_cve_fix_critical;