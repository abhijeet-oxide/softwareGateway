package artifactory

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/security"
)

// The three things Xray knows about an image that are not a list of CVEs: the
// component inventory, the policy verdict, and whether anything in it is
// outright malicious.
//
// See internal/security/document.go for why these are one concept rather than
// three features. This file is the JFrog half: which endpoints, in what shape,
// and what to say when a platform does not have one.
//
// # Why every failure here is a message rather than an error
//
// Because these are the endpoints a JFrog deployment is most likely to differ
// on. `component/exportDetails` is not on every Xray version, `violations`
// needs a permission a read-only scanner token often lacks, and a locked-down
// platform may serve neither. None of that is a reason to fail an export of
// vulnerabilities that were retrieved perfectly well - so an unavailable
// document is a document with no payload and a sentence saying which of those
// happened, and the export ships with a note in place of a file.

const (
	// exportDetailsPath produces the component inventory for one artifact.
	//
	// The long-standing endpoint, and the one most JFrog platforms have. It
	// answers with a ZIP of files rather than a bare document, which is why
	// there is an unzipper below - a surprise worth handling once here rather
	// than discovering per deployment.
	exportDetailsPath = "/xray/api/v1/component/exportDetails"
	// violationsPath lists what the configured watches say about an artifact.
	violationsPath = "/xray/api/v1/violations"
)

// violationPageLimit is how many violations one request asks for.
//
// A hundred, matching Xray's own paging default. An image with more than a few
// hundred violations has a watch configured to flag everything, and paging
// through ten thousand of them to render a tab nobody can act on is a cost paid
// on every export for no reader's benefit - see maxViolationPages.
const violationPageLimit = 100

// maxViolationPages bounds the paging.
//
// Ten pages, a thousand violations. Past that the answer is "this watch flags
// everything", which is a configuration observation and not a list somebody
// works through, and the response says so rather than pretending to be
// complete.
const maxViolationPages = 10

// Documents implements security.DocumentProvider.
//
// Fetched through the same pacer as everything else, because these are requests
// to the same Xray and an export that ignored the concurrency budget would be a
// download button that takes the scanner down.
func (p *XrayProvider) Documents(
	ctx context.Context, refs []security.ArtifactRef,
	kinds []security.DocumentKind, opts security.ScanOptions,
) ([]security.Document, error) {
	if len(refs) == 0 || len(kinds) == 0 {
		return nil, nil
	}

	wanted := map[security.DocumentKind]bool{}
	for _, k := range kinds {
		wanted[k] = true
	}

	var (
		mu   sync.Mutex
		out  []security.Document
		done int
	)
	total := len(refs) * len(kinds)
	security.ReportStage(opts.Progress, security.StageExporting, 0, total)

	emit := func(docs ...security.Document) {
		mu.Lock()
		defer mu.Unlock()
		out = append(out, docs...)
		done += len(docs)
		security.ReportStage(opts.Progress, security.StageExporting, done, total)
	}

	var wg sync.WaitGroup
	for _, ref := range refs {
		if !scannable(ref) {
			// A Helm chart has no SBOM and no policy verdict for the same
			// reason it has no vulnerabilities: there is nothing in it to
			// index. Answered locally, so an export of a 260-artifact release
			// does not spend 400 requests learning it.
			continue
		}
		wg.Add(1)
		go func(ref security.ArtifactRef) {
			defer wg.Done()
			if err := p.pace.Acquire(ctx); err != nil {
				return
			}
			defer p.pace.Release()

			if wanted[security.DocumentSBOM] {
				emit(p.sbomFor(ctx, ref))
			}
			if wanted[security.DocumentPolicy] || wanted[security.DocumentMalware] {
				policy, malware := p.violationsFor(ctx, ref)
				if wanted[security.DocumentPolicy] {
					emit(policy)
				}
				if wanted[security.DocumentMalware] {
					emit(malware)
				}
			}
		}(ref)
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return out, err
	}
	return out, nil
}

