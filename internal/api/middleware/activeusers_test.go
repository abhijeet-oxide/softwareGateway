package middleware

import (
	"testing"
	"time"
)

// THE COUNT HAS TO FALL.
//
// A set that only ever grows is a "users" number that climbs for the life of
// the process and never drops - which is worse than not having one, because it
// reads as growth. The whole value of this metric is that it goes DOWN when
// people stop coming back, and that only happens if entries expire.
func TestActiveUsersForgetsPeopleWhoStopped(t *testing.T) {
	const window = 15 * time.Minute
	set := newActiveSet(window)
	t0 := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)

	if n := set.touch("alice", t0); n != 1 {
		t.Fatalf("first caller counted %d, want 1", n)
	}
	if n := set.touch("bob", t0.Add(time.Minute)); n != 2 {
		t.Fatalf("second caller counted %d, want 2", n)
	}
	// Alice comes back, so she is still active.
	if n := set.touch("alice", t0.Add(10*time.Minute)); n != 2 {
		t.Fatalf("a returning caller counted %d, want 2", n)
	}

	// Past the window for Bob but not for Alice.
	if n := set.touch("alice", t0.Add(20*time.Minute)); n != 1 {
		t.Errorf("counted %d after a caller went quiet for longer than %v, want 1.\n"+
			"An active-user count that never falls reads as growth, which is the\n"+
			"opposite of what it is for: noticing that people stopped coming back.",
			n, window)
	}
}

// The same person on ten tabs is one person.
func TestActiveUsersCountsPeopleNotRequests(t *testing.T) {
	set := newActiveSet(time.Minute)
	now := time.Now()
	for range 10 {
		set.touch("alice", now)
	}
	if n := set.touch("alice", now); n != 1 {
		t.Errorf("ten requests from one subject counted %d, want 1", n)
	}
}
