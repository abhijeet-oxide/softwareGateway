package store

import (
	"testing"
	"time"
)

// archivedAt reads the column back for one package.
func archivedAt(t *testing.T, h *cacheHarness, id int64) *string {
	t.Helper()
	var at *string
	if err := h.packages.DB().QueryRowContext(t.Context(),
		`SELECT archived_at FROM packages WHERE id = ?`, id).Scan(&at); err != nil {
		t.Fatalf("read archived_at: %v", err)
	}
	return at
}

func setPackageState(t *testing.T, h *cacheHarness, id int64, state string) {
	t.Helper()
	if _, err := h.packages.DB().ExecContext(t.Context(),
		`UPDATE packages SET state = ? WHERE id = ?`, state, id); err != nil {
		t.Fatalf("set state: %v", err)
	}
}

// A release the vendor withdrew is marked; the ones still published are not.
//
// This is the whole feature: discovery lists what a repository has, and
// anything recorded here that is no longer in that list has been taken down.
// Until this existed the row was left exactly as it was, so the interface went
// on offering to download something that no longer exists - a request that can
// only fail, late, against a 404 at the registry.
func TestAWithdrawnReleaseIsArchived(t *testing.T) {
	h := newCacheHarness(t)
	gone := h.seedPackageAt("orb_24.6.2871", time.Now().Add(-90*24*time.Hour))
	kept := h.seedPackageAt("orb_24.7.3099", time.Now().Add(-30*24*time.Hour))

	archived, restored, err := h.packages.ArchiveVanishedTags(
		t.Context(), h.repoID, map[string]bool{"orb_24.7.3099": true})
	if err != nil {
		t.Fatal(err)
	}
	if archived != 1 || restored != 0 {
		t.Fatalf("archived=%d restored=%d, want 1 and 0", archived, restored)
	}
	if archivedAt(t, h, gone) == nil {
		t.Error("the release the repository no longer lists was not archived")
	}
	if at := archivedAt(t, h, kept); at != nil {
		t.Errorf("a release the repository still lists was archived at %v", at)
	}
}

// A release we already HAVE is never archived, however the vendor's catalogue
// changes.
//
// The vendor withdrawing a release upstream says nothing about our copy: it is
// in the internal registries, it can still be promoted, and it can still be
// shipped. Marking it archived would be a claim about our own estate that is
// simply false - and it would take the Promote button with it.
func TestADownloadedReleaseIsNeverArchived(t *testing.T) {
	h := newCacheHarness(t)
	landed := h.seedPackageAt("orb_24.6.2871", time.Now().Add(-90*24*time.Hour))

	for _, state := range []string{"transferred", "verified", "queued", "transferring"} {
		setPackageState(t, h, landed, state)
		if _, _, err := h.packages.ArchiveVanishedTags(
			t.Context(), h.repoID, map[string]bool{"something-else": true}); err != nil {
			t.Fatal(err)
		}
		if at := archivedAt(t, h, landed); at != nil {
			t.Errorf("a release in state %q was archived at %v", state, at)
		}
	}
}

// A tag that comes back stops being archived, with nobody's help.
//
// A vendor can restore a tag, and a registry can lie by omission for one scan -
// a proxy serving a truncated catalogue, a paging bug. Neither should need
// somebody to notice and put a row right by hand.
func TestAReturnedReleaseIsRestored(t *testing.T) {
	h := newCacheHarness(t)
	id := h.seedPackageAt("orb_24.6.2871", time.Now().Add(-90*24*time.Hour))

	if _, _, err := h.packages.ArchiveVanishedTags(
		t.Context(), h.repoID, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if archivedAt(t, h, id) == nil {
		t.Fatal("setup: the release was not archived")
	}

	archived, restored, err := h.packages.ArchiveVanishedTags(
		t.Context(), h.repoID, map[string]bool{"orb_24.6.2871": true})
	if err != nil {
		t.Fatal(err)
	}
	if archived != 0 || restored != 1 {
		t.Fatalf("archived=%d restored=%d, want 0 and 1", archived, restored)
	}
	if at := archivedAt(t, h, id); at != nil {
		t.Errorf("a release the vendor published again is still archived at %v", at)
	}
}

// Running the same scan twice archives nothing the second time.
//
// Not tidiness: `archived` is logged and will be alerted on, and a count that
// repeats every scan for a release withdrawn last March is a number nobody can
// read.
func TestArchivingIsIdempotent(t *testing.T) {
	h := newCacheHarness(t)
	h.seedPackageAt("orb_24.6.2871", time.Now().Add(-90*24*time.Hour))

	present := map[string]bool{"orb_24.7.3099": true}
	if _, _, err := h.packages.ArchiveVanishedTags(t.Context(), h.repoID, present); err != nil {
		t.Fatal(err)
	}
	archived, restored, err := h.packages.ArchiveVanishedTags(t.Context(), h.repoID, present)
	if err != nil {
		t.Fatal(err)
	}
	if archived != 0 || restored != 0 {
		t.Errorf("a repeated scan reported archived=%d restored=%d, want 0 and 0",
			archived, restored)
	}
}