// sbomFor retrieves one artifact's component inventory.
func (p *XrayProvider) sbomFor(ctx context.Context, ref security.ArtifactRef) security.Document {
	doc := security.Document{
		Artifact:    ref,
		Kind:        security.DocumentSBOM,
		Provider:    providerName,
		ContentType: "application/json",
		FetchedAt:   time.Now().UTC(),
	}

	path := p.PathFor(ref)
	if path == "" {
		doc.Message = "This repository has no Artifactory repository key configured, " +
			"so JFrog Xray cannot be asked for an SBOM. Set xrayRepositoryKey."
		return doc
	}

	body, contentType, err := p.client.ExportDetails(ctx, path)
	if err != nil {
		doc.Message = describeDocumentFailure("SBOM", err)
		return doc
	}
	doc.Payload = body
	doc.ContentType = contentType
	doc.Available = len(body) > 0
	doc.SourceBytes = len(body)
	if !doc.Available {
		doc.Message = "JFrog Xray produced no SBOM for this image."
	}
	return doc
}

// violationsFor retrieves one artifact's policy verdict, and splits the malware
// out of it.
//
// One request, two documents. The malicious findings are IN the violations
// response - Xray does not have a separate malware endpoint - and asking twice
// to present them on two tabs would double the requests for one answer.
func (p *XrayProvider) violationsFor(
	ctx context.Context, ref security.ArtifactRef,
) (policy, malware security.Document) {
	now := time.Now().UTC()
	policy = security.Document{
		Artifact: ref, Kind: security.DocumentPolicy, Provider: providerName,
		ContentType: "application/json", FetchedAt: now,
	}
	malware = security.Document{
		Artifact: ref, Kind: security.DocumentMalware, Provider: providerName,
		ContentType: "application/json", FetchedAt: now,
	}

	path := p.PathFor(ref)
	if path == "" {
		msg := "This repository has no Artifactory repository key configured, " +
			"so JFrog Xray cannot be asked which policies this image violates. " +
			"Set xrayRepositoryKey."
		policy.Message, malware.Message = msg, msg
		return policy, malware
	}

	raw, parsed, err := p.client.Violations(ctx, path)
	if err != nil {
		msg := describeDocumentFailure("policy violations", err)
		policy.Message, malware.Message = msg, msg
		return policy, malware
	}

	policy.Payload = raw
	policy.Available = len(raw) > 0
	policy.SourceBytes = len(raw)
	policy.Violations = normalizeViolations(parsed)

	// The malware document is the malicious subset, re-encoded. Its own body
	// rather than a pointer into the policy one, because the export writes it
	// into malware/<image>/<tag>/ and a reader opening that folder is entitled
	// to a file that contains only what the folder promises.
	var malicious []xrayViolation
	for _, v := range parsed {
		if isMaliciousViolation(v) {
			malicious = append(malicious, v)
		}
	}
	malware.Violations = normalizeViolations(malicious)
	if len(malicious) == 0 {
		malware.Available = true
		malware.Payload = []byte(`{"violations":[],"total_violations":0}`)
		malware.SourceBytes = len(malware.Payload)
		return policy, malware
	}
	encoded, err := json.MarshalIndent(map[string]any{
		"violations":       malicious,
		"total_violations": len(malicious),
	}, "", "  ")
	if err != nil {
		malware.Message = "The malicious findings could not be re-encoded: " + err.Error()
		return policy, malware
	}
	malware.Payload = encoded
	malware.Available = true
	malware.SourceBytes = len(encoded)
	return policy, malware
}

// describeDocumentFailure says what went wrong in a sentence a person can act
// on, in the same register as describeXrayFailure.
//
// These endpoints fail for reasons the vulnerability path does not, and the
// most common one - a token with scan permission and no violation permission -
// reads as a flat 403 that sends people to check whether Xray is running.
func describeDocumentFailure(what string, err error) string {
	var re *registry.Error
	if errors.As(err, &re) {
		switch registry.ClassOf(re.Err) {
		case registry.ClassNotFound:
			return "This JFrog platform does not offer the " + what + " endpoint. " +
				"It arrived with a later Xray version; everything else on this page is unaffected."
		case registry.ClassAuth:
			return "The JFrog credential is not allowed to read " + what + ". " +
				"Reading vulnerabilities and reading policy violations are separate " +
				"permissions in Xray, and this token has only the first."
		case registry.ClassTimeout:
			return "JFrog Xray did not produce the " + what + " in time. " +
				"Generating one for a large image is slow; try again, or raise " +
				"coordinator.security.requestTimeout."
		}
		if re.Detail != "" {
			return "JFrog Xray could not produce the " + what + ": " + re.Detail
		}
	}
	return "JFrog Xray could not produce the " + what + ": " + err.Error()
}

