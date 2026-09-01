package security

import (
	"context"
	"testing"
	"time"
)

// recordingCache is a Cache that holds what it is given and says whether
// anybody asked it to forget.
type recordingCache struct {
	summaries   map[string]Report
	details     map[string]Report
	invalidated int
}

func newRecordingCache() *recordingCache {
	return &recordingCache{summaries: map[string]Report{}, details: map[string]Report{}}
}

func (c *recordingCache) LoadSummaries(_ context.Context, _ Scope, refs []ArtifactRef) (map[string]Report, error) {
	return pick(c.summaries, refs), nil
}

func (c *recordingCache) LoadDetails(_ context.Context, _ Scope, refs []ArtifactRef) (map[string]Report, error) {
	return pick(c.details, refs), nil
}

func (c *recordingCache) Save(_ context.Context, _ Scope, reports []Report, detail bool, _ CacheTTL) error {
	for _, r := range reports {
		// The store's own rule, mirrored here because it is the rule this test
		// is about: an artifact the scanner would not answer for keeps what it
		// had. See TestSecurityUnavailableDoesNotErasePreviousResult.
		if r.Status == StatusUnavailable {
			if _, held := c.summaries[r.Artifact.Ref()]; held {
				continue
			}
		}
		c.summaries[r.Artifact.Ref()] = r
		if detail {
			c.details[r.Artifact.Ref()] = r
		}
	}
	return nil
}

func (c *recordingCache) Invalidate(_ context.Context, _ Scope, _ []ArtifactRef) error {
	c.invalidated++
	return nil
}

func pick(from map[string]Report, refs []ArtifactRef) map[string]Report {
	out := map[string]Report{}
	for _, ref := range refs {
		if r, ok := from[ref.Ref()]; ok {
			out[ref.Ref()] = r
		}
	}
	return out
}

// countingProvider answers with whatever it was configured to answer, and
// counts the times it was asked.
type countingProvider struct {
	reports map[string]Report
	calls   int
	seen    []string
}

// asked is which artifacts the scanner was actually queried about since the
// last reset, which is the number this file is about.
func (p *countingProvider) asked() []string { return p.seen }
func (p *countingProvider) reset()          { p.seen = nil; p.calls = 0 }

func (p *countingProvider) Name() string  { return "stub" }
func (p *countingProvider) Enabled() bool { return true }

func (p *countingProvider) Scan(_ context.Context, refs []ArtifactRef, _ ScanOptions) ([]Report, error) {
	p.calls++
	out := make([]Report, 0, len(refs))
	for _, ref := range refs {
		p.seen = append(p.seen, ref.Ref())
		r, ok := p.reports[ref.Ref()]
		if !ok {
			r = Report{Status: StatusNotScanned, Provider: "stub"}
		}
		r.Artifact = ref
		r.Provider = "stub"
		r.RetrievedAt = time.Now().UTC()
		out = append(out, r)
	}
	return out, nil
}

func scannedWith(cve string) Report {
	r := Report{
		Status: StatusScanned,
		Findings: []Finding{{
			CVE: cve, Severity: SeverityCritical,
			Component: Component{ID: "deb://openssl", Name: "openssl", Version: "1.1.1n"},
		}},
	}
	r.Recount()
	return r
}

// A refresh must not empty the store before it has an answer.
//
// The stored rows are keyed by ARTIFACT, so they are shared by every release
// carrying that image. Clearing them at the start of a sync emptied every other
// release for as long as the sync ran, and left a sync interrupted by a scanner
// outage with nothing at all - the store's rule that an unavailable answer
// keeps the previous result cannot help when the previous result was deleted a
// moment earlier.
func TestRefreshOverwritesRatherThanClearingFirst(t *testing.T) {
	cache := newRecordingCache()
	ref := ArtifactRef{Name: "cfx-main", Digest: "sha256:aaa", Kind: "image"}
	scope := Scope{Product: "cfx", Repository: "jfrog", Provider: "stub"}

	provider := &countingProvider{reports: map[string]Report{ref.Ref(): scannedWith("CVE-2024-3094")}}
	svc := NewService(stubResolver{provider}, cache, nil)

	first, err := svc.Posture(t.Context(), Request{
		Scope: scope, Artifacts: []ArtifactRef{ref}, Detail: true,
	})
	if err != nil {
		t.Fatalf("first retrieval: %v", err)
	}
	if first.Posture.Counts.Total != 1 {
		t.Fatalf("first retrieval found %d, want 1", first.Posture.Counts.Total)
	}

	// Now a refresh where the scanner will not answer, which is what a sync
	// against a busy Xray looks like from here.
	provider.reports[ref.Ref()] = Report{
		Status: StatusUnavailable, Message: "JFrog Xray did not answer in time.",
	}
	if _, err := svc.Posture(t.Context(), Request{
		Scope: scope, Artifacts: []ArtifactRef{ref}, Detail: true, Refresh: true,
	}); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if cache.invalidated != 0 {
		t.Errorf("a refresh cleared the cache %d times; it must overwrite instead", cache.invalidated)
	}
	// What another release carrying this image would now be shown.
	held, err := cache.LoadDetails(t.Context(), scope, []ArtifactRef{ref})
	if err != nil {
		t.Fatalf("LoadDetails: %v", err)
	}
	kept := held[ref.Ref()]
	if kept.Status != StatusScanned || len(kept.Findings) != 1 {
		t.Fatalf("the refresh destroyed the stored result: status %q, %d findings",
			kept.Status, len(kept.Findings))
	}

	// And a refresh still ASKS - skipping the read is what makes it a refresh.
	if provider.calls != 2 {
		t.Errorf("provider called %d times, want 2: a refresh must not be served from cache",
			provider.calls)
	}
}

