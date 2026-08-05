// Package fakeregistry is an in-process OCI Distribution v2 server for tests.
//
// It exists so `make test` needs no Docker. That is a rule worth protecting:
// a suite that requires containers is a suite developers run less often, and
// the difference compounds over a project's life.
//
// It implements the subset M2 exercises — /v2/ probe, token auth, tag listing
// with Link pagination, and manifest HEAD/GET — plus injectable faults, so the
// retry and backoff paths can be tested deterministically rather than by
// hoping a real registry misbehaves.
package fakeregistry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry is a fake Distribution v2 registry.
type Registry struct {
	server *httptest.Server

	mu sync.RWMutex
	// repos maps a repository path to its tags and manifests.
	repos map[string]*repo

	// Auth. When username is set, /v2/ challenges and a token must be
	// obtained before any other endpoint answers.
	username string
	password string
	// tokenAuth selects the bearer token flow rather than plain basic auth.
	tokenAuth bool

	// Fault injection.
	faults faultConfig

	// Counters, for asserting on behaviour rather than just outcomes.
	TagListCalls  atomic.Int64
	ManifestCalls atomic.Int64
	TokenCalls    atomic.Int64
	pageSize      int
}

type repo struct {
	// manifests maps digest -> raw bytes.
	manifests map[string][]byte
	// mediaTypes maps digest -> content type.
	mediaTypes map[string]string
	// tags maps tag -> digest.
	tags map[string]string
}

type faultConfig struct {
	mu sync.Mutex
	// failNext[path-prefix] = remaining failures to emit.
	failNext map[string]*fault
}

type fault struct {
	status     int
	remaining  int
	retryAfter string
	// truncate returns a body shorter than Content-Length claims, to exercise
	// the mid-response disconnect path.
	truncate bool
}

// Option configures a Registry.
type Option func(*Registry)

// WithBasicAuth requires basic credentials on every request.
func WithBasicAuth(user, pass string) Option {
	return func(r *Registry) { r.username, r.password, r.tokenAuth = user, pass, false }
}

// WithTokenAuth requires the full bearer token flow: a 401 challenge, a token
// fetch against the realm, then a bearer-authenticated retry.
func WithTokenAuth(user, pass string) Option {
	return func(r *Registry) { r.username, r.password, r.tokenAuth = user, pass, true }
}

// WithPageSize forces tag pagination at n per page, so Link-header handling is
// exercised without needing hundreds of tags.
func WithPageSize(n int) Option {
	return func(r *Registry) { r.pageSize = n }
}

// New starts a fake registry. It is closed automatically via t.Cleanup by the
// caller, or explicitly with Close.
func New(opts ...Option) *Registry {
	r := &Registry{
		repos:    map[string]*repo{},
		faults:   faultConfig{failNext: map[string]*fault{}},
		pageSize: 0,
	}
	for _, o := range opts {
		o(r)
	}
	r.server = httptest.NewServer(http.HandlerFunc(r.handle))
	return r
}

// Close shuts the server down.
func (r *Registry) Close() { r.server.Close() }

// Host returns the host:port, suitable for a repository's Registry field.
func (r *Registry) Host() string {
	u, _ := url.Parse(r.server.URL)
	return u.Host
}

// URL returns the full base URL.
func (r *Registry) URL() string { return r.server.URL }

// ---------------------------------------------------------------------------
// Seeding
// ---------------------------------------------------------------------------

// AddManifest stores a manifest and points a tag at it, returning the digest.
//
// Content is arbitrary bytes: these tests care about addressing and
// pagination, not about manifest schema.
func (r *Registry) AddManifest(repoPath, tag string, content []byte, mediaType string) string {
	sum := sha256.Sum256(content)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	r.mu.Lock()
	defer r.mu.Unlock()

	rp, ok := r.repos[repoPath]
	if !ok {
		rp = &repo{
			manifests:  map[string][]byte{},
			mediaTypes: map[string]string{},
			tags:       map[string]string{},
		}
		r.repos[repoPath] = rp
	}

	rp.manifests[digest] = content
	rp.mediaTypes[digest] = mediaType
	if tag != "" {
		rp.tags[tag] = digest
	}
	return digest
}