// ---------------------------------------------------------------------------
// Client calls
// ---------------------------------------------------------------------------

// exportDetailsRequest asks for one artifact's inventory.
//
// CycloneDX rather than SPDX, and JSON rather than XML, because that is what
// the tools people feed an SBOM to read first.
//
// # The two fields that were wrong, and what Xray said about them
//
// It answered "one or more parameters are missing" for every SBOM, and it was
// right twice over.
//
//	cyclonedx_format  A format switch is a PAIR in this API: `cyclonedx: true`
//	                  with no `cyclonedx_format` is a request for a document in
//	                  no particular encoding, and Xray refuses it. Same for spdx.
//
//	path              The artifact was identified by `component_name`, built as
//	                  "docker://" plus an Artifactory PATH - which produced
//	                  `docker://orbs/cfx-5000/25.7.2131/manifest.json`. A docker
//	                  component in Xray is `docker://<image>:<tag>`; that string
//	                  is neither, and no image has that name. `path` is the
//	                  addressing the violations call on the next endpoint along
//	                  already uses successfully against the same deployment, and
//	                  it is the one identifier this platform can always build
//	                  correctly, because it is how Artifactory stores the thing.
type exportDetailsRequest struct {
	// Path is the artifact's full Artifactory path, repository key included.
	Path string `json:"path"`
	// PackageType stays alongside it: some versions use it to choose how to
	// read the path, and it is free to send.
	PackageType string `json:"package_type,omitempty"`
	// OutputFormat is kept exactly as it was. The old request sent it and Xray
	// did not complain about it, so it is not one of the missing parameters -
	// and changing a field that was working, while fixing two that were not, is
	// how a fix becomes an experiment.
	OutputFormat string `json:"output_format,omitempty"`

	CycloneDX       bool   `json:"cyclonedx"`
	CycloneDXFormat string `json:"cyclonedx_format,omitempty"`

	Violations bool `json:"violations"`
	License    bool `json:"license"`
	Security   bool `json:"security"`
}

// ExportDetails asks Xray for one artifact's component inventory.
//
// # Why the response is unzipped here
//
// Because `exportDetails` answers with a ZIP archive containing one file per
// requested section, and a caller handed that ZIP would have to know Xray's
// packaging to find the SBOM inside it - which is precisely the knowledge this
// package exists to contain. A platform that answers with the bare document is
// handled too: the body is sniffed for the ZIP magic rather than assumed.
func (c *XrayClient) ExportDetails(ctx context.Context, path string) ([]byte, string, error) {
	req := exportDetailsRequest{
		Path:            path,
		PackageType:     "docker",
		OutputFormat:    "json",
		CycloneDX:       true,
		CycloneDXFormat: "json",
		Violations:      false,
		License:         true,
		Security:        true,
	}
	body, contentType, err := c.raw(ctx, http.MethodPost, exportDetailsPath, req)
	if err != nil {
		return nil, "", err
	}
	if unzipped, name, ok := firstZipEntry(body); ok {
		return unzipped, contentTypeForName(name), nil
	}
	if contentType == "" {
		contentType = "application/json"
	}
	return body, contentType, nil
}

