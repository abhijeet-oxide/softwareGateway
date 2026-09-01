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
}

func (p *countingProvider) Name() string  { return "stub" }
func (p *countingProvider) Enabled() bool { return true }

func (p *countingProvider) Scan(_ context.Context, refs []ArtifactRef, _ ScanOptions) ([]Report, error) {
	p.calls++
	out := make([]Report, 0, len(refs))
	for _, ref := range refs {
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
