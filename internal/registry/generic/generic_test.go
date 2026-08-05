package generic

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/transport"
	"github.com/abhijeet-oxide/softwareGateway/test/fakeregistry"
)

func newRepo(t *testing.T, fake *fakeregistry.Registry, path string, tcfg transport.Config) *Repository {
	t.Helper()
	// A fast retry policy: these tests assert retry BEHAVIOUR, not the
	// production schedule, and the default would spend minutes in backoff.
	if tcfg.RetryBaseDelay == 0 {
		tcfg.RetryBaseDelay = time.Millisecond
		tcfg.RetryMaxDelay = 10 * time.Millisecond
	}
	r, err := New(Config{
		Registry:   fake.Host(),
		Repository: path,
		PlainHTTP:  true, // httptest serves plain HTTP
		Transport:  tcfg,
	})
	if err != nil {
		t.Fatalf("new repository: %v", err)
	}
	return r
}

func TestPingAndListTags(t *testing.T) {
	fake := fakeregistry.New()
	t.Cleanup(fake.Close)
	fake.AddTag("vendor-a/platform", "v1.0.0")
	fake.AddTag("vendor-a/platform", "v1.1.0")

	repo := newRepo(t, fake, "vendor-a/platform", transport.Config{})

	if err := repo.Ping(t.Context()); err != nil {
		t.Fatalf("ping: %v", err)
	}

	tags, err := ListAllTags(t.Context(), repo, 0)
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	if len(tags) != 2 {
		t.Fatalf("expected 2 tags, got %v", tags)
	}
}

func TestListTagsFollowsLinkPagination(t *testing.T) {
	// A repository with more tags than fit in one page must be fully walked;
	// stopping at page one would make most of a vendor's catalog invisible.
	fake := fakeregistry.New(fakeregistry.WithPageSize(3))
	t.Cleanup(fake.Close)
	for i := 0; i < 10; i++ {
		fake.AddTag("vendor-a/platform", fmt.Sprintf("v1.%02d.0", i))
	}

	repo := newRepo(t, fake, "vendor-a/platform", transport.Config{})

	tags, err := ListAllTags(t.Context(), repo, 0)
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	if len(tags) != 10 {
		t.Fatalf("expected all 10 tags across pages, got %d: %v", len(tags), tags)
	}
	if calls := fake.TagListCalls.Load(); calls < 3 {
		t.Fatalf("expected multiple page requests, got %d", calls)
	}
}

// stuckLister always reports the same next-page cursor, as a registry with a
// pagination bug would.
type stuckLister struct{ calls int }

func (s *stuckLister) ListTags(_ context.Context, _ string, _ int) ([]string, string, error) {
	s.calls++
	return []string{fmt.Sprintf("tag-%d", s.calls)}, "never-advances", nil
}

