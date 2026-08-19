package discovery

import (
	"context"
	"time"
)

// Walking a release before anybody asks for it.
//
// # What discovery leaves behind, and why it is not enough
//
// Discovery records what a tag's own manifest LISTS and stops (docs/design/07
// §12). That is the right trade for the question it answers — "what is new" —
// and it leaves a package whose contents are unknown: no sizes, no files, no
// account of what is inside. Every page that opens it says "nobody has looked",
// and every comparison of it walks the vendor's registry from scratch.
//
// Somebody eventually pays for that walk. The only question is whether they pay
// while waiting for a page they asked for, or whether it happened at three in
// the morning after the release was published.
//
// # Bounded on both axes, because a vendor catalogue is enormous
//
// RECENT, because the releases anybody opens, compares or downloads are the new
// ones — an eight-month-old release is walked when somebody actually asks. And
// a FEW PER SCAN, so a source that has just discovered two hundred packages
// catches up over several scans rather than opening two hundred conversations
// with somebody else's registry in one go.
//
// A failure is logged and dropped. This is work nobody asked for, so it must
// never be the reason a scan reports failure — the release stays unanalysed and
// is picked up next time, or by the person who opens it.

const (
	// autoAnalyseWindow is how new a release must be to be walked unasked.
	// The same seven days the interface treats as "new".
	autoAnalyseWindow = 7 * 24 * time.Hour
	// autoAnalyseBatch bounds one scan's worth of walking.
	autoAnalyseBatch = 3
)

// analyseRecent walks a few recently published releases that nobody has walked.
//
// Returns how many were analysed, for the log line that says the system did
// something on its own.
func (s *Scanner) analyseRecent(ctx context.Context) (analysed int) {
	if s.packages == nil {
		return 0
	}

	// The repositories this scanner has clients for are the ones it just
	// scanned, which is exactly the set whose packages it can reach: a
	// product's sources are distinct registries, and a client built for one of
	// them cannot fetch from another.
	for _, repoPath := range s.knownRepositories() {
		if ctx.Err() != nil || analysed >= autoAnalyseBatch {
			return analysed
		}

		repoID, err := s.ensureRepositoryRow(ctx, repoPath)
		if err != nil {
			s.log.DebugContext(ctx, "could not resolve a repository for analysis",
				"repository", repoPath, "error", err)
			continue
		}

		rows, err := s.packages.UnanalysedRecent(
			ctx, repoID, autoAnalyseWindow, autoAnalyseBatch-analysed)
		if err != nil {
			s.log.WarnContext(ctx, "could not look for releases to analyse",
				"repository", repoPath, "error", err)
			continue
		}

		client, err := s.clientFor(repoPath)
		if err != nil {
			continue
		}

		for _, pkg := range rows {
			if ctx.Err() != nil {
				return analysed
			}
			res, err := InspectPackage(ctx, s.packages, pkg, client,
				s.sourceCfg.Concurrency.PerRegistry)
			if err != nil {
				// Expected often enough to be a debug line: a release whose
				// components the vendor has withdrawn cannot be walked, and
				// saying so at WARN once per scan per release would drown the
				// log in something nobody can act on.
				s.log.DebugContext(ctx, "could not analyse a release unasked",
					"package", pkg.Tag, "repository", pkg.SourceRepository, "error", err)
				continue
			}
			analysed++
			s.log.InfoContext(ctx, "analysed a new release",
				"package", pkg.Tag, "repository", pkg.SourceRepository,
				"artifacts", res.Artifacts, "fetched", res.Fetched)
		}
	}
	return analysed
}

// knownRepositories is the repository paths this scanner has clients for.
func (s *Scanner) knownRepositories() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]string, 0, len(s.clients))
	for path := range s.clients {
		out = append(out, path)
	}
	return out
}
