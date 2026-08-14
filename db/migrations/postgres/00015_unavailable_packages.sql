-- Content the vendor will not serve us, remembered rather than re-reported.
--
-- A vendor registry serves a catalogue spanning every customer. Asking about a
-- product this customer has not licensed gets a 403 and a sentence:
--
--   {"ErrorType":"User Authorization",
--    "ErrorMessage":"No valid entitlement found for End User: 215952 and
--                    Product sales items: CFXC24STD03.00."}
--
-- That is the entitlement check WORKING. It is not an outage, not a bad
-- credential and not a configuration mistake, and there is nothing for an
-- operator to fix. On a real catalogue it happens to dozens of orbs on every
-- pass, forever.
--
-- Which is exactly why it needs a table rather than a log line. Treated as an
-- error it made every scheduled scan exit non-zero with thirty-seven lines of
-- URL attached — a signal that fires every fifteen minutes is a signal nobody
-- reads. Discarded entirely it becomes invisible, and the question "which orbs
-- are we not entitled to?" has no answer at all.
--
-- Recorded here it is a FACT with a history: what we could not read, why, when
-- we first saw it, and when we last confirmed it. `last_seen_at` is what makes
-- the row falsifiable — entitlement changes when somebody buys something, and a
-- row that stops being refreshed is one that came back.
--
-- Keyed by (product, repository, tag) rather than by scan, because the
-- interesting object is the artifact, not the attempt. Fifty scans of the same
-- unentitled orb are one row with a moving timestamp.

-- +goose Up
CREATE TABLE unavailable_packages (
    id                 BIGSERIAL PRIMARY KEY,
    product_id         BIGINT      NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    repository_path    TEXT        NOT NULL,
    tag                TEXT        NOT NULL,
    -- The vendor's own names, stored rather than derived, so a listing reads
    -- correctly even for a source whose `vendor` has since been changed.
    display_repository TEXT        NOT NULL DEFAULT '',
    display_tag        TEXT        NOT NULL DEFAULT '',
    -- reason is the class: `not_entitled` today.
    reason             TEXT        NOT NULL,
    -- detail is what the registry itself said, verbatim.
    detail             TEXT        NOT NULL DEFAULT '',
    first_seen_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (product_id, repository_path, tag)
);

CREATE INDEX unavailable_packages_product_idx
    ON unavailable_packages (product_id, last_seen_at DESC);

-- +goose Down
DROP TABLE unavailable_packages;