// AddTag seeds a tag with generated content, returning its digest.
func (r *Registry) AddTag(repoPath, tag string) string {
	return r.AddManifest(repoPath, tag,
		[]byte(fmt.Sprintf(`{"schemaVersion":2,"tag":%q}`, tag)),
		"application/vnd.oci.image.manifest.v1+json")
}

// RepushTag replaces a tag's content, producing a new digest for the same tag.
//
// This is the supersession case: same tag, different digest.
func (r *Registry) RepushTag(repoPath, tag, newContent string) string {
	return r.AddManifest(repoPath, tag, []byte(newContent),
		"application/vnd.oci.image.manifest.v1+json")
}

// RemoveTag deletes a tag.
func (r *Registry) RemoveTag(repoPath, tag string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rp, ok := r.repos[repoPath]; ok {
		delete(rp.tags, tag)
	}
}

// ---------------------------------------------------------------------------
// Fault injection
// ---------------------------------------------------------------------------

// FailNext makes the next n requests whose path contains `pathContains` fail
// with the given status.
func (r *Registry) FailNext(pathContains string, status, n int) {
	r.faults.mu.Lock()
	defer r.faults.mu.Unlock()
	r.faults.failNext[pathContains] = &fault{status: status, remaining: n}
}

// FailNextWithRetryAfter is FailNext plus a Retry-After header, to exercise
// server-directed backoff.
func (r *Registry) FailNextWithRetryAfter(pathContains string, status, n int, retryAfter string) {
	r.faults.mu.Lock()
	defer r.faults.mu.Unlock()
	r.faults.failNext[pathContains] = &fault{status: status, remaining: n, retryAfter: retryAfter}
}

// TruncateNext makes the next response for a matching path claim a longer
// Content-Length than it sends, simulating a mid-response disconnect.
func (r *Registry) TruncateNext(pathContains string, n int) {
	r.faults.mu.Lock()
	defer r.faults.mu.Unlock()
	r.faults.failNext[pathContains] = &fault{remaining: n, truncate: true}
}

// takeFault consumes a pending fault for a path, if any.
func (r *Registry) takeFault(path string) *fault {
	r.faults.mu.Lock()
	defer r.faults.mu.Unlock()

	for prefix, f := range r.faults.failNext {
		if f.remaining > 0 && strings.Contains(path, prefix) {
			f.remaining--
			out := *f
			if f.remaining == 0 {
				delete(r.faults.failNext, prefix)
			}
			return &out
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

func (r *Registry) handle(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Path

	// The token endpoint is never itself gated on a token.
	if path == "/token" {
		r.handleToken(w, req)
		return
	}

	if f := r.takeFault(path); f != nil {
		if f.truncate {
			w.Header().Set("Content-Length", "10000")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("short"))
			return
		}
		if f.retryAfter != "" {
			w.Header().Set("Retry-After", f.retryAfter)
		}
		writeError(w, f.status, "INJECTED", "injected fault")
		return
	}

	if !r.authorized(req) {
		r.challenge(w)
		return
	}

	switch {
	case path == "/v2/" || path == "/v2":
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))

	case path == "/v2/_catalog":
		r.handleCatalog(w)

	case strings.HasSuffix(path, "/tags/list"):
		r.handleTagList(w, req)

	case strings.Contains(path, "/manifests/"):
		r.handleManifest(w, req)

	case strings.Contains(path, "/referrers/"):
		// Not implemented: exercises the capability probe's negative path and
		// the cosign tag-schema fallback in M5.
		writeError(w, http.StatusNotFound, "UNSUPPORTED", "referrers API not implemented")

	default:
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no such endpoint")
	}
}