// A sync asks the scanner only about what it does not already have.
//
// This is what storing answers by ARTIFACT is for. Releases of one product
// share nearly all of their images, so syncing November's release the morning
// after October's used to re-ask the scanner about 150 images that were the
// same bytes, already answered for, an hour old - ten minutes of somebody
// waiting, against somebody else's rate limit, to re-learn something known.
func TestASyncReusesAnswersInsideTheAgeLimit(t *testing.T) {
	cache := newRecordingCache()
	scope := Scope{Product: "cfx", Repository: "jfrog", Provider: "stub"}
	shared := ArtifactRef{Name: "base", Digest: "sha256:aaa", Kind: "image"}
	fresh := ArtifactRef{Name: "app", Digest: "sha256:bbb", Kind: "image"}

	provider := &countingProvider{reports: map[string]Report{
		shared.Ref(): scannedWith("CVE-2024-3094"),
		fresh.Ref():  scannedWith("CVE-2024-6387"),
	}}
	svc := NewService(stubResolver{provider}, cache, nil)

	// October's release, which answers for the shared image.
	if _, err := svc.Posture(t.Context(), Request{
		Scope: scope, Artifacts: []ArtifactRef{shared}, Detail: true,
	}); err != nil {
		t.Fatalf("first release: %v", err)
	}
	asked := provider.asked()
	if len(asked) != 1 {
		t.Fatalf("first release asked about %v, want the one image", asked)
	}

	// November's, which carries the same base image and one of its own.
	provider.reset()
	res, err := svc.Posture(t.Context(), Request{
		Scope: scope, Artifacts: []ArtifactRef{shared, fresh}, Detail: true,
		MaxAge: time.Hour,
	})
	if err != nil {
		t.Fatalf("second release: %v", err)
	}
	asked = provider.asked()
	if len(asked) != 1 || asked[0] != fresh.Ref() {
		t.Fatalf("second release asked about %v, want only the image it has not seen", asked)
	}
	// And the reused one is still in the answer, with its findings.
	if res.Posture.Counts.Total != 2 {
		t.Errorf("posture counted %d findings, want both images' - reuse must not lose data",
			res.Posture.Counts.Total)
	}

	// Past the age it is asked about again, because a vulnerability answer
	// goes out of date without the image changing.
	provider.reset()
	if _, err := svc.Posture(t.Context(), Request{
		Scope: scope, Artifacts: []ArtifactRef{shared, fresh}, Detail: true,
		MaxAge: time.Nanosecond,
	}); err != nil {
		t.Fatalf("aged retrieval: %v", err)
	}
	if len(provider.asked()) != 2 {
		t.Errorf("past the age limit the scanner was asked about %v, want both",
			provider.asked())
	}
}

// A stored "the scanner would not answer" is not an answer, and must not be
// reused as one. An image that failed during an outage would otherwise stay
// unanswered until somebody noticed and forced a refresh by hand.
func TestAFailedArtifactIsAlwaysAskedAboutAgain(t *testing.T) {
	cache := newRecordingCache()
	scope := Scope{Product: "cfx", Repository: "jfrog", Provider: "stub"}
	ref := ArtifactRef{Name: "app", Digest: "sha256:ccc", Kind: "image"}

	provider := &countingProvider{reports: map[string]Report{
		ref.Ref(): {Status: StatusUnavailable, Message: "JFrog Xray did not answer in time."},
	}}
	svc := NewService(stubResolver{provider}, cache, nil)

	if _, err := svc.Posture(t.Context(), Request{
		Scope: scope, Artifacts: []ArtifactRef{ref}, Detail: true, MaxAge: time.Hour,
	}); err != nil {
		t.Fatalf("first: %v", err)
	}

	provider.reset()
	provider.reports[ref.Ref()] = scannedWith("CVE-2024-3094")
	res, err := svc.Posture(t.Context(), Request{
		Scope: scope, Artifacts: []ArtifactRef{ref}, Detail: true, MaxAge: time.Hour,
	})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if len(provider.asked()) != 1 {
		t.Fatal("an image the scanner failed on was treated as answered for")
	}
	if res.Posture.Counts.Total != 1 {
		t.Errorf("the retry did not take: %d findings", res.Posture.Counts.Total)
	}
}
