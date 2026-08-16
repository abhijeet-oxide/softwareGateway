package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// Three things a transfer could not say about itself, all reported from one
// re-transfer of an orb: what it was made of, how fast it went once it had
// finished, and how much longer it had to run.

// The release, in the terms it is published in.
func TestDescribeSaysWhatTheTransferIsMadeOf(t *testing.T) {
	transfer := &v1.Transfer{
		ID: "9bc63dc2", Product: "cfx-5000-product", Tag: "orb_25.7",
		State: v1.TransferRunning,
		Content: []v1.ContentGroup{
			{Kind: "index", Total: 2, Copied: 1, Outstanding: 1},
			{Kind: "image", Total: 231, Copied: 6, Present: 225},
			{Kind: "chart", Total: 21, Present: 21},
			{Kind: "file", Total: 6, Present: 5, Failed: 1},
		},
	}

	var buf bytes.Buffer
	if err := describeTransfer(&buf, transfer, &rateTracker{}, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	if !strings.Contains(out, "Content") {
		t.Fatalf("describe has no content breakdown:\n%s", out)
	}
	for _, want := range []string{"images", "charts", "files", "231", "225"} {
		if !strings.Contains(out, want) {
			t.Errorf("the breakdown does not report %q:\n%s", want, out)
		}
	}
	// The total, so a reader is not left adding four rows in their head.
	if !strings.Contains(out, "all") || !strings.Contains(out, "260") {
		t.Errorf("the breakdown does not total its rows:\n%s", out)
	}
	// The plural of index is not `indexs`, and a single chart is not `charts`.
	// Both are the sort of detail a reader notices before any of the numbers,
	// and after which they trust none of them.
	if strings.Contains(out, "indexs") {
		t.Errorf("the plural of index came out wrong:\n%s", out)
	}
	if !strings.Contains(out, "indexes") {
		t.Errorf("two indexes were not pluralised:\n%s", out)
	}
	if got := kindLabel("chart", 1); got != "chart" {
		t.Errorf("one chart is labelled %q", got)
	}
}

// A finished transfer has a speed: the average over the whole run, which is
// what somebody compares against the next run and the other target.
func TestAFinishedTransferReportsItsAverageSpeed(t *testing.T) {
	done := &v1.Transfer{
		ID: "9bc63dc2", State: v1.TransferSucceeded,
		StartedAt:   time.Now().Add(-100 * time.Second).Format(time.RFC3339Nano),
		CompletedAt: time.Now().Format(time.RFC3339Nano),
		Progress:    v1.TransferProgress{BytesTransferred: "524288000"},
	}

	// 500 MiB over 100 seconds.
	if got := speedOf(done, 0); got == "-" {
		t.Fatal("a finished transfer reported no speed at all")
	} else if !strings.HasPrefix(got, "5.0 MiB") {
		t.Errorf("speed = %q, want the average of 5.0 MiB/s", got)
	}

	// A transfer that moved nothing still has no rate. `0 B/s` on a delta that
	// did exactly what it should reads as a link that was not working.
	moved := &v1.Transfer{
		ID: "218985ce", State: v1.TransferSucceeded,
		StartedAt:   done.StartedAt,
		CompletedAt: done.CompletedAt,
		Progress:    v1.TransferProgress{BytesTransferred: "0"},
	}
	if got := speedOf(moved, 0); got != "-" {
		t.Errorf("a transfer that moved no bytes reported %q, want a dash", got)
	}

	// A RUNNING transfer with nothing in flight still reports nothing: a rate
	// measured before it stalled describes a period that has ended.
	stalled := &v1.Transfer{
		ID: "5a1c0000", State: v1.TransferRunning,
		StartedAt: done.StartedAt,
		Progress: v1.TransferProgress{
			BytesTransferred: "524288000", JobsOutstanding: 40,
		},
	}
	if got := speedOf(stalled, 0); got != "-" {
		t.Errorf("a stalled transfer reported %q, want a dash", got)
	}
}

// The ETA of a re-transfer, which is almost entirely skips.
//
// The reported symptom: hours remaining on a transfer that finished in under a
// minute. Outstanding bytes is the size of the work left, and on a re-transfer
// nearly none of that work is a transfer — so dividing it by a rate measured
// while moving the little that did move is arithmetic on the wrong quantity.
func TestTheEstimateAllowsForWorkThatWillBeSkipped(t *testing.T) {
	// 64 GiB planned, 60 GiB of it still outstanding — and of the 4 GiB looked
	// at so far, only 40 MiB actually moved. The rest was already at the target.
	delta := &v1.Transfer{
		State: v1.TransferRunning,
		Progress: v1.TransferProgress{
			PlannedBytes:     "68719476736",
			OutstandingBytes: "64424509440",
			BytesTransferred: "41943040",
			JobsInFlight:     4,
		},
	}

	const rate = 20 << 20 // 20 MiB/s

	got, ok := estimateAt(delta, rate)
	if !ok {
		t.Fatal("no estimate for a transfer that is plainly progressing")
	}
	// Unscaled, this is 60 GiB at 20 MiB/s: nearly an hour. Scaled by the
	// proportion that has actually needed moving, it is well under a minute.
	if got > time.Minute {
		t.Errorf("estimate = %s, want under a minute: the outstanding bytes were "+
			"extrapolated as though they all had to move", got)
	}

	// A transfer where everything genuinely has to move is unchanged: the
	// scaling must not shorten an honest estimate.
	fresh := &v1.Transfer{
		State: v1.TransferRunning,
		Progress: v1.TransferProgress{
			PlannedBytes:     "68719476736",
			OutstandingBytes: "64424509440",
			BytesTransferred: "4294967296",
			JobsInFlight:     4,
		},
	}
	got, ok = estimateAt(fresh, rate)
	if !ok {
		t.Fatal("no estimate for a transfer that is moving every byte")
	}
	if got < 50*time.Minute {
		t.Errorf("estimate = %s, want about an hour: 60 GiB at 20 MiB/s", got)
	}
}

// Before anything has completed there is no ratio to apply, and the honest
// answer is the unscaled figure rather than a guess in either direction.
func TestTheEstimateIsUnscaledUntilSomethingHasCompleted(t *testing.T) {
	starting := &v1.Transfer{
		State: v1.TransferRunning,
		Progress: v1.TransferProgress{
			PlannedBytes:     "1048576000",
			OutstandingBytes: "1048576000",
			BytesTransferred: "1048576",
			JobsInFlight:     4,
		},
	}

	if got := movableBytes(starting); got != 1048576000 {
		t.Errorf("movable = %d, want the outstanding bytes unscaled", got)
	}
}
