package security

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Running a registration, and recording what it did.
//
// # Why this is a Service method and not a Syncer
//
// Because it is not a background job. A sync is minutes against a scanner and
// has to survive the request that started it - hence a claim, a heartbeat, a
// goroutine and a progress endpoint. A registration is our own request count
// against a responsive service: a release of 157 images is 157 submissions at
// the configured concurrency plus three calls for the application version.
// Seconds, not minutes.
//
// So it runs on the request, returns the result, and the caller renders it.
// A user who presses Replicate finds out whether it worked before the page
// finishes reloading, which is the whole reason not to make it a job.

// Registrations is where a completed registration is recorded.
//
// A consumer-defined interface, like Recorder beside it: three calls, not the
// table behind them.
type Registrations interface {
	// Claim marks a release as being registered with one scanner, and returns
	// an error the caller reports as "already running" when somebody holds it.
	Claim(ctx context.Context, packageID int64, provider string) error
	// Record stores a finished registration.
	Record(ctx context.Context, packageID int64, reg Registration, log []SyncLogEntry) error
	// Fail records a registration that could not run at all, keeping whatever
	// the last good one knew.
	Fail(ctx context.Context, packageID int64, provider, reason string, log []SyncLogEntry) error
	// Beat renews a running replication's claim and stores where it has got to,
	// answering false once the row is no longer claimed - which is how a run
	// finds out it was stopped from another Coordinator.
	Beat(ctx context.Context, packageID int64, provider string, snapshot ProgressSnapshot) (bool, error)
	// Stop releases a running replication's claim from anywhere.
	Stop(ctx context.Context, packageID int64, provider string) (bool, error)
}

// ReplicateRequest is one release's registration with one scanner.
type ReplicateRequest struct {
	PackageID int64
	Scope     Scope
	// Release names the release, which is what the scanner's own grouping is
	// built from.
	Release ReleaseRef
	// Artifacts is what to register, assembled by the caller from the release's
	// own tree.
	Artifacts []ArtifactRef
}

// ReplicateResult is what happened, and the transcript of it.
type ReplicateResult struct {
	Registration Registration
	Log          []SyncLogEntry
	// Err is why the run stopped, where it did. Carried on the result rather
	// than returned beside it because a background run has nobody to return to,
	// and the transcript has to be able to say what ended it either way.
	Err error
}

// ErrNotRegistrable means the scanner does not need telling about artifacts.
//
// The ordinary answer for Xray, which indexes a repository. A caller turns it
// into "there is nothing to replicate for this scanner" rather than an error
// page - and the interface never offers the button for such a scanner at all.
var ErrNotRegistrable = errors.New("security: this scanner does not need artifacts registered with it")

// Replicate registers a release with one scanner and records the result.
//
// # Why the claim is taken here and released on every path
//
// Two people pressing Replicate on one release should see one operation, and
// the second should be told so rather than watching a button appear to do
// nothing. Every return below either records a result or records a failure, so
// a claim is never left held by a request that has ended - which would leave
// the release refusing the button until the sweep noticed.
func (s *Service) Replicate(ctx context.Context, req ReplicateRequest) (ReplicateResult, error) {
	registrar, err := s.registrarFor(ctx, req.Scope)
	if err != nil {
		return ReplicateResult{}, err
	}
	if err := s.registrations.Claim(ctx, req.PackageID, registrar.Name()); err != nil {
		return ReplicateResult{}, err
	}
	progress := s.openProgress(req, registrar.Name())
	defer s.closeProgress(req.PackageID, registrar.Name())
	return s.replicate(ctx, req, registrar, progress), nil
}

// StartReplicate claims a release and registers it in the BACKGROUND.
//
// # Why this is a job now, when it used to be a request
//
// Not because it got slower. Because its position was invisible. Running on the
// request meant the only thing that knew where a replication had got to was the
// request handling it, so a reader who pressed the button and reloaded the page
// - or navigated away and came back, or restarted the Coordinator - was shown
// the word "registering" and nothing else. That is indistinguishable from a
// hang, and it is the state somebody is in for the whole of a slow run.
//
// As a job it has the shape every other long operation here has: a claim, a
// heartbeat that also writes the position down, a progress reader and a stop.
// The panel is then drawn from the DATABASE rather than from the memory of
// whichever replica took the click, which is what makes it survive all four.
//
// Returns as soon as the claim is decided, never when the work is done.
func (s *Service) StartReplicate(ctx context.Context, req ReplicateRequest) error {
	registrar, err := s.registrarFor(ctx, req.Scope)
	if err != nil {
		return err
	}
	provider := registrar.Name()
	if err := s.registrations.Claim(ctx, req.PackageID, provider); err != nil {
		return err
	}

	progress := s.openProgress(req, provider)

	// Detached from the request's context, and bounded. A user who navigates
	// away must not cancel a replication half way through - that is the whole
	// point of making it a job - and an unbounded goroutine against an
	// unresponsive Anchore is one that never ends holding a claim nobody can
	// see.
	runCtx, cancel := context.WithTimeout(
		context.WithoutCancel(context.Background()), maxReplicationRun)

	s.replicating.Store(replicationKey(req.PackageID, provider), &runningReplication{
		progress: progress, cancel: cancel,
	})

	go func() {
		defer cancel()
		defer s.closeProgress(req.PackageID, provider)
		go s.beatReplication(runCtx, cancel, req.PackageID, provider, progress)
		s.replicate(runCtx, req, registrar, progress)
	}()
	return nil
}

