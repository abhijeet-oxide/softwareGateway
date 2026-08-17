package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func replicationFixture(t *testing.T) (*Replication, int64, context.Context) {
	t.Helper()
	s := openTestStore(t)
	ctx := context.Background()

	var productID int64
	err := s.DB().QueryRowContext(ctx,
		`INSERT INTO products (name, config_hash, config) VALUES ('vendor-a', 'h', '{}') RETURNING id`,
	).Scan(&productID)
	if err != nil {
		t.Fatalf("seed product: %v", err)
	}
	return NewReplication(s), productID, ctx
}

// "Never applied" and "applied with empty hashes" are different states, and
// only one of them means somebody should run apply.
func TestNoRecordIsItsOwnAnswer(t *testing.T) {
	r, productID, ctx := replicationFixture(t)

	if _, err := r.Get(ctx, productID, "ocp-prod"); !errors.Is(err, ErrNoRecord) {
		t.Fatalf("want ErrNoRecord, got %v", err)
	}
}

func TestAppliedIsUpsertedNotAppended(t *testing.T) {
	r, productID, ctx := replicationFixture(t)
	now := time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC)

	if err := r.Applied(ctx, productID, "ocp-prod", "mirror", "cfg1", "sec1", "alice", now); err != nil {
		t.Fatalf("Applied: %v", err)
	}
	if err := r.Applied(ctx, productID, "ocp-prod", "mirror", "cfg2", "sec2", "bob", now.Add(time.Hour)); err != nil {
		t.Fatalf("Applied again: %v", err)
	}

	got, err := r.Get(ctx, productID, "ocp-prod")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AppliedConfigHash != "cfg2" || got.AppliedBy != "bob" {
		t.Fatalf("the current applied state must win: %+v", got)
	}

	all, err := r.List(ctx, "vendor-a")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("two applies of one target must be one row, got %d", len(all))
	}
}

// An apply closes drift by definition — we have just made the registry say
// what the configuration says. Leaving the flag set reports a fault the
// operator has already fixed, which is how a drift signal stops being read.
func TestApplyClosesDrift(t *testing.T) {
	r, productID, ctx := replicationFixture(t)
	now := time.Now().UTC()

	if err := r.Observed(ctx, productID, "ocp-prod", "mirror", true, "tags differ", now); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	before, _ := r.Get(ctx, productID, "ocp-prod")
	if !before.DriftDetected {
		t.Fatal("expected drift to be recorded")
	}

	if err := r.Applied(ctx, productID, "ocp-prod", "mirror", "cfg", "sec", "alice", now); err != nil {
		t.Fatalf("Applied: %v", err)
	}
	after, _ := r.Get(ctx, productID, "ocp-prod")
	if after.DriftDetected || after.DriftSummary != "" {
		t.Fatalf("an apply must close drift, got %+v", after)
	}
}

// Observe runs on every reload and from a metrics sweep. It must never disturb
// applied_config_hash, which is the only fixed point drift is measured from.
func TestObserveDoesNotTouchTheAppliedRecord(t *testing.T) {
	r, productID, ctx := replicationFixture(t)
	now := time.Now().UTC()

	if err := r.Applied(ctx, productID, "ocp-prod", "mirror", "cfg1", "sec1", "alice", now); err != nil {
		t.Fatalf("Applied: %v", err)
	}
	if err := r.Observed(ctx, productID, "ocp-prod", "mirror", true, "robot differs", now); err != nil {
		t.Fatalf("Observed: %v", err)
	}

	got, err := r.Get(ctx, productID, "ocp-prod")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AppliedConfigHash != "cfg1" || got.AppliedSecretHash != "sec1" || got.AppliedBy != "alice" {
		t.Fatalf("an observation overwrote the apply record: %+v", got)
	}
	if !got.DriftDetected || got.DriftSummary != "robot differs" {
		t.Fatalf("the observation was not recorded: %+v", got)
	}
}

// Observing a target we have never applied must not invent an apply.
func TestObserveBeforeAnyApply(t *testing.T) {
	r, productID, ctx := replicationFixture(t)

	if err := r.Observed(ctx, productID, "ocp-prod", "mirror", true, "not configured", time.Now()); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	got, err := r.Get(ctx, productID, "ocp-prod")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AppliedConfigHash != "" || got.AppliedAt.Valid {
		t.Fatalf("an observation must not imply an apply: %+v", got)
	}
}

