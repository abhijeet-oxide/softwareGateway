package worker

import (
	"context"
	"testing"
	"time"
)

// Leases catch a dead worker. Nothing caught a live worker holding a job that
// was going nowhere: the heartbeat renews on the worker being alive, not on the
// job moving, so a request that never returns is held forever and reported as
// one job in flight, zero failed, on a transfer that does not move again.

func TestAJobThatStopsMovingIsAbandoned(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	dog := newWatchdog(cancel, 50*time.Millisecond)
	defer dog.stop()

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("a job that reported no progress was never abandoned")
	}
	if !dog.tripped() {
		t.Error("the cancellation is not attributable to the watchdog, so it would be " +
			"reported as a shutdown rather than as a stall")
	}
}

// AND A JOB THAT IS MOVING IS NEVER ABANDONED, at any duration. This is the
// half that makes a stall timeout usable at all: an 8 GB blob on a slow link is
// legitimately an eight-hour job, and any fixed deadline generous enough for it
// would leave a stuck manifest hanging for most of a day.
func TestATransferringJobIsNeverAbandoned(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	dog := newWatchdog(cancel, 80*time.Millisecond)
	defer dog.stop()

	// Ten beats at half the timeout: comfortably longer than the timeout in
	// total, and never silent for it.
	for range 10 {
		time.Sleep(40 * time.Millisecond)
		dog.beat()
		if ctx.Err() != nil {
			t.Fatal("a job reporting progress was abandoned mid-transfer")
		}
	}
	if dog.tripped() {
		t.Error("the watchdog tripped on a job that was moving")
	}
}

// Stopping is what every finished job does, however it finished. A timer left
// running would cancel a context that has been reused, or fire against a job
// that succeeded twenty minutes ago.
func TestAFinishedJobStopsTheClock(t *testing.T) {
	_, cancel := context.WithCancel(t.Context())
	defer cancel()

	dog := newWatchdog(cancel, 50*time.Millisecond)
	dog.stop()

	time.Sleep(200 * time.Millisecond)
	if dog.tripped() {
		t.Error("the watchdog fired after the job had finished")
	}
}

// Disabled is a supported configuration — a deployment that would rather hang
// than risk abandoning a legitimate long job must be able to say so.
func TestTheWatchdogCanBeDisabled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	dog := newWatchdog(cancel, -1)
	defer dog.stop()

	time.Sleep(150 * time.Millisecond)
	if ctx.Err() != nil || dog.tripped() {
		t.Error("a disabled watchdog cancelled a job")
	}
}
