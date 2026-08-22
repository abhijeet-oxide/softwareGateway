package security

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// PackageResult is one release's finished sync, in this package's own terms.
//
// Defined here rather than in the store so the sync can be written without
// importing persistence - the dependency points the other way, and the store
// adapts this into its row.
type PackageResult struct {
	PackageID int64

	Provider   string
	Repository string
	Role       string

	Posture     Posture
	Fingerprint string
}

// Recorder is where a sync writes what it found.
//
// Three methods and each is a state transition, not a setter. That shape is
// deliberate: the claim has to be atomic against a second caller, and the
// failure path has to be distinguishable from the success path by a reader who
// only ever sees the row.
type Recorder interface {
	// Claim marks a release as syncing. Returns an error the caller reports as
	// "already running" when somebody else holds it.
	Claim(ctx context.Context, packageID int64, staleAfter time.Duration) error
	// Record stores a finished sync.
	Record(ctx context.Context, res PackageResult) error
	// Fail records a sync that gave up, keeping the last good counts.
	Fail(ctx context.Context, packageID int64, reason string) error
}

// SyncRequest is one release's sync.
type SyncRequest struct {
	PackageID int64
	// Label is what to call the release in progress messages.
	Label string
	Scope Scope
	// Artifacts is what to ask about, assembled by the caller from the
	// release's own tree.
	Artifacts []ArtifactRef
	TTL       CacheTTL
}

// SyncStatus is what a caller is told when it asks for a sync.
type SyncStatus string

const (
	// SyncStarted means this call claimed the release and work is under way.
	SyncStarted SyncStatus = "started"
	// SyncAlreadyRunning means somebody else holds it. Not a failure - the
	// thing the caller wanted is happening.
	SyncAlreadyRunning SyncStatus = "already_running"
)

// staleClaimAfter is how long a claim is honoured before it is treated as
// abandoned.
//
// Generous, because a release of several hundred artifacts against a busy
// scanner is genuinely slow, and stealing a live claim costs a duplicate query.
// Finite, because a process that died mid-sync would otherwise leave a release
// permanently unsyncable, showing a spinner nobody can clear - which is worse
// than a rare double sync that converges on the same rows.
const staleClaimAfter = 30 * time.Minute

// Syncer runs vulnerability syncs in the background.
//
// # Why a job rather than a read
//
// Retrieving a release's posture is a scanner query per batch across a few
// hundred artifacts - tens of seconds, sometimes minutes. Doing that on a page
// render means a page that takes minutes, twenty of them behind a listing, and
// a scanner rate-limiting the whole estate because somebody scrolled.
//
// So it is an explicit act with a durable result. Somebody asks, the release
// says it is syncing, the answer is stored, and from then on every listing,
// comparison and search reads what is stored and troubles no scanner at all.
// That is also what makes search answerable at all: there is a table to search.
type Syncer struct {
	service  *Service
	recorder Recorder
	log      *slog.Logger

	mu       sync.Mutex
	inFlight map[int64]*SyncProgress
}

// NewSyncer builds the syncer.
func NewSyncer(service *Service, recorder Recorder, log *slog.Logger) *Syncer {
	if log == nil {
		log = slog.Default()
	}
	return &Syncer{
		service:  service,
		recorder: recorder,
		log:      log,
		inFlight: map[int64]*SyncProgress{},
	}
}

// SyncProgress is what one running sync will say about itself.
type SyncProgress struct {
	mu        sync.Mutex
	stages    map[string]stagePosition
	order     []string
	notes     []string
	startedAt time.Time
	done      bool
}

type stagePosition struct{ done, total int }

// Stage implements Progress.
func (p *SyncProgress) Stage(name string, done, total int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur, seen := p.stages[name]
	if !seen {
		p.order = append(p.order, name)
	}
	// Never backwards: batches land concurrently and an earlier number
	// arriving late would make the bar stutter.
	if done >= cur.done {
		cur.done = done
	}
	if total > cur.total {
		cur.total = total
	}
	p.stages[name] = cur
}

// Note implements Progress.
func (p *SyncProgress) Note(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.notes) >= 20 {
		p.notes = p.notes[1:]
	}
	p.notes = append(p.notes, s)
}

// Snapshot returns a copy safe to serialize.
func (p *SyncProgress) Snapshot() (stages []SyncStage, notes []string, startedAt time.Time, done bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, name := range p.order {
		pos := p.stages[name]
		stages = append(stages, SyncStage{Name: name, Done: pos.done, Total: pos.total})
	}
	return stages, append([]string(nil), p.notes...), p.startedAt, p.done
}

// SyncStage is one phase's position.
type SyncStage struct {
	Name        string
	Done, Total int
}

// Start claims a release and syncs it in the background.
//
// Returns as soon as the claim is decided, never when the work is done. The
// caller is answering an HTTP request and the work is minutes; holding the
// request open would put every intermediary's idle timeout into the control
// plane, which is the same argument discovery's `wait: false` makes.
//
// The background context is deliberately detached from the request's. A user
// who navigates away must not cancel a sync half way through and leave the row
// claimed - the whole point of making this a job is that it outlives the page.
func (s *Syncer) Start(ctx context.Context, req SyncRequest) (SyncStatus, error) {
	if err := s.recorder.Claim(ctx, req.PackageID, staleClaimAfter); err != nil {
		return SyncAlreadyRunning, err
	}

	progress := &SyncProgress{stages: map[string]stagePosition{}, startedAt: time.Now()}
	s.mu.Lock()
	s.inFlight[req.PackageID] = progress
	s.mu.Unlock()

	go s.run(req, progress)
	return SyncStarted, nil
}

