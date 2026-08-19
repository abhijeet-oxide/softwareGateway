package compare

import (
	"context"
	"sort"
	"sync"

	"github.com/abhijeet-oxide/softwareGateway/internal/oci"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
)

// Looking inside the layers of a component that changed.
//
// # Why a layer digest is not an answer
//
// "Two layers changed" is true and useless. A release that edited one line of
// one configuration file and a release that rewrote everything produce the same
// sentence, because a layer is an archive and its digest changes when anything
// inside it does.
//
// So for a component that differs, the layers are OPENED and the comparison is
// done over the FILES. `oci.ReadFiles` does the reading, and it reads the format
// the OCI specification names — a layer is a tar changeset, and the media types
// say so — rather than anything a vendor invented. Nothing in this file knows
// whose bundle it is looking at.
//
// # Why every layer is read, not only the ones whose digests differ
//
// Because layers stack, and a file can move between them without changing.
// Reading only the layers that differ would report a repacked-but-identical file
// as added on one side and missing on the other, which is a false finding of
// exactly the kind this command exists to avoid producing.
//
// Reading them all costs one fetch per DISTINCT digest across both sides — a
// layer both sides share is fetched once and cancels out — and the budget is
// what stops that being expensive.
//
// # The budget is the whole cost control
//
// A comparison must not download a two-gigabyte image layer to answer a question
// about a four-kilobyte configuration bundle. What would have been a vendor
// list — "these artifact types are worth opening" — is a byte budget instead,
// which needs no plugin and degrades correctly for a vendor nobody has written
// one for. Past it, a layer is one opaque unit and the row says so.

// budget hands out permission to download, once.
//
// Shared across the whole comparison rather than per component, because the
// thing being protected is the operator's link and not any one row.
type budget struct {
	mu        sync.Mutex
	remaining int64
}

// take reserves n bytes, reporting whether there was room.
func (b *budget) take(n int64) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > b.remaining {
		return false
	}
	b.remaining -= n
	return true
}

// fileView is one side's files, by path.
type fileView struct {
	byPath map[string]string
	// truncated records that a layer was left unopened, so the caller can say
	// the list is incomplete rather than implying it is the whole answer.
	truncated bool
}

// inspectFiles fills in the file-level difference for every changed row.
//
// Only for rows that already disagree: a component whose digest matches on both
// sides is byte-identical, and opening it could not produce a finding.
// # It runs with NO budget too
//
// The budget controls whether layer archives are DOWNLOADED. It does not
// control whether the files can be named: a layer that carries
// `org.opencontainers.image.title` names the file inside it, in the manifest we
// have already read, for nothing. That is every layer of a vendor's generic
// artifact — the configuration bundles, the release notes, the scripts — and
// naming them is most of what a reader wants from this column.
//
// This used to return immediately on a zero budget, so a comparison run without
// the expensive option reported no file-level information at all and the column
// read empty on every row. The expensive part is opening an archive; saying
// which named files a component carries is free.
func inspectFiles(
	ctx context.Context, clientA, clientB ClientFactory, rows []Row,
	fileBudget int64, concurrency int,
) {
	if concurrency <= 0 {
		concurrency = 4
	}
	if fileBudget < 0 {
		fileBudget = 0
	}

	remaining := &budget{remaining: fileBudget}
	// One reader per side, and a cache keyed by digest so a layer both sides
	// share is fetched once however many components reference it.
	readerA := newLayerReader(clientA, remaining)
	readerB := newLayerReader(clientB, remaining)

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i := range rows {
		row := &rows[i]
		if row.Verdict != VerdictChanged || row.A == nil || row.B == nil {
			continue
		}
		if !layersDiffer(row.A.Layers, row.B.Layers) {
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			viewA := readerA.view(ctx, row.A)
			viewB := readerB.view(ctx, row.B)
			row.FilesAdded, row.FilesRemoved, row.FilesChanged = diffViews(viewA, viewB)
			row.FilesTruncated = viewA.truncated || viewB.truncated
		}()
	}
	wg.Wait()
}

// layersDiffer reports whether the two sides' layer sets are not identical.
func layersDiffer(a, b []Layer) bool {
	if len(a) != len(b) {
		return true
	}
	inA := make(map[string]bool, len(a))
	for _, l := range a {
		inA[l.Digest] = true
	}
	for _, l := range b {
		if !inA[l.Digest] {
			return true
		}
	}
	return false
}

