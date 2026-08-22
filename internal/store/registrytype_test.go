package store

import "testing"

// A source declaring `type: jfrog` validated, reconciled, and then failed every
// scan with "CHECK constraint failed: registry_type IN (...)" - a message about
// a column, for a document that was correct.
//
// The two spellings are one backend and the column knows one. Normalising
// beside the constraint it protects is what makes that unreachable rather than
// fixed once, at one of the two call sites.
func TestEnsureRepositoryCanonicalisesJFrog(t *testing.T) {
	st := openTestStore(t)
	p := NewPackages(st)
	ctx := t.Context()

	var productID int64
	if err := st.DB().QueryRowContext(ctx,
		`INSERT INTO products (name, config_hash, config) VALUES ('cfx', 'h', '{}') RETURNING id`,
	).Scan(&productID); err != nil {
		t.Fatal(err)
	}

	tx, err := st.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	id, err := p.EnsureRepository(ctx, tx, productID, "source", "vendor-jfrog",
		"acme.jfrog.io", "docker-local/cfx", "jfrog", "discovery", "")
	if err != nil {
		t.Fatalf("EnsureRepository with type jfrog: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var stored string
	if err := st.DB().QueryRowContext(ctx,
		"SELECT registry_type FROM repositories WHERE id = ?", id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "artifactory" {
		t.Errorf("stored registry_type = %q, want artifactory", stored)
	}
}
