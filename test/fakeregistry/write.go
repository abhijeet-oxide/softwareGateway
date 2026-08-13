package fakeregistry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// The write half of the fake registry.
//
// This is what keeps the suite Docker-free, and that rule is worth protecting
// rather than treating as a preference: a suite needing containers is a suite
// developers run less often, and the difference compounds. There is also no
// Docker available in the environment this is developed in, so without these
// handlers the transfer engine would have no test story at all.
//
// It implements the parts a transfer actually exercises, INCLUDING the ones
// that are uneven in the wild, because those are where the interesting bugs
// are:
//
//   - an upload session whose Location is relative (registries vary; a client
//     that only handles absolute URLs breaks against half of them);
//   - a cross-repository mount answering BOTH 201 Created and — when told to
//     decline — 202 Accepted with a real upload session, which is a normal
//     outcome and not an error;
//   - manifest push rejecting BLOB_UNKNOWN when a referenced blob is absent,
//     which is the backstop that catches a stale placement record;
//   - digest rejection on commit, so a client that streams the wrong bytes is
//     caught here rather than in production.

// upload is an in-progress blob upload session.
type upload struct {
	repo string
	buf  []byte
}

// uploads tracks sessions by ID.
type uploads struct {
	mu sync.Mutex
	m  map[string]*upload
	n  int
}

func newUploads() *uploads { return &uploads{m: map[string]*upload{}} }

func (u *uploads) create(repo string) (string, *upload) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.n++
	id := fmt.Sprintf("upload-%d", u.n)
	s := &upload{repo: repo}
	u.m[id] = s
	return id, s
}

func (u *uploads) get(id string) (*upload, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	s, ok := u.m[id]
	return s, ok
}

func (u *uploads) remove(id string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.m, id)
}

// handleUpload serves POST/PATCH/PUT under /v2/<repo>/blobs/uploads/.
func (r *Registry) handleUpload(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Path
	idx := strings.Index(path, "/blobs/uploads/")
	repoPath := strings.TrimPrefix(path[:idx], "/v2/")
	sessionID := strings.Trim(path[idx+len("/blobs/uploads/"):], "/")

	switch req.Method {
	case http.MethodPost:
		r.startUpload(w, req, repoPath)
	case http.MethodPatch:
		r.appendChunk(w, req, repoPath, sessionID)
	case http.MethodPut:
		r.commitUpload(w, req, repoPath, sessionID)
	case http.MethodDelete:
		r.cancelUpload(w, sessionID)
	default:
		writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed on uploads")
	}
}

// cancelUpload discards a session without committing it.
//
// The spec's way of abandoning an upload, and the mechanism calibration relies
// on to push real bytes at a real registry and leave nothing behind. Modelled
// here so that the property can be ASSERTED — a test that ends with the
// registry holding no blobs is the only proof that the probe is non-destructive.
func (r *Registry) cancelUpload(w http.ResponseWriter, sessionID string) {
	if _, ok := r.uploads.get(sessionID); !ok {
		writeError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "no such upload")
		return
	}
	r.uploads.remove(sessionID)
	r.CancelledUploads.Add(1)
	w.WriteHeader(http.StatusNoContent)
}

// startUpload begins a session, or performs a cross-repository mount.
func (r *Registry) startUpload(w http.ResponseWriter, req *http.Request, repoPath string) {
	q := req.URL.Query()

	if dgst := q.Get("mount"); dgst != "" {
		r.handleMount(w, repoPath, dgst, q.Get("from"))
		return
	}

	id, _ := r.uploads.create(repoPath)

	// A RELATIVE Location, deliberately.
	//
	// The specification permits either, real registries send both, and a client
	// that assumes absolute silently breaks against the ones that do not. The
	// fake sends the harder form so that bug cannot survive here.
	w.Header().Set("Location", "/v2/"+repoPath+"/blobs/uploads/"+id)
	w.Header().Set("Docker-Upload-UUID", id)
	w.Header().Set("Range", "0-0")
	w.WriteHeader(http.StatusAccepted)
}

