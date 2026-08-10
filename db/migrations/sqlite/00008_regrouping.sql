-- Make a source's `vendor` retroactive, and give an accessory tag somewhere to
-- go. See the Postgres migration of the same name for the argument.

-- +goose Up
ALTER TABLE repositories ADD COLUMN grouped_layout TEXT;

ALTER TABLE packages ADD COLUMN accessory_of INTEGER REFERENCES packages(id);
CREATE INDEX packages_accessory_of_idx ON packages (accessory_of)
    WHERE accessory_of IS NOT NULL;

-- +goose Down
DROP INDEX packages_accessory_of_idx;
ALTER TABLE packages DROP COLUMN accessory_of;
ALTER TABLE repositories DROP COLUMN grouped_layout;
