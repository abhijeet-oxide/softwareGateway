package anchore

import (
	"encoding/json"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/security"
)

// The normalizer is where a quirk becomes a wrong number on a page. These are
// the ones that would be silent.

// Anchore reports both a vendor grading and an NVD one, and the platform must
// report the harsher of them without discarding the other.
func TestWorstSeverityIsReportedAndBothAreKept(t *testing.T) {
	var res imageVulnerabilities
	if err := json.Unmarshal([]byte(`{
	  "vulnerabilities": [{
	    "vuln": "CVE-2024-5", "severity": "Low", "fix": "None",
	    "package_name": "openssl", "package_version": "1.1.1n", "package_type": "dpkg",
	    "feed": "vulnerabilities", "feed_group": "debian:11",
	    "nvd_data": [{"id": "CVE-2024-5", "source": "nvd", "cvss_v3": {"base_score": 9.8}}],
	    "vendor_data": [{"id": "CVE-2024-5", "source": "debian", "cvss_v3": {"base_score": 3.1}}]
	  }]
	}`), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}

	findings := normalizeImage(res)
	if len(findings) != 1 {
		t.Fatalf("expected one finding, got %d", len(findings))
	}
	f := findings[0]
	// Anchore's own headline grade is the only one carrying a severity WORD,
	// so it is what the platform reports; the scores travel as observations.
	if f.Severity != security.SeverityLow {
		t.Errorf("severity = %q, want the grade Anchore reported", f.Severity)
	}
	if f.CVSSScore != 9.8 {
		t.Errorf("CVSS = %v, want the highest of the two gradings", f.CVSSScore)
	}
	if len(f.Observations) != 3 {
		t.Fatalf("expected Anchore's grade plus both gradings, got %d", len(f.Observations))
	}
	if f.Fixable {
		t.Error(`fix "None" was read as a fixed version - the bug that turns 4 fixable into 900`)
	}
}

// The component identity is what a cross-scanner merge aligns on, so it must
// be the shared vocabulary and must not carry a version.
func TestComponentIdentityIsTheSharedVocabulary(t *testing.T) {
	cases := map[string]string{
		"dpkg":       "deb://openssl",
		"APKG":       "apk://openssl",
		"java":       "maven://openssl",
		"go-module":  "go://openssl",
		"":           "generic://openssl",
		"some-newer": "some-newer://openssl",
	}
	for in, want := range cases {
		got := componentOf(packageVuln{PackageName: "openssl", PackageVersion: "1.0", PackageType: in})
		if got.ID != want {
			t.Errorf("package type %q -> component %q, want %q", in, got.ID, want)
		}
	}
}

// The application-version report is a different shape and must produce findings
// keyed by the image each match landed in.
func TestVersionReportIsKeyedByImage(t *testing.T) {
	var res versionVulnerabilities
	if err := json.Unmarshal([]byte(`{
	  "application": {"name": "cfx", "version_name": "25.7"},
	  "vulnerabilities": [{
	    "id": "CVE-2024-7",
	    "vendor_data": {"severity": "High", "feed": "vulnerabilities", "group": "debian:11",
	                    "will_not_fix": true, "description": "the vendor's paragraph"},
	    "nvd": [{"id": "CVE-2024-7", "severity": "Critical",
	             "cvss": {"cvss_v3": {"base_score": 9.1}}}],
	    "matches": [
	      {"fix": "2.0", "location": {"artifact": {"id": "sha256:aaa", "type": "image"},
	                                  "package": {"name": "zlib", "type": "dpkg", "version": "1.2"}}},
	      {"fix": "None", "location": {"artifact": {"id": "sha256:bbb", "type": "image"},
	                                   "package": {"name": "zlib", "type": "dpkg", "version": "1.1"}}}
	    ]
	  }]
	}`), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}

	byImage := normalizeVersionReport(res)
	if len(byImage) != 2 {
		t.Fatalf("expected findings for two images, got %d", len(byImage))
	}
	fixed := byImage["sha256:aaa"]
	if len(fixed) != 1 || !fixed[0].Fixable {
		t.Errorf("the match with a fix version is not fixable: %+v", fixed)
	}
	unfixed := byImage["sha256:bbb"]
	if len(unfixed) != 1 || unfixed[0].Fixable {
		t.Errorf(`the match whose fix is "None" reported as fixable: %+v`, unfixed)
	}
	// The worst grading across the two sources, and the vendor's refusal.
	if fixed[0].Severity != security.SeverityCritical {
		t.Errorf("severity = %q, want the worse of vendor and NVD", fixed[0].Severity)
	}
	if !fixed[0].WillNotFix {
		t.Error("lost the vendor's will-not-fix position")
	}
}

