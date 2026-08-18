package statemachine

// The concrete lifecycles from docs/design/10-state-machines.md.
//
// These declarations are the single source of truth: the CHECK constraints in
// db/migrations must enumerate exactly these states, and machines_test.go
// asserts the terminal sets and the invariants in section 8.

// ---------------------------------------------------------------------------
// Job (layer) lifecycle — docs/design/10-state-machines.md section 4.
// The highest-volume machine: hundreds of thousands of rows.
// ---------------------------------------------------------------------------

type JobState string

const (
	JobBlocked   JobState = "blocked"   // wave >= 1 at creation
	JobPending   JobState = "pending"   // leasable
	JobLeased    JobState = "leased"    // held by a worker
	JobSucceeded JobState = "succeeded" // bytes moved
	JobSkipped   JobState = "skipped"   // already present or mounted — zero bytes
	JobFailed    JobState = "failed"    // attempts exhausted; retryable by request
	JobCancelled JobState = "cancelled"
)

type JobEvent string

const (
	JobCreatedWave0     JobEvent = "CreatedWave0"
	JobCreatedWaveN     JobEvent = "CreatedWaveN"
	JobWaveAdvanced     JobEvent = "WaveAdvanced"
	JobLeasedE          JobEvent = "Leased"
	JobCompletedMoved   JobEvent = "CompletedTransferred"
	JobCompletedExists  JobEvent = "CompletedExists"
	JobCompletedMounted JobEvent = "CompletedMounted"
	JobRetryable        JobEvent = "FailedRetryable"
	JobTerminal         JobEvent = "FailedTerminal"
	JobLeaseExpired     JobEvent = "LeaseExpired"
	JobBlobUnknown      JobEvent = "BlobUnknownOnManifest"
	JobRetryRequested   JobEvent = "RetryRequested"
	JobCancelledE       JobEvent = "Cancelled"
)

// JobStateZero is the pseudo-state a job occupies before it exists, so that
// creation is modelled as a transition rather than an assignment.
const JobStateZero JobState = ""

// Job is the job/layer lifecycle.
var Job = New[JobState, JobEvent]("job",
	[]JobState{JobSucceeded, JobSkipped, JobCancelled},

	Transition[JobState, JobEvent]{JobStateZero, JobCreatedWave0, JobPending},
	Transition[JobState, JobEvent]{JobStateZero, JobCreatedWaveN, JobBlocked},

	Transition[JobState, JobEvent]{JobBlocked, JobWaveAdvanced, JobPending},
	Transition[JobState, JobEvent]{JobPending, JobLeasedE, JobLeased},

	// Three completion outcomes. `skipped` is a first-class success, not an
	// exception: sum(size_bytes) where state='skipped' IS the bandwidth-saved
	// metric. Folding it into `succeeded` would make the system's most
	// valuable optimization invisible.
	Transition[JobState, JobEvent]{JobLeased, JobCompletedMoved, JobSucceeded},
	Transition[JobState, JobEvent]{JobLeased, JobCompletedExists, JobSkipped},
	Transition[JobState, JobEvent]{JobLeased, JobCompletedMounted, JobSkipped},

	Transition[JobState, JobEvent]{JobLeased, JobRetryable, JobPending},
	Transition[JobState, JobEvent]{JobLeased, JobTerminal, JobFailed},

	// The reaper resolves an expired lease to one or the other depending on
	// attempts; both edges must exist.
	Transition[JobState, JobEvent]{JobLeased, JobLeaseExpired, JobPending},

	// Stale-placement self-heal: a manifest push returned BLOB_UNKNOWN, so the
	// blob jobs it depends on are requeued.
	Transition[JobState, JobEvent]{JobLeased, JobBlobUnknown, JobPending},

	Transition[JobState, JobEvent]{JobFailed, JobRetryRequested, JobPending},

	Transition[JobState, JobEvent]{JobPending, JobCancelledE, JobCancelled},
	Transition[JobState, JobEvent]{JobBlocked, JobCancelledE, JobCancelled},
	Transition[JobState, JobEvent]{JobLeased, JobCancelledE, JobCancelled},
)

// ---------------------------------------------------------------------------
// Transfer / promotion lifecycle — section 3.
// One machine, not two: a promotion is a transfer whose origin is a target
// repository, so it uses these states unchanged.
// ---------------------------------------------------------------------------

type TransferState string

