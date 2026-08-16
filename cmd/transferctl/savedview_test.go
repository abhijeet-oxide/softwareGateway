package main

import (
	"bytes"
	"strings"
	"testing"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// transferFixture is one row's worth of server response, named rather than
// positional so a test states only the fields it is about.
type transferFixture struct {
	id, state              string
	tag, displayTag        string
	sourceName, targetName string
	source, target         string
	planned, transferred   string
	dedupe, skipped, saved string
}

func listWith(fixtures ...transferFixture) *v1.ListTransfersResponse {
	out := &v1.ListTransfersResponse{}
	for _, f := range fixtures {
		tag := f.tag
		if tag == "" {
			tag = "25.7"
		}
		out.Transfers = append(out.Transfers, v1.Transfer{
			ID: f.id, Product: "cfx-5000-product",
			Tag: tag, DisplayTag: f.displayTag,
			Source: f.source, Target: f.target,
			SourceName: f.sourceName, TargetName: f.targetName,
			State: v1.TransferState(f.state),
			Progress: v1.TransferProgress{
				JobsPlanned:        2489,
				JobsDone:           1086,
				PlannedBytes:       v1.Int64String(f.planned),
				BytesTransferred:   v1.Int64String(f.transferred),
				DedupeSkippedBytes: v1.Int64String(f.dedupe),
				SkippedBytes:       v1.Int64String(f.skipped),
				SavedBytes:         v1.Int64String(f.saved),
			},
		})
	}
	return out
}

// What a transfer SAVED, and where it went.
//
// The reported case: 63.7 GiB planned, 32.1 GiB of it already at the target, and
// a listing reading `COPIED 0 B/63.7 GiB · SAVED 0 B`. Both numbers were
// arithmetically defensible and together they said the transfer had done
// nothing and saved nothing, which was false in the second half.
//
// SAVED read `dedupeSkippedBytes` alone — the part known at PLANNING time, from
// placement records. On a database that has just been rebuilt there are no
// placement records, so that number is zero by construction, and every byte the
// transfer saved was saved by a worker discovering the content already present.
// The column was measuring the one kind of saving that could not happen.

func TestTheListShowsWhatTheTransferSaved(t *testing.T) {
	resp := listWith(transferFixture{
		id: "9bc63dc2-1111-2222-3333-444444444444", state: "RUNNING",
		planned: "68440000000", transferred: "0",
		dedupe: "0", skipped: "34400000000", saved: "34400000000",
	})

	var buf bytes.Buffer
	if err := renderTransferList(&buf, resp, rateTrackers{}, true); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	if strings.Contains(row(out, "9bc63dc2"), "0 B\t") && !strings.Contains(out, "GiB") {
		t.Fatalf("nothing in the row reports a saving:\n%s", out)
	}
	if !strings.Contains(row(out, "9bc63dc2"), "32.0 GiB") {
		t.Errorf("the 32 GiB found already at the target is not in the row:\n%s", out)
	}
}

// Nothing saved is a dash, not a zero. A first transfer of new content saves
// nothing, and a column of `0 B` down the page is noise to read past.
func TestNothingSavedIsADash(t *testing.T) {
	resp := listWith(transferFixture{
		id: "aaaaaaaa-1111-2222-3333-444444444444", state: "SUCCEEDED",
		planned: "1024", transferred: "1024", saved: "0",
	})

	var buf bytes.Buffer
	if err := renderTransferList(&buf, resp, rateTrackers{}, true); err != nil {
		t.Fatal(err)
	}
	if got := savedBytes(resp.Transfers[0].Progress); got != "-" {
		t.Errorf("saved = %q with nothing saved, want a dash", got)
	}
}

// Where a transfer reads from and writes to, in the listing.
//
// There was no way to tell two transfers of the same package apart: same
// product, same tag, different destinations, identical rows.
func TestTheListSaysWhereEachTransferGoes(t *testing.T) {
	resp := listWith(transferFixture{
		id: "9bc63dc2-1111-2222-3333-444444444444", state: "RUNNING",
		sourceName: "near", targetName: "att-stage",
		source:  "cfx-5000-product-orb-docker.swdp-us.support.nokia.com/orbs/cfx-5000-k8s",
		target:  "artifact.it.att.com/apm0014228-oci-stage",
		planned: "1024", transferred: "0", saved: "0",
	})

	var buf bytes.Buffer
	if err := renderTransferList(&buf, resp, rateTrackers{}, true); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	for _, want := range []string{"FROM", "TO", "near", "att-stage"} {
		if !strings.Contains(out, want) {
			t.Errorf("the listing does not carry %q:\n%s", want, out)
		}
	}
	// The configured name, not the resolved path: the row has to stay narrow
	// enough to scan, and the name is also what goes back into --from/--to.
	if strings.Contains(out, "swdp-us.support.nokia.com") {
		t.Errorf("the listing spends a hundred columns on a resolved host:\n%s", out)
	}
}

// describe names the package the way the listing does, and totals the saving.
//
// The two pages disagreed: `list` showed `25.7_mp2604_2131` and `describe`
// showed `orb_25.7_mp2604_2131` for the same transfer, leaving the reader to
// work out that they were the same package.
func TestDescribeAgreesWithTheListingAboutTheName(t *testing.T) {
	transfer := listWith(transferFixture{
		id: "9bc63dc2-1111-2222-3333-444444444444", state: "RUNNING",
		tag: "orb_25.7_mp2604_2131", displayTag: "25.7_mp2604_2131",
		sourceName: "near", targetName: "att-stage",
		planned: "68440000000", transferred: "0",
		skipped: "34400000000", saved: "34400000000",
	}).Transfers[0]

	var buf bytes.Buffer
	if err := describeTransfer(&buf, &transfer, &rateTracker{}, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	if strings.Contains(out, "orb_25.7") {
		t.Errorf("describe prints the raw tag the listing shortens:\n%s", out)
	}
	if !strings.Contains(out, "25.7_mp2604_2131") {
		t.Errorf("describe does not name the package at all:\n%s", out)
	}
	// The heading carries the total, so "what did this save" is answered
	// without adding up the breakdown underneath it.
	if !strings.Contains(out, "Not transferred:") || !strings.Contains(out, "32.0 GiB") {
		t.Errorf("describe does not total what was not transferred:\n%s", out)
	}
}

// A reason that did not apply is left out rather than printed as zero.
func TestDescribeOmitsASavingThatDidNotHappen(t *testing.T) {
	transfer := listWith(transferFixture{
		id: "aaaaaaaa-1111-2222-3333-444444444444", state: "RUNNING",
		planned: "1024", transferred: "1024", saved: "0",
	}).Transfers[0]

	var buf bytes.Buffer
	if err := describeTransfer(&buf, &transfer, &rateTracker{}, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "Deduplicated") {
		t.Errorf("describe prints a zero Deduplicated row:\n%s", buf.String())
	}
}

// row returns the listing line containing a marker, for assertions that are
// about one transfer rather than the page.
func row(out, marker string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, marker) {
			return line
		}
	}
	return ""
}