// handleMount performs a cross-repository mount, or declines it.
func (r *Registry) handleMount(w http.ResponseWriter, repoPath, dgst, fromRepo string) {
	// A registry that declines answers 202 with an ordinary upload session. It
	// is a NORMAL outcome — the client falls through to streaming — and treating
	// it as an error would turn a supported-everywhere operation into a failure
	// on every registry that happens not to implement the shortcut.
	if r.declineMounts {
		id, _ := r.uploads.create(repoPath)
		w.Header().Set("Location", "/v2/"+repoPath+"/blobs/uploads/"+id)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	r.mu.Lock()
	src, ok := r.repos[fromRepo]
	var content []byte
	if ok {
		content, ok = src.blobs[dgst]
	}
	r.mu.Unlock()

	if !ok {
		// Not in the source repository, so there is nothing to relocate. 202
		// again: the client must upload it normally.
		id, _ := r.uploads.create(repoPath)
		w.Header().Set("Location", "/v2/"+repoPath+"/blobs/uploads/"+id)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	r.putBlob(repoPath, dgst, content)
	r.countMount(repoPath, dgst)

	w.Header().Set("Location", "/v2/"+repoPath+"/blobs/"+dgst)
	w.Header().Set("Docker-Content-Digest", dgst)
	w.WriteHeader(http.StatusCreated)
}

// appendChunk accepts a PATCH into an open session.
func (r *Registry) appendChunk(w http.ResponseWriter, req *http.Request, repoPath, id string) {
	s, ok := r.uploads.get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "no such upload session")
		return
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
		return
	}
	s.buf = append(s.buf, body...)

	w.Header().Set("Location", "/v2/"+repoPath+"/blobs/uploads/"+id)
	w.Header().Set("Range", "0-"+strconv.Itoa(len(s.buf)-1))
	w.WriteHeader(http.StatusAccepted)
}

// commitUpload finalises a session, verifying the digest.
func (r *Registry) commitUpload(w http.ResponseWriter, req *http.Request, repoPath, id string) {
	s, ok := r.uploads.get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "no such upload session")
		return
	}
	defer r.uploads.remove(id)

	// A monolithic upload sends everything on the PUT; a chunked one has
	// already sent it by PATCH. Both are valid and both end here.
	body, err := io.ReadAll(req.Body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
		return
	}
	content := append(s.buf, body...)

	want := req.URL.Query().Get("digest")
	sum := sha256.Sum256(content)
	got := "sha256:" + hex.EncodeToString(sum[:])

	// THE REGISTRY'S OWN CHECK, and half of the end-to-end integrity story: our
	// verifier catches a source serving wrong bytes, and this catches
	// corruption anywhere in our path including in transit.
	if want != "" && want != got {
		writeError(w, http.StatusBadRequest, "DIGEST_INVALID",
			fmt.Sprintf("uploaded content is %s, not the declared %s", got, want))
		return
	}

	r.putBlob(repoPath, got, content)
	r.countUpload(repoPath, got, int64(len(content)))

	w.Header().Set("Location", "/v2/"+repoPath+"/blobs/"+got)
	w.Header().Set("Docker-Content-Digest", got)
	w.WriteHeader(http.StatusCreated)
}

// handleBlob serves GET and HEAD on /v2/<repo>/blobs/<digest>.
func (r *Registry) handleBlob(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Path
	idx := strings.Index(path, "/blobs/")
	repoPath := strings.TrimPrefix(path[:idx], "/v2/")
	dgst := path[idx+len("/blobs/"):]

	r.mu.Lock()
	var content []byte
	ok := false
	if rp, exists := r.repos[repoPath]; exists {
		content, ok = rp.blobs[dgst]
	}
	if !ok && r.globalBlobIndex && req.Method == http.MethodHead {
		// Artifactory answers HEAD from a checksum index spanning the whole
		// Artifactory repository, not the image path the request named. So a
		// blob present under any path reads as present under all of them —
		// while the manifest push, which links per path, still refuses.
		//
		// Reproduced rather than described, because the bug it caused was
		// invisible in every test that asked only well-behaved questions: every
		// blob job reported success and every manifest failed.
		for _, rp := range r.repos {
			if c, found := rp.blobs[dgst]; found {
				content, ok = c, true
				break
			}
		}
	}
	r.mu.Unlock()

	if !ok {
		writeError(w, http.StatusNotFound, "BLOB_UNKNOWN", "no such blob")
		return
	}

	w.Header().Set("Docker-Content-Digest", dgst)
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	w.Header().Set("Content-Type", "application/octet-stream")

	if req.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)

	// Counted on GET only. A HEAD is the deduplication check, which is meant to
	// happen and costs nothing; a GET is bytes leaving this registry.
	r.ServedBlobs.Add(1)
	r.ServedBytes.Add(int64(len(content)))
}

// putBlob stores a blob, creating the repository if needed.
func (r *Registry) putBlob(repoPath, dgst string, content []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	rp, ok := r.repos[repoPath]
	if !ok {
		rp = newRepo()
		r.repos[repoPath] = rp
	}
	rp.blobs[dgst] = content
}

