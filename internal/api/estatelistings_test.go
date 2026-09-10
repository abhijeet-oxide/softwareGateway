package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/discovery"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// THE TWO FLEET-WIDE READS, which exist to end a poll storm.
//
// Both replaced a per-product fan-out in the browser: the Overview asked
// GET /products/{p}/discovery once per product on a timer, and the Packages
// page asked GET /products/{p}/packages once per product and merged, paged and
// searched the answers in memory. What is worth pinning here is that each route
// answers for EVERY product in one request, because that is the whole point of
// it, and that the estate listing still pages and still searches - the two
// things the in-browser version could not do correctly.

func TestFleetDiscoveryStatusAnswersForEveryProduct(t *testing.T) {
	h := newAPIHarness(t)
	h.discoverer.progress = []discovery.SourceProgress{
		{Product: "vendor-a", Source: "vendor", Interval: 15 * time.Minute},
		{Product: "vendor-b", Source: "mirror", Interval: time.Hour},
	}

	var resp v1.DiscoveryStatusResponse
	if code := h.get("/api/v1/discovery", &resp); code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if !resp.Running {
		t.Error("running = false over a loop that is running")
	}
	if len(resp.Sources) != 2 {
		t.Fatalf("listed %d source(s), want both products' in one request", len(resp.Sources))
	}
	// The product travels WITH each source. Without it a single response
	// covering several products cannot be split up again, which is the only
	// thing the caller wants to do with it.
	seen := map[string]string{}
	for _, s := range resp.Sources {
		seen[s.Product] = s.Source
	}
	if seen["vendor-a"] != "vendor" || seen["vendor-b"] != "mirror" {
		t.Errorf("sources = %v, want each named under its own product", seen)
	}
	// Sources always serialise as an array: a caller iterating them must not
	// have to nil-check the field.
	if resp.Sources == nil {
		t.Error("sources must serialise as [] rather than null")
	}
}

// A follower runs no discovery loop, and says so rather than 404ing or
// answering with an empty estate that reads as "nothing is scheduled".
func TestFleetDiscoveryStatusOnAFollower(t *testing.T) {
	h := newAPIHarness(t)
	h.discoverer.running = false
	h.discoverer.progress = []discovery.SourceProgress{
		{Product: "vendor-a", Source: "vendor"},
	}

	var resp v1.DiscoveryStatusResponse
	if code := h.get("/api/v1/discovery", &resp); code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if resp.Running {
		t.Error("running = true on a replica whose loop is not running")
	}
	if len(resp.Sources) != 0 {
		t.Errorf("listed %d source(s); a follower holds no live progress to report", len(resp.Sources))
	}
}

func TestListAllPackagesSpansProductsAndPages(t *testing.T) {
	h := newAPIHarness(t)
	h.seedPackage("v1.0.0", digestA)
	h.seedPackage("v1.1.0", digestB)

	var all v1.ListPackagesResponse
	if code := h.get("/api/v1/packages", &all); code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if len(all.Packages) != 2 {
		t.Fatalf("listed %d release(s), want 2", len(all.Packages))
	}
	// Every row names its product. The estate listing is the first one whose
	// rows can differ, and a row that does not say which product it belongs to
	// cannot be linked to or filtered.
	for _, p := range all.Packages {
		if p.Product != "vendor-a" {
			t.Errorf("release %s carries product %q, want vendor-a", p.Tag, p.Product)
		}
	}

	// PAGING, which the in-browser merge could not do: one page, and a token
	// that leads to the next.
	var first v1.ListPackagesResponse
	if code := h.get("/api/v1/packages?pageSize=1", &first); code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if len(first.Packages) != 1 {
		t.Fatalf("page of 1 returned %d release(s)", len(first.Packages))
	}
	if first.NextPageToken == "" {
		t.Fatal("no next page token over a second page that exists")
	}

	var second v1.ListPackagesResponse
	if code := h.get("/api/v1/packages?pageSize=1&pageToken="+first.NextPageToken, &second); code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if len(second.Packages) != 1 {
		t.Fatalf("second page returned %d release(s)", len(second.Packages))
	}
	if second.Packages[0].Tag == first.Packages[0].Tag {
		t.Error("the second page repeated the first page's release")
	}
	// And it ENDS: a token is only offered while there is something behind it.
	if second.NextPageToken != "" {
		t.Errorf("next page token %q offered past the last release", second.NextPageToken)
	}
}

func TestListPackagesSearchIsServerSide(t *testing.T) {
	h := newAPIHarness(t)
	h.seedPackage("v1.0.0", digestA)
	h.seedPackage("v2.5.0", digestB)

	for _, path := range []string{
		"/api/v1/packages?q=v2.5",
		"/api/v1/products/vendor-a/packages?q=v2.5",
	} {
		var resp v1.ListPackagesResponse
		if code := h.get(path, &resp); code != http.StatusOK {
			t.Fatalf("GET %s: expected 200, got %d", path, code)
		}
		if len(resp.Packages) != 1 {
			t.Fatalf("GET %s matched %d release(s), want 1", path, len(resp.Packages))
		}
		if resp.Packages[0].Tag != "v2.5.0" {
			t.Errorf("GET %s matched %q, want v2.5.0", path, resp.Packages[0].Tag)
		}
	}

	// A term matching nothing returns an empty page rather than everything -
	// the failure mode of a filter built by string concatenation.
	var none v1.ListPackagesResponse
	if code := h.get("/api/v1/packages?q=nothing-like-this", &none); code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if len(none.Packages) != 0 {
		t.Errorf("a term matching nothing returned %d release(s)", len(none.Packages))
	}
}

