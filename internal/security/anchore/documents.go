package anchore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/abhijeet-oxide/softwareGateway/internal/security"
)

// The bodies Anchore produces that are not a list of vulnerabilities: the
// SBOM, the policy verdict, the malware scan.
//
// # Why these ride the same concept as Xray's
//
// Because a reader does not care which scanner produced the SBOM they are
// downloading, and neither does the storage, the retention, the eviction or the
// export tree. Implementing security.DocumentProvider means Anchore's SBOM
// arrives in the same bundle, at the same path, under the same button as
// Xray's - and a deployment running both gets both, side by side, without a
// line of new plumbing anywhere above this file.

// SBOMFormat is which flavour of SBOM this deployment wants from Anchore.
//
// Anchore serves three and they are not interchangeable: SPDX is what a
// compliance team asks for, CycloneDX is what most tooling reads, and the
// native format carries Anchore's own analysis detail that neither standard has
// a field for. SPDX is the default because the person who presses "download
// SBOM" in this platform is overwhelmingly about to send it to somebody.
const (
	SBOMFormatSPDX      = "spdx-json"
	SBOMFormatCycloneDX = "cyclonedx-json"
	SBOMFormatNative    = "native-json"
)

// Documents implements security.DocumentProvider.
//
// A per-artifact or per-kind failure is a Document with no payload and a
// message, never an error: one image whose SBOM will not generate must not lose
// the other hundred and fifty-six.
func (p *Provider) Documents(
	ctx context.Context, refs []security.ArtifactRef, kinds []security.DocumentKind,
	opts security.ScanOptions,
) ([]security.Document, error) {
	if len(refs) == 0 || len(kinds) == 0 {
		return nil, nil
	}

	// Only kinds Anchore actually serves. Asking for something it has no
	// endpoint for would be one 404 per image on the release somebody is
	// already waiting for.
	wanted := make([]security.DocumentKind, 0, len(kinds))
	for _, kind := range kinds {
		switch kind {
		case security.DocumentSBOM, security.DocumentPolicy, security.DocumentMalware:
			wanted = append(wanted, kind)
		}
	}
	if len(wanted) == 0 {
		return nil, nil
	}

	total := len(refs) * len(wanted)
	security.ReportStage(opts.Progress, security.StageFetching, 0, total)
	security.ReportInfo(opts.Progress, fmt.Sprintf(
		"Retrieving %s from Anchore for %d images.", describeKinds(wanted), len(refs)))

	var (
		mu   sync.Mutex
		out  []security.Document
		done int
	)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(p.concurrency())

	for _, ref := range refs {
		for _, kind := range wanted {
			g.Go(func() error {
				doc := p.document(gctx, ref, kind)
				mu.Lock()
				defer mu.Unlock()
				out = append(out, doc)
				done++
				security.ReportStage(opts.Progress, security.StageFetching, done, total)
				return nil
			})
		}
	}
	_ = g.Wait()
	return out, nil
}

func (p *Provider) document(
	ctx context.Context, ref security.ArtifactRef, kind security.DocumentKind,
) security.Document {
	doc := security.Document{
		Artifact: ref, Kind: kind, Provider: ProviderName,
		ContentType: "application/json", FetchedAt: time.Now().UTC(),
	}
	if !scannable(ref) || strings.TrimSpace(ref.Digest) == "" {
		doc.Message = unsupportedMessage(ref)
		return doc
	}

	var raw []byte
	var err error
	switch kind {
	case security.DocumentSBOM:
		raw, err = p.client.SBOM(ctx, ref.Digest, p.sbomFormat())
	case security.DocumentPolicy:
		raw, err = p.client.PolicyEvaluation(ctx, ref.Digest, TagString(withLocation(ref, p.settings)))
	case security.DocumentMalware:
		raw, err = p.client.Malware(ctx, ref.Digest)
	default:
		doc.Message = fmt.Sprintf("Anchore does not produce %s.", strings.ToLower(kind.Label()))
		return doc
	}

	switch {
	case NotFound(err):
		// Not a failure. An image Anchore has not analysed has no SBOM yet, and
		// a malware scan is only present where the deployment enabled the
		// analyser - which is a configuration fact, not a fault.
		doc.Message = notHeldMessage(kind)
		return doc
	case err != nil:
		doc.Message = describeFailure(err)
		return doc
	case len(raw) == 0:
		doc.Message = notHeldMessage(kind)
		return doc
	}

	doc.Payload = raw
	doc.SourceBytes = len(raw)
	doc.Available = true

	// The normalized half, where there is one. The raw body is what an export
	// ships; these are what the policy and malware tables render, and both come
	// from the one request rather than from two.
	switch kind {
	case security.DocumentPolicy:
		doc.Violations = normalizePolicy(raw)
	case security.DocumentMalware:
		doc.Findings = normalizeMalware(raw, ref)
	}
	return doc
}

