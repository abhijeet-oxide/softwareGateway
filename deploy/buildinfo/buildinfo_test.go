package buildinfo_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/deploy/buildinfo"
)

type stub map[string]buildinfo.Manifest

func (s stub) Manifest(_ context.Context, ref string) (buildinfo.Manifest, error) {
	m, ok := s[ref]
	if !ok {
		return buildinfo.Manifest{}, fmt.Errorf("not published")
	}
	return m, nil
}

func release() buildinfo.Release {
	return buildinfo.Release{
		Name: "software-gateway", Number: "0.1.8",
		Registry: "reg.example.internal", Repository: "proj",
		Started: time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC),
	}
}

func manifest(n int) buildinfo.Manifest {
	m := buildinfo.Manifest{
		Digest: fmt.Sprintf("sha256:%064x", n),
		Config: buildinfo.Blob{Digest: fmt.Sprintf("sha256:%064x", n+100)},
	}
	m.Layers = append(m.Layers, buildinfo.Blob{Digest: fmt.Sprintf("sha256:%064x", n+200)})
	return m
}

// TestAReleaseIsEveryComponentOrNothing is the invariant the whole tool exists
// for. A Build Info listing two images of three looks complete in the Builds
// view, and the one it omits is the one that failed to publish - so a component
// the registry does not have fails the publication instead of shrinking it.
func TestAReleaseIsEveryComponentOrNothing(t *testing.T) {
	comps := buildinfo.Components([]string{"coordinator", "worker", "web"}, "0.1.8", "software-gateway", "0.1.8")
	if len(comps) != 4 {
		t.Fatalf("three images and a chart is four components, got %d", len(comps))
	}

	full := stub{}
	for i, c := range comps {
		full[c.Ref("reg.example.internal", "proj")] = manifest(i + 1)
	}

	bi, err := buildinfo.Collect(t.Context(), full, release(), comps)
	if err != nil {
		t.Fatalf("a complete release must publish: %v", err)
	}
	if len(bi.Modules) != 4 {
		t.Errorf("published %d modules for a 4-component release", len(bi.Modules))
	}

	for _, drop := range comps {
		t.Run("without "+drop.Name, func(t *testing.T) {
			partial := stub{}
			for ref, m := range full {
				if ref != drop.Ref("reg.example.internal", "proj") {
					partial[ref] = m
				}
			}
			_, err := buildinfo.Collect(t.Context(), partial, release(), comps)
			if err == nil {
				t.Fatalf("a release missing %s was published as if it were whole", drop.Name)
			}
			if !strings.Contains(err.Error(), drop.Name) {
				t.Errorf("the error must name the missing component; got: %v", err)
			}
		})
	}
}

// TestTheChartCarriesItsOwnVersion guards the configuration-only release: the
// chart moves and the images do not, and a Build Info that tagged them all with
// one version would name images that do not exist.
func TestTheChartCarriesItsOwnVersion(t *testing.T) {
	comps := buildinfo.Components([]string{"coordinator"}, "0.1.7", "software-gateway", "0.1.9")
	byName := map[string]buildinfo.Component{}
	for _, c := range comps {
		byName[c.Name] = c
	}
	if got := byName["coordinator"].Version; got != "0.1.7" {
		t.Errorf("image version = %q, want the image version 0.1.7", got)
	}
	if got := byName["chart"].Version; got != "0.1.9" {
		t.Errorf("chart version = %q, want the chart version 0.1.9", got)
	}
	if got := byName["coordinator"].Ref("r.example", "proj"); got != "r.example/proj/software-gateway-coordinator:0.1.7" {
		t.Errorf("ref = %q", got)
	}
}