// firstZipEntry pulls the largest file out of a ZIP body.
//
// The LARGEST rather than the first, because the archive carries a manifest and
// a licence summary beside the inventory, and the inventory is the one anybody
// opened the file for. Ordering inside a ZIP is not a contract; size, for these
// three, reliably is.
func firstZipEntry(body []byte) ([]byte, string, bool) {
	if len(body) < 4 || !bytes.HasPrefix(body, []byte("PK\x03\x04")) {
		return nil, "", false
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil || len(zr.File) == 0 {
		return nil, "", false
	}
	best := zr.File[0]
	for _, f := range zr.File {
		if f.UncompressedSize64 > best.UncompressedSize64 {
			best = f
		}
	}
	rc, err := best.Open()
	if err != nil {
		return nil, "", false
	}
	defer func() { _ = rc.Close() }()
	out, err := io.ReadAll(io.LimitReader(rc, maxDocumentBytes))
	if err != nil {
		return nil, "", false
	}
	return out, best.Name, true
}

func contentTypeForName(name string) string {
	switch {
	case strings.HasSuffix(name, ".xml"):
		return "application/xml"
	case strings.HasSuffix(name, ".json"):
		return "application/json"
	default:
		return "application/json"
	}
}

// xrayViolation is one breach of a configured watch, as Xray reports it.
type xrayViolation struct {
	ID            string   `json:"issue_id"`
	Type          string   `json:"type"`
	Severity      string   `json:"severity"`
	WatchName     string   `json:"watch_name"`
	Policies      []string `json:"policies"`
	RuleName      string   `json:"rule_name"`
	Summary       string   `json:"description"`
	Created       string   `json:"created"`
	ImpactedArt   string   `json:"impacted_artifact"`
	Cve           string   `json:"cve"`
	InfectedFiles []struct {
		Name          string   `json:"name"`
		PackageType   string   `json:"package_type"`
		DisplayName   string   `json:"display_name"`
		FixedVersions []string `json:"fixed_versions"`
	} `json:"infected_files"`
	// MaliciousPackage is set by the Xray versions that grade malware
	// explicitly. Read where it is present, inferred where it is not - see
	// isMaliciousViolation.
	MaliciousPackage bool `json:"malicious_package"`
}

type violationsResponse struct {
	Violations []xrayViolation `json:"violations"`
	Total      int             `json:"total_violations"`
}

type violationsRequest struct {
	Filters    violationFilters `json:"filters"`
	Pagination violationPaging  `json:"pagination"`
}

type violationFilters struct {
	Resources violationResources `json:"resources"`
	// WatchName scopes the answer to the watches this repository is configured
	// for, where any are. An unscoped query on a platform with fifty watches
	// returns fifty verdicts on one image, only one of which the reader's
	// pipeline gates on.
	WatchName string `json:"watch_name,omitempty"`
}

type violationResources struct {
	Artifacts []violationArtifact `json:"artifacts"`
}

type violationArtifact struct {
	Repo string `json:"repo"`
	Path string `json:"path"`
}

type violationPaging struct {
	OrderBy string `json:"order_by"`
	Limit   int    `json:"limit"`
	Offset  int    `json:"offset"`
}

// Violations lists what the configured policies say about one artifact.
//
// Returns the raw body of the FIRST page alongside the parsed union of every
// page. The raw body is what an export ships, and shipping page one of ten is a
// file that looks complete - so where paging happened the raw body is rebuilt
// from the parsed union instead, and the export carries all of it.
func (c *XrayClient) Violations(ctx context.Context, path string) ([]byte, []xrayViolation, error) {
	if c.repoKey == "" {
		return nil, nil, errNoRepositoryKey
	}

	watch := ""
	if len(c.watches) > 0 {
		watch = c.watches[0]
	}

	var (
		all       []xrayViolation
		firstBody []byte
		pages     int
	)
	for offset := 1; pages < maxViolationPages; offset++ {
		req := violationsRequest{
			Filters: violationFilters{
				Resources: violationResources{Artifacts: []violationArtifact{{
					Repo: c.repoKey,
					Path: strings.TrimPrefix(path, c.repoKey+"/"),
				}}},
				WatchName: watch,
			},
			Pagination: violationPaging{OrderBy: "created", Limit: violationPageLimit, Offset: offset},
		}
		body, _, err := c.raw(ctx, http.MethodPost, violationsPath, req)
		if err != nil {
			return nil, nil, err
		}
		if pages == 0 {
			firstBody = body
		}
		pages++

		var page violationsResponse
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, nil, &registry.Error{
				Op: "POST " + violationsPath, Repository: hostOf(c.endpoint),
				Detail: err.Error(), Err: registry.ErrMalformedResponse,
			}
		}
		all = append(all, page.Violations...)
		if len(page.Violations) < violationPageLimit {
			break
		}
	}

	if pages == 1 {
		return firstBody, all, nil
	}
	// More than one page: the stored body has to be the whole answer, or an
	// export ships a hundred violations out of six hundred and says nothing.
	rebuilt, err := json.MarshalIndent(map[string]any{
		"violations":       all,
		"total_violations": len(all),
		"truncated":        pages >= maxViolationPages,
	}, "", "  ")
	if err != nil {
		return firstBody, all, nil
	}
	return rebuilt, all, nil
}