const (
	TransferPending  TransferState = "pending"
	TransferPlanning TransferState = "planning"
	TransferReady    TransferState = "ready"
	TransferRunning  TransferState = "running"
	TransferPaused   TransferState = "paused"
	// TransferSyncing is a delegated transfer waiting on the REGISTRY.
	//
	// It exists because a delegated transfer creates no jobs. Without it such a
	// transfer would sit in `running` with an empty job set, and the wave-drain
	// check would settle it immediately — reporting success at the moment we
	// had asked Quay to start and before anything had happened.
	TransferSyncing   TransferState = "syncing"
	TransferVerifying TransferState = "verifying"
	TransferSucceeded TransferState = "succeeded"
	// TransferDiverged is terminal, and is neither success nor failure.
	//
	// The registry reported a completed sync, we walked the destination, and
	// the tag resolves to a DIFFERENT digest than the one we asked for. That is
	// normal for a mirror whose upstream tag moved: a fact worth recording, not
	// an error worth paging about. Folding it into `succeeded` would lose the
	// only signal that what shipped is not what was requested; folding it into
	// `failed` would page somebody about a mirror working as designed.
	TransferDiverged   TransferState = "diverged"
	TransferFailed     TransferState = "failed"
	TransferCancelling TransferState = "cancelling"
	TransferCancelled  TransferState = "cancelled"
)

type TransferEvent string

const (
	TransferCreated         TransferEvent = "Created"
	TransferPlanningStarted TransferEvent = "PlanningStarted"
	TransferPlanCompleted   TransferEvent = "PlanCompleted"
	TransferPlanFailed      TransferEvent = "PlanFailed"
	TransferPlanEmpty       TransferEvent = "PlanEmpty"
	TransferFirstJobLeased  TransferEvent = "FirstJobLeased"
	TransferPauseRequested  TransferEvent = "PauseRequested"
	TransferResumeRequested TransferEvent = "ResumeRequested"
	TransferAllWavesDrained TransferEvent = "AllWavesDrained"
	TransferJobFailed       TransferEvent = "JobFailedTerminally"
	TransferVerifyPassed    TransferEvent = "VerificationPassed"
	TransferVerifyFailed    TransferEvent = "VerificationFailed"
	TransferRetryRequested  TransferEvent = "RetryRequested"
	TransferCancelRequested TransferEvent = "CancelRequested"
	TransferDrained         TransferEvent = "InFlightDrained"
	TransferSkipVerify      TransferEvent = "VerificationDisabled"

	// Delegated replication (docs/design/18 §6).
	TransferSyncRequested TransferEvent = "SyncRequested"
	TransferSyncSucceeded TransferEvent = "SyncSucceeded"
	TransferSyncDiverged  TransferEvent = "SyncDiverged"
	TransferSyncFailed    TransferEvent = "SyncFailed"
)

const TransferStateZero TransferState = ""

