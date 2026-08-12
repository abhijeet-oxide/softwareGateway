package store

import (
	"strconv"
	"strings"
	"testing"
)

// The listing answers "which jobs are failing". This has to answer "why", and
// the difference is the whole point: five hundred manifests rejected by one
// destination for one reason are five hundred rows and ONE cause.

func TestOneCauseHoldingManyJobsIsReportedOnce(t *testing.T) {
	h := newFailureHarness(t)
	id := h.transferWithJobs(0)

	// What the real thing looks like: the same rejection, once per manifest,
	// each naming its own digest and its own destination path.
	for i := range 40 {
		job := h.manifestJob(id, i)
		h.fail(job, "unsupported", "push manifest "+h.digestOf(job)+" "+
			h.repositoryOf(job)+": HTTP 400: manifest invalid: schema version is not supported")
	}

	groups, err := h.packages.FailureGroups(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Fatalf("reported %d causes for one rejection repeated 40 times", len(groups))
	}

	g := groups[0]
	if g.Failed != 40 {
		t.Errorf("the cause holds %d jobs, want 40", g.Failed)
	}
	if !strings.Contains(g.Message, "manifest invalid") {
		t.Errorf("message %q does not carry what the registry said", g.Message)
	}
	// The per-job parts must be gone from the shared sentence, or the grouping
	// would not have happened at all.
	if strings.Contains(g.Message, "sha256:") && !strings.Contains(g.Message, "<digest>") {
		t.Errorf("message %q still carries a digest", g.Message)
	}
	// …and present on the example, which is what somebody goes and looks at.
	if g.ExampleDigest == "" || !strings.Contains(g.ExampleError, g.ExampleDigest) {
		t.Errorf("the example does not name a concrete job: %+v", g)
	}
	if g.Retryable {
		t.Error("an unsupported rejection was reported as worth retrying")
	}
}

// Two causes are two blocks. A summary that folded them together would hide
// the one that is actionable behind the one that is not.
func TestDistinctCausesStaySeparateAndAreRankedByBlastRadius(t *testing.T) {
	h := newFailureHarness(t)
	id := h.transferWithJobs(0)

	for i := range 3 {
		job := h.manifestJob(id, i)
		h.fail(job, "auth", "push manifest "+h.digestOf(job)+": HTTP 401: unauthorized")
	}
	for i := 10; i < 20; i++ {
		job := h.manifestJob(id, i)
		h.fail(job, "unavailable", "push manifest "+h.digestOf(job)+": HTTP 503: service unavailable")
	}

	groups, err := h.packages.FailureGroups(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("reported %d causes, want 2", len(groups))
	}
	// Biggest first: the one worth fixing leads.
	if groups[0].Class != "unavailable" || groups[0].Failed != 10 {
		t.Errorf("first group = %s holding %d, want the 10 unavailable",
			groups[0].Class, groups[0].Failed)
	}
	if groups[1].Class != "auth" {
		t.Errorf("second group = %s, want auth", groups[1].Class)
	}
	if groups[1].Retryable {
		t.Error("an auth failure was reported as worth retrying")
	}
	if !groups[0].Retryable {
		t.Error("a 503 was reported as not worth retrying")
	}
}

// "Failed" and "retrying" need different responses, and a single total hides
// which is which. A transfer where everything is still backing off is waiting,
// not broken.
func TestRetryingJobsAreCountedApartFromFailedOnes(t *testing.T) {
	h := newFailureHarness(t)
	id := h.transferWithJobs(0)

	done := h.manifestJob(id, 1)
	h.fail(done, "timeout", "push manifest "+h.digestOf(done)+": HTTP 504: gateway timeout")

	waiting := h.manifestJob(id, 2)
	h.fail(waiting, "timeout", "push manifest "+h.digestOf(waiting)+": HTTP 504: gateway timeout")
	// Exactly what a job mid-backoff looks like: pending, with the reason it is
	// waiting still on it.
	h.exec(`UPDATE jobs SET state='pending' WHERE id = ?`, waiting)

	groups, err := h.packages.FailureGroups(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Fatalf("reported %d causes, want 1", len(groups))
	}
	if groups[0].Failed != 1 || groups[0].Retrying != 1 {
		t.Errorf("counted %d failed and %d retrying, want one of each",
			groups[0].Failed, groups[0].Retrying)
	}
}

