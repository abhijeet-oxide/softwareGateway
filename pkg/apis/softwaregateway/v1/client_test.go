package v1

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSlowServerIsATimeoutNotAnOutage is the fix for the message operators kept
// hitting on `products check` and `packages discover`:
//
//	coordinator unreachable: context deadline exceeded (Client.Timeout
//	exceeded while awaiting headers)
//
// The Coordinator was neither unreachable nor broken. It was working.
func TestSlowServerIsATimeoutNotAnOutage(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	c := NewClient(srv.URL, WithTimeout(150*time.Millisecond))
	_, err := c.ListProducts(context.Background())

	if err == nil {
		t.Fatal("expected a timeout")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("expected ErrTimeout, got: %v", err)
	}
	if errors.Is(err, ErrUnreachable) {
		t.Error("a slow server must not be reported as unreachable - that sends " +
			"the operator to investigate a healthy service")
	}
	// The elapsed budget belongs in the message: Go's own wording names neither
	// the deadline nor the flag that changes it.
	if !strings.Contains(err.Error(), "150ms") {
		t.Errorf("the message should name the timeout that was in force, got: %v", err)
	}
}

func TestClosedPortIsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	c := NewClient(url, WithTimeout(2*time.Second))
	_, err := c.ListProducts(context.Background())

	if err == nil {
		t.Fatal("expected a connection failure")
	}
	if !errors.Is(err, ErrUnreachable) {
		t.Errorf("expected ErrUnreachable, got: %v", err)
	}
	if errors.Is(err, ErrTimeout) {
		t.Error("a refused connection is not a timeout")
	}
}

// A cancelled caller context is a timeout for classification purposes: no
// answer came back and the request may still be running server-side.
func TestCancelledContextIsNotReportedAsAnOutage(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	c := NewClient(srv.URL, WithTimeout(30*time.Second))
	if _, err := c.ListProducts(ctx); !errors.Is(err, ErrTimeout) {
		t.Errorf("expected ErrTimeout for a deadline-exceeded context, got: %v", err)
	}
}

// A slash cannot survive a URL path segment: %2F is decoded before routing, so
// `orbs/core:v1` in the path arrives at the router as two segments and matches
// nothing - a 404 for a package that exists. The repository moves to the query
// string instead.
func TestScopedReferenceMovesTheRepositoryToTheQuery(t *testing.T) {
	cases := []struct {
		ref          string
		wantSegment  string
		wantQuery    string
		whatItCovers string
	}{
		{"v1.0.0", "v1.0.0", "", "a bare tag is left alone"},
		{
			"sha256:1111111111111111111111111111111111111111111111111111111111111111",
			"sha256:1111111111111111111111111111111111111111111111111111111111111111", "",
			"a digest is algorithm:hex, not repository:tag",
		},
		{"cfx-5000-db:v1", "cfx-5000-db:v1", "", "a repository with no slash routes as one segment"},
		{
			"orbs/cfx-5000-db:orb_23.8.1076",
			"orb_23.8.1076", "?repository=orbs%2Fcfx-5000-db",
			"a slashed repository moves to the query",
		},
		{"/orbs/core/:v1", "v1", "?repository=orbs%2Fcore", "surrounding slashes are trimmed"},
	}

	for _, c := range cases {
		seg, query := splitPackageRef(c.ref)
		if seg != c.wantSegment || query != c.wantQuery {
			t.Errorf("splitPackageRef(%q) = (%q, %q), want (%q, %q) - %s",
				c.ref, seg, query, c.wantSegment, c.wantQuery, c.whatItCovers)
		}
	}
}

// The security calls must reach the paths the router registers, and must not
// emit two `?` when a reference already carried a repository.
func TestSecurityClientURLsReachTheHandler(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	ctx := t.Context()

	if _, err := c.PackageSecurity(ctx, "cfx", "orbs/core:v1",
		PackageSecurityOptions{Detail: true, ProgressToken: "tok"}); err != nil {
		t.Fatalf("PackageSecurity: %v", err)
	}
	if _, err := c.CompareSecurity(ctx, "cfx", "25.7.2100",
		SecurityCompareRequest{Against: "25.7.2131"}); err != nil {
		t.Fatalf("CompareSecurity: %v", err)
	}
	if _, err := c.SearchSecurity(ctx, "cfx", "CVE-2024-3094",
		SecuritySearchOptions{Kind: "cve", Exact: true}); err != nil {
		t.Fatalf("SearchSecurity: %v", err)
	}
	if _, err := c.SecurityProgress(ctx, "tok"); err != nil {
		t.Fatalf("SecurityProgress: %v", err)
	}

	want := []string{
		// The repository travels as a query parameter because a slash cannot
		// survive a path segment - and the added parameters join it with `&`.
		"GET /api/v1/products/cfx/packages/v1/security?repository=orbs%2Fcore&detail=true&progressToken=tok",
		// The colon before the verb is structural and must not be escaped.
		"POST /api/v1/products/cfx/packages/25.7.2100:compareSecurity?",
		"GET /api/v1/products/cfx/security/search?exact=true&kind=cve&q=CVE-2024-3094",
		"GET /api/v1/security/progress/tok?",
	}
	if len(got) != len(want) {
		t.Fatalf("made %d requests, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request %d:\n got %s\nwant %s", i, got[i], want[i])
		}
	}
}