// Transfer is the transfer and promotion lifecycle.
var Transfer = New[TransferState, TransferEvent]("transfer",
	[]TransferState{TransferSucceeded, TransferDiverged, TransferCancelled},

	Transition[TransferState, TransferEvent]{TransferStateZero, TransferCreated, TransferPending},
	Transition[TransferState, TransferEvent]{TransferPending, TransferPlanningStarted, TransferPlanning},
	Transition[TransferState, TransferEvent]{TransferPlanning, TransferPlanCompleted, TransferReady},
	Transition[TransferState, TransferEvent]{TransferPlanning, TransferPlanFailed, TransferFailed},

	// Everything already present: the correct outcome is success with zero
	// jobs, not an error and not a transfer stuck in `ready` forever waiting
	// for work that will never be created. Common for promotion.
	Transition[TransferState, TransferEvent]{TransferPlanning, TransferPlanEmpty, TransferSucceeded},

	Transition[TransferState, TransferEvent]{TransferReady, TransferFirstJobLeased, TransferRunning},

	Transition[TransferState, TransferEvent]{TransferRunning, TransferPauseRequested, TransferPaused},
	Transition[TransferState, TransferEvent]{TransferReady, TransferPauseRequested, TransferPaused},
	Transition[TransferState, TransferEvent]{TransferPaused, TransferResumeRequested, TransferRunning},

	Transition[TransferState, TransferEvent]{TransferRunning, TransferAllWavesDrained, TransferVerifying},
	Transition[TransferState, TransferEvent]{TransferRunning, TransferSkipVerify, TransferSucceeded},
	Transition[TransferState, TransferEvent]{TransferRunning, TransferJobFailed, TransferFailed},

	Transition[TransferState, TransferEvent]{TransferVerifying, TransferVerifyPassed, TransferSucceeded},
	Transition[TransferState, TransferEvent]{TransferVerifying, TransferVerifyFailed, TransferFailed},

	// Delegated replication. `planning` is where the branch happens: a target
	// whose registry fetches for itself produces no jobs, so instead of
	// PlanCompleted it emits SyncRequested and waits to be told what the
	// registry did.
	//
	// Note the absence of any transition INTO a byte-carrying state from here.
	// A delegated transfer never becomes `running`, never has a wave, and never
	// acquires a progress denominator — which is what makes it impossible for
	// one to grow a percentage by accident.
	Transition[TransferState, TransferEvent]{TransferPlanning, TransferSyncRequested, TransferSyncing},
	Transition[TransferState, TransferEvent]{TransferSyncing, TransferSyncSucceeded, TransferSucceeded},
	Transition[TransferState, TransferEvent]{TransferSyncing, TransferSyncDiverged, TransferDiverged},
	Transition[TransferState, TransferEvent]{TransferSyncing, TransferSyncFailed, TransferFailed},
	// A sync can be asked for again after it failed, which is the delegated
	// equivalent of retrying failed jobs.
	Transition[TransferState, TransferEvent]{TransferFailed, TransferSyncRequested, TransferSyncing},
	Transition[TransferState, TransferEvent]{TransferSyncing, TransferCancelRequested, TransferCancelling},

	Transition[TransferState, TransferEvent]{TransferFailed, TransferRetryRequested, TransferReady},

	Transition[TransferState, TransferEvent]{TransferPending, TransferCancelRequested, TransferCancelling},
	Transition[TransferState, TransferEvent]{TransferPlanning, TransferCancelRequested, TransferCancelling},
	Transition[TransferState, TransferEvent]{TransferReady, TransferCancelRequested, TransferCancelling},
	Transition[TransferState, TransferEvent]{TransferRunning, TransferCancelRequested, TransferCancelling},
	Transition[TransferState, TransferEvent]{TransferPaused, TransferCancelRequested, TransferCancelling},
	Transition[TransferState, TransferEvent]{TransferVerifying, TransferCancelRequested, TransferCancelling},
	Transition[TransferState, TransferEvent]{TransferFailed, TransferCancelRequested, TransferCancelling},

	// `cancelling` is a real state, not a flag: leased jobs take up to one
	// heartbeat to abort. Without it a cancel either looks instantaneous while
	// bytes are still moving, or looks stuck.
	Transition[TransferState, TransferEvent]{TransferCancelling, TransferDrained, TransferCancelled},
)

// ---------------------------------------------------------------------------
// Package lifecycle — section 2.
// ---------------------------------------------------------------------------

type PackageState string

const (
	PackageDiscovered         PackageState = "discovered"
	PackageQueued             PackageState = "queued"
	PackageTransferring       PackageState = "transferring"
	PackageTransferred        PackageState = "transferred"
	PackageVerifying          PackageState = "verifying"
	PackageVerified           PackageState = "verified"
	PackageVerificationFailed PackageState = "verification_failed"
	PackageFailed             PackageState = "failed"
	PackageSuperseded         PackageState = "superseded"
)

type PackageEvent string

const (
	PackageDiscoveredE       PackageEvent = "Discovered"
	PackageTransferRequested PackageEvent = "TransferRequested"
	PackageTransferStarted   PackageEvent = "TransferStarted"
	PackageAllSucceeded      PackageEvent = "AllTransfersSucceeded"
	PackageTransferFailed    PackageEvent = "TransferFailed"
	PackageVerifyStarted     PackageEvent = "VerificationStarted"
	PackageVerifyPassed      PackageEvent = "VerificationPassed"
	PackageVerifyFailed      PackageEvent = "VerificationFailed"
	PackageRetryRequested    PackageEvent = "RetryRequested"
	PackageSupersededE       PackageEvent = "Superseded"
	PackageVerifySkipped     PackageEvent = "VerificationDisabled"
)

const PackageStateZero PackageState = ""