// A job that has succeeded since must not appear. Its error describes an
// attempt that has been superseded, and reporting it would send somebody to
// investigate something that already fixed itself.
func TestASucceededJobIsNotAFailure(t *testing.T) {
	h := newFailureHarness(t)
	id := h.transferWithJobs(0)

	job := h.manifestJob(id, 1)
	h.fail(job, "timeout", "push manifest: HTTP 504: gateway timeout")
	h.exec(`UPDATE jobs SET state='succeeded' WHERE id = ?`, job)

	groups, err := h.packages.FailureGroups(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 0 {
		t.Errorf("reported %+v for a transfer whose only error is on a succeeded job", groups)
	}
}

// The normaliser has to survive a message naming a digest that is NOT the
// job's own — a manifest rejected for a missing layer names the layer, and the
// layer differs per manifest while the cause does not.
func TestAMessageNamingSomebodyElsesDigestStillGroups(t *testing.T) {
	h := newFailureHarness(t)
	id := h.transferWithJobs(0)

	for i := range 5 {
		job := h.manifestJob(id, i)
		h.fail(job, "not_found", "push manifest "+h.digestOf(job)+
			": HTTP 400: manifest blob unknown: sha256:"+strings.Repeat(strconv.Itoa(i), 64))
	}

	groups, err := h.packages.FailureGroups(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Fatalf("reported %d causes; the missing-layer digest fragmented the grouping", len(groups))
	}
	if groups[0].Failed != 5 {
		t.Errorf("the cause holds %d jobs, want 5", groups[0].Failed)
	}
}

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

type failureHarness struct {
	*recoveryHarness
}

func newFailureHarness(t *testing.T) *failureHarness {
	t.Helper()
	return &failureHarness{recoveryHarness: newRecoveryHarness(t)}
}

func (h *failureHarness) manifestJob(transferID string, n int) int64 {
	h.t.Helper()

	digest := "sha256:" + strings.Repeat("a", 60) + padded(n)
	repo := "apm0014228-oci-stage/orbs/cfx-5000-product/component-" + strconv.Itoa(n)

	res, err := h.st.DB().ExecContext(h.t.Context(),
		`INSERT INTO jobs (transfer_id, kind, digest, size_bytes, source_repo_id, target_repo_id,
		                   target_repository, state, wave, attempts, max_attempts)
		 VALUES (?, 'manifest', ?, 512, ?, ?, ?, 'blocked', 1, 0, 8)`,
		transferID, digest, h.repoID, h.repoID, repo)
	if err != nil {
		h.t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

// fail marks a job terminally failed with a given class and message, directly
// — the completion path is exercised elsewhere and would drag wave settlement
// into a test about reporting.
func (h *failureHarness) fail(jobID int64, class, message string) {
	h.t.Helper()
	h.exec(`UPDATE jobs SET state='failed', attempts=8, last_error=?, last_error_class=?
	         WHERE id = ?`, message, class, jobID)
}

func (h *failureHarness) digestOf(jobID int64) string {
	h.t.Helper()
	var d string
	if err := h.query(`SELECT digest FROM jobs WHERE id = ?`, jobID).Scan(&d); err != nil {
		h.t.Fatal(err)
	}
	return d
}

func (h *failureHarness) repositoryOf(jobID int64) string {
	h.t.Helper()
	var r string
	if err := h.query(`SELECT COALESCE(target_repository,'') FROM jobs WHERE id = ?`, jobID).
		Scan(&r); err != nil {
		h.t.Fatal(err)
	}
	return r
}

func padded(n int) string {
	s := strconv.Itoa(n)
	for len(s) < 4 {
		s = "0" + s
	}
	return s
}
