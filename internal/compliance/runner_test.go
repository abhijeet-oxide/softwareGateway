package compliance_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
	celc "github.com/abhijeet-oxide/softwareGateway/internal/compliance/cel"
)

// recorder captures what a run writes, in order.
type recorder struct {
	mu        sync.Mutex
	started   []string
	beats     int
	recorded  *compliance.Run
	failed    string
	live      bool
	startErr  error
	recordErr error
	done      chan struct{}
}

func newRecorder() *recorder { return &recorder{done: make(chan struct{})} }

func (r *recorder) StartComplianceRun(_ context.Context, runID string, _ int64, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.startErr != nil {
		return r.startErr
	}
	r.started = append(r.started, runID)
	return nil
}

func (r *recorder) BeatComplianceRun(context.Context, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.beats++
	return nil
}

func (r *recorder) RecordComplianceRun(_ context.Context, _ string, _ int64, run *compliance.Run) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recordErr != nil {
		return r.recordErr
	}
	r.recorded = run
	close(r.done)
	return nil
}

func (r *recorder) FailComplianceRun(_ context.Context, _ string, _ int64, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed = reason
	close(r.done)
	return nil
}

func (r *recorder) ComplianceRunning(context.Context, int64) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return "", r.live, nil
}

func (r *recorder) wait(t *testing.T) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never finished")
	}
}

// stubSource hands back a fixed release, optionally after a delay or an error.
type stubSource struct {
	rel   *compliance.Release
	err   error
	block chan struct{}
	// cleaned is closed by the cleanup function. A bool would be a data race
	// with the runner's goroutine, and polling one would race the deferred
	// call that sets it - cleanup runs just AFTER the result is recorded.
	cleaned chan struct{}
}

func (s *stubSource) Prepare(
	ctx context.Context, _ compliance.Request,
	report func(compliance.Stage, int, int, string),
) (*compliance.Release, compliance.Determiner, func(), error) {
	cleanup := func() {
		if s.cleaned != nil {
			close(s.cleaned)
		}
	}
	report(compliance.StageFetching, 1, 1, "")
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return nil, nil, cleanup, ctx.Err()
		}
	}
	if s.err != nil {
		return nil, nil, cleanup, s.err
	}
	return s.rel, nil, cleanup, nil
}

func tinyCatalog(t *testing.T) *compliance.Catalog {
	t.Helper()
	comp, err := celc.NewCompiler()
	if err != nil {
		t.Fatal(err)
	}
	check := compliance.Check{
		ID: "TST-01", Title: "Containers declare a memory limit",
		Severity: compliance.SeverityBlock, Tier: 1, Category: "Test",
		AppliesTo: compliance.AppliesTo{Kinds: []string{"Deployment"}, Containers: compliance.ScopeAll},
		Assert:    compliance.Assert{Required: []string{"resources.limits.memory"}},
	}
	prog, err := comp.Compile(check)
	if err != nil {
		t.Fatal(err)
	}
	cat := compliance.NewCatalog()
	if err := cat.Add(check, prog); err != nil {
		t.Fatal(err)
	}
	cat.BundleDigest = "sha256:test"
	return cat
}

func releaseWithOneFailure() *compliance.Release {
	return &compliance.Release{
		Product: "p", Tag: "v1",
		Resources: []compliance.Resource{{
			Object: map[string]any{
				"apiVersion": "apps/v1", "kind": "Deployment",
				"metadata": map[string]any{"name": "app", "namespace": "ns"},
				"spec": map[string]any{"template": map[string]any{
					"spec": map[string]any{"containers": []any{
						map[string]any{"name": "main"},
					}},
				}},
			},
		}},
	}
}

func newRunner(t *testing.T, rec *recorder, src compliance.Source) *compliance.Runner {
	t.Helper()
	cat := tinyCatalog(t)
	return &compliance.Runner{
		Catalog:  func() *compliance.Catalog { return cat },
		Source:   src,
		Recorder: rec,
		Beat:     10 * time.Millisecond,
	}
}

func TestRunRecordsItsResult(t *testing.T) {
	rec := newRecorder()
	cleaned := make(chan struct{})
	r := newRunner(t, rec, &stubSource{rel: releaseWithOneFailure(), cleaned: cleaned})

	if _, err := r.Start(context.Background(), compliance.Request{
		RunID: "run-1", PackageID: 7, Product: "p", Release: "v1",
	}); err != nil {
		t.Fatal(err)
	}
	rec.wait(t)

	if rec.recorded == nil {
		t.Fatal("nothing was recorded")
	}
	if rec.recorded.Verdict != compliance.VerdictFail {
		t.Errorf("verdict = %s, want fail", rec.recorded.Verdict)
	}
	if rec.recorded.Counts.Blocking != 1 {
		t.Errorf("blocking = %d, want 1", rec.recorded.Counts.Blocking)
	}
	// The working directory must go, whether the run succeeded or not. It is
	// released after the result is recorded, so this waits rather than polls.
	select {
	case <-cleaned:
	case <-time.After(5 * time.Second):
		t.Error("the working directory was not cleaned up")
	}
}

// Two people pressing the button at the same moment must start one run.
func TestSecondRunIsRefusedWhileOneIsLive(t *testing.T) {
	rec := newRecorder()
	block := make(chan struct{})
	r := newRunner(t, rec, &stubSource{rel: releaseWithOneFailure(), block: block})

	req := compliance.Request{RunID: "run-1", PackageID: 7, Product: "p", Release: "v1"}
	if _, err := r.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	// The in-process guard.
	req2 := req
	req2.RunID = "run-2"
	if _, err := r.Start(context.Background(), req2); !errors.Is(err, compliance.ErrRunInFlight) {
		t.Fatalf("second start returned %v, want ErrRunInFlight", err)
	}
	close(block)
	rec.wait(t)

	if len(rec.started) != 1 {
		t.Errorf("%d runs were claimed, want 1", len(rec.started))
	}
}

// The row is the authority: another Coordinator's live run must refuse this one.
func TestAnotherCoordinatorsRunIsRefused(t *testing.T) {
	rec := newRecorder()
	rec.live = true
	r := newRunner(t, rec, &stubSource{rel: releaseWithOneFailure()})

	_, err := r.Start(context.Background(), compliance.Request{RunID: "x", PackageID: 7})
	if !errors.Is(err, compliance.ErrRunInFlight) {
		t.Fatalf("err = %v, want ErrRunInFlight", err)
	}
	if len(rec.started) != 0 {
		t.Error("a claim was taken despite another Coordinator holding one")
	}
}

// A failure must be recorded, not only logged. A release with no result is
// indistinguishable from one nobody checked.
func TestFailureIsRecorded(t *testing.T) {
	rec := newRecorder()
	r := newRunner(t, rec, &stubSource{err: errors.New("the registry refused us")})

	if _, err := r.Start(context.Background(), compliance.Request{RunID: "run-1", PackageID: 7}); err != nil {
		t.Fatal(err)
	}
	rec.wait(t)

	if rec.failed == "" {
		t.Fatal("the failure was not recorded")
	}
	if rec.failed != "the registry refused us" {
		t.Errorf("reason = %q; a reason nobody can act on is a dead end", rec.failed)
	}
}

// Cancelling records why, so the next reader knows the run was stopped rather
// than never started.
func TestCancelRecordsThatItWasCancelled(t *testing.T) {
	rec := newRecorder()
	block := make(chan struct{})
	defer close(block)
	r := newRunner(t, rec, &stubSource{rel: releaseWithOneFailure(), block: block})

	if _, err := r.Start(context.Background(), compliance.Request{RunID: "run-1", PackageID: 7}); err != nil {
		t.Fatal(err)
	}
	// The run has to reach Prepare before there is anything to cancel.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, live := r.Progress(7); live {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !r.Cancel(7) {
		t.Fatal("Cancel found no live run")
	}
	rec.wait(t)

	if rec.failed != "the check was cancelled" {
		t.Errorf("reason = %q, want the cancellation to be named", rec.failed)
	}
}

// Progress must be visible while the run is working, or somebody who pressed a
// button and sees nothing presses it again.
func TestProgressIsVisibleWhileRunning(t *testing.T) {
	rec := newRecorder()
	block := make(chan struct{})
	r := newRunner(t, rec, &stubSource{rel: releaseWithOneFailure(), block: block})

	if _, err := r.Start(context.Background(), compliance.Request{RunID: "run-1", PackageID: 7}); err != nil {
		t.Fatal(err)
	}
	var seen bool
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p, live := r.Progress(7); live && p.Stage != "" && p.Label != "" {
			seen = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !seen {
		t.Error("no progress was reported")
	}
	close(block)
	rec.wait(t)

	// And it must stop being reported once the run is over, or the interface
	// shows a spinner forever.
	//
	// Waited for rather than asserted immediately: the recorder's signal fires
	// INSIDE the write, and the run releases its slot on the way out of the
	// call after it. Reading the map the instant the write returns races the
	// runner's own unwinding, not the behaviour under test.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, live := r.Progress(7); !live {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Error("a finished run still reports progress")
}

// The claim is touched while the run is alive, so the sweeper can tell a live
// run from a Coordinator that died holding one.
func TestHeartbeatRunsWhileWorking(t *testing.T) {
	rec := newRecorder()
	block := make(chan struct{})
	r := newRunner(t, rec, &stubSource{rel: releaseWithOneFailure(), block: block})

	if _, err := r.Start(context.Background(), compliance.Request{RunID: "run-1", PackageID: 7}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rec.mu.Lock()
		n := rec.beats
		rec.mu.Unlock()
		if n > 0 {
			close(block)
			rec.wait(t)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(block)
	t.Error("the claim was never touched, so a healthy run would be swept as stale")
}
