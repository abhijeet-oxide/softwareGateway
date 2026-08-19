-- A runnable manifest goes before blobs, not behind them.
--
-- # What changed underneath, and why the order has to follow
--
-- Under the wave barrier the two never competed: every blob finished before any
-- manifest could run. Per-artifact readiness (00011) ended that - a manifest
-- becomes runnable the moment its OWN content lands, while blobs belonging to
-- other artifacts are still moving - so for the first time both are in the
-- runnable set at once, and the order between them is a decision.
--
-- It was being made by accident, and made wrong. Ordering by size largest-first
-- is right for blobs: it is what stops a multi-gigabyte layer running alone at
-- the end while every other slot idles. It is exactly wrong for a manifest,
-- which is about a kilobyte and therefore sorts LAST however urgent it is.
-- Measured: a manifest whose content had landed sat behind 8 GiB of blobs.
--
-- A manifest costs one round trip, moves no bandwidth worth counting, and
-- UNBLOCKS the index above it. Making it wait delays the whole tree to save
-- nothing.
--
-- Manifests cannot starve blobs in return, and the reason is structural rather
-- than a tuning choice: a manifest is only runnable once its own content is
-- present, so the supply of them is bounded by work that has already finished.
--
-- # Why `kind DESC` rather than a CASE expression
--
-- 'manifest' > 'blob' lexicographically, so DESC puts manifests first and the
-- index can match the ORDER BY exactly - which is the property that keeps the
-- dequeue an index scan with no sort. A CASE would express the intent more
-- plainly and would not be indexable.
--
-- THIS DEPENDS ON THERE BEING TWO KINDS AND ON THEIR SPELLING. A third kind
-- would land wherever the alphabet put it, silently. The `jobs.kind` CHECK
-- constraint limits the set, and TestARunnableManifestIsNotQueuedBehindBlobs
-- pins the resulting order, so adding one breaks a test rather than production.

-- +goose Up
DROP INDEX IF EXISTS jobs_dequeue_idx;
CREATE INDEX jobs_dequeue_idx
    ON jobs (priority DESC, kind DESC, site_rank, size_bytes DESC, id)
    WHERE state = 'pending' AND NOT paused;

-- +goose Down
DROP INDEX IF EXISTS jobs_dequeue_idx;
CREATE INDEX jobs_dequeue_idx
    ON jobs (priority DESC, site_rank, size_bytes DESC, id)
    WHERE state = 'pending' AND NOT paused;
