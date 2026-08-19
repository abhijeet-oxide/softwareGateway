package transfer_test

import "testing"

// THE TWO SIZES, settled against a ledger rather than argued from the code.
//
// A release page says an orb weighs 29.8 GB and its download says `of 63.7 GB`,
// and the difference reads as a bug. The claim is that both are true and mean
// different things:
//
//	total_bytes   what the release IS   - each distinct digest counted ONCE
//	planned_bytes what landing it COSTS - each (repository, digest) counted once
//
// and that the gap is real work, because a registry stores content PER
// REPOSITORY and a component published under its own name as well as inside the
// bundle needs its manifest and its layers under both paths.
//
// Reading that off the two functions is not evidence, so this measures it. The
// fake registry keeps a ledger of what actually arrived where, and the first
// assertion is against the ledger: one digest, reaching two destination
// repositories, in one transfer.
func TestTheReleaseSizeAndThePushCostAreBothTrue(t *testing.T) {
	s := newSlice(t)
	pkg, _ := seedORB(t, s, "orb_23.8.1076")

	plan := s.plan(pkg, "sizes")
	if got := s.drain("worker-1", 8); got.Failed != 0 {
		t.Fatalf("the transfer failed: %v", got.LastError)
	}

	// 1. SOMETHING GENUINELY GOES TO TWO PLACES. Found from the jobs rather
	//    than named by the test, so this measures what the planner decided
	//    instead of what the fixture author expected.
	digest, repositories := twiceDestined(t, s, "sizes")
	if digest == "" {
		t.Fatal("no blob in this transfer is destined for two repositories, so " +
			"the fixture cannot demonstrate the gap at all")
	}

	// 2. AND IT ARRIVED IN BOTH. Uploaded or mounted - a mount is the registry
	//    relocating it internally, which is the second copy costing no bytes
	//    rather than the second copy not happening.
	for _, repo := range repositories {
		if s.dst.BlobUploads(repo, digest)+s.dst.BlobMounts(repo, digest) == 0 {
			t.Errorf("nothing put %s into %s. If content did NOT have to reach both "+
				"its container and its own name, the push cost being larger than the "+
				"release size would have no cause", digest, repo)
		}
	}

	// 3. THE ARITHMETIC, from the jobs the plan wrote - the same rows the
	//    interface reads and the same query somebody would run by hand.
	distinct, perSite := jobBytes(t, s, "sizes")

	if plan.PlannedBytes != perSite {
		t.Errorf("planned = %d, want %d - the planned figure is not the "+
			"per-(repository, digest) sum, so the explanation of it is wrong",
			plan.PlannedBytes, perSite)
	}
	if perSite <= distinct {
		t.Fatalf("per-site = %d and distinct = %d: nothing is published twice here",
			perSite, distinct)
	}

	// 4. AND THE RELEASE'S OWN SIZE IS THE DISTINCT ONE. It must not grow
	//    because a transfer was planned against a second repository - the
	//    release did not get bigger.
	row, err := s.packages.GetPackageByID(t.Context(), pkg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.TotalBytes == nil {
		t.Fatal("the release has no measured size, so there is nothing to compare")
	}
	if *row.TotalBytes != distinct {
		t.Errorf("the release measures %d and its distinct job bytes come to %d: "+
			"the two ways of counting the same content disagree",
			*row.TotalBytes, distinct)
	}
}

// twiceDestined finds content this transfer sends to more than one repository.
//
// Data-driven on purpose: the test asserts what the planner decided, not what
// the fixture author remembers naming.
func twiceDestined(t *testing.T, s *slice, transferID string) (string, []string) {
	t.Helper()

	var digest string
	err := s.st.DB().QueryRowContext(t.Context(), `
		SELECT digest FROM jobs
		 WHERE transfer_id = ? AND kind = 'blob'
		 GROUP BY digest
		HAVING count(DISTINCT target_repository) > 1
		 LIMIT 1`, transferID).Scan(&digest)
	if err != nil {
		return "", nil
	}

	rows, err := s.st.DB().QueryContext(t.Context(),
		`SELECT DISTINCT target_repository FROM jobs WHERE transfer_id = ? AND digest = ?`,
		transferID, digest)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var repo string
		if err := rows.Scan(&repo); err != nil {
			t.Fatal(err)
		}
		out = append(out, repo)
	}
	return digest, out
}

// jobBytes returns what a transfer's jobs come to, counted both ways.
//
// Deliberately SQL over the jobs table rather than a call into the planner: it
// is the same query somebody would run against a live Coordinator to check the
// same claim, so a passing test and a hand-check are the same measurement.
//
// EVERY job, not only the blobs - a manifest published under a second name is a
// second manifest push, and it is part of the cost for the same reason its
// layers are.
func jobBytes(t *testing.T, s *slice, transferID string) (distinct, perSite int64) {
	t.Helper()

	if err := s.st.DB().QueryRowContext(t.Context(), `
		SELECT COALESCE(SUM(size_bytes), 0) FROM (
			SELECT DISTINCT digest, size_bytes FROM jobs WHERE transfer_id = ?
		) AS d`, transferID).Scan(&distinct); err != nil {
		t.Fatal(err)
	}
	if err := s.st.DB().QueryRowContext(t.Context(),
		`SELECT COALESCE(SUM(size_bytes), 0) FROM jobs WHERE transfer_id = ?`,
		transferID).Scan(&perSite); err != nil {
		t.Fatal(err)
	}
	return distinct, perSite
}
