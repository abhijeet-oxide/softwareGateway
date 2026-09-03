package anchore

import "encoding/json"

// Anchore's wire shapes, as the 5.22 OpenAPI document defines them.
//
// # Why these are hand-written and not generated
//
// Because the generated set is four hundred types for the twelve endpoints
// this integration uses, and because the fields that matter here are the ones
// the document declares `nullable` - a fixed version that is the string "None",
// a CVSS score that is null rather than absent, a severity that is a vendor's
// word rather than one of five. A generated struct carries all of that
// faithfully and leaves every caller to cope; these carry it once, in the one
// place that has read the schema.
//
// Everything is a pointer or a tolerant type where the document says the value
// may be missing or null. Nothing here has a required field this code would
// crash on.

// image is Anchore's record of one image: AnchoreImage in the schema.
type image struct {
	ImageDigest    string        `json:"image_digest"`
	ParentDigest   string        `json:"parent_digest"`
	AccountName    string        `json:"account_name"`
	AnalysisStatus string        `json:"analysis_status"`
	ImageStatus    string        `json:"image_status"`
	CreatedAt      *jsonTime     `json:"created_at"`
	LastUpdated    *jsonTime     `json:"last_updated"`
	ImageDetail    []imageDetail `json:"image_detail"`
	// AnalysisStatusDetail carries the per-phase progress and, on a failure,
	// the reason. Kept as raw JSON: its shape varies by Anchore build and the
	// only use for it here is to put Anchore's own words in a message.
	AnalysisStatusDetail json.RawMessage `json:"analysis_status_detail"`
}

// analyzedAt is when Anchore finished, where it says.
//
// ImageDetail carries it per tag reference; the record itself carries only
// created and updated times, and `last_updated` moves for reasons that are not
// an analysis. The newest analyzed_at across the details is the honest answer.
func (i image) analyzedAt() *jsonTime {
	var best *jsonTime
	for idx := range i.ImageDetail {
		at := i.ImageDetail[idx].AnalyzedAt
		if at == nil {
			continue
		}
		if best == nil || at.Time().After(best.Time()) {
			best = at
		}
	}
	return best
}

// fullTag is a pull string Anchore knows this image by, for display.
func (i image) fullTag() string {
	for _, d := range i.ImageDetail {
		if d.FullTag != "" {
			return d.FullTag
		}
	}
	return ""
}

// imageDetail is one tag reference of one image: ImageDetail in the schema.
type imageDetail struct {
	FullTag    string    `json:"fulltag"`
	Registry   string    `json:"registry"`
	Repository string    `json:"repo"`
	Tag        string    `json:"tag"`
	ImageID    string    `json:"imageId"`
	AnalyzedAt *jsonTime `json:"analyzed_at"`
}

// imageList is the envelope GET /images answers with.
type imageList struct {
	Items []image `json:"items"`
}

