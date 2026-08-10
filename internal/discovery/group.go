package discovery

import (
	"context"
	"database/sql"
	"errors"
	"sync"

	"github.com/abhijeet-oxide/softwareGateway/internal/oci"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/vendors"
)

// This file holds the part of a scan between "which tags are new" and "record
// them": fetching each new tag's manifest, then handing the whole set to the
// source's Layout so a vendor's several tags become the one release they
// actually represent.
//
// Why it is a separate phase rather than per-tag work: a vendor's relationships
// are BETWEEN tags, and discovery has no ordering guarantee. Seeing `orb_X`
// alone tells you nothing about whether a `signed_orb_X` exists, so grouping
// cannot be decided until the repository's new tags are all in hand.

// resolvedTag is one tag after the HEAD, before anything expensive.
type resolvedTag struct {
	work tagWork
	desc registry.Descriptor
	// known means this exact (repository, tag, digest) is already recorded, so
	// there is nothing to fetch and nothing to group.
	known bool
	// gone means the tag disappeared between the listing and the resolve. Not
	// an error: the next scan will not list it.
	gone bool
	err  error
}

// headPhase resolves every tag to a digest and asks whether we already have it.
//
// One HEAD per tag and nothing else. This is what keeps the steady state cheap:
// a re-scan of a repository where nothing changed transfers no manifest bodies
// at all, and that property survives grouping precisely because grouping runs
// only over tags this phase reports as new.
func (s *Scanner) headPhase(ctx context.Context, work []tagWork, limit int) []resolvedTag {
	out := make([]resolvedTag, len(work))
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup

	for i, w := range work {
		if ctx.Err() != nil {
			break
		}

		wg.Add(1)
		go func(i int, w tagWork) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			r := resolvedTag{work: w}
			defer func() { out[i] = r }()

			desc, err := w.client.ResolveTag(ctx, w.tag)
			if err != nil {
				if errors.Is(err, registry.ErrNotFound) {
					r.gone = true
					return
				}
				r.err = err
				return
			}
			if err := desc.Digest.Validate(); err != nil {
				r.err = err
				return
			}
			r.desc = desc

			// An optimisation, not the correctness mechanism: the unique
			// constraint inside recordPackage is what actually prevents a
			// duplicate, so a scan racing us between here and that insert is
			// harmless.
			r.known, r.err = s.packages.PackageExists(ctx, w.repoID, w.tag, desc.Digest.String())
		}(i, w)
	}
	wg.Wait()
	return out
}

// fetched is a tag whose manifest we now hold.
type fetched struct {
	work tagWork
	tree oci.Tree
	err  error
}

// fetchPhase fetches the manifest of every tag the head phase found new.
//
// Exactly one request per new tag, whatever the package contains — the oci.Tree
// walk stays deferred (see fetchPackage). Together with the HEAD, a newly
// discovered tag costs two round trips.
func (s *Scanner) fetchPhase(ctx context.Context, resolved []resolvedTag, limit int) []fetched {
	var pending []resolvedTag
	for _, r := range resolved {
		if r.err == nil && !r.gone && !r.known {
			pending = append(pending, r)
		}
	}
	if len(pending) == 0 {
		return nil
	}

	out := make([]fetched, len(pending))
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup

	for i, r := range pending {
		if ctx.Err() != nil {
			break
		}

		wg.Add(1)
		go func(i int, r resolvedTag) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			t, err := oci.FetchRoot(ctx, r.work.client, r.desc)
			out[i] = fetched{work: r.work, tree: t, err: err}
			if err == nil {
				s.progress.update(func(p *ScanProgress) { p.Artifacts += len(t.Artifacts) })
			}
		}(i, r)
	}
	wg.Wait()
	return out
}

// scannedTagsFor converts fetched manifests into the shape a Layout consumes.
//
// The Layout is given descriptors and annotations, never the Scanner — it
// classifies and relates, and cannot fetch, push or record. A broken Layout can
// mislabel a tag; it cannot corrupt a transfer.
func scannedTagsFor(items []fetched) []vendors.ScannedTag {
	out := make([]vendors.ScannedTag, 0, len(items))
	for _, f := range items {
		if f.err != nil || len(f.tree.Artifacts) == 0 {
			continue
		}
		root := f.tree.Artifacts[0]

		// Children are the artifacts the root lists — depth 1 — which for an
		// index is exactly its `manifests` array, annotations included. That is
		// all a wrapper-index layout needs, and it costs no extra request
		// because the index already stated it.
		var children []registry.Descriptor
		for _, a := range f.tree.Artifacts[1:] {
			if a.Depth == 1 {
				children = append(children, a.Descriptor)
			}
		}

		out = append(out, vendors.ScannedTag{
			Tag:         f.work.tag,
			Descriptor:  root.Descriptor,
			Raw:         root.Raw,
			Annotations: root.Descriptor.Annotations,
			Children:    children,
		})
	}
	return out
}

