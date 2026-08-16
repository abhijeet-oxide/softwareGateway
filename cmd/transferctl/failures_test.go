package main

import (
	"bytes"
	"strings"
	"testing"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// The complaint this command answers was not "the table is missing a column".
// It was that a screen of five hundred rows saying the same thing is not a
// diagnosis. So the assertions are about SHAPE — one block per cause, the
// registry's own words intact, and a next step that differs by class.

func TestFailuresPrintsOneBlockPerCauseNotOnePerJob(t *testing.T) {
	out := renderTo(t, &v1.ListFailuresResponse{
		TransferID: "28161ab4-e88a-452e-87f1-8af51ae8fef8",
		Failures: []v1.FailureGroup{{
			Class:   "unsupported",
			Message: "push manifest <digest> <repository>: HTTP 400: manifest invalid",
			Failed:  512, Kinds: []string{"manifest"}, Waves: []int{1},
			ExampleDigest:     "sha256:1626c8c6f662a1b3",
			ExampleRepository: "apm0014228-oci-stage/orbs/cfx-5000/nokia-ims-mtcm",
		}},
	})

	if n := strings.Count(out, "manifest invalid"); n != 1 {
		t.Errorf("the cause is printed %d times for 512 jobs, want once:\n%s", n, out)
	}
	if !strings.Contains(out, "512 jobs failed") {
		t.Errorf("the output does not say how many jobs the cause is holding:\n%s", out)
	}
	if !strings.Contains(out, "HTTP 400") {
		t.Errorf("the output drops what the registry actually answered:\n%s", out)
	}
	// One example, so the reader has somewhere concrete to go.
	if !strings.Contains(out, "nokia-ims-mtcm") {
		t.Errorf("no example destination to go and inspect:\n%s", out)
	}
}

// "Retrying on its own" and "this will fail identically eight more times" need
// opposite responses, and the class is what tells them apart.
func TestFailuresSaysWhetherWaitingWouldHelp(t *testing.T) {
	cases := []struct {
		class     string
		retryable bool
		want      string
	}{
		{"auth", false, "credential"},
		{"unsupported", false, "Not retryable"},
		{"unavailable", true, "Retrying"},
		{"blob_unknown", true, "Self-healing"},
	}

	for _, tc := range cases {
		out := renderTo(t, &v1.ListFailuresResponse{
			TransferID: "28161ab4-0000-0000-0000-000000000000",
			Failures: []v1.FailureGroup{{
				Class: tc.class, Message: "something went wrong",
				Failed: 3, Retryable: tc.retryable,
			}},
		})
		if !strings.Contains(out, tc.want) {
			t.Errorf("class %q produced no %q guidance:\n%s", tc.class, tc.want, out)
		}
	}
}

// A transfer with nothing wrong must say so in one line rather than printing
// an empty table, which reads as a broken command.
func TestFailuresOfAHealthyTransferSaysNothingIsWrong(t *testing.T) {
	out := renderTo(t, &v1.ListFailuresResponse{TransferID: "28161ab4"})
	if !strings.Contains(out, "Nothing is failing") {
		t.Errorf("output = %q, want a plain statement that nothing is wrong", out)
	}
}

// Jobs still backing off are not jobs that have given up, and an operator
// reading a total cannot tell which is which.
func TestFailuresCountsRetryingApartFromFailed(t *testing.T) {
	out := renderTo(t, &v1.ListFailuresResponse{
		TransferID: "28161ab4",
		Failures: []v1.FailureGroup{{
			Class: "timeout", Message: "gateway timeout",
			Failed: 18, Retrying: 22, Retryable: true,
		}},
	})
	if !strings.Contains(out, "18 jobs failed, 22 retrying") {
		t.Errorf("output does not separate what has given up from what is waiting:\n%s", out)
	}
}

func renderTo(t *testing.T, resp *v1.ListFailuresResponse) string {
	t.Helper()

	var buf bytes.Buffer
	if err := renderFailures(&buf, resp); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// The example names the tag, because the shared sentence no longer can.
//
// Grouping normalises the tag away — that is what makes one refusal across a
// release's three tags one cause rather than three — and with it went the exact
// string somebody needs to reproduce the failure against the registry by hand.
// The example line is about a single job and carries it back.
func TestTheExampleNamesTheTagThatWasRefused(t *testing.T) {
	out := renderTo(t, &v1.ListFailuresResponse{
		TransferID: "4ce320df-1111-2222-3333-444444444444",
		Failures: []v1.FailureGroup{{
			Class: "auth", Failed: 1, Kinds: []string{"manifest"}, Waves: []int{2},
			Message: "tag <digest> as <tag> artifact.it.att.com/<repository>: HTTP 401: " +
				"unauthorized: authentication required (the tag does not exist at the " +
				"destination, so this write would have created it)",
			ExampleDigest:     "sha256:04e1aeb8b0dbf1c2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f80912",
			ExampleRepository: "apm0014228-oci-stage/orbs/cfx-5000-k8s-215952-ncp",
			ExampleTags:       []string{"orb_25.7_mp2604_2131"},
		}},
	})

	if !strings.Contains(out, "orb_25.7_mp2604_2131") {
		t.Errorf("the example does not name the refused tag:\n%s", out)
	}
	// A coordinate, not two facts side by side: repository, then tag.
	if !strings.Contains(out, "apm0014228-oci-stage/orbs/cfx-5000-k8s-215952-ncp:orb_25.7_mp2604_2131") {
		t.Errorf("the example is not a reference somebody can paste:\n%s", out)
	}
}

// A transfer that failed before it had any jobs.
//
// The summary is built from job errors, and a transfer that fails while being
// PLANNED has none — an origin that cannot be reached, a package whose tree
// will not walk. It reported "Nothing is failing in this transfer" about a
// transfer whose state was `failed`, which is the one answer that cannot be
// right.
func TestATransferThatFailedBeforePlanningSaysWhy(t *testing.T) {
	out := renderTo(t, &v1.ListFailuresResponse{
		TransferID:    "b3174872-1111-2222-3333-444444444444",
		State:         v1.TransferFailed,
		FailureReason: "walk orbs/cfx-5000-k8s-…-template-v0.04: fetch manifest sha256:aa…: HTTP 404: not found",
	})

	if strings.Contains(out, "Nothing is failing") {
		t.Fatalf("a failed transfer was reported as healthy:\n%s", out)
	}
	if !strings.Contains(out, "HTTP 404") {
		t.Errorf("the recorded reason is not reported:\n%s", out)
	}
	if !strings.Contains(out, "b3174872") {
		t.Errorf("the block does not name the transfer:\n%s", out)
	}
	// And it says what to do, which is not `retry`: planning queued nothing, so
	// there is no job to requeue.
	if !strings.Contains(out, "nothing to retry") {
		t.Errorf("the reader is not told that retry does not apply:\n%s", out)
	}
}

// A transfer that is simply fine still says so.
func TestAHealthyTransferStillReportsNothingFailing(t *testing.T) {
	out := renderTo(t, &v1.ListFailuresResponse{
		TransferID: "b5d85331-1111-2222-3333-444444444444",
		State:      v1.TransferSucceeded,
	})
	if !strings.Contains(out, "Nothing is failing") {
		t.Errorf("a healthy transfer was reported as failing:\n%s", out)
	}
}

// Failed, with nothing recorded to say why. Saying that beats claiming health.
func TestAFailedTransferWithNoReasonSaysThat(t *testing.T) {
	out := renderTo(t, &v1.ListFailuresResponse{
		TransferID: "b3174872-1111-2222-3333-444444444444",
		State:      v1.TransferFailed,
	})
	if strings.Contains(out, "Nothing is failing") {
		t.Fatalf("a failed transfer was reported as healthy:\n%s", out)
	}
	if !strings.Contains(out, "no reason was recorded") {
		t.Errorf("the gap is not stated plainly:\n%s", out)
	}
}

// What a delete says it did, and — in the same breath — what it did not.
//
// A reader who has just deleted something needs to know whether they have
// changed the destination. Finding that out afterwards is too late.
func TestDeleteSaysNothingAtTheDestinationWasRemoved(t *testing.T) {
	var buf bytes.Buffer
	if err := renderControl(&buf, "delete", &v1.TransferControlResponse{
		TransferID: "b3174872-1111-2222-3333-444444444444",
		State:      "FAILED",
		Jobs:       0,
	}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	if !strings.Contains(out, "Deleted b3174872") {
		t.Errorf("the delete does not say what it removed:\n%s", out)
	}
	if !strings.Contains(out, "Nothing at the destination was removed") {
		t.Errorf("the delete does not say what it left alone:\n%s", out)
	}
}
