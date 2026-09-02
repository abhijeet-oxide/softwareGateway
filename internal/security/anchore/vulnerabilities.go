package anchore

import (
	"context"
	"encoding/json"
	"fmt"
)

// Reading what Anchore found, by both routes the integration guide names.
//
// The two views are collected rather than one replacing the other, because
// their aggregation genuinely differs: the image view is one row per
// (advisory, package) in one image, which is the platform's own finding
// identity; the application-version view is one row per advisory with a list of
// matches across every associated image, which is the view a release
// conversation happens in. Neither is derivable from the other without losing
// something - the image view has no idea which release it belongs to, and the
// version view flattens two images carrying the same package into one entry.

// vulnerabilityType is the value of {vuln_type} on the image endpoint.
//
// "all" rather than "os" or "non-os": a release here is a vendor's container
// image, and the language-level packages inside it - the jars, the wheels, the
// node modules - are exactly the part a vendor ships and a customer cannot
// patch. Asking for the OS packages alone would report a Java application's
// image as clean because its base was.
const vulnerabilityType = "all"

// ImageVulnerabilities reads one image's findings, and returns the raw body
// alongside.
//
// # Why the raw body comes back too
//
// Because this is the request that was going to happen anyway, and the body is
// what somebody eventually hands to a vendor. Re-fetching it at download time
// would be the whole sync again for a button somebody expects to be instant;
// storing the platform's own re-encoding of it would hand a vendor a document
// that looks authoritative and is not what their scanner said.
//
// # include_vuln_description
//
// Asked for, deliberately, and it is the reason an Anchore finding is worth
// merging with an Xray one at all: Xray carries a CVSS vector and a policy,
// Anchore carries the advisory's prose and its EPSS. Without this parameter the
// description is omitted and the enrichment has nothing to add.
func (c *Client) ImageVulnerabilities(
	ctx context.Context, digest string, refresh bool,
) (imageVulnerabilities, []byte, error) {
	path := fmt.Sprintf("/images/%s/vuln/%s%s", pathEscape(digest), vulnerabilityType,
		query(
			"include_vuln_description", "true",
			"force_refresh", boolParam(refresh),
		))

	raw, err := c.raw(ctx, path)
	if err != nil {
		return imageVulnerabilities{}, nil, err
	}
	var out imageVulnerabilities
	if err := json.Unmarshal(raw, &out); err != nil {
		// The raw body still travels: a response this build cannot parse is
		// exactly the one somebody needs to look at, and discarding it leaves
		// them with a parse error and no evidence.
		return imageVulnerabilities{}, raw, fmt.Errorf(
			"anchore: could not read the vulnerability response for %s: %w", digest, err)
	}
	return out, raw, nil
}

// VersionVulnerabilities reads the application-version report: every advisory
// across every image associated with one release.
//
// Returns the raw body as well, on the same argument as the image one - and
// this is the body that goes into an export as the release-level answer, the
// one thing an image-by-image bundle cannot reconstruct.
func (c *Client) VersionVulnerabilities(
	ctx context.Context, version Version,
) (versionVulnerabilities, []byte, error) {
	path := fmt.Sprintf("/applications/%s/versions/%s/vulnerabilities",
		pathEscape(version.ApplicationID), pathEscape(version.VersionID))

	raw, err := c.raw(ctx, path)
	if err != nil {
		return versionVulnerabilities{}, nil, err
	}
	var out versionVulnerabilities
	if err := json.Unmarshal(raw, &out); err != nil {
		return versionVulnerabilities{}, raw, fmt.Errorf(
			"anchore: could not read the application version vulnerability report: %w", err)
	}
	return out, raw, nil
}

// SBOM reads one image's component inventory in the requested format.
//
// Format is one of the three the API serves: native-json, spdx-json,
// cyclonedx-json. Returned as bytes and never parsed: an SBOM is a document to
// hand on, and a component list this platform re-derived from it would be a
// different document.
func (c *Client) SBOM(ctx context.Context, digest, format string) ([]byte, error) {
	if format == "" {
		format = "spdx-json"
	}
	return c.raw(ctx, fmt.Sprintf("/images/%s/sboms/%s", pathEscape(digest), format))
}

// PolicyEvaluation reads Anchore's own policy verdict for one image.
//
// The gate rather than the backlog, in the same sense as an Xray watch: it is
// what a release decision is made against, it disappears when somebody edits a
// policy, and it can fail an image with no CVE anywhere near it.
func (c *Client) PolicyEvaluation(ctx context.Context, digest, tag string) ([]byte, error) {
	return c.raw(ctx, fmt.Sprintf("/images/%s/check%s", pathEscape(digest),
		query("tag", tag, "detail", "true")))
}

// Malware reads what Anchore's malware scanner found in one image.
//
// A separate content type rather than a kind of vulnerability, which is the
// same shape this platform already uses: a malicious package is not a backlog
// item to grade against ninety thousand others, it is a release that does not
// ship tonight.
func (c *Client) Malware(ctx context.Context, digest string) ([]byte, error) {
	return c.raw(ctx, fmt.Sprintf("/images/%s/content/malware", pathEscape(digest)))
}

// VEX reads the OpenVEX document for one image, where the deployment records
// exploitability statements.
func (c *Client) VEX(ctx context.Context, digest string) ([]byte, error) {
	return c.raw(ctx, fmt.Sprintf("/images/%s/vex/openvex", pathEscape(digest)))
}
