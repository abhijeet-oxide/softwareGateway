package security

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeRecorder captures what a sync decided.
type fakeRecorder struct {
	claimed  int
	recorded []PackageResult
	failed   []string
	claimErr error
}

func (f *fakeRecorder) Claim(context.Context, int64, time.Duration) error {
	if f.claimErr != nil {
		return f.claimErr
	}
	f.claimed++
	return nil
}
func (f *fakeRecorder) Record(_ context.Context, res PackageResult) error {
	f.recorded = append(f.recorded, res)
	return nil
}
func (f *fakeRecorder) Fail(_ context.Context, _ int64, reason string) error {
	f.failed = append(f.failed, reason)
	return nil
}

// stubProvider answers with whatever it is given.
type stubProvider struct {
	enabled bool
	byRef   map[string]Report
	err     error
}

func (s stubProvider) Name() string  { return "jfrog-xray" }
func (s stubProvider) Enabled() bool { return s.enabled }
func (s stubProvider) Scan(_ context.Context, refs []ArtifactRef, _ ScanOptions) ([]Report, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := make([]Report, 0, len(refs))
	for _, ref := range refs {
		if r, ok := s.byRef[ref.Digest]; ok {
			r.Artifact = ref
			out = append(out, r)
			continue
		}
		out = append(out, Report{Artifact: ref, Status: StatusUnavailable, Provider: "jfrog-xray",
			Message: "JFrog Xray could not be reached."})
	}
	return out, nil
}

type stubResolver struct{ p Provider }

func (s stubResolver) ProviderFor(context.Context, string, string) (Provider, error) { return s.p, nil }

func syncerFor(t *testing.T, p Provider) (*Syncer, *fakeRecorder) {
	t.Helper()
	rec := &fakeRecorder{}
	return NewSyncer(NewService(stubResolver{p}, nil, nil), rec, nil), rec
}

func waitFor(t *testing.T, done func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if done() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the sync never finished")
}

func syncRequest() SyncRequest {
	return SyncRequest{
		PackageID: 1, Label: "25.7.2131",
		Scope:     Scope{Product: "cfx", Repository: "cfx-jfrog-lab", Role: "target", Provider: "jfrog-xray"},
		Artifacts: []ArtifactRef{{Name: "cfx-main", Digest: "sha256:a", Kind: "image"}},
	}
}

func TestSyncRecordsWhatItFound(t *testing.T) {
	report := Report{Status: StatusScanned, Provider: "jfrog-xray", Findings: []Finding{
		{CVE: "CVE-1", Severity: SeverityHigh, Fixable: true,
			Component: Component{ID: "deb://openssl", Name: "openssl"}},
	}}
	report.Recount()

	s, rec := syncerFor(t, stubProvider{enabled: true, byRef: map[string]Report{"sha256:a": report}})
	if _, err := s.Start(t.Context(), syncRequest()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, func() bool { return len(rec.recorded) > 0 || len(rec.failed) > 0 })

	if len(rec.failed) > 0 {
		t.Fatalf("recorded a failure: %v", rec.failed)
	}
	got := rec.recorded[0]
	if got.Posture.Counts.Total != 1 || got.Repository != "cfx-jfrog-lab" {
		t.Errorf("result = %+v", got)
	}
	// The claim is released the moment the goroutine ends, so a second sync
	// can start.
	waitFor(t, func() bool { return !s.Running(1) })
}

// A sync that reached the scanner and got results for NOTHING is not a success
// with zero findings. Recorded as one it would put a clean-looking row in a
// listing for a release nobody scanned.
func TestSyncWithNoResultsIsAFailure(t *testing.T) {
	s, rec := syncerFor(t, stubProvider{enabled: true, byRef: map[string]Report{}})
	if _, err := s.Start(t.Context(), syncRequest()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, func() bool { return len(rec.recorded) > 0 || len(rec.failed) > 0 })

	if len(rec.recorded) > 0 {
		t.Fatalf("recorded a success with no results: %+v", rec.recorded[0])
	}
	if len(rec.failed) != 1 {
		t.Fatalf("failures = %v", rec.failed)
	}
	// The reason carries the scanner's own words, which is the only part that
	// tells anybody what to do.
	if !contains(rec.failed[0], "could not be reached") {
		t.Errorf("reason = %q, want the scanner's message", rec.failed[0])
	}
}

// A release with nothing scannable in it is an honest zero and stays a success.
func TestSyncWithNothingScannableSucceeds(t *testing.T) {
	s, rec := syncerFor(t, stubProvider{enabled: true, byRef: map[string]Report{
		"sha256:a": {Status: StatusUnsupported, Provider: "jfrog-xray"},
	}})
	if _, err := s.Start(t.Context(), syncRequest()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, func() bool { return len(rec.recorded) > 0 || len(rec.failed) > 0 })

	if len(rec.failed) > 0 {
		t.Fatalf("a release of signatures was recorded as a failure: %v", rec.failed)
	}
}

// A disabled scanner is not a clean release either.
func TestSyncWithDisabledScannerFails(t *testing.T) {
	s, rec := syncerFor(t, Disabled{ProviderName: "jfrog-xray", Reason: "Xray is not enabled."})
	if _, err := s.Start(t.Context(), syncRequest()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, func() bool { return len(rec.recorded) > 0 || len(rec.failed) > 0 })

	if len(rec.recorded) > 0 {
		t.Fatal("a disabled scanner was recorded as a successful sync")
	}
	if len(rec.failed) != 1 || !contains(rec.failed[0], "not enabled") {
		t.Errorf("failures = %v", rec.failed)
	}
}

// A claim somebody else holds is reported back, and no work starts.
func TestSyncRespectsAHeldClaim(t *testing.T) {
	rec := &fakeRecorder{claimErr: errors.New("already running")}
	s := NewSyncer(NewService(stubResolver{stubProvider{enabled: true}}, nil, nil), rec, nil)

	status, err := s.Start(t.Context(), syncRequest())
	if err == nil {
		t.Fatal("Start hid a held claim")
	}
	if status != SyncAlreadyRunning {
		t.Errorf("status = %q, want already_running", status)
	}
	if s.Running(1) {
		t.Error("work started despite the claim being held")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
