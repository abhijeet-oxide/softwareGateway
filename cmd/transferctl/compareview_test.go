package main

import (
	"bytes"
	"strings"
	"testing"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// A comparison is a diff, and it is laid out as one.
//
// The markers are the ones everybody already reads without being told: `-` only
// on the first end, `+` only on the second, `~` present on both and different.
// The point of the layout is that somebody can find what is wrong without
// reading any prose, and then read the prose for the one row that matters.

func TestOnlyDifferencesAreShownByDefault(t *testing.T) {
	var buf bytes.Buffer
	if err := renderCompare(&buf, mixedReport(), false, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	// The three that differ are there.
	for _, want := range []string{"lms", "cvlk", "brandnew"} {
		if !strings.Contains(out, want) {
			t.Errorf("a differing component %q is missing:\n%s", want, out)
		}
	}
	// The forty that agree are not, and the reader is told so rather than left
	// wondering whether they were checked.
	if strings.Contains(out, "mcc") {
		t.Errorf("an identical component is shown by default:\n%s", out)
	}
	if !strings.Contains(out, "not shown") {
		t.Errorf("the hidden rows are not accounted for:\n%s", out)
	}
}

func TestEveryComponentIsShownWithAll(t *testing.T) {
	var buf bytes.Buffer
	if err := renderCompare(&buf, mixedReport(), true, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "mcc") {
		t.Errorf("--all does not show an identical component:\n%s", buf.String())
	}
}

// Both ends are named at the top, with what was actually walked. Without it a
// reader cannot tell which way round `-` and `+` mean.
func TestBothEndsAreNamed(t *testing.T) {
	var buf bytes.Buffer
	if err := renderCompare(&buf, mixedReport(), false, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	for _, want := range []string{"near", "att-stage", "orbs/cfx-5000-k8s:orb_25.7"} {
		if !strings.Contains(out, want) {
			t.Errorf("the header does not carry %q:\n%s", want, out)
		}
	}
}

// A difference is a sentence and a table cell is not. Truncating "1.25.212
// points at sha256:8533f4a71a43" into a column removes the half that says what
// is wrong, so the sentences go underneath.
func TestDifferencesAreSpeltOutUnderTheTable(t *testing.T) {
	var buf bytes.Buffer
	if err := renderCompare(&buf, mixedReport(), false, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	if !strings.Contains(out, "Differences") {
		t.Fatalf("there is no differences block:\n%s", out)
	}
	if !strings.Contains(out, "points at sha256:8533f4a71a43") {
		t.Errorf("the mutation is not spelt out:\n%s", out)
	}
}

// Changed files are counted by default and named on request. A release that
// edited one configuration file should not print four hundred paths at somebody
// who is scanning to find which component is interesting.
func TestChangedFilesAreCountedThenNamed(t *testing.T) {
	var quiet, loud bytes.Buffer
	if err := renderCompare(&quiet, mixedReport(), false, false); err != nil {
		t.Fatal(err)
	}
	if err := renderCompare(&loud, mixedReport(), false, true); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(quiet.String(), "--files") {
		t.Errorf("the count does not say how to see the names:\n%s", quiet.String())
	}
	// COUNTED IN FILES, not in layers. "2 layers changed" is true and useless.
	if !strings.Contains(quiet.String(), "2 files") {
		t.Errorf("the change is not counted in files:\n%s", quiet.String())
	}
	if strings.Contains(quiet.String(), "layers changed") {
		t.Errorf("the change is still reported in layers:\n%s", quiet.String())
	}
	if strings.Contains(quiet.String(), "CONFIGURATION/example_parameters.json") {
		t.Errorf("file names are printed without --files:\n%s", quiet.String())
	}
	if !strings.Contains(loud.String(), "CONFIGURATION/example_parameters.json") {
		t.Errorf("--files does not name the changed files:\n%s", loud.String())
	}
}

// Content in a bundle's own repository that the release does not account for.
func TestUnexplainedContentIsReported(t *testing.T) {
	r := mixedReport()
	r.ExtraTagsB = []string{"orb_25.6_mp2601_2011"}

	var buf bytes.Buffer
	if err := renderCompare(&buf, r, false, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	if !strings.Contains(out, "not part of this release") {
		t.Errorf("unexplained content is not called out:\n%s", out)
	}
	if !strings.Contains(out, "orb_25.6_mp2601_2011") {
		t.Errorf("the unexplained tag is not named:\n%s", out)
	}
}

// A partial list must SAY it is partial. Presented as the whole answer,
// somebody would conclude a file was untouched when it was never looked at.
func TestAnIncompleteFileListSaysSo(t *testing.T) {
	r := mixedReport()
	for i := range r.Rows {
		if r.Rows[i].Verdict == "changed" {
			r.Rows[i].FilesTruncated = true
		}
	}

	var buf bytes.Buffer
	if err := renderCompare(&buf, r, false, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "not opened") {
		t.Errorf("a truncated file list does not say so:\n%s", buf.String())
	}
}

// An identical pair says so in one line, at the end, where the eye lands.
func TestAnIdenticalComparisonSaysSoPlainly(t *testing.T) {
	r := &v1.CompareResponse{
		Product: "cfx-5000-product",
		A:       v1.CompareEnd{Label: "lab", Reference: "nokia-lab/orbs/x:orb_25.7"},
		B:       v1.CompareEnd{Label: "prod", Reference: "nokia-prod/orbs/x:orb_25.7"},
		Rows:    []v1.CompareRow{sameRow("mcc")},
		Same:    1,
	}

	var buf bytes.Buffer
	if err := renderCompare(&buf, r, false, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "Identical") {
		t.Errorf("an identical comparison does not say so:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "Differences") {
		t.Errorf("an identical comparison prints a differences block:\n%s", buf.String())
	}
}

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// mixedReport is one of each verdict, which is what the layout has to handle.
func mixedReport() *v1.CompareResponse {
	return &v1.CompareResponse{
		Product: "cfx-5000-product",
		A: v1.CompareEnd{
			Label: "near", Reference: "orbs/cfx-5000-k8s:orb_25.7_mp2604_2131",
		},
		B: v1.CompareEnd{
			Label:     "att-stage",
			Reference: "apm0014228-oci-stage/orbs/cfx-5000-k8s:orb_25.7_mp2604_2131",
		},
		Rows: []v1.CompareRow{
			{
				Type: "image", Name: "cfx-5000-product/lms", Verdict: "only-a",
				A:           side("1.25.212", "sha256:438001f263abcdef0123456789abcdef01234567"),
				Differences: []string{"present on the first side only"},
			},
			{
				Type: "chart", Name: "cfx-5000-product/charts/brandnew", Verdict: "only-b",
				B:           side("2.0.0", "sha256:aabbccddeeff0011223344556677889900aabbcc"),
				Differences: []string{"present on the second side only"},
			},
			{
				Type: "image", Name: "cfx-5000-product/cvlk", Verdict: "changed",
				A: side("1.0.7", "sha256:4573b0b15cebdeadbeef00112233445566778899"),
				B: side("1.0.7", "sha256:8533f4a71a43cafebabe00112233445566778899"),
				Differences: []string{
					"cfx-5000-product/cvlk:1.0.7 points at sha256:8533f4a71a43 on " +
						"the second side, not sha256:4573b0b15ceb",
				},
				FilesChanged: []string{"CONFIGURATION/example_parameters.json"},
				FilesRemoved: []string{"DOCUMENTATION/old_readme"},
			},
			sameRow("mcc"),
		},
		Same:    1,
		Changed: 1,
		OnlyA:   1,
		OnlyB:   1,
	}
}

func sameRow(name string) v1.CompareRow {
	digest := "sha256:112233445566778899aabbccddeeff0011223344"
	return v1.CompareRow{
		Type: "image", Name: "cfx-5000-product/" + name, Verdict: "same",
		A: side("25.7.2503", digest), B: side("25.7.2503", digest),
	}
}

func side(tag, digest string) *v1.CompareSide {
	return &v1.CompareSide{Tag: tag, Digest: digest, Size: "1024"}
}