func (r *Registry) authorized(req *http.Request) bool {
	if r.username == "" {
		return true
	}

	auth := req.Header.Get("Authorization")
	if r.tokenAuth {
		return strings.HasPrefix(auth, "Bearer ") && strings.TrimPrefix(auth, "Bearer ") == r.expectedToken()
	}

	user, pass, ok := req.BasicAuth()
	return ok && user == r.username && pass == r.password
}

func (r *Registry) expectedToken() string {
	sum := sha256.Sum256([]byte(r.username + ":" + r.password))
	return hex.EncodeToString(sum[:16])
}

func (r *Registry) challenge(w http.ResponseWriter) {
	if r.tokenAuth {
		w.Header().Set("WWW-Authenticate",
			fmt.Sprintf(`Bearer realm=%q,service="fake"`, r.server.URL+"/token"))
	} else {
		w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
	}
	writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
}

func (r *Registry) handleToken(w http.ResponseWriter, req *http.Request) {
	r.TokenCalls.Add(1)

	user, pass, ok := req.BasicAuth()
	if !ok || user != r.username || pass != r.password {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "bad credentials")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token":      r.expectedToken(),
		"expires_in": 300,
	})
}

func (r *Registry) handleCatalog(w http.ResponseWriter) {
	r.mu.RLock()
	names := make([]string, 0, len(r.repos))
	for name := range r.repos {
		names = append(names, name)
	}
	r.mu.RUnlock()
	sort.Strings(names)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"repositories": names})
}

func (r *Registry) handleTagList(w http.ResponseWriter, req *http.Request) {
	r.TagListCalls.Add(1)

	repoPath := strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/v2/"), "/tags/list")

	r.mu.RLock()
	rp, ok := r.repos[repoPath]
	var tags []string
	if ok {
		for t := range rp.tags {
			tags = append(tags, t)
		}
	}
	r.mu.RUnlock()

	if !ok {
		writeError(w, http.StatusNotFound, "NAME_UNKNOWN", "repository not found")
		return
	}
	sort.Strings(tags)

	// Pagination. `last` resumes after a tag; `n` bounds the page.
	if last := req.URL.Query().Get("last"); last != "" {
		idx := sort.SearchStrings(tags, last)
		if idx < len(tags) && tags[idx] == last {
			idx++
		}
		tags = tags[idx:]
	}

	limit := r.pageSize
	if q := req.URL.Query().Get("n"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 && (limit == 0 || n < limit) {
			limit = n
		}
	}

	more := false
	if limit > 0 && len(tags) > limit {
		tags = tags[:limit]
		more = true
	}

	if more && len(tags) > 0 {
		next := fmt.Sprintf("/v2/%s/tags/list?last=%s&n=%d",
			repoPath, url.QueryEscape(tags[len(tags)-1]), limit)
		w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, next))
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"name": repoPath, "tags": tags})
}

func (r *Registry) handleManifest(w http.ResponseWriter, req *http.Request) {
	r.ManifestCalls.Add(1)

	idx := strings.Index(req.URL.Path, "/manifests/")
	repoPath := strings.TrimPrefix(req.URL.Path[:idx], "/v2/")
	ref := req.URL.Path[idx+len("/manifests/"):]

	r.mu.RLock()
	rp, ok := r.repos[repoPath]
	var content []byte
	var digest, mediaType string
	if ok {
		digest = ref
		if d, isTag := rp.tags[ref]; isTag {
			digest = d
		}
		content, ok = rp.manifests[digest]
		mediaType = rp.mediaTypes[digest]
	}
	r.mu.RUnlock()

	if !ok || content == nil {
		writeError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
		return
	}

	if mediaType == "" {
		mediaType = "application/vnd.oci.image.manifest.v1+json"
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))

	if req.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]string{{"code": code, "message": message}},
	})
}
