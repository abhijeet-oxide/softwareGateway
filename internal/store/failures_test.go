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

// The message that motivated all of this, verbatim from the production run.
//
// Two things in it defeated the first normaliser, and each one on its own was
// enough to put every manifest in a group of its own:
//
//   - Artifactory names the blob `sha256__<hex>` in its storage layout, and a
//     bare-hex pattern cannot match that because `_` is a word character, so
//     there is no word boundary in front of the hex.
//   - It echoes the destination with its own repository key stripped, so the
//     path we sent is not the path in the message and a whole-string
//     substitution finds nothing.
func TestTheProductionRejectionGroupsIntoOneCause(t *testing.T) {
	h := newFailureHarness(t)
	id := h.transferWithJobs(0)

	messages := []string{
		"push manifest sha256:aaaa000000000000000000000000000000000000000000000000000000000001 " +
			"artifact.it.att.com/apm0014228-oci-stage/orbs/cfx-5000-product/nokia-ims-sas: " +
			"HTTP 400: manifest invalid: manifest invalid: map[description:Failed to copy blob " +
			"sha256:bbbb000000000000000000000000000000000000000000000000000000000001 to " +
			"cfx-5000-product/nokia-ims-sas/sha256:aaaa000000000000000000000000000000000000000000000000000000000001/" +
			"sha256__dc82908b11cffc06a831a0ff2382e0d9080441a806978d087c1f6d0f2e3278ee0]: " +
			"unsupported by registry",
		"push manifest sha256:aaaa000000000000000000000000000000000000000000000000000000000002 " +
			"artifact.it.att.com/apm0014228-oci-stage/orbs/cfx-5000-product/nokia-ims-rcg: " +
			"HTTP 400: manifest invalid: manifest invalid: map[description:Failed to copy blob " +
			"sha256:bbbb000000000000000000000000000000000000000000000000000000000002 to " +
			"cfx-5000-product/nokia-ims-rcg/sha256:aaaa000000000000000000000000000000000000000000000000000000000002/" +
			"sha256__5a8ead63d505514b3d5f4dbf8c0b1a0b2c3d4e5f60718293a4b5c6d7e8f90a1b]: " +
			"unsupported by registry",
	}

	for i, message := range messages {
		job := h.manifestJob(id, i)
		// The path the transfer wrote to, which is NOT the path the registry
		// echoed: Artifactory strips its own repository key.
		h.exec(`UPDATE jobs SET target_repository = ? WHERE id = ?`,
			"apm0014228-oci-stage/orbs/cfx-5000-product/nokia-ims-"+[]string{"sas", "rcg"}[i], job)
		h.fail(job, "blob_unknown", strings.ReplaceAll(message,
			"sha256:aaaa00000000000000000000000000000000000000000000000000000000000"+
				strconv.Itoa(i+1), h.digestOf(job)))
	}

	groups, err := h.packages.FailureGroups(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		for _, g := range groups {
			t.Logf("group: %s", g.Message)
		}
		t.Fatalf("reported %d causes for one rejection; a bundle would produce hundreds",
			len(groups))
	}
	if strings.Contains(groups[0].Message, "nokia-ims-sas") {
		t.Errorf("the destination path survived normalisation: %s", groups[0].Message)
	}
	if strings.Contains(groups[0].Message, "sha256__") {
		t.Errorf("Artifactory's storage-layout digest survived normalisation: %s",
			groups[0].Message)
	}
	// And what remains is the part somebody can act on.
	if !strings.Contains(groups[0].Message, "Failed to copy blob") {
		t.Errorf("the cause was normalised away: %s", groups[0].Message)
	}
}

// ---------------------------------------------------------------------------
// The repair, and what stops it
// ---------------------------------------------------------------------------

