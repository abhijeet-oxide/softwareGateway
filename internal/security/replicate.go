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
	if s.registrations == nil {
		return ReplicateResult{}, fmt.Errorf(
			"security: no registration storage is configured on this Coordinator")
	}

	provider, err := s.provider(ctx, req.Scope)
	if err != nil {
		return ReplicateResult{}, err
	}
	if !provider.Enabled() {
		return ReplicateResult{}, fmt.Errorf("%s is not enabled for this release", ProviderLabel(req.Scope.Provider))
	}
	registrar, ok := provider.(Registrar)
	if !ok {
		return ReplicateResult{}, ErrNotRegistrable
	}

	if err := s.registrations.Claim(ctx, req.PackageID, provider.Name()); err != nil {
		return ReplicateResult{}, err
	}

	progress := &SyncProgress{stages: map[string]stagePosition{}, startedAt: time.Now()}
	progress.Log(LogInfo, fmt.Sprintf(
		"Replicating %s to %s: %d artifacts to consider.",
		labelOr(req.Release.Label, "this release"), ProviderLabel(provider.Name()),
		len(req.Artifacts)))

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
			writeCtx, req.PackageID, provider.Name(), reason, progress.Entries()); failErr != nil {
			s.log.Error("could not record a failed security replication",
				"package", req.PackageID, "provider", provider.Name(), "error", failErr)
		}
		return ReplicateResult{Log: progress.Entries()}, err
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
			"package", req.PackageID, "provider", provider.Name(), "error", err)
	}

	s.log.Info("security replication complete",
		"package", req.PackageID, "provider", provider.Name(),
		"expected", reg.Expected, "submitted", reg.Submitted,
		"alreadyKnown", reg.AlreadyKnown, "associated", reg.Associated,
		"state", reg.State)

	return ReplicateResult{Registration: reg, Log: progress.Entries()}, nil
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
