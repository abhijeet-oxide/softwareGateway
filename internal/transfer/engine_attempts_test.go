package transfer

import "testing"

// The attempt caps existed as a function nothing called, so every job got the
// column default of eight whatever went wrong. These assert the policy in
// docs/design/11 §2.3 now that it is actually consulted.
func TestAttemptCapsFollowTheFailure(t *testing.T) {
	for class, want := range map[string]int{
		// Registries recover.
		"transient":    8,
		"unavailable":  8,
		"timeout":      8,
		"rate_limited": 8,
		// The destination reporting a stale placement self-heals once the
		// blobs are requeued, so it keeps the full budget.
		ClassBlobUnknown: 8,
		// Retrying rarely helps and it may be real corruption worth surfacing.
		"integrity": 2,
		// Credentials do not fix themselves, and hammering an auth endpoint
		// gets the whole fleet throttled.
		"auth":        1,
		"not_found":   1,
		"unsupported": 1,
		// This worker cannot execute the job. Another might, so it is not
		// hopeless - but eight tries against the same misconfigured fleet only
		// burns the attempts a real retry would need.
		ClassConfiguration: 1,
	} {
		if got := MaxAttemptsFor(class); got != want {
			t.Errorf("%s gets %d attempts, want %d", class, got, want)
		}
	}
}
