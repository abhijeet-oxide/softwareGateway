package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Digest is a content address, "sha256:<64 hex>".
//
// Our own type rather than a library's: ADR-001 is open, and a digest crossing
// into domain packages must not pin the library choice. It is a string so it
// travels through URLs, logs, JSON and SQL without conversion — the alternative
// (raw bytes) would cost an encode at every boundary to save 30 bytes a row.
type Digest string

// Algorithms we accept. sha512 is permitted by the OCI spec but is not used in
// practice; rejecting it keeps the parser strict rather than speculative.
const (
	AlgorithmSHA256 = "sha256"
)

// NewDigestFromBytes computes the digest of content.
func NewDigestFromBytes(b []byte) Digest {
	sum := sha256.Sum256(b)
	return Digest(AlgorithmSHA256 + ":" + hex.EncodeToString(sum[:]))
}

// Validate reports whether the digest is well formed.
//
// Called on every value received from a registry. A malformed digest that
// reached the database would poison the content-address model that every
// deduplication decision rests on.
func (d Digest) Validate() error {
	algo, hex, ok := strings.Cut(string(d), ":")
	if !ok {
		return fmt.Errorf("digest %q: missing algorithm prefix", d)
	}
	if algo != AlgorithmSHA256 {
		return fmt.Errorf("digest %q: unsupported algorithm %q", d, algo)
	}
	if len(hex) != 64 {
		return fmt.Errorf("digest %q: expected 64 hex characters, got %d", d, len(hex))
	}
	for i := 0; i < len(hex); i++ {
		c := hex[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("digest %q: %q is not lowercase hex", d, c)
		}
	}
	return nil
}

// Algorithm returns the algorithm prefix.
func (d Digest) Algorithm() string {
	algo, _, _ := strings.Cut(string(d), ":")
	return algo
}

// Short renders an abbreviated form for human-facing output.
func (d Digest) Short() string {
	algo, hex, ok := strings.Cut(string(d), ":")
	if !ok || len(hex) < 12 {
		return string(d)
	}
	return algo + ":" + hex[:12]
}

func (d Digest) String() string { return string(d) }

// Descriptor identifies a piece of content.
//
// Structurally similar to ocispec.Descriptor, but ours: neither ADR-001
// candidate's types appear in domain packages, and each backend converts at
// its own boundary.
type Descriptor struct {
	MediaType    string            `json:"mediaType"`
	Digest       Digest            `json:"digest"`
	Size         int64             `json:"size"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	Platform     *Platform         `json:"platform,omitempty"`
}

// Platform is an image's target platform. Nil for non-image artifacts such as
// Helm charts and configuration bundles.
type Platform struct {
	OS           string   `json:"os"`
	Architecture string   `json:"architecture"`
	Variant      string   `json:"variant,omitempty"`
	OSVersion    string   `json:"os.version,omitempty"`
	OSFeatures   []string `json:"os.features,omitempty"`
}

// String renders "linux/amd64" or "linux/arm64/v8".
func (p *Platform) String() string {
	if p == nil {
		return ""
	}
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// Media types we recognise. The index and manifest types drive the artifact
// tree walk; the rest are recorded for reporting.
const (
	MediaTypeOCIIndex        = "application/vnd.oci.image.index.v1+json"
	MediaTypeOCIManifest     = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeDockerList      = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaTypeDockerManifest  = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeHelmChartConfig = "application/vnd.cncf.helm.config.v1+json"
)

// ManifestAcceptTypes is the Accept header sent when resolving or fetching a
// manifest.
//
// Both OCI and Docker types are listed because vendors publish both, and a
// registry returns whichever the client says it understands. Omitting the
// Docker types would make older vendor repositories appear empty — a
// confusing failure that looks like a permissions problem.
var ManifestAcceptTypes = []string{
	MediaTypeOCIIndex,
	MediaTypeOCIManifest,
	MediaTypeDockerList,
	MediaTypeDockerManifest,
}

// IsIndex reports whether a media type is a multi-manifest index.
func IsIndex(mediaType string) bool {
	return mediaType == MediaTypeOCIIndex || mediaType == MediaTypeDockerList
}

// IsManifest reports whether a media type is a single-artifact manifest.
func IsManifest(mediaType string) bool {
	return mediaType == MediaTypeOCIManifest || mediaType == MediaTypeDockerManifest
}

// UploadState is a resumable upload position. M3; declared here so the
// jobs.upload_state column has a Go shape.
type UploadState struct {
	Location string `json:"location"`
	Offset   int64  `json:"offset"`
}
