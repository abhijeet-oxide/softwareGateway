-- Content the vendor will not serve us, remembered rather than re-reported.
-- See the Postgres migration of the same name for the argument.

-- +goose Up
CREATE TABLE unavailable_packages (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    product_id         INTEGER NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    repository_path    TEXT    NOT NULL,
    tag                TEXT    NOT NULL,
    display_repository TEXT    NOT NULL DEFAULT '',
    display_tag        TEXT    NOT NULL DEFAULT '',
    reason             TEXT    NOT NULL,
    detail             TEXT    NOT NULL DEFAULT '',
    first_seen_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    last_seen_at       TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    UNIQUE (product_id, repository_path, tag)
);

CREATE INDEX unavailable_packages_product_idx
    ON unavailable_packages (product_id, last_seen_at DESC);

-- +goose Down
DROP TABLE unavailable_packages;
