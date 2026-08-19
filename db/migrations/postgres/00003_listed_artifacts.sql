-- Discovery no longer traverses a package's manifest tree.
--
-- It fetches the tag's OWN manifest and nothing else. When that manifest is an
-- index, its children are recorded from the descriptors the index already
-- carries - digest, media type, size, platform - without a request each.
--
-- Nothing is lost by deferring: the root digest immutably determines the whole
-- tree, so it can be walked exactly, at any time, from one digest we already
-- hold. What the traversal bought was a cache, paid for on every newly
-- discovered tag: a bundle whose index references sixty artifacts cost sixty
-- extra round trips, and a first scan of a real vendor catalogue ran into five
-- figures of requests.
--
-- So `raw` becomes nullable: NULL means "this artifact is listed by its parent
-- index and has not been fetched". A row with raw bytes was fetched and
-- verified against its digest; a row without has the vendor's word for it. The
-- distinction is worth being able to make, which is why this is NULL rather
-- than an empty blob.

-- +goose Up
ALTER TABLE package_artifacts ALTER COLUMN raw DROP NOT NULL;

-- +goose Down
-- Rows recorded from an index have no bytes to restore, so they are removed
-- rather than invented. The next scan re-records them.
DELETE FROM package_artifacts WHERE raw IS NULL;
ALTER TABLE package_artifacts ALTER COLUMN raw SET NOT NULL;