// layerReader turns one side's layers into files, caching by digest.
type layerReader struct {
	client ClientFactory
	budget *budget

	mu    sync.Mutex
	cache map[string]cachedLayer
}

type cachedLayer struct {
	files []oci.File
	// opened is false when the layer was not an archive, was past the budget,
	// or could not be read — all of which mean "treat it as one unit".
	opened bool
}

func newLayerReader(client ClientFactory, b *budget) *layerReader {
	return &layerReader{client: client, budget: b, cache: map[string]cachedLayer{}}
}

// view builds one component's file map.
//
// Layers are applied IN ORDER, so a later layer replacing a path wins — which is
// how a layered filesystem works and therefore the only reading that matches
// what a consumer would actually get.
func (r *layerReader) view(ctx context.Context, item *Item) fileView {
	out := fileView{byPath: map[string]string{}}

	for _, layer := range item.Layers {
		cached := r.read(ctx, item.Repository, layer)
		if !cached.opened {
			// A layer nobody opened contributes a file only if it NAMES one.
			//
			// `org.opencontainers.image.title` is the vendor saying "this
			// layer is this file" — an ORAS-style single-file layer, which is
			// what every configuration bundle and release note in a vendor
			// artifact is. That name is in the manifest we have already read,
			// so it costs nothing and it is exactly right.
			//
			// A layer with no title is an image layer holding an unknown
			// number of paths. Entering it as one row called `layer sha256:…`
			// would restate the digest difference in the column that exists to
			// say more than the digest, so it enters nothing and the view says
			// it is incomplete.
			if layer.Title == "" {
				out.truncated = true
				continue
			}
			out.byPath[layer.Title] = layer.Digest
			continue
		}
		for _, f := range cached.files {
			if f.Whiteout {
				delete(out.byPath, f.Path)
				continue
			}
			out.byPath[f.Path] = f.Digest
		}
	}
	return out
}

// read fetches and parses one layer, once.
func (r *layerReader) read(ctx context.Context, repository string, layer Layer) cachedLayer {
	r.mu.Lock()
	if cached, ok := r.cache[layer.Digest]; ok {
		r.mu.Unlock()
		return cached
	}
	r.mu.Unlock()

	result := cachedLayer{}
	if r.budget.take(layer.Size) {
		result = r.fetch(ctx, repository, layer)
	}

	r.mu.Lock()
	r.cache[layer.Digest] = result
	r.mu.Unlock()
	return result
}

func (r *layerReader) fetch(
	ctx context.Context, repository string, layer Layer,
) cachedLayer {
	repo, err := r.client(repository)
	if err != nil {
		return cachedLayer{}
	}

	body, err := repo.FetchBlob(ctx, registry.Digest(layer.Digest))
	if err != nil {
		return cachedLayer{}
	}
	defer func() { _ = body.Close() }()

	// Bounded by the layer's DECLARED size, which the budget has already
	// approved. A registry serving more bytes than its own descriptor claims is
	// lying about content-addressed data, and reading past the claim would be
	// trusting it.
	files, opened, err := oci.ReadFiles(body, layer.Size)
	if err != nil || !opened {
		// A blob that will not parse is one opaque unit, which is what the
		// caller assumed before asking. Failing the comparison over it would
		// trade a complete answer for none.
		return cachedLayer{}
	}
	return cachedLayer{files: files, opened: true}
}

// opaqueName is what to call a layer that was not opened.
//
// Its title where the vendor set one — which for an ORAS-style single-file
// layer IS the file's name, so the row reads correctly without anything being
// opened at all — and the short digest otherwise.
func opaqueName(layer Layer) string {
	if layer.Title != "" {
		return layer.Title
	}
	return "layer " + short(layer.Digest)
}

// diffViews states what changed between two file maps.
//
// Three lists rather than two, because "changed" is not "added and removed".
// Reporting an edited configuration file as both would double every finding and
// make a one-line change read like a rewrite.
func diffViews(a, b fileView) (added, removed, changed []string) {
	for path, digest := range b.byPath {
		other, inA := a.byPath[path]
		switch {
		case !inA:
			added = append(added, path)
		case other != digest:
			changed = append(changed, path)
		}
	}
	for path := range a.byPath {
		if _, inB := b.byPath[path]; !inB {
			removed = append(removed, path)
		}
	}

	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changed)
	return added, removed, changed
}
