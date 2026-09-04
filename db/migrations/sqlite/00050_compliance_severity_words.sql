-- One word for one thing.
--
-- The severity was stored as `block`, `warn` and `info` and printed as
-- "Critical", "Warning" and "Informational" - so a filter, an export column and
-- a screen each called the same level something different, and the value a
-- person saw in a saved CSV matched nothing they could type into the search.
--
-- The stored rows are rewritten here rather than translated on every read,
-- because a report is read far more often than it is written and a translation
-- in the read path is a translation somebody eventually forgets. The readers
-- still accept the old spelling (see compliance.ParseSeverity): this migration
-- is what makes that a shrinking population rather than a permanent one, and a
-- database restored from an older backup still renders correctly without it.

-- +goose Up

UPDATE compliance_results SET severity = 'critical' WHERE severity = 'block';
UPDATE compliance_results SET severity = 'warning'  WHERE severity = 'warn';
UPDATE compliance_results SET severity = 'inform'   WHERE severity = 'info';

-- +goose Down

UPDATE compliance_results SET severity = 'block' WHERE severity = 'critical';
UPDATE compliance_results SET severity = 'warn'  WHERE severity = 'warning';
UPDATE compliance_results SET severity = 'info'  WHERE severity = 'inform';
