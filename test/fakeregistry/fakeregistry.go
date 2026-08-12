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
	"io"
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

	// uploads tracks in-progress blob upload sessions.
	uploads *uploads
	// declineMounts makes every cross-repository mount answer 202 with an
	// ordinary upload session, as a registry that does not support the shortcut
	// does. Both branches need exercising: 202 is a normal outcome and a client
	// that treats it as an error fails against half the registries in use.
	declineMounts bool
	// globalBlobIndex reproduces Artifactory: HEAD answers about the whole
	// registry, manifest push links per path. See WithArtifactoryQuirks.
	globalBlobIndex bool

	// Counters, for asserting on behaviour rather than just outcomes.
	TagListCalls  atomic.Int64
	ManifestCalls atomic.Int64
	TokenCalls    atomic.Int64
	// MountedBlobs and UploadedBlobs separate the fast path from the slow one,
	// so a test can assert that a promotion moved ZERO bytes rather than merely
	// that it succeeded.
	MountedBlobs  atomic.Int64
	UploadedBlobs atomic.Int64
	UploadedBytes atomic.Int64
	// ServedBlobs and ServedBytes are the READ side, and they are what a test
	// asserting "the vendor was not asked for this twice" measures. Uploads
	// alone cannot show that: a blob fetched twice and uploaded twice looks the
	// same as one fetched once, from the destination's counters.
	ServedBlobs atomic.Int64
	ServedBytes atomic.Int64
	// CancelledUploads counts sessions abandoned with DELETE. Calibration's
	// write probe is built entirely on this: it exists so a test can prove the
	// bytes were pushed AND that nothing was committed.
	CancelledUploads atomic.Int64
	pageSize         int
}

type repo struct {
	// manifests maps digest -> raw bytes.
	manifests map[string][]byte
	// mediaTypes maps digest -> content type.
	mediaTypes map[string]string
	// tags maps tag -> digest.
	tags map[string]string
	// blobs maps digest -> content. Populated by seeding and by uploads.
	blobs map[string][]byte
}

func newRepo() *repo {
	return &repo{
		manifests:  map[string][]byte{},
		mediaTypes: map[string]string{},
		tags:       map[string]string{},
		blobs:      map[string][]byte{},
	}
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
	// code and message are the registry's OWN error, as a real registry
	// reports it. Empty means the generic injected fault.
	code    string
	message string
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

// WithArtifactoryQuirks makes the registry answer about blobs the way
// Artifactory does.
//
// Two linked behaviours, and the pair is what makes the bug: HEAD on a blob
// answers from a checksum index spanning the whole registry rather than the
// image path asked about, and a manifest push referencing a blob that is not
// linked under ITS path is refused as MANIFEST_INVALID with the blob named only
// in prose.
//
// A transfer against this registry skips every upload, records placements for
// content that is not where it says, and then fails every manifest — which is
// exactly what happened to a 63.7 GiB bundle in production.
func WithArtifactoryQuirks() Option {
	return func(r *Registry) { r.globalBlobIndex = true }
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
	r.uploads = newUploads()
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
		rp = newRepo()
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

// Layer describes a blob referenced by a seeded manifest.
//
// Only the descriptor is modelled: discovery records blob identity and size
// from the manifest and never reads blob bytes, so seeding real blob content
// would add nothing a test could observe.
type Layer struct {
	Digest    string
	Size      int64
	MediaType string
	// content is what the registry serves for this digest.
	//
	// Unexported: a caller builds a Layer through NewLayer, which derives the
	// digest FROM the content, so the two can never disagree. A struct literal
	// with a hand-written digest is exactly the fixture that would make a
	// digest-verification test pass while verifying nothing.
	content string
}

// NewLayer builds a layer descriptor from content, so its digest is real.
func NewLayer(content string) Layer {
	sum := sha256.Sum256([]byte(content))
	return Layer{
		content:   content,
		Digest:    "sha256:" + hex.EncodeToString(sum[:]),
		Size:      int64(len(content)),
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
	}
}

// AddImage seeds a genuine OCI image manifest with a config and layers.
//
// Used where a test needs the artifact TREE rather than just a digest: the
// walk in internal/discovery parses config and layer descriptors out of these
// bytes.
func (r *Registry) AddImage(repoPath, tag string, layers ...Layer) string {
	// The config digest is derived from the layers as well as the name, so two
	// images differing only in their layers get different configs — as real
	// per-platform images do. Deriving it from the tag alone would make the
	// children of an index (seeded with no tag) share one config and quietly
	// understate the blob count.
	material := "config-" + repoPath + "-" + tag
	for _, l := range layers {
		material += "|" + l.Digest
	}
	config := NewLayer(material)

	// Blob CONTENT is stored, not just the descriptor. Discovery never reads a
	// blob so M2 did not need it; a transfer moves them, and a fake with
	// descriptors but no bytes would make every transfer test fail at the first
	// GET.
	r.putBlob(repoPath, config.Digest, []byte(material))
	for _, l := range layers {
		r.putBlob(repoPath, l.Digest, []byte(l.content))
	}

	body := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    config.Digest,
			"size":      config.Size,
		},
		"layers": layerDescriptors(layers),
	}

	raw, err := json.Marshal(body)
	if err != nil {
		panic("fakeregistry: marshal image manifest: " + err.Error())
	}
	return r.AddManifest(repoPath, tag, raw, "application/vnd.oci.image.manifest.v1+json")
}

