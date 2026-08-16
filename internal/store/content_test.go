package store

import (
	"strings"
	"testing"
)

// What a transfer is made of, and how each component went.
//
// The reported gap: a listing that says `2486/2489 jobs` and `63.7 GiB` and
// cannot answer "did the charts land?" — a question about the release, asked in
// the terms the release is published in.

func TestTheBreakdownCountsComponentsByWhatTheyAreAndHowTheyWent(t *testing.T) {
	h := newFailureHarness(t)
	id := h.transferWithJobs(0)

	image := h.seedArtifact("application/vnd.oci.image.manifest.v1+json", "")
	chart := h.seedArtifact("application/vnd.oci.image.manifest.v1+json",
		"application/vnd.cncf.helm.config.v1+json")
	file := h.seedArtifact("application/vnd.oci.image.manifest.v1+json",
		"application/vnd.nokia.generic_custo")

	h.jobForArtifact(id, image, "succeeded")
	h.jobForArtifact(id, chart, "skipped")
	h.jobForArtifact(id, file, "failed")

	rows, err := h.packages.ContentBreakdown(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]ContentRow{}
	for _, row := range rows {
		got[row.ArtifactType] = row
	}

	for _, want := range []struct {
		artifactType, outcome string
	}{
		{"", ContentCopied},
		{"application/vnd.cncf.helm.config.v1+json", ContentPresent},
		{"application/vnd.nokia.generic_custo", ContentFailed},
	} {
		row, ok := got[want.artifactType]
		if !ok {
			t.Fatalf("no row for artifact type %q: %+v", want.artifactType, rows)
		}
		if row.Outcome != want.outcome {
			t.Errorf("%q came out %q, want %q", want.artifactType, row.Outcome, want.outcome)
		}
		if row.Count != 1 {
			t.Errorf("%q counted %d components, want 1", want.artifactType, row.Count)
		}
	}
}

// A component published under two names is ONE component.
//
// The transfer pushes it twice — once where the bundle resolves it, once under
// the name the vendor gave it — and counting jobs would report every component
// of an orb twice.
func TestAComponentInTwoPlacesIsCountedOnce(t *testing.T) {
	h := newFailureHarness(t)
	id := h.transferWithJobs(0)

	component := h.seedArtifact("application/vnd.oci.image.manifest.v1+json", "")
	h.jobForArtifact(id, component, "succeeded")
	h.jobForArtifact(id, component, "succeeded")

	rows, err := h.packages.ContentBreakdown(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Count != 1 {
		t.Fatalf("a component in two repositories counted as %+v, want one component", rows)
	}
}

// The worst outcome wins. A component whose second site failed has not arrived,
// whatever its first site did — and reporting it as copied would put a failure
// in the copied column.
func TestAComponentThatFailedAnywhereIsFailed(t *testing.T) {
	h := newFailureHarness(t)
	id := h.transferWithJobs(0)

	component := h.seedArtifact("application/vnd.oci.image.manifest.v1+json", "")
	h.jobForArtifact(id, component, "succeeded")
	h.jobForArtifact(id, component, "failed")

	rows, err := h.packages.ContentBreakdown(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Outcome != ContentFailed {
		t.Fatalf("outcome = %+v, want the failure to win", rows)
	}
}

// A component the plan has not reached yet is outstanding, not missing. A
// transfer still planning has artifacts and no jobs at all.
func TestAComponentWithNoJobIsOutstanding(t *testing.T) {
	h := newFailureHarness(t)
	id := h.transferWithJobs(0)
	h.seedArtifact("application/vnd.oci.image.manifest.v1+json", "")

	rows, err := h.packages.ContentBreakdown(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Outcome != ContentOutstanding {
		t.Fatalf("outcome = %+v, want it outstanding", rows)
	}
}

// seedArtifact adds one component to the harness's package.
func (h *failureHarness) seedArtifact(mediaType, artifactType string) int64 {
	h.t.Helper()

	h.n++
	res, err := h.st.DB().ExecContext(h.t.Context(),
		`INSERT INTO package_artifacts
		        (package_id, digest, media_type, artifact_type, size_bytes, depth)
		 VALUES (?, ?, ?, ?, 512, 1)`,
		h.pkgID, "sha256:"+strings.Repeat("f", 60)+padded(h.n), mediaType,
		nullable(artifactType))
	if err != nil {
		h.t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

// jobForArtifact adds one manifest job for a component, in a given state.
func (h *failureHarness) jobForArtifact(transferID string, artifactID int64, state string) {
	h.t.Helper()

	h.n++
	h.exec(`INSERT INTO jobs (transfer_id, kind, digest, size_bytes, artifact_id,
	                          source_repo_id, target_repo_id, target_repository,
	                          state, wave, attempts, max_attempts)
	        VALUES (?, 'manifest', ?, 512, ?, ?, ?, 'dest/path', ?, 1, 1, 8)`,
		transferID, "sha256:"+strings.Repeat("b", 60)+padded(h.n), artifactID,
		h.repoID, h.repoID, state)
}
