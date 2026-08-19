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
// # Which layers are read
//
// The ones that can still say something. A layer carrying
// `org.opencontainers.image.title` has already told us its path and its content
// digest in the manifest, so downloading it adds nothing unless the SAME path
// carries different content on the other side — where opening both archives is
// the only way to say which paths inside them changed. An untitled layer names
// nothing at all and is always opened. See worthOpening.
//
// What is NOT done is skipping a layer because its digest differs from its
// opposite number, one position along. Layers stack, and a file can move
// between them without changing; deciding by position would report a
// repacked-but-identical file as added on one side and missing on the other.
// The decision is per NAME, across the whole component, and it is symmetric.
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
	fileBudget int64, concurrency int, reportA, reportB *sideReporter,
) {
	if concurrency <= 0 {
		concurrency = 4
	}
	if fileBudget < 0 {
		fileBudget = 0
	}

	remaining := &budget{remaining: fileBudget}
	// One reader per side, and a cache keyed by digest so a layer referenced by
	// several components is fetched once.
	readerA := newLayerReader(clientA, remaining, reportA)
	readerB := newLayerReader(clientB, remaining, reportB)

	// THE PLAN FIRST, then the work.
	//
	// Which layers are worth opening is decided from the two manifests, which
	// are already in hand — so the whole of this phase can be counted before
	// any of it starts. It used to announce each layer as it reached it, which
	// is why the bar read "379 of 383+": a denominator discovered one unit at a
	// time is never more than one unit ahead of the position.
	work := make([]fileWork, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		if row.Verdict != VerdictChanged || row.A == nil || row.B == nil {
			continue
		}
		if !layersDiffer(row.A.Layers, row.B.Layers) {
			continue
		}
		work = append(work, fileWork{row: row, open: worthOpening(row.A.Layers, row.B.Layers)})
	}

	reportA.expect(PhaseFiles, countOpenable(work, func(w fileWork) *Item { return w.row.A }))
	reportA.certain()
	reportB.expect(PhaseFiles, countOpenable(work, func(w fileWork) *Item { return w.row.B }))
	reportB.certain()

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, w := range work {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			viewA := readerA.view(ctx, w.row.A, w.open)
			viewB := readerB.view(ctx, w.row.B, w.open)
			w.row.FilesAdded, w.row.FilesRemoved, w.row.FilesChanged = diffViews(viewA, viewB)
			w.row.FilesTruncated = viewA.truncated || viewB.truncated
		}()
	}
	wg.Wait()
}

// fileWork is one component to look inside, and which of its layers to open.
type fileWork struct {
	row  *Row
	open func(Layer) bool
}

// countOpenable is how many DISTINCT layers one side will fetch.
//
// Distinct, because the reader caches by digest: a layer two components share
// is fetched once, and counting it twice would leave the bar short of its own
// total at the end.
func countOpenable(work []fileWork, side func(fileWork) *Item) int {
	seen := make(map[string]bool)
	for _, w := range work {
		item := side(w)
		if item == nil {
			continue
		}
		for _, layer := range item.Layers {
			if w.open(layer) {
				seen[layer.Digest] = true
			}
		}
	}
	return len(seen)
}

// worthOpening decides which of a component's layers must be downloaded.
//
// # Most of them cannot tell us anything we do not already know
//
// A layer that carries `org.opencontainers.image.title` names the file inside
// it, in the manifest we have already read. So for a titled layer we know its
// path and its content digest for free, and downloading it can only add
// something in ONE case: the same path exists on both sides with different
// content, and opening the two archives would say which paths INSIDE them
// changed.
//
// Everything else is already answered. A title on one side only is a file
// added or removed, and the archive's contents would not change that. A title
// on both sides with the same digest is byte-identical. Reading either was
// spending a download to reprint the manifest.
//
// This is what "I analysed the release and the comparison is no faster" was
// about. Analysis caches manifests, and manifests were never the expensive
// part once file comparison was switched on: a release of two hundred
// components was downloading every layer of every changed one — hundreds of
// megabytes — to learn what its own manifest had already said.
//
// # Untitled layers are always opened
//
// An image layer names nothing. Opening it is the only way to learn a single
// path inside it, so there is nothing to weigh: without the download the row
// can say no more than the digest already did.
//
// # The set is SHARED by both sides, and must be
//
// An opened archive contributes its inner paths and an unopened one
// contributes its title. Opening a layer on one side and not the other would
// compare inner paths against an archive name and report every file in it as
// both added and removed.
func worthOpening(a, b []Layer) func(Layer) bool {
	digestOf := func(layers []Layer) map[string]string {
		out := make(map[string]string, len(layers))
		for _, l := range layers {
			if l.Title != "" {
				out[l.Title] = l.Digest
			}
		}
		return out
	}
	inA, inB := digestOf(a), digestOf(b)

	changed := make(map[string]bool)
	for title, digest := range inA {
		if other, both := inB[title]; both && other != digest {
			changed[title] = true
		}
	}

	return func(l Layer) bool {
		return l.Title == "" || changed[l.Title]
	}
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
	report *sideReporter

	mu    sync.Mutex
	cache map[string]cachedLayer
}

type cachedLayer struct {
	files []oci.File
	// opened is false when the layer was not an archive, was past the budget,
	// or could not be read — all of which mean "treat it as one unit".
	opened bool
}

func newLayerReader(client ClientFactory, b *budget, report *sideReporter) *layerReader {
	return &layerReader{
		client: client, budget: b, report: report, cache: map[string]cachedLayer{},
	}
}

// view builds one component's file map.
//
// Layers are applied IN ORDER, so a later layer replacing a path wins — which is
// how a layered filesystem works and therefore the only reading that matches
// what a consumer would actually get.
func (r *layerReader) view(ctx context.Context, item *Item, open func(Layer) bool) fileView {
	out := fileView{byPath: map[string]string{}}

	for _, layer := range item.Layers {
		var cached cachedLayer
		if open == nil || open(layer) {
			cached = r.read(ctx, item.Repository, layer)
		}
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
	// Counted whether or not the budget allowed the download: the caller
	// planned this layer and is waiting for the answer about it, and a layer
	// declined for cost is answered — as one opaque unit — just as surely as
	// one that was read.
	r.report.did(PhaseFiles, 1)

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
