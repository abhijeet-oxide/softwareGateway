package transfer

import "testing"

// The key is what makes a repeated request one piece of work rather than two,
// so what it does and does not distinguish IS the behaviour.

func TestIdempotencyKeyIsOrderIndependent(t *testing.T) {
	a := IdempotencyKey("replicate", 42, 7, []int64{3, 1, 2}, "", 50)
	b := IdempotencyKey("replicate", 42, 7, []int64{1, 2, 3}, "", 50)
	if a != b {
		t.Error("target order changed the key: a cosmetic YAML reorder would duplicate every pending request")
	}
}

func TestIdempotencyKeyDistinguishesIntent(t *testing.T) {
	base := IdempotencyKey("replicate", 42, 7, []int64{1}, "", 50)

	for name, got := range map[string]string{
		"different operation": IdempotencyKey("promote", 42, 7, []int64{1}, "", 50),
		"different package":   IdempotencyKey("replicate", 43, 7, []int64{1}, "", 50),
		"different target":    IdempotencyKey("replicate", 42, 7, []int64{2}, "", 50),
		"extra target":        IdempotencyKey("replicate", 42, 7, []int64{1, 2}, "", 50),
		"different schedule":  IdempotencyKey("replicate", 42, 7, []int64{1}, "2026-01-01T00:00:00Z", 50),
		// Re-requesting at a different priority is a distinct intent and is
		// deliberately allowed.
		"different priority": IdempotencyKey("replicate", 42, 7, []int64{1}, "", 10),
		// THE ONE THAT WAS MISSING. Promoting to production from lab-eu and
		// from lab-us is the same operation, package, target and priority.
		// Without the origin in the key the second silently returned the
		// first's request and promoted from the wrong region.
		"different origin": IdempotencyKey("replicate", 42, 8, []int64{1}, "", 50),
	} {
		if got == base {
			t.Errorf("%s produced the same key", name)
		}
	}
}
