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

// A Helm chart is an ordinary image manifest whose CONFIG says what it is.
//
// This is the shape Helm has always published — `artifactType` arrived in OCI
// 1.1 and Helm predates it — so a breakdown reading only the manifest's own two
// fields sees an image manifest with nothing to distinguish it. It reported 257
// images for an orb the vendor's own catalogue lists as 157 images and 97
// charts: not a rounding difference, a whole category made invisible.
func TestAChartIsRecognisedByItsConfig(t *testing.T) {
	h := newFailureHarness(t)
	id := h.transferWithJobs(0)

	chart := h.seedArtifact("application/vnd.oci.image.manifest.v1+json", "")
	h.seedConfig(chart, "application/vnd.cncf.helm.config.v1+json")
	h.jobForArtifact(id, chart, "skipped")

	image := h.seedArtifact("application/vnd.oci.image.manifest.v1+json", "")
	h.seedConfig(image, "application/vnd.oci.image.config.v1+json")
	h.jobForArtifact(id, image, "succeeded")

	rows, err := h.packages.ContentBreakdown(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]string{}
	for _, row := range rows {
		got[row.ConfigMediaType] = row.Outcome
	}
	if outcome := got["application/vnd.cncf.helm.config.v1+json"]; outcome != ContentPresent {
		t.Errorf("the chart's config was not carried through: %+v", rows)
	}
	if outcome := got["application/vnd.oci.image.config.v1+json"]; outcome != ContentCopied {
		t.Errorf("the image's config was not carried through: %+v", rows)
	}
}

// seedConfig gives a component the config blob that says what it is.
func (h *failureHarness) seedConfig(artifactID int64, mediaType string) {
	h.t.Helper()

	h.n++
	digest := "sha256:" + strings.Repeat("9", 60) + padded(h.n)
	h.exec(`INSERT INTO blobs (digest, size_bytes, media_type) VALUES (?, 512, ?)`,
		digest, mediaType)
	h.exec(`INSERT INTO artifact_blobs (artifact_id, digest, kind, ordinal)
	        VALUES (?, ?, 'config', 0)`, artifactID, digest)
}

// The vendor's own annotations reach the caller, because for some vendors they
// are the ONLY evidence.
//
// A NEAR orb's charts are plain image manifests carrying an ordinary image
// config: media type, artifact type and config media type are identical for its
// charts and its images. The store must not try to read the annotation — naming
// `com.nokia.ncd.orb.type` here would be vendor knowledge in the wrong place —
// but it must carry it out, and it must not fold two components that disagree
// about it into one row.
func TestTheBreakdownCarriesAnnotationsOutVerbatim(t *testing.T) {
	h := newFailureHarness(t)
	id := h.transferWithJobs(0)

	const manifest = "application/vnd.oci.image.manifest.v1+json"

	chart := h.seedArtifact(manifest, "")
	h.seedAnnotations(chart, `{"com.nokia.ncd.orb.type":"helmchart"}`)
	h.seedConfig(chart, "application/vnd.oci.image.config.v1+json")
	h.jobForArtifact(id, chart, "succeeded")

	image := h.seedArtifact(manifest, "")
	h.seedAnnotations(image, `{"com.nokia.ncd.orb.type":"cnfimage"}`)
	h.seedConfig(image, "application/vnd.oci.image.config.v1+json")
	h.jobForArtifact(id, image, "succeeded")

	rows, err := h.packages.ContentBreakdown(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]ContentRow{}
	for _, row := range rows {
		got[row.Annotations["com.nokia.ncd.orb.type"]] = row
	}
	if len(got) != 2 {
		t.Fatalf("two components indistinguishable but for their annotations "+
			"came out as %d rows: %+v", len(rows), rows)
	}
	for _, orbType := range []string{"helmchart", "cnfimage"} {
		if row := got[orbType]; row.Count != 1 {
			t.Errorf("%s counted %d, want 1: %+v", orbType, row.Count, rows)
		}
	}
}

// seedAnnotations gives a component the annotations its vendor wrote on it.
func (h *failureHarness) seedAnnotations(artifactID int64, annotations string) {
	h.t.Helper()
	h.exec(`UPDATE package_artifacts SET annotations = ? WHERE id = ?`,
		annotations, artifactID)
}

// A release's files are the layers its publisher NAMED.
//
// `org.opencontainers.image.title` is the ORAS convention for a single-file
// layer, and it is the only place a file's name exists — the blob is bytes. It
// is recorded when the release is analysed, so listing files troubles no
// registry afterwards.
//
// An untitled layer is a tar of an unknown number of paths. It is counted and
// not listed, because one row called `layer sha256:…` is a summary wearing the
// clothes of an answer.
func TestPackageFilesListsWhatThePublisherNamed(t *testing.T) {
	h := newFailureHarness(t)

	artifact := h.seedArtifact("application/vnd.oci.image.manifest.v1+json", "")
	h.seedAnnotations(artifact, `{"org.opencontainers.image.ref.name":"orbs/cfx/custo:25.7"}`)
	h.seedTitledLayer(artifact, 0, "CONFIGURATION/nodes.json", 4096)
	h.seedTitledLayer(artifact, 1, "CONFIGURATION/network.json", 2048)

	image := h.seedArtifact("application/vnd.oci.image.manifest.v1+json", "")
	h.seedTitledLayer(image, 0, "", 900_000_000)

	files, opaque, err := h.packages.PackageFiles(t.Context(), h.pkgID)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("listed %d files, want the two that were named: %+v", len(files), files)
	}
	if files[0].Path != "CONFIGURATION/nodes.json" || files[1].Path != "CONFIGURATION/network.json" {
		t.Errorf("files came out %q, %q — want them in layer order",
			files[0].Path, files[1].Path)
	}
	if files[0].ArtifactRef != "orbs/cfx/custo:25.7" {
		t.Errorf("component = %q, want the artifact's own name", files[0].ArtifactRef)
	}
	if files[0].SizeBytes != 4096 {
		t.Errorf("size = %d, want the layer's size", files[0].SizeBytes)
	}
	if opaque != 1 {
		t.Errorf("opaque layers = %d, want the one nobody named", opaque)
	}
}

// seedTitledLayer gives an artifact a layer, named or not.
func (h *failureHarness) seedTitledLayer(artifactID int64, ordinal int, title string, size int64) {
	h.t.Helper()

	h.n++
	digest := "sha256:" + strings.Repeat("7", 60) + padded(h.n)
	h.exec(`INSERT INTO blobs (digest, size_bytes, media_type)
	        VALUES (?, ?, 'application/vnd.oci.image.layer.v1.tar')`, digest, size)
	h.exec(`INSERT INTO artifact_blobs (artifact_id, digest, kind, ordinal, title)
	        VALUES (?, ?, 'layer', ?, ?)`, artifactID, digest, ordinal, nullable(title))
}