// Package is the software-package lifecycle.
var Package = New[PackageState, PackageEvent]("package",
	[]PackageState{PackageVerified, PackageVerificationFailed, PackageSuperseded},

	Transition[PackageState, PackageEvent]{PackageStateZero, PackageDiscoveredE, PackageDiscovered},
	Transition[PackageState, PackageEvent]{PackageDiscovered, PackageTransferRequested, PackageQueued},
	Transition[PackageState, PackageEvent]{PackageQueued, PackageTransferStarted, PackageTransferring},
	Transition[PackageState, PackageEvent]{PackageTransferring, PackageAllSucceeded, PackageTransferred},
	Transition[PackageState, PackageEvent]{PackageTransferring, PackageTransferFailed, PackageFailed},
	Transition[PackageState, PackageEvent]{PackageTransferred, PackageVerifyStarted, PackageVerifying},
	Transition[PackageState, PackageEvent]{PackageTransferred, PackageVerifySkipped, PackageVerified},
	Transition[PackageState, PackageEvent]{PackageVerifying, PackageVerifyPassed, PackageVerified},
	Transition[PackageState, PackageEvent]{PackageVerifying, PackageVerifyFailed, PackageVerificationFailed},
	Transition[PackageState, PackageEvent]{PackageFailed, PackageRetryRequested, PackageQueued},
	// Re-verify without re-transferring.
	Transition[PackageState, PackageEvent]{PackageVerificationFailed, PackageRetryRequested, PackageVerifying},

	Transition[PackageState, PackageEvent]{PackageDiscovered, PackageSupersededE, PackageSuperseded},
	Transition[PackageState, PackageEvent]{PackageQueued, PackageSupersededE, PackageSuperseded},
	Transition[PackageState, PackageEvent]{PackageTransferring, PackageSupersededE, PackageSuperseded},
	Transition[PackageState, PackageEvent]{PackageTransferred, PackageSupersededE, PackageSuperseded},
	Transition[PackageState, PackageEvent]{PackageVerifying, PackageSupersededE, PackageSuperseded},
	Transition[PackageState, PackageEvent]{PackageFailed, PackageSupersededE, PackageSuperseded},
)

// ---------------------------------------------------------------------------
// Verification lifecycle — section 5.
// ---------------------------------------------------------------------------

type VerificationState string

const (
	VerificationPending VerificationState = "pending"
	VerificationRunning VerificationState = "running"
	VerificationPassed  VerificationState = "passed"
	VerificationFailed  VerificationState = "failed" // signature did not check out — SECURITY event
	VerificationError   VerificationState = "error"  // could not run — AVAILABILITY event
	VerificationSkipped VerificationState = "skipped"
)

type VerificationEvent string

const (
	VerificationRequested   VerificationEvent = "Requested"
	VerificationStarted     VerificationEvent = "Started"
	VerificationSkipPolicy  VerificationEvent = "SkippedByPolicy"
	VerificationSigsValid   VerificationEvent = "SignaturesValid"
	VerificationSigsInvalid VerificationEvent = "SignaturesInvalid"
	VerificationUnavailable VerificationEvent = "VerificationUnavailable"
	VerificationRetry       VerificationEvent = "RetryRequested"
)

const VerificationStateZero VerificationState = ""

// Verification is the verification lifecycle.
//
// `failed` and `error` are deliberately distinct and this is the most
// important distinction in the machine set: collapsing them would make a
// Sigstore outage indistinguishable from a supply-chain attack. Note there is
// no edge out of `failed` — retrying a signature that definitively does not
// verify accomplishes nothing except repeating the alert.
var Verification = New[VerificationState, VerificationEvent]("verification",
	[]VerificationState{VerificationPassed, VerificationFailed, VerificationSkipped},

	Transition[VerificationState, VerificationEvent]{VerificationStateZero, VerificationRequested, VerificationPending},
	Transition[VerificationState, VerificationEvent]{VerificationPending, VerificationStarted, VerificationRunning},
	Transition[VerificationState, VerificationEvent]{VerificationPending, VerificationSkipPolicy, VerificationSkipped},
	Transition[VerificationState, VerificationEvent]{VerificationRunning, VerificationSigsValid, VerificationPassed},
	Transition[VerificationState, VerificationEvent]{VerificationRunning, VerificationSigsInvalid, VerificationFailed},
	Transition[VerificationState, VerificationEvent]{VerificationRunning, VerificationUnavailable, VerificationError},
	Transition[VerificationState, VerificationEvent]{VerificationError, VerificationRetry, VerificationPending},
)
