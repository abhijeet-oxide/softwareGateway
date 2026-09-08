package worker

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestAWorkerThatCannotReachTheCoordinatorIsNotWedged is the distinction the
// whole watchdog turns on.
//
// A worker whose Coordinator is down ticks, fails, backs off and ticks again.
// That is the correct behaviour, and restarting the container for it would
// turn a control-plane blip into a fleet-wide crash-loop at the moment the
// fleet most needs to keep transferring what it already holds.
func TestAWorkerThatCannotReachTheCoordinatorIsNotWedged(t *testing.T) {
	l := &Loop{}
	l.pulse.sleeping(DefaultLeaseRetry)
	l.pulse.leased(errors.New("connection refused"))

	if err := l.Wedged(); err != nil {
		t.Fatalf("wedged: %v, want nil - a failing lease is not a wedge", err)
	}
	ok, at, detail := l.LeaseHealth()
	if ok {
		t.Fatal("lease reported healthy after a failure")
	}
	if at.IsZero() || detail != "connection refused" {
		t.Fatalf("lease health: %v %q, want the attempt time and its error", at, detail)
	}
}

// TestALoopThatStopsGoingRoundIsWedged is what the probe exists for: a worker
// serves nothing, so its probe server answers cheerfully while the lease loop
// is deadlocked or its goroutine is gone.
func TestALoopThatStopsGoingRoundIsWedged(t *testing.T) {
	l := &Loop{}
	// A pass that declared a short sleep and then never came back. Wound past
	// the grace period rather than waiting it out.
	l.pulse.due.Store(time.Now().Add(-time.Second).UnixNano())

	err := l.Wedged()
	if err == nil {
		t.Fatal("wedged: nil, want a failure")
	}
	// The message has to name the fault, because it is what a restarted
	// container's last words will be.
	if got := err.Error(); !strings.Contains(got, "lease loop") {
		t.Fatalf("wedged: %q, want it to name the lease loop", got)
	}
}

// TestALoopThatHasNotStartedIsNotWedged: a process still wiring itself up must
// not be restarted for not having begun.
func TestALoopThatHasNotStartedIsNotWedged(t *testing.T) {
	l := &Loop{}
	if err := l.Wedged(); err != nil {
		t.Fatalf("wedged before start: %v, want nil", err)
	}
	ok, at, detail := l.LeaseHealth()
	if ok || !at.IsZero() || detail != "no lease attempted yet" {
		t.Fatalf("lease health before start: %v %v %q", ok, at, detail)
	}
}

// TestTheDeadlineFollowsTheChosenWait: the interval is the Coordinator's to
// choose, so a fixed watchdog would either restart workers told to come back
// in five minutes, or be too long to catch anything.
func TestTheDeadlineFollowsTheChosenWait(t *testing.T) {
	short := &Loop{}
	short.pulse.sleeping(time.Second)
	long := &Loop{}
	long.pulse.sleeping(5 * time.Minute)

	if long.pulse.due.Load() <= short.pulse.due.Load() {
		t.Fatal("a longer declared wait did not move the deadline out")
	}
	// Neither is wedged: both are within the grace of what they declared.
	if err := short.Wedged(); err != nil {
		t.Fatalf("short wait wedged: %v", err)
	}
	if err := long.Wedged(); err != nil {
		t.Fatalf("long wait wedged: %v", err)
	}
}

// TestAnUnregisteredWorkerIsNotReady is the regression this file exists to
// hold. It is the exact condition that shipped: authentication on, no
// credential to present, every lease refused five seconds apart, and two
// containers reporting themselves healthy over a deployment that had never
// worked.
func TestAnUnregisteredWorkerIsNotReady(t *testing.T) {
	l := &Loop{}
	l.pulse.sleeping(DefaultLeaseRetry)
	l.pulse.leased(errors.New("UNAUTHENTICATED: no bearer token"))

	ok, _, detail := l.Registered()
	if ok {
		t.Fatal("a worker the Coordinator refuses reported itself registered")
	}
	// The refusal has to survive into the probe's own words, because that is
	// where somebody reads it.
	if !strings.Contains(detail, "no bearer token") {
		t.Errorf("the reason was dropped on the way to the probe: %q", detail)
	}

	// And it must NOT be a wedge: a refused worker is ticking correctly and
	// restarting it fixes nothing.
	if err := l.Wedged(); err != nil {
		t.Errorf("a refused worker was reported as wedged, which would restart it: %v", err)
	}
}

// A worker that has not yet asked is not registered. Compose gives it a start
// period to get there; what it must not do is claim the fleet is complete
// while it is still on its way.
func TestAWorkerThatHasNotAskedYetIsNotRegistered(t *testing.T) {
	l := &Loop{}
	if ok, _, detail := l.Registered(); ok {
		t.Fatalf("registered before asking anybody: %q", detail)
	}
}

// An idle queue is a healthy worker. Being handed no work is the Coordinator
// accepting the request and answering it, which is the whole test.
func TestAnIdleWorkerIsRegistered(t *testing.T) {
	l := &Loop{}
	l.pulse.sleeping(DefaultLeaseRetry)
	l.pulse.leased(nil)

	ok, at, detail := l.Registered()
	if !ok {
		t.Fatalf("an accepted lease did not count as registration: %q", detail)
	}
	if at.IsZero() {
		t.Error("no time recorded for the accepted lease")
	}
}

// The second half of the gate. Without a staleness bound, a lease loop that
// stopped calling altogether would leave the last success recorded as good
// forever - the same false green in a different shape, and one that liveness
// alone does not cover because compose does not act on liveness.
func TestARegistrationGoesStale(t *testing.T) {
	l := &Loop{}
	l.pulse.sleeping(DefaultLeaseRetry)
	l.pulse.leased(nil)
	// Wound back past the bound rather than waited out.
	l.pulse.leaseAt.Store(time.Now().Add(-RegistrationStale - time.Second).UnixNano())

	ok, _, detail := l.Registered()
	if ok {
		t.Fatal("a worker that stopped asking still reported itself registered")
	}
	if !strings.Contains(detail, "stopped asking") {
		t.Errorf("the report does not say what happened: %q", detail)
	}
}
