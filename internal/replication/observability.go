package replication

import (
	"context"

	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// Metrics and audit for delegated replication.
//
// The audit catalogue here records what we DID or OBSERVED, never
// "transferred" — which we did not. That distinction is the whole reason these
// events exist as a category of their own rather than reusing the transfer
// events: a year from now, "we asked Quay to sync and Quay says it did" and
// "we moved 45 GB" must not read the same way.

// Audit event types (docs/design/12 §4.1, category `Replication`).
const (
	AuditConfigApplied   = "ReplicationConfigApplied"
	AuditConfigDrifted   = "ReplicationConfigDrifted"
	AuditSyncRequested   = "MirrorSyncRequested"
	AuditSyncSucceeded   = "MirrorSyncSucceeded"
	AuditSyncFailed      = "MirrorSyncFailed"
	AuditContentDiverge  = "MirrorContentDiverged"
	AuditProxyConfigured = "ProxyCacheConfigured"
	AuditCacheWarmed     = "CacheWarmed"
)

// Metrics is the slice of the registry this package touches.
//
// A consumer-defined interface so the service and the watcher can be
// constructed without one — which is what every test does, and what a
// deployment with metrics disabled does.
type Metrics interface {
	RecordSync(product, target, result string)
	RecordSyncDuration(product, target string, seconds float64)
	SetDrift(product, target string, drifted bool)
	RecordProxyProbe(product, target, result string)
}

// Auditor appends to the durable business record.
type Auditor interface {
	Record(ctx context.Context, row store.AuditRow) error
}

// StoreAuditor adapts the packages store, opening its own transaction.
//
// Its own transaction rather than joining a caller's, because a replication
// event has no other write to commit atomically with: the registry-side change
// already happened, and refusing to record it because a database write failed
// would lose the only record that it did.
type StoreAuditor struct{ packages *store.Packages }

// NewStoreAuditor builds one.
func NewStoreAuditor(p *store.Packages) *StoreAuditor { return &StoreAuditor{packages: p} }

// Record appends one event.
func (a *StoreAuditor) Record(ctx context.Context, row store.AuditRow) error {
	if a == nil || a.packages == nil {
		return nil
	}
	tx, err := a.packages.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := a.packages.InsertAudit(ctx, tx, row); err != nil {
		return err
	}
	return tx.Commit()
}

// audit records an event, swallowing failures with a log line.
//
// Swallowed because the alternative is worse: an apply that succeeded against
// Quay and then reported an error because the audit insert failed would leave
// the operator believing nothing happened, and re-running a destructive apply
// is the one thing this design works hardest to make deliberate.
func (s *Service) audit(ctx context.Context, row store.AuditRow) {
	if s.auditor == nil {
		return
	}
	if err := s.auditor.Record(ctx, row); err != nil {
		s.log.WarnContext(ctx, "could not record a replication audit event",
			"event", row.EventType, "product", row.ProductName, "error", err)
	}
}
