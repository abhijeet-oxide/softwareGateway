package store

import (
	"strings"
	"testing"
)

// blob_placements is read for two different questions, and they need different
// degrees of trust because being wrong costs different things.
//
//	as a SKIP        asserts the content is there and moves nothing.
//	                 Wrong -> the manifest referencing it fails.
//	as a MOUNT HINT  suggests where to relocate from.
//	                 Wrong -> the registry declines and the worker streams.
//
// They shared one 24-hour TTL, which meant the second lost its answer a day
// after the first — and that is expensive in exactly the case that matters
// most. A vendor's bundle carries its release in its repository PATH, so every
// new release lands in a brand-new destination repository with no placements of
// its own. The only thing keeping its blobs off the WAN is a mount from the
// version-stable component repositories the last release wrote to.

func TestAStalePlacementIsStillWorthMountingFrom(t *testing.T) {
	h := newMountHarness(t)

	sibling := h.repo("target", "apm0014228-oci-stage/orbs/cfx-5000-product/nokia-ims-sas")
	// The new release's own repository: a different path, because the release
	// identifier is IN the path. Nothing was ever placed here.
	fresh := h.repo("target", "apm0014228-oci-stage/orbs/cfx-5000-k8s-25.8-9999-ncm")

	digest := "sha256:" + strings.Repeat("a", 64)
	h.place(sibling, digest)
	// A week later, which is a perfectly ordinary gap between two releases.
	h.age(digest, 7*24*60*60)

	// The SKIP must not trust it. That check is what stops us claiming content
	// is somewhere it may have been garbage-collected from.
	placed, err := h.packages.PlacedDigests(t.Context(), sibling, []string{digest})
	if err != nil {
		t.Fatal(err)
	}
	if placed[digest] {
		t.Error("a week-old placement was trusted as proof the content is still there")
	}

	// The MOUNT HINT must. Being wrong costs one request; being absent costs
	// the whole bundle over the WAN a second time.
	near, err := h.packages.MountableFrom(t.Context(), fresh, []string{digest})
	if err != nil {
		t.Fatal(err)
	}
	if near[digest] == "" {
		t.Fatal("no mount candidate for a blob a sibling repository holds; " +
			"the new release will re-stream the whole bundle")
	}
	if !strings.Contains(near[digest], "nokia-ims-sas") {
		t.Errorf("mount candidate = %q, want the sibling holding the blob", near[digest])
	}
}

// The scoping that makes a mount legal is unchanged: registry-internal, and
// under the credential the worker will actually use.
func TestAMountCandidateStaysWithinOneRegistryAndProduct(t *testing.T) {
	h := newMountHarness(t)

	target := h.repo("target", "apm0014228-oci-stage/orbs/thing")
	elsewhere := h.repoOn("target", "other-registry.example.com", "apm0014228-oci-stage/orbs/thing")

	digest := "sha256:" + strings.Repeat("b", 64)
	h.place(elsewhere, digest)

	near, err := h.packages.MountableFrom(t.Context(), target, []string{digest})
	if err != nil {
		t.Fatal(err)
	}
	if near[digest] != "" {
		t.Errorf("offered a mount from %q on a different registry; a mount is "+
			"registry-internal and this one cannot work", near[digest])
	}
}

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

type mountHarness struct {
	*transferRefHarness
	productID int64
	n         int
}

func newMountHarness(t *testing.T) *mountHarness {
	t.Helper()

	h := &mountHarness{transferRefHarness: newTransferRefHarness(t)}
	if err := h.st.DB().QueryRowContext(t.Context(),
		`SELECT id FROM products LIMIT 1`).Scan(&h.productID); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *mountHarness) repo(role, path string) int64 {
	h.t.Helper()
	return h.repoOn(role, "artifact.it.att.com", path)
}

func (h *mountHarness) repoOn(role, host, path string) int64 {
	h.t.Helper()

	h.n++
	tx, err := h.st.DB().BeginTx(h.t.Context(), nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	id, err := h.packages.EnsureRepository(h.t.Context(), tx, h.productID, role,
		"r"+strings.Repeat("x", h.n), host, path, "generic", "config", "")
	if err != nil {
		h.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *mountHarness) place(repoID int64, digest string) {
	h.t.Helper()

	tx, err := h.st.DB().BeginTx(h.t.Context(), nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := h.packages.RecordPlacement(h.t.Context(), tx, repoID, digest, 4096,
		"transferred"); err != nil {
		h.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		h.t.Fatal(err)
	}
}

// age pushes a placement's verification stamp into the past.
func (h *mountHarness) age(digest string, seconds int) {
	h.t.Helper()

	if _, err := h.st.DB().ExecContext(h.t.Context(),
		`UPDATE blob_placements
		    SET verified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now', ?)
		  WHERE digest = ?`, "-"+itoa(seconds)+" seconds", digest); err != nil {
		h.t.Fatal(err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
