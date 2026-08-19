package transport

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
)

// authTransport implements the OCI Distribution token flow:
//
//  1. request           -> 401 with WWW-Authenticate: Bearer realm=,service=,scope=
//  2. GET <realm>?service=&scope=   (Basic credentials)
//  3. -> {"token": "...", "expires_in": 300}
//  4. retry the original request with Authorization: Bearer <token>
//
// Implemented once, here. Vendor differences are entirely in step 2's
// credential shape, which is why ACR, Artifactory and Quay need no transport
// of their own (docs/design/06 §4).
type authTransport struct {
	next  http.RoundTripper
	cfg   Config
	cache *tokenCache
	group singleflight.Group
}

// newAuthTransportWithCache wraps next, sharing the given token cache.
//
// The cache is per SOURCE and keyed by scope, so two repositories on one
// registry each get their own entry while sharing the machinery - and a source
// with forty repositories performs forty token exchanges rather than forty per
// scan.
func newAuthTransportWithCache(next http.RoundTripper, cfg Config, cache *tokenCache) http.RoundTripper {
	if cache == nil {
		cache = newTokenCache()
	}
	return &authTransport{next: next, cfg: cfg, cache: cache}
}

func (t *authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	scope := t.cfg.Scope
	if scope == "" {
		scope = scopeFor(r, t.cfg.Repository)
	}

	// Attach a cached token if we have one. This is the whole point of the
	// cache: without it an 850-blob package performs 850 token exchanges,
	// adding a round trip to every request and reliably tripping the vendor's
	// own rate limits. It is one of the highest-value small optimizations in
	// the system and one of the easiest to omit by accident.
	if tok, ok := t.cache.get(scope); ok {
		r = withBearer(r, tok)
	}

	resp, err := t.next.RoundTrip(r)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}

	challenge := parseChallenge(resp.Header.Get("WWW-Authenticate"))
	if challenge.scheme == "" {
		return resp, nil // 401 without a challenge: nothing to act on
	}

	// A request whose body cannot be replayed must not be retried.
	if r.Body != nil && r.Body != http.NoBody && r.GetBody == nil {
		return resp, nil
	}
	drain(resp)

	if challenge.scheme == "basic" {
		if t.cfg.Username == "" {
			// Classified, so the retry layer above stops immediately rather
			// than spending the full backoff schedule on a problem no amount
			// of waiting can fix.
			return nil, fmt.Errorf("%w: registry %s requires basic auth but no credentials are configured",
				registry.ErrUnauthorized, t.cfg.Registry)
		}
		req, err := rewind(r, 1)
		if err != nil {
			return nil, err
		}
		req.SetBasicAuth(t.cfg.Username, t.cfg.Password)
		return t.next.RoundTrip(req)
	}

	tok, err := t.fetchToken(r, challenge, scope)
	if err != nil {
		return nil, err
	}

	req, err := rewind(r, 1)
	if err != nil {
		return nil, err
	}
	return t.next.RoundTrip(withBearer(req, tok))
}

