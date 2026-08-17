package replication

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/quay"
)

func testProduct() *product.Product {
	return &product.Product{
		Metadata: product.Metadata{Name: "vendor-a"},
		Spec: product.Spec{Targets: []product.Target{
			{Name: "jfrog-store", Registry: "artifactory.example.com", Repository: "docker-local/vendor-a"},
			mirrorTarget(),
		}},
	}
}

// newService builds one with a fixed clock and an injected client, so the
// tests exercise the ORDER of operations rather than HTTP.
func newService(t *testing.T, api ManagementAPI) (*Service, *product.Product, product.Target) {
	t.Helper()
	res := NewResolver(nil, nil, "test").WithClock(func() time.Time { return applyTime })
	svc := NewService(res, nil, nil)
	p := testProduct()
	tgt := p.Spec.Targets[1]
	svc.SetClient(p.Metadata.Name, tgt.Name, api)
	return svc, p, tgt
}

func TestStatusReportsPendingApplyBeforeAnythingIsApplied(t *testing.T) {
	svc, p, tgt := newService(t, &fakeQuay{repo: &quay.Repository{State: "NORMAL"}})

	st, err := svc.Status(context.Background(), p, tgt)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.HasApplied {
		t.Fatal("nothing has been applied")
	}
	if !st.PendingApply {
		t.Fatal("a configured target that has never been applied is pending")
	}
	if st.Observation == nil || !st.Observation.Drift.Absent {
		t.Fatalf("the registry holds no mirror, which is Absent: %+v", st.Observation)
	}
}

// A copy target must stay exactly as uninteresting as it is today: no
// management client is built, and nothing is reported that could be mistaken
// for a registry-side configuration.
func TestStatusOfACopyTargetTouchesNothing(t *testing.T) {
	api := &fakeQuay{}
	res := NewResolver(nil, nil, "test").WithClock(func() time.Time { return applyTime })
	svc := NewService(res, nil, nil)
	p := testProduct()

	st, err := svc.Status(context.Background(), p, p.Spec.Targets[0])
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Mode != product.ReplicationCopy || st.Desired != nil || st.Observation != nil {
		t.Fatalf("a copy target has no delegated state: %+v", st)
	}
	if len(api.calls) != 0 {
		t.Fatalf("no registry call should have been made: %v", api.calls)
	}
}

// An unreachable registry must not fail the whole status: the configured
// intent and the last apply are both known without a network call, and saying
// "we could not look" is more useful than saying nothing.
func TestStatusSurvivesAnUnreachableRegistry(t *testing.T) {
	res := NewResolver(nil, nil, "test").WithClock(func() time.Time { return applyTime })
	svc := NewService(res, nil, nil)
	p := testProduct()
	// No client injected and no secret resolver, so building one fails.

	st, err := svc.Status(context.Background(), p, p.Spec.Targets[1])
	if err != nil {
		t.Fatalf("Status must not fail because the registry is unreachable: %v", err)
	}
	if st.Unreachable == nil {
		t.Fatal("the reason must be reported")
	}
	if st.Desired == nil {
		t.Fatal("the configured intent is known without a network call and must still be returned")
	}
}

// The whole point of the M8 guard rail: a first apply is destructive and does
// not happen without confirmation.
func TestFirstApplyNeedsConfirmation(t *testing.T) {
	api := &fakeQuay{repo: &quay.Repository{State: "NORMAL"}}
	svc, p, tgt := newService(t, api)

	res, err := svc.Apply(context.Background(), p, tgt, ApplyOptions{Actor: "alice"})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.NeedsConfirmation || res.Applied {
		t.Fatalf("a destructive apply must stop and ask: %+v", res)
	}
	for _, c := range api.calls {
		if !strings.HasPrefix(c, "Get") {
			t.Fatalf("nothing may be written before confirmation, got %v", api.calls)
		}
	}
}

func TestConfirmedApplyWrites(t *testing.T) {
	api := &fakeQuay{repo: &quay.Repository{State: "NORMAL"}}
	svc, p, tgt := newService(t, api)

	res, err := svc.Apply(context.Background(), p, tgt, ApplyOptions{Confirm: true, Actor: "alice"})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Applied {
		t.Fatalf("expected the apply to happen: %+v", res)
	}
	var wrote bool
	for _, c := range api.calls {
		if c == "PutMirror" {
			wrote = true
		}
	}
	if !wrote {
		t.Fatalf("expected a write, got %v", api.calls)
	}
}