// A GHSA identifier is not a CVE and upper-casing one produces a string nothing
// else matches.
func TestOnlyCVEsAreUpperCased(t *testing.T) {
	if got := upperCVE("cve-2024-1"); got != "CVE-2024-1" {
		t.Errorf("upperCVE of a CVE = %q", got)
	}
	if got := upperCVE("GHSA-xxxx-yyyy"); got != "" {
		t.Errorf("upperCVE of a GHSA = %q, want empty so the id field carries it", got)
	}
}

// The pull string names the INTERNAL registry, because Anchore cannot reach the
// vendor's.
func TestPullStringUsesTheInternalLocation(t *testing.T) {
	ref := security.ArtifactRef{
		Name: "app", Digest: "sha256:aaa", Registry: "vendor.example.com", Repository: "vendor/app",
	}
	located := withLocation(ref, Settings{Registry: "internal.example.com", Repository: "orbs"})
	pull, err := PullString(located)
	if err != nil {
		t.Fatalf("pull string: %v", err)
	}
	if pull != "internal.example.com/orbs/vendor/app@sha256:aaa" {
		t.Errorf("pull string = %q", pull)
	}

	// An artifact that already sits under the configured root keeps its path
	// rather than gaining a second copy of it.
	located = withLocation(security.ArtifactRef{
		Digest: "sha256:aaa", Repository: "orbs/vendor/app",
	}, Settings{Registry: "internal.example.com", Repository: "orbs"})
	pull, _ = PullString(located)
	if pull != "internal.example.com/orbs/vendor/app@sha256:aaa" {
		t.Errorf("pull string doubled the root: %q", pull)
	}
}

func TestOnlyContainerImagesAreScannable(t *testing.T) {
	if !scannable(security.ArtifactRef{Kind: "image", MediaType: "application/vnd.oci.image.manifest.v1+json"}) {
		t.Fatal("container image was excluded from Anchore")
	}
	if scannable(security.ArtifactRef{Kind: "index", MediaType: "application/vnd.oci.image.index.v1+json"}) {
		t.Fatal("OCI image index was submitted to Anchore")
	}
}

// A policy gate that passed is not a violation, and a table of them would be a
// table nobody trusts.
func TestOnlyFailingGatesBecomeViolations(t *testing.T) {
	raw := []byte(`[{
	  "sha256:aaa": {"internal.example.com/app:1.0": [{
	    "status": "fail",
	    "detail": {"result": {"policyId": "default", "result": {"sha256:aaa": {"result": {
	      "header": ["Image_Id","Repo_Tag","Trigger_Id","Gate","Trigger","Check_Output","Gate_Action","Policy_Id"],
	      "rows": [
	        ["id","tag","t1","vulnerabilities","package","CVE-2024-9 in openssl","stop","default"],
	        ["id","tag","t2","dockerfile","instruction","no HEALTHCHECK","warn","default"],
	        ["id","tag","t3","licenses","package","fine","go","default"]
	      ]
	    }}}}}
	  }]}
	}]`)

	violations := normalizePolicy(raw)
	if len(violations) != 2 {
		t.Fatalf("expected the stop and the warn only, got %d: %+v", len(violations), violations)
	}
	if violations[0].CVE != "CVE-2024-9" {
		t.Errorf("the vulnerability gate did not link to its CVE: %q", violations[0].CVE)
	}
	if violations[0].Type != "security" || violations[1].Type != "operational_risk" {
		t.Errorf("gates typed wrongly: %q and %q", violations[0].Type, violations[1].Type)
	}
}
