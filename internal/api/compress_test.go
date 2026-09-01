package api

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/security"
)

// The API's largest answers are JSON in which almost everything repeats, and
// until this middleware existed every one of them went out uncompressed. A
// comparison of two large releases was over a hundred megabytes on the wire -
// minutes of a reader watching a spinner on any link slower than a LAN - and
// nearly all of it was the same few thousand strings written out again and
// again.
//
// Two things are asserted, because either alone would let the fix rot: that a
// caller asking for gzip gets it AND that what arrives decodes to the same
// document a caller who did not ask for it receives.
func TestJSONResponsesAreCompressedWhenAskedFor(t *testing.T) {
	h := newSecurityHarness(t)
	h.seedPackage("25.7.2131", digestA)
	// Enough repetition for compression to have something to do, which is also
	// the shape of the real payloads: a few strings, many times.
	findings := make([]security.Finding, 0, 400)
	for i := range 400 {
		findings = append(findings,
			apiFinding("CVE-2024-0001", security.SeverityHigh, "openssl", true))
		findings[i].Component.Version = strings.Repeat("1", 8) + "." + string(rune('a'+i%26))
	}
	h.syncer.reports[digestA] = scannedReport(findings...)
	h.post("/api/v1/products/vendor-a/packages/25.7.2131:syncSecurity", `{}`, nil)

	path := "/api/v1/products/vendor-a/packages/25.7.2131/security?detail=true"

	plain, plainHeader := h.getUncompressed(path)
	if enc := plainHeader.Get("Content-Encoding"); enc != "" {
		t.Fatalf("a caller that did not ask for an encoding got %q", enc)
	}

	compressed, header := h.getGzipped(path)
	if header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("content-encoding = %q, want gzip", header.Get("Content-Encoding"))
	}
	// Without this a shared cache would serve the compressed body to a client
	// that cannot read it. Values, not Get: the header is multi-valued here -
	// the answer varies by Authorization as well - and Get would only ever
	// show the first reason.
	if !slices.Contains(header.Values("Vary"), "Accept-Encoding") {
		t.Errorf("vary = %q, want it to name Accept-Encoding", header.Values("Vary"))
	}
	if len(compressed) >= len(plain) {
		t.Errorf("compressed %d bytes from %d - nothing was gained", len(compressed), len(plain))
	}

	zr, err := gzip.NewReader(strings.NewReader(string(compressed)))
	if err != nil {
		t.Fatalf("the body is not gzip: %v", err)
	}
	decoded, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read the compressed body: %v", err)
	}
	if string(decoded) != string(plain) {
		t.Fatalf("the compressed body decodes to something else (%d vs %d bytes)",
			len(decoded), len(plain))
	}
	// And it is still the document, not merely the same bytes.
	var doc map[string]any
	if err := json.Unmarshal(decoded, &doc); err != nil {
		t.Fatalf("the decoded body is not JSON: %v", err)
	}
}

// An export is already compressed. Running deflate over it would spend CPU on
// both ends to make the body very slightly larger.
func TestAlreadyCompressedDownloadsAreNotRecompressed(t *testing.T) {
	h := newSecurityHarness(t)
	h.seedPackage("25.7.2131", digestA)
	h.syncer.reports[digestA] = scannedReport(
		apiFinding("CVE-2024-3094", security.SeverityCritical, "xz-utils", true))
	h.post("/api/v1/products/vendor-a/packages/25.7.2131:syncSecurity", `{}`, nil)

	_, header := h.getGzipped(
		"/api/v1/products/vendor-a/packages/25.7.2131/security/export?format=zip&view=detailed")
	if enc := header.Get("Content-Encoding"); enc != "" {
		t.Errorf("a zip was sent with content-encoding %q", enc)
	}
}

// getUncompressed and getGzipped both set Accept-Encoding themselves, which
// turns OFF net/http's transparent decompression - the point here is what went
// on the wire, and the default client hides exactly that.
func (h *apiHarness) getUncompressed(path string) ([]byte, http.Header) {
	h.t.Helper()
	return h.getWithEncoding(path, "identity")
}

func (h *apiHarness) getGzipped(path string) ([]byte, http.Header) {
	h.t.Helper()
	return h.getWithEncoding(path, "gzip")
}

func (h *apiHarness) getWithEncoding(path, encoding string) ([]byte, http.Header) {
	h.t.Helper()
	req, err := http.NewRequestWithContext(h.t.Context(), http.MethodGet, h.server.URL+path, nil)
	if err != nil {
		h.t.Fatalf("build request %s: %v", path, err)
	}
	req.Header.Set("Accept-Encoding", encoding)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read %s: %v", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("GET %s: %d %s", path, resp.StatusCode, string(body))
	}
	return body, resp.Header
}