// A LISTING ROW CARRIES ITS OWN HISTORY, which is what lets it state a status.
//
// Without this the browser fetched the two hundred most recent transfers of the
// whole estate on every listing and joined them by package id - a join that is
// simply wrong once the listing is paged, because page four's releases were
// downloaded long before the two-hundredth most recent transfer. One batched
// query per page replaces it. See attachTransfers.
func TestPackageListingCarriesTransferHistory(t *testing.T) {
	h := newAPIHarness(t)
	id := h.seedTransfer("aaaa1111-0000-0000-0000-000000000001")

	for _, path := range []string{
		"/api/v1/packages",
		"/api/v1/products/vendor-a/packages",
	} {
		var resp v1.ListPackagesResponse
		if code := h.get(path, &resp); code != http.StatusOK {
			t.Fatalf("GET %s: expected 200, got %d", path, code)
		}
		if len(resp.Packages) != 1 {
			t.Fatalf("GET %s listed %d release(s), want 1", path, len(resp.Packages))
		}
		got := resp.Packages[0].Transfers
		if len(got) != 1 {
			t.Fatalf("GET %s carried %d transfer(s) on the row, want 1", path, len(got))
		}
		if got[0].ID != id {
			t.Errorf("GET %s carried transfer %q, want %q", path, got[0].ID, id)
		}
		// The STATE is the whole reason the history is here: it is what the
		// row's status is derived from.
		if got[0].State != v1.TransferState(strings.ToUpper("running")) {
			t.Errorf("GET %s carried state %q, want RUNNING", path, got[0].State)
		}
		// And the DESTINATION, which is what says whether a release reached
		// production or only the lab.
		if got[0].Target == "" {
			t.Errorf("GET %s carried no destination; a landed release has to say where", path)
		}
	}
}

// A PAGER NEEDS A COUNT, and the page token is not one.
//
// The token says only "there is at least one more page". An interface that drew
// page NUMBERS from it showed exactly one page beyond wherever the reader was
// and grew another every time they moved: ten per page read as "twenty
// releases, at most", and switching to fifty per page still read as two pages,
// because the arithmetic had nothing to do with how many releases exist.
func TestPackageListingReportsHowManyThereAre(t *testing.T) {
	h := newAPIHarness(t)
	h.seedPackage("v1.0.0", digestA)
	h.seedPackage("v1.1.0", digestB)
	h.seedPackage("v1.2.0", "sha256:"+strings.Repeat("3", 64))

	cases := []struct{ name, path string }{
		{"estate", "/api/v1/packages"},
		{"one product", "/api/v1/products/vendor-a/packages"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A page that does not hold everything still says how much there is,
			// which is the whole point: three releases, one at a time.
			var first v1.ListPackagesResponse
			if code := h.get(tc.path+"?pageSize=1", &first); code != http.StatusOK {
				t.Fatalf("expected 200, got %d", code)
			}
			if first.TotalSize != 3 {
				t.Errorf("totalSize = %d on a page of 1, want 3", first.TotalSize)
			}

			// And it does not change as the reader walks. A total that grew per
			// page is the defect this replaced.
			var second v1.ListPackagesResponse
			if code := h.get(tc.path+"?pageSize=1&pageToken="+first.NextPageToken, &second); code != http.StatusOK {
				t.Fatalf("expected 200, got %d", code)
			}
			if second.TotalSize != 3 {
				t.Errorf("totalSize = %d on page two, want 3 - it must not depend on the page", second.TotalSize)
			}

			// A page that holds everything answers from what it already has,
			// without a second query to learn what len() says.
			var whole v1.ListPackagesResponse
			if code := h.get(tc.path, &whole); code != http.StatusOK {
				t.Fatalf("expected 200, got %d", code)
			}
			if whole.TotalSize != 3 {
				t.Errorf("totalSize = %d on a single page, want 3", whole.TotalSize)
			}
			if whole.NextPageToken != "" {
				t.Errorf("next page token %q offered over a listing that fits", whole.NextPageToken)
			}
		})
	}

	// THE COUNT IS OF WHAT THE FILTER MATCHES, not of the table. A search that
	// matches one release says one, however many there are.
	var searched v1.ListPackagesResponse
	if code := h.get("/api/v1/packages?q=v1.2&pageSize=1", &searched); code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if searched.TotalSize != 1 {
		t.Errorf("totalSize = %d for a search matching one release, want 1", searched.TotalSize)
	}
}

// The transfer listing carries the same count, for the same reason.
func TestTransferListingReportsHowManyThereAre(t *testing.T) {
	h := newAPIHarness(t)
	h.seedTransfer("aaaa1111-0000-0000-0000-000000000001")
	h.seedTransfer("bbbb2222-0000-0000-0000-000000000002")

	var first v1.ListTransfersResponse
	if code := h.get("/api/v1/transfers?pageSize=1&view=summary", &first); code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if first.TotalSize != 2 {
		t.Errorf("totalSize = %d on a page of 1, want 2", first.TotalSize)
	}

	var second v1.ListTransfersResponse
	if code := h.get("/api/v1/transfers?pageSize=1&view=summary&pageToken="+first.NextPageToken,
		&second); code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if second.TotalSize != 2 {
		t.Errorf("totalSize = %d on page two, want 2 - it must not depend on the page", second.TotalSize)
	}
}
