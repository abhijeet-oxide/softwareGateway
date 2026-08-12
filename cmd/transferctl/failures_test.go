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
