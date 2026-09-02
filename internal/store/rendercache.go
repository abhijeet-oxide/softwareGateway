package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
)

// The rendered-chart cache: reading it, writing it, and keeping it inside a
// budget.
//
// See db/migrations/postgres/00040_compliance_render_cache.sql for why reusing a
// render is safe, and internal/compliance/rendercache.go for what makes up the
// key. This file is only the SQL.

// lookupBatch bounds one IN list.
//
// A 95-chart release asks for 190 keys, which is one statement on both dialects
// - but SQLite's default parameter ceiling is 999 and a release is not the only
// caller this could ever have, so the read is chunked rather than assuming.
const lookupBatch = 400

// LookupRenders returns the cached renders present among these keys.
//
// A miss is an absence, never an error: the caller renders the chart. The rows
// found are TOUCHED, because the sweep evicts by last use and a cache that never
// recorded a hit would evict exactly the entries every run depends on.
func (p *Packages) LookupRenders(
	ctx context.Context, keys []string,
) (map[string]compliance.CachedRender, error) {
	out := make(map[string]compliance.CachedRender, len(keys))
	if len(keys) == 0 {
		return out, nil
	}

	for start := 0; start < len(keys); start += lookupBatch {
		end := min(start+lookupBatch, len(keys))
		chunk := keys[start:end]

		marks := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, k := range chunk {
			marks[i] = "?"
			args[i] = k
		}
		query := p.dialect.Rewrite(`
			SELECT cache_key, chart_digest, variant, inputs_digest,
			       chart_name, chart_version, app_version, subchart_path,
			       values_yaml, manifests
			  FROM compliance_render_cache
			 WHERE cache_key IN (` + strings.Join(marks, ",") + `)`)

		rows, err := p.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("read the render cache: %w", err)
		}
		var found []string
		for rows.Next() {
			var (
				key       string
				r         compliance.CachedRender
				variant   string
				values    string
				manifests string
			)
			if err := rows.Scan(&key, &r.ChartDigest, &variant, &r.InputsDigest,
				&r.ChartName, &r.ChartVersion, &r.AppVersion, &r.SubchartPath,
				&values, &manifests); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan a cached render: %w", err)
			}
			r.Variant = compliance.RenderVariant(variant)
			r.ValuesYAML = []byte(values)
			r.Manifests = []byte(manifests)
			out[key] = r
			found = append(found, key)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("read the render cache: %w", err)
		}
		_ = rows.Close()

		if err := p.touchRenders(ctx, found); err != nil {
			// The read succeeded and the caller can use it. A cache whose LRU
			// hand did not move is a cache that evicts badly, which is a
			// tomorrow problem; failing this run over it is a today problem.
			return out, nil //nolint:nilerr // see above
		}
	}
	return out, nil
}

func (p *Packages) touchRenders(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	marks := make([]string, len(keys))
	args := make([]any, 0, len(keys)+1)
	args = append(args, securityTime(time.Now().UTC()))
	for i, k := range keys {
		marks[i] = "?"
		args = append(args, k)
	}
	_, err := p.db.ExecContext(ctx, p.dialect.Rewrite(
		`UPDATE compliance_render_cache SET last_used_at = ?
		  WHERE cache_key IN (`+strings.Join(marks, ",")+`)`), args...)
	return err
}

