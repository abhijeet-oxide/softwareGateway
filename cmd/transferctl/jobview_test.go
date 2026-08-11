package main

import (
	"strconv"
	"testing"
	"time"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// What a job row says it is copying, and where.
//
// These assertions are about legibility rather than correctness of the copy,
// but the two are not separable in practice: an operator who cannot tell which
// image a stuck blob belongs to cannot act on the fact that it is stuck.

// A component that names itself somewhere else is the case the two-site
// placement exists for, and the case a job row is most easily read wrongly:
// it sits in the bundle's repository by DIGEST, and is published under its own
// name separately. Neither column may imply a reference that does not resolve.
func TestRelocatedComponentShowsItsNameNotAFabricatedTag(t *testing.T) {
	source := "orbs/cfx-5000-k8s-215952-edgenac-25.7-2131"
	parent := &v1.JobParent{
		Digest: "sha256:aa",
		Ref:    "cfx-5000-product/zts-docker-candidates.repo.example.net/seldon/tfserving-proxy:1.22.4",
	}

	// The site that keeps the bundle resolvable: untagged, by construction.
	inBundle := v1.Job{
		Kind:             "blob",
		SourceRepository: source,
		TargetRepository: "apm-oci-stage/" + source,
		Parent:           parent,
	}
	wantSource := source +
		" (cfx-5000-product/zts-docker-candidates.repo.example.net/seldon/tfserving-proxy:1.22.4)"
	if got := jobSource(inBundle); got != wantSource {
		t.Errorf("source = %q, want %q", got, wantSource)
	}
	if got, want := jobTarget(inBundle), "apm-oci-stage/"+source; got != want {
		t.Errorf("target = %q, want %q\n(a tag here advertises one the transfer never creates)", got, want)
	}

	// The site published under the component's own name: tagged, and the tag
	// is the row's own rather than borrowed.
	asItself := inBundle
	asItself.TargetRepository =
		"apm-oci-stage/cfx-5000-product/zts-docker-candidates.repo.example.net/seldon/tfserving-proxy"
	asItself.TargetTags = []string{"1.22.4"}
	if got, want := jobTarget(asItself), asItself.TargetRepository+":1.22.4"; got != want {
		t.Errorf("target = %q, want %q", got, want)
	}
}

// Where the name DOES belong to the repository the row reads from, the tag is
// a reference that resolves, so it is shown as one.
func TestNameWithinItsOwnRepositoryReadsAsAReference(t *testing.T) {
	job := v1.Job{
		Kind:             "manifest",
		SourceRepository: "orbs/cfx-5000-k8s",
		TargetRepository: "sw-gateway/orbs/cfx-5000-k8s",
		TargetTags:       []string{"latest", "orb_23.8.1076"},
		Parent:           &v1.JobParent{Ref: "orbs/cfx-5000-k8s:orb_23.8.1076"},
	}

	if got, want := jobSource(job), "orbs/cfx-5000-k8s:orb_23.8.1076"; got != want {
		t.Errorf("source = %q, want %q", got, want)
	}
	if got, want := jobTarget(job), "sw-gateway/orbs/cfx-5000-k8s:latest,orb_23.8.1076"; got != want {
		t.Errorf("target = %q, want %q", got, want)
	}
}

func TestSharedBlobIsMarkedNotHidden(t *testing.T) {
	job := v1.Job{
		Kind:             "blob",
		SourceRepository: "orbs/cfx-5000-k8s",
		TargetRepository: "sw-gateway/orbs/cfx-5000-k8s",
		Parent:           &v1.JobParent{Ref: "orbs/cfx-5000-k8s:1.2.3", Shared: true},
	}

	if got, want := jobSource(job), "orbs/cfx-5000-k8s:1.2.3 *"; got != want {
		t.Errorf("source = %q, want %q", got, want)
	}
	// The mark belongs to the attribution, which is a source-side fact. The
	// destination path is exact either way.
	if got, want := jobTarget(job), "sw-gateway/orbs/cfx-5000-k8s"; got != want {
		t.Errorf("target = %q, want %q", got, want)
	}
}

func TestJobPathsSayNothingRatherThanGuess(t *testing.T) {
	if got := jobSource(v1.Job{}); got != "-" {
		t.Errorf("source of an unplaced job = %q, want -", got)
	}
	if got := jobTarget(v1.Job{}); got != "-" {
		t.Errorf("target of an unplaced job = %q, want -", got)
	}
	// No tags and no named parent: the path is still exact, just untagged.
	job := v1.Job{SourceRepository: "orbs/x", TargetRepository: "t/orbs/x"}
	if got, want := jobSource(job), "orbs/x"; got != want {
		t.Errorf("source = %q, want %q", got, want)
	}
}

func TestSplitRefIgnoresARegistryPort(t *testing.T) {
	cases := []struct{ ref, repository, tag string }{
		{"orbs/CFX-5000-k8s/nginx:1.2.3", "orbs/CFX-5000-k8s/nginx", "1.2.3"},
		{"near.example.com:5000/orbs/x", "near.example.com:5000/orbs/x", ""},
		{"orbs/x", "orbs/x", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		repo, tag := splitRef(c.ref)
		if repo != c.repository || tag != c.tag {
			t.Errorf("splitRef(%q) = %q, %q, want %q, %q",
				c.ref, repo, tag, c.repository, c.tag)
		}
	}
}

// Progress arithmetic: the numbers a watcher reads off the table.

func TestPercentCompleteCountsJobsSoItReaches100(t *testing.T) {
	// Bytes would not: a deduplicated job moves nothing and still counts in
	// plannedBytes, so a bytes-based percentage stops short and looks stuck.
	p := v1.TransferProgress{JobsPlanned: 4, JobsDone: 4}
	if got := percentComplete(p); got != 100 {
		t.Errorf("percent = %v, want 100", got)
	}
	if got := percentComplete(v1.TransferProgress{}); got != 0 {
		t.Errorf("percent of an unplanned transfer = %v, want 0", got)
	}
}

func TestElapsedRunsFromTheFirstLease(t *testing.T) {
	start := time.Now().Add(-90 * time.Second)
	tr := &v1.Transfer{
		CreatedAt: start.Add(-time.Hour).Format(time.RFC3339Nano),
		StartedAt: start.Format(time.RFC3339Nano),
	}

	d, ok := elapsed(tr)
	if !ok {
		t.Fatal("elapsed not reported for a started transfer")
	}
	// An hour of queueing must not be counted as an hour of transferring.
	if d > 5*time.Minute {
		t.Errorf("elapsed = %s, want ~90s measured from the first lease", d)
	}

	if _, ok := elapsed(&v1.Transfer{CreatedAt: tr.CreatedAt}); ok {
		t.Error("elapsed reported for a transfer that has not started")
	}
}

func TestEstimateWithdrawsRatherThanGuess(t *testing.T) {
	tr := &v1.Transfer{
		StartedAt: time.Now().Add(-10 * time.Second).Format(time.RFC3339Nano),
		Progress: v1.TransferProgress{
			PlannedBytes:     bytesOf(1000),
			BytesTransferred: bytesOf(0),
		},
	}
	if _, ok := estimate(tr); ok {
		t.Error("estimate offered with no bytes moved to extrapolate from")
	}

	tr.Progress.BytesTransferred = bytesOf(500)
	d, ok := estimate(tr)
	if !ok {
		t.Fatal("no estimate once a rate is observable")
	}
	// Half of 1000 bytes in ten seconds: the other half takes about as long.
	if d < 5*time.Second || d > 20*time.Second {
		t.Errorf("estimate = %s, want ~10s", d)
	}
}

func TestRateTrackerNeedsTwoReadings(t *testing.T) {
	var r rateTracker
	at := time.Now()

	r.observe(1_000, at)
	if r.current != 0 || r.peak != 0 {
		t.Errorf("first sample reported a rate: current=%v peak=%v", r.current, r.peak)
	}

	r.observe(3_000, at.Add(2*time.Second))
	if r.current != 1_000 {
		t.Errorf("current = %v, want 1000 B/s", r.current)
	}
	if r.peak != 1_000 {
		t.Errorf("peak = %v, want 1000 B/s", r.peak)
	}

	// A slower interval must not lower the peak.
	r.observe(3_400, at.Add(4*time.Second))
	if r.current != 200 {
		t.Errorf("current = %v, want 200 B/s", r.current)
	}
	if r.peak != 1_000 {
		t.Errorf("peak = %v, want the earlier 1000 B/s", r.peak)
	}

	// A retry resets a job's counter, so totals can go backwards. That is a
	// sample to skip, not a negative rate to report.
	r.observe(100, at.Add(6*time.Second))
	if r.current != 200 {
		t.Errorf("current = %v after a backwards sample, want the previous 200 B/s", r.current)
	}
}

func bytesOf(n int64) v1.Int64String {
	return v1.Int64String(strconv.FormatInt(n, 10))
}
