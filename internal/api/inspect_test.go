package api

import (
	"net/http"
	"strings"
	"testing"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// The bug these exist for.
//
// `POST /packages/{package}:inspect` was registered as a chi partial-segment
// pattern, and chi splits such a segment at the FIRST occurrence of the
// delimiter. For a tag that is the colon before `inspect` and everything works.
// For a DIGEST — `sha256:ccbd…:inspect` — the first colon is inside the
// reference, so `{package}` bound to `sha256`, the literal `:inspect` failed to
// match `:ccbd…`, and the request fell through to the GET-only route. What came
// back was:
//
//	INVALID_ARGUMENT: POST is not supported on /api/v1/products/…
//
// which names neither the real problem nor anything the caller could change.
// The route is now the plain package pattern with the verb split in the
// handler, and these cases are the reason it stays that way.

func TestInspectAcceptsEveryReferenceForm(t *testing.T) {
	h := newAPIHarness(t)
	h.seedPackage("orb_23.8.1076", digestA)

	cases := []struct {
		name string
		path string
	}{
		{
			name: "tag",
			path: "/api/v1/products/vendor-a/packages/orb_23.8.1076:inspect",
		},
		{
			// The case that was broken. A digest contains a colon of its own,
			// so it is the one form a first-colon split cannot survive.
			name: "digest",
			path: "/api/v1/products/vendor-a/packages/" + digestA + ":inspect",
		},
		{
			// A scoped reference travels as `repository=` rather than in the
			// path, because a slash cannot survive a path segment. Included so
			// the two mechanisms are known to compose.
			name: "repository-scoped tag",
			path: "/api/v1/products/vendor-a/packages/orb_23.8.1076:inspect" +
				"?repository=vendor-a%2Fplatform",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var resp v1.InspectPackageResponse
			if code := h.post(tc.path, "{}", &resp); code != http.StatusOK {
				t.Fatalf("POST %s = %d, want 200", tc.path, code)
			}
			if resp.Package.Tag != "orb_23.8.1076" {
				t.Errorf("inspected package tag = %q, want orb_23.8.1076", resp.Package.Tag)
			}
		})
	}
}

// The client must produce the URLs the server accepts. Testing the two halves
// separately is what let them disagree in the first place: the handler was
// correct and unreachable.
func TestClientInspectURLsReachTheHandler(t *testing.T) {
	h := newAPIHarness(t)
	h.seedPackage("orb_23.8.1076", digestA)

	client := v1.NewClient(h.server.URL)

	for _, ref := range []string{
		"orb_23.8.1076",
		digestA,
		"vendor-a/platform:orb_23.8.1076",
	} {
		resp, err := client.InspectPackage(t.Context(), "vendor-a", ref)
		if err != nil {
			t.Fatalf("InspectPackage(%q): %v", ref, err)
		}
		if resp.Package.Tag != "orb_23.8.1076" {
			t.Errorf("InspectPackage(%q) returned tag %q", ref, resp.Package.Tag)
		}
	}
}

// A POST that names no custom method must say so rather than reporting the
// method as unsupported, which is what the router used to do and which sent
// people looking at their HTTP client.
func TestPostWithoutACustomMethodExplainsItself(t *testing.T) {
	h := newAPIHarness(t)
	h.seedPackage("orb_23.8.1076", digestA)

	var prob v1.Problem
	code := h.post("/api/v1/products/vendor-a/packages/orb_23.8.1076", "{}", &prob)
	if code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", code)
	}
	if prob.Code != v1.CodeInvalidArgument {
		t.Errorf("problem code = %s, want %s", prob.Code, v1.CodeInvalidArgument)
	}
	// The fix has to be IN the message. "invalid argument" alone is the error
	// this replaced.
	if !strings.Contains(prob.Detail, "inspect") {
		t.Errorf("detail %q does not name the method to use", prob.Detail)
	}
}

// An unknown verb is a different error from a missing one, and both are the
// caller's to fix.
func TestUnknownCustomMethodIsNamed(t *testing.T) {
	h := newAPIHarness(t)
	h.seedPackage("orb_23.8.1076", digestA)

	var prob v1.Problem
	code := h.post("/api/v1/products/vendor-a/packages/orb_23.8.1076:expand", "{}", &prob)
	if code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", code)
	}
	if !strings.Contains(prob.Detail, "expand") || !strings.Contains(prob.Detail, "inspect") {
		t.Errorf("detail %q should name both what was asked for and what exists", prob.Detail)
	}
}
