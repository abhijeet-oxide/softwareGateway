package statemachine

import (
	"errors"
	"testing"
)

func TestApplyRejectsUnmodelledTransition(t *testing.T) {
	// The property the package exists for: an unmodelled (from, event) pair is
	// an error, never a silent no-op and never a fall-through to `from`.
	_, err := Transfer.Apply(TransferSucceeded, TransferCancelRequested)
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("cancelling a succeeded transfer must be illegal, got err=%v", err)
	}

	_, err = Job.Apply(JobSucceeded, JobRetryRequested)
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("retrying a succeeded job must be illegal, got err=%v", err)
	}
}

func TestApplyReturnsZeroValueOnError(t *testing.T) {
	// A caller that ignores the error must not receive a plausible-looking
	// state it might then persist.
	got, err := Job.Apply(JobSucceeded, JobLeasedE)
	if err == nil {
		t.Fatal("expected error")
	}
	if got != JobStateZero {
		t.Fatalf("expected zero state on error, got %q", got)
	}
}

func TestJobHappyPaths(t *testing.T) {
	for _, tc := range []struct {
		name  string
		from  JobState
		event JobEvent
		want  JobState
	}{
		{"wave 0 starts leasable", JobStateZero, JobCreatedWave0, JobPending},
		{"wave N starts blocked", JobStateZero, JobCreatedWaveN, JobBlocked},
		{"wave advance unblocks", JobBlocked, JobWaveAdvanced, JobPending},
		{"lease", JobPending, JobLeasedE, JobLeased},
		{"bytes moved", JobLeased, JobCompletedMoved, JobSucceeded},
		{"already present", JobLeased, JobCompletedExists, JobSkipped},
		{"mounted", JobLeased, JobCompletedMounted, JobSkipped},
		{"retryable failure requeues", JobLeased, JobRetryable, JobPending},
		{"exhausted failure is terminal", JobLeased, JobTerminal, JobFailed},
		{"lease expiry requeues", JobLeased, JobLeaseExpired, JobPending},
		{"failed is retryable by request", JobFailed, JobRetryRequested, JobPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Job.Apply(tc.from, tc.event)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSkippedIsTerminalSuccess(t *testing.T) {
	// docs/design/10 section 4: `skipped` is a first-class success, because
	// sum(size_bytes) where state='skipped' is the bandwidth-saved metric.
	if !Job.IsTerminal(JobSkipped) {
		t.Fatal("skipped must be terminal")
	}
	if !Job.IsTerminal(JobSucceeded) {
		t.Fatal("succeeded must be terminal")
	}
}

func TestFailedIsNotTerminal(t *testing.T) {
	// `failed` is retryable in every machine that has a retry edge. Marking it
	// terminal would make transfers:retry impossible.
	if Job.IsTerminal(JobFailed) {
		t.Fatal("job failed must not be terminal — it is retryable")
	}
	if Transfer.IsTerminal(TransferFailed) {
		t.Fatal("transfer failed must not be terminal — it is retryable")
	}
	if Package.IsTerminal(PackageFailed) {
		t.Fatal("package failed must not be terminal — it is retryable")
	}
}

func TestEmptyPlanSucceeds(t *testing.T) {
	// Everything already deduplicated away: success with zero jobs, not an
	// error and not a transfer stuck in ready forever.
	got, err := Transfer.Apply(TransferPlanning, TransferPlanEmpty)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != TransferSucceeded {
		t.Fatalf("an empty plan must succeed, got %q", got)
	}
}

func TestCancelIsTwoPhase(t *testing.T) {
	// `cancelling` exists because leased jobs take up to one heartbeat to
	// abort. A direct running -> cancelled edge would hide that window.
	if Transfer.Can(TransferRunning, TransferDrained) {
		t.Fatal("running must not jump straight to cancelled")
	}
	mid, err := Transfer.Apply(TransferRunning, TransferCancelRequested)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mid != TransferCancelling {
		t.Fatalf("got %q, want cancelling", mid)
	}
	end, err := Transfer.Apply(mid, TransferDrained)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if end != TransferCancelled {
		t.Fatalf("got %q, want cancelled", end)
	}
}

func TestVerificationFailedNeverRetries(t *testing.T) {
	// docs/design/08 section 8 / docs/design/10 section 5.
	// `failed` means the signature did not check out — a security event.
	// `error` means verification could not run — an availability event.
	// Only `error` retries; retrying a bad signature just repeats the alert.
	if Verification.Can(VerificationFailed, VerificationRetry) {
		t.Fatal("a definitively invalid signature must not be retryable")
	}
	got, err := Verification.Apply(VerificationError, VerificationRetry)
	if err != nil {
		t.Fatalf("unavailable verification must be retryable: %v", err)
	}
	if got != VerificationPending {
		t.Fatalf("got %q, want pending", got)
	}
}

func TestVerificationFailedAndErrorAreDistinct(t *testing.T) {
	if VerificationFailed == VerificationError {
		t.Fatal("failed and error must be distinct states")
	}
	if !Verification.IsTerminal(VerificationFailed) {
		t.Fatal("failed is terminal")
	}
	if Verification.IsTerminal(VerificationError) {
		t.Fatal("error is not terminal — it retries")
	}
}

func TestPackageSupersedableFromAnyNonTerminalState(t *testing.T) {
	// A vendor can re-push a tag at any moment; every non-terminal state must
	// accept supersession or discovery would be unable to record it.
	for _, s := range []PackageState{
		PackageDiscovered, PackageQueued, PackageTransferring,
		PackageTransferred, PackageVerifying, PackageFailed,
	} {
		if !Package.Can(s, PackageSupersededE) {
			t.Errorf("package state %q must be supersedable", s)
		}
	}
}

func TestNoEdgesLeaveTerminalStates(t *testing.T) {
	// Invariant S5: a terminal state is never left. Enumerating every event
	// against every terminal state is the only way to be sure a later edit
	// does not quietly add an escape hatch.
	jobEvents := []JobEvent{
		JobCreatedWave0, JobCreatedWaveN, JobWaveAdvanced, JobLeasedE,
		JobCompletedMoved, JobCompletedExists, JobCompletedMounted,
		JobRetryable, JobTerminal, JobLeaseExpired, JobBlobUnknown,
		JobRetryRequested, JobCancelledE,
	}
	for _, s := range []JobState{JobSucceeded, JobSkipped, JobCancelled} {
		for _, e := range jobEvents {
			if Job.Can(s, e) {
				t.Errorf("job: terminal %q must not accept %q", s, e)
			}
		}
	}

	transferEvents := []TransferEvent{
		TransferCreated, TransferPlanningStarted, TransferPlanCompleted,
		TransferPlanFailed, TransferPlanEmpty, TransferFirstJobLeased,
		TransferPauseRequested, TransferResumeRequested, TransferAllWavesDrained,
		TransferJobFailed, TransferVerifyPassed, TransferVerifyFailed,
		TransferRetryRequested, TransferCancelRequested, TransferDrained,
		TransferSkipVerify,
	}
	for _, s := range []TransferState{TransferSucceeded, TransferCancelled} {
		for _, e := range transferEvents {
			if Transfer.Can(s, e) {
				t.Errorf("transfer: terminal %q must not accept %q", s, e)
			}
		}
	}
}
