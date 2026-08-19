package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The manifest cache, and why there is one.
//
// A package's manifest BODIES are the only part of what we record that grows
// without bound and can be thrown away without losing a fact.
//
// Everything else a package knows about itself - which artifacts it contains,
// their digests, media types, sizes and platforms, the blobs they reference,
// the totals derived from them - is a few kilobytes even for a sixty-artifact
// release bundle, is read by every listing and every plan, and is kept forever.
//
// The bodies are different. They are large, they are read only when something
// PUSHES a manifest, and they are exactly recoverable: a manifest is addressed
// by the hash of its own bytes, so re-fetching one either returns the same
// bytes or fails the digest check. Keeping them is an optimisation, not a
// commitment - and over a vendor catalogue with thousands of packages
// accumulated across years, an optimisation nobody bounded becomes the largest
// thing in the database.
//
// So they are a cache with a budget, evicted least-recently-used. The schema
// keeps the FETCH (`fetched_at`) separate from the BYTES (`raw`) precisely so
// eviction costs nothing but a re-fetch if the bytes are wanted again - see
// migration 00007. Without that split, evicting would unlearn the walk and the
// next inspect would re-read the whole tree from the vendor, which is the one
// outcome that would make the cache not worth having.

// ManifestCachePolicy bounds the cached manifest bodies.
//
// Two bounds rather than one, because they answer different questions. TTL is
// "these have not been wanted in a long time"; the budget is "this is all the
// space there is". A deployment that transfers everything it discovers wants
// the budget to bite; one that discovers far more than it transfers wants the
// TTL to, and would otherwise carry a full budget of manifests nobody will ever
// push.
type ManifestCachePolicy struct {
	// BudgetBytes is the ceiling on cached manifest bodies. Zero or less
	// disables the budget pass, which is the right setting for a deployment
	// that would rather spend disk than re-fetch.
	BudgetBytes int64
	// TTL evicts bodies untouched for longer than this. Zero disables the age
	// pass.
	TTL time.Duration
}

// ManifestCacheStats describes what the cache currently holds.
type ManifestCacheStats struct {
	// Manifests is how many artifact rows exist in total, cached or not. It is
	// the size of what is KEPT, and the number to compare the cache against.
	Manifests int64
	// CachedManifests and CachedBytes are the evictable part.
	CachedManifests int64
	CachedBytes     int64
	// OldestUse is the least-recent raw_used_at still cached, empty when
	// nothing is.
	OldestUse string
}

// SweepResult reports one sweep.
type SweepResult struct {
	// ExpiredManifests and ExpiredBytes were evicted for age.
	ExpiredManifests int64
	ExpiredBytes     int64
	// EvictedManifests and EvictedBytes were evicted to fit the budget.
	EvictedManifests int64
	EvictedBytes     int64
	// RemainingBytes is what the cache holds afterwards.
	RemainingBytes int64
}

// Freed is the total reclaimed by this sweep.
func (r SweepResult) Freed() int64 { return r.ExpiredBytes + r.EvictedBytes }

// Manifests is the total number of bodies this sweep dropped.
func (r SweepResult) Manifests() int64 { return r.ExpiredManifests + r.EvictedManifests }

// sweepBatch bounds one eviction statement.
//
// The candidate list is read into memory to decide where the budget line falls,
// so it is chunked: a cache holding a million manifest bodies must not become a
// million-row result set and a statement with a million placeholders.
const sweepBatch = 2000

// maxSweepPasses stops a sweep that cannot make progress.
//
// Progress is guaranteed while there are candidates - every pass evicts at
// least one - but a bound turns a bug that would otherwise spin forever inside
// a background loop into a log line and a retry on the next tick.
const maxSweepPasses = 200

// SweepManifestCache reclaims cached manifest bodies down to the policy.
//
// It never touches `fetched_at`, the artifact rows, the blob links or the
// measured totals - so a swept package is still fully described, still lists
// its contents, and still reports its transfer size. What it loses is the
// shortcut of not having to ask the registry for bytes it can prove it received.
//
// Safe to run against a live system: an inspect racing a sweep either finds the
// bytes or re-fetches them, and both produce the same tree.
func (p *Packages) SweepManifestCache(ctx context.Context, policy ManifestCachePolicy) (SweepResult, error) {
	var res SweepResult

	if policy.TTL > 0 {
		n, freed, err := p.expireManifestCache(ctx, policy.TTL)
		if err != nil {
			return SweepResult{}, err
		}
		res.ExpiredManifests, res.ExpiredBytes = n, freed
	}

	if policy.BudgetBytes > 0 {
		n, freed, err := p.trimManifestCache(ctx, policy.BudgetBytes)
		if err != nil {
			return SweepResult{}, err
		}
		res.EvictedManifests, res.EvictedBytes = n, freed
	}

	stats, err := p.ManifestCacheStats(ctx)
	if err != nil {
		return SweepResult{}, err
	}
	res.RemainingBytes = stats.CachedBytes
	return res, nil
}