// AddIndex seeds an OCI index pointing at already-seeded child manifests.
//
// platforms and childDigests are parallel slices; a child with an empty
// platform string gets no platform field.
func (r *Registry) AddIndex(repoPath, tag string, childDigests, platforms []string) string {
	return r.AddAnnotatedIndex(repoPath, tag, childDigests, platforms, nil)
}

// AddAnnotatedIndex adds an index carrying annotations.
//
// Real vendor bundles are annotated — a release date, a product name, a
// per-child type — and a fake registry that never sets any would let a whole
// class of parsing bug through untested.
func (r *Registry) AddAnnotatedIndex(
	repoPath, tag string, childDigests, platforms []string, annotations map[string]string,
) string {
	manifests := make([]map[string]any, 0, len(childDigests))

	r.mu.Lock()
	rp := r.repos[repoPath]
	for i, d := range childDigests {
		size := 0
		if rp != nil {
			size = len(rp.manifests[d])
		}
		entry := map[string]any{
			"mediaType": "application/vnd.oci.image.manifest.v1+json",
			"digest":    d,
			"size":      size,
		}
		if i < len(platforms) && platforms[i] != "" {
			os, arch, _ := strings.Cut(platforms[i], "/")
			entry["platform"] = map[string]any{"os": os, "architecture": arch}
		}
		manifests = append(manifests, entry)
	}
	r.mu.Unlock()

	index := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests":     manifests,
	}
	if len(annotations) > 0 {
		index["annotations"] = annotations
	}
	raw, err := json.Marshal(index)
	if err != nil {
		panic("fakeregistry: marshal index: " + err.Error())
	}
	return r.AddManifest(repoPath, tag, raw, "application/vnd.oci.image.index.v1+json")
}

func layerDescriptors(layers []Layer) []map[string]any {
	out := make([]map[string]any, 0, len(layers))
	for _, l := range layers {
		mt := l.MediaType
		if mt == "" {
			mt = "application/vnd.oci.image.layer.v1.tar+gzip"
		}
		out = append(out, map[string]any{
			"mediaType": mt,
			"digest":    l.Digest,
			"size":      l.Size,
		})
	}
	return out
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

// RejectNext makes the next n matching requests fail with a specific OCI error
// CODE, not merely a status.
//
// The code is the part that decides what a failure means: a registry answering
// 400 with MANIFEST_BLOB_UNKNOWN is telling us something repairable, and one
// answering 400 with MANIFEST_INVALID is telling us to stop. A fake that could
// only produce a status could not tell those apart, and neither could a test.
func (r *Registry) RejectNext(pathContains string, status int, code, message string, n int) {
	r.faults.mu.Lock()
	defer r.faults.mu.Unlock()
	r.faults.failNext[pathContains] = &fault{
		status: status, remaining: n, code: code, message: message,
	}
}

// TruncateNext makes the next response for a matching path claim a longer
// Content-Length than it sends, simulating a mid-response disconnect.
func (r *Registry) TruncateNext(pathContains string, n int) {
	r.faults.mu.Lock()
	defer r.faults.mu.Unlock()
	r.faults.failNext[pathContains] = &fault{remaining: n, truncate: true}
}

// ClearFaults removes every pending fault.
//
// The "outage is over" half of a recovery test. Without it a test that injects
// a generous fault count to survive the retry schedule leaves the remainder
// queued, and the recovery it means to assert never happens — the failure looks
// like a bug in discovery rather than in the test.
func (r *Registry) ClearFaults() {
	r.faults.mu.Lock()
	defer r.faults.mu.Unlock()
	r.faults.failNext = map[string]*fault{}
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
		code, message := f.code, f.message
		if code == "" {
			code, message = "INJECTED", "injected fault"
		}
		writeError(w, f.status, code, message)
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

	case strings.Contains(path, "/blobs/uploads/"):
		r.handleUpload(w, req)

	case strings.Contains(path, "/blobs/"):
		r.handleBlob(w, req)

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

	if req.Method == http.MethodPut {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
			return
		}
		mediaType := req.Header.Get("Content-Type")
		if mediaType == "" {
			mediaType = "application/vnd.oci.image.manifest.v1+json"
		}
		r.putManifest(w, repoPath, ref, raw, mediaType)
		return
	}

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
