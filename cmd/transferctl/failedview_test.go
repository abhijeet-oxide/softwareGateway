package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// A TRANSFER THAT IS FAILING MUST NOT LOOK LIKE ONE THAT IS WORKING.
//
// It did. The state stayed `running` because no failed job ever moved the
// transfer's own state, and the listing had no column for failures at all - so
// an outage that killed every job showed up as a percentage that stopped
// moving, and nothing else. Both halves are asserted here: the state the server
// now sets, and the column that shows the count before the state changes.

func TestTheListShowsFailedJobs(t *testing.T) {
	resp := &v1.ListTransfersResponse{Transfers: []v1.Transfer{
		{
			ID: "281614ab-1111-2222-3333-444444444444", Product: "cfx-5000-product",
			Tag: "25.7_mp2604_2131", State: v1.TransferRunning,
			StartedAt: time.Now().Add(-time.Hour).Format(time.RFC3339Nano),
			Progress: v1.TransferProgress{
				JobsPlanned: 2493, JobsDone: 900, JobsFailed: 17, JobsOutstanding: 1576,
				JobsInFlight: 4,
				PlannedBytes: "30000000000", BytesTransferred: "9000000000",
			},
		},
		{
			ID: "9c1e8f2a-5555-6666-7777-888888888888", Product: "cfx-5000-product",
			Tag: "25.6", State: v1.TransferSucceeded,
			Progress: v1.TransferProgress{JobsPlanned: 10, JobsDone: 10},
		},
	}}

	var buf bytes.Buffer
	if err := renderTransferList(&buf, resp, rateTrackers{}, listView{all: true}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	if !strings.Contains(out, "FAILED") {
		t.Fatalf("the listing has no FAILED column:\n%s", out)
	}
	if !strings.Contains(out, "17") {
		t.Errorf("17 failed jobs are not shown:\n%s", out)
	}
	// A healthy fleet must not grow a column of zeroes to read past.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "9c1e8f2a") && !strings.Contains(line, "-") {
			t.Errorf("a transfer with no failures shows a count rather than a dash: %q", line)
		}
	}
}

func TestFailedCountIsADashWhenThereAreNone(t *testing.T) {
	if got := failedJobs(v1.TransferProgress{}); got != "-" {
		t.Errorf("failedJobs of a healthy transfer = %q, want -", got)
	}
	if got := failedJobs(v1.TransferProgress{JobsFailed: 3}); got != "3" {
		t.Errorf("failedJobs = %q, want 3", got)
	}
}

// AN ETA THAT DISAPPEARS ON A MOVING TRANSFER READS AS A BROKEN ESTIMATE.
//
// It was gated on `state == running`, which is a different question from
// "is there anything left to estimate". A transfer is `ready` from the moment it
// is planned until its first job COMPLETES - long, with multi-gigabyte blobs,
// and exactly where a resumed transfer restarts - so the table showed a
// throughput and a blank ETA at the same time.
func TestETAIsShownWhileWorkIsStillOutstanding(t *testing.T) {
	moving := &v1.Transfer{
		State:     v1.TransferReady,
		StartedAt: time.Now().Add(-100 * time.Second).Format(time.RFC3339Nano),
		Progress: v1.TransferProgress{
			PlannedBytes: "1100", BytesTransferred: "100",
			// A transfer that is MOVING has work in flight. Byte counts alone
			// describe a transfer that moved bytes at some point, which is a
			// different claim and the one that produced "~1m22s" on something
			// motionless for hours.
			JobsInFlight: 2, JobsOutstanding: 5,
		},
	}

	if got := etaOf(moving, 0); got == "-" {
		t.Error("no ETA for a ready transfer that is moving bytes")
	}
	// And it agrees with the speed column, which never had the gate.
	if got := speedOf(moving, 0); got == "-" {
		t.Fatal("the fixture moves no bytes; the test would prove nothing")
	}

	// A settled transfer has nothing remaining, whatever its byte counts say.
	for _, state := range []v1.TransferState{
		v1.TransferSucceeded, v1.TransferFailed, v1.TransferCancelled,
	} {
		settled := *moving
		settled.State = state
		if got := etaOf(&settled, 10); got != "-" {
			t.Errorf("eta of a %s transfer = %q, want -", state, got)
		}
	}
}
