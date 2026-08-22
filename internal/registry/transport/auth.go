package transport

import (
	"bytes"
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

// maxReauthAttempts bounds how many times one request re-authenticates after
// a 401, counting the first.
//
// # The bug this exists for
//
// A tag write to Artifactory was refused with 401 on a brand-new token —
// fetched moments earlier, for the same scope, over the same connection — and
// an unmodified retry of the identical request, seconds later, succeeded
// immediately. That is not a standing credential problem; a wrong password or
// a missing grant fails the same way every time, and this did not. The likely
// cause is replication lag between whichever registry node issued the token
// (or validated the Basic credential) and whichever node served the write, in
// a load-balanced Artifactory cluster — the kind of gap one immediate retry
// clears and a hundred would not, which is why this is bounded rather than
// handed to the outer retry policy's full backoff schedule.
const maxReauthAttempts = 3

// reauthBackoff is the pause between repeated 401s, short because the failure
// this exists for clears in well under a second.
const reauthBackoff = 250 * time.Millisecond

func (t *authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	scope := t.cfg.Scope
	if scope == "" {
		scope = scopeFor(r, t.cfg.Repository)
	}

	// Make a small body replayable so a 401 can be re-authenticated.
	//
	// A registry token this transport caches can expire mid-transfer — the
	// default TTL is 60 seconds when the registry omits expires_in, which
	// Artifactory does for its reference tokens — and the LAST write of a long
	// transfer, the tag, is the one most likely to land after that expiry with
	// no cached token to attach. The reauth path below can only retry a request
	// whose body it can replay, and ORAS does not set GetBody on a manifest
	// PUT. Without this, that final tag write met a 401 it could not recover
	// from and failed the whole transfer, intermittently, by duration.
	//
	// Capped so a gigabyte blob upload — large or unknown ContentLength — is
	// never buffered; a manifest is kilobytes and always fits.
	if err := ensureReplayable(r); err != nil {
		return nil, err
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

	for attempt := 1; attempt < maxReauthAttempts; attempt++ {
		challenge := parseChallenge(resp.Header.Get("WWW-Authenticate"))
		if challenge.scheme == "" {
			return resp, nil // 401 without a challenge: nothing to act on
		}

		// A request whose body cannot be replayed must not be retried.
		if r.Body != nil && r.Body != http.NoBody && r.GetBody == nil {
			return resp, nil
		}
		drain(resp)

		var req *http.Request
		if challenge.scheme == "basic" {
			if t.cfg.Username == "" {
				// Classified, so the retry layer above stops immediately rather
				// than spending the full backoff schedule on a problem no amount
				// of waiting can fix.
				return nil, fmt.Errorf("%w: registry %s requires basic auth but no credentials are configured",
					registry.ErrUnauthorized, t.cfg.Registry)
			}
			req, err = rewind(r, attempt)
			if err != nil {
				return nil, err
			}
			req.SetBasicAuth(t.cfg.Username, t.cfg.Password)
		} else {
			// A token that has already been refused once for this scope is
			// not handed out again: forcing a fresh exchange is what turns a
			// second attempt into a DIFFERENT attempt rather than the same
			// one repeated.
			if attempt > 1 {
				t.cache.delete(scope)
			}
			tok, tokErr := t.fetchToken(r, challenge, scope)
			if tokErr != nil {
				return nil, tokErr
			}
			req, err = rewind(r, attempt)
			if err != nil {
				return nil, err
			}
			req = withBearer(req, tok)
		}

		resp, err = t.next.RoundTrip(req)
		if err != nil || resp.StatusCode != http.StatusUnauthorized {
			return resp, err
		}
		r = req
		if attempt < maxReauthAttempts-1 {
			if sleepErr := sleep(r.Context(), reauthBackoff); sleepErr != nil {
				return nil, sleepErr
			}
		}
	}
	return resp, nil
}

// maxReplayBody caps what ensureReplayable will buffer: comfortably larger than
// any manifest, far smaller than a blob body worth holding in memory.
const maxReplayBody = 4 << 20 // 4 MiB

// ensureReplayable buffers a small request body so the request can be resent
// after a 401.
//
// A no-op unless the body is present, has no GetBody already, and declares a
// length within maxReplayBody. Blob uploads — gigabytes, or an unknown length —
// are left untouched: they must never be held in memory, and they reach the
// registry through the upload-session endpoints rather than as one PUT anyway.
func ensureReplayable(r *http.Request) error {
	if r.Body == nil || r.Body == http.NoBody || r.GetBody != nil {
		return nil
	}
	if r.ContentLength < 0 || r.ContentLength > maxReplayBody {
		return nil
	}

	buf, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		return fmt.Errorf("buffer request body for re-authentication: %w", err)
	}

	r.Body = io.NopCloser(bytes.NewReader(buf))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buf)), nil
	}
	r.ContentLength = int64(len(buf))
	return nil
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

// delete discards a scope's cached token, so the next fetch is a real
// exchange rather than a repeat of a token the registry has already refused.
func (c *tokenCache) delete(scope string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.tokens, scope)
}
