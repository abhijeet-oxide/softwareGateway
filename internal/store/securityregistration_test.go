package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/security"
)

func TestRegistrationRoundTrip(t *testing.T) {
	st := openTestStore(t)
	regs := NewSecurityRegistrations(st)
	ctx := context.Background()
	pkg := seedPackageFor(t, st)

	// Nothing recorded is a real answer, not an error.
	if _, ok, err := regs.Get(ctx, pkg, "anchore"); err != nil || ok {
		t.Fatalf("an unregistered release reported %v, %v", ok, err)
	}

	if err := regs.Claim(ctx, pkg, "anchore"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// A second press while one is running is refused, so two people pressing
	// Replicate see one operation rather than two racing each other.
	if err := regs.Claim(ctx, pkg, "anchore"); !errors.Is(err, ErrRegistrationInFlight) {
		t.Fatalf("a second claim returned %v, want ErrRegistrationInFlight", err)
	}

	reg := security.Registration{
		Provider: "anchore", State: security.RegistrationComplete,
		Expected: 157, Submitted: 157, Associated: 157, Analysed: 12,
		Application: "cfx-5000", ApplicationID: "app-1",
		Version: "25.7.2131", VersionID: "ver-1",
		URL: "https://anchore.example.com/applications/app-1/versions/ver-1",
	}
	if err := regs.Record(ctx, pkg, reg, []security.SyncLogEntry{
		{At: time.Now().UTC(), Level: security.LogInfo, Message: "Registered 157 images."},
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	row, ok, err := regs.Get(ctx, pkg, "anchore")
	if err != nil || !ok {
		t.Fatalf("get: %v, %v", ok, err)
	}
	if !row.Done() {
		t.Errorf("state = %q, want registered", row.State)
	}
	if row.Associated != 157 || row.Analysed != 12 {
		t.Errorf("counts = %+v", row)
	}
	if row.VersionID != "ver-1" || row.URL == "" {
		t.Errorf("the scanner's own identity was not kept: %+v", row)
	}
	if row.RegisteredAt == nil {
		t.Error("a finished registration has no time")
	}
	if row.StartedAt != nil {
		t.Error("a finished registration still holds its claim")
	}
	if len(row.Log) != 1 {
		t.Errorf("the transcript was lost: %+v", row.Log)
	}

	// The claim is free again once the run finished.
	if err := regs.Claim(ctx, pkg, "anchore"); err != nil {
		t.Errorf("a finished registration would not re-claim: %v", err)
	}
}

// A failed run keeps what the last good one knew: a release replicated last
// week whose Anchore is unreachable today still holds what it held.
func TestFailedRegistrationKeepsTheLastCounts(t *testing.T) {
	st := openTestStore(t)
	regs := NewSecurityRegistrations(st)
	ctx := context.Background()
	pkg := seedPackageFor(t, st)

	if err := regs.Record(ctx, pkg, security.Registration{
		Provider: "anchore", State: security.RegistrationComplete,
		Expected: 10, Associated: 10, Application: "cfx", Version: "1.0",
	}, nil); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := regs.Fail(ctx, pkg, "anchore", "Anchore refused the credential.", nil); err != nil {
		t.Fatalf("fail: %v", err)
	}

	row, _, err := regs.Get(ctx, pkg, "anchore")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.State != security.RegistrationFailed || row.Error == "" {
		t.Errorf("state = %q error = %q", row.State, row.Error)
	}
	if row.Associated != 10 || row.Application != "cfx" {
		t.Errorf("a failed run cleared what the last good one knew: %+v", row)
	}
}

// A Coordinator killed mid-replication must not leave a release refusing the
// button forever.
func TestAbandonedRegistrationIsReleased(t *testing.T) {
	st := openTestStore(t)
	regs := NewSecurityRegistrations(st)
	ctx := context.Background()
	pkg := seedPackageFor(t, st)

	if err := regs.Claim(ctx, pkg, "anchore"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Age the claim past the window.
	if _, err := st.DB().ExecContext(ctx, regs.q(
		`UPDATE security_registrations SET started_at = ? WHERE package_id = ?`),
		securityTime(time.Now().UTC().Add(-2*StaleRegistrationAfter)), pkg); err != nil {
		t.Fatalf("age the claim: %v", err)
	}

	n, err := regs.ReleaseAbandoned(ctx)
	if err != nil || n != 1 {
		t.Fatalf("released %d, %v", n, err)
	}
	if err := regs.Claim(ctx, pkg, "anchore"); err != nil {
		t.Errorf("the released claim could not be retaken: %v", err)
	}
}

// A stale claim is reclaimable without the sweep, so one dead Coordinator does
// not block a release until a timer fires.
func TestStaleClaimIsRetakenDirectly(t *testing.T) {
	st := openTestStore(t)
	regs := NewSecurityRegistrations(st)
	ctx := context.Background()
	pkg := seedPackageFor(t, st)

	if err := regs.Claim(ctx, pkg, "anchore"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, regs.q(
		`UPDATE security_registrations SET started_at = ? WHERE package_id = ?`),
		securityTime(time.Now().UTC().Add(-2*StaleRegistrationAfter)), pkg); err != nil {
		t.Fatalf("age the claim: %v", err)
	}
	if err := regs.Claim(ctx, pkg, "anchore"); err != nil {
		t.Errorf("a stale claim was not retaken: %v", err)
	}
}
