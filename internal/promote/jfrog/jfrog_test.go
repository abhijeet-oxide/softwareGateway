package jfrog

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/promote"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
)

// What this plugin gets wrong is expensive in two specific ways, and both are
// silent: promoting with `copy:false` empties the source, and getting the
// repository key wrong 404s in a way that reads like a missing image. Both are
// pinned here.

func hop(origin, destination promote.Endpoint, names ...promote.Name) promote.Hop {
	if len(names) == 0 {
		names = []promote.Name{{Repository: "orbs/cfx", Tag: "v1", Digest: "sha256:aa"}}
	}
	return promote.Hop{
		Product: "nokia", Package: "v1", ManifestDigest: "sha256:aa",
		Origin: origin, Destination: destination, Names: names,
	}
}

func jfrogEnd(name, host, repo string, opts ...map[string]string) promote.Endpoint {
	e := promote.Endpoint{
		Name: name, Registry: host, Repository: repo, RegistryType: "jfrog",
	}
	if len(opts) > 0 {
		e.Options = opts[0]
	}
	return e
}

func promoterFor(t *testing.T, h promote.Hop) *Promoter {
	t.Helper()
	p, err := New(promote.Config{
		Origin:      h.Origin,
		Destination: h.Destination,
		OriginClient: registry.ClientConfig{
			Username: "svc", Password: "token", PlainHTTP: true,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p.(*Promoter)
}

func TestClaimsTwoRepositoriesOfOneArtifactory(t *testing.T) {
	h := hop(
		jfrogEnd("lab", "acme.jfrog.io", "docker-lab/nokia"),
		jfrogEnd("production", "acme.jfrog.io", "docker-prod/nokia"),
	)
	v := promoterFor(t, h).Claim(h)
	if !v.Claimed {
		t.Fatalf("two repositories of one Artifactory must be claimed: %s", v.Reason)
	}
	if v.Reason == "" {
		t.Error("a claim must still say what it is going to do")
	}
}

// The constraint that makes the fast path exist at all. Two hosts is two
// Artifactories, and there is nothing internal about that.
func TestDeclinesTwoHosts(t *testing.T) {
	h := hop(
		jfrogEnd("lab", "eu.jfrog.io", "docker-lab/nokia"),
		jfrogEnd("production", "us.jfrog.io", "docker-prod/nokia"),
	)
	v := promoterFor(t, h).Claim(h)
	if v.Claimed {
		t.Fatal("two Artifactory hosts must not be claimed")
	}
	// The refusal is the diagnosis, and an operator who configured two hosts
	// by mistake should read it rather than wait out a 45 GB transfer. It
	// names the ORIGIN and not the hosts: both are already on screen beside
	// it, and repeating them is a sentence the reader parses to learn what
	// they can see.
	if !strings.Contains(v.Reason, "lab") {
		t.Errorf("the reason must name the origin it differs from; got %q", v.Reason)
	}
	if strings.Contains(v.Reason, "us.jfrog.io") {
		t.Errorf("the reason repeats a host the row already shows: %q", v.Reason)
	}
}

// A target configured `generic` against an Artifactory is deliberately not
// claimed. Configuration says what a deployment intends to speak, and
// inferring JFrog from a hostname would make the fast path depend on DNS.
func TestDeclinesWhenEitherEndIsNotDeclaredJFrog(t *testing.T) {
	lab := jfrogEnd("lab", "acme.jfrog.io", "docker-lab/nokia")
	prod := jfrogEnd("production", "acme.jfrog.io", "docker-prod/nokia")
	prod.RegistryType = "generic"

	h := hop(lab, prod)
	if v := promoterFor(t, h).Claim(h); v.Claimed {
		t.Fatal("a generic destination must not be claimed")
	}
}

func TestArtifactoryIsTheSameBackendAsJFrog(t *testing.T) {
	lab := jfrogEnd("lab", "acme.jfrog.io", "docker-lab/nokia")
	lab.RegistryType = "artifactory"
	h := hop(lab, jfrogEnd("production", "acme.jfrog.io", "docker-prod/nokia"))

	if v := promoterFor(t, h).Claim(h); !v.Claimed {
		t.Fatalf("`artifactory` and `jfrog` name one backend: %s", v.Reason)
	}
}

// A subdomain deployment puts the repository key in the HOSTNAME, so there is
// nothing in the path to derive it from. Refusing with a message naming the
// field is the whole of the answer; guessing would produce a 404 that reads
// like a missing release.
func TestDeclinesWhenTheRepositoryKeyCannotBeDerived(t *testing.T) {
	h := hop(
		jfrogEnd("lab", "acme-lab.jfrog.io", ""),
		jfrogEnd("production", "acme-lab.jfrog.io", "docker-prod"),
	)
	v := promoterFor(t, h).Claim(h)
	if v.Claimed {
		t.Fatal("a target with no path and no key must not be claimed")
	}
	if !strings.Contains(v.Reason, OptionRepositoryKey) {
		t.Errorf("the reason must name the field that settles it; got %q", v.Reason)
	}
}

func TestAnExplicitRepositoryKeyIsEnoughOnASubdomainDeployment(t *testing.T) {
	h := hop(
		jfrogEnd("lab", "acme-docker.jfrog.io", "nokia/orbs",
			map[string]string{OptionRepositoryKey: "docker-lab"}),
		jfrogEnd("production", "acme-docker.jfrog.io", "nokia/orbs",
			map[string]string{OptionRepositoryKey: "docker-prod"}),
	)
	if v := promoterFor(t, h).Claim(h); !v.Claimed {
		t.Fatalf("an explicit key must settle a subdomain deployment: %s", v.Reason)
	}
}

// Two targets that resolve to one JFrog coordinate are one place under two
// names, and promoting between them would be a no-op reported as a success.
func TestDeclinesAHopThatDoesNotMove(t *testing.T) {
	h := hop(
		jfrogEnd("lab", "acme.jfrog.io", "docker-lab/nokia"),
		jfrogEnd("lab-alias", "acme.jfrog.io", "docker-lab/nokia"),
	)
	if v := promoterFor(t, h).Claim(h); v.Claimed {
		t.Fatal("a hop between one repository and itself must not be claimed")
	}
}

// THE ONE THAT MATTERS. JFrog's promote MOVES by default: `copy:false` would
// delete the release from lab at the moment production was filled.
func TestPromoteAlwaysCopiesAndNeverMoves(t *testing.T) {
	var body promoteRequest
	var path string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	host := hostOnly(t, srv.URL)
	h := hop(
		jfrogEnd("lab", host, "docker-lab/nokia"),
		jfrogEnd("production", host, "docker-prod/nokia"),
		promote.Name{Repository: "orbs/cfx", Tag: "v1", Digest: "sha256:aa"},
	)

	out, err := promoterFor(t, h).Promote(t.Context(), h)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if out.Promoted != 1 {
		t.Errorf("promoted %d names, want 1", out.Promoted)
	}

	if !body.Copy {
		t.Fatal("copy must be true: JFrog's default is a MOVE, which would empty lab")
	}

	// The repository key is the URL segment and is NOT repeated in the docker
	// repository. `docker-lab/docker-lab/nokia/orbs/cfx` 404s in a way that
	// reads like a missing image rather than a wrong path.
	if want := "/artifactory/api/docker/docker-lab/v2/promote"; path != want {
		t.Errorf("posted to %q, want %q", path, want)
	}
	if want := "nokia/orbs/cfx"; body.DockerRepository != want {
		t.Errorf("dockerRepository %q, want %q", body.DockerRepository, want)
	}
	if want := "nokia/orbs/cfx"; body.TargetDockerRepository != want {
		t.Errorf("targetDockerRepository %q, want %q", body.TargetDockerRepository, want)
	}
	if body.TargetRepo != "docker-prod" {
		t.Errorf("targetRepo %q, want docker-prod", body.TargetRepo)
	}
	if body.Tag != "v1" || body.TargetTag != "v1" {
		t.Errorf("tag %q -> %q, want v1 -> v1", body.Tag, body.TargetTag)
	}
}

// The destination's own prefix, not the origin's. Copying the origin path
// would nest lab inside production - the mistake the whole re-basing rule
// exists to prevent.
func TestTheDestinationPathIsRebasedNotCopied(t *testing.T) {
	var body promoteRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	host := hostOnly(t, srv.URL)
	h := hop(
		jfrogEnd("lab", host, "docker-lab/nokia-lab"),
		jfrogEnd("production", host, "docker-prod/nokia-prod"),
		promote.Name{Repository: "orbs/cfx", Tag: "v1", Digest: "sha256:aa"},
	)
	if _, err := promoterFor(t, h).Promote(t.Context(), h); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	if want := "nokia-lab/orbs/cfx"; body.DockerRepository != want {
		t.Errorf("read from %q, want %q", body.DockerRepository, want)
	}
	if want := "nokia-prod/orbs/cfx"; body.TargetDockerRepository != want {
		t.Errorf("wrote to %q, want %q - the origin's prefix must not travel", body.TargetDockerRepository, want)
	}
}

// A 403 here is nearly always a credential that can pull and push over the
// docker endpoint and still lacks the Artifactory permission promotion needs.
// "Forbidden" alone sends people to look at the wrong thing.
func TestForbiddenNamesThePermissionThatIsMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":[{"status":403,"message":"Forbidden"}]}`))
	}))
	t.Cleanup(srv.Close)

	host := hostOnly(t, srv.URL)
	h := hop(
		jfrogEnd("lab", host, "docker-lab/nokia"),
		jfrogEnd("production", host, "docker-prod/nokia"),
	)
	_, err := promoterFor(t, h).Promote(t.Context(), h)
	if err == nil {
		t.Fatal("a 403 must fail the promotion")
	}
	if !errors.Is(err, registry.ErrForbidden) {
		t.Errorf("must classify as forbidden for the shared retry policy: %v", err)
	}
	if !strings.Contains(err.Error(), "promote between them") {
		t.Errorf("the message must name the permission that is missing: %v", err)
	}
}

