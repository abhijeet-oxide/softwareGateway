package transport

import (
	"net/http"
	"testing"
)

// TestSharedClientsShareOnePool is the fix for a limit that silently became a
// per-repository allowance.
//
// Every repository used to build its own transport stack, so `maxConnections:
// 32` across sixteen repositories scanned in parallel permitted 512 concurrent
// connections to one host, and `requestsPerSecond: 50` permitted 800. Through a
// corporate proxy that is not a faster scan.
func TestSharedClientsShareOnePool(t *testing.T) {
	shared, err := NewShared(Config{
		Registry: "registry.example.com", MaxConnections: 32,
		RequestsPerSecond: 50, Burst: 100, ForceHTTP1: true,
	})
	if err != nil {
		t.Fatalf("new shared: %v", err)
	}

	a := shared.Client(Config{Registry: "registry.example.com", Repository: "orbs/a"})
	b := shared.Client(Config{Registry: "registry.example.com", Repository: "orbs/b"})

	if baseOf(t, a) != baseOf(t, b) {
		t.Error("two repositories on one source got different connection pools; " +
			"maxConnections is then a per-repository allowance, not a ceiling")
	}
	if limiterOf(t, a) != limiterOf(t, b) {
		t.Error("two repositories on one source got different rate limiters; " +
			"requestsPerSecond is then multiplied by the repository count")
	}
	if cacheOf(t, a) != cacheOf(t, b) {
		t.Error("two repositories on one source got different token caches; " +
			"each would then perform its own token exchange")
	}
}

// A standalone client must still work, and must not accidentally share with
// anything - preflight probes and one-off catalog clients rely on it.
func TestStandaloneClientsShareNothing(t *testing.T) {
	a, err := New(Config{Registry: "r.example.com", Repository: "x", ForceHTTP1: true})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	b, err := New(Config{Registry: "r.example.com", Repository: "y", ForceHTTP1: true})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if baseOf(t, a) == baseOf(t, b) {
		t.Error("two standalone clients unexpectedly share a pool")
	}
}

func TestUnlimitedRateSkipsTheLimiter(t *testing.T) {
	shared, err := NewShared(Config{Registry: "r.example.com", RequestsPerSecond: 0})
	if err != nil {
		t.Fatalf("new shared: %v", err)
	}
	if shared.limiter != nil {
		t.Error("rps of zero must mean unlimited, not a zero-rate bucket that " +
			"blocks every request forever")
	}
	// And the chain must still be usable.
	if c := shared.Client(Config{Registry: "r.example.com", Repository: "x"}); c == nil {
		t.Fatal("nil client")
	}
}

// ---- helpers ----

func baseOf(t *testing.T, c *http.Client) *http.Transport {
	t.Helper()
	return tlsConfigOwner(t, c)
}

func limiterOf(t *testing.T, c *http.Client) any {
	t.Helper()
	for rt := c.Transport; ; {
		switch v := rt.(type) {
		case *rateLimitTransport:
			return v.limiter
		case *traceTransport:
			rt = v.next
		case *userAgentTransport:
			rt = v.next
		case *retryTransport:
			rt = v.next
		case *authTransport:
			rt = v.next
		default:
			t.Fatalf("no rate limiter in the chain, reached %T", rt)
			return nil
		}
	}
}

func cacheOf(t *testing.T, c *http.Client) any {
	t.Helper()
	for rt := c.Transport; ; {
		switch v := rt.(type) {
		case *authTransport:
			return v.cache
		case *traceTransport:
			rt = v.next
		case *userAgentTransport:
			rt = v.next
		case *rateLimitTransport:
			rt = v.next
		case *retryTransport:
			rt = v.next
		default:
			t.Fatalf("no auth transport in the chain, reached %T", rt)
			return nil
		}
	}
}

func tlsConfigOwner(t *testing.T, c *http.Client) *http.Transport {
	t.Helper()
	for rt := c.Transport; ; {
		switch v := rt.(type) {
		case *http.Transport:
			return v
		case *traceTransport:
			rt = v.next
		case *userAgentTransport:
			rt = v.next
		case *rateLimitTransport:
			rt = v.next
		case *retryTransport:
			rt = v.next
		case *authTransport:
			rt = v.next
		default:
			t.Fatalf("unexpected round tripper %T", rt)
			return nil
		}
	}
}