func (p *Provider) sbomFormat() string {
	if f := strings.TrimSpace(p.settings.SBOMFormat); f != "" {
		return f
	}
	return SBOMFormatSPDX
}

func notHeldMessage(kind security.DocumentKind) string {
	switch kind {
	case security.DocumentSBOM:
		return "Anchore has no SBOM for this image yet. It is generated when analysis completes."
	case security.DocumentPolicy:
		return "Anchore has no policy evaluation for this image. " +
			"A policy must be active for the account, and the image must have been analysed."
	case security.DocumentMalware:
		return "Anchore reported no malware analysis for this image. " +
			"The malware analyser is off by default in Anchore."
	default:
		return "Anchore has nothing of this kind for this image."
	}
}

func describeKinds(kinds []security.DocumentKind) string {
	names := make([]string, 0, len(kinds))
	for _, k := range kinds {
		names = append(names, strings.ToLower(k.Label()))
	}
	switch len(names) {
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}

// ---------------------------------------------------------------------------
// The policy verdict, normalized
// ---------------------------------------------------------------------------

// policyEvaluation is the shape of GET /images/{digest}/check.
//
// # Why this is decoded so defensively
//
// Because the shape is genuinely awkward: the response is a list of objects
// keyed by image digest, then by tag, then a list of evaluations, each carrying
// a `detail.result.result` keyed by digest again, whose `result.rows` are
// POSITIONAL ARRAYS rather than objects. A struct cannot express it; a map and
// a documented set of indices can.
//
// The indices are Anchore's stable gate-result columns:
//
//	0 image id | 1 repo tag | 2 trigger id | 3 gate | 4 trigger
//	5 check output | 6 action | 7 policy id | ...
//
// A row shorter than that is skipped rather than guessed at, because a
// mis-indexed gate result would attribute one rule's verdict to another.
type policyEvaluation []map[string]map[string][]policyResult

// policyResult is one evaluation of one image against one policy.
type policyResult struct {
	Status string `json:"status"`
	Detail struct {
		Result struct {
			// Result is keyed by image digest AGAIN. Anchore's own shape, not
			// a mistake here.
			Result   map[string]policyGateResults `json:"result"`
			PolicyID string                       `json:"policyId"`
		} `json:"result"`
	} `json:"detail"`
}

// policyGateResults holds one policy's gate rows, positional as Anchore sends
// them.
type policyGateResults struct {
	Result struct {
		Header []string `json:"header"`
		Rows   [][]any  `json:"rows"`
	} `json:"result"`
}

// gate-result column indices. Named rather than inline, because `row[5]` in
// the middle of a loop is a number nobody can check.
const (
	colTriggerID = 2
	colGate      = 3
	colTrigger   = 4
	colOutput    = 5
	colAction    = 6
	colPolicyID  = 7
	colMinimum   = 7
)

// normalizePolicy turns Anchore's policy verdict into the platform's
// violations.
//
// Only STOP and WARN results become violations. A `go` row is the policy saying
// this image is fine on that gate, and a violations table with rows that are
// not violations is a table nobody trusts - the same mistake the Xray path
// documents avoiding.
func normalizePolicy(raw []byte) []security.Violation {
	var evaluations policyEvaluation
	if err := json.Unmarshal(raw, &evaluations); err != nil {
		// The raw body is still stored and still downloadable. Losing the
		// normalized table costs a tab; failing here would cost the download
		// as well.
		return nil
	}

	var out []security.Violation
	for _, byDigest := range evaluations {
		for _, byTag := range byDigest {
			for _, results := range byTag {
				for _, evaluation := range results {
					policyID := evaluation.Detail.Result.PolicyID
					for _, gate := range evaluation.Detail.Result.Result {
						for _, row := range gate.Result.Rows {
							if v, ok := violationFrom(row, policyID); ok {
								out = append(out, v)
							}
						}
					}
				}
			}
		}
	}
	return out
}

func violationFrom(row []any, policyID string) (security.Violation, bool) {
	if len(row) <= colMinimum {
		return security.Violation{}, false
	}
	action := strings.ToLower(cell(row, colAction))
	switch action {
	case "stop", "warn":
	default:
		return security.Violation{}, false
	}

	gate := cell(row, colGate)
	trigger := cell(row, colTrigger)
	output := cell(row, colOutput)
	policy := cell(row, colPolicyID)
	if policy == "" {
		policy = policyID
	}

	v := security.Violation{
		ID:       cell(row, colTriggerID),
		Type:     policyType(gate),
		Severity: policySeverity(action),
		Policy:   policy,
		Rule:     strings.TrimSpace(gate + ":" + trigger),
		Summary:  output,
		Provider: ProviderName,
	}
	// A vulnerability gate names the CVE in its output, and linking the gate to
	// the finding that tripped it is the difference between a row somebody can
	// act on and one they have to go looking for.
	v.CVE = cveIn(output)
	return v, true
}

// policyType grades a gate the way this platform's violations are typed, so an
// Anchore licence gate and an Xray licence violation land in the same bucket.
func policyType(gate string) string {
	switch strings.ToLower(strings.TrimSpace(gate)) {
	case "vulnerabilities", "malware":
		return "security"
	case "licenses", "license":
		return "license"
	case "dockerfile", "files", "packages", "metadata", "secret_scans", "always", "passwd_file":
		return "operational_risk"
	default:
		return "security"
	}
}

// policySeverity maps a gate action onto the platform's ladder.
//
// A `stop` is what blocks a release, so it grades high rather than critical:
// the ladder is about the vulnerability's severity everywhere else, and letting
// a policy action outrank a critical CVE would put a Dockerfile lint above an
// exploited remote-code-execution in every sorted list.
func policySeverity(action string) security.Severity {
	if strings.EqualFold(action, "stop") {
		return security.SeverityHigh
	}
	return security.SeverityMedium
}

// cell reads one positional column as a string, whatever JSON type it arrived
// as. Anchore sends numbers unquoted in some columns.
func cell(row []any, i int) string {
	if i >= len(row) || row[i] == nil {
		return ""
	}
	switch v := row[i].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strings.TrimSuffix(fmt.Sprintf("%.0f", v), ".0")
	case bool:
		return fmt.Sprintf("%t", v)
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", v))
	}
}