// TestTheDocumentIsShapedTheWayArtifactoryReads keeps the fields Artifactory
// renders from: a digest with its prefix in the property and without it in the
// artifact sha256, and a timestamp in the format it accepts rather than RFC3339,
// which it answers with a 400 that names no field.
func TestTheDocumentIsShapedTheWayArtifactoryReads(t *testing.T) {
	comps := buildinfo.Components([]string{"worker"}, "0.1.8", "", "")
	s := stub{comps[0].Ref("reg.example.internal", "proj"): manifest(7)}
	bi, err := buildinfo.Collect(t.Context(), s, release(), comps)
	if err != nil {
		t.Fatal(err)
	}
	m := bi.Modules[0]
	if m.Type != "docker" || m.ID != "software-gateway-worker:0.1.8" {
		t.Errorf("module = %s %s", m.Type, m.ID)
	}
	if !strings.HasPrefix(m.Properties["docker.image.id"], "sha256:") {
		t.Errorf("docker.image.id must carry its prefix, got %q", m.Properties["docker.image.id"])
	}
	for _, a := range m.Artifacts {
		if strings.HasPrefix(a.SHA256, "sha256:") {
			t.Errorf("artifact %s sha256 must not carry the prefix, got %q", a.Name, a.SHA256)
		}
	}
	// manifest + config + one layer
	if len(m.Artifacts) != 3 {
		t.Errorf("artifacts = %d, want manifest, config and one layer", len(m.Artifacts))
	}
	if bi.Started != "2026-09-15T10:00:00.000+0000" {
		t.Errorf("started = %q, which Artifactory rejects", bi.Started)
	}
}

// TestPublishSendsWhatItSays covers the request, including the credential going
// in a header rather than a query parameter.
func TestPublishSendsWhatItSays(t *testing.T) {
	var gotPath, gotAuth, gotQuery string
	var gotBody buildinfo.BuildInfo
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotQuery = r.URL.Path, r.Header.Get("Authorization"), r.URL.RawQuery
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := &buildinfo.Client{BaseURL: srv.URL, Username: "ci", Token: "tok", HTTP: srv.Client()}
	if err := c.Publish(t.Context(), &buildinfo.BuildInfo{Name: "software-gateway", Number: "0.1.8"}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/build" {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "Basic ") {
		t.Errorf("auth = %q", gotAuth)
	}
	if strings.Contains(gotQuery, "tok") {
		t.Errorf("the token reached the query string: %q", gotQuery)
	}
	if gotBody.Number != "0.1.8" {
		t.Errorf("body number = %q", gotBody.Number)
	}
}

// TestARefusedCredentialSaysWhichPermission: a 403 here is almost always deploy
// permission on the build, which is granted separately from pushing images, and
// the raw status says none of that.
func TestARefusedCredentialSaysWhichPermission(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := &buildinfo.Client{BaseURL: srv.URL, Username: "ci", Token: "tok", HTTP: srv.Client()}
	err := c.Publish(t.Context(), &buildinfo.BuildInfo{Name: "n", Number: "1"})
	if err == nil || !strings.Contains(err.Error(), "permission") {
		t.Errorf("a 403 must name the permission; got %v", err)
	}
}

// TestPromotionCopiesRatherThanMoves: a promotion that empties the staging
// repository takes the release away from anything still pulling it.
func TestPromotionCopiesRatherThanMoves(t *testing.T) {
	var got buildinfo.Promotion
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := &buildinfo.Client{BaseURL: srv.URL, Username: "ci", Token: "tok", HTTP: srv.Client()}
	err := c.Promote(t.Context(), "software-gateway", "0.1.8", buildinfo.Promotion{
		Status: "Released", SourceRepo: "oci-stage", TargetRepo: "oci-release", Copy: true, Artifacts: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/api/build/promote/software-gateway/0.1.8" {
		t.Errorf("path = %q", path)
	}
	if !got.Copy {
		t.Error("promotion must copy: moving empties the repository anything running the release still pulls from")
	}
	if got.Dependencies {
		t.Error("dependencies must not be promoted: they belong to the repositories they came from")
	}
}

// TestPromotionNeedsSomewhereToGo.
func TestPromotionNeedsSomewhereToGo(t *testing.T) {
	c := &buildinfo.Client{BaseURL: "https://example.invalid"}
	if err := c.Promote(t.Context(), "n", "1", buildinfo.Promotion{}); err == nil {
		t.Error("a promotion with no target must not be sent")
	}
}
