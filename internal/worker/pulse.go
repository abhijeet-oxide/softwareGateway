package worker

import (
	"fmt"
	"sync/atomic"
	"time"
)

/*
The loop's own heartbeat, and the only thing in this package the process's
probes read.

# Why a worker needs one at all

Liveness for a service that SERVES traffic is nearly free: if it answers the
probe, the thing it does is working. A worker serves nothing. Its probe server
is a separate goroutine from the lease loop, so a loop that has deadlocked, or
whose goroutine has died, leaves a process that answers every probe
cheerfully and does no work at all - forever, because nothing restarts it.
That is the failure this exists to catch, and it is the failure a worker is
most likely to have.

# Why the deadline moves rather than being a fixed interval

The interval between ticks is not this process's to choose. A saturated worker
waits `fullWorkerRecheck`, an idle one waits however long the Coordinator's
last lease response told it to, and a busy one goes round immediately on every
completion. A fixed watchdog would either restart workers the Coordinator had
told to come back in five minutes, or be so long that it never caught anything.

So the loop declares its own deadline: each time round it says how long it
intends to sleep, and being late by more than `pulseGrace` on top of THAT is
what counts as wedged.
*/

// pulseGrace is how far past its own declared wake-up a loop may be before the
// process calls itself wedged.
//
// Two minutes, which is long against every interval the loop actually uses
// (5s to 30s, or whatever the Coordinator asked for) and short against how
// long anybody would want a silently dead worker to keep holding a slot in
// the fleet. It absorbs a scheduler that has descheduled the process and a
// lease call that is timing out slowly, neither of which is a wedge.
const pulseGrace = 2 * time.Minute

type pulse struct {
	// due is the unix nano after which the next tick is overdue. Atomic
	// because the probe server reads it from another goroutine on every
	// liveness check.
	due atomic.Int64

	leaseAt atomic.Int64 // unix nano of the last lease attempt
	leaseOK atomic.Bool
	// leaseErr is what the last failed lease said, for a diagnostic that can
	// name the cause rather than only the symptom. A pointer so the zero value
	// is "nothing has failed yet" rather than an empty string that reads the
	// same as a cleared one.
	leaseErr atomic.Pointer[string]
}

// sleeping records that the loop is about to wait for `wait` before its next
// pass. Called by the loop, once per pass, and by Run before the first one.
func (p *pulse) sleeping(wait time.Duration) {
	p.due.Store(time.Now().Add(wait + pulseGrace).UnixNano())
}

// leased records the outcome of one lease call.
func (p *pulse) leased(err error) {
	p.leaseAt.Store(time.Now().UnixNano())
	if err == nil {
		p.leaseOK.Store(true)
		p.leaseErr.Store(nil)
		return
	}
	p.leaseOK.Store(false)
	msg := err.Error()
	p.leaseErr.Store(&msg)
}

// Wedged reports that the lease loop has stopped going round, and nil while it
// is going round on whatever schedule it chose.
//
// PROCESS-LOCAL, which is what makes it fit for a liveness probe: it reads two
// integers this process wrote and makes no call to anything. A worker that
// cannot reach the Coordinator is NOT wedged - it is ticking, failing, backing
// off and ticking again, which is the correct behaviour and must never restart
// the container.
func (l *Loop) Wedged() error {
	due := l.pulse.due.Load()
	if due == 0 {
		// The loop has not started yet. Not a wedge: a process that is still
		// wiring itself up must not be restarted for not having begun.
		return nil
	}
	if late := time.Since(time.Unix(0, due)); late > 0 {
		return fmt.Errorf("lease loop has not run for %s past its own next wake-up", late.Round(time.Second))
	}
	return nil
}

// LeaseHealth reports what happened on the last attempt to lease work.
//
// A DIAGNOSTIC, deliberately not readiness. A worker that cannot lease is
// still correctly running the jobs it already holds, and the two most common
// reasons it cannot - a Coordinator restarting, and a Coordinator refusing
// this worker's credentials - are neither of them fixed by anything that
// happens to this container.
func (l *Loop) LeaseHealth() (ok bool, at time.Time, detail string) {
	nanos := l.pulse.leaseAt.Load()
	if nanos == 0 {
		return false, time.Time{}, "no lease attempted yet"
	}
	at = time.Unix(0, nanos)
	if l.pulse.leaseOK.Load() {
		return true, at, "leasing"
	}
	if msg := l.pulse.leaseErr.Load(); msg != nil {
		return false, at, *msg
	}
	return false, at, "the last lease did not succeed"
}
