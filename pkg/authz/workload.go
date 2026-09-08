package authz

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// The PROVING half of this package, opposite Verifier.
//
// A person authenticates in a browser and the Coordinator verifies what comes
// back. A worker has no browser and nobody to type anything, so it proves who
// it is with the OAuth 2.0 client credentials grant (RFC 6749 section 4.4) and
// receives an ordinary access token. Everything downstream is then identical:
// the same header, the same signature check, the same identity, the same
// roles. There is no second trust root and no separate verification path to
// keep in step with this one, which is the whole reason the data plane
// authenticates this way rather than with a token of its own invention.
//
// # What this is NOT
//
// It is not a shared secret in an environment variable. Those are passwords
// that never expire, are copied into every manifest that mentions the service,
// and can only be revoked by redeploying everything holding them. The
// credential here buys SHORT-LIVED tokens from the identity provider, holds
// exactly the roles the provider says it holds, and is revoked by disabling
// one machine account.
//
// # One identity for the whole fleet
//
// Workers are interchangeable by design: stateless, holding nothing, deciding
// nothing. A worker's id names it in the queue, which is scheduling and not
// authorization. Per-worker credentials would carry identical grants, so they
// would contain nothing, and would cost the property the fleet exists for:
// that scaling it is a number and not a conversation.

// WorkloadCredentials is what a machine account needs in order to obtain a
// token. It is written by the deployment's seeder and read from a file that is
// mounted, never from an argument or an image layer.
type WorkloadCredentials struct {
	// Issuer is the identity provider's PUBLIC url, the one that lands in the
	// token's `iss` claim and that the Coordinator validates against.
	Issuer string `json:"issuer"`
	// TokenURL is where tokens are actually fetched from, which in a container
	// network is an internal address that Issuer is not reachable at. Empty
	// derives it from Issuer.
	TokenURL string `json:"tokenUrl"`
	ClientID string `json:"clientId"`
	// ClientSecret is a credential. Nothing in this package logs it, and
	// String() below exists so that a struct printed in a debug line cannot.
	ClientSecret string `json:"clientSecret"`
	// ProjectID is the project whose roles this workload holds.
	//
	// It travels WITH the credentials because the workload cannot derive it.
	// ZITADEL keys its role claim by project id and asserts roles only for a
	// project named in the token's audience, which is asked for as a scope: a
	// token fetched without it verifies perfectly and arrives carrying no
	// roles at all, which reads as a permissions problem rather than as a
	// missing scope. That is an afternoon, so it is a required field.
	ProjectID string `json:"projectId"`
}

// String redacts the secret. A credentials struct reaches a log line sooner or
// later, and when it does this is the difference between a diagnostic and an
// incident.
func (c WorkloadCredentials) String() string {
	return fmt.Sprintf("WorkloadCredentials{Issuer:%s ClientID:%s ProjectID:%s ClientSecret:<redacted>}",
		c.Issuer, c.ClientID, c.ProjectID)
}

// Validate reports what is missing, naming the field rather than failing at
// the token endpoint with an OAuth error code.
func (c WorkloadCredentials) Validate() error {
	var missing []string
	if c.Issuer == "" {
		missing = append(missing, "issuer")
	}
	if c.ClientID == "" {
		missing = append(missing, "clientId")
	}
	if c.ClientSecret == "" {
		missing = append(missing, "clientSecret")
	}
	if c.ProjectID == "" {
		missing = append(missing, "projectId")
	}
	if len(missing) > 0 {
		return fmt.Errorf("workload credentials are incomplete: no %s", strings.Join(missing, ", "))
	}
	return nil
}

