package artifactory

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/security"
)

// Xray answered "one or more parameters are missing" for every SBOM, and this
// is the request that made it stop.
//
// Two things it insists on, both of which the old body got wrong:
//
//   - a format switch is a PAIR. `cyclonedx: true` with no `cyclonedx_format`
//     asks for a document in no particular encoding, and it refuses.
//   - the artifact has to be identified by something that exists. It was sent a
//     `component_name` of "docker://" plus an Artifactory PATH, which named no
//     image in any repository.
func TestExportDetailsSendsAnIdentifierAndAFormat(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != exportDetailsPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"components":[]}`))
	}))
	t.Cleanup(srv.Close)

	c, err := NewXrayClient(XrayConfig{
		Endpoint: srv.URL, Username: "svc", Password: "token", RepositoryKey: "docker-local",
	})
	if err != nil {
		t.Fatalf("NewXrayClient: %v", err)
	}

	const path = "docker-local/orbs/cfx-amf/25.10.2/manifest.json"
	if _, _, err := c.ExportDetails(t.Context(), path); err != nil {
		t.Fatalf("ExportDetails: %v", err)
	}

	if got["path"] != path {
		t.Errorf("path = %v, want the artifact's full Artifactory path %q", got["path"], path)
	}
	// The old body's identifier. It named nothing, and sending it alongside a
	// good one is how a request comes back ambiguous.
	if name, ok := got["component_name"]; ok {
		t.Errorf("component_name = %v: a path is not a docker component name", name)
	}
	if got["cyclonedx"] != true {
		t.Errorf("cyclonedx = %v, want true", got["cyclonedx"])
	}
	if got["cyclonedx_format"] != "json" {
		t.Errorf("cyclonedx_format = %v, want json - the format switch is a pair",
			got["cyclonedx_format"])
	}
}

// Xray answers exportDetails with a ZIP of one file per requested section, and
// the inventory is the one anybody opened it for.
func TestExportDetailsUnwrapsTheArchive(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	small, _ := zw.Create("manifest.txt")
	_, _ = small.Write([]byte("a short manifest"))
	big, _ := zw.Create("cyclonedx.json")
	_, _ = big.Write([]byte(`{"bomFormat":"CycloneDX","components":[{"name":"openssl"}]}`))
	_ = zw.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(buf.Bytes())
	}))
	t.Cleanup(srv.Close)

	c, err := NewXrayClient(XrayConfig{
		Endpoint: srv.URL, Username: "svc", Password: "token", RepositoryKey: "docker-local",
	})
	if err != nil {
		t.Fatalf("NewXrayClient: %v", err)
	}

	body, contentType, err := c.ExportDetails(t.Context(), "docker-local/x/1/manifest.json")
	if err != nil {
		t.Fatalf("ExportDetails: %v", err)
	}
	if !bytes.Contains(body, []byte("CycloneDX")) {
		t.Errorf("got %q, want the inventory rather than the manifest beside it", body)
	}
	if contentType != "application/json" {
		t.Errorf("contentType = %q, want application/json", contentType)
	}
}

// The path is built from the artifact, and an image with no tag falls back to
// its digest - which is how Artifactory stores an untagged manifest.
func TestPathForNamesTheManifest(t *testing.T) {
	p := &XrayProvider{client: &XrayClient{repoKey: "docker-local"}}

	got := p.PathFor(security.ArtifactRef{
		Repository: "docker-local/orbs/cfx-amf", Tag: "25.10.2", Kind: "image",
	})
	if want := "docker-local/orbs/cfx-amf/25.10.2/manifest.json"; got != want {
		t.Errorf("PathFor = %q, want %q", got, want)
	}

	untagged := p.PathFor(security.ArtifactRef{
		Repository: "docker-local/orbs/cfx-amf",
		Digest:     "sha256:abc123",
		Kind:       "image",
	})
	if want := "docker-local/orbs/cfx-amf/abc123/manifest.json"; untagged != want {
		t.Errorf("PathFor(untagged) = %q, want %q", untagged, want)
	}
}
