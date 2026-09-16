package main

import "testing"

// Which listen addresses reach beyond this pod.
//
// This decides whether the Coordinator warns that the profiler is publishing
// the heap - registry credentials included - to the pod network. The case that
// matters is ":6060", because it is both the shortest thing to type and the
// one that binds every interface.
func TestLoopbackAddr(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:6060", true},
		{"localhost:6060", true},
		{"[::1]:6060", true},
		{"127.0.0.5:6060", true},

		// Everything below reaches the pod network.
		{":6060", false},        // every interface, and the easiest mistake
		{"0.0.0.0:6060", false}, // the same thing said out loud
		{"[::]:6060", false},
		{"10.0.0.7:6060", false},
		{"coordinator:6060", false}, // a service name is not loopback

		// Not an address at all. The listener will fail on it; warning is the
		// safe answer for something nobody can read.
		{"6060", false},
		{"", false},
	} {
		if got := loopbackAddr(tc.addr); got != tc.want {
			t.Errorf("loopbackAddr(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}
