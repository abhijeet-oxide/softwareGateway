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

// The row that could not be true.
//
//	COPIED 490.1 KiB/29.8 GiB · SAVED 63.7 GiB
//
// A transfer that had saved more than the whole of what it was moving. Both
// figures were right and they were measured against different things: COPIED
// counted against the work the plan QUEUED, and content already at the
// destination when the transfer was planned is never queued — it is in SAVED.
func TestTheByteColumnsAreMeasuredAgainstTheSameThing(t *testing.T) {
	// 29.8 GiB queued, 63.7 GiB never queued: a 93.5 GiB release.
	p := v1.TransferProgress{
		PlannedBytes:       "32000000000",
		DedupeSkippedBytes: "68400000000",
		SkippedBytes:       "0",
		SavedBytes:         "68400000000",
		BytesTransferred:   "501862",
	}

	total := packageBytes(p)
	if saved := int64Of(p.SavedBytes); saved > total {
		t.Errorf("saved %d of a %d-byte release: the columns still disagree", saved, total)
	}

	// COPIED states what crossed the wire and nothing else: how much of a
	// release HAS to cross is settled one job at a time as the transfer runs,
	// so a fraction there would state a fact nobody has yet.
	if got := copiedBytes(p); strings.Contains(got, "/") {
		t.Errorf("COPIED = %q, want what crossed the wire without a denominator", got)
	}
	if got := plannedBytes(p); !strings.Contains(got, "93.5 GiB") {
		t.Errorf("PLANNED = %q, want the 93.5 GiB of work", got)
	}
}

// A transfer that is nearly all skips is not nearly finished.
//
// Its remaining jobs each cost a round trip whatever they weigh, so bytes do
// not govern its duration. Estimated on bytes alone it reported `<1s` with 794
// jobs still to run — the same defect as the hours-long estimate before it, in
// the opposite direction.
func TestTheEstimateRespectsTheJobsStillToRun(t *testing.T) {
	skipping := &v1.Transfer{
		State:     v1.TransferRunning,
		StartedAt: time.Now().Add(-83 * time.Second).Format(time.RFC3339Nano),
		Progress: v1.TransferProgress{
			PlannedBytes:     "32000000000",
			OutstandingBytes: "300000000",
			BytesTransferred: "501862",
			JobsPlanned:      1364,
			JobsDone:         570,
			JobsOutstanding:  794,
			JobsInFlight:     16,
		},
	}

	got, ok := estimateAt(skipping, 11.6*1024)
	if !ok {
		t.Fatal("no estimate for a transfer that is plainly working")
	}
	// 794 jobs left at 570 in 83 seconds is a shade under two minutes. The
	// bytes say less than a second, and the bytes are not what is left.
	if got < time.Minute {
		t.Errorf("estimate = %s, want about two minutes: %d jobs remain",
			got, skipping.Progress.JobsOutstanding)
	}

	// The byte estimate still wins where the bytes are the work: the two are
	// lower bounds and the transfer is not done until both are met.
	heavy := &v1.Transfer{
		State:     v1.TransferRunning,
		StartedAt: time.Now().Add(-83 * time.Second).Format(time.RFC3339Nano),
		Progress: v1.TransferProgress{
			PlannedBytes:     "32000000000",
			OutstandingBytes: "30000000000",
			BytesTransferred: "2000000000",
			JobsPlanned:      1364,
			JobsDone:         570,
			JobsOutstanding:  794,
			JobsInFlight:     16,
		},
	}
	got, ok = estimateAt(heavy, 11.6*1024)
	if !ok {
		t.Fatal("no estimate for a transfer moving real bytes")
	}
	if got < time.Hour {
		t.Errorf("estimate = %s, want hours: 28 GiB remain at 11.6 KiB/s", got)
	}
}

