package main

import (
	"strings"
	"testing"
	"time"
)

// The flags that turn into a measurement, and the arithmetic that decides how
// long to wait for it.

func TestParseByteSizeDistinguishesBinaryFromDecimal(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"1024", 1024},
		{"30GiB", 30 << 30},
		{"30gib", 30 << 30},
		// GB is 1000³ and GiB is 1024³. Treating them as the same puts a 7%
		// error into a projection printed to three significant figures.
		{"1GB", 1_000_000_000},
		{"1G", 1 << 30},
		{"1.5MiB", 1572864},
		{"500 MB", 500_000_000},
	}
	for _, c := range cases {
		got, err := parseByteSize(c.in)
		if err != nil {
			t.Errorf("parseByteSize(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseByteSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}

	if _, err := parseByteSize("thirty gigabytes"); err == nil {
		t.Error("prose was accepted as a size")
	}
}

func TestParseLevelsRejectsWhatCannotBeASweep(t *testing.T) {
	got, err := parseLevels("1,2,4, 8")
	if err != nil {
		t.Fatalf("parseLevels: %v", err)
	}
	if len(got) != 4 || got[3] != 8 {
		t.Errorf("levels = %v, want 1 2 4 8", got)
	}

	for _, bad := range []string{"0", "-1", "four", "1,,x"} {
		if _, err := parseLevels(bad); err == nil {
			t.Errorf("parseLevels(%q) was accepted", bad)
		}
	}
}

// The estimate is what the operator is told to expect AND what the client
// timeout is built from, so it must scale with the flags rather than sit at a
// constant that a long sweep silently blows through.
func TestRuntimeEstimateScalesWithTheSweep(t *testing.T) {
	short := estimatedRuntime([]int{1, 2}, time.Second, false)
	long := estimatedRuntime([]int{1, 2, 4, 8, 16, 32}, 10*time.Second, true)

	if long <= short*10 {
		t.Errorf("a six-level ten-second two-sided sweep estimated %s against %s for a "+
			"two-level one-second one-sided one", long, short)
	}
	if short <= 0 {
		t.Error("estimate of zero")
	}
}

// Evidence is wrapped rather than truncated: it is the justification, and a
// justification cut off mid-sentence is worse than none.
func TestWrapBreaksOnWordsAndKeepsEverything(t *testing.T) {
	text := "direct moved 18.0 MiB/s against 2.1 MiB/s through the proxy - 757% faster"
	lines := wrap(text, 30)

	if len(lines) < 2 {
		t.Fatalf("wrapped to %d line(s): %v", len(lines), lines)
	}
	for _, l := range lines {
		if len(l) > 30 && !strings.Contains(l, " ") {
			t.Errorf("line %q exceeds the width and cannot be broken", l)
		}
	}
	if got := strings.Join(lines, " "); got != text {
		t.Errorf("wrapping lost or changed text:\n got %q\nwant %q", got, text)
	}
}
