package main

import (
	"bytes"
	"strings"
	"testing"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// A scan of a real vendor catalogue meets content it is not allowed to read,
// every time, and that is not a failure.
//
// What it produced before: exit code 1, and thirty-seven lines of
//
//	orbs/cfx-5000-k8s tag orb_24.7.1186: resolve tag orb_24.7.1186: HTTP 403: forbidden
//
// differing only in the two parts that carry no information about what went
// wrong. A signal that fires on every scheduled scan forever is a signal
// nobody reads, and thirty-seven copies of one sentence is not a diagnosis.

func TestUnentitledContentIsReportedAsAFactRatherThanAFailure(t *testing.T) {
	resp := &v1.DiscoverPackagesResponse{
		Repositories: 42, TagsListed: 16921, TagsAdmitted: 68,
		Vocabulary: &v1.ScanVocabulary{
			Unit: "orb", Units: "orbs", Version: "orb version", Versions: "orb versions",
		},
		TagErrors: []v1.ScanIssue{
			notEntitledIssue("orbs/cfx-5000-k8s", "cfx-5000-k8s", "orb_24.7.1186", "24.7.1186"),
			notEntitledIssue("orbs/cfx-5000-k8s", "cfx-5000-k8s", "orb_24.7.4047", "24.7.4047"),
			notEntitledIssue("orbs/cfx-5000-product", "cfx-5000-product", "orb_25.7.1130", "25.7.1130"),
		},
	}

	var buf bytes.Buffer
	if err := renderDiscoverResult(&buf, "cfx-5000-product", resp); err != nil {
		t.Fatalf("a scan refused content it is not entitled to exited non-zero: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "3 orbs could not be read") {
		t.Errorf("the count is not reported in the vendor's nouns:\n%s", out)
	}
	if !strings.Contains(out, "no entitlement") {
		t.Errorf("the reason is not stated:\n%s", out)
	}

	// Grouped: one line per orb, versions after it. Three failures over two
	// orbs must not be three lines.
	if strings.Count(out, "cfx-5000-k8s") != 1 {
		t.Errorf("failures are not grouped by orb:\n%s", out)
	}
	if !strings.Contains(out, "24.7.1186, 24.7.4047") {
		t.Errorf("the versions of one orb are not listed together:\n%s", out)
	}

	// The vendor's names, not the paths. `orbs/` and `orb_` are structural.
	if strings.Contains(out, "orbs/cfx-5000-k8s") || strings.Contains(out, "orb_24.7") {
		t.Errorf("the raw paths are shown where the vendor has its own names:\n%s", out)
	}

	// The registry's own sentence, once — it names the customer and the
	// product, and it is identical across every row.
	if strings.Count(out, "No valid entitlement found") != 1 {
		t.Errorf("the vendor's sentence is missing or repeated:\n%s", out)
	}
}

// The summary counts things in the vendor's nouns too. "42 repositories
// scanned" makes a NEAR operator translate every line before reading it.
func TestTheScanSummarySpeaksTheVendorsVocabulary(t *testing.T) {
	resp := &v1.DiscoverPackagesResponse{
		Repositories: 42, TagsListed: 16921, TagsAdmitted: 68,
		Vocabulary: &v1.ScanVocabulary{
			Unit: "orb", Units: "orbs", Version: "orb version", Versions: "orb versions",
		},
	}

	var buf bytes.Buffer
	if err := renderDiscoverResult(&buf, "cfx-5000-product", resp); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	for _, want := range []string{"Orbs scanned", "Orb versions listed", "Orb versions after filters"} {
		if !strings.Contains(out, want) {
			t.Errorf("the summary does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Repositories scanned") || strings.Contains(out, "Tags listed") {
		t.Errorf("the summary still uses the OCI nouns for a vendor that renames them:\n%s", out)
	}
}

// A source with no vendor gets the standard nouns, unchanged. The shortening is
// the vendor's to declare, and inventing one for a conformant registry is the
// mistake this whole seam exists to prevent.
func TestAConformantSourceKeepsTheStandardNouns(t *testing.T) {
	resp := &v1.DiscoverPackagesResponse{Repositories: 3, TagsListed: 40, TagsAdmitted: 12}

	var buf bytes.Buffer
	if err := renderDiscoverResult(&buf, "vendor-a", resp); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	for _, want := range []string{"Repositories scanned", "Tags listed", "Tags after filters"} {
		if !strings.Contains(out, want) {
			t.Errorf("the summary does not say %q:\n%s", want, out)
		}
	}
}

// A genuine fault still fails the command. Folding entitlement in must not
// silence the case the exit code exists for.
func TestAGenuineTagFailureStillExitsNonZero(t *testing.T) {
	resp := &v1.DiscoverPackagesResponse{
		Repositories: 2, TagsListed: 10, TagsAdmitted: 4,
		TagErrors: []v1.ScanIssue{
			notEntitledIssue("orbs/a", "a", "orb_1.0", "1.0"),
			{Repository: "orbs/b", Tag: "orb_2.0",
				Message: "orbs/b tag orb_2.0: resolve tag: HTTP 504: gateway timeout"},
		},
	}

	var buf bytes.Buffer
	err := renderDiscoverResult(&buf, "cfx-5000-product", resp)
	if err == nil {
		t.Fatal("a gateway timeout during the scan exited zero")
	}
	// And the entitlement half is still reported as a fact, not counted as a
	// failure: one fault, not two.
	if strings.Contains(err.Error(), "2 ") {
		t.Errorf("unentitled content is counted as a failure: %v", err)
	}
}

func notEntitledIssue(repo, displayRepo, tag, displayTag string) v1.ScanIssue {
	return v1.ScanIssue{
		Repository: repo, Tag: tag,
		DisplayRepository: displayRepo, DisplayTag: displayTag,
		Class: classNotEntitled,
		Message: repo + " tag " + tag + ": resolve tag " + tag +
			": HTTP 403: No valid entitlement found for End User: 215952 and " +
			"Product sales items: CFXC24STD03.00.: forbidden",
	}
}
