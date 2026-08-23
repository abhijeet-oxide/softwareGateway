package security

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeRecorder captures what a sync decided.
type fakeRecorder struct {
	mu        sync.Mutex
	claimed   int
	owner     string
	recorded  []PackageResult
	failed    []string
	failedLog []SyncLogEntry
	claimErr  error
	// held is what the heartbeat answers: false stands a run down.
	held    bool
	beats   int
	stopped int
}

func (f *fakeRecorder) Claim(_ context.Context, _ int64, owner string, _ time.Duration) error {
	if f.claimErr != nil {
		return f.claimErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimed++
	f.owner = owner
	f.held = true
	return nil
}

func (f *fakeRecorder) Heartbeat(_ context.Context, _ int64, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beats++
	return f.held, nil
}

func (f *fakeRecorder) Stop(context.Context, int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped++
	held := f.held
	f.held = false
	return held, nil
}
func (f *fakeRecorder) Record(_ context.Context, res PackageResult) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recorded = append(f.recorded, res)
	return nil
}
func (f *fakeRecorder) Fail(_ context.Context, _ int64, reason string, log []SyncLogEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed = append(f.failed, reason)
	f.failedLog = log
	return nil
}

// outcomes is how many endings this recorder has seen, read safely from a test
// polling while the sync runs.
func (f *fakeRecorder) outcomes() (recorded, failed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.recorded), len(f.failed)
}

// stubProvider answers with whatever it is given.
type stubProvider struct {
	enabled bool
	byRef   map[string]Report
	err     error
}

func (s stubProvider) Name() string  { return "jfrog-xray" }
func (s stubProvider) Enabled() bool { return s.enabled }
func (s stubProvider) Scan(_ context.Context, refs []ArtifactRef, _ ScanOptions) ([]Report, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := make([]Report, 0, len(refs))
	for _, ref := range refs {
		if r, ok := s.byRef[ref.Digest]; ok {
			r.Artifact = ref
			out = append(out, r)
			continue
		}
		out = append(out, Report{Artifact: ref, Status: StatusUnavailable, Provider: "jfrog-xray",
			Message: "JFrog Xray could not be reached."})
	}
	return out, nil
}

type stubResolver struct{ p Provider }

func (s stubResolver) ProviderFor(context.Context, string, string) (Provider, error) { return s.p, nil }

func syncerFor(t *testing.T, p Provider) (*Syncer, *fakeRecorder) {
	t.Helper()
	rec := &fakeRecorder{}
	return NewSyncer(NewService(stubResolver{p}, nil, nil), rec, nil), rec
}

func waitFor(t *testing.T, done func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if done() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the sync never finished")
}

func syncRequest() SyncRequest {
	return SyncRequest{
		PackageID: 1, Label: "25.7.2131",
		Scope:     Scope{Product: "cfx", Repository: "cfx-jfrog-lab", Role: "target", Provider: "jfrog-xray"},
		Artifacts: []ArtifactRef{{Name: "cfx-main", Digest: "sha256:a", Kind: "image"}},
	}
}

func TestSyncRecordsWhatItFound(t *testing.T) {
	report := Report{Status: StatusScanned, Provider: "jfrog-xray", Findings: []Finding{
		{CVE: "CVE-1", Severity: SeverityHigh, Fixable: true,
			Component: Component{ID: "deb://openssl", Name: "openssl"}},
	}}
	report.Recount()

	s, rec := syncerFor(t, stubProvider{enabled: true, byRef: map[string]Report{"sha256:a": report}})
	if _, err := s.Start(t.Context(), syncRequest()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, func() bool { r, f := rec.outcomes(); return r > 0 || f > 0 })

	if len(rec.failed) > 0 {
		t.Fatalf("recorded a failure: %v", rec.failed)
	}
	got := rec.recorded[0]
	if got.Posture.Counts.Total != 1 || got.Repository != "cfx-jfrog-lab" {
		t.Errorf("result = %+v", got)
	}
	// The claim is released the moment the goroutine ends, so a second sync
	// can start.
	waitFor(t, func() bool { return !s.Running(1) })
}

// A sync that reached the scanner and got results for NOTHING is not a success
// with zero findings. Recorded as one it would put a clean-looking row in a
// listing for a release nobody scanned.
func TestSyncWithNoResultsIsAFailure(t *testing.T) {
	s, rec := syncerFor(t, stubProvider{enabled: true, byRef: map[string]Report{}})
	if _, err := s.Start(t.Context(), syncRequest()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, func() bool { r, f := rec.outcomes(); return r > 0 || f > 0 })

	if len(rec.recorded) > 0 {
		t.Fatalf("recorded a success with no results: %+v", rec.recorded[0])
	}
	if len(rec.failed) != 1 {
		t.Fatalf("failures = %v", rec.failed)
	}
	// The reason carries the scanner's own words, which is the only part that
	// tells anybody what to do.
	if !contains(rec.failed[0], "could not be reached") {
		t.Errorf("reason = %q, want the scanner's message", rec.failed[0])
	}
}

