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
// transfer's own state, and the listing had no column for failures at all — so
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
	if err := renderTransferList(&buf, resp, rateTrackers{}); err != nil {
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
