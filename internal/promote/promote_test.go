package promote_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/promote"
)

// The registry's job is to be BORING and DETERMINISTIC. A plugin registry
// whose answer depends on import order reproduces everywhere except where it
// was reported, so the order is pinned here rather than left to a map.

type fake struct {
	name   string
	claims bool
}

func (f fake) Name() string { return f.name }

func (f fake) Claim(promote.Hop) promote.Verdict {
	return promote.Verdict{Claimed: f.claims, Reason: f.name + " spoke"}
}

func (f fake) Promote(context.Context, promote.Hop) (promote.Outcome, error) {
	return promote.Outcome{Promoter: f.name, Promoted: 1}, nil
}

// registerOnce keeps the package-level registry usable from several tests
// without them fighting over names.
var registered sync.Once

func register(t *testing.T) {
	t.Helper()
	registered.Do(func() {
		// Named so the sorted order is NOT the order they are registered in:
		// if resolution followed registration, "zulu" would answer first.
		promote.Register("zulu", func(promote.Config) (promote.Promoter, error) {
			return fake{name: "zulu", claims: claimZulu.Load()}, nil
		})
		promote.Register("alpha", func(promote.Config) (promote.Promoter, error) {
			return fake{name: "alpha", claims: claimAlpha.Load()}, nil
		})
	})
}

var claimAlpha, claimZulu atomicBool

type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (a *atomicBool) Load() bool     { a.mu.Lock(); defer a.mu.Unlock(); return a.v }
func (a *atomicBool) Store(val bool) { a.mu.Lock(); defer a.mu.Unlock(); a.v = val }

func aHop() promote.Hop {
	return promote.Hop{
		Product: "nokia", Package: "v1",
		Names: []promote.Name{{Repository: "orbs/cfx", Tag: "v1", Digest: "sha256:aa"}},
	}
}

// Nothing claiming is the ORDINARY answer, not a failure: it means the bytes
// are copied, which is what every hop did before plugins existed.
func TestNothingClaimingIsNotAnError(t *testing.T) {
	register(t)
	claimAlpha.Store(false)
	claimZulu.Store(false)

	res, err := promote.Resolve(promote.Config{}, aHop())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Promoter != nil {
		t.Fatal("nothing should have claimed")
	}
	// Every decline is carried, so a dry run can say WHY the fast path is
	// unavailable rather than merely that it is. With SEVERAL, each is named -
	// with one, the name is dropped, because the reason already says what
	// happened and the prefix is a word to parse first.
	if reason := res.DeclinedReason(); !strings.Contains(reason, "alpha") ||
		!strings.Contains(reason, "zulu") {
		t.Errorf("both declines must be reported; got %q", reason)
	}
	if len(res.Declined) != 2 {
		t.Fatalf("%d declines carried, want 2", len(res.Declined))
	}
	one := promote.Resolution{Declined: res.Declined[:1]}
	if got := one.DeclinedReason(); got != res.Declined[0].Reason {
		t.Errorf("a single decline must be its reason alone; got %q", got)
	}
}

func TestTheClaimingPromoterIsReturned(t *testing.T) {
	register(t)
	claimAlpha.Store(false)
	claimZulu.Store(true)

	res, err := promote.Resolve(promote.Config{}, aHop())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Promoter == nil || res.Promoter.Name() != "zulu" {
		t.Fatalf("want zulu, got %v", res.Promoter)
	}
	if !res.Verdict.Claimed || res.Verdict.Promoter != "zulu" {
		t.Errorf("the verdict must be stamped with who answered: %+v", res.Verdict)
	}
}

// Two claims is a configuration error, refused rather than resolved by
// precedence. A precedence rule would silently pick one, and the operator
// whose hop went the slower way would have nothing to read.
func TestTwoClaimsAreRefusedAndNameBoth(t *testing.T) {
	register(t)
	claimAlpha.Store(true)
	claimZulu.Store(true)
	t.Cleanup(func() { claimAlpha.Store(false); claimZulu.Store(false) })

	_, err := promote.Resolve(promote.Config{}, aHop())
	if err == nil {
		t.Fatal("two promoters claiming one hop must be refused")
	}
	for _, want := range []string{"alpha", "zulu"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must name both plugins; %q omits %q", err, want)
		}
	}
}

// A hop with nothing to publish is not a hop, and a promoter handed one would
// report success having done nothing.
func TestAHopWithNoNamesIsRefused(t *testing.T) {
	register(t)
	if _, err := promote.Resolve(promote.Config{}, promote.Hop{Package: "v1"}); err == nil {
		t.Fatal("a hop with no names must be refused")
	}
}

func TestPromotersAreListedInNameOrder(t *testing.T) {
	register(t)
	got := promote.Promoters()
	if len(got) < 2 || got[0] != "alpha" {
		t.Fatalf("Promoters() must be sorted by name, got %v", got)
	}
}
