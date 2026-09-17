package replication

import (
	"context"

	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// Snapshot is every product id and applied record a listing will need, read up
// front in two queries.
//
// # The N+1 this exists to remove
//
// Status is written for ONE target, and the per-target reads inside it are
// right at that size: resolve the product's row id, then read what we last
// applied to this target. The fleet listing calls it once per target of every
// product a person can see, so those two reads became two per target - plus
// two more inside recordObservation, which resolved the very same product id
// again to write the observation down.
//
// Measured on a running deployment, /api/v1/replication was making twenty
// database round trips per request against one for the transfers listing. It
// never showed up as slow, because the wall time of that endpoint is dominated
// by talking to the registries; it showed up as a flat line at 20.6 on the
// api_request_queries histogram, which is exactly what that metric exists for.
//
// # Why a value rather than a cache
//
// Because a cache needs an invalidation story and this needs none. It is built
// per request, used for that request's reads, and discarded - so it cannot
// serve a stale product id after a reconcile, and two requests cannot see each
// other's view. The alternative, a map on the Service keyed by product name,
// would be correct right up to the first product that was deleted and
// recreated with the same name.
type Snapshot struct {
	productIDs map[string]int64
	applied    map[recordKey]store.TargetReplication
}

type recordKey struct{ product, target string }

// Snapshot reads the applied state of every named product's targets.
//
// Two queries regardless of how many products or targets are asked about. A
// nil Service store, or an error from either read, yields a snapshot that
// simply knows nothing - callers fall back to the per-target path, which is
// the behaviour they had before this existed.
func (s *Service) Snapshot(ctx context.Context, productNames []string) *Snapshot {
	snap := &Snapshot{
		productIDs: map[string]int64{},
		applied:    map[recordKey]store.TargetReplication{},
	}
	if s.store == nil || len(productNames) == 0 {
		return snap
	}

	ids, err := s.store.ProductIDs(ctx, productNames)
	if err != nil {
		s.log.Warn("prefetch product ids for a replication listing", "error", err)
		return snap
	}
	snap.productIDs = ids

	// Every product at once. The listing is a fleet view by definition, and a
	// query per product would be the same N+1 one level up.
	records, err := s.store.List(ctx, "")
	if err != nil {
		s.log.Warn("prefetch replication records", "error", err)
		return snap
	}
	want := make(map[string]bool, len(productNames))
	for _, n := range productNames {
		want[n] = true
	}
	for _, rec := range records {
		if want[rec.Product] {
			snap.applied[recordKey{rec.Product, rec.TargetName}] = rec
		}
	}
	return snap
}

// productID returns a prefetched id. The second value is false when this
// snapshot has nothing to say, which is not the same as the product having no
// row - the caller falls back to reading it.
func (s *Snapshot) productID(product string) (int64, bool) {
	if s == nil {
		return 0, false
	}
	id, ok := s.productIDs[product]
	return id, ok
}

// record returns a prefetched applied record.
//
// `known` reports whether this snapshot covers the product at all. It has to
// be separate from finding the record, because "this product has no row for
// this target" is a real answer - it means nobody has applied it yet - and
// returning it as "ask the database" would put the N+1 straight back.
func (s *Snapshot) record(product, target string) (rec store.TargetReplication, found, known bool) {
	if s == nil {
		return rec, false, false
	}
	if _, ok := s.productIDs[product]; !ok {
		return rec, false, false
	}
	rec, found = s.applied[recordKey{product, target}]
	return rec, found, true
}