// A release with nothing scannable in it is an honest zero and stays a success.
func TestSyncWithNothingScannableSucceeds(t *testing.T) {
	s, rec := syncerFor(t, stubProvider{enabled: true, byRef: map[string]Report{
		"sha256:a": {Status: StatusUnsupported, Provider: "jfrog-xray"},
	}})
	if _, err := s.Start(t.Context(), syncRequest()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, func() bool { r, f := rec.outcomes(); return r > 0 || f > 0 })

	if len(rec.failed) > 0 {
		t.Fatalf("a release of signatures was recorded as a failure: %v", rec.failed)
	}
}

// A disabled scanner is not a clean release either.
func TestSyncWithDisabledScannerFails(t *testing.T) {
	s, rec := syncerFor(t, Disabled{ProviderName: "jfrog-xray", Reason: "Xray is not enabled."})
	if _, err := s.Start(t.Context(), syncRequest()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, func() bool { r, f := rec.outcomes(); return r > 0 || f > 0 })

	if len(rec.recorded) > 0 {
		t.Fatal("a disabled scanner was recorded as a successful sync")
	}
	if len(rec.failed) != 1 || !contains(rec.failed[0], "not enabled") {
		t.Errorf("failures = %v", rec.failed)
	}
}

// A claim somebody else holds is reported back, and no work starts.
func TestSyncRespectsAHeldClaim(t *testing.T) {
	rec := &fakeRecorder{claimErr: errors.New("already running")}
	s := NewSyncer(NewService(stubResolver{stubProvider{enabled: true}}, nil, nil), rec, nil)

	status, err := s.Start(t.Context(), syncRequest())
	if err == nil {
		t.Fatal("Start hid a held claim")
	}
	if status != SyncAlreadyRunning {
		t.Errorf("status = %q, want already_running", status)
	}
	if s.Running(1) {
		t.Error("work started despite the claim being held")
	}
}

// A sync somebody stops leaves nothing behind: no failure recorded, no claim
// held, and no goroutine still asking a scanner about a release nobody is
// waiting for.
func TestCancelStopsARunningSync(t *testing.T) {
	started := make(chan struct{})
	s, rec := syncerFor(t, blockingProvider{started: started})

	if _, err := s.Start(t.Context(), syncRequest()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-started

	stopped, err := s.Cancel(t.Context(), 1)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if !stopped {
		t.Error("Cancel reported nothing to stop while a sync was running")
	}

	waitFor(t, func() bool { return !s.Running(1) })
	if r, f := rec.outcomes(); r > 0 || f > 0 {
		t.Errorf("a stopped sync recorded an outcome: recorded=%d failed=%d", r, f)
	}
	if rec.stopped != 1 {
		t.Errorf("claims released = %d, want 1", rec.stopped)
	}
}

// A claim taken away elsewhere - somebody pressing Stop on another replica -
// stands this run down at its next beat.
func TestARunStandsDownWhenItsClaimGoes(t *testing.T) {
	started := make(chan struct{})
	rec := &fakeRecorder{}
	s := NewSyncer(NewService(stubResolver{blockingProvider{started: started}}, nil, nil), rec, nil)
	// Beat immediately rather than every fifteen seconds: the behaviour under
	// test is what the beat DOES, not how often it happens.
	s.beatEvery = time.Millisecond

	if _, err := s.Start(t.Context(), syncRequest()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-started

	rec.mu.Lock()
	rec.held = false
	rec.mu.Unlock()

	waitFor(t, func() bool { return !s.Running(1) })
	if r, f := rec.outcomes(); r > 0 || f > 0 {
		t.Errorf("a stood-down run recorded an outcome: recorded=%d failed=%d", r, f)
	}
}

// The sentence that keeps arriving with a smaller number in it is one piece of
// news, and it reads as one line rather than as a growing list of failures.
func TestNotesCollapseRetellings(t *testing.T) {
	p := &SyncProgress{stages: map[string]stagePosition{}, startedAt: time.Now()}
	p.Note("JFrog Xray timed out on 50 artifacts. Retrying as two smaller requests.")
	p.Note("JFrog Xray timed out on 25 artifacts. Retrying as two smaller requests.")
	p.Note("JFrog Xray timed out on 7 artifacts. Retrying as two smaller requests.")
	p.Note("Requesting scan results for 12 images.")

	_, notes, _, _ := p.Snapshot()
	if len(notes) != 2 {
		t.Fatalf("notes = %v, want the timeout as one line and the other beside it", notes)
	}
	// The newest telling wins - its number is the current one - and the line
	// says how many times it has been said.
	if !contains(notes[0], "7 artifacts") || !contains(notes[0], "(3 times)") {
		t.Errorf("collapsed note = %q", notes[0])
	}

	entries := p.Entries()
	if len(entries) != 2 {
		t.Fatalf("log = %v, want the retellings collapsed there too", entries)
	}
	if entries[0].Repeat != 2 || !contains(entries[0].Message, "7 artifacts") {
		t.Errorf("log entry = %+v", entries[0])
	}
}

// blockingProvider holds a scan open until its context ends, so a test can stop
// one mid-flight.
type blockingProvider struct{ started chan struct{} }

func (blockingProvider) Name() string  { return "jfrog-xray" }
func (blockingProvider) Enabled() bool { return true }

func (p blockingProvider) Scan(ctx context.Context, _ []ArtifactRef, _ ScanOptions) ([]Report, error) {
	select {
	case <-p.started:
	default:
		close(p.started)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
