// Package backoff implements exponential backoff with full jitter.
//
// See docs/design/10-state-machines.md section 6.
package backoff

import (
	"math/rand/v2"
	"time"
)

// Defaults from docs/design/10-state-machines.md section 6.
const (
	DefaultBase = 1 * time.Second
	DefaultCap  = 5 * time.Minute
)

// Policy computes retry delays.
//
// Full jitter - delay is uniform in [0, exponential) rather than the
// exponential value itself - because failures here are strongly correlated: one
// registry returning 503 fails every in-flight job at once. Without jitter they
// retry in lockstep, re-hammering a struggling registry in synchronized waves
// and turning a blip into an outage.
type Policy struct {
	Base time.Duration // first-attempt ceiling; zero means DefaultBase
	Cap  time.Duration // maximum ceiling; zero means DefaultCap
}

// Delay returns the wait before attempt n (zero-based: n=0 is the delay after
// the first failure). Negative n is treated as 0.
func (p Policy) Delay(n int) time.Duration {
	base, max := p.Base, p.Cap
	if base <= 0 {
		base = DefaultBase
	}
	if max <= 0 {
		max = DefaultCap
	}
	if n < 0 {
		n = 0
	}

	ceiling := max
	// Shifting past 62 overflows int64; the value is far above any sane cap
	// well before that, so clamp rather than wrap. n is non-negative by the
	// clamp above, so shifting a Duration directly by it is safe and avoids
	// an int-to-uint conversion the compiler cannot prove is in range.
	if n < 62 {
		if scaled := base << n; scaled > 0 && scaled < max {
			ceiling = scaled
		}
	}

	// A weak RNG is correct here: this is retry jitter, not a secret. Nothing
	// about the delay is security-sensitive, and drawing from crypto/rand
	// would add a syscall to every retry for no benefit. The exemption is
	// deliberately local rather than a project-wide gosec exclusion, so a real
	// misuse elsewhere still fails the lint.
	return time.Duration(rand.Int64N(int64(ceiling) + 1)) //nolint:gosec // retry jitter, not security-sensitive
}

// MaxAttempts caps retries per error class.
// See docs/design/10-state-machines.md section 6 for the rationale behind each.
type MaxAttempts int

const (
	// AttemptsTransient covers 5xx, timeouts and 429. Registries recover.
	AttemptsTransient MaxAttempts = 8
	// AttemptsIntegrity covers digest mismatch. Retrying rarely helps and may
	// indicate real corruption worth surfacing rather than absorbing.
	AttemptsIntegrity MaxAttempts = 2
	// AttemptsTerminal covers auth failures, missing source content and
	// unsupported capabilities. Credentials do not fix themselves, and
	// hammering an auth endpoint gets us throttled.
	AttemptsTerminal MaxAttempts = 1
)

// Exhausted reports whether a job that has made `attempts` attempts should stop.
func (m MaxAttempts) Exhausted(attempts int) bool { return attempts >= int(m) }