// Progress returns the live progress of a running sync, if this replica is the
// one running it.
//
// A miss is a normal answer: the sync may be on another replica, or may have
// finished. The caller falls back to the stored state, which is durable.
func (s *Syncer) Progress(packageID int64) (*SyncProgress, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.inFlight[packageID]
	return p, ok
}

// Running reports whether this replica is syncing the release right now.
func (s *Syncer) Running(packageID int64) bool {
	_, ok := s.Progress(packageID)
	return ok
}

func (s *Syncer) run(req SyncRequest, progress *SyncProgress) {
	// Bounded, because an unbounded background job against an unresponsive
	// scanner is a goroutine that never ends and a row that stays claimed until
	// the stale sweep notices.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), staleClaimAfter)
	defer cancel()

	defer func() {
		s.mu.Lock()
		delete(s.inFlight, req.PackageID)
		s.mu.Unlock()
	}()

	res, err := s.service.Posture(ctx, Request{
		Scope:     req.Scope,
		Artifacts: req.Artifacts,
		// Detail, always. A sync exists to fill the index that search and
		// comparison read; counts alone would leave both of them empty and
		// force the scanner query back onto the page that reads them.
		Detail:   true,
		Refresh:  true,
		TTL:      req.TTL,
		Progress: progress,
	})
	if err != nil {
		s.log.Error("security sync failed", "package", req.PackageID, "label", req.Label, "error", err)
		if ferr := s.recorder.Fail(ctx, req.PackageID, err.Error()); ferr != nil {
			s.log.Error("could not record security sync failure", "package", req.PackageID, "error", ferr)
		}
		return
	}

	// A sync that reached the scanner and was told "off" is not a success with
	// zero findings. Recording it as one would put a clean-looking row in a
	// listing for a release nobody scanned.
	if !res.Enabled {
		reason := "JFrog Xray is not enabled for the repository holding this release."
		if len(res.Posture.Reports) > 0 && res.Posture.Reports[0].Message != "" {
			reason = res.Posture.Reports[0].Message
		}
		if ferr := s.recorder.Fail(ctx, req.PackageID, reason); ferr != nil {
			s.log.Error("could not record disabled scanner", "package", req.PackageID, "error", ferr)
		}
		return
	}

	// A sync that reached the scanner and got results for NOTHING is not a
	// success with zero findings. It is the failure this whole feature exists
	// to keep visible: recorded as a success it would put a clean-looking row -
	// "0 vulnerabilities" - in a listing for a release nobody scanned.
	//
	// Distinct from a release with nothing scannable in it, which is an honest
	// zero and stays a success.
	if cov := res.Posture.Coverage; cov.Scannable() > 0 && cov.Scanned == 0 {
		reason := fmt.Sprintf(
			"The scanner returned no results for any of the %d artifacts in this release.",
			cov.Scannable())
		if msg := firstMessage(res.Posture.Reports); msg != "" {
			reason += " " + msg
		}
		s.log.Warn("security sync produced no results",
			"package", req.PackageID, "label", req.Label, "artifacts", cov.Scannable())
		if ferr := s.recorder.Fail(ctx, req.PackageID, reason); ferr != nil {
			s.log.Error("could not record empty security sync", "package", req.PackageID, "error", ferr)
		}
		return
	}

	ReportStage(progress, StageCorrelating, len(res.Posture.Reports), len(res.Posture.Reports))
	if err := s.recorder.Record(ctx, PackageResult{
		PackageID:   req.PackageID,
		Provider:    res.Provider,
		Repository:  req.Scope.Repository,
		Role:        req.Scope.Role,
		Posture:     res.Posture,
		Fingerprint: res.Fingerprint,
	}); err != nil {
		s.log.Error("could not record security sync", "package", req.PackageID, "error", err)
		return
	}

	s.log.Info("security sync complete",
		"package", req.PackageID, "label", req.Label,
		"artifacts", res.Posture.Coverage.Artifacts,
		"scanned", res.Posture.Coverage.Scanned,
		"vulnerabilities", res.Posture.Counts.Total,
		"fromCache", res.FromCache, "fetched", res.Fetched)
}

// firstMessage is whatever the provider said about the first artifact it could
// not answer for.
//
// One message rather than all of them: a release of three hundred artifacts
// against an unreachable scanner produces three hundred identical sentences,
// and the useful part is the sentence, not the count.
func firstMessage(reports []Report) string {
	for _, r := range reports {
		if r.Status == StatusUnavailable && r.Message != "" {
			return r.Message
		}
	}
	for _, r := range reports {
		if r.Message != "" {
			return r.Message
		}
	}
	return ""
}

// Describe renders a sync's position as a sentence, for a caller that has a
// stored state and no live progress.
func Describe(stages []SyncStage) string {
	if len(stages) == 0 {
		return "Starting"
	}
	last := stages[len(stages)-1]
	if last.Total > 0 {
		return fmt.Sprintf("%s %d of %d", last.Name, last.Done, last.Total)
	}
	return last.Name
}
