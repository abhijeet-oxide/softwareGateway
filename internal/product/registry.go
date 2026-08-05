package product

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Registry is the process-wide set of loaded products.
//
// Reads are lock-free-ish (RWMutex read path) and the write path swaps the
// whole map at once, so a reload is never observed half-applied: a caller
// either sees the previous configuration or the new one.
type Registry struct {
	mu       sync.RWMutex
	products map[string]*Product
	invalid  map[string]InvalidProduct
	// fileToName remembers which file provided which product on the last
	// successful load. It exists for one case: a document that fails to PARSE
	// yields no product name, so without this the product would be silently
	// dropped rather than retained. See Swap.
	fileToName map[string]string
	lastReload time.Time
	// generation increments on every successful swap, so callers can detect
	// that configuration changed without diffing it.
	generation uint64
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		products:   map[string]*Product{},
		invalid:    map[string]InvalidProduct{},
		fileToName: map[string]string{},
	}
}

// Swap replaces the product set with the result of a load.
//
// Fail-closed per product: an invalid document leaves the PREVIOUS valid
// version of that product in place rather than removing it. A bad edit
// degrades to "the change did not take effect", never to "the product
// silently stopped replicating". See docs/design/02 section 7.
func (r *Registry) Swap(res LoadResult) {
	r.mu.Lock()
	defer r.mu.Unlock()

	next := make(map[string]*Product, len(res.Valid))
	nextFiles := make(map[string]string, len(res.Valid))
	for _, p := range res.Valid {
		next[p.Metadata.Name] = p
		nextFiles[p.SourceFile] = p.Metadata.Name
	}

	invalid := make(map[string]InvalidProduct, len(res.Invalid))
	for _, bad := range res.Invalid {
		name := bad.Name
		if name == "" {
			// The document failed to PARSE, so it never yielded a name. Fall
			// back to which product this file provided last time: without
			// this, a stray syntax error would not merely fail to apply, it
			// would silently stop the product replicating — the opposite of
			// the fail-closed-per-product guarantee.
			name = r.fileToName[bad.File]
		}
		if name == "" {
			// Never successfully loaded from this file. Key by path so the
			// failure is still reported.
			invalid[bad.File] = bad
			continue
		}

		invalid[name] = bad
		if prev, ok := r.products[name]; ok {
			if _, replaced := next[name]; !replaced {
				next[name] = prev
				// Keep the mapping alive so repeated bad edits keep retaining.
				nextFiles[bad.File] = name
			}
		}
	}

	r.products = next
	r.fileToName = nextFiles
	r.invalid = invalid
	r.lastReload = time.Now()
	r.generation++
}

// Get returns a product by name.
func (r *Registry) Get(name string) (*Product, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.products[name]
	return p, ok
}

// List returns every loaded product, ordered by name for stable API output.
func (r *Registry) List() []*Product {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]*Product, 0, len(r.products))
	for _, p := range r.products {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out
}

// Count returns the number of loaded products.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.products)
}

// Invalid returns the documents that failed to load, ordered by key.
func (r *Registry) Invalid() []InvalidProduct {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]InvalidProduct, 0, len(r.invalid))
	for _, bad := range r.invalid {
		out = append(out, bad)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out
}

// Generation returns the swap counter.
func (r *Registry) Generation() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.generation
}

// LastReload returns the time of the last swap.
func (r *Registry) LastReload() time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lastReload
}

// Repository resolves "product/repositoryName" for either role.
func (r *Registry) Repository(productName, repoName string) (RepositoryRef, error) {
	p, ok := r.Get(productName)
	if !ok {
		return RepositoryRef{}, fmt.Errorf("product %q is not configured", productName)
	}
	for _, ref := range p.Repositories() {
		if ref.Name == repoName {
			return ref, nil
		}
	}
	return RepositoryRef{}, fmt.Errorf("product %q has no repository %q", productName, repoName)
}
