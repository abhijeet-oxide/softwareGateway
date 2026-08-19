package transfer_test

import (
	"context"
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/transfer"
)

// fakeDelegation records what the expander asked for.
type fakeDelegation struct {
	strategy transfer.Strategy
	requests []transfer.SyncRequest
	err      error
	handle   transfer.SyncHandle
}

func (f *fakeDelegation) Strategy(context.Context, string, string) (transfer.Strategy, error) {
	return f.strategy, nil
}

func (f *fakeDelegation) RequestSync(_ context.Context, req transfer.SyncRequest) (transfer.SyncHandle, error) {
	f.requests = append(f.requests, req)
	return f.handle, f.err
}

func delegatedExpander(s *slice, d transfer.Delegation) *transfer.Expander {
	return transfer.NewExpander(
		s.packages,
		transfer.NewPlanner(s.packages, 4, testLogger(s.t)),
		testResolver{packages: s.packages},
		0, testLogger(s.t),
	).WithDelegation(d, store.NewReplication(s.st))
}

// The heart of the strategy seam: a destination whose registry fetches for
// itself produces NO jobs, and the transfer waits on the registry instead.
func TestDelegatedTransferCreatesNoJobs(t *testing.T) {
	s := newSlice(t)
	pkg, _ := seedORB(t, s, "orb_23.8.1076")
	s.openTransfer("to-quay", pkg, "replicate", s.sourceID, s.targetID)

	d := &fakeDelegation{strategy: transfer.StrategyMirror, handle: transfer.SyncHandle{SyncID: 7}}
	res, err := delegatedExpander(s, d).Expand(t.Context())
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if res.Jobs != 0 {
		t.Fatalf("a delegated transfer must plan no jobs, got %d", res.Jobs)
	}
	if res.Failed != 0 {
		t.Fatalf("delegating is not a failure, got %d", res.Failed)
	}
	if got := s.transferState("to-quay"); got != "syncing" {
		t.Fatalf("state = %q, want syncing", got)
	}
}

// The expected digest has to reach the registry side, because it is the only
// thing that makes `diverged` detectable later.
func TestDelegatedTransferCarriesTheExpectedDigest(t *testing.T) {
	s := newSlice(t)
	pkg, _ := seedORB(t, s, "orb_23.8.1076")
	s.openTransfer("to-quay", pkg, "replicate", s.sourceID, s.targetID)

	d := &fakeDelegation{strategy: transfer.StrategyMirror}
	if _, err := delegatedExpander(s, d).Expand(t.Context()); err != nil {
		t.Fatalf("expand: %v", err)
	}
	if len(d.requests) != 1 {
		t.Fatalf("expected one sync request, got %d", len(d.requests))
	}
	req := d.requests[0]
	if req.ExpectedDigest != pkg.ManifestDigest {
		t.Fatalf("expected digest = %q, want %q", req.ExpectedDigest, pkg.ManifestDigest)
	}
	if req.Tag != pkg.Tag || req.TransferID != "to-quay" {
		t.Fatalf("request = %+v", req)
	}
}

// Nothing can be pushed into a pull-through cache, and the refusal has to name
// the command that does work.
func TestProxyDestinationIsRefusedByName(t *testing.T) {
	s := newSlice(t)
	pkg, _ := seedORB(t, s, "orb_23.8.1076")
	s.openTransfer("to-cache", pkg, "replicate", s.sourceID, s.targetID)

	d := &fakeDelegation{strategy: transfer.StrategyProxy}
	res, err := delegatedExpander(s, d).Expand(t.Context())
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if res.Failed != 1 {
		t.Fatalf("expected the transfer to fail, got %+v", res)
	}
	if got := s.transferState("to-cache"); got != "failed" {
		t.Fatalf("state = %q", got)
	}
	if reason := s.failureReason("to-cache"); !strings.Contains(reason, "warm") {
		t.Fatalf("the refusal must name `warm`, got %q", reason)
	}
	if len(d.requests) != 0 {
		t.Fatal("a proxy cache must not be asked to sync")
	}
}

// A failure to reach the registry fails the transfer rather than leaving it in
// `syncing` - a transfer that never asked cannot be settled by the watcher.
func TestSyncRequestFailureFailsTheTransfer(t *testing.T) {
	s := newSlice(t)
	pkg, _ := seedORB(t, s, "orb_23.8.1076")
	s.openTransfer("to-quay", pkg, "replicate", s.sourceID, s.targetID)

	d := &fakeDelegation{strategy: transfer.StrategyMirror, err: errNotApplied}
	res, err := delegatedExpander(s, d).Expand(t.Context())
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if res.Failed != 1 || s.transferState("to-quay") != "failed" {
		t.Fatalf("expected a failed transfer, got %+v state %q", res, s.transferState("to-quay"))
	}
	if reason := s.failureReason("to-quay"); !strings.Contains(reason, "targets apply") {
		t.Fatalf("the reason must carry the next step, got %q", reason)
	}
}

// Every deployment before M8 - and every estate with no delegated targets -
// must behave exactly as it did.
func TestNoDelegationConfiguredIsUnchanged(t *testing.T) {
	s := newSlice(t)
	pkg, _ := seedORB(t, s, "orb_23.8.1076")
	s.openTransfer("to-lab", pkg, "replicate", s.sourceID, s.targetID)

	expander := transfer.NewExpander(
		s.packages,
		transfer.NewPlanner(s.packages, 4, testLogger(t)),
		testResolver{packages: s.packages},
		0, testLogger(t),
	)
	res, err := expander.Expand(t.Context())
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if res.Jobs == 0 {
		t.Fatal("with no delegation configured every transfer is a copy and must plan jobs")
	}
	if got := s.transferState("to-lab"); got != "ready" {
		t.Fatalf("state = %q, want ready", got)
	}
}

// A copy destination behind a configured delegation is still a copy.
func TestCopyStrategyStillPlansJobs(t *testing.T) {
	s := newSlice(t)
	pkg, _ := seedORB(t, s, "orb_23.8.1076")
	s.openTransfer("to-lab", pkg, "replicate", s.sourceID, s.targetID)

	d := &fakeDelegation{strategy: transfer.StrategyCopy}
	res, err := delegatedExpander(s, d).Expand(t.Context())
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if res.Jobs == 0 {
		t.Fatal("a copy destination must plan jobs")
	}
	if len(d.requests) != 0 {
		t.Fatal("a copy destination must not be asked to sync")
	}
}

var errNotApplied = errNotAppliedType{}

type errNotAppliedType struct{}

func (errNotAppliedType) Error() string {
	return "target \"ocp-prod\" has no mirror configuration on the registry yet - run `transferctl targets apply vendor-a ocp-prod` first"
}

// failureReason reads why a transfer stopped, which for a delegated one is the
// whole of what it has instead of a job list.
func (s *slice) failureReason(id string) string {
	s.t.Helper()
	var reason string
	var nullable *string
	if err := s.st.DB().QueryRowContext(s.t.Context(),
		`SELECT failure_reason FROM transfers WHERE id = ?`, id).Scan(&nullable); err != nil {
		s.t.Fatal(err)
	}
	if nullable != nil {
		reason = *nullable
	}
	return reason
}
