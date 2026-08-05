package backoff

import (
	"testing"
	"time"
)

func TestDelayNeverExceedsCap(t *testing.T) {
	p := Policy{Base: time.Second, Cap: 5 * time.Minute}
	for n := 0; n < 200; n++ {
		for i := 0; i < 20; i++ {
			if d := p.Delay(n); d > p.Cap {
				t.Fatalf("attempt %d produced %v, above cap %v", n, d, p.Cap)
			}
		}
	}
}

func TestDelayHandlesHugeAttemptCountsWithoutOverflow(t *testing.T) {
	// 1s << 200 overflows int64 many times over. A naive shift wraps to a
	// negative duration, which would make rand.Int64N panic.
	p := Policy{Base: time.Second, Cap: time.Minute}
	for _, n := range []int{61, 62, 63, 64, 1000, 1 << 30} {
		d := p.Delay(n)
		if d < 0 || d > p.Cap {
			t.Fatalf("attempt %d produced out-of-range %v", n, d)
		}
	}
}

func TestDelayIsNonNegativeAndJittered(t *testing.T) {
	p := Policy{Base: time.Second, Cap: time.Minute}
	seen := make(map[time.Duration]bool)
	for i := 0; i < 100; i++ {
		d := p.Delay(5)
		if d < 0 {
			t.Fatalf("negative delay %v", d)
		}
		seen[d] = true
	}
	// Full jitter must actually spread. Identical values across 100 draws
	// would mean the jitter was lost, which is the bug that turns a blip into
	// a synchronized retry storm.
	if len(seen) < 10 {
		t.Fatalf("expected jittered delays, got only %d distinct values", len(seen))
	}
}

func TestZeroPolicyUsesDefaults(t *testing.T) {
	var p Policy
	for i := 0; i < 50; i++ {
		if d := p.Delay(3); d < 0 || d > DefaultCap {
			t.Fatalf("zero policy produced %v", d)
		}
	}
}

func TestNegativeAttemptTreatedAsZero(t *testing.T) {
	p := Policy{Base: time.Second, Cap: time.Minute}
	if d := p.Delay(-5); d < 0 || d > time.Second {
		t.Fatalf("negative attempt produced %v, want within first-attempt ceiling", d)
	}
}

func TestMaxAttemptsExhausted(t *testing.T) {
	if !AttemptsTerminal.Exhausted(1) {
		t.Fatal("terminal class must be exhausted after 1 attempt")
	}
	if AttemptsIntegrity.Exhausted(1) {
		t.Fatal("integrity class allows 2 attempts")
	}
	if !AttemptsIntegrity.Exhausted(2) {
		t.Fatal("integrity class must be exhausted after 2")
	}
	if AttemptsTransient.Exhausted(7) {
		t.Fatal("transient class allows 8 attempts")
	}
	if !AttemptsTransient.Exhausted(8) {
		t.Fatal("transient class must be exhausted after 8")
	}
}