// A repair that does not help must not repeat. Both stops are asserted here,
// because either one alone leaves a loop: the manifest keeps its attempt count
// across a repair, and a blob already force-uploaded is not sent again.
//
// Without them, a destination that rejects a manifest for any reason mentioning
// a blob would loop forever — push, repair, re-upload, push — with every
// appearance of progress and no exit.
func TestARepairThatDoesNotHelpIsNotRepeated(t *testing.T) {
	h := newFailureHarness(t)
	id := h.transferWithJobs(0)

	artifact, digest := h.seedArtifactWithBlob(id)
	manifest := h.manifestJobForArtifact(id, artifact)
	blob := h.blobJobFor(id, digest)

	// Before the transaction: SQLite runs on one connection, so any statement
	// issued outside an open tx would wait for it and neither would finish.
	h.exec(`UPDATE jobs SET attempts = 3 WHERE id = ?`, manifest)

	tx, err := h.st.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := h.packages.RepairMissingBlobs(t.Context(), tx, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if first.Blobs != 1 {
		t.Fatalf("the first repair requeued %d blob(s), want 1", first.Blobs)
	}
	if first.AlreadyRepaired {
		t.Error("the first repair reported itself as already done")
	}

	// The attempt count SURVIVES. This is the bound: reset it and the budget
	// never runs out.
	if _, attempts := h.jobState(manifest); attempts != 3 {
		t.Errorf("the manifest came back with %d attempts, want its budget kept at 3", attempts)
	}
	if state, _ := h.jobState(blob); state != "pending" {
		t.Errorf("the blob is %q after a repair, want pending", state)
	}

	// The forced upload happens and the destination rejects the manifest
	// anyway — the case where the placement cache was never the problem.
	h.exec(`UPDATE jobs SET state='succeeded' WHERE id = ?`, blob)

	tx2, err := h.st.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.packages.RepairMissingBlobs(t.Context(), tx2, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatal(err)
	}

	if !second.AlreadyRepaired {
		t.Error("a second repair ran; the loop has no exit")
	}
	if second.Blobs != 0 {
		t.Errorf("the second repair requeued %d blob(s), want none", second.Blobs)
	}
	if state, _ := h.jobState(blob); state != "succeeded" {
		t.Errorf("the blob was sent back for a second forced upload (%q)", state)
	}
}

// Two manifests sharing a blob: one having already forced X must not stop the
// other from repairing its own Y. The bound is per blob, not per manifest.
func TestOneManifestsRepairDoesNotBlockAnothers(t *testing.T) {
	h := newFailureHarness(t)
	id := h.transferWithJobs(0)

	artifactA, digestA := h.seedArtifactWithBlob(id)
	artifactB, digestB := h.seedArtifactWithBlob(id)
	manifestA := h.manifestJobForArtifact(id, artifactA)
	manifestB := h.manifestJobForArtifact(id, artifactB)
	h.blobJobFor(id, digestA)
	blobB := h.blobJobFor(id, digestB)

	h.repair(manifestA)

	res := h.repair(manifestB)
	if res.Blobs != 1 || res.AlreadyRepaired {
		t.Errorf("B's repair = %+v; A having repaired first must not stop it", res)
	}
	if state, _ := h.jobState(blobB); state != "pending" {
		t.Errorf("B's blob is %q, want pending", state)
	}
}

func (h *failureHarness) repair(manifestJob int64) RepairResult {
	h.t.Helper()

	tx, err := h.st.DB().BeginTx(h.t.Context(), nil)
	if err != nil {
		h.t.Fatal(err)
	}
	res, err := h.packages.RepairMissingBlobs(h.t.Context(), tx, manifestJob)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		h.t.Fatal(err)
	}
	return res
}

// seedArtifactWithBlob records one artifact and one blob it references, the way
// the walk does — the repair resolves blobs through artifact_blobs, so a
// fixture without those rows would prove nothing.
func (h *failureHarness) seedArtifactWithBlob(string) (artifactID int64, digest string) {
	h.t.Helper()

	h.n++
	digest = "sha256:" + strings.Repeat("c", 60) + padded(h.n)
	manifestDigest := "sha256:" + strings.Repeat("d", 60) + padded(h.n)

	res, err := h.st.DB().ExecContext(h.t.Context(),
		`INSERT INTO package_artifacts (package_id, digest, media_type, size_bytes, depth)
		 VALUES (?, ?, 'application/vnd.oci.image.manifest.v1+json', 512, 1)`,
		h.pkgID, manifestDigest)
	if err != nil {
		h.t.Fatal(err)
	}
	artifactID, err = res.LastInsertId()
	if err != nil {
		h.t.Fatal(err)
	}

	h.exec(`INSERT INTO blobs (digest, size_bytes, media_type)
	         VALUES (?, 4096, 'application/octet-stream')`, digest)
	h.exec(`INSERT INTO artifact_blobs (artifact_id, digest, kind, ordinal)
	         VALUES (?, ?, 'layer', 0)`, artifactID, digest)
	return artifactID, digest
}

func (h *failureHarness) manifestJobForArtifact(transferID string, artifactID int64) int64 {
	h.t.Helper()

	h.n++
	res, err := h.st.DB().ExecContext(h.t.Context(),
		`INSERT INTO jobs (transfer_id, kind, digest, size_bytes, artifact_id, source_repo_id,
		                   target_repo_id, target_repository, state, wave, attempts, max_attempts)
		 VALUES (?, 'manifest', ?, 512, ?, ?, ?, 'dest/path', 'leased', 1, 1, 8)`,
		transferID, "sha256:"+strings.Repeat("e", 60)+padded(h.n), artifactID, h.repoID, h.repoID)
	if err != nil {
		h.t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *failureHarness) blobJobFor(transferID, digest string) int64 {
	h.t.Helper()

	res, err := h.st.DB().ExecContext(h.t.Context(),
		`INSERT INTO jobs (transfer_id, kind, digest, size_bytes, source_repo_id, target_repo_id,
		                   state, wave, attempts, max_attempts, skip_reason)
		 VALUES (?, 'blob', ?, 4096, ?, ?, 'skipped', 0, 1, 8, 'exists_at_target')`,
		transferID, digest, h.repoID, h.repoID)
	if err != nil {
		h.t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}