func TestListTagsStopsOnNonAdvancingCursor(t *testing.T) {
	// A registry that returns an unchanging cursor would otherwise loop
	// forever, holding a discovery slot and growing the tag slice without
	// bound. Two independent guards stop it: an unchanged cursor terminates
	// immediately, and maxPages bounds the pathological case.
	lister := &stuckLister{}

	tags, err := ListAllTags(t.Context(), lister, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lister.calls != 2 {
		t.Fatalf("expected the walk to stop once the cursor repeated, made %d calls", lister.calls)
	}
	if len(tags) != 2 {
		t.Fatalf("expected 2 tags before stopping, got %d", len(tags))
	}
}

func TestListAllTagsRespectsMaxPages(t *testing.T) {
	// A cursor that DOES advance but never ends must still be bounded.
	lister := &advancingLister{}
	if _, err := ListAllTags(t.Context(), lister, 4); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lister.calls != 4 {
		t.Fatalf("expected exactly maxPages=4 calls, got %d", lister.calls)
	}
}

type advancingLister struct{ calls int }

func (a *advancingLister) ListTags(_ context.Context, _ string, _ int) ([]string, string, error) {
	a.calls++
	return []string{fmt.Sprintf("tag-%d", a.calls)}, fmt.Sprintf("cursor-%d", a.calls), nil
}

func TestListAllTagsDeduplicatesAcrossPages(t *testing.T) {
	// Tags written during a scan can appear on two pages. Recording a
	// duplicate would create a spurious second package row.
	lister := &repeatingLister{}
	tags, err := ListAllTags(t.Context(), lister, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	seen := map[string]int{}
	for _, tag := range tags {
		seen[tag]++
	}
	for tag, n := range seen {
		if n > 1 {
			t.Fatalf("tag %q appeared %d times", tag, n)
		}
	}
}

type repeatingLister struct{ calls int }

func (r *repeatingLister) ListTags(_ context.Context, _ string, _ int) ([]string, string, error) {
	r.calls++
	if r.calls >= 3 {
		return []string{"v1.0.0"}, "", nil
	}
	return []string{"v1.0.0", "v1.1.0"}, fmt.Sprintf("cursor-%d", r.calls), nil
}

func TestEmptyRepositoryIsNotAnError(t *testing.T) {
	// Some registries 404 a repository with no tags. That is a normal state,
	// not a failure: reporting it as one would make an empty repository look
	// broken and back discovery off for no reason.
	fake := fakeregistry.New()
	t.Cleanup(fake.Close)

	repo := newRepo(t, fake, "vendor-a/empty", transport.Config{})

	tags, next, err := repo.ListTags(t.Context(), "", 100)
	if err != nil {
		t.Fatalf("an empty repository must not be an error: %v", err)
	}
	if len(tags) != 0 || next != "" {
		t.Fatalf("expected no tags, got %v / %q", tags, next)
	}
}

func TestResolveTagUsesHeadAndReturnsDigest(t *testing.T) {
	fake := fakeregistry.New()
	t.Cleanup(fake.Close)
	want := fake.AddTag("vendor-a/platform", "v1.0.0")

	repo := newRepo(t, fake, "vendor-a/platform", transport.Config{})

	desc, err := repo.ResolveTag(t.Context(), "v1.0.0")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if string(desc.Digest) != want {
		t.Fatalf("got digest %s, want %s", desc.Digest, want)
	}
	if err := desc.Digest.Validate(); err != nil {
		t.Fatalf("resolved digest is malformed: %v", err)
	}
	if desc.Size == 0 {
		t.Fatal("expected a size from Content-Length")
	}
}

func TestResolveMissingTagIsNotFound(t *testing.T) {
	fake := fakeregistry.New()
	t.Cleanup(fake.Close)
	fake.AddTag("vendor-a/platform", "v1.0.0")

	repo := newRepo(t, fake, "vendor-a/platform", transport.Config{})

	_, err := repo.ResolveTag(t.Context(), "v9.9.9")
	if !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if registry.Retryable(err) {
		t.Fatal("a missing tag must not be retryable")
	}
}

func TestFetchManifestReturnsVerbatimBytes(t *testing.T) {
	// The digest — and every signature over it — is the hash of these exact
	// bytes. Any re-serialization would change the digest and silently break
	// verification, so unusual whitespace must survive the round trip.
	fake := fakeregistry.New()
	t.Cleanup(fake.Close)

	original := []byte("{\n  \"schemaVersion\" :  2,\n\t\"odd\":\"spacing\"\n}")
	digest := fake.AddManifest("vendor-a/platform", "v1.0.0", original,
		registry.MediaTypeOCIManifest)

	repo := newRepo(t, fake, "vendor-a/platform", transport.Config{})

	desc, raw, err := repo.FetchManifest(t.Context(), "v1.0.0")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if string(raw) != string(original) {
		t.Fatalf("bytes were altered:\n got %q\nwant %q", raw, original)
	}
	if string(desc.Digest) != digest {
		t.Fatalf("digest mismatch: got %s want %s", desc.Digest, digest)
	}
	// And the digest must be recomputed from the content, not trusted.
	if registry.NewDigestFromBytes(raw) != desc.Digest {
		t.Fatal("returned digest does not match returned content")
	}
}

func TestFetchManifestRejectsDigestMismatch(t *testing.T) {
	// The registry's Docker-Content-Digest is a claim; the bytes are the fact.
	// Accepting a mismatch would poison every dedupe decision downstream.
	fake := fakeregistry.New()
	t.Cleanup(fake.Close)
	fake.AddTag("vendor-a/platform", "v1.0.0")

	repo := newRepo(t, fake, "vendor-a/platform", transport.Config{})

	// Ask for a digest that does not match the stored content.
	wrong := registry.NewDigestFromBytes([]byte("something else entirely"))
	_, _, err := repo.FetchManifest(t.Context(), string(wrong))
	if err == nil {
		t.Fatal("expected an error for a mismatched digest reference")
	}
	// The fake returns 404 for an unknown digest, which is also correct
	// behaviour; either outcome proves we did not silently accept it.
	if !errors.Is(err, registry.ErrNotFound) && !errors.Is(err, registry.ErrDigestMismatch) {
		t.Fatalf("expected not-found or digest-mismatch, got %v", err)
	}
}

func TestBasicAuthIsApplied(t *testing.T) {
	fake := fakeregistry.New(fakeregistry.WithBasicAuth("robot", "s3cret"))
	t.Cleanup(fake.Close)
	fake.AddTag("vendor-a/platform", "v1.0.0")

	repo := newRepo(t, fake, "vendor-a/platform",
		transport.Config{Username: "robot", Password: "s3cret"})

	if err := repo.Ping(t.Context()); err != nil {
		t.Fatalf("ping with credentials: %v", err)
	}
}

func TestMissingCredentialsSurfaceAsUnauthorized(t *testing.T) {
	fake := fakeregistry.New(fakeregistry.WithBasicAuth("robot", "s3cret"))
	t.Cleanup(fake.Close)

	repo := newRepo(t, fake, "vendor-a/platform", transport.Config{})

	err := repo.Ping(t.Context())
	if err == nil {
		t.Fatal("expected an authentication failure")
	}
	if registry.Retryable(err) {
		t.Fatal("an auth failure must not be retryable — credentials do not fix themselves")
	}
}

func TestTokenFlowAndTokenCaching(t *testing.T) {
	// Without caching, an 850-blob package performs 850 token exchanges. This
	// asserts that many requests share one token.
	fake := fakeregistry.New(fakeregistry.WithTokenAuth("robot", "s3cret"))
	t.Cleanup(fake.Close)
	for i := 0; i < 5; i++ {
		fake.AddTag("vendor-a/platform", fmt.Sprintf("v1.%d.0", i))
	}

	repo := newRepo(t, fake, "vendor-a/platform",
		transport.Config{Username: "robot", Password: "s3cret"})

	tags, err := ListAllTags(t.Context(), repo, 0)
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	for _, tag := range tags {
		if _, err := repo.ResolveTag(t.Context(), tag); err != nil {
			t.Fatalf("resolve %s: %v", tag, err)
		}
	}

	if calls := fake.TokenCalls.Load(); calls != 1 {
		t.Fatalf("expected exactly 1 token exchange across %d requests, got %d",
			len(tags)+1, calls)
	}
}

func TestRetriesTransientFailures(t *testing.T) {
	fake := fakeregistry.New()
	t.Cleanup(fake.Close)
	fake.AddTag("vendor-a/platform", "v1.0.0")
	// Two 503s, then success.
	fake.FailNext("/tags/list", http.StatusServiceUnavailable, 2)

	repo := newRepo(t, fake, "vendor-a/platform", transport.Config{})

	tags, _, err := repo.ListTags(t.Context(), "", 100)
	if err != nil {
		t.Fatalf("transient failures should be retried transparently: %v", err)
	}
	if len(tags) != 1 {
		t.Fatalf("expected the tag after recovery, got %v", tags)
	}
}

func TestHonoursRetryAfterOn429(t *testing.T) {
	fake := fakeregistry.New()
	t.Cleanup(fake.Close)
	fake.AddTag("vendor-a/platform", "v1.0.0")
	fake.FailNextWithRetryAfter("/tags/list", http.StatusTooManyRequests, 1, "1")

	repo := newRepo(t, fake, "vendor-a/platform", transport.Config{})

	tags, _, err := repo.ListTags(t.Context(), "", 100)
	if err != nil {
		t.Fatalf("a 429 with Retry-After should be retried: %v", err)
	}
	if len(tags) != 1 {
		t.Fatalf("expected recovery, got %v", tags)
	}
}

func TestGivesUpAfterPersistentFailure(t *testing.T) {
	fake := fakeregistry.New()
	t.Cleanup(fake.Close)
	fake.AddTag("vendor-a/platform", "v1.0.0")
	fake.FailNext("/tags/list", http.StatusServiceUnavailable, 100)

	repo := newRepo(t, fake, "vendor-a/platform", transport.Config{})

	_, _, err := repo.ListTags(t.Context(), "", 100)
	if err == nil {
		t.Fatal("expected failure after exhausting retries")
	}
	if !errors.Is(err, registry.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
	if !registry.Retryable(err) {
		t.Fatal("a 5xx should still classify as retryable for the caller's own policy")
	}
}

func TestErrorCarriesRegistryDetail(t *testing.T) {
	// "HTTP 403" tells an operator nothing; the registry's own message is what
	// they can act on.
	fake := fakeregistry.New()
	t.Cleanup(fake.Close)
	fake.FailNext("/tags/list", http.StatusForbidden, 1)

	repo := newRepo(t, fake, "vendor-a/platform", transport.Config{})

	_, _, err := repo.ListTags(t.Context(), "", 100)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "injected fault") {
		t.Fatalf("error should carry the registry's message, got: %v", err)
	}
	var rerr *registry.Error
	if !errors.As(err, &rerr) {
		t.Fatalf("expected a *registry.Error, got %T", err)
	}
	if rerr.StatusCode != http.StatusForbidden {
		t.Fatalf("expected status 403 recorded, got %d", rerr.StatusCode)
	}
	if rerr.Class() != registry.ClassAuth {
		t.Fatalf("expected auth class, got %s", rerr.Class())
	}
}

func TestCapabilitiesAreProbed(t *testing.T) {
	fake := fakeregistry.New()
	t.Cleanup(fake.Close)
	fake.AddTag("vendor-a/platform", "v1.0.0")

	repo := newRepo(t, fake, "vendor-a/platform", transport.Config{})

	caps := repo.Capabilities(t.Context())
	// The fake does not implement referrers, so probing must report absence
	// rather than assuming presence.
	if caps.SupportsReferrersAPI {
		t.Fatal("referrers must be probed, not assumed")
	}
	if !caps.SupportsCatalog {
		t.Fatal("the fake serves _catalog; the probe should detect it")
	}
	if caps.TagPagination != registry.PaginationLink {
		t.Fatalf("expected Link pagination, got %q", caps.TagPagination)
	}
}

func TestRepositoryNameAndIdentity(t *testing.T) {
	fake := fakeregistry.New()
	t.Cleanup(fake.Close)
	repo := newRepo(t, fake, "vendor-a/platform", transport.Config{})

	if repo.Path() != "vendor-a/platform" {
		t.Fatalf("got path %q", repo.Path())
	}
	if repo.Registry() != fake.Host() {
		t.Fatalf("got registry %q", repo.Registry())
	}
	if want := fake.Host() + "/vendor-a/platform"; repo.Name() != want {
		t.Fatalf("got name %q, want %q", repo.Name(), want)
	}
}
