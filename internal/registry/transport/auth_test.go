package transport

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestARefusedTokenIsRetriedRatherThanTrustedForever is the regression this
// package exists for now.
//
// Observed against Artifactory: a tag write was refused with 401 on a
// brand-new bearer token, and an unmodified retry of the identical request
// seconds later succeeded immediately. A wrong credential fails the same way
// every time; this did not, which is why one bounded extra attempt — forcing a
// fresh token exchange rather than trusting the one the registry just refused
// — is the right fix instead of surfacing the first 401 as terminal.
func TestARefusedTokenIsRetriedRatherThanTrustedForever(t *testing.T) {
	var realmHits, resourceHits atomic.Int32

	var realmURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		realmHits.Add(1)
		fmt.Fprintf(w, `{"token":"tok-%d","expires_in":300}`, realmHits.Load())
	})
	mux.HandleFunc("/v2/repo/manifests/orb_1.0", func(w http.ResponseWriter, r *http.Request) {
		n := resourceHits.Add(1)
		switch {
		case n == 1:
			// No token attached yet: the ordinary first challenge.
			w.Header().Set("WWW-Authenticate",
				fmt.Sprintf(`Bearer realm="%s",service="registry",scope="repository:repo:pull,push"`, realmURL))
			w.WriteHeader(http.StatusUnauthorized)
		case n == 2:
			// A brand-new token, refused anyway — the replication-lag case.
			w.Header().Set("WWW-Authenticate",
				fmt.Sprintf(`Bearer realm="%s",service="registry",scope="repository:repo:pull,push"`, realmURL))
			w.WriteHeader(http.StatusUnauthorized)
		default:
			// The retry, on a token fetched fresh because the first was
			// discarded rather than reused.
			if got := r.Header.Get("Authorization"); got != "Bearer tok-2" {
				t.Errorf("third attempt used %q, want the freshly re-fetched token", got)
			}
			w.WriteHeader(http.StatusOK)
		}
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()
	realmURL = srv.URL + "/token"

	c := mustClient(t, Config{Registry: "test-registry", Repository: "repo"})
	resp, err := c.Get(srv.URL + "/v2/repo/manifests/orb_1.0")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 — the bounded retry should have cleared the transient 401", resp.StatusCode)
	}
	if got := resourceHits.Load(); got != 3 {
		t.Errorf("resource was hit %d times, want 3 (initial, refused-token, cleared)", got)
	}
	if got := realmHits.Load(); got != 2 {
		t.Errorf("realm was hit %d times, want 2 — the refused token must trigger a real re-exchange, not a repeat", got)
	}
}

// TestAGenuinelyBadCredentialStillFailsFast proves the bound: this is not an
// infinite retry loop dressed up as a fix. A credential that is wrong stays
// wrong for every attempt, and the loop must still stop.
func TestAGenuinelyBadCredentialStillFailsFast(t *testing.T) {
	var hits atomic.Int32
	var realmURL string

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"token":"tok","expires_in":300}`)
	})
	mux.HandleFunc("/v2/repo/manifests/orb_1.0", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("WWW-Authenticate",
			fmt.Sprintf(`Bearer realm="%s",service="registry",scope="repository:repo:pull,push"`, realmURL))
		w.WriteHeader(http.StatusUnauthorized)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()
	realmURL = srv.URL + "/token"

	c := mustClient(t, Config{Registry: "test-registry", Repository: "repo"})
	resp, err := c.Get(srv.URL + "/v2/repo/manifests/orb_1.0")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 — a standing bad credential must still be reported", resp.StatusCode)
	}
	if got := hits.Load(); got != maxReauthAttempts {
		t.Errorf("resource was hit %d times, want the bound of %d", got, maxReauthAttempts)
	}
}

// TestABodyWriteCanReauthenticateAfterTokenExpiry is the regression for the
// intermittent tag-write 401.
//
// A manifest PUT carries a body, and the client that makes it (ORAS) sets no
// GetBody. The registry token expires 60 seconds after issue when the registry
// omits expires_in — which Artifactory does — so the final tag write of a long
// transfer arrived with no cached token, met a 401, and could not be replayed
// to re-authenticate because its body had already been consumed. This proves
// the transport now buffers a small body so that write recovers.
func TestABodyWriteCanReauthenticateAfterTokenExpiry(t *testing.T) {
	var resourceHits atomic.Int32
	var realmURL string
	const manifest = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		// No expires_in, exactly like Artifactory's reference tokens.
		fmt.Fprint(w, `{"token":"tok"}`)
	})
	mux.HandleFunc("/v2/repo/manifests/orb_1.0", func(w http.ResponseWriter, r *http.Request) {
		n := resourceHits.Add(1)
		if n == 1 {
			// First arrival with no token: the challenge, as for any write
			// whose cached token has lapsed.
			w.Header().Set("WWW-Authenticate",
				fmt.Sprintf(`Bearer realm="%s",service="registry",scope="repository:repo:pull,push"`, realmURL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// The retry must carry the body in full, or the manifest it writes is
		// truncated — the whole point of buffering it.
		body, _ := io.ReadAll(r.Body)
		if string(body) != manifest {
			t.Errorf("retry sent %d bytes, want the whole manifest replayed", len(body))
		}
		w.WriteHeader(http.StatusCreated)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()
	realmURL = srv.URL + "/token"

	c := mustClient(t, Config{Registry: "test-registry", Repository: "repo"})

	// A body reader with NO GetBody, as ORAS constructs a manifest PUT.
	req, err := http.NewRequest(http.MethodPut, srv.URL+"/v2/repo/manifests/orb_1.0",
		nopGetBody{strings.NewReader(manifest)})
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(manifest))
	req.GetBody = nil

	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201 — a body write must recover from an expired token", resp.StatusCode)
	}
	if got := resourceHits.Load(); got != 2 {
		t.Errorf("resource hit %d times, want 2 (challenge then authenticated retry)", got)
	}
}

// nopGetBody wraps a reader so http.NewRequest cannot infer a GetBody from it,
// reproducing the body shape ORAS hands the transport.
type nopGetBody struct{ io.Reader }

func (nopGetBody) Close() error { return nil }