// putManifest stores a manifest, rejecting one whose referenced blobs are
// absent.
//
// The BLOB_UNKNOWN rejection is the backstop that makes the optimistic
// placement cache safe: when our record of "this blob is already there" is
// wrong — a registry garbage collector removed it underneath us — the registry
// itself tells us, and the transfer invalidates the placement and requeues the
// blobs. Without this the fake would accept a manifest pointing at nothing and
// the whole recovery path would go untested.
func (r *Registry) putManifest(
	w http.ResponseWriter, repoPath, ref string, raw []byte, mediaType string,
) {
	if ref != "" && !strings.HasPrefix(ref, "sha256:") {
		r.countTagWrite(repoPath, ref)
		if r.denyTagOverwrite && r.tagExists(repoPath, ref) {
			// Verbatim in shape from Artifactory: an overwrite the credential
			// may not perform is reported as though nobody were logged in.
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED",
				"authentication required")
			return
		}
	}

	if missing := r.missingReferences(repoPath, raw); missing != "" {
		if r.globalBlobIndex {
			// The same registry's spelling of the same fact: a missing blob
			// reported as MANIFEST_INVALID with the blob named only in a
			// free-text description. Verbatim from Artifactory.
			writeError(w, http.StatusBadRequest, "MANIFEST_INVALID",
				"map[description:Failed to copy blob "+missing+" to "+repoPath+"/"+ref+"]")
			return
		}
		writeError(w, http.StatusBadRequest, "BLOB_UNKNOWN",
			"manifest references "+missing+", which is not present in this repository")
		return
	}

	sum := sha256.Sum256(raw)
	dgst := "sha256:" + hex.EncodeToString(sum[:])

	r.mu.Lock()
	rp, ok := r.repos[repoPath]
	if !ok {
		rp = newRepo()
		r.repos[repoPath] = rp
	}
	rp.manifests[dgst] = raw
	rp.mediaTypes[dgst] = mediaType
	// A push by digest sets no tag: that is how a transfer stages content
	// without making it visible, and it is what invariant I1 rests on.
	if ref != "" && !strings.HasPrefix(ref, "sha256:") {
		rp.tags[ref] = dgst
	}
	r.mu.Unlock()

	w.Header().Set("Docker-Content-Digest", dgst)
	w.Header().Set("Location", "/v2/"+repoPath+"/manifests/"+dgst)
	w.WriteHeader(http.StatusCreated)
}

// WithDeclinedMounts makes every cross-repository mount answer 202 with an
// ordinary upload session, as a registry without the shortcut does.
//
// The 202 branch needs a test as much as the 201 one: it is a NORMAL outcome
// under the specification, support is uneven in the wild, and a client that
// treats it as an error fails against every registry that answers this way.
func WithDeclinedMounts() Option {
	return func(r *Registry) { r.declineMounts = true }
}

// WithTagOverwriteDenied refuses any PUT that would move a tag that already
// exists, answering 401 as Artifactory does when the credential may deploy but
// not overwrite.
//
// This is a real deployment, not a hypothetical: a permission target granting
// Deploy without Delete lets a transfer create every tag it has never created
// and refuses every tag it has, so the failure appears only on the SECOND
// transfer of a release — the run that should have been the cheapest.
func WithTagOverwriteDenied() Option {
	return func(r *Registry) { r.denyTagOverwrite = true }
}

func (r *Registry) tagExists(repoPath, tag string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	rp, ok := r.repos[repoPath]
	if !ok {
		return false
	}
	_, ok = rp.tags[tag]
	return ok
}

func (r *Registry) countTagWrite(repoPath, tag string) {
	r.ledgerMu.Lock()
	defer r.ledgerMu.Unlock()
	if r.tagLedger == nil {
		r.tagLedger = map[string]int{}
	}
	r.tagLedger[repoPath+"|"+tag]++
}

func (r *Registry) countMount(repoPath, digest string) {
	r.MountedBlobs.Add(1)

	r.ledgerMu.Lock()
	defer r.ledgerMu.Unlock()
	if r.mountLedger == nil {
		r.mountLedger = map[string]int{}
	}
	r.mountLedger[repoPath+"|"+digest]++
}

func (r *Registry) countUpload(repoPath, digest string, n int64) {
	r.UploadedBlobs.Add(1)
	r.UploadedBytes.Add(n)

	r.ledgerMu.Lock()
	defer r.ledgerMu.Unlock()
	if r.uploadLedger == nil {
		r.uploadLedger = map[string]int{}
	}
	r.uploadLedger[repoPath+"|"+digest]++
}

// Manifest returns a stored manifest's raw bytes, for assertions that need to
// read what was actually published rather than trust the fixture's own
// variables.
func (r *Registry) Manifest(repoPath, dgst string) []byte {
	r.mu.RLock()
	defer r.mu.RUnlock()

	rp, ok := r.repos[repoPath]
	if !ok {
		return nil
	}
	return rp.manifests[dgst]
}

// RemoveBlob deletes a blob from one repository.
//
// What a garbage collector does, and the event the whole optimistic placement
// cache rests on being able to survive. A test cannot exercise the recovery
// without being able to cause the loss.
func (r *Registry) RemoveBlob(repoPath, dgst string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if rp, ok := r.repos[repoPath]; ok {
		delete(rp.blobs, dgst)
	}
}

