-- Make a source's `vendor` retroactive, and give an accessory tag somewhere to
-- go.
--
-- # Why grouping was a one-way door
--
-- A vendor Layout turns a repository's tags into the packages they actually
-- represent - NEAR publishes `orb_X`, `signature_orb_X` and `signed_orb_X` for
-- one release, and they are one package with a signature, not three packages.
--
-- Grouping runs over the tags a scan finds NEW. That is deliberate and it is
-- what keeps the steady state cheap: a re-scan of an unchanged repository costs
-- one HEAD per tag and fetches no manifest bodies at all. The consequence
-- nobody had accounted for is that a repository scanned BEFORE its source
-- declared a vendor is never grouped again. Its packages keep
-- `signature_status = 'unknown'`, carry no relations, and have no transfer root
-- - so a transfer of one would move the payload and leave the signature behind,
-- which is the exact failure the Layout exists to prevent. Re-scanning does not
-- help, because re-scanning is precisely the path that skips them.
--
-- `repositories.grouped_layout` closes it. It records which convention a
-- repository's packages were last grouped under; when it disagrees with the
-- source's configured vendor, the next scan re-fetches that repository's tags
-- and groups them properly, once. It then agrees, and the steady state is cheap
-- again.
--
-- NULL means "never grouped", which is what every existing row gets - correct,
-- and it makes the first scan after this migration do the reconciliation for
-- any repository whose source declares a vendor.
--
-- # Why an accessory needs a column rather than a deletion
--
-- Under the standard layout all three NEAR tags became packages in their own
-- right. Grouping them afterwards fixes `orb_X` but leaves the other two listed
-- as though they were releases, which is most of the noise the Layout was meant
-- to remove.
--
-- They cannot simply be deleted: a transfer may reference them, and the history
-- of what was actually shipped has to stay answerable. `state = 'superseded'`
-- is the wrong word - that means the same tag re-pushed with different content,
-- and overloading it would corrupt the one question supersession answers.
--
-- So: `accessory_of` names the package this row turned out to be part of.
-- Deliberately shaped like `superseded_by` - the row survives, keeps its
-- history, and stops being listed as a release of its own.
--
-- Reversible by construction: removing a source's vendor clears it, and the
-- rows return to being ordinary packages.

-- +goose Up
ALTER TABLE repositories ADD COLUMN grouped_layout TEXT;

ALTER TABLE packages ADD COLUMN accessory_of BIGINT REFERENCES packages(id);
CREATE INDEX packages_accessory_of_idx ON packages (accessory_of)
    WHERE accessory_of IS NOT NULL;

-- +goose Down
DROP INDEX packages_accessory_of_idx;
ALTER TABLE packages DROP COLUMN accessory_of;
ALTER TABLE repositories DROP COLUMN grouped_layout;
