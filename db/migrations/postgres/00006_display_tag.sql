-- The tag with a vendor's structural noise removed.
--
-- Every listing repeats it: `orb_23.8.1076` is `23.8.1076` plus four characters
-- that say nothing, on every row, forever. Removing them is cosmetic - the real
-- tag is what is stored, transferred and returned by `-o json`.
--
-- A COLUMN rather than a transform at render time, for two reasons.
--
-- The transform is the VENDOR's: only the source's layout plugin knows that
-- `orb_` is Nokia's word for "release", and neither the store nor the CLI may
-- know it. Computing it once at discovery is how that knowledge stays in the
-- plugin.
--
-- And it makes the shortened form RESOLVABLE. A listing that shows a name you
-- cannot type back is a trap: someone copies what they see, gets "not found",
-- and reasonably concludes the package is gone. With the column, matching
-- either spelling is one more equality rather than a pattern match that would
-- have to encode the vendor's convention in SQL.
--
-- NULL means no shortening applies, which is what any conformant registry gets.

-- +goose Up
ALTER TABLE packages ADD COLUMN display_tag TEXT;
CREATE INDEX packages_display_tag_idx ON packages (source_repo_id, display_tag);

-- +goose Down
DROP INDEX packages_display_tag_idx;
ALTER TABLE packages DROP COLUMN display_tag;
