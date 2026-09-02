package compliance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Running one compliance check, from a button press to a stored result.
//
// # Why this is a service and not a handler
//
// A run takes minutes: fetching charts from a vendor registry, rendering each
// one twice, evaluating seventy checks against six hundred resources. An HTTP
// request cannot hold that open - the browser gives up, a proxy times out, and
// the work is lost with nothing recorded. So the handler starts a run and
// returns; the run reports progress; the result is written when it finishes.
//
// This is the shape internal/security.Syncer already has, and it is here for
// the same reasons: a claim so two Coordinators cannot both run one release, a
// heartbeat so a Coordinator that dies does not leave a release claimed
// forever, and live progress so somebody who pressed a button can see that
// something is happening.

// Stage is what a run is doing, in the words the interface shows.
type Stage string

const (
	StageFetching   Stage = "fetching"
	StageRendering  Stage = "rendering"
	StageEvaluating Stage = "evaluating"
	StageRecording  Stage = "recording"
)

// Label is the stage as a sentence, so the interface does not have to hold a
// second copy of this vocabulary.
func (s Stage) Label() string {
	switch s {
	case StageFetching:
		return "Fetching charts"
	case StageRendering:
		return "Rendering charts"
	case StageEvaluating:
		return "Evaluating checks"
	case StageRecording:
		return "Recording results"
	default:
		return "Working"
	}
}

// Progress is what a run reports while it is running.
//
// Done and Total are counts of the CURRENT stage, not of the whole run: "12 of
// 97 charts rendered" is a number somebody can reason about, and a single
// percentage across four stages of wildly different cost is not.
type Progress struct {
	RunID   string    `json:"runId"`
	Stage   Stage     `json:"stage"`
	Label   string    `json:"label"`
	Done    int       `json:"done"`
	Total   int       `json:"total"`
	Note    string    `json:"note,omitempty"`
	Started time.Time `json:"started"`
}

// Recorder is what a finished run is written through.
//
// An interface, so this package does not import the store: the dependency
// points the other way, exactly as it does for security.
type Recorder interface {
	StartComplianceRun(ctx context.Context, runID string, packageID int64, trigger string) error
	BeatComplianceRun(ctx context.Context, runID string) error
	RecordComplianceRun(ctx context.Context, runID string, packageID int64, run *Run) error
	FailComplianceRun(ctx context.Context, runID string, packageID int64, reason string) error
	ComplianceRunning(ctx context.Context, packageID int64) (string, bool, error)
}

// Source produces a release to judge, and reports what it could not.
//
// The interface exists so a run can be driven from an unpacked directory in a
// test and from a registry in production without the runner knowing which.
type Source interface {
	// Prepare acquires and renders. The returned cleanup is always safe to
	// call, even when the error is non-nil.
	Prepare(ctx context.Context, req Request, report func(Stage, int, int, string)) (*Release, Determiner, func(), error)
}

// Request names what to check.
type Request struct {
	RunID     string
	PackageID int64
	Product   string
	Release   string
	// Digest is the release's package digest, recorded on every finding so a
	// report identifies bytes rather than a tag somebody can move.
	Digest  string
	Trigger string
}

// ErrRunInFlight means this release is already being checked.
//
// Not a failure to show as one: it is the honest answer to "start a check" when
// one is running, and the interface should show the run in progress rather than
// an error nobody caused.
var ErrRunInFlight = errors.New("compliance: this release is already being checked")

// Runner starts, tracks and records compliance runs.
type Runner struct {
	Catalog  func() *Catalog
	Source   Source
	Recorder Recorder
	Waivers  WaiverSet
	Log      *slog.Logger

	// MaxResults truncates rather than exhausting memory on a pathological
	// release. A truncated run says so.
	MaxResults int
	// Beat is how often a live run touches its claim, and StaleAfter is how
	// long a claim survives without one. Beat must be comfortably shorter, or
	// a healthy run releases its own claim.
	Beat       time.Duration
	StaleAfter time.Duration

	mu      sync.Mutex
	running map[int64]*runState
}

type runState struct {
	progress Progress
	cancel   context.CancelFunc
}

// Defaults sized so a run that stalls is noticed in minutes and a healthy one
// never trips the sweeper.
const (
	DefaultBeat       = 20 * time.Second
	DefaultStaleAfter = 5 * time.Minute
	DefaultMaxResults = 200_000
)