// LoadWorkloadCredentials reads a credentials file written by the seeder.
//
// A missing file is reported as such rather than as a parse failure: it is by
// far the likeliest thing to be wrong, it means the volume is not mounted or
// the seeder has not run, and neither is improved by a JSON error.
func LoadWorkloadCredentials(path string) (WorkloadCredentials, error) {
	var c WorkloadCredentials
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, fmt.Errorf("no workload credentials at %s: the identity provider has not been seeded, "+
				"or the volume holding them is not mounted here", path)
		}
		return c, fmt.Errorf("read workload credentials at %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("workload credentials at %s are not valid JSON: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// tokenLeeway is how long before expiry a cached token stops being used.
//
// Generous on purpose. A token that expires in flight costs a lease attempt
// and a confusing 401 in a log; refetching one a minute early costs one HTTP
// call every twelve hours.
const tokenLeeway = 60 * time.Second

// TokenSource hands out access tokens for a workload, fetching a new one only
// when the one it holds is close to expiring.
//
// Safe for concurrent use: a worker leases, heartbeats and reports from
// several goroutines at once, and an unsynchronised source would have all of
// them fetch a token simultaneously the moment one expired.
type TokenSource struct {
	creds  WorkloadCredentials
	client *http.Client
	scope  string
	url    string

	mu      sync.Mutex
	token   string
	expires time.Time
	// lastErr is kept so a health check can name the reason the fleet is idle
	// rather than only reporting that it is.
	lastErr error
	lastTry time.Time
}

// NewTokenSource prepares a source. It performs no network call, so a worker
// starts and reports its own state rather than failing to start because the
// identity provider was restarting at that moment.
func NewTokenSource(creds WorkloadCredentials, client *http.Client) (*TokenSource, error) {
	if err := creds.Validate(); err != nil {
		return nil, err
	}
	tokenURL := creds.TokenURL
	if tokenURL == "" {
		tokenURL = strings.TrimRight(creds.Issuer, "/") + "/oauth/v2/token"
	}
	// The token endpoint is reached at an internal address while the issuer
	// answers only to its public name: ZITADEL selects its instance from the
	// Host header and answers 404 to one it does not recognise, so a request
	// to `zitadel:8080` is refused unless it says `Host: localhost:8090`. The
	// same fact the Verifier states about fetching keys, from the other side.
	host := ""
	if u, err := url.Parse(creds.Issuer); err == nil {
		host = u.Host
	}
	if client == nil {
		client = defaultHTTPClient()
	}
	if host != "" {
		c := *client
		base := http.DefaultTransport
		if client.Transport != nil {
			base = client.Transport
		}
		c.Transport = hostRewriter{host: host, base: base}
		client = &c
	}
	return &TokenSource{
		creds:  creds,
		client: client,
		url:    tokenURL,
		// Two scopes and both are load bearing. The first puts the project in
		// the audience; the second is what makes the provider assert the roles
		// granted on it. Without them the token is valid and anonymous.
		scope: "openid urn:zitadel:iam:org:project:id:" + creds.ProjectID + ":aud" +
			" urn:zitadel:iam:org:projects:roles",
	}, nil
}

// Token returns a valid access token, fetching one if necessary.
func (t *TokenSource) Token(ctx context.Context) (string, error) {
	t.mu.Lock()
	if t.token != "" && time.Now().Before(t.expires) {
		tok := t.token
		t.mu.Unlock()
		return tok, nil
	}
	t.mu.Unlock()

	tok, expiry, err := t.fetch(ctx)

	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastTry = time.Now()
	t.lastErr = err
	if err != nil {
		return "", err
	}
	t.token, t.expires = tok, expiry
	return tok, nil
}

// Invalidate drops the cached token.
//
// Called when the Coordinator refuses one. A token can stop being accepted
// before it expires - the account is disabled, a role is withdrawn, the
// provider's keys rotate - and a source that trusted its own clock would spend
// the remainder of the token's life retrying with the credential that was just
// refused.
func (t *TokenSource) Invalidate() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.token, t.expires = "", time.Time{}
}

// Health reports whether this workload can currently obtain a token.
//
// It uses the CACHE, so calling it does not spend a token request: a source
// holding a valid token is healthy by demonstration. It is only when there is
// nothing cached that it says what the last attempt failed with.
func (t *TokenSource) Health() (ok bool, detail string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && time.Now().Before(t.expires) {
		return true, fmt.Sprintf("authenticated as %s, token valid for %s",
			t.creds.ClientID, time.Until(t.expires).Round(time.Second))
	}
	if t.lastErr != nil {
		return false, t.lastErr.Error()
	}
	return false, "no token has been requested yet"
}

// tokenResponse is the part of RFC 6749 section 5.1 this needs.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
	Error       string `json:"error"`
	Description string `json:"error_description"`
}

// fetch performs the client credentials grant.
func (t *TokenSource) fetch(ctx context.Context) (string, time.Time, error) {
	form := url.Values{
		"grant_type": {"client_credentials"},
		"scope":      {t.scope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// HTTP Basic, per RFC 6749 section 2.3.1, which requires both halves to be
	// form-urlencoded BEFORE they are base64'd. Client id and secret here
	// contain nothing that needs it, and a credential that silently stops
	// working the day somebody's generator emits a "+" is not worth the two
	// saved lines.
	req.SetBasicAuth(url.QueryEscape(t.creds.ClientID), url.QueryEscape(t.creds.ClientSecret))

	resp, err := t.client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%s did not answer: %w", t.url, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out tokenResponse
	_ = json.Unmarshal(body, &out)

	if resp.StatusCode != http.StatusOK || out.AccessToken == "" {
		// The provider's own words, which name the actual fault: an unknown
		// client, a secret that no longer matches, a grant type the account is
		// not allowed to use. A bare status code sends somebody to look at the
		// network instead.
		detail := strings.TrimSpace(out.Description)
		if detail == "" {
			detail = strings.TrimSpace(out.Error)
		}
		if detail == "" {
			detail = strings.TrimSpace(string(body))
			if len(detail) > 200 {
				detail = detail[:200]
			}
		}
		if detail == "" {
			detail = resp.Status
		}
		return "", time.Time{}, fmt.Errorf("%s refused these credentials: %s", t.url, detail)
	}

	// A provider that says nothing is assumed to have issued something
	// ordinary. Nothing depends on the guess: the Coordinator verifies the
	// real expiry, and a token refused early is refetched by Invalidate.
	lifetime := 12 * time.Hour
	if out.ExpiresIn > 0 {
		lifetime = time.Duration(out.ExpiresIn) * time.Second
	}
	// Held for its lifetime less the leeway, and NEVER for longer than the
	// lifetime itself. A provider issuing tokens shorter than the leeway would
	// otherwise leave this either permanently expired, refetching on every
	// single call, or - far worse - holding a token past the moment it stopped
	// being valid and presenting it until something refused it.
	hold := lifetime - tokenLeeway
	if hold < lifetime/2 {
		hold = lifetime / 2
	}
	return out.AccessToken, time.Now().Add(hold), nil
}

// TokenExpiry reads the `exp` claim of a JWT WITHOUT verifying it.
//
// For diagnostics only, and it says so in the name: this is the client's own
// token, so there is nothing to defend against, and the alternative is a
// worker that cannot say when its credential runs out. Nothing decides
// anything on this - the Coordinator verifies signatures.
func TokenExpiry(raw string) (time.Time, bool) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}
