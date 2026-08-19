package worker

import (
	"context"
	"sync"
	"time"
)

// A bound on how long ONE job may make no progress.
//
// # The gap this closes
//
// Leases protect against a dead worker. A worker that is SIGKILLed stops
// heartbeating, its leases expire, and the reaper returns its work to the queue
// - the whole crash-recovery story, and it works.
//
// It has nothing to say about a live worker holding a job that is going
// nowhere. The heartbeat reports which job IDs the worker holds; it does not
// ask whether any of them is moving. So a request that never returns - a proxy
// that accepted a connection and went quiet, a registry that took the body and
// stopped - is held forever, heartbeated forever, and invisible: the transfer
// reports one job in flight, zero failed, and does not move again.
//
// That happened. One manifest job, one worker, and a transfer that sat at
// 2491 of 2493 with nothing failing and nothing to look at.
//
// The transport's own timeouts do not cover it. ResponseHeaderTimeout starts
// after the request body has been written, so a stalled UPLOAD is outside it,
// and there is deliberately no client-level timeout because that would cap a
// legitimate multi-hour transfer of a large blob.
//
// # Why a stall timeout rather than a deadline
//
// A deadline cannot be both correct for an 8 GB blob and useful for a manifest.
// At the rates this system actually sees, that blob is an eight-hour job - so
// any deadline generous enough for it leaves a stuck manifest hanging for most
// of a day.
//
// What distinguishes the two is not elapsed time, it is MOVEMENT. A blob that
// is transferring reports progress; the watchdog is beaten and the job may run
// as long as it needs. A blob that has stopped reports nothing, and so does a
// manifest that will never return, and both are caught by the same clock.
//
// A manifest job reports no progress at all - it is one small request - so for
// it this is a flat deadline, which is the right shape: a manifest push that
// has not answered in minutes is not going to.
type watchdog struct {
	mu     sync.Mutex
	timer  *time.Timer
	after  time.Duration
	fired  bool
	cancel context.CancelFunc
}

// newWatchdog starts the clock. A zero or negative timeout disables it.
func newWatchdog(cancel context.CancelFunc, after time.Duration) *watchdog {
	w := &watchdog{after: after, cancel: cancel}
	if after <= 0 {
		return w
	}
	w.timer = time.AfterFunc(after, w.trip)
	return w
}

// beat reports that the job moved, and pushes the deadline out.
func (w *watchdog) beat() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.timer == nil || w.fired {
		return
	}
	w.timer.Reset(w.after)
}

// stop releases the timer once the job is finished with, however it finished.
func (w *watchdog) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.timer != nil {
		w.timer.Stop()
	}
}

// tripped reports that this job was cancelled by the watchdog rather than by a
// shutdown.
//
// The distinction decides what is REPORTED. A job cancelled by shutdown is not
// a failure - its lease expires and another worker takes it. A job cancelled
// for stalling is a failure worth recording, with a message saying so, because
// otherwise the only trace of a registry that stops answering is a job that
// quietly ran twice.
func (w *watchdog) tripped() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.fired
}

func (w *watchdog) trip() {
	w.mu.Lock()
	w.fired = true
	w.mu.Unlock()

	w.cancel()
}