// Some Artifactory versions answer a bare, undiagnosed 400 to a tag-scoped
// promote request against a packageType=oci/federated repository. When the
// source repository holds nothing but tags this hop already intends to
// publish, a repository-level retry (no `tag`/`targetTag`) is safe and must
// be tried automatically.
func TestUndiagnosedBadRequestFallsBackWhenSafe(t *testing.T) {
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/tags/list"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"tags":["v1"]}`))
		case r.Method == http.MethodPost:
			posts++
			raw, _ := io.ReadAll(r.Body)
			var body promoteRequest
			_ = json.Unmarshal(raw, &body)
			if body.Tag == "" {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"status":200,"messages":[{"status":"info","message":"Promotion ended successfully"}]}`))
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errors":[{"status":400,"message":"Bad Request"}]}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	host := hostOnly(t, srv.URL)
	h := hop(
		jfrogEnd("lab", host, "docker-lab/nokia"),
		jfrogEnd("production", host, "docker-prod/nokia"),
	)
	out, err := promoterFor(t, h).Promote(t.Context(), h)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if out.Promoted != 1 {
		t.Errorf("Promoted = %d, want 1", out.Promoted)
	}
	if posts != 2 {
		t.Errorf("expected the tag-scoped call and one repository-level retry, got %d POSTs", posts)
	}
}

// The same undiagnosed 400, but the source repository also holds a tag this
// hop never asked to publish. A repository-level retry would carry that tag
// along uninvited, so Promote must refuse rather than fall back, and must
// name the offending tag.
func TestUndiagnosedBadRequestRefusesFallbackWhenUnsafe(t *testing.T) {
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/tags/list"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"tags":["v1","v2-not-part-of-this-hop"]}`))
		case r.Method == http.MethodPost:
			posts++
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errors":[{"status":400,"message":"Bad Request"}]}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	host := hostOnly(t, srv.URL)
	h := hop(
		jfrogEnd("lab", host, "docker-lab/nokia"),
		jfrogEnd("production", host, "docker-prod/nokia"),
	)
	_, err := promoterFor(t, h).Promote(t.Context(), h)
	if err == nil {
		t.Fatal("a repository holding an unrelated tag must not be promoted whole")
	}
	if !strings.Contains(err.Error(), "v2-not-part-of-this-hop") {
		t.Errorf("the message must name the tag this hop did not ask to publish: %v", err)
	}
	if posts != 1 {
		t.Errorf("the repository-level fallback must not be attempted when unsafe; got %d POSTs", posts)
	}
}

