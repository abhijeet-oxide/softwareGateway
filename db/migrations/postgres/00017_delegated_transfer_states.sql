-- Two states a transfer can be in when the REGISTRY moves the bytes.
--
-- `syncing` is the one that cannot be avoided. A delegated transfer creates no
-- jobs, so without it such a transfer would sit in `running` with an empty job
-- set — and the wave-drain check would settle it immediately, reporting success
-- at the moment we had asked Quay to start and before anything had happened.
-- Reusing an existing state here would be exactly the failure docs/design/18
-- section 6 warns about: the same-shaped object with a field that quietly means
-- something else.
--
-- `diverged` is a terminal outcome distinct from both success and failure. Quay
-- reported that a sync succeeded, we walked the destination, and the tag
-- resolves to a DIFFERENT digest than the one we expected. That is normal for a
-- mirror whose upstream tag moved, and it is a fact worth recording rather than
-- an error worth paging somebody about. Collapsing it into `succeeded` would
-- lose the only signal that the content shipped is not the content requested;
-- collapsing it into `failed` would page somebody about a mirror working
-- exactly as designed.

-- +goose Up
ALTER TABLE transfers DROP CONSTRAINT transfers_state_check;
ALTER TABLE transfers ADD CONSTRAINT transfers_state_check
    CHECK (state IN ('pending','planning','ready','running','paused','syncing',
                     'verifying','succeeded','diverged','failed',
                     'cancelling','cancelled'));

-- +goose Down
-- Any transfer in one of the new states would violate the old constraint, so
-- they are settled first. `diverged` becomes `succeeded` because the content
-- did arrive; `syncing` becomes `failed` because it never finished and the old
-- schema has no way to say so.
UPDATE transfers SET state = 'succeeded' WHERE state = 'diverged';
UPDATE transfers SET state = 'failed', failure_reason = 'downgraded from syncing'
 WHERE state = 'syncing';
ALTER TABLE transfers DROP CONSTRAINT transfers_state_check;
ALTER TABLE transfers ADD CONSTRAINT transfers_state_check
    CHECK (state IN ('pending','planning','ready','running','paused',
                     'verifying','succeeded','failed','cancelling','cancelled'));
