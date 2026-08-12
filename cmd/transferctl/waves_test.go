package main

import (
	"bytes"
	"strings"
	"testing"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// THE TOTALS CANNOT EXPLAIN AN IDLE-LOOKING TRANSFER.
//
// "519 outstanding, 2 in flight" mixes three populations that behave completely
// differently — runnable now, waiting out a backoff, and gated behind a wave
// that has not drained — and only the last one explains why a fleet with
// capacity to spare is running two jobs.

func realisticWaves() []v1.TransferWave {
	return []v1.TransferWave{
		{Wave: 0, Kind: "blob", Current: true, Total: 1976, Done: 1974, Running: 2,
			PlannedBytes: "66600000000", TransferredBytes: "66460000000"},
		{Wave: 1, Kind: "manifest", Total: 431, Blocked: 431,
			PlannedBytes: "1900000", TransferredBytes: "0"},
		{Wave: 2, Kind: "manifest", Total: 78, Blocked: 78,
			PlannedBytes: "340000", TransferredBytes: "0"},
	}
}

func TestEachWaveShowsCompletionAgainstItsOwnTotal(t *testing.T) {
	var out bytes.Buffer
	if err := renderWaves(&out, realisticWaves()); err != nil {
		t.Fatal(err)
	}
	text := out.String()

	for _, want := range []string{
		"1974/1976", // wave 0: two blobs left, not "519 outstanding"
		"0/431",     // wave 1: full, and none of it started
		"0/78",
		"blob", "manifest", // which kind each wave holds
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the wave table does not show %q:\n%s", want, text)
		}
	}

	// The current wave is marked, because everything above it is finished and
	// everything below it cannot start.
	var marked int
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), ">") {
			marked++
		}
	}
	if marked != 1 {
		t.Errorf("%d wave(s) marked as current, want exactly 1:\n%s", marked, text)
	}
}

// Zeroes are dashes so the eye lands on the numbers that are not zero — which
// in a wave table is nearly all of the information.
func TestZeroCountsAreDashes(t *testing.T) {
	if got := count(0); got != "-" {
		t.Errorf("count(0) = %q, want -", got)
	}
	if got := count(431); got != "431" {
		t.Errorf("count(431) = %q", got)
	}
}

// Manifest waves are kilobytes and blob waves are the whole transfer. Seeing
// them side by side is why wave 0 takes hours and the rest take seconds.
func TestWaveBytesShowCopiedAgainstPlanned(t *testing.T) {
	got := bytesOfWave(v1.TransferWave{PlannedBytes: "1900000", TransferredBytes: "0"})
	if !strings.Contains(got, "/") || !strings.Contains(got, "MiB") {
		t.Errorf("wave bytes = %q, want copied/planned", got)
	}
	if got := bytesOfWave(v1.TransferWave{}); got != "-" {
		t.Errorf("a wave with no planned bytes = %q, want -", got)
	}
}

func TestNoWavesRendersNothing(t *testing.T) {
	var out bytes.Buffer
	if err := renderWaves(&out, nil); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("an empty wave list printed a heading:\n%s", out.String())
	}
}