// analysisRequest is ImageAnalysisRequest.
//
// Only the digest source is ever sent. See submit: a tag is a moving target
// and the whole platform identifies an artifact by digest, so submitting by tag
// would let Anchore analyse whatever the tag points at now and report it as
// this release's result.
type analysisRequest struct {
	ImageType   string            `json:"image_type,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Source      analysisSource    `json:"source"`
}

type analysisSource struct {
	Digest *digestSource `json:"digest,omitempty"`
	Tag    *tagSource    `json:"tag,omitempty"`
}

// digestSource is RegistryDigestSource. Both fields are required by the schema:
// the pull string identifies the bytes, and the tag is recorded alongside so
// Anchore's own interface can show a name rather than a hash.
type digestSource struct {
	PullString string `json:"pull_string"`
	Tag        string `json:"tag"`
}

type tagSource struct {
	PullString string `json:"pull_string"`
}

// application is Application.
type application struct {
	ApplicationID string    `json:"application_id"`
	Name          string    `json:"name"`
	Description   string    `json:"description,omitempty"`
	CreatedAt     *jsonTime `json:"created_at,omitempty"`
}

// applicationVersion is ApplicationVersion.
type applicationVersion struct {
	ApplicationVersionID string    `json:"application_version_id"`
	ApplicationID        string    `json:"application_id,omitempty"`
	VersionName          string    `json:"version_name"`
	CreatedAt            *jsonTime `json:"created_at,omitempty"`
}

// associationRequest is ArtifactAssociationRequest.
//
// ArtifactKeys is `object` in the schema - "a json with key-pair values to
// query on" - and for an image the key Anchore matches on is the digest. A map
// rather than a struct because the schema declares it open, and a build that
// wants a second key must not need this type changed.
type associationRequest struct {
	ArtifactType string            `json:"artifact_type"`
	ArtifactKeys map[string]string `json:"artifact_keys"`
}

// artifactList is ArtifactListResponse: what a version actually holds.
type artifactList struct {
	AssociatedImageArtifacts []struct {
		Metadata struct {
			AssociationID string `json:"association_id"`
		} `json:"artifact_association_metadata"`
		Image struct {
			ImageDigest    string `json:"image_digest"`
			AnalysisStatus string `json:"analysis_status"`
		} `json:"image"`
	} `json:"associated_image_artifacts"`
}

// digests is the image digests a version is associated with.
func (l artifactList) digests() map[string]string {
	out := make(map[string]string, len(l.AssociatedImageArtifacts))
	for _, a := range l.AssociatedImageArtifacts {
		if a.Image.ImageDigest != "" {
			out[a.Image.ImageDigest] = a.Metadata.AssociationID
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Vulnerabilities
// ---------------------------------------------------------------------------

// imageVulnerabilities is ImagePackageVulnerabilityResponse.
type imageVulnerabilities struct {
	ImageDigest       string        `json:"image_digest"`
	VulnerabilityType string        `json:"vulnerability_type"`
	ExtendedSupport   bool          `json:"extended_support"`
	Vulnerabilities   []packageVuln `json:"vulnerabilities"`
}

// packageVuln is PackageVulnerability plus the image-level extension.
//
// One row per (vulnerability, package) - which is exactly the platform's own
// finding identity, and the reason normalization here is a field mapping
// rather than a restructuring.
type packageVuln struct {
	Vuln        string `json:"vuln"`
	Severity    string `json:"severity"`
	URL         string `json:"url"`
	Description string `json:"description"`
	// Fix is the version containing a fix, or the literal string "None".
	//
	// "None" rather than an empty string or a null, and it is the single most
	// important quirk in this file: read naively, every unfixable finding in
	// an Anchore release reports a fixed version called None and renders as
	// fixable. See fixVersions.
	Fix string `json:"fix"`

	Package        string `json:"package"`
	PackageName    string `json:"package_name"`
	PackageVersion string `json:"package_version"`
	PackageType    string `json:"package_type"`
	PackagePath    string `json:"package_path"`
	PackageCPE     string `json:"package_cpe"`
	PackageCPE23   string `json:"package_cpe23"`
	PURL           string `json:"purl"`

	Feed      string `json:"feed"`
	FeedGroup string `json:"feed_group"`

	NVDData    []nvdData    `json:"nvd_data"`
	VendorData []vendorData `json:"vendor_data"`

	WillNotFix bool `json:"will_not_fix"`
	// InheritedFromBase says the vulnerable package came from the base image.
	//
	// Carried because it changes who fixes it: a finding inherited from a base
	// image is the base image's maintainer's work, and a vendor asked to fix
	// one they inherited will say so.
	InheritedFromBase bool `json:"inherited_from_base"`

	AnnotationStatus string    `json:"annotation_status"`
	DetectedAt       *jsonTime `json:"detected_at"`
	FixObservedAt    *jsonTime `json:"fix_observed_at"`
}

// nvdData is NvdDataObject: upstream grading, and the two fields that make
// this integration worth having.
type nvdData struct {
	ID          string     `json:"id"`
	Type        string     `json:"type"`
	Source      string     `json:"source"`
	Description string     `json:"description"`
	CVSSV2      *cvssScore `json:"cvss_v2"`
	CVSSV3      *cvssScore `json:"cvss_v3"`
	// IsKEV says the vulnerability is on CISA's Known Exploited catalogue.
	//
	// The reason Anchore is being integrated at all, from a release manager's
	// point of view: it is the one field that separates "somebody has used
	// this" from "somebody has scored this".
	IsKEV bool  `json:"is_kev"`
	EPSS  *epss `json:"epss"`
}

// vendorData is VendorDataObject: the distribution's own grading, which
// routinely differs from NVD's and is routinely the more accurate of the two
// for a package as that distribution ships it.
type vendorData struct {
	ID     string     `json:"id"`
	Type   string     `json:"type"`
	Source string     `json:"source"`
	CVSSV2 *cvssScore `json:"cvss_v2"`
	CVSSV3 *cvssScore `json:"cvss_v3"`
}

// cvssScore is CVSSV2Scores / CVSSV3Scores. Every field is nullable in the
// schema, hence the pointers: a null base score and a base score of zero are
// different facts, and only one of them means "not scored".
type cvssScore struct {
	BaseScore           *float64 `json:"base_score"`
	ExploitabilityScore *float64 `json:"exploitability_score"`
	ImpactScore         *float64 `json:"impact_score"`
	// Vector is not in the 5.22 image response but is in some builds' vendor
	// data. Read where present, because a reader who works in vectors wants
	// the vector and reconstructing one from three scores is not possible.
	Vector string `json:"vector,omitempty"`
}

func (c *cvssScore) base() float64 {
	if c == nil || c.BaseScore == nil {
		return 0
	}
	return *c.BaseScore
}

// epss is PackageEPSS.
type epss struct {
	Score      float64 `json:"epss"`
	Percentile float64 `json:"percentile"`
}

// ---------------------------------------------------------------------------
// Application-version vulnerabilities
// ---------------------------------------------------------------------------

// versionVulnerabilities is ApplicationVersionVulnerabilityReport.
//
// A genuinely different shape from the image response, and not a superset of
// it: one entry per ADVISORY, with a `matches` list saying which package in
// which artifact it landed on. The integration guide is explicit that the two
// views must both be collected rather than one replacing the other, because
// their aggregation differs - and this shape is why.
type versionVulnerabilities struct {
	Application struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		VersionName string `json:"version_name"`
		VersionID   string `json:"version_id"`
		Artifacts   struct {
			Images []struct {
				ImageDigest string `json:"image_digest"`
			} `json:"images"`
		} `json:"artifacts"`
	} `json:"application"`
	Vulnerabilities []versionVuln `json:"vulnerabilities"`
}

type versionVuln struct {
	ID         string         `json:"id"`
	NVD        []versionNVD   `json:"nvd"`
	VendorData *versionVendor `json:"vendor_data"`
	Matches    []versionMatch `json:"matches"`
}

type versionNVD struct {
	ID          string       `json:"id"`
	Severity    string       `json:"severity"`
	Description string       `json:"description"`
	URL         string       `json:"url"`
	CVSS        *versionCVSS `json:"cvss"`
}

type versionVendor struct {
	Severity    string       `json:"severity"`
	Description string       `json:"description"`
	URL         string       `json:"url"`
	Feed        string       `json:"feed"`
	Group       string       `json:"group"`
	WillNotFix  bool         `json:"will_not_fix"`
	CVSS        *versionCVSS `json:"cvss"`
}

type versionCVSS struct {
	CVSSV2 *cvssScore `json:"cvss_v2"`
	CVSSV3 *cvssScore `json:"cvss_v3"`
}

type versionMatch struct {
	Fix      string `json:"fix"`
	Location struct {
		Artifact struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"artifact"`
		Package struct {
			Name     string `json:"name"`
			Type     string `json:"type"`
			Version  string `json:"version"`
			Location string `json:"location"`
		} `json:"package"`
	} `json:"location"`
}

// apiError is ApiErrorResponse, for turning Anchore's own words into a message.
type apiError struct {
	Message  string `json:"message"`
	Detail   any    `json:"detail"`
	HTTPCode int    `json:"httpcode"`
}