// BlobExists reports whether a blob is present, for assertions.
func (r *Registry) BlobExists(repoPath, dgst string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rp, ok := r.repos[repoPath]
	if !ok {
		return false
	}
	_, ok = rp.blobs[dgst]
	return ok
}

// ManifestBytes returns a stored manifest verbatim, for assertions.
//
// Verbatim matters: the test that proves a transfer does not mutate content
// compares these bytes with the source's, and a helper that re-serialized would
// make that test pass while the property failed.
func (r *Registry) ManifestBytes(repoPath, ref string) ([]byte, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	rp, ok := r.repos[repoPath]
	if !ok {
		return nil, false
	}
	dgst := ref
	if !strings.HasPrefix(ref, "sha256:") {
		dgst, ok = rp.tags[ref]
		if !ok {
			return nil, false
		}
	}
	raw, ok := rp.manifests[dgst]
	return raw, ok
}

// TagDigest returns what a tag resolves to, or "" when it does not exist.
func (r *Registry) TagDigest(repoPath, tag string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rp, ok := r.repos[repoPath]
	if !ok {
		return ""
	}
	return rp.tags[tag]
}

// missingReferences returns the first referenced digest absent from this
// repository, or "" when everything is present.
func (r *Registry) missingReferences(repoPath string, raw []byte) string {
	var body struct {
		Config    *struct{ Digest string }  `json:"config"`
		Layers    []struct{ Digest string } `json:"layers"`
		Manifests []struct{ Digest string } `json:"manifests"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		// Unparseable content is not this check's business: a registry stores
		// what it is given and validates references, not schema.
		return ""
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	rp, ok := r.repos[repoPath]
	if !ok {
		rp = newRepo()
	}

	if body.Config != nil && body.Config.Digest != "" {
		if _, ok := rp.blobs[body.Config.Digest]; !ok {
			return body.Config.Digest
		}
	}
	for _, l := range body.Layers {
		if l.Digest == "" {
			continue
		}
		if _, ok := rp.blobs[l.Digest]; !ok {
			return l.Digest
		}
	}
	// A child MANIFEST must be present as a manifest, which is what makes the
	// wave ordering testable: pushing an index before its children is exactly
	// what invariant I1 forbids.
	for _, m := range body.Manifests {
		if m.Digest == "" {
			continue
		}
		if _, ok := rp.manifests[m.Digest]; !ok {
			return m.Digest
		}
	}
	return ""
}

// AnnotatedLayer is one file inside a generic artifact.
//
// The PATH lives in an annotation rather than anywhere structural, which is
// how OCI carries a directory layout: `org.opencontainers.image.title` is the
// reserved key, and a vendor's `CONFIGURATION/example_parameters.json` is just
// its value. Nothing about it is special to a registry — which is precisely
// why a transfer preserves it by copying manifest bytes verbatim rather than
// by understanding them.
type AnnotatedLayer struct {
	// Title is org.opencontainers.image.title — the path within the artifact.
	Title     string
	Content   string
	MediaType string
}

// AddArtifact seeds a non-image OCI artifact: a manifest whose layers are
// arbitrary files with arbitrary media types.
//
// This is what NEAR's `generic_system` and `generic_custo` are, and what any
// number of future artifact types will be. The fake needs it because a
// transfer of one must be provable without hard-coding the vendor's shapes
// into the test — the whole claim being that the tool does not know what an
// ORB contains.
func (r *Registry) AddArtifact(
	repoPath, tag, artifactType string, layers ...AnnotatedLayer,
) string {
	config := NewLayer("config-" + repoPath + "-" + tag + "-" + artifactType)
	r.putBlob(repoPath, config.Digest, []byte(config.content))

	descriptors := make([]map[string]any, 0, len(layers))
	for _, l := range layers {
		blob := NewLayer(l.Content)
		r.putBlob(repoPath, blob.Digest, []byte(l.Content))

		mediaType := l.MediaType
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		d := map[string]any{
			"mediaType": mediaType,
			"digest":    blob.Digest,
			"size":      blob.Size,
		}
		if l.Title != "" {
			d["annotations"] = map[string]string{"org.opencontainers.image.title": l.Title}
		}
		descriptors = append(descriptors, d)
	}

	body := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{
			"mediaType": "application/vnd.oci.empty.v1+json",
			"digest":    config.Digest,
			"size":      config.Size,
		},
		"layers": descriptors,
	}
	if artifactType != "" {
		body["artifactType"] = artifactType
	}

	raw, err := json.Marshal(body)
	if err != nil {
		panic("fakeregistry: marshal artifact manifest: " + err.Error())
	}
	return r.AddManifest(repoPath, tag, raw, "application/vnd.oci.image.manifest.v1+json")
}
