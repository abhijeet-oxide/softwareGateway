package main

import (
	"bytes"
	"strings"
	"testing"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// What a package describe has to answer after a transfer of it has failed.
//
// The reported case: a vendor refused ONE component of a release —
// `HTTP 403: No valid entitlement found for End User: 215952 and Product sales
// items: CFXC24STD03…` — and nothing about the package said so. The refusal was
// recorded on the transfer that met it, which is the right place for it to
// live and the wrong place for it to stay.

func TestDescribeShowsWhyATransferOfThisPackageFailed(t *testing.T) {
	const reason = "walk package 68: fetch manifest sha256:6869d2e39a2477a: " +
		"HTTP 403: No valid entitlement found for End user: 215952 and " +
		"Product sales items: CFXC24STD03"

	pkg := &v1.Package{
		Tag: "orb_24.7.3099", Product: "cfx-5000-product",
		ManifestDigest: "sha256:c39f78313010", MediaType: "application/vnd.oci.image.index.v1+json",
		State: v1.PackageDiscovered,
		Transfers: []v1.PackageTransfer{
			{
				ID: "b3174872-1111-2222-3333-444444444444", Target: "cfx-jfrog-lab",
				State: v1.TransferFailed, FailureReason: reason,
				CreatedAt: "2026-08-16T23:41:00Z",
			},
			{
				ID: "9bc63dc2-1111-2222-3333-444444444444", Target: "cfx-jfrog-prod",
				State: v1.TransferSucceeded, CompletedAt: "2026-08-15T19:02:00Z",
			},
		},
	}

	var buf bytes.Buffer
	if err := renderPackageDetail(&buf, "cfx-5000-product", "orb_24.7.3099", pkg, nil); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	if !strings.Contains(out, "Transfers") {
		t.Fatalf("describe says nothing about what was attempted:\n%s", out)
	}
	// Per TARGET, because "has it been transferred" has one answer per
	// destination and this package has two different ones.
	for _, want := range []string{"cfx-jfrog-lab", "failed", "cfx-jfrog-prod", "succeeded"} {
		if !strings.Contains(out, want) {
			t.Errorf("the transfers section does not report %q:\n%s", want, out)
		}
	}
	// The vendor's own sentence, whole: it names the customer and the sales
	// item, which is what somebody takes to their account manager.
	if !strings.Contains(out, "No valid entitlement found for End user: 215952") {
		t.Errorf("the refusal is not reported in the vendor's own words:\n%s", out)
	}
	// And the digest, which is what turns "an entitlement is missing" into
	// "this component is the one".
	if !strings.Contains(out, "sha256:6869d2e39a2477a") {
		t.Errorf("the refused component's digest is not shown:\n%s", out)
	}
}

// A package nobody has tried to transfer says nothing at all, rather than
// growing an empty section.
func TestDescribeOfAnUntransferredPackageHasNoTransfersSection(t *testing.T) {
	pkg := &v1.Package{
		Tag: "orb_24.7.3099", Product: "cfx-5000-product",
		ManifestDigest: "sha256:c39f78313010", State: v1.PackageDiscovered,
	}

	var buf bytes.Buffer
	if err := renderPackageDetail(&buf, "cfx-5000-product", "orb_24.7.3099", pkg, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "Transfers") {
		t.Errorf("an untried package grew an empty transfers section:\n%s", buf.String())
	}
}

// STATE is bookkeeping about the CATALOGUE, not about transfers, and a column
// called STATE on a page about packages is read as "has this been transferred?"
// — which it has never meant.
func TestTheListingHidesStateUntilAskedFor(t *testing.T) {
	resp := &v1.ListPackagesResponse{Packages: []v1.Package{{
		Tag: "orb_25.7_mp2604_2131", ManifestDigest: "sha256:8533f4a71a43",
		State: v1.PackageDiscovered, DiscoveredAt: "2026-08-13T16:42:00Z",
	}}}

	var plain bytes.Buffer
	if err := renderPackageList(&plain, resp, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain.String(), "STATE") {
		t.Errorf("the default listing still carries the STATE column:\n%s", plain.String())
	}
	// The columns that answer the questions people actually ask are untouched.
	for _, want := range []string{"TAG", "DIGEST", "SIGNED", "DISCOVERED"} {
		if !strings.Contains(plain.String(), want) {
			t.Errorf("the default listing lost %q:\n%s", want, plain.String())
		}
	}

	var wide bytes.Buffer
	if err := renderPackageList(&wide, resp, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wide.String(), "STATE") || !strings.Contains(wide.String(), "discovered") {
		t.Errorf("--wide does not bring the STATE column back:\n%s", wide.String())
	}
}
