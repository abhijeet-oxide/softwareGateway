package authz

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkloadCredentialsValidateNamesEveryMissingField(t *testing.T) {
	err := WorkloadCredentials{}.Validate()
	if err == nil {
		t.Fatal("empty credentials validated")
	}
	for _, field := range []string{"issuer", "clientId", "clientSecret", "projectId"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("error does not name %q: %v", field, err)
		}
	}
}

// The one property of this type that matters if it is ever printed.
func TestWorkloadCredentialsStringHidesTheSecret(t *testing.T) {
	c := WorkloadCredentials{
		Issuer: "https://id.example", ClientID: "swgw-worker",
		ClientSecret: "s3cr3t-do-not-print", ProjectID: "42",
	}
	if got := c.String(); strings.Contains(got, c.ClientSecret) {
		t.Fatalf("String() leaked the secret: %s", got)
	}
}

func TestLoadWorkloadCredentialsSaysWhenTheFileIsNotThere(t *testing.T) {
	_, err := LoadWorkloadCredentials(filepath.Join(t.TempDir(), "absent.json"))
	if err == nil {
		t.Fatal("a missing file loaded")
	}
	// The likeliest cause, named. A JSON error here would send somebody to
	// look at a file that does not exist.
	if !strings.Contains(err.Error(), "not been seeded") {
		t.Errorf("unhelpful error for a missing file: %v", err)
	}
}

func TestLoadWorkloadCredentialsRejectsAnIncompleteFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.json")
	if err := os.WriteFile(path, []byte(`{"issuer":"https://id.example","clientId":"w"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorkloadCredentials(path); err == nil {
		t.Fatal("credentials with no secret and no project loaded")
	}
}

// tokenServer is an identity provider that issues one token per request and
// counts how many times it was asked.
func tokenServer(t *testing.T, expiresIn int64) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Errorf("token request was not a form: %v", err)
		}
		if got := r.PostForm.Get("grant_type"); got != "client_credentials" {
			t.Errorf("grant_type = %q, want client_credentials", got)
		}
		// Both halves of the scope are load bearing: the first puts the
		// project in the audience, the second is what makes the provider
		// assert the roles granted on it.
		scope := r.PostForm.Get("scope")
		if !strings.Contains(scope, "project:id:proj-1:aud") {
			t.Errorf("scope does not name the project audience: %q", scope)
		}
		if !strings.Contains(scope, "projects:roles") {
			t.Errorf("scope does not ask for roles: %q", scope)
		}
		if user, pass, ok := r.BasicAuth(); !ok || user != "swgw-worker" || pass != "shh" {
			t.Errorf("credentials not presented as Basic auth: %q %q %v", user, pass, ok)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "token-" + string(rune('0'+n)),
			"token_type":   "Bearer",
			"expires_in":   expiresIn,
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func testCreds(tokenURL string) WorkloadCredentials {
	return WorkloadCredentials{
		Issuer: "https://id.example", TokenURL: tokenURL,
		ClientID: "swgw-worker", ClientSecret: "shh", ProjectID: "proj-1",
	}
}

func TestTokenSourceCachesUntilCloseToExpiry(t *testing.T) {
	srv, calls := tokenServer(t, 3600)
	ts, err := NewTokenSource(testCreds(srv.URL), srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	first, err := ts.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		got, err := ts.Token(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got != first {
			t.Fatalf("token changed while still valid: %q then %q", first, got)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("asked the identity provider %d times for one valid token", n)
	}
}

// A cached token is never held past the moment it stops being valid. The
// leeway trims the hold; it must never extend it, which is what a naive clamp
// against "already expired" would do to a short-lived token.
func TestTokenSourceNeverCachesPastTheRealExpiry(t *testing.T) {
	for _, lifetime := range []int64{10, 45, 120, 3600} {
		srv, _ := tokenServer(t, lifetime)
		ts, err := NewTokenSource(testCreds(srv.URL), srv.Client())
		if err != nil {
			t.Fatal(err)
		}
		issued := time.Now()
		if _, err := ts.Token(context.Background()); err != nil {
			t.Fatal(err)
		}
		real := issued.Add(time.Duration(lifetime) * time.Second)
		if ts.expires.After(real) {
			t.Errorf("expires_in=%d cached until %s, past the token's own expiry at %s",
				lifetime, ts.expires.Sub(issued), real.Sub(issued))
		}
	}
}

func TestTokenSourceInvalidateForcesAFreshToken(t *testing.T) {
	srv, calls := tokenServer(t, 3600)
	ts, err := NewTokenSource(testCreds(srv.URL), srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	first, _ := ts.Token(context.Background())
	ts.Invalidate()
	second, err := ts.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Error("Invalidate did not drop the cached token")
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("token requests = %d, want 2", n)
	}
}

// The provider's own words, not a status code: an unknown client and a wrong
// secret are different repairs and only the body says which.
func TestTokenSourceReportsWhyTheProviderRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"client secret does not match"}`))
	}))
	defer srv.Close()

	ts, err := NewTokenSource(testCreds(srv.URL), srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = ts.Token(context.Background())
	if err == nil {
		t.Fatal("a refusal was reported as success")
	}
	if !strings.Contains(err.Error(), "client secret does not match") {
		t.Errorf("the provider's reason was dropped: %v", err)
	}

	ok, detail := ts.Health()
	if ok {
		t.Error("health is good with no obtainable token")
	}
	if !strings.Contains(detail, "client secret does not match") {
		t.Errorf("health does not name the reason: %s", detail)
	}
}

func TestTokenSourceHealthIsUnaskedBeforeTheFirstToken(t *testing.T) {
	srv, _ := tokenServer(t, 3600)
	ts, err := NewTokenSource(testCreds(srv.URL), srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if ok, detail := ts.Health(); ok || !strings.Contains(detail, "no token") {
		t.Errorf("health before any request = %v %q", ok, detail)
	}
	if _, err := ts.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	ok, detail := ts.Health()
	if !ok {
		t.Errorf("health after a good token = %v %q", ok, detail)
	}
	if !strings.Contains(detail, "swgw-worker") {
		t.Errorf("health does not say who it authenticated as: %s", detail)
	}
}

// Derived rather than configured, so a credentials file needs one url.
func TestTokenSourceDerivesTheTokenEndpointFromTheIssuer(t *testing.T) {
	c := testCreds("")
	ts, err := NewTokenSource(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://id.example/oauth/v2/token"; ts.url != want {
		t.Errorf("token url = %q, want %q", ts.url, want)
	}
}

func TestTokenExpiryReadsAnUnverifiedClaim(t *testing.T) {
	// header.payload.signature, payload carrying exp only. Nothing here
	// verifies anything; this is a diagnostic.
	raw := "e30." + base64URL(`{"exp":1788873122}`) + ".sig"
	got, ok := TokenExpiry(raw)
	if !ok {
		t.Fatal("a well-formed token was not read")
	}
	if !got.Equal(time.Unix(1788873122, 0)) {
		t.Errorf("exp = %v", got)
	}
	if _, ok := TokenExpiry("not-a-jwt"); ok {
		t.Error("a non-token was read as one")
	}
}

func base64URL(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}