// isMaliciousViolation reports whether a violation is a malware hit.
//
// The explicit flag where an Xray version sets it, and Xray's own summary
// convention where it does not - the same narrow match normalizeArtifact uses,
// for the same reason: a false negative leaves a hit in the policy tab where it
// is still visible, and a loose match moves ordinary findings into a tab that
// means "stop".
func isMaliciousViolation(v xrayViolation) bool {
	if v.MaliciousPackage {
		return true
	}
	if isMalwareIssueType(v.Type) {
		return true
	}
	summary := strings.ToLower(strings.TrimSpace(v.Summary))
	for _, prefix := range maliciousPrefixes {
		if strings.HasPrefix(summary, prefix) {
			return true
		}
	}
	return false
}

// normalizeViolations turns Xray's violations into the platform's.
func normalizeViolations(in []xrayViolation) []security.Violation {
	out := make([]security.Violation, 0, len(in))
	for _, v := range in {
		policy := ""
		if len(v.Policies) > 0 {
			policy = strings.Join(v.Policies, ", ")
		}
		component := security.Component{}
		var fixed []string
		if len(v.InfectedFiles) > 0 {
			f := v.InfectedFiles[0]
			name := f.DisplayName
			if name == "" {
				name = f.Name
			}
			component = componentOf(xrayComponent{ComponentID: name, PkgType: f.PackageType})
			fixed = dedupe(f.FixedVersions)
		}
		out = append(out, security.Violation{
			ID:        v.ID,
			Type:      strings.ToLower(strings.TrimSpace(v.Type)),
			Severity:  security.ParseSeverity(v.Severity),
			Watch:     v.WatchName,
			Policy:    policy,
			Rule:      v.RuleName,
			Summary:   v.Summary,
			CVE:       strings.ToUpper(strings.TrimSpace(v.Cve)),
			Component: component,
			FixedIn:   fixed,
			Created:   parseXrayTime(v.Created),
			Provider:  providerName,
		})
	}
	return out
}

// maxDocumentBytes bounds one document. An SBOM for a large container image is
// tens of megabytes; a body past this is a platform answering with something
// other than an SBOM.
const maxDocumentBytes = 256 << 20

// raw performs one request and returns the body unparsed.
//
// Its own path rather than a variant of send, because everything else in this
// client wants a decoded struct and these want the bytes: an SBOM that has been
// through a Go struct and back is not the SBOM the scanner produced, and the
// whole reason for keeping it is that it is.
func (c *XrayClient) raw(ctx context.Context, method, path string, body any) ([]byte, string, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, "", fmt.Errorf("xray: encode %s %s: %w", method, path, err)
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, bytes.NewReader(encoded))
	if err != nil {
		return nil, "", fmt.Errorf("xray: build %s %s: %w", method, path, err)
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, application/zip, */*")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", &registry.Error{
			Op: method + " " + path, Repository: hostOf(c.endpoint),
			Err: c.classifyTransport(ctx, err),
		}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return nil, "", c.statusError(method, path, resp)
	}
	out, err := io.ReadAll(io.LimitReader(resp.Body, maxDocumentBytes))
	if err != nil {
		return nil, "", &registry.Error{
			Op: method + " " + path, Repository: hostOf(c.endpoint),
			StatusCode: resp.StatusCode, Detail: err.Error(),
			Err: registry.ErrUnavailable,
		}
	}
	return out, resp.Header.Get("Content-Type"), nil
}

var _ security.DocumentProvider = (*XrayProvider)(nil)