// A watch redraws the page every few seconds. A hint about a flag the reader
// has already read, and cannot act on without stopping the watch, is not worth
// a line of the screen each time.
func TestAWatchDoesNotRepeatTheHintAboutAll(t *testing.T) {
	resp := listWith(
		transferFixture{id: "4ce3200f-1111-2222-3333-444444444444", state: "RUNNING"},
		transferFixture{id: "9bc63dc2-1111-2222-3333-444444444444", state: "SUCCEEDED"},
	)

	var watching bytes.Buffer
	if err := renderTransferList(&watching, resp, rateTrackers{},
		listView{watching: true}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(watching.String(), "--all") {
		t.Errorf("a watch repeated the hint about --all:\n%s", watching.String())
	}
	// The transfer is still hidden — only the advice about it goes.
	if strings.Contains(watching.String(), "9bc63dc2") {
		t.Errorf("a watch stopped hiding finished transfers:\n%s", watching.String())
	}

	var once bytes.Buffer
	if err := renderTransferList(&once, resp, rateTrackers{}, listView{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(once.String(), "--all") {
		t.Errorf("a single listing lost the hint:\n%s", once.String())
	}
}

// A transfer with nothing outstanding has nothing remaining.
//
// The outstanding-bytes fallback exists for a Coordinator too old to send the
// figure, and it was reached by testing the value rather than its presence — so
// a transfer that had genuinely finished its bytes fell through to `planned
// minus transferred` and reported thousands of hours left with nothing to do.
func TestNothingOutstandingIsNothingRemaining(t *testing.T) {
	done := &v1.Transfer{
		State:     v1.TransferRunning,
		StartedAt: time.Now().Add(-229 * time.Second).Format(time.RFC3339Nano),
		Progress: v1.TransferProgress{
			PlannedBytes:     "68400000000",
			OutstandingBytes: "0",
			BytesTransferred: "1153434",
			JobsPlanned:      1364, JobsDone: 1364, JobsOutstanding: 0,
		},
	}

	if got := remainingBytes(done); got != 0 {
		t.Errorf("remaining = %d bytes with nothing outstanding", got)
	}
	if d, ok := estimate(done); ok {
		t.Errorf("estimated %s for a transfer with nothing left to do", d)
	}

	// A Coordinator that sends no figure at all still gets the old subtraction.
	old := &v1.Transfer{
		State:     v1.TransferRunning,
		StartedAt: done.StartedAt,
		Progress: v1.TransferProgress{
			PlannedBytes:     "1000",
			BytesTransferred: "400",
			JobsOutstanding:  4,
		},
	}
	if got := remainingBytes(old); got != 600 {
		t.Errorf("remaining = %d, want the 600 the fallback computes", got)
	}
}

// The release and the planned work are different quantities, and both are
// shown so nobody has to guess which one a number is.
//
// A component is published inside its bundle AND under its own name, and a
// registry stores blobs per repository — so a 29.8 GiB orb is 63.7 GiB of
// placements. Reporting only the second made the tool look unable to count
// against a listing that said 29.8 GiB.
func TestDescribeSeparatesTheReleaseFromTheWork(t *testing.T) {
	tr := &v1.Transfer{
		ID: "b5d85331", State: v1.TransferRunning,
		Progress: v1.TransferProgress{
			ContentBytes: "32000000000",
			PlannedBytes: "68400000000",
			SavedBytes:   "68398000000", BytesTransferred: "1153434",
		},
	}

	var buf bytes.Buffer
	if err := describeTransfer(&buf, tr, &rateTracker{}, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	if !strings.Contains(out, "Release:") || !strings.Contains(out, "29.8 GiB") {
		t.Errorf("describe does not report the size of the release:\n%s", out)
	}
	if !strings.Contains(out, "Planned:") || !strings.Contains(out, "63.7 GiB") {
		t.Errorf("describe does not report the planned work:\n%s", out)
	}
	// And says why they differ, rather than leaving the reader to decide one of
	// them is wrong.
	if !strings.Contains(out, "per repository") {
		t.Errorf("describe does not explain why the two differ:\n%s", out)
	}
}
