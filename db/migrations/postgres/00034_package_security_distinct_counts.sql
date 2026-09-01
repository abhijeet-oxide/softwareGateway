-- +goose Up

ALTER TABLE package_security ADD COLUMN distinct_fixable INTEGER NOT NULL DEFAULT 0;
ALTER TABLE package_security ADD COLUMN distinct_critical INTEGER NOT NULL DEFAULT 0;
ALTER TABLE package_security ADD COLUMN distinct_high INTEGER NOT NULL DEFAULT 0;
ALTER TABLE package_security ADD COLUMN distinct_medium INTEGER NOT NULL DEFAULT 0;
ALTER TABLE package_security ADD COLUMN distinct_low INTEGER NOT NULL DEFAULT 0;
ALTER TABLE package_security ADD COLUMN distinct_unknown INTEGER NOT NULL DEFAULT 0;

-- +goose Down

ALTER TABLE package_security DROP COLUMN distinct_unknown;
ALTER TABLE package_security DROP COLUMN distinct_low;
ALTER TABLE package_security DROP COLUMN distinct_medium;
ALTER TABLE package_security DROP COLUMN distinct_high;
ALTER TABLE package_security DROP COLUMN distinct_critical;
ALTER TABLE package_security DROP COLUMN distinct_fixable;