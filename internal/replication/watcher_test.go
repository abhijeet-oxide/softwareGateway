package replication

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/quay"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

type fakeProducts struct{ p *product.Product }

func (f fakeProducts) Get(name string) (*product.Product, bool) {
	if f.p == nil || f.p.Metadata.Name != name {
		return nil, false
	}
	return f.p, true
}

type fakeDest struct {
	digest string
	err    error
	calls  int
}

func (f *fakeDest) ResolveAtTarget(context.Context, string, string, string) (string, error) {
	f.calls++
	return f.digest, f.err
}

// recorder is the SyncRecorder, captured in memory. These tests are about the
// DECISION the watcher makes, not about the SQL that records it.
type recorder struct {
	open        []store.OpenSync
	syncUpdates []store.MirrorSync
	settled     []settlement
}

type settlement struct {
	transferID string
	state      string
	reason     string
}

const (
	syncedDigest   = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	divergedDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

func watcherFor(t *testing.T, api ManagementAPI, dest DestinationReader, obsStatus string) (*Watcher, *recorder) {
	t.Helper()
	res := NewResolver(nil, nil, "test").WithClock(func() time.Time { return applyTime })
	svc := NewService(res, nil, nil)

	p := testProduct()
	tgt := p.Spec.Targets[1]
	svc.SetClient(p.Metadata.Name, tgt.Name, api)

	rec := &recorder{}
	_ = obsStatus
	return NewWatcher(svc, rec, fakeProducts{p: p}, dest, nil), rec
}

func (r *recorder) OpenSyncs(context.Context, int) ([]store.OpenSync, error) {
	return r.open, nil
}

func (r *recorder) SettleDelegated(_ context.Context, transferID, state, reason string) error {
	r.settled = append(r.settled, settlement{transferID, state, reason})
	return nil
}

func (r *recorder) UpdateSync(_ context.Context, _ int64, s store.MirrorSync) error {
	r.syncUpdates = append(r.syncUpdates, s)
	return nil
}

func openSync() store.OpenSync {
	return store.OpenSync{
		SyncID: 1, TransferID: "t-1", Product: "vendor-a", TargetName: "ocp-prod",
		ExpectedDigest: syncedDigest, Tag: "v3.2.1", Repository: "platform-prod/vendor-a-platform",
		RequestedAt: sql.NullString{String: quay.FormatTime(time.Now()), Valid: true},
	}
}

func mirrorState(status string) *quay.MirrorState {
	return &quay.MirrorState{IsEnabled: true, RawSyncStatus: status,
		RootRule: quay.RootRule{RuleKind: quay.RuleKindTagGlobCSV, RuleValue: []string{"v3.*"}}}
}

// The registry says it worked AND the destination holds what we asked for.
func TestSyncSucceedsWhenTheDigestMatches(t *testing.T) {
	api := &fakeQuay{repo: &quay.Repository{State: "MIRROR"}, mirror: mirrorState("SUCCESS")}
	dest := &fakeDest{digest: syncedDigest}
	w, rec := watcherFor(t, api, dest, "SUCCESS")

	outcome, err := w.check(context.Background(), openSync())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if outcome != store.SyncSucceeded {
		t.Fatalf("outcome = %q", outcome)
	}
	if dest.calls != 1 {
		t.Fatal("a completed sync must be confirmed by walking the destination")
	}
	if rec.settled[0].state != store.SyncSucceeded {
		t.Fatalf("transfer settled as %q", rec.settled[0].state)
	}
}

// THE case this whole design exists for: the registry reports success and the
// destination holds a different digest. Not a failure — the upstream tag moved
// and the mirror followed it — but not success either.
func TestSyncDivergesWhenTheDigestDiffers(t *testing.T) {
	api := &fakeQuay{repo: &quay.Repository{State: "MIRROR"}, mirror: mirrorState("SUCCESS")}
	dest := &fakeDest{digest: divergedDigest}
	w, rec := watcherFor(t, api, dest, "SUCCESS")

	outcome, err := w.check(context.Background(), openSync())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if outcome != store.SyncDiverged {
		t.Fatalf("outcome = %q, want diverged", outcome)
	}
	msg := rec.settled[0].reason
	if !strings.Contains(msg, "upstream tag has moved") {
		t.Fatalf("the message must explain what diverged means, got %q", msg)
	}
	if !strings.Contains(msg, "sha256:2222222222") {
		t.Fatalf("the message must name what is actually there, got %q", msg)
	}
	if rec.syncUpdates[0].ObservedDigest != divergedDigest {
		t.Fatalf("the observed digest must be recorded, got %+v", rec.syncUpdates[0])
	}
}

// The most common misconfiguration: the sync succeeds and the tag is not
// there, because the glob never matched it.
func TestAbsentTagBlamesTheGlobs(t *testing.T) {
	api := &fakeQuay{repo: &quay.Repository{State: "MIRROR"}, mirror: mirrorState("SUCCESS")}
	dest := &fakeDest{err: &registry.Error{Err: registry.ErrNotFound}}
	w, rec := watcherFor(t, api, dest, "SUCCESS")

	outcome, err := w.check(context.Background(), openSync())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if outcome != store.SyncFailed {
		t.Fatalf("outcome = %q", outcome)
	}
	if !strings.Contains(rec.settled[0].reason, "tag globs") {
		t.Fatalf("the message must point at the likely cause, got %q", rec.settled[0].reason)
	}
}

// A running sync is not settled, and is not an error either.
func TestRunningSyncIsLeftAlone(t *testing.T) {
	for _, status := range []string{"SYNCING", "SYNC_NOW"} {
		api := &fakeQuay{repo: &quay.Repository{State: "MIRROR"}, mirror: mirrorState(status)}
		w, rec := watcherFor(t, api, &fakeDest{}, status)

		outcome, err := w.check(context.Background(), openSync())
		if err != nil {
			t.Fatalf("%s: check: %v", status, err)
		}
		if outcome != "" {
			t.Fatalf("%s: a running sync must not settle, got %q", status, outcome)
		}
		if len(rec.settled) != 0 {
			t.Fatalf("%s: nothing should have been settled", status)
		}
	}
}

func TestFailedSyncSettlesAsFailed(t *testing.T) {
	api := &fakeQuay{repo: &quay.Repository{State: "MIRROR"}, mirror: mirrorState("FAIL")}
	dest := &fakeDest{digest: syncedDigest}
	w, rec := watcherFor(t, api, dest, "FAIL")

	outcome, err := w.check(context.Background(), openSync())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if outcome != store.SyncFailed {
		t.Fatalf("outcome = %q", outcome)
	}
	if dest.calls != 0 {
		t.Fatal("a failed sync needs no destination walk")
	}
	_ = rec
}

// An unrecognised status is not guessed at — but it is not allowed to hang
// forever either, which is what the timeout is for.
func TestUnknownStatusWaitsThenTimesOut(t *testing.T) {
	api := &fakeQuay{repo: &quay.Repository{State: "MIRROR"}, mirror: mirrorState("QUIESCING")}
	w, _ := watcherFor(t, api, &fakeDest{}, "QUIESCING")

	outcome, err := w.check(context.Background(), openSync())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if outcome != "" {
		t.Fatalf("an unrecognised status must not settle while it is fresh, got %q", outcome)
	}

	// Same status, but the request is older than the bound.
	w.WithTimeout(time.Minute)
	old := openSync()
	old.RequestedAt = sql.NullString{String: quay.FormatTime(time.Now().Add(-time.Hour)), Valid: true}

	outcome, err = w.check(context.Background(), old)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if outcome != store.SyncFailed {
		t.Fatalf("an expired sync must settle, got %q", outcome)
	}
}

// A sync the registry never started is a real failure once the bound passes,
// and the message has to say which of the two it was.
func TestNeverRunTimesOut(t *testing.T) {
	api := &fakeQuay{repo: &quay.Repository{State: "MIRROR"}, mirror: mirrorState("NEVER_RUN")}
	w, rec := watcherFor(t, api, &fakeDest{}, "NEVER_RUN")
	w.WithTimeout(time.Minute)

	old := openSync()
	old.RequestedAt = sql.NullString{String: quay.FormatTime(time.Now().Add(-time.Hour)), Valid: true}

	outcome, err := w.check(context.Background(), old)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if outcome != store.SyncFailed {
		t.Fatalf("outcome = %q", outcome)
	}
	if !strings.Contains(rec.settled[0].reason, "never started") {
		t.Fatalf("the message must distinguish never-started from failed, got %q", rec.settled[0].reason)
	}
}

// Without a way to look at the destination we must not report success on the
// registry's word. `unknown` is the honest answer.
func TestNoDestinationReaderReportsUnknownNotSuccess(t *testing.T) {
	api := &fakeQuay{repo: &quay.Repository{State: "MIRROR"}, mirror: mirrorState("SUCCESS")}
	w, rec := watcherFor(t, api, nil, "SUCCESS")

	outcome, err := w.check(context.Background(), openSync())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if outcome != store.SyncUnknown {
		t.Fatalf("outcome = %q, want unknown", outcome)
	}
	// The sync row keeps the distinction; the transfer has no "we could not
	// tell" state and settles as failed rather than claiming success.
	if rec.settled[0].state != store.SyncFailed {
		t.Fatalf("transfer settled as %q", rec.settled[0].state)
	}
	if rec.syncUpdates[0].Status != store.SyncUnknown {
		t.Fatalf("the sync row must keep `unknown`, got %q", rec.syncUpdates[0].Status)
	}
}

// Not being able to READ the registry is not the sync failing, so the transfer
// stays open and is retried next tick.
func TestUnreachableRegistryDoesNotSettle(t *testing.T) {
	res := NewResolver(nil, nil, "test").WithClock(func() time.Time { return applyTime })
	svc := NewService(res, nil, nil) // no client injected: unreachable
	rec := &recorder{}
	w := NewWatcher(svc, rec, fakeProducts{p: testProduct()}, &fakeDest{}, nil)

	if _, err := w.check(context.Background(), openSync()); err == nil {
		t.Fatal("an unreachable registry must surface as an error, not as an outcome")
	}
	if len(rec.settled) != 0 {
		t.Fatal("nothing may be settled when we could not look")
	}
}

// A product removed from configuration while a sync was open cannot ever
// complete, and a row that hangs forever is worse than one that says why.
func TestUnconfiguredProductSettlesRatherThanHanging(t *testing.T) {
	api := &fakeQuay{repo: &quay.Repository{State: "MIRROR"}, mirror: mirrorState("SUCCESS")}
	w, rec := watcherFor(t, api, &fakeDest{digest: syncedDigest}, "SUCCESS")
	w.products = fakeProducts{} // nothing loaded

	outcome, err := w.check(context.Background(), openSync())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if outcome != store.SyncFailed {
		t.Fatalf("outcome = %q", outcome)
	}
	if !strings.Contains(rec.settled[0].reason, "no longer configured") {
		t.Fatalf("reason = %q", rec.settled[0].reason)
	}
}