// expireManifestCache drops bodies untouched for longer than ttl.
//
// The sum is taken in the same statement's WHERE clause as the update, one
// after the other rather than atomically. A body written between them is at
// most one sweep's worth of accounting drift in a log line; taking a lock over
// the whole table to avoid that would be a far worse trade.
func (p *Packages) expireManifestCache(ctx context.Context, ttl time.Duration) (int64, int64, error) {
	cutoff := int64(ttl.Seconds())
	if cutoff <= 0 {
		return 0, 0, nil
	}

	// COALESCE, because a row cached before this column existed has a NULL
	// raw_used_at and must still be countable. The same COALESCE in the WHERE
	// clause below treats it as ancient, which it is.
	countQ := p.dialect.Rewrite(`
		SELECT COUNT(*), COALESCE(SUM(raw_bytes), 0)
		  FROM package_artifacts
		 WHERE raw IS NOT NULL
		   AND COALESCE(raw_used_at, fetched_at) < ` + p.dialect.TimeAgo("?"))

	var n, freed int64
	if err := p.db.QueryRowContext(ctx, countQ, cutoff).Scan(&n, &freed); err != nil {
		return 0, 0, fmt.Errorf("measure expired manifest cache: %w", err)
	}
	if n == 0 {
		return 0, 0, nil
	}

	updateQ := p.dialect.Rewrite(`
		UPDATE package_artifacts
		   SET raw = NULL, raw_bytes = 0, raw_used_at = NULL
		 WHERE raw IS NOT NULL
		   AND COALESCE(raw_used_at, fetched_at) < ` + p.dialect.TimeAgo("?"))

	if _, err := p.db.ExecContext(ctx, updateQ, cutoff); err != nil {
		return 0, 0, fmt.Errorf("expire manifest cache: %w", err)
	}
	return n, freed, nil
}

// trimManifestCache evicts least-recently-used bodies until the cache fits.
//
// LRU rather than by size or by age alone. The bodies worth keeping are the
// ones a transfer touched recently, because the access pattern that matters is
// "this product line is being replicated this week" - and evicting the largest
// first would preferentially throw away exactly the packages whose re-fetch
// costs most.
func (p *Packages) trimManifestCache(ctx context.Context, budget int64) (int64, int64, error) {
	var evicted, freed int64

	for pass := 0; pass < maxSweepPasses; pass++ {
		stats, err := p.ManifestCacheStats(ctx)
		if err != nil {
			return 0, 0, err
		}
		over := stats.CachedBytes - budget
		if over <= 0 {
			return evicted, freed, nil
		}

		ids, bytes, err := p.oldestCached(ctx, over)
		if err != nil {
			return 0, 0, err
		}
		if len(ids) == 0 {
			// Over budget with nothing cached to drop. Nothing is wrong: the
			// budget is smaller than a single body, or a concurrent writer beat
			// us to them.
			return evicted, freed, nil
		}
		if err := p.dropCached(ctx, ids); err != nil {
			return 0, 0, err
		}
		evicted += int64(len(ids))
		freed += bytes
	}
	return evicted, freed, nil
}