// A dry run is the same code path with the write suppressed, so the plan
// cannot drift from the act.
func TestValidateOnlyWritesNothing(t *testing.T) {
	api := &fakeQuay{repo: &quay.Repository{State: "NORMAL"}}
	svc, p, tgt := newService(t, api)

	res, err := svc.Apply(context.Background(), p, tgt, ApplyOptions{ValidateOnly: true, Confirm: true})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Applied || res.Plan == nil || len(res.Plan.Steps) == 0 {
		t.Fatalf("a dry run returns the plan and changes nothing: %+v", res)
	}
	for _, c := range api.calls {
		if !strings.HasPrefix(c, "Get") {
			t.Fatalf("a dry run performed a mutating call: %v", api.calls)
		}
	}
}

// `manage: detect` means what it says. A user who asked us never to write must
// not have that overridden by an explicit apply.
func TestManageDetectRefusesToWrite(t *testing.T) {
	api := &fakeQuay{repo: &quay.Repository{State: "NORMAL"}}
	svc, p, tgt := newService(t, api)
	tgt.Replication.Mirror.Manage = product.ManageDetect

	_, err := svc.Apply(context.Background(), p, tgt, ApplyOptions{Confirm: true})
	if err == nil {
		t.Fatal("manage: detect must refuse an apply")
	}
	if !strings.Contains(err.Error(), "detect") {
		t.Fatalf("the refusal must name the field that causes it: %v", err)
	}
	if len(api.calls) != 0 {
		t.Fatalf("nothing should have been contacted: %v", api.calls)
	}
}

func TestApplyRefusedOnACopyTarget(t *testing.T) {
	svc, p, _ := newService(t, &fakeQuay{})

	_, err := svc.Apply(context.Background(), p, p.Spec.Targets[0], ApplyOptions{Confirm: true})
	if err == nil {
		t.Fatal("a copy target has no registry-side configuration to apply")
	}
	if !strings.Contains(err.Error(), "copy mode") {
		t.Fatalf("the refusal must say why: %v", err)
	}
}

func TestSyncRefusedOnACopyTarget(t *testing.T) {
	svc, p, _ := newService(t, &fakeQuay{})

	_, err := svc.Sync(context.Background(), p, p.Spec.Targets[0], "alice")
	if err == nil {
		t.Fatal("a copy target has no sync to request")
	}
}

func TestSyncReportsAlreadyRunningRatherThanFailing(t *testing.T) {
	api := &fakeQuay{repo: &quay.Repository{State: "MIRROR"}, syncing: true}
	svc, p, tgt := newService(t, api)

	out, err := svc.Sync(context.Background(), p, tgt, "alice")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !out.Requested || !out.AlreadyRunning {
		t.Fatalf("a sync already in progress satisfies the request: %+v", out)
	}
}

// The second apply of an unchanged configuration is a no-op, which is what
// makes an apply safe to repeat and a drift check meaningful.
func TestSecondApplyIsANoOp(t *testing.T) {
	api := &fakeQuay{repo: &quay.Repository{State: "NORMAL"}}
	svc, p, tgt := newService(t, api)

	if _, err := svc.Apply(context.Background(), p, tgt, ApplyOptions{Confirm: true}); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	res, err := svc.Apply(context.Background(), p, tgt, ApplyOptions{Confirm: true})
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if res.Plan == nil || !res.Plan.NoOp {
		t.Fatalf("expected a no-op, got %+v", res.Plan)
	}
}

// After an apply the registry agrees with Git, and a status read says so.
func TestStatusIsInSyncAfterAnApply(t *testing.T) {
	api := &fakeQuay{repo: &quay.Repository{State: "NORMAL"}}
	svc, p, tgt := newService(t, api)

	if _, err := svc.Apply(context.Background(), p, tgt, ApplyOptions{Confirm: true}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	st, err := svc.Status(context.Background(), p, tgt)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Observation == nil || st.Observation.Drift.Drifted() {
		t.Fatalf("expected no drift after an apply: %+v", st.Observation)
	}
}
