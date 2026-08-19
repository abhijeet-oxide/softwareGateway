package main

import (
	"bytes"
	"strings"
	"testing"
)

// What the listing shows by default, and how a row that will not fit is
// shortened.
//
// Both come from the same reported view: a terminal holding two transfers, one
// of them finished weeks ago, and rows so wide that each wrapped onto a second
// line - so the columns stopped lining up and the one transfer still moving was
// the harder of the two to find.

// A finished transfer is counted, not listed. Nothing is lost: the count says
// how much is being held back, and --all brings it out.
func TestFinishedTransfersAreHeldBackUntilAskedFor(t *testing.T) {
	resp := listWith(
		transferFixture{id: "218985ce-1111-2222-3333-444444444444", state: "PLANNING"},
		transferFixture{id: "9bc63dc2-1111-2222-3333-444444444444", state: "SUCCEEDED"},
		transferFixture{id: "5a1c0000-1111-2222-3333-444444444444", state: "CANCELLED"},
	)

	var buf bytes.Buffer
	if err := renderTransferList(&buf, resp, rateTrackers{}, listView{}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	if !strings.Contains(out, "218985ce") {
		t.Errorf("the transfer still planning is not listed:\n%s", out)
	}
	for _, hidden := range []string{"9bc63dc2", "5a1c0000"} {
		if strings.Contains(out, hidden) {
			t.Errorf("the finished transfer %s is listed by default:\n%s", hidden, out)
		}
	}
	// Held back, not dropped - and the reader is told how to see them.
	if !strings.Contains(out, "2 finished transfers not shown") {
		t.Errorf("the listing does not account for what it held back:\n%s", out)
	}
	if !strings.Contains(out, "--all") {
		t.Errorf("the listing does not say how to see them:\n%s", out)
	}

	var everything bytes.Buffer
	if err := renderTransferList(&everything, resp, rateTrackers{}, listView{all: true}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"218985ce", "9bc63dc2", "5a1c0000"} {
		if !strings.Contains(everything.String(), id) {
			t.Errorf("--all does not list %s:\n%s", id, everything.String())
		}
	}
	if strings.Contains(everything.String(), "not shown") {
		t.Errorf("--all still reports something held back:\n%s", everything.String())
	}
}

// A FAILED transfer has finished too, and is never hidden: it is the one thing
// this listing exists to put in front of somebody.
func TestAFailedTransferIsNeverHidden(t *testing.T) {
	resp := listWith(transferFixture{
		id: "9bc63dc2-1111-2222-3333-444444444444", state: "FAILED",
	})

	var buf bytes.Buffer
	if err := renderTransferList(&buf, resp, rateTrackers{}, listView{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "9bc63dc2") {
		t.Errorf("a failed transfer was hidden from the default listing:\n%s", buf.String())
	}
}

// Nothing in flight is an answer. A bare column header reads as a command that
// produced no output.
func TestAListingWithNothingInFlightSaysSo(t *testing.T) {
	resp := listWith(transferFixture{
		id: "9bc63dc2-1111-2222-3333-444444444444", state: "SUCCEEDED",
	})

	var buf bytes.Buffer
	if err := renderTransferList(&buf, resp, rateTrackers{}, listView{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "Nothing in flight.") {
		t.Errorf("a listing with nothing to show said nothing:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "1 finished transfer not shown") {
		t.Errorf("the listing does not account for what it held back:\n%s", buf.String())
	}
}

// The middle goes, not the end.
//
// These strings share their fronts and differ at their backs, so an ellipsis at
// the end would render the two rows identically - which is worse than a wrapped
// line, because a wrapped line can still be read.
func TestAShortenedReferenceKeepsBothOfItsEnds(t *testing.T) {
	const (
		a = "cfx-near/orbs/cfx-5000-k8s-215952-edgenac-25.7-2131_20260807-wnv5a0cscf0003c-ncm"
		b = "cfx-near/orbs/cfx-5000-k8s-215952-ncp-25.7.2131_20260806-wnv7a0ecsf0001c"
	)

	shortA, shortB := middleTruncate(a, 30), middleTruncate(b, 30)

	for _, got := range []string{shortA, shortB} {
		if n := len([]rune(got)); n > 30 {
			t.Errorf("%q is %d runes, want at most 30", got, n)
		}
		if !strings.HasPrefix(got, "cfx-near/") {
			t.Errorf("%q lost the front, which says where it is", got)
		}
		if !strings.Contains(got, "…") {
			t.Errorf("%q does not mark what was removed", got)
		}
	}
	if !strings.HasSuffix(shortA, "-ncm") {
		t.Errorf("%q lost the tail, which is what distinguishes it", shortA)
	}
	if shortA == shortB {
		t.Errorf("two different references both shortened to %q", shortA)
	}
	// Short enough already is left exactly alone.
	if got := middleTruncate("cfx-near", 30); got != "cfx-near" {
		t.Errorf("a value inside the budget was altered to %q", got)
	}
}

// The row is fitted by taking from the widest column that can give, and never
// from one that cannot.
func TestARowIsFittedByShorteningWhatCanBeShortened(t *testing.T) {
	rows := [][]string{
		{"ID", "FROM", "TO", "DONE"},
		{
			"9bc63dc2",
			"cfx-near/orbs/cfx-5000-k8s-215952-edgenac-25.7-2131_20260807-wnv5a0cscf0003c-ncm",
			"artifact.it.att.com/apm0014228-oci-stage/orbs/cfx-5000-k8s-215952-edgenac",
			"100%",
		},
	}

	fitCells(rows, []int{1, 2}, 80)

	if got := rowWidth([]int{
		len([]rune(rows[1][0])), len([]rune(rows[1][1])),
		len([]rune(rows[1][2])), len([]rune(rows[1][3])),
	}); got > 80 {
		t.Errorf("the fitted row is %d wide, want at most 80: %v", got, rows[1])
	}
	// The columns that were not offered stay whole.
	if rows[1][0] != "9bc63dc2" || rows[1][3] != "100%" {
		t.Errorf("a column that cannot be shortened was shortened: %v", rows[1])
	}
	// Both elastic columns were long, so both gave rather than one being
	// destroyed while the other kept its full width.
	for _, i := range []int{1, 2} {
		if n := len([]rune(rows[1][i])); n < minCell {
			t.Errorf("column %d was cut to %d runes, below the floor of %d", i, n, minCell)
		}
	}
}

// An unknown width - a pipe, a file, a test - shortens nothing. A redirected
// listing is being read by something whose width nothing here knows.
func TestAnUnknownWidthLeavesEveryRowWhole(t *testing.T) {
	const long = "cfx-near/orbs/cfx-5000-k8s-215952-edgenac-25.7-2131_20260807-wnv5a0cscf0003c-ncm"

	rows := [][]string{{"FROM"}, {long}}
	fitCells(rows, []int{0}, 0)

	if rows[1][0] != long {
		t.Errorf("a value was shortened with no width to fit: %q", rows[1][0])
	}
	if got := terminalWidth(&bytes.Buffer{}); got != 0 {
		t.Errorf("terminalWidth of a buffer = %d, want 0", got)
	}
}

// The listing renders into a buffer in every test above, which is exactly the
// case that must not shorten anything - so this asserts the whole path, not
// just the helper.
func TestTheListingItselfLeavesARedirectedRowWhole(t *testing.T) {
	resp := listWith(transferFixture{
		id: "9bc63dc2-1111-2222-3333-444444444444", state: "RUNNING",
		source: "cfx-near/orbs/cfx-5000-k8s-215952-edgenac-25.7-2131_20260807-wnv5a0cscf0003c-ncm",
		target: "artifact.it.att.com/apm0014228-oci-stage",
	})

	var buf bytes.Buffer
	if err := renderTransferList(&buf, resp, rateTrackers{}, listView{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), resp.Transfers[0].Source) {
		t.Errorf("a redirected listing shortened a path:\n%s", buf.String())
	}
}

// A table that cannot fit at any shortening is left exactly as it was.
//
// The reclaimable width is in two columns; the rest of the row - thirteen fixed
// columns and the padding between fifteen of them - exceeds a narrow terminal on
// its own. Shortening then buys nothing, because the row wraps either way, and
// costs the reader every path in the listing.
func TestATableThatCannotFitIsLeftAlone(t *testing.T) {
	const from = "cfx-near/orbs/cfx-5000-k8s-215952-edgenac-25.7-2131_20260807-wnv5a0cscf0003c-ncm"

	rows := [][]string{
		{"ID", "PRODUCT", "TAG", "FROM", "TO", "STATE", "DONE", "JOBS", "FAILED",
			"COPIED", "SAVED", "SPEED", "RUNNING", "ELAPSED", "ETA"},
		{"9bc63dc2", "cfx-5000-product", "25.7_mp2604_2131", from, "cfx-jfrog-lab",
			"failed", "100%", "2486/2489", "3", "0 B/63.7 GiB", "63.7 GiB", "-", "0",
			"8m17s", "-"},
	}

	fitCells(rows, transferElastic, 120)

	if rows[1][3] != from {
		t.Errorf("a path was shortened on the way to a row that still wraps: %q", rows[1][3])
	}
	if rows[1][1] != "cfx-5000-product" {
		t.Errorf("the product name was shortened: %q", rows[1][1])
	}

	// Wide enough to be worth it, and the same row now gives.
	fitCells(rows, transferElastic, 200)
	if rows[1][3] == from {
		t.Errorf("a row that could have been fitted was left to wrap: %q", rows[1][3])
	}
	if !strings.HasSuffix(rows[1][3], "-ncm") {
		t.Errorf("the shortened path lost the end that distinguishes it: %q", rows[1][3])
	}
}