// oldestCached returns the least-recently-used cached rows adding up to at
// least `need` bytes, capped at one batch.
func (p *Packages) oldestCached(ctx context.Context, need int64) ([]int64, int64, error) {
	query := p.dialect.Rewrite(`
		SELECT id, raw_bytes
		  FROM package_artifacts
		 WHERE raw IS NOT NULL
		 ORDER BY COALESCE(raw_used_at, fetched_at) ASC, id ASC
		 LIMIT ?`)

	rows, err := p.db.QueryContext(ctx, query, sweepBatch)
	if err != nil {
		return nil, 0, fmt.Errorf("read manifest cache candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		ids   []int64
		total int64
	)
	for rows.Next() {
		var (
			id    int64
			bytes int64
		)
		if err := rows.Scan(&id, &bytes); err != nil {
			return nil, 0, fmt.Errorf("scan manifest cache candidate: %w", err)
		}
		ids = append(ids, id)
		total += bytes
		if total >= need {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return ids, total, nil
}

// dropCached releases the bodies of the named rows.
func (p *Packages) dropCached(ctx context.Context, ids []int64) error {
	const chunk = 500
	for start := 0; start < len(ids); start += chunk {
		end := min(start+chunk, len(ids))
		batch := ids[start:end]

		placeholders := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, id := range batch {
			placeholders[i] = "?"
			args[i] = id
		}

		query := p.dialect.Rewrite(`
			UPDATE package_artifacts
			   SET raw = NULL, raw_bytes = 0, raw_used_at = NULL
			 WHERE id IN (` + strings.Join(placeholders, ",") + `)`)

		if _, err := p.db.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("evict manifest cache entries: %w", err)
		}
	}
	return nil
}

// ManifestCacheStats reports what the cache holds.
func (p *Packages) ManifestCacheStats(ctx context.Context) (ManifestCacheStats, error) {
	var (
		s      ManifestCacheStats
		oldest *string
	)

	query := p.dialect.Rewrite(`
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN raw IS NOT NULL THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(raw_bytes), 0),
		       MIN(raw_used_at)
		  FROM package_artifacts`)

	if err := p.db.QueryRowContext(ctx, query).Scan(
		&s.Manifests, &s.CachedManifests, &s.CachedBytes, &oldest); err != nil {
		return ManifestCacheStats{}, fmt.Errorf("read manifest cache stats: %w", err)
	}
	if oldest != nil {
		s.OldestUse = *oldest
	}
	return s, nil
}

// TouchManifestCache marks a package's cached bodies as recently used.
//
// Called by whoever actually READS the bytes - the planner, on its way to
// pushing them - and deliberately not by `describe` or `list`, which read the
// tree's shape and never the bodies. An LRU stamp bumped by looking at a
// package would rank the cache by curiosity rather than by use, and would keep
// alive exactly the manifests nobody is going to push.
func (p *Packages) TouchManifestCache(ctx context.Context, packageID int64) error {
	query := p.dialect.Rewrite(`
		UPDATE package_artifacts
		   SET raw_used_at = ` + p.dialect.Now() + `
		 WHERE package_id = ? AND raw IS NOT NULL`)

	if _, err := p.db.ExecContext(ctx, query, packageID); err != nil {
		return fmt.Errorf("touch manifest cache for package %d: %w", packageID, err)
	}
	return nil
}

// CachedManifest is a manifest body we still hold, and what it is.
type CachedManifest struct {
	Raw       []byte
	MediaType string
	SizeBytes int64
}

// ManifestByDigest returns a manifest body we have already fetched.
//
// # Why a lookup by digest alone is correct
//
// A manifest is addressed by the hash of its own bytes. Bytes that hash to a
// digest are THE bytes for that digest, whichever package, product or registry
// we happened to fetch them from - so this asks no question about ownership and
// needs none. The alternative, a lookup scoped to one package, would miss the
// case this exists for: the same component appearing in two releases of the
// same product line, which is most of them.
//
// # And why a miss is unremarkable
//
// Bodies are an evictable cache (see the top of this file). A miss means "not
// here", never "does not exist", and every caller has a registry to fall back
// to. `touched_at` is deliberately NOT updated here: a read-through for a
// comparison is not the same as a push wanting the bytes, and letting a
// comparison of an old release keep the eviction pass off a body nobody
// transfers would be the cache serving the wrong master.
func (p *Packages) ManifestByDigest(ctx context.Context, digest string) (CachedManifest, bool, error) {
	var m CachedManifest
	err := p.db.QueryRowContext(ctx, p.dialect.Rewrite(`
		SELECT raw, media_type, size_bytes
		  FROM package_artifacts
		 WHERE digest = ? AND raw IS NOT NULL
		 LIMIT 1`), digest).Scan(&m.Raw, &m.MediaType, &m.SizeBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return CachedManifest{}, false, nil
	}
	if err != nil {
		return CachedManifest{}, false, fmt.Errorf("read cached manifest %s: %w", digest, err)
	}
	return m, true, nil
}