// ReplicationProgress is the live position of a replication this replica is
// running, if it is the one running it.
//
// A miss is an ordinary answer - the work may be on another replica, or may
// have finished - and the caller falls back to the position stored on the row,
// which is durable. That fallback is the whole reason the heartbeat writes it.
func (s *Service) ReplicationProgress(packageID int64, provider string) (ProgressSnapshot, bool) {
	v, ok := s.replicating.Load(replicationKey(packageID, provider))
	if !ok {
		return ProgressSnapshot{}, false
	}
	return v.(*runningReplication).progress.SnapshotFull(), true
}

// CancelReplication stops a running replication, wherever it is running.
//
// Two halves, exactly as the sync has: the claim lives in the database, so
// releasing it stops a run on any replica at its next beat, and cancelling the
// local context makes it immediate for the common case where the reader is
// talking to the Coordinator doing the work.
func (s *Service) CancelReplication(
	ctx context.Context, packageID int64, provider string,
) (bool, error) {
	if s.registrations == nil {
		return false, nil
	}
	stopped, err := s.registrations.Stop(ctx, packageID, provider)
	if err != nil {
		return false, err
	}
	if v, ok := s.replicating.Load(replicationKey(packageID, provider)); ok {
		run := v.(*runningReplication)
		run.progress.Log(LogWarning, "The replication was stopped before it finished.")
		run.cancel()
		return true, nil
	}
	return stopped, nil
}

// maxReplicationRun bounds one background replication.
//
// Generous against the slowest plausible Anchore for a large release, and far
// short of forever: a run that hits it ends, records what it managed and
// releases the claim, rather than becoming a goroutine nobody can see.
const maxReplicationRun = 30 * time.Minute

// replicationBeat is how often a running replication renews its claim and
// writes its position down. Fast enough that a watcher sees the panel move,
// slow enough that a release of a hundred and fifty images is a couple of dozen
// small updates rather than one per image.
const replicationBeat = 3 * time.Second

// runningReplication is one replication this replica is running: what it will
// say about itself, and the handle that stops it.
type runningReplication struct {
	progress *SyncProgress
	cancel   context.CancelFunc
}

func replicationKey(packageID int64, provider string) string {
	return fmt.Sprintf("%d/%s", packageID, provider)
}

func (s *Service) openProgress(req ReplicateRequest, provider string) *SyncProgress {
	progress := NewProgress()
	progress.Log(LogInfo, fmt.Sprintf(
		"Replicating %s to %s: %d artifacts to consider.",
		labelOr(req.Release.Label, "this release"), ProviderLabel(provider),
		len(req.Artifacts)))
	return progress
}

func (s *Service) closeProgress(packageID int64, provider string) {
	s.replicating.Delete(replicationKey(packageID, provider))
}

// registrarFor resolves the scanner and refuses early where it cannot register.
func (s *Service) registrarFor(ctx context.Context, scope Scope) (Registrar, error) {
	if s.registrations == nil {
		return nil, fmt.Errorf(
			"security: no registration storage is configured on this Coordinator")
	}
	provider, err := s.provider(ctx, scope)
	if err != nil {
		return nil, err
	}
	if !provider.Enabled() {
		return nil, fmt.Errorf("%s is not enabled for this release", ProviderLabel(scope.Provider))
	}
	registrar, ok := provider.(Registrar)
	if !ok {
		return nil, ErrNotRegistrable
	}
	return registrar, nil
}

