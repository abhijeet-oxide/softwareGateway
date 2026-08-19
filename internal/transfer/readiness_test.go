package transfer_test

import (
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// Per-artifact readiness and the site ranking, end to end against the bundle
// fixture - the two things the queue's own tests can only assert in the
// abstract.

// A component published under its own name is the SAME digest already bound for
// a sibling repository, so the second copy is a cross-repository mount rather
// than a wide-area transfer. That used to depend on insertion order separating
// the two, which is not something the schema said. site_rank says it.
func TestTheSecondSiteOfAComponentIsRankedBehindTheFirst(t *testing.T) {
	s := newSlice(t)
	pkg, components := seedORB(t, s, "orb_23.8.1076")
	s.plan(pkg, "t1")

	published := components[0]

	if got := s.count(`SELECT COUNT(*) FROM jobs
	                    WHERE kind='manifest' AND digest=? AND target_repository=? AND site_rank=1`,
		published.digest, published.destination); got != 1 {
		t.Errorf("the copy published under %q is not ranked 1", published.destination)
	}
	if got := s.count(`SELECT COUNT(*) FROM jobs
	                    WHERE kind='manifest' AND digest=? AND site_rank=0`,
		published.digest); got != 1 {
		t.Errorf("%d rank-0 copies of the component, want the one inside the bundle", got)
	}
}

// The claim per-artifact readiness exists to make: a manifest becomes runnable
// on ITS OWN content, without waiting for blobs belonging to anything else.
//
// The bundle is the case that matters - four components, each independent of
// the others - and under wave gating not one of them could be written until
// every blob of all four had landed.
func TestAComponentIsPushableBeforeTheRestOfTheBundleHasMoved(t *testing.T) {
	s := newSlice(t)
	pkg, components := seedORB(t, s, "orb_23.8.1076")
	s.plan(pkg, "t1")

	target := components[0]

	if got := s.manifestState(target.digest); got != "blocked" {
		t.Fatalf("the component starts %q, want blocked; the fixture would prove nothing", got)
	}

	// Finish exactly the blobs this one component references, and nothing else.
	blobs := s.blobJobsOf(target.digest)
	if len(blobs) == 0 {
		t.Fatal("the component references no blob jobs")
	}
	for _, id := range blobs {
		s.succeedJob(id)
	}

	if got := s.manifestState(target.digest); got != "pending" {
		t.Errorf("the component manifest is %q once its own blobs landed, want pending", got)
	}

	// And the rest of the bundle has NOT moved. Both halves matter: if every
	// blob had finished, this would not distinguish per-artifact readiness from
	// the wave barrier it replaced.
	if outstanding := s.count(
		`SELECT COUNT(*) FROM jobs WHERE kind='blob' AND state='pending'`); outstanding == 0 {
		t.Fatal("every blob finished; the test cannot tell per-artifact from wave gating")
	}
	// Invariant I1, holding at the same moment: the ORB index names components
	// that have not been pushed, so it waits.
	if got := s.manifestState(pkg.ManifestDigest); got != "blocked" {
		t.Errorf("the ORB index is %q while its components are unpushed, want blocked", got)
	}
}

// ---------------------------------------------------------------------------
// harness additions
// ---------------------------------------------------------------------------

func (s *slice) count(query string, args ...any) int {
	s.t.Helper()

	var n int
	if err := s.st.DB().QueryRowContext(s.t.Context(), query, args...).Scan(&n); err != nil {
		s.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// manifestState is the state of the copy that keeps the bundle resolvable -
// the one every other placement of the same digest is ranked behind.
func (s *slice) manifestState(digest string) string {
	s.t.Helper()

	var state string
	if err := s.st.DB().QueryRowContext(s.t.Context(),
		`SELECT state FROM jobs WHERE kind='manifest' AND digest=? AND site_rank=0`,
		digest).Scan(&state); err != nil {
		s.t.Fatalf("state of manifest %s: %v", digest, err)
	}
	return state
}

// blobJobsOf lists the blob jobs an artifact's manifest references, wherever
// they are bound.
func (s *slice) blobJobsOf(manifestDigest string) []int64 {
	s.t.Helper()

	rows, err := s.st.DB().QueryContext(s.t.Context(), `
		SELECT DISTINCT j.id
		  FROM jobs j
		  JOIN artifact_blobs ab ON ab.digest = j.digest
		  JOIN package_artifacts a ON a.id = ab.artifact_id
		 WHERE a.digest = ? AND j.kind = 'blob'
		 ORDER BY j.id`, manifestDigest)
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			s.t.Fatal(err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		s.t.Fatal(err)
	}
	return out
}

// succeedJob reports a job done through the REAL completion path, so the
// promotion under test is the one a worker's report triggers.
func (s *slice) succeedJob(jobID int64) {
	s.t.Helper()

	if _, err := s.st.DB().ExecContext(s.t.Context(),
		`UPDATE jobs SET state='leased', lease_owner='worker-1', attempts=1 WHERE id = ?`,
		jobID); err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.packages.CompleteJob(s.t.Context(), store.Completion{
		JobID: jobID, Owner: "worker-1", Outcome: "succeeded", Attempt: 1, Placed: true,
	}); err != nil {
		s.t.Fatal(err)
	}
}