func TestSyncHistoryRecordsOutcomes(t *testing.T) {
	r, productID, ctx := replicationFixture(t)
	now := time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC)

	id, err := r.RecordSyncRequested(ctx, productID, "ocp-prod", sql.NullInt64{}, "sha256:expected", now)
	if err != nil {
		t.Fatalf("RecordSyncRequested: %v", err)
	}

	err = r.UpdateSync(ctx, id, MirrorSync{
		Status: SyncDiverged, QuayStatus: "SUCCESS",
		FinishedAt:     sql.NullString{String: "2026-09-01T03:00:00.000Z", Valid: true},
		ObservedDigest: "sha256:different",
		Message:        "the upstream tag moved",
	})
	if err != nil {
		t.Fatalf("UpdateSync: %v", err)
	}

	got, err := r.ListSyncs(ctx, "vendor-a", "ocp-prod", 10)
	if err != nil {
		t.Fatalf("ListSyncs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d syncs", len(got))
	}
	s := got[0]
	// Quay said SUCCESS. We say diverged. Both are recorded, because the
	// question "did Quay do what we asked" needs both halves.
	if s.Status != SyncDiverged || s.QuayStatus != "SUCCESS" {
		t.Fatalf("status = %q / quay %q", s.Status, s.QuayStatus)
	}
	if s.ObservedDigest != "sha256:different" || s.ExpectedDigest != "sha256:expected" {
		t.Fatalf("digests = observed %q expected %q", s.ObservedDigest, s.ExpectedDigest)
	}
	if !s.RequestedAt.Valid || !s.FinishedAt.Valid {
		t.Fatalf("timestamps = %+v", s)
	}
}

// A sync Quay ran on its own schedule has no transfer, which is the normal
// case and the whole point of mirroring.
func TestSyncWithoutATransfer(t *testing.T) {
	r, productID, ctx := replicationFixture(t)

	id, err := r.RecordSyncRequested(ctx, productID, "ocp-prod", sql.NullInt64{}, "", time.Now())
	if err != nil {
		t.Fatalf("RecordSyncRequested: %v", err)
	}
	if id == 0 {
		t.Fatal("expected an id")
	}
	got, _ := r.ListSyncs(ctx, "vendor-a", "ocp-prod", 10)
	if got[0].TransferID.Valid {
		t.Fatal("a scheduled sync has no transfer")
	}
}

// An unrecognised Quay status must survive as `unknown` plus its raw value.
func TestUnknownSyncStatusIsStorable(t *testing.T) {
	r, productID, ctx := replicationFixture(t)

	id, err := r.RecordSyncRequested(ctx, productID, "ocp-prod", sql.NullInt64{}, "", time.Now())
	if err != nil {
		t.Fatalf("RecordSyncRequested: %v", err)
	}
	if err := r.UpdateSync(ctx, id, MirrorSync{Status: SyncUnknown, QuayStatus: "QUIESCING"}); err != nil {
		t.Fatalf("UpdateSync: %v", err)
	}
	got, _ := r.ListSyncs(ctx, "vendor-a", "ocp-prod", 10)
	if got[0].Status != SyncUnknown || got[0].QuayStatus != "QUIESCING" {
		t.Fatalf("got %+v", got[0])
	}
}

// The CHECK constraint is the second line of defence against a state the
// domain does not define.
func TestUnknownStatusValueIsRejectedByTheDatabase(t *testing.T) {
	r, productID, ctx := replicationFixture(t)

	_, err := r.db.ExecContext(ctx, r.dialect.Rewrite(
		`INSERT INTO mirror_syncs (product_id, target_name, status) VALUES (?, ?, 'nearly')`),
		productID, "ocp-prod")
	if err == nil {
		t.Fatal("the database must refuse a status the domain does not define")
	}
}

func TestUnknownModeIsRejectedByTheDatabase(t *testing.T) {
	r, productID, ctx := replicationFixture(t)

	err := r.Applied(ctx, productID, "ocp-prod", "sync", "cfg", "sec", "alice", time.Now())
	if err == nil {
		t.Fatal("the database must refuse a mode outside copy/mirror/proxy")
	}
}

func TestListIsScopedToOneProduct(t *testing.T) {
	r, productID, ctx := replicationFixture(t)
	var other int64
	if err := r.db.QueryRowContext(ctx,
		`INSERT INTO products (name, config_hash, config) VALUES ('vendor-b', 'h', '{}') RETURNING id`,
	).Scan(&other); err != nil {
		t.Fatalf("seed second product: %v", err)
	}

	_ = r.Applied(ctx, productID, "ocp-prod", "mirror", "a", "", "", time.Now())
	_ = r.Applied(ctx, other, "ocp-dev", "proxy", "b", "", "", time.Now())

	mine, err := r.List(ctx, "vendor-a")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(mine) != 1 || mine[0].TargetName != "ocp-prod" {
		t.Fatalf("got %+v", mine)
	}

	all, err := r.List(ctx, "")
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d rows across products", len(all))
	}
}