// beatReplication renews the claim and writes the position down until the run
// ends, and stops the run if the claim has gone.
func (s *Service) beatReplication(
	ctx context.Context, cancel context.CancelFunc,
	packageID int64, provider string, progress *SyncProgress,
) {
	ticker := time.NewTicker(replicationBeat)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			held, err := s.registrations.Beat(ctx, packageID, provider, progress.SnapshotFull())
			if err != nil {
				// A database that cannot be reached is not a claim that was
				// taken away. Losing a beat is what the stale window has slack
				// for, and abandoning a run of a hundred and fifty images over
				// one failed UPDATE is the worse trade.
				s.log.Warn("could not renew the security replication claim",
					"package", packageID, "provider", provider, "error", err)
				continue
			}
			if !held {
				s.log.Info("security replication claim released elsewhere; stopping",
					"package", packageID, "provider", provider)
				cancel()
				return
			}
		}
	}
}

// replicate does the work and records the outcome. Shared by the synchronous
// and background entry points so the two cannot record different things.
func (s *Service) replicate(
	ctx context.Context, req ReplicateRequest, registrar Registrar, progress *SyncProgress,
) ReplicateResult {
	provider := registrar.Name()

	reg, err := registrar.Register(ctx, dedupeRefs(req.Artifacts), RegisterOptions{
		Release:  req.Release,
		Progress: progress,
	})
	if err != nil {
		reason := err.Error()
		progress.Log(LogError, "The replication stopped before it finished: "+reason)
		// A detached context, because a registration that ran out of time has
		// an expired one - and recording the failure through it would leave the
		// row claimed and the reader watching a spinner over a run that ended.
		writeCtx, done := writeContext(ctx)
		defer done()
		if failErr := s.registrations.Fail(
			writeCtx, req.PackageID, provider, reason, progress.Entries()); failErr != nil {
			s.log.Error("could not record a failed security replication",
				"package", req.PackageID, "provider", provider, "error", failErr)
		}
		return ReplicateResult{Log: progress.Entries(), Err: err}
	}

	progress.Log(closingLevel(reg), closingLine(reg))

	writeCtx, done := writeContext(ctx)
	defer done()
	if err := s.registrations.Record(
		writeCtx, req.PackageID, reg, progress.Entries()); err != nil {
		// The work HAPPENED. Failing the request now would tell a user their
		// images were not registered when they were, and the next press would
		// find every one of them already known - so the result is returned and
		// the storage failure is logged.
		s.log.Error("could not record a security replication",
			"package", req.PackageID, "provider", provider, "error", err)
	}

	s.log.Info("security replication complete",
		"package", req.PackageID, "provider", provider,
		"expected", reg.Expected, "submitted", reg.Submitted,
		"alreadyKnown", reg.AlreadyKnown, "associated", reg.Associated,
		"state", reg.State)

	return ReplicateResult{Registration: reg, Log: progress.Entries()}
}

// closingLevel is how loudly the transcript ends.
func closingLevel(reg Registration) string {
	switch reg.State {
	case RegistrationComplete:
		return LogSuccess
	case RegistrationFailed:
		return LogError
	default:
		return LogWarning
	}
}

// closingLine is the sentence a reader looks for at the bottom of the
// transcript, and it says what to do next rather than only what happened.
//
// "Registered" is not the end of anything from the reader's point of view: the
// analysis they actually want has not started, and a closing line that stopped
// at the good news would leave them waiting for a page to change on its own.
func closingLine(reg Registration) string {
	switch {
	case reg.Expected == 0:
		return "There was nothing to replicate: this release holds no images Anchore can analyse."
	case reg.State == RegistrationFailed:
		return fmt.Sprintf("Replication failed. None of the %d images were registered. %s",
			reg.Expected, reg.FirstFailure())
	case reg.State == RegistrationPartial:
		return fmt.Sprintf(
			"Replication finished with %d of %d images registered. %s "+
				"Replicate again once the rest have been transferred.",
			reg.Associated, reg.Expected, reg.FirstFailure())
	case reg.Submitted == 0:
		return fmt.Sprintf(
			"All %d images were already registered. Nothing was submitted. "+
				"%d have been analysed so far; sync this release to collect their results.",
			reg.Expected, reg.Analysed)
	default:
		return fmt.Sprintf(
			"Replication finished. %d images submitted for analysis and %d registered in total. "+
				"Analysis runs on the scanner's own schedule - sync this release to collect "+
				"results as they finish.",
			reg.Submitted, reg.Associated)
	}
}

// WithRegistrations attaches the registration store.
func (s *Service) WithRegistrations(r Registrations) *Service {
	s.registrations = r
	return s
}

// Registrable reports whether a scanner has to be told about artifacts.
//
// Asked by the interface to decide whether to offer the button at all, and
// answered without any I/O: it is a property of the provider's type, not of the
// release. Xray indexes a repository and answers false.
func (s *Service) Registrable(ctx context.Context, scope Scope) bool {
	provider, err := s.provider(ctx, scope)
	if err != nil {
		return false
	}
	_, ok := provider.(Registrar)
	return ok
}
