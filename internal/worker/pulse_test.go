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
