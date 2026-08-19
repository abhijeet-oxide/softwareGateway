package discovery

import (
	"context"
	"sync"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
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
// # It runs BESIDE the scan, not inside it
//
// This used to be the last step of a scan, which made a scan as slow as the
// walking — a scan that found ten releases took as long as walking ten
// manifest trees, and the scan reported nothing during it. Now the scan wakes
// the analyser and returns; the analyser walks several releases at once and
// says so in the release's own state, so a page opened during it shows
// `Analyzing` rather than an invitation to start the walk again.
//
// # Bounded on every axis, because a vendor catalogue is enormous
//
// RECENT, because the releases anybody opens, compares or downloads are the new
// ones — an eight-month-old release is walked when somebody actually asks. A
// BATCH per pass, so a source that has just discovered two hundred packages
// catches up over several passes. And the per-release concurrency is DIVIDED by
// the number of releases in flight, so walking four at once puts no more load
// on the vendor's registry than walking one did.
//
// # The claim survives a crash
//
// A release being walked is claimed in the database, not in memory, because the
// process holding it can die. On start the analyser releases claims that are
// too old for anything to still be holding, and those releases are walked
// again — which is what stops a restart leaving a release marked `Analyzing`
// for ever.

const (
	// autoAnalyseWindow is how new a release must be to be walked unasked.
	// The same seven days the interface treats as "new".
	autoAnalyseWindow = 7 * 24 * time.Hour
	// autoAnalyseBatch bounds one pass's worth of walking, per repository.
	autoAnalyseBatch = 12
	// analyseWorkers is how many releases are walked at once.
	analyseWorkers = 4
	// analyseIdle is how often the analyser looks for work with nobody waking
	// it. It exists for the releases a RESTART released: without it they would
	// wait for the next scan of their source, which may be an hour away.
	analyseIdle = 5 * time.Minute
	// analyseStale is how long a claim may be held before it is assumed
	// abandoned. Longer than any single release takes to walk, and short
	// enough that a crash costs half an hour rather than a day.
	analyseStale = 30 * time.Minute
)

// runAnalyser is the background walk. One per source, started with the source
// worker's own context so it lives as long as the loop does.
func (s *Scanner) runAnalyser(ctx context.Context) {
	if s.packages == nil {
		return
	}

	// Claims nobody can still be holding, released before anything else — a
	// release left marked `analyzing` by a crash is invisible to the queue
	// below until this runs.
	if n, err := s.packages.RecoverAnalyses(ctx, analyseStale); err != nil {
		s.log.WarnContext(ctx, "could not release abandoned analysis claims", "error", err)
	} else if n > 0 {
		s.log.InfoContext(ctx, "released analysis claims left by a stopped process, "+
			"and will walk those releases again", "releases", n)
	}

	ticker := time.NewTicker(analyseIdle)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.analyseWake:
			s.analysePass(ctx)
		case <-ticker.C:
			s.analysePass(ctx)
		}
	}
}

// wakeAnalyser asks for a pass without waiting for one.
//
// A one-slot signal rather than a queue: the analyser looks for ALL outstanding
// work when it wakes, so a second request while one is pending is the same
// request. Non-blocking, because the caller is a scan and the scan must not
// wait on this — the whole point of the change.
func (s *Scanner) wakeAnalyser() {
	select {
	case s.analyseWake <- struct{}{}:
	default:
	}
}

// analysePass walks whatever is outstanding, several releases at a time.
func (s *Scanner) analysePass(ctx context.Context) {
	type job struct {
		pkg    store.PackageRow
		client registry.Source
	}

	var jobs []job
	for _, repoPath := range s.knownRepositories() {
		if ctx.Err() != nil {
			return
		}

		repoID, err := s.ensureRepositoryRow(ctx, repoPath)
		if err != nil {
			s.log.DebugContext(ctx, "could not resolve a repository for analysis",
				"repository", repoPath, "error", err)
			continue
		}

		rows, err := s.packages.UnanalysedRecent(ctx, repoID, autoAnalyseWindow, autoAnalyseBatch)
		if err != nil {
			s.log.WarnContext(ctx, "could not look for releases to analyse",
				"repository", repoPath, "error", err)
			continue
		}
		if len(rows) == 0 {
			continue
		}

		client, err := s.clientFor(repoPath)
		if err != nil {
			continue
		}
		for _, pkg := range rows {
			jobs = append(jobs, job{pkg: pkg, client: client})
		}
	}
	if len(jobs) == 0 {
		return
	}

	// The configured ceiling is per REGISTRY, so walking four releases at once
	// must not mean four times the requests. Divided rather than multiplied.
	perRelease := s.sourceCfg.Concurrency.PerRegistry / analyseWorkers
	if perRelease < 2 {
		perRelease = 2
	}

	sem := make(chan struct{}, analyseWorkers)
	var wg sync.WaitGroup
	var walked int64
	var mu sync.Mutex

	for _, j := range jobs {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// The claim decides who walks it. Another replica, another source
			// worker, or a person who pressed Analyze may have taken it between
			// the query above and here, and losing that race is an ordinary
			// outcome with nothing to report.
			claimed, err := s.packages.ClaimAnalysis(ctx, j.pkg.ID)
			if err != nil || !claimed {
				return
			}

			res, walkErr := InspectPackage(ctx, s.packages, j.pkg, j.client, perRelease)
			if err := s.packages.FinishAnalysis(ctx, j.pkg.ID, walkErr); err != nil {
				s.log.WarnContext(ctx, "could not record the end of an analysis",
					"package", j.pkg.Tag, "error", err)
			}
			if walkErr != nil {
				// Expected often enough to be a debug line: a release whose
				// components the vendor has withdrawn cannot be walked, and
				// saying so at WARN once a pass would drown the log in
				// something nobody can act on. The reason is on the release
				// itself, where somebody looking at that release will find it.
				s.log.DebugContext(ctx, "could not analyse a release unasked",
					"package", j.pkg.Tag, "repository", j.pkg.SourceRepository,
					"error", walkErr)
				return
			}

			mu.Lock()
			walked++
			mu.Unlock()
			s.log.InfoContext(ctx, "analysed a new release",
				"package", j.pkg.Tag, "repository", j.pkg.SourceRepository,
				"artifacts", res.Artifacts, "fetched", res.Fetched)
		}(j)
	}
	wg.Wait()

	if walked > 0 {
		s.log.InfoContext(ctx, "analysed newly discovered releases", "releases", walked)
	}
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