// Even when no safe fallback exists (a fresh repository, nothing to compare
// against), the message itself must explain that Artifactory gave nothing
// beyond the status code, and name the known cause.
func TestUndiagnosedBadRequestNamesTheKnownCause(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/tags/list"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"status":404,"message":"repo does not exist"}]}`))
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errors":[{"status":400,"message":"Bad Request"}]}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	host := hostOnly(t, srv.URL)
	h := hop(
		jfrogEnd("lab", host, "docker-lab/nokia"),
		jfrogEnd("production", host, "docker-prod/nokia"),
	)
	_, err := promoterFor(t, h).Promote(t.Context(), h)
	if err == nil {
		t.Fatal("an undiagnosed 400 must fail the promotion")
	}
	if !strings.Contains(err.Error(), "packageType=oci") {
		t.Errorf("the message must name the known cause: %v", err)
	}
}

// An anonymous target cannot promote, and finding that out here beats finding
// it out as a 401 that reads like a rotated password.
func TestAnAnonymousTargetIsRefusedBeforeAnyRequest(t *testing.T) {
	h := hop(
		jfrogEnd("lab", "acme.jfrog.io", "docker-lab/nokia"),
		jfrogEnd("production", "acme.jfrog.io", "docker-prod/nokia"),
	)
	p, err := New(promote.Config{
		Origin: h.Origin, Destination: h.Destination,
		OriginClient: registry.ClientConfig{PlainHTTP: true},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.Promote(t.Context(), h); !errors.Is(err, registry.ErrUnauthorized) {
		t.Fatalf("want an unauthorized error, got %v", err)
	}
}

func TestTrimKey(t *testing.T) {
	cases := []struct{ configured, key, want string }{
		// Repository-path deployment: the key IS the first segment.
		{"docker-prod/nokia/orbs", "docker-prod", "nokia/orbs"},
		// The key alone, so the docker repository is the root.
		{"docker-prod", "docker-prod", ""},
		// Subdomain deployment: the key is in the host and nothing is trimmed.
		{"nokia/orbs", "docker-prod", "nokia/orbs"},
		// A prefix that merely starts with the key is not the key.
		{"docker-production/nokia", "docker-prod", "docker-production/nokia"},
		{"/docker-prod/nokia/", "docker-prod", "nokia"},
	}
	for _, c := range cases {
		if got := trimKey(c.configured, c.key); got != c.want {
			t.Errorf("trimKey(%q, %q) = %q, want %q", c.configured, c.key, got, c.want)
		}
	}
}

func hostOnly(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Host
}