// cveIn pulls a CVE identifier out of a gate's output sentence.
func cveIn(s string) string {
	upper := strings.ToUpper(s)
	i := strings.Index(upper, "CVE-")
	if i < 0 {
		return ""
	}
	end := i
	for end < len(upper) && (upper[end] == '-' || upper[end] == '.' ||
		(upper[end] >= '0' && upper[end] <= '9') ||
		(upper[end] >= 'A' && upper[end] <= 'Z')) {
		end++
	}
	id := strings.Trim(upper[i:end], "-.")
	if len(id) < 8 {
		return ""
	}
	return id
}

// ---------------------------------------------------------------------------
// Malware
// ---------------------------------------------------------------------------

// malwareContent is the shape of GET /images/{digest}/content/malware.
type malwareContent struct {
	Content []struct {
		Scanner  string `json:"scanner"`
		Enabled  bool   `json:"enabled"`
		Findings []struct {
			Path      string `json:"path"`
			Signature string `json:"signature"`
		} `json:"findings"`
	} `json:"content"`
}

// normalizeMalware turns a malware scan into findings.
//
// Its own list rather than rows among the vulnerabilities, because it is read
// by a different person for a different reason: a vulnerability count is a
// backlog and a malware hit is a release that does not ship tonight, and
// burying one among ninety thousand is how it ships anyway.
func normalizeMalware(raw []byte, ref security.ArtifactRef) []security.Finding {
	var content malwareContent
	if err := json.Unmarshal(raw, &content); err != nil {
		return nil
	}
	var out []security.Finding
	for _, scanner := range content.Content {
		for _, f := range scanner.Findings {
			if f.Signature == "" && f.Path == "" {
				continue
			}
			out = append(out, security.Finding{
				ID: f.Signature,
				// Malware is not graded. Critical is the honest grade for a
				// known-bad signature in a shipped image, and there is no
				// scale on which it is anything else.
				Severity: security.SeverityCritical,
				Summary:  strings.TrimSpace(f.Signature + " in " + f.Path),
				Component: security.Component{
					Name: f.Path, Type: "file", Path: f.Path,
					ID: "file://" + f.Path,
				},
				Provider: ProviderName,
				Sources:  []string{ProviderName},
				Policy:   scanner.Scanner,
			})
		}
	}
	_ = ref
	return security.DedupeFindings(out)
}

var _ security.DocumentProvider = (*Provider)(nil)