// groupPhase turns one repository's new tags into the packages they represent.
//
// A Layout failure is NOT fatal to the scan. Falling back to one-package-per-tag
// means a vendor changing its convention degrades to noisier output rather than
// to discovering nothing — and the log says which happened.
func (s *Scanner) groupPhase(
	ctx context.Context, src registry.Source, scanned []vendors.ScannedTag,
	accessory map[string]bool,
) []vendors.Package {
	if len(scanned) == 0 {
		return nil
	}

	pkgs, err := s.layout.Group(ctx, src, scanned)
	if err != nil {
		s.log.WarnContext(ctx, "layout could not group tags; recording them individually",
			"layout", s.layout.Name(), "tags", len(scanned), "error", err)

		// The fallback records one package per tag — but NOT for tags the
		// filters never admitted. Those are here only because the Layout asked
		// for them, and turning a failed grouping into three packages per
		// release would be a worse outcome than the noisier one this fallback
		// exists to produce.
		var admitted []vendors.ScannedTag
		for _, t := range scanned {
			if !accessory[t.Tag] {
				admitted = append(admitted, t)
			}
		}
		return vendors.Standard{}.Fallback(admitted)
	}
	return pkgs
}

// rootDigestOf returns the transfer root when it differs from the package's own
// manifest, and empty otherwise.
//
// Empty rather than a copy of the manifest digest: the column means "plan from
// something else", and duplicating the value there would make every package
// look like a special case.
func rootDigestOf(pkg vendors.Package) string {
	if pkg.Root.Digest == "" || pkg.Root.Digest == pkg.Descriptor.Digest {
		return ""
	}
	return pkg.Root.Digest.String()
}

// relationRows converts a Layout's related artifacts into storage rows.
func relationRows(pkg vendors.Package) []store.RelationRow {
	if len(pkg.Related) == 0 {
		return nil
	}
	out := make([]store.RelationRow, 0, len(pkg.Related))
	for _, r := range pkg.Related {
		out = append(out, store.RelationRow{
			Role:      string(r.Role),
			Digest:    r.Descriptor.Digest.String(),
			Tag:       r.Tag,
			MediaType: r.Descriptor.MediaType,
			SizeBytes: r.Descriptor.Size,
		})
	}
	return out
}

// attachRelations records what a scan learned about a package discovered
// EARLIER.
//
// The case: a vendor publishes a release, then signs it on a later day. The
// payload was recorded by an earlier scan, so the scan that finds the signature
// has no new package to attach it to — only an existing one to correct. Without
// this the package would read `unsigned` forever, which is worse than `unknown`
// because it looks like an answer somebody checked.
func (s *Scanner) attachRelations(ctx context.Context, repoID int64, pkg vendors.Package, regroup bool) error {
	packageID, err := s.packages.FindPackageByTag(ctx, repoID, pkg.Tag)
	if err != nil {
		// Not found is not an error here: the Layout may name a payload tag
		// that discovery has never admitted — excluded by a tag filter, say.
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.packages.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if regroup {
		// Re-derived under a DIFFERENT convention, so the old relations are not
		// a subset of the new ones — they may be wrong rather than merely
		// incomplete. Deleted and rewritten inside one transaction, so no reader
		// ever sees the package between the two and briefly believes it unsigned.
		//
		// Never done on the discovery path, where insert-or-ignore is what keeps
		// re-deriving the same relationship a no-op.
		if err := s.packages.DeleteRelations(ctx, tx, packageID); err != nil {
			return err
		}
	}

	if err := s.packages.ReplaceRelations(ctx, tx, packageID, relationRows(pkg)); err != nil {
		return err
	}
	if err := s.packages.UpdateSignatureState(ctx, tx, packageID,
		string(pkg.Status(s.layout.LooksForSignatures())),
		rootDigestOf(pkg), pkg.RootTag); err != nil {
		return err
	}

	if regroup {
		// The tags this package absorbed exist as packages of their own, because
		// they were discovered before the vendor was declared and nothing was
		// grouping then. They stop being listed as releases; they are not
		// deleted, because a transfer may reference them and what was shipped
		// has to stay answerable.
		if err := s.absorbAccessories(ctx, tx, repoID, packageID, pkg); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// absorbAccessories marks the package rows of a release's accessory tags.
//
// The Layout does not name accessories directly — it simply does not return
// them as packages — but every one it absorbed is reachable as a related
// artifact with a tag, which is the same set.
func (s *Scanner) absorbAccessories(
	ctx context.Context, tx *sql.Tx, repoID, packageID int64, pkg vendors.Package,
) error {
	for _, rel := range pkg.Related {
		if rel.Tag == "" || rel.Tag == pkg.Tag {
			continue
		}
		// Inside the caller's transaction, not on a pooled connection: on SQLite
		// the open write transaction holds the database lock, so the same lookup
		// on another connection waits for a transaction that is waiting for it.
		accessoryID, err := s.packages.FindPackageByTagTx(ctx, tx, repoID, rel.Tag)
		if errors.Is(err, store.ErrNotFound) {
			// The usual case, and not a problem: this tag never became a package
			// because grouping absorbed it when it was discovered.
			continue
		}
		if err != nil {
			return err
		}
		if accessoryID == packageID {
			continue
		}
		if err := s.packages.MarkAccessory(ctx, tx, accessoryID, packageID); err != nil {
			return err
		}
	}
	return nil
}
