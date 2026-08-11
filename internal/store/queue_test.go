package store

import (
	"strings"
	"testing"
)

// A listing shortens every transfer ID to its first segment, and then tells
// the reader to run `transfers describe <that>`. If describe would not accept
// it, the output is a trap — which is exactly what it was.

func TestTransferIDResolvesFromAPrefix(t *testing.T) {
	h := newTransferRefHarness(t)

	full := h.transfer("4d882940-1111-2222-3333-444444444444")
	h.transfer("9c1e8f2a-5555-6666-7777-888888888888")

	for _, ref := range []string{full, "4d882940", "4d"} {
		got, err := h.packages.ResolveTransferID(t.Context(), ref)
		if err != nil {
			t.Errorf("%q did not resolve: %v", ref, err)
			continue
		}
		if got != full {
			t.Errorf("%q resolved to %s, want %s", ref, got, full)
		}
	}
}

// An ambiguous prefix must name the candidates rather than pick one: acting on
// the wrong transfer is worse than being asked to type more.
func TestAmbiguousTransferPrefixIsRefused(t *testing.T) {
	h := newTransferRefHarness(t)
	a := h.transfer("4d882940-1111-2222-3333-444444444444")
	b := h.transfer("4d882941-5555-6666-7777-888888888888")

	_, err := h.packages.ResolveTransferID(t.Context(), "4d88")
	if err == nil {
		t.Fatal("an ambiguous prefix resolved rather than being refused")
	}
	for _, want := range []string{a, b} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name candidate %s: %v", want, err)
		}
	}
}

func TestUnknownTransferPrefixSaysSo(t *testing.T) {
	h := newTransferRefHarness(t)
	h.transfer("4d882940-1111-2222-3333-444444444444")

	if _, err := h.packages.ResolveTransferID(t.Context(), "deadbeef"); err == nil {
		t.Fatal("an unknown prefix resolved")
	}
}

// A prefix carrying a LIKE wildcard must not become a pattern.
func TestTransferPrefixWildcardsAreLiteral(t *testing.T) {
	h := newTransferRefHarness(t)
	h.transfer("4d882940-1111-2222-3333-444444444444")

	if _, err := h.packages.ResolveTransferID(t.Context(), "%"); err == nil {
		t.Fatal("a bare wildcard matched every transfer instead of nothing")
	}
}

type transferRefHarness struct {
	t        *testing.T
	st       Store
	packages *Packages
	pkgID    int64
	repoID   int64
}

func newTransferRefHarness(t *testing.T) *transferRefHarness {
	t.Helper()

	st := openTestStore(t)
	h := &transferRefHarness{t: t, st: st, packages: NewPackages(st)}

	res, err := st.DB().ExecContext(t.Context(),
		`INSERT INTO products (name, config_hash, config) VALUES ('p','h','{}')`)
	if err != nil {
		t.Fatal(err)
	}
	productID, _ := res.LastInsertId()

	tx, err := st.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	h.repoID, err = h.packages.EnsureRepository(t.Context(), tx, productID, "source", "src",
		"registry.example.com", "vendor/suite", "generic", "config", "")
	if err != nil {
		t.Fatal(err)
	}
	h.pkgID, err = h.packages.InsertPackage(t.Context(), tx, PackageRow{
		ProductID: productID, SourceRepoID: h.repoID, Tag: "v1",
		ManifestDigest: "sha256:" + strings.Repeat("a", 64), MediaType: "application/json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *transferRefHarness) transfer(id string) string {
	h.t.Helper()

	if _, err := h.st.DB().ExecContext(h.t.Context(),
		`INSERT INTO transfer_requests (id, product_id, package_id, operation, source_repo_id, idempotency_key)
		 SELECT ?, product_id, id, 'replicate', source_repo_id, ? FROM packages WHERE id = ?`,
		"req-"+id, "key-"+id, h.pkgID); err != nil {
		h.t.Fatal(err)
	}

	tx, err := h.st.DB().BeginTx(h.t.Context(), nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := h.packages.CreateTransfer(h.t.Context(), tx, TransferRow{
		ID: id, RequestID: "req-" + id, PackageID: h.pkgID,
		SourceRepoID: h.repoID, TargetRepoID: h.repoID, Priority: 50,
	}); err != nil {
		h.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		h.t.Fatal(err)
	}
	return id
}
