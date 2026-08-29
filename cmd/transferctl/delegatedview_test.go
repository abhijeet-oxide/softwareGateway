package main

import (
	"bytes"
	"strings"
	"testing"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

func delegatedTransfer(state v1.TransferState) *v1.Transfer {
	return &v1.Transfer{
		ID: "t-1", Product: "vendor-a", Tag: "v3.2.1",
		SourceName: "jfrog-store", TargetName: "ocp-prod",
		Source: "artifactory.example.com/docker-local", Target: "quay.apps.ocp.example.com/platform-prod",
		State: state, Strategy: "mirror",
	}
}

func describe(t *testing.T, tr *v1.Transfer) string {
	t.Helper()
	var buf bytes.Buffer
	// A real tracker: the copy path reads it, and the delegated path must not.
	if err := describeTransfer(&buf, tr, &rateTracker{}, false); err != nil {
		t.Fatalf("describe: %v", err)
	}
	return buf.String()
}

// The rule from docs/design/18 §6.1, at the place a reader would notice it
// breaking: no percentage, no speed, no ETA for bytes we did not move.
func TestDelegatedDescribeHasNoInventedNumbers(t *testing.T) {
	out := describe(t, delegatedTransfer(v1.TransferSyncing))

	for _, forbidden := range []string{"%", "MB/s", "GiB", "ETA"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("found %q in a delegated transfer's detail:\n%s", forbidden, out)
		}
	}
	if !strings.Contains(out, "no percentage to report") {
		t.Fatalf("the absence has to be stated, not left blank:\n%s", out)
	}
	if !strings.Contains(out, "cannot count them") {
		t.Fatalf("the byte line must say why it is empty:\n%s", out)
	}
}

// `diverged` is the outcome the whole design exists to distinguish, so the
// wording must not read as either success or failure.
func TestDivergedReadsAsNeitherSuccessNorFailure(t *testing.T) {
	out := describe(t, delegatedTransfer(v1.TransferDiverged))

	if !strings.Contains(out, "DIFFERENT digest") || !strings.Contains(out, "upstream tag moved") {
		t.Fatalf("diverged must explain itself:\n%s", out)
	}
	if strings.Contains(out, "failed") {
		t.Fatalf("diverged is not a failure:\n%s", out)
	}
}

// The next question after reading this page is always "what is the registry
// configured to do", so the page names the command that answers it.
func TestDelegatedDescribePointsAtTheTarget(t *testing.T) {
	out := describe(t, delegatedTransfer(v1.TransferSucceeded))
	if !strings.Contains(out, "targets describe vendor-a ocp-prod") {
		t.Fatalf("expected the follow-up command:\n%s", out)
	}
}

// A copy transfer must render exactly as it always did.
func TestCopyTransferIsUnchanged(t *testing.T) {
	tr := delegatedTransfer(v1.TransferRunning)
	tr.Strategy = "copy"
	tr.Progress = &v1.TransferProgress{JobsPlanned: 10, JobsDone: 4}

	out := describe(t, tr)
	if !strings.Contains(out, "Progress") || !strings.Contains(out, "40%") {
		t.Fatalf("a copy transfer keeps its measurements:\n%s", out)
	}
}

// An unset strategy is a transfer from before this field existed, and must be
// treated as a copy rather than as delegated.
func TestUnsetStrategyIsACopy(t *testing.T) {
	tr := delegatedTransfer(v1.TransferRunning)
	tr.Strategy = ""
	if delegated(tr) {
		t.Fatal("an unset strategy predates the field and means copy")
	}
}

// Every measured column of a list row, blanked together.
func TestDelegatedListRowCarriesNoNumbers(t *testing.T) {
	var buf bytes.Buffer
	err := renderTransferList(&buf, &v1.ListTransfersResponse{
		Transfers: []v1.Transfer{*delegatedTransfer(v1.TransferSyncing)},
	}, rateTrackers{}, listView{all: true})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "0%") || strings.Contains(out, " 0 ") {
		t.Fatalf("a delegated row must carry no zeroes that read as measurements:\n%s", out)
	}
	if strings.Count(out, "-") < 8 {
		t.Fatalf("expected every measured column to be dashed:\n%s", out)
	}
	if !strings.Contains(out, "syncing") {
		t.Fatalf("the state is what it DOES have:\n%s", out)
	}
}