// StoreRenders records what a run produced.
//
// Upsert rather than insert, because two Coordinators may check two releases
// that share a chart at the same instant and produce byte-identical output for
// it. That is a race with no wrong outcome - the rows are equal - so it resolves
// by overwriting rather than by failing a run over a duplicate key.
func (p *Packages) StoreRenders(ctx context.Context, renders []compliance.CachedRender) error {
	if len(renders) == 0 {
		return nil
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin a render cache write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := securityTime(time.Now().UTC())
	stmt, err := tx.PrepareContext(ctx, p.dialect.Rewrite(`
		INSERT INTO compliance_render_cache
		  (cache_key, chart_digest, variant, inputs_digest,
		   chart_name, chart_version, app_version, subchart_path,
		   values_yaml, manifests, bytes, created_at, last_used_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (cache_key) DO UPDATE SET
		  manifests = EXCLUDED.manifests,
		  values_yaml = EXCLUDED.values_yaml,
		  bytes = EXCLUDED.bytes,
		  chart_name = EXCLUDED.chart_name,
		  chart_version = EXCLUDED.chart_version,
		  app_version = EXCLUDED.app_version,
		  subchart_path = EXCLUDED.subchart_path,
		  last_used_at = EXCLUDED.last_used_at`))
	if err != nil {
		return fmt.Errorf("prepare a render cache write: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, r := range renders {
		key := r.Key()
		if key == "" {
			continue
		}
		if _, err := stmt.ExecContext(ctx,
			key, r.ChartDigest, string(r.Variant), r.InputsDigest,
			r.ChartName, r.ChartVersion, r.AppVersion, r.SubchartPath,
			string(r.ValuesYAML), string(r.Manifests),
			len(r.Manifests)+len(r.ValuesYAML), now, now); err != nil {
			return fmt.Errorf("write a cached render for %s: %w", r.ChartDigest, err)
		}
	}
	return tx.Commit()
}

// RenderCachePolicy bounds the cache.
//
// Two bounds, as with the manifest cache. The TTL removes a chart a vendor has
// stopped shipping, which no budget would ever reach because nothing asks for
// it; the byte budget removes the tail when a large estate keeps the whole cache
// warm and the TTL never fires. Zero on either means that bound is off.
type RenderCachePolicy struct {
	TTL    time.Duration
	Budget int64
}

// Enabled reports whether anything would be evicted.
func (p RenderCachePolicy) Enabled() bool { return p.TTL > 0 || p.Budget > 0 }

// RenderCacheResult is what one sweep reclaimed.
type RenderCacheResult struct {
	Expired int64
	Trimmed int64
	Bytes   int64
}

// Rows is the total, for deciding whether the sweep is worth a log line.
func (r RenderCacheResult) Rows() int64 { return r.Expired + r.Trimmed }

// SweepRenderCache expires stale entries and trims the rest to the budget.
//
// Nothing here can be wrong, only slow: an evicted entry costs one render to
// rebuild. That is what makes an LRU acceptable at all - the cost of a bad
// eviction decision is bounded and self-correcting.
func (p *Packages) SweepRenderCache(
	ctx context.Context, policy RenderCachePolicy,
) (RenderCacheResult, error) {
	var res RenderCacheResult

	if policy.TTL > 0 {
		result, err := p.db.ExecContext(ctx, p.dialect.Rewrite(
			`DELETE FROM compliance_render_cache WHERE last_used_at < `+
				p.dialect.TimeAgo("?")), policy.TTL.Seconds())
		if err != nil {
			return res, fmt.Errorf("expire the render cache: %w", err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return res, fmt.Errorf("count expired renders: %w", err)
		}
		res.Expired = n
	}

	if policy.Budget <= 0 {
		return res, nil
	}

	var total int64
	if err := p.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(bytes), 0) FROM compliance_render_cache`).Scan(&total); err != nil {
		return res, fmt.Errorf("measure the render cache: %w", err)
	}
	if total <= policy.Budget {
		return res, nil
	}

	// Oldest-used first until the overage is covered. Selected in a read and
	// deleted by key rather than deleted by a correlated subquery, because the
	// two dialects disagree about DELETE ... ORDER BY LIMIT and this shape works
	// unchanged on both.
	need := total - policy.Budget
	rows, err := p.db.QueryContext(ctx,
		`SELECT cache_key, bytes FROM compliance_render_cache ORDER BY last_used_at ASC`)
	if err != nil {
		return res, fmt.Errorf("choose renders to evict: %w", err)
	}
	var (
		keys    []string
		freeing int64
	)
	for rows.Next() && freeing < need {
		var (
			key   string
			bytes int64
		)
		if err := rows.Scan(&key, &bytes); err != nil {
			_ = rows.Close()
			return res, fmt.Errorf("scan a render to evict: %w", err)
		}
		keys = append(keys, key)
		freeing += bytes
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return res, fmt.Errorf("choose renders to evict: %w", err)
	}
	_ = rows.Close()

	for start := 0; start < len(keys); start += lookupBatch {
		end := min(start+lookupBatch, len(keys))
		chunk := keys[start:end]
		marks := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, k := range chunk {
			marks[i] = "?"
			args[i] = k
		}
		result, err := p.db.ExecContext(ctx, p.dialect.Rewrite(
			`DELETE FROM compliance_render_cache WHERE cache_key IN (`+
				strings.Join(marks, ",")+`)`), args...)
		if err != nil {
			return res, fmt.Errorf("evict cached renders: %w", err)
		}
		n, _ := result.RowsAffected()
		res.Trimmed += n
	}
	res.Bytes = freeing
	return res, nil
}

// RenderCacheStats is what the cache currently holds.
type RenderCacheStats struct {
	Entries int64
	Bytes   int64
}

// RenderCacheStats reports the cache's size, for the health page and the log
// line a Coordinator writes when it starts.
func (p *Packages) RenderCacheStats(ctx context.Context) (RenderCacheStats, error) {
	var s RenderCacheStats
	err := p.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(bytes), 0) FROM compliance_render_cache`).
		Scan(&s.Entries, &s.Bytes)
	if err != nil {
		return s, fmt.Errorf("measure the render cache: %w", err)
	}
	return s, nil
}