// Start claims the release and runs the check in the background.
//
// It returns as soon as the claim is taken, so the caller's HTTP request
// finishes in milliseconds and the browser can start polling progress.
func (r *Runner) Start(ctx context.Context, req Request) (Progress, error) {
	if r.Catalog == nil || r.Source == nil || r.Recorder == nil {
		return Progress{}, errors.New("compliance: the runner is not configured")
	}

	// Two guards, and both are needed. The map stops a second run inside this
	// process; the row stops one in another Coordinator. Neither alone is
	// enough, and the row is the authority.
	r.mu.Lock()
	if r.running == nil {
		r.running = map[int64]*runState{}
	}
	if st, live := r.running[req.PackageID]; live {
		p := st.progress
		r.mu.Unlock()
		return p, ErrRunInFlight
	}
	r.mu.Unlock()

	if _, live, err := r.Recorder.ComplianceRunning(ctx, req.PackageID); err != nil {
		return Progress{}, err
	} else if live {
		return Progress{}, ErrRunInFlight
	}

	if err := r.Recorder.StartComplianceRun(ctx, req.RunID, req.PackageID, triggerOr(req.Trigger)); err != nil {
		return Progress{}, err
	}

	// Detached from the request's context on purpose: the run must outlive the
	// HTTP call that started it. Cancellation comes from Cancel(), and the
	// heartbeat is what makes a run whose process died recoverable.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	// The value returned to the caller is copied HERE, before the goroutine
	// exists. Reading state.progress after launching it would race the run's
	// first report - a race the caller cannot see and the race detector can.
	initial := Progress{
		RunID: req.RunID, Stage: StageFetching, Label: StageFetching.Label(),
		Started: time.Now().UTC(),
	}
	state := &runState{progress: initial, cancel: cancel}
	r.mu.Lock()
	r.running[req.PackageID] = state
	r.mu.Unlock()

	go func() {
		defer cancel()
		defer func() {
			r.mu.Lock()
			delete(r.running, req.PackageID)
			r.mu.Unlock()
		}()
		go r.beat(runCtx, req.RunID)
		r.execute(runCtx, req)
	}()

	return initial, nil
}

// Progress reports what a live run is doing, if one is.
func (r *Runner) Progress(packageID int64) (Progress, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.running[packageID]
	if !ok {
		return Progress{}, false
	}
	return st.progress, true
}

// Cancel stops a live run.
//
// The run then records itself as cancelled rather than vanishing: somebody
// pressed stop, and the next reader needs to know that is why there is no
// result rather than wondering whether it was ever started.
func (r *Runner) Cancel(packageID int64) bool {
	r.mu.Lock()
	st, ok := r.running[packageID]
	r.mu.Unlock()
	if !ok {
		return false
	}
	st.cancel()
	return true
}

// report updates the live progress. Cheap and lock-held only for the write, so
// a fast inner loop can call it per chart without contending with the poller.
func (r *Runner) report(packageID int64, stage Stage, done, total int, note string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.running[packageID]
	if !ok {
		return
	}
	st.progress.Stage = stage
	st.progress.Label = stage.Label()
	st.progress.Done = done
	st.progress.Total = total
	st.progress.Note = note
}

// beat touches the claim so the sweeper can tell a live run from a dead one.
func (r *Runner) beat(ctx context.Context, runID string) {
	interval := r.Beat
	if interval <= 0 {
		interval = DefaultBeat
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.Recorder.BeatComplianceRun(ctx, runID); err != nil {
				r.logger().Warn("compliance heartbeat failed",
					slog.String("run", runID), slog.String("error", err.Error()))
			}
		}
	}
}

// execute is the run itself.
func (r *Runner) execute(ctx context.Context, req Request) {
	log := r.logger().With(slog.String("run", req.RunID),
		slog.String("product", req.Product), slog.String("release", req.Release))
	started := time.Now().UTC()

	rel, determiner, cleanup, err := r.Source.Prepare(ctx, req, func(s Stage, done, total int, note string) {
		r.report(req.PackageID, s, done, total, note)
	})
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		r.failed(ctx, req, log, err)
		return
	}

	r.report(req.PackageID, StageEvaluating, 0, len(rel.Resources), "")
	eng := &Engine{
		Catalog:    r.Catalog(),
		Determiner: determiner,
		Waivers:    r.Waivers,
		MaxResults: orDefault(r.MaxResults, DefaultMaxResults),
	}
	run, err := Execute(ctx, eng, rel, started)
	if err != nil {
		r.failed(ctx, req, log, err)
		return
	}

	r.report(req.PackageID, StageRecording, 0, len(run.Results), "")
	// Recorded with a context that is not the run's: a cancelled or timed-out
	// run still has to write why it stopped, and using the dead context would
	// leave the release claimed with nothing recorded.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()

	if err := r.Recorder.RecordComplianceRun(writeCtx, req.RunID, req.PackageID, run); err != nil {
		r.failed(writeCtx, req, log, fmt.Errorf("recording the result: %w", err))
		return
	}
	log.Info("compliance run complete",
		slog.String("verdict", string(run.Verdict)),
		slog.Int("blocking", run.Counts.Blocking),
		slog.Int("warning", run.Counts.Warning),
		slog.Int("undecided", run.Counts.Error),
		slog.Int("pass", run.Counts.Pass),
		slog.Duration("took", time.Since(started)))
}

// failed records why a run stopped.
//
// Always recorded, never only logged. A release whose check failed and said so
// is a release somebody can act on; one that silently has no result is
// indistinguishable from one nobody has checked.
func (r *Runner) failed(ctx context.Context, req Request, log *slog.Logger, cause error) {
	reason := cause.Error()
	if errors.Is(cause, context.Canceled) {
		reason = "the check was cancelled"
	}
	log.Warn("compliance run failed", slog.String("error", reason))

	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := r.Recorder.FailComplianceRun(writeCtx, req.RunID, req.PackageID, reason); err != nil {
		log.Error("could not record the failure, so this release will look unchecked",
			slog.String("error", err.Error()))
	}
}

func (r *Runner) logger() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

func triggerOr(t string) string {
	if t == "" {
		return "manual"
	}
	return t
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}
