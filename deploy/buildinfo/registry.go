package buildinfo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Registry reads manifests over the OCI distribution API.
//
// Written here rather than reached for from internal/: this is a deployment
// helper and must not depend on the product's packages, the same way chartstage
// and secretsinv do not. It needs one verb.
type Registry struct {
	Username string
	Token    string
	HTTP     *http.Client
	// Scheme is http only for a test server.
	Scheme string
}

// Manifest resolves one reference to its manifest digest and blobs.
func (r *Registry) Manifest(ctx context.Context, ref string) (Manifest, error) {
	host, path, tag, err := splitRef(ref)
	if err != nil {
		return Manifest{}, err
	}
	scheme := r.Scheme
	if scheme == "" {
		scheme = "https"
	}
	url := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", scheme, host, path, tag)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Manifest{}, err
	}
	// Both media types, and the index types: a multi-platform tag answers with
	// an index, and its digest is still the thing a deployment pulls.
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
	}, ","))
	if r.Token != "" {
		req.SetBasicAuth(r.Username, r.Token)
	}

	client := r.HTTP
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return Manifest{}, err
	}
	defer res.Body.Close() //nolint:errcheck // response body close

	if res.StatusCode == http.StatusNotFound {
		return Manifest{}, fmt.Errorf("not published")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return Manifest{}, fmt.Errorf("%s: %s", res.Status, strings.TrimSpace(string(detail)))
	}

	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return Manifest{}, err
	}
	var doc struct {
		Config struct {
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}

	// The registry's own digest header when it sends one - it is authoritative,
	// and cheaper than hashing the body to find out.
	m := Manifest{Digest: res.Header.Get("Docker-Content-Digest")}
	if m.Digest == "" {
		return Manifest{}, fmt.Errorf("registry returned no Docker-Content-Digest for %s", ref)
	}
	m.Config = Blob{Digest: doc.Config.Digest, Size: doc.Config.Size}
	for _, l := range doc.Layers {
		m.Layers = append(m.Layers, Blob{Digest: l.Digest, Size: l.Size})
	}
	return m, nil
}

// splitRef separates `host/path/name:tag`.
func splitRef(ref string) (host, path, tag string, err error) {
	at := strings.LastIndex(ref, ":")
	slash := strings.LastIndex(ref, "/")
	if at < 0 || at < slash {
		return "", "", "", fmt.Errorf("%q has no tag", ref)
	}
	tag = ref[at+1:]
	rest := ref[:at]
	slash = strings.Index(rest, "/")
	if slash < 0 {
		return "", "", "", fmt.Errorf("%q names no repository", ref)
	}
	return rest[:slash], rest[slash+1:], tag, nil
}
