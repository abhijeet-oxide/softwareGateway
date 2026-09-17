package middleware

import (
	"net/http"
	"sync"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/metrics"
)

// How far back "active" reaches.
//
// Fifteen minutes rather than a day, because the question this answers is "is
// anybody using this right now" - the number to put beside a latency graph
// when deciding whether a slow afternoon mattered. A daily figure answers a
// different question and a reporting system should answer that one, from the
// audit log, where the record is durable and the arithmetic can be redone.
const activeUserWindow = 15 * time.Minute

// ActiveUsers counts the distinct people using the service.
//
// # Why this is not a label on api_requests_total
//
// Because a label per user is a time series per user, forever - a metric that
// grows with every person who ever signs in and never shrinks. The package
// comment in internal/platform/metrics is explicit about it. This keeps the
// identities in memory, where they expire, and publishes only the COUNT.
//
// # Why it exists at all
//
// A user who finds the tool slow does not file a ticket; they stop opening it.
// Request rate cannot tell that apart from a quiet week, because one person
// refreshing a dashboard makes more requests than ten people doing a day's
// work. This can: when latency rose and this fell, the two are the same event.
//
// # What is not counted
//
// Workload identities and unauthenticated callers. A Kubernetes service
// account polling an endpoint is not a person, and counting it would put a
// floor under the number that never moves - which is the one thing that would
// make it useless for noticing people leaving.
func ActiveUsers(reg *metrics.Registry) func(http.Handler) http.Handler {
	seen := newActiveSet(activeUserWindow)
	reg.ActiveUsers.Set(0)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)

			// AFTER the handler, because the identity is established by the
			// authentication middleware inside this one. Before it, every
			// request is anonymous and the count is always zero.
			id := IdentityFrom(r.Context())
			if id.Subject == "" || id.Subject == Anonymous.Subject || id.Method == "kubernetes" {
				return
			}
			reg.ActiveUsers.Set(float64(seen.touch(id.Subject, time.Now())))
		})
	}
}

// activeSet is the distinct subjects seen within a window.
//
// A map with expiry rather than anything cleverer: it holds one entry per
// person who has made a request in the last fifteen minutes, which is the
// size of the team, and the sweep is over that same set. An HLL or a
// bloom filter would be the right structure at a thousand times this scale and
// the wrong one at this one, where exactness is free.
type activeSet struct {
	mu     sync.Mutex
	window time.Duration
	last   map[string]time.Time
}

func newActiveSet(window time.Duration) *activeSet {
	return &activeSet{window: window, last: map[string]time.Time{}}
}

// touch records a subject and returns how many are currently active.
//
// The sweep happens here rather than on a timer so there is no goroutine to
// own, and because the only moment the number can be wrong in a way anybody
// sees is the moment it is read.
func (a *activeSet) touch(subject string, now time.Time) int {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.last[subject] = now
	cutoff := now.Add(-a.window)
	for s, t := range a.last {
		if t.Before(cutoff) {
			delete(a.last, s)
		}
	}
	return len(a.last)
}