// fetchToken exchanges credentials for a bearer token, once per scope even
// under concurrency.
//
// singleflight matters here: sixteen concurrent requests hitting an expiry
// would otherwise perform sixteen identical token exchanges, which is both
// wasteful and a good way to be throttled.
func (t *authTransport) fetchToken(r *http.Request, c challenge, scope string) (string, error) {
	key := c.realm + "|" + c.service + "|" + scope

	v, err, _ := t.group.Do(key, func() (any, error) {
		// Another goroutine may have populated the cache while we queued.
		if tok, ok := t.cache.get(scope); ok {
			return tok, nil
		}

		u, err := url.Parse(c.realm)
		if err != nil {
			return nil, fmt.Errorf("parse auth realm %q: %w", c.realm, err)
		}
		q := u.Query()
		if c.service != "" {
			q.Set("service", c.service)
		}
		if scope != "" {
			q.Set("scope", scope)
		}
		u.RawQuery = q.Encode()

		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		if t.cfg.Username != "" {
			req.Header.Set("Authorization", "Basic "+basicAuth(t.cfg.Username, t.cfg.Password))
		}

		resp, err := t.next.RoundTrip(req)
		if err != nil {
			return nil, fmt.Errorf("fetch token from %s: %w", c.realm, err)
		}
		defer func() {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
			_ = resp.Body.Close()
		}()

		if resp.StatusCode != http.StatusOK {
			// Bad credentials at the token endpoint are terminal for the same
			// reason: they will not become good by being asked again.
			err := registry.ErrUnavailable
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				err = registry.ErrUnauthorized
			}
			return nil, fmt.Errorf("%w: fetch token from %s: HTTP %d", err, c.realm, resp.StatusCode)
		}

		var body struct {
			Token       string `json:"token"`
			AccessToken string `json:"access_token"`
			ExpiresIn   int    `json:"expires_in"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
			return nil, fmt.Errorf("decode token response from %s: %w", c.realm, err)
		}

		// Registries return the token under either key.
		tok := body.Token
		if tok == "" {
			tok = body.AccessToken
		}
		if tok == "" {
			return nil, fmt.Errorf("token response from %s contained no token", c.realm)
		}

		ttl := time.Duration(body.ExpiresIn) * time.Second
		if ttl <= 0 {
			// The spec's default when expires_in is absent.
			ttl = 60 * time.Second
		}
		t.cache.set(scope, tok, ttl)
		return tok, nil
	})
	if err != nil {
		return "", err
	}
	tok, ok := v.(string)
	if !ok {
		// Unreachable: every non-error return above is a string. Checked rather
		// than asserted because a panic inside a RoundTripper unwinds through
		// the whole transfer, and a returned error does not.
		return "", fmt.Errorf("internal: token for %s had unexpected type %T", c.realm, v)
	}
	return tok, nil
}

func withBearer(r *http.Request, token string) *http.Request {
	req := r.Clone(r.Context())
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

func basicAuth(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

// scopeFor derives the token scope from the request.
//
// Read operations need pull; anything else needs pull,push. Requesting the
// narrowest scope that works means a compromised token is less useful and some
// registries issue it faster.
func scopeFor(r *http.Request, repository string) string {
	if repository == "" {
		return ""
	}
	action := "pull"
	switch r.Method {
	case http.MethodGet, http.MethodHead:
	default:
		action = "pull,push"
	}
	return "repository:" + repository + ":" + action
}

// challenge is a parsed WWW-Authenticate header.
type challenge struct {
	scheme  string
	realm   string
	service string
	scope   string
}

// parseChallenge reads `Bearer realm="...",service="...",scope="..."`.
func parseChallenge(header string) challenge {
	var c challenge
	header = strings.TrimSpace(header)
	if header == "" {
		return c
	}

	scheme, rest, ok := strings.Cut(header, " ")
	c.scheme = strings.ToLower(strings.TrimSpace(scheme))
	if !ok {
		return c
	}

	for _, part := range splitParams(rest) {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"`)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "realm":
			c.realm = value
		case "service":
			c.service = value
		case "scope":
			c.scope = value
		}
	}
	return c
}

// splitParams splits on commas that are not inside quotes - a scope value
// legitimately contains commas ("repository:x:pull,push").
func splitParams(s string) []string {
	var out []string
	var cur strings.Builder
	inQuotes := false

	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			inQuotes = !inQuotes
			cur.WriteByte(c)
		case c == ',' && !inQuotes:
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, strings.TrimSpace(cur.String()))
	}
	return out
}

// tokenCache holds bearer tokens keyed by scope.
type tokenCache struct {
	mu     sync.RWMutex
	tokens map[string]cachedToken
}

type cachedToken struct {
	token   string
	expires time.Time
}

// refreshMargin retires a token slightly early, so a request never races an
// expiry that happens mid-flight.
const refreshMargin = 30 * time.Second

func newTokenCache() *tokenCache {
	return &tokenCache{tokens: map[string]cachedToken{}}
}

func (c *tokenCache) get(scope string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.tokens[scope]
	if !ok || time.Now().After(t.expires) {
		return "", false
	}
	return t.token, true
}

func (c *tokenCache) set(scope, token string, ttl time.Duration) {
	expires := time.Now().Add(ttl)
	if ttl > refreshMargin {
		expires = expires.Add(-refreshMargin)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens[scope] = cachedToken{token: token, expires: expires}
}
