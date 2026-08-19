// Package compare answers one question about two places: what is different?
//
// # One mechanism, five questions
//
// The questions operators actually ask look like five different tools:
//
//	did the transfer land?          source  vs  target,  same version
//	did the promotion land?         lab     vs  prod,    same version
//	what changed in this release?   source  vs  source,  two versions
//	was anything mutated?           either  vs  either,  same version
//	is there anything extra there?  either  vs  either
//
// They are one tool, because they are all "walk two bundles and align their
// components". Nothing here knows which of the five it is being used for, and
// that is what keeps it from growing five code paths that disagree.
//
// # Why both sides are WALKED rather than one being read from a record
//
// Nothing in this package trusts what we recorded. Every fact about both sides
// comes from a registry in this call — which is the whole point of an integrity
// check, and is also what lets a side be a target we have never planned
// against, or a version somebody published by hand.
//
// It works uniformly because a transfer copies manifests VERBATIM. The index at
// the destination carries the same `org.opencontainers.image.ref.name`
// annotations the vendor wrote, so a component identifies itself the same way
// wherever it is, and two sides can be aligned without either of them being
// "the original".
//
// # What identity means here
//
// Components are aligned by the repository half of their `ref.name` — the
// vendor's name for the component, which survives copying and survives a new
// release. The TAG is compared rather than matched on, because in a
// version-to-version comparison the tag is precisely the thing that changed.
// An artifact the vendor named nothing is aligned by digest, which can only
// ever match itself; that is the honest answer for something with no identity
// beyond its bytes.
package compare

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/abhijeet-oxide/softwareGateway/internal/oci"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/transfer"
	"github.com/abhijeet-oxide/softwareGateway/internal/vendors"
)

// ClientFactory builds a repository handle for one path on one side's registry.
type ClientFactory func(repository string) (registry.Repository, error)

// SideSpec is one end of a comparison.
type SideSpec struct {
	// Label is what to call this side in output: a configured endpoint name,
	// and the version where the two sides differ in version.
	Label string
	// Repository is where the bundle's root lives on this side.
	Repository string
	// References are candidate roots, most complete first. The most complete
	// one BOTH SIDES hold is what gets walked — see chooseRoots.
	//
	// Plural because a release may be addressable several ways: a pre-1.1
	// vendor bundling a payload with its detached signature publishes a wrapper
	// index over both, and walking the wrapper reaches the whole release rather
	// than the part a consumer happens to pull. A place holding only the
	// payload is a real and ordinary state, and comparing that beats reporting
	// the whole side missing.
	//
	// Each root is best given in both spellings it has, tag and digest: a
	// missing TAG and a missing MANIFEST are different failures with different
	// fixes, and a list carrying only the tag cannot tell them apart.
	References []string
	// BasePath is the prefix beneath which this side reproduces the vendor's
	// structure — a target's configured `repository`. Empty for a source,
	// which holds the vendor's paths unprefixed.
	BasePath string
	// PublishesComponentsByName says this side is expected to serve each
	// component from the repository its `ref.name` names, as well as from
	// inside the bundle.
	//
	// TRUE ONLY FOR A SIDE THIS SYSTEM WROTE. That second publication is
	// something a TRANSFER creates — see internal/transfer/layout.go, "Why a
	// component lands in TWO places". It is not required by OCI, and it is not
	// something a vendor has agreed to do.
	//
	// NEAR does not do it. Its components live inside the orb repository,
	// tagged `docker_<digest>` and `helmoci_<digest>`; the
	// `cfx-5000-product/admin` in a NEAR component's `ref.name` is the
	// ORIGINAL UPSTREAM COORDINATE the vendor built from, not a path NEAR
	// serves. Asking a NEAR registry for it therefore returns 404 for every
	// component of every orb.
	//
	// Which is what this flag exists to stop. Probing both sides
	// unconditionally reported a vendor's own layout as a defect, once per
	// component: 253 findings, each reading `not published as
	// cfx-5000-product/… on the first side`, on a transfer that had copied
	// every byte correctly.
	PublishesComponentsByName bool
}

// String renders a side as a reference somebody could paste into a pull.
func (s SideSpec) String() string {
	ref := ""
	if len(s.References) > 0 {
		ref = s.References[0]
	}
	if s.Repository == "" {
		return ref
	}
	return s.Repository + ":" + ref
}

// Options is what to compare.
type Options struct {
	A, B SideSpec
	// Concurrency bounds the registry calls made against each side.
	Concurrency int
	// Progress is called as the comparison proceeds. Nil reports nothing.
	//
	// A comparison of a real release is minutes of reading two registries, and
	// a caller with no way to say what is happening can only offer an animation
	// — which is the same thing it would offer for a comparison that had
	// silently stopped.
	Progress ProgressFunc

	// Classify names what a component IS. Nil means the OCI rules alone.
	//
	// Injected rather than resolved here because naming a vendor is the one
	// thing this package may not do, and for a vendor whose charts are plain
	// image manifests the OCI rules cannot answer: only the annotation the
	// vendor wrote says which is which. The composition root builds it from
	// the product's configured layouts, and the release page and the transfer
	// breakdown are built from the same thing — a comparison that named
	// components differently from the pages either side of it would be read as
	// a difference in the content.
	Classify vendors.Classifier
}

// Progress is one side's position in the comparison.
//
// Per SIDE, because the two are walked concurrently against different
// registries and one of them is usually the slow one — a single merged number
// would hide which.
//
// # The unit is REQUESTS, and that is what makes it monotonic
//
// A comparison is four kinds of work over four different populations: manifests
// walked, component names probed, repository tags resolved, layer archives
// read. Reporting each phase's own count meant the number RESET to zero at
// every phase boundary — a bar that filled, emptied and filled again, which
// reads as the thing having restarted.
//
// So Done counts round trips completed on this side and never decreases, and
// Total counts round trips KNOWN so far, which grows as each phase discovers
// its population — exactly as the manifest walk's own denominator does. Phase
// is a label saying what those requests currently are.
type Progress struct {
	// Key is which END this is — "a" or "b" — and it is what a consumer should
	// index by.
	//
	// The label is not an identity: two sides of a version comparison are the
	// same place, so they carry the same label unless something upstream has
	// disambiguated them, and a tracker keyed by label would have the second
	// side overwriting the first's position.
	Key string
	// Side is the end's label, as it appears in the report.
	Side string
	// Phase is what is being done, in the words the report uses.
	Phase string
	// Done is round trips completed on this side. Monotonic.
	Done int
	// Total is round trips known so far. It grows; it never shrinks.
	Total int
	// Estimated says the total may still grow, so a caller can render the
	// percentage as an estimate rather than as a position.
	Estimated bool
	// Concurrency is how many requests this side may have in flight at once.
	//
	// Reported because "is it going as fast as it can" is the second question
	// anybody watching a four-minute bar asks, and a comparison running one
	// request at a time looks identical to one running thirty-two — which is
	// exactly the bug that made comparisons slow before anyone noticed.
	Concurrency int
}

// The phases a comparison passes through, named as the reader would name them.
const (
	PhaseManifests = "reading manifests"
	PhaseNames     = "checking component names"
	PhaseTags      = "checking for unaccounted tags"
	PhaseDone      = "finished"
)

// ProgressFunc receives progress. It is called from several goroutines and
// must be safe for concurrent use.
type ProgressFunc func(Progress)

// sideReporter accumulates one side's work into a single monotonic count.
//
// One per side, shared by every phase that side passes through, so the number a
// caller sees only ever goes up — see Progress for why that matters.
type sideReporter struct {
	key         string
	side        string
	concurrency int
	report      ProgressFunc

	mu        sync.Mutex
	phase     string
	done      int
	known     int
	estimated bool
}

func newSideReporter(key, side string, concurrency int, report ProgressFunc) *sideReporter {
	return &sideReporter{
		key: key, side: side, concurrency: concurrency, report: report, estimated: true,
	}
}

// walked records the manifest walk's absolute position.
//
// Absolute rather than incremental because the walk reports totals, and it is
// the first phase — so its numbers ARE the side's numbers until another phase
// adds to them.
func (r *sideReporter) walked(fetched, known int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	r.phase = PhaseManifests
	// The HIGHEST reported, not the latest. The walk counts with an atomic
	// across its fetching goroutines, so callbacks arrive out of order — the
	// goroutine that computed 3 can emit after the one that computed 2 — and
	// taking the latest made the count flicker downwards.
	if fetched > r.done {
		r.done = fetched
	}
	if known > r.known {
		r.known = known
	}
	r.emitLocked()
}

// expect adds a phase's population to what is known, and names the phase.
//
// Called once a phase can count its own work: how many components have names to
// probe, how many tags a repository listed. A phase with NOTHING to do adds
// nothing and does not become the label — which is why a comparison against a
// source, where no component is expected to answer to its own name, no longer
// sits on "checking component names 0 of 259" for the rest of its life.
func (r *sideReporter) expect(phase string, units int) {
	if r == nil || units <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	r.phase = phase
	r.known += units
	r.emitLocked()
}

// did records completed work in a phase.
func (r *sideReporter) did(phase string, n int) {
	if r == nil || n <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	r.phase = phase
	r.done += n
	if r.done > r.known {
		// A phase that turned out to be larger than it announced. The count is
		// still true; the denominator was the estimate.
		r.known = r.done
	}
	r.emitLocked()
}

// certain marks the denominator FINAL — no longer an estimate.
//
// The manifest walk discovers its own size as it goes, so its total is a
// running maximum and the caller says so. A later phase can know its total
// before it starts, and a reader deserves to be told the difference between
// "379 of 383 so far" and "379 of 383".
func (r *sideReporter) certain() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	r.estimated = false
	r.emitLocked()
}

// settled marks this side finished, so a caller stops showing it as in flight.
func (r *sideReporter) settled() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	r.phase = PhaseDone
	r.known = r.done
	r.estimated = false
	r.emitLocked()
}

// emitLocked reports the current position, WITH THE LOCK HELD.
//
// Deliberately: the mutation and the report have to be one atomic step, or two
// goroutines can snapshot 5 and 6 and deliver them in the other order — which
// is a count going backwards for a reader, however monotonic the counter is.
// The callback is a map write under the caller's own mutex, so serialising it
// costs nothing worth measuring.
func (r *sideReporter) emitLocked() {
	if r.report == nil {
		return
	}
	r.report(Progress{
		Key: r.key, Side: r.side, Phase: r.phase,
		Done: r.done, Total: r.known, Estimated: r.estimated,
		Concurrency: r.concurrency,
	})
}

// Layer is one blob a component is made of.
type Layer struct {
	Digest    string
	Size      int64
	MediaType string
	// Title is `org.opencontainers.image.title`, which the vendor sets on the
	// layers of a generic artifact to name the FILE inside it. It is what makes
	// "which files changed" answerable at all; empty for an ordinary image
	// layer, which has no name and needs none.
	Title string
}

// Site is a place a component is reachable on one side, other than inside the
// bundle.
//
// A bundle's components are published twice: inside the bundle so its index
// still resolves, and under the component's own name so it can be pulled as
// itself. The second is the one a consumer uses and the one that silently fails
// to appear, so it is checked separately rather than assumed from the first.
type Site struct {
	Repository string
	// Present is whether the content is there at all.
	Present bool
	// TagDigest is what the component's own tag resolves to there, empty when
	// it resolves to nothing.
	TagDigest string
	// Error is why the site could not be asked, when it could not.
	Error string
}

// Item is one component as one side holds it.
type Item struct {
	// Key aligns this component with its counterpart on the other side.
	Key string
	// Type is what it is, in the words somebody uses: index, image, chart,
	// file, signature. FOR READING, never for identity — see Kind.
	Type string
	// Kind is what OCI says this artifact is: its `artifactType` where it has
	// one, its media type otherwise.
	//
	// Verbatim, and that is the point. Type is a friendly bucketing of these
	// values and several distinct artifacts share each bucket, so it can say
	// two things are alike when the registry says they are not. Identity uses
	// the specification's own answer; only the display uses ours.
	Kind string
	// Name is the vendor's name for it, or the bundle path where it has none.
	Name string
	// Tag is the name it answers to, from `ref.name` or from the root
	// reference.
	Tag    string
	Digest string
	// Size is what this component WEIGHS: its manifest plus its config plus
	// its layers.
	//
	// Not the manifest descriptor's size, which is what this used to be and is
	// the size of a few kilobytes of JSON. A reader comparing two releases is
	// asking what changed and how much of it there is; answering with the size
	// of the pointer rather than the thing reported a 900 MB image as 2 KB and
	// made every total on the page meaningless.
	Size       int64
	Repository string
	Depth      int
	// Named is the component's own site on this side. Nil where the component
	// names no repository of its own.
	Named  *Site
	Layers []Layer
	// Unreachable is set when this side's index NAMES this component and the
	// registry would not serve it — the signature of a transfer that stopped
	// part-way, and a finding rather than an error.
	Unreachable string
}

// Verdict is how one component's two sides relate.
type Verdict string

const (
	// VerdictSame is byte-for-byte identical content under the same name.
	VerdictSame Verdict = "same"
	// VerdictChanged is the same component holding different content — the
	// answer to "what is new in this release", and to "what was mutated".
	VerdictChanged Verdict = "changed"
	// VerdictOnlyA is present on the first side and absent from the second:
	// content that did not arrive, or a component a new release dropped.
	VerdictOnlyA Verdict = "only-a"
	// VerdictOnlyB is the reverse: content added by a new release, or content
	// at a destination that its source does not have.
	VerdictOnlyB Verdict = "only-b"
)

// Row is one component, on both sides.
type Row struct {
	Key     string
	Type    string
	Name    string
	Verdict Verdict
	A, B    *Item
	// Differences states each disagreement as a fact. Empty for VerdictSame.
	Differences []string
	// Files is the account of the NAMED FILES inside this component — the
	// answer to "which configuration changed", which "two layers changed"
	// cannot give.
	//
	// Every file of a component that differs, unchanged ones included, because
	// the unchanged ones are the context that makes the changed ones legible.
	// Empty for a component that agrees on both sides, where by construction
	// nothing inside it can differ.
	Files []FileDiff
}

// IsRoot reports whether this row is the bundle itself rather than one of its
// components.
func (r Row) IsRoot() bool { return r.Key == rootKey }

// Report is the whole comparison.
type Report struct {
	A, B SideSpec

	// ResolvedA and ResolvedB are the references each side was ACTUALLY walked
	// from, which is not always the one that was asked for.
	ResolvedA, ResolvedB string

	Rows []Row

	Same    int
	Changed int
	OnlyA   int
	OnlyB   int

	// ExtraTagsA and ExtraTagsB are tags in each side's BUNDLE repository
	// pointing at content this release does not account for.
	//
	// Judged by what a tag RESOLVES TO, never by how it is spelled: nothing in
	// the specification constrains how a publisher names a tag, so a
	// name-shaped test is a test of one vendor's convention. Asked only of the
	// bundle's own repository — a component's repository legitimately holds
	// every other version of that component, and asking it there would report
	// each previous release as a discrepancy.
	ExtraTagsA []string
	ExtraTagsB []string
	// ExtraTruncatedA and ExtraTruncatedB say the repository listed more tags
	// than this comparison would resolve, so the lists above are a partial
	// account of what is unexplained rather than the whole one.
	ExtraTruncatedA bool
	ExtraTruncatedB bool
}

// Differences is how many rows disagree.
func (r Report) Differences() int { return r.Changed + r.OnlyA + r.OnlyB }

// ReferenceA and ReferenceB render each side as a reference somebody could
// paste into a pull — of WHAT WAS WALKED, not of what was asked for.
//
// The distinction is itself a finding in the case that matters. A destination
// missing the vendor's wrapper tag is walked from its payload instead, and a
// header still reading `…:signed_orb_25.7_mp2604_2131` would be asserting
// something this comparison has already discovered to be false.
func (r Report) ReferenceA() string { return walkedReference(r.A, r.ResolvedA) }

// ReferenceB is ReferenceA for the second side.
func (r Report) ReferenceB() string { return walkedReference(r.B, r.ResolvedB) }

func walkedReference(spec SideSpec, resolved string) string {
	if resolved == "" {
		return spec.String()
	}
	if spec.Repository == "" {
		return resolved
	}
	// A digest is joined with `@`, a tag with `:`. Both spellings are pullable
	// and the wrong one is not, which matters because the whole point of this
	// string is that somebody can paste it.
	if strings.Contains(resolved, ":") {
		return spec.Repository + "@" + resolved
	}
	return spec.Repository + ":" + resolved
}

// Identical reports that the two sides agree completely, extras included.
func (r Report) Identical() bool {
	return r.Differences() == 0 && len(r.ExtraTagsA) == 0 && len(r.ExtraTagsB) == 0
}

// Run walks both sides and aligns them.
func Run(ctx context.Context, clientA, clientB ClientFactory, opts Options) (Report, error) {
	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = 4
	}

	// WHAT each side is walked from, decided for BOTH SIDES AT ONCE and before
	// either walk starts. Two independent resolutions can pick two different
	// artifacts, and everything downstream of that compares one thing against
	// another thing — see chooseRoots.
	rootA, rootB, notes, err := chooseRoots(ctx, clientA, clientB, opts)
	if err != nil {
		return Report{}, err
	}

	// Both sides at once. They are different registries in the case that
	// matters most — a vendor across a WAN and a destination in the datacentre
	// — and walking them in series would make every comparison cost the sum of
	// two round-trip-bound walks instead of the larger of them.
	// One reporter per side, shared by every phase that side passes through, so
	// the count a caller sees only ever goes up. See Progress.
	reporterA := newSideReporter("a", opts.A.Label, concurrency, opts.Progress)
	reporterB := newSideReporter("b", opts.B.Label, concurrency, opts.Progress)

	var (
		wg           sync.WaitGroup
		invA, invB   inventory
		errA, errB   error
		extraA, extB extras
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		invA, extraA, errA = readSide(ctx, clientA, opts.A, rootA, concurrency, opts.Classify, reporterA)
	}()
	go func() {
		defer wg.Done()
		invB, extB, errB = readSide(ctx, clientB, opts.B, rootB, concurrency, opts.Classify, reporterB)
	}()
	wg.Wait()

	if errA != nil {
		return Report{}, fmt.Errorf("read %s: %w", opts.A.Label, errA)
	}
	if errB != nil {
		return Report{}, fmt.Errorf("read %s: %w", opts.B.Label, errB)
	}

	report := Report{
		A: opts.A, B: opts.B,
		ResolvedA: rootA.chosen, ResolvedB: rootB.chosen,
		ExtraTagsA: extraA.Tags, ExtraTagsB: extB.Tags,
		ExtraTruncatedA: extraA.Truncated, ExtraTruncatedB: extB.Truncated,
	}
	report.Rows = align(invA, invB, notes)

	// The files inside whatever changed. Free: a component's manifest already
	// names its files and states their content digests, so this is two lists
	// aligned by path and no registry is troubled for it.
	inspectFiles(report.Rows)

	// Both sides are done. Said explicitly, so a caller stops rendering the
	// estimate as though more were still to come.
	reporterA.settled()
	reporterB.settled()

	for _, row := range report.Rows {
		switch row.Verdict {
		case VerdictSame:
			report.Same++
		case VerdictChanged:
			report.Changed++
		case VerdictOnlyA:
			report.OnlyA++
		case VerdictOnlyB:
			report.OnlyB++
		}
	}
	return report, nil
}

// inventory is one side's components, by key.
type inventory map[string]*Item

// add records one component, keeping two components that want the same key
// apart.
//
// A key already held by the SAME digest is one component the bundle references
// twice — a base image shared by two of them — and there is nothing to add.
//
// A key already held by DIFFERENT content is a name that does not identify one
// artifact, and dropping either of them loses a component from the comparison
// silently. The tag breaks the tie: it is deliberately not part of a key, since
// version-to-version alignment depends on two releases keying alike, but where
// a name is ambiguous the tag is the only thing left that distinguishes them.
//
// Deterministic across both sides, because both walk the same manifest bytes in
// the same order.
func (inv inventory) add(item *Item) {
	held, seen := inv[item.Key]
	switch {
	case !seen:
		inv[item.Key] = item
	case held.Digest == item.Digest:
		return
	default:
		item.Key += "\x00" + item.Tag
		if _, taken := inv[item.Key]; !taken {
			inv[item.Key] = item
		}
	}
}

// readSide walks one bundle and probes each component's own site.
//
// WHAT it walks from is decided by chooseRoots and handed in, not resolved
// here: a side that picks its own root cannot know whether the other side
// picked the same one.
func readSide(
	ctx context.Context, client ClientFactory, spec SideSpec, choice rootChoice,
	concurrency int, with vendors.Classifier, report *sideReporter,
) (inventory, extras, error) {
	root, desc, ref := choice.repo, choice.desc, choice.chosen

	// TOLERANT, and this is the difference between a comparison and an error
	// message. A transfer that stopped part-way leaves an index naming children
	// the destination does not have; a walk that aborted on the first of them
	// could not report the other nineteen — and "this side could not be walked"
	// is the least useful possible answer to "what is missing?".
	tree, missing, _, err := oci.WalkPartialProgress(ctx, root, desc, concurrency,
		func(p oci.Progress) { report.walked(p.Fetched, p.Known) })
	if err != nil {
		return nil, extras{}, fmt.Errorf("walk %s: %w", spec, err)
	}

	inv := make(inventory, len(tree.Artifacts)+len(missing))
	for i, a := range tree.Artifacts {
		inv.add(itemFrom(a, spec, ref, i == 0, with))
	}

	// A component the index names and the registry will not serve. Recorded
	// from the REFERENCING descriptor, so it keeps the name its parent gave it
	// and aligns against its counterpart on the other side — which is what
	// turns "something is missing" into "cfx-5000-product/lms is missing".
	for _, m := range missing {
		item := itemFrom(oci.Artifact{Descriptor: m.Descriptor, Depth: m.Depth},
			spec, ref, false, with)
		item.Unreachable = summarise(m.Err)
		inv.add(item)
	}

	probeNamedSites(ctx, client, inv, concurrency, report)

	// EXTRA TAGS ARE A QUESTION ABOUT A DESTINATION, and only about a
	// destination.
	//
	// The pass resolves every tag in the bundle's repository to find content
	// this release does not account for. At a target that is a real finding:
	// something landed there that nobody in this comparison put there. At a
	// SOURCE it is the vendor's own catalogue — every other release it has ever
	// published — reported as unaccounted content, which is noise on every
	// version-to-version comparison anybody runs.
	//
	// It was also the most expensive thing a comparison did, by a wide margin:
	// one resolve per tag in a repository holding years of releases, against
	// the vendor, on both sides. A comparison that had finished walking
	// appeared to stop for minutes, doing this.
	if !spec.PublishesComponentsByName {
		return inv, extras{}, nil
	}

	tags, truncated := extraTags(ctx, root, inv, spec,
		unwalkedRoots(ctx, root, choice, concurrency), concurrency, report)
	return inv, extras{Tags: tags, Truncated: truncated}, nil
}

// extras is one side's unexplained content, and whether the question was fully
// asked.
type extras struct {
	Tags      []string
	Truncated bool
}

// unwalkedRoots is the content reachable from a root THIS SIDE HOLDS but the
// comparison did not walk from.
//
// A side may hold a more complete root than the one both sides agreed on: the
// vendor has the signed wrapper, the destination does not, so the payload is
// what gets compared. The wrapper and the signature hanging off it are still
// part of the release — the vendor published them as part of it — and calling
// their tags "not part of this release" because of which root the OTHER side
// was missing states something false about this one.
//
// One request per unwalked root, and only its immediate children: anything
// deeper is reachable from the root that WAS walked, or that root would not
// have been a fallback for this one.
func unwalkedRoots(
	ctx context.Context, root registry.Repository, choice rootChoice, concurrency int,
) []string {
	var out []string
	for _, ref := range choice.order {
		desc, held := choice.holds[ref]
		if !held || ref == choice.chosen {
			continue
		}

		// FetchRoot reads one manifest and records the children it lists,
		// without descending — which is exactly the shape wanted here, and is
		// the same reader the rest of the system parses manifests with.
		tree, err := oci.FetchRoot(ctx, root, desc)
		if err != nil {
			out = append(out, string(desc.Digest))
			continue
		}
		for _, a := range tree.Artifacts {
			out = append(out, string(a.Descriptor.Digest))
			for _, b := range a.Blobs {
				out = append(out, string(b.Descriptor.Digest))
			}
		}
	}
	return out
}

// rootChoice is where one side will be walked from, and what else it holds.
type rootChoice struct {
	repo registry.Repository
	// holds maps every candidate reference this side RESOLVES to what it
	// resolves to. A candidate the side does not hold is simply absent, which
	// is what makes the two sides' disagreements comparable.
	holds map[string]registry.Descriptor
	// order is the candidate list, most complete first.
	order []string
	// firstErr is why the most complete candidate failed, kept so a side
	// holding none of them says something better than "not found".
	firstErr error

	// chosen and desc are what this side is walked from, filled in by
	// chooseRoots once BOTH sides are known.
	chosen string
	desc   registry.Descriptor
}

// chooseRoots decides what each side is walked from, preferring a reference
// BOTH sides hold.
//
// # Why this is not two independent resolutions
//
// It used to be: each side took the first candidate it could resolve, on its
// own. Where a destination was missing the vendor's wrapper tag, the two ends
// then silently walked DIFFERENT ARTIFACTS — the source's signed wrapper
// against the destination's bare payload — and every finding downstream was an
// artefact of that. The roots' digests differ because they are different
// manifests, not because anything was corrupted; the signature hanging off the
// wrapper is reported missing at a destination nobody looked for it on; and
// the counts at the bottom describe a comparison that never happened.
//
// Comparing like with like is the whole contract, so the shared reference wins
// even where one side could offer a more complete one. What that side has and
// the other does not is then reported as a FINDING on the root — which is the
// honest shape of it: "the destination is missing the wrapper tag" is a fact
// about the release, and "the two roots have different digests" was never a
// fact about anything.
//
// # Where the sides are MEANT to differ
//
// Comparing two versions gives the ends disjoint candidate lists on purpose.
// There is nothing shared to prefer and nothing to report, so the sides keep
// their own roots and referenceNotes stays silent.
func chooseRoots(
	ctx context.Context, clientA, clientB ClientFactory, opts Options,
) (rootChoice, rootChoice, []string, error) {
	var (
		wg         sync.WaitGroup
		a, b       rootChoice
		errA, errB error
	)
	wg.Add(2)
	go func() { defer wg.Done(); a, errA = probeRoot(ctx, clientA, opts.A) }()
	go func() { defer wg.Done(); b, errB = probeRoot(ctx, clientB, opts.B) }()
	wg.Wait()

	if errA != nil {
		return a, b, nil, fmt.Errorf("read %s: %w", opts.A.Label, errA)
	}
	if errB != nil {
		return a, b, nil, fmt.Errorf("read %s: %w", opts.B.Label, errB)
	}

	// The most complete reference both sides hold.
	for _, ref := range a.order {
		if _, ok := a.holds[ref]; !ok {
			continue
		}
		if _, ok := b.holds[ref]; !ok {
			continue
		}
		a.chosen, b.chosen = ref, ref
		break
	}
	if a.chosen == "" {
		// Nothing in common: two versions, or a destination holding none of
		// the references the source was found under. Each side walks the most
		// complete thing it has, which is the only answer available.
		a.chosen, b.chosen = firstHeld(a), firstHeld(b)
	}
	a.desc, b.desc = a.holds[a.chosen], b.holds[b.chosen]

	return a, b, referenceNotes(a, b), nil
}

// probeRoot asks one side about EVERY candidate, not just until one answers.
//
// The candidates a side does not hold are half the finding, and they are only
// knowable by asking. The cost is bounded and small — three or four manifest
// reads against one repository — against a walk that is about to make hundreds.
func probeRoot(
	ctx context.Context, client ClientFactory, spec SideSpec,
) (rootChoice, error) {
	if len(spec.References) == 0 {
		return rootChoice{}, errors.New("no reference to compare from")
	}

	root, err := client(spec.Repository)
	if err != nil {
		return rootChoice{}, fmt.Errorf("build a client for %s: %w", spec.Repository, err)
	}

	choice := rootChoice{
		repo:  root,
		holds: make(map[string]registry.Descriptor, len(spec.References)),
		order: spec.References,
	}

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for i, reference := range spec.References {
		wg.Add(1)
		go func(i int, reference string) {
			defer wg.Done()
			desc, err := resolveOne(ctx, root, reference)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if i == 0 {
					choice.firstErr = err
				}
				return
			}
			choice.holds[reference] = desc
		}(i, reference)
	}
	wg.Wait()

	if len(choice.holds) == 0 {
		if choice.firstErr != nil {
			return rootChoice{}, choice.firstErr
		}
		return rootChoice{}, fmt.Errorf("%s holds none of %s",
			spec.Repository, strings.Join(spec.References, ", "))
	}
	return choice, nil
}

// firstHeld is the most complete reference one side actually has.
func firstHeld(c rootChoice) string {
	for _, ref := range c.order {
		if _, ok := c.holds[ref]; ok {
			return ref
		}
	}
	return ""
}

// referenceNotes states, as facts, where the two sides disagree about which of
// the SAME references they hold.
//
// Only references both sides were asked about can disagree. Where the two
// candidate lists share nothing the ends are deliberately different things —
// two versions — and there is no disagreement to report.
func referenceNotes(a, b rootChoice) []string {
	shared := false
	for _, ref := range a.order {
		if containsString(b.order, ref) {
			shared = true
			break
		}
	}
	if !shared {
		return nil
	}

	var out []string
	for _, ref := range a.order {
		if !containsString(b.order, ref) {
			continue
		}
		_, hasA := a.holds[ref]
		_, hasB := b.holds[ref]
		switch {
		case hasA && !hasB:
			out = append(out, fmt.Sprintf(
				"%s resolves on the first side and not on the second", ref))
		case hasB && !hasA:
			out = append(out, fmt.Sprintf(
				"%s resolves on the second side and not on the first", ref))
		}
	}
	if len(out) == 0 {
		return nil
	}

	// What was compared INSTEAD, because a reader who has just been told a
	// reference is missing will otherwise reasonably assume nothing was.
	if a.chosen == b.chosen {
		out = append(out, fmt.Sprintf(
			"compared from %s, which both sides hold", a.chosen))
	} else {
		out = append(out, fmt.Sprintf(
			"no reference is held by both sides, so %s and %s were walked — these "+
				"are two artifacts rather than two copies of one", a.chosen, b.chosen))
	}
	return out
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func resolveOne(
	ctx context.Context, root registry.Repository, reference string,
) (registry.Descriptor, error) {
	if strings.Contains(reference, ":") {
		// Already a digest. Fetching by it also proves it is there, which is
		// the first thing a comparison wants to know.
		desc, _, err := root.FetchManifest(ctx, reference)
		if err != nil {
			return registry.Descriptor{}, fmt.Errorf("fetch %s: %w", reference, err)
		}
		return desc, nil
	}

	desc, err := root.ResolveTag(ctx, reference)
	if err != nil {
		return registry.Descriptor{}, fmt.Errorf("resolve %s: %w", reference, err)
	}
	return desc, nil
}

// itemFrom turns one walked artifact into a comparable component.
func itemFrom(
	a oci.Artifact, spec SideSpec, rootRef string, isRoot bool, with vendors.Classifier,
) *Item {
	ref := parseRefName(a.Descriptor.Annotations[registry.AnnotationRefName])

	item := &Item{
		Type:       classify(with, a.Descriptor, a.ConfigMediaType()),
		Kind:       kindOf(a.Descriptor),
		Digest:     string(a.Descriptor.Digest),
		Size:       contentSize(a),
		Repository: spec.Repository,
		Depth:      a.Depth,
		Tag:        ref.tag,
		Name:       ref.repository,
	}

	switch {
	case isRoot:
		// The bundle itself. Keyed by a constant so two versions of one orb
		// align on their roots rather than each looking like a component the
		// other lacks.
		item.Key = rootKey
		item.Name = spec.Repository
		if item.Tag == "" {
			item.Tag = rootRef
		}
	case ref.repository != "":
		// The repository AND what OCI says the artifact is.
		//
		// The repository alone is not an identity. `ref.name` is a full
		// reference — repository and tag — and only its repository half is
		// stable enough to align two releases by, so a bundle naming two
		// children after one repository leaves the halves we keep identical.
		// That is not hypothetical: a pre-1.1 vendor bundling a payload with
		// its detached signature names both after the bundle, and keyed by
		// repository alone they collided, one silently won, and the release's
		// SIGNATURE was absent from both sides of every comparison that walked
		// such a bundle.
		//
		// artifactType is the field the specification reserves for exactly this
		// question, and unlike the tag it does not change from release to
		// release — so it separates them at no cost to the alignment the tag is
		// deliberately kept out of the key for.
		item.Key = strings.ToLower(ref.repository) + "\x00" + item.Kind
	default:
		// Named nothing. It can only ever match itself, which is the honest
		// answer for content whose only identity is its bytes.
		item.Key = "\x00digest/" + string(a.Descriptor.Digest)
		item.Name = a.Descriptor.Digest.Short()
	}

	// LAYERS ONLY. The config blob is metadata about the component — its
	// entrypoint, its chart values schema — and it is not a file inside it.
	// Counting it as one made every component's file account read as
	// incomplete, because a config carries no title and an untitled blob is
	// something we cannot name without opening it.
	for _, b := range a.Blobs {
		if b.Kind != "layer" {
			continue
		}
		item.Layers = append(item.Layers, Layer{
			Digest:    string(b.Descriptor.Digest),
			Size:      b.Descriptor.Size,
			MediaType: b.Descriptor.MediaType,
			Title:     b.Descriptor.Annotations[annotationTitle],
		})
	}

	// Only where this side is expected to serve components under their own
	// names. See SideSpec.PublishesComponentsByName: the expectation belongs to
	// a destination this system wrote, and asking it of a vendor's registry
	// reports the vendor's layout as a defect once per component.
	if ref.repository != "" && !isRoot && spec.PublishesComponentsByName {
		item.Named = &Site{
			Repository: transfer.DestinationPath(spec.BasePath, ref.repository),
		}
	}
	return item
}

// rootKey aligns the two sides' roots with each other rather than with a
// component.
const rootKey = "\x00root"

// annotationTitle names the file inside a generic artifact's layer.
const annotationTitle = "org.opencontainers.image.title"

// probeNamedSites asks each side whether its components are pullable under
// their own names.
//
// The site a consumer actually uses, and the one that silently fails to appear:
// a tag applied to the bundle and not to the component leaves a destination
// that passes every content-addressed check and serves nothing anybody asked
// for. Failures are recorded on the site rather than returned, because a
// component whose own repository does not exist is a FINDING, not an error.
func probeNamedSites(
	ctx context.Context, client ClientFactory, inv inventory, concurrency int,
	report *sideReporter,
) {
	var (
		mu      sync.Mutex
		clients = map[string]registry.Repository{}
	)
	clientFor := func(path string) (registry.Repository, error) {
		mu.Lock()
		defer mu.Unlock()
		if c, ok := clients[path]; ok {
			return c, nil
		}
		c, err := client(path)
		if err != nil {
			return nil, err
		}
		clients[path] = c
		return c, nil
	}

	// Announced with the REAL population — components that have a name to
	// probe — rather than with the size of the inventory. A source is not
	// expected to serve its components under their own names, so for a
	// version-to-version comparison this phase has NOTHING to do, and
	// announcing 259 units of work it will never perform left the progress
	// display stopped at "0 of 259" for the rest of the comparison.
	total := 0
	for _, item := range inv {
		if item.Named != nil {
			total++
		}
	}
	report.expect(PhaseNames, total*2) // one presence check, one tag resolve

	var done atomic.Int64
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, item := range inv {
		if item.Named == nil {
			continue
		}
		wg.Add(1)
		go func(it *Item) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// Counted whatever the outcome: a component whose own repository
			// does not exist is a finding, and the probe for it happened.
			defer func() {
				done.Add(1)
				report.did(PhaseNames, 2)
			}()

			repo, err := clientFor(it.Named.Repository)
			if err != nil {
				it.Named.Error = err.Error()
				return
			}
			if _, _, err := repo.FetchManifest(ctx, it.Digest); err == nil {
				it.Named.Present = true
			} else if !errors.Is(err, registry.ErrNotFound) {
				it.Named.Error = summarise(err)
			}
			if it.Tag == "" {
				return
			}
			if desc, err := repo.ResolveTag(ctx, it.Tag); err == nil {
				it.Named.TagDigest = string(desc.Digest)
			}
		}(item)
	}
	wg.Wait()
}

// extraTags lists tags in the BUNDLE's repository that point at content this
// release does not account for.
//
// Asked only of the bundle's own repository. A component's repository
// legitimately holds every other version of that component, and asking it there
// would report each previous release as a discrepancy.
//
// A registry that will not list tags yields nothing rather than an error. The
// comparison's findings do not depend on it.
//
// # Judged by CONTENT, never by the shape of a tag name
//
// The first version of this compared tag NAMES against the names in the
// inventory, and that is not a question OCI lets you answer. A tag is a pointer;
// what it points AT is the only thing that says whether the release accounts for
// it. Nothing in the specification constrains how a publisher spells a tag, so a
// name-shaped test is a test of one vendor's convention wearing generic clothes.
//
// It failed exactly that way. NEAR tags every component inside the orb's own
// repository — one tag per component, spelled from the component's digest — so a
// correct orb reported hundreds of "unexplained" tags, every one of them
// pointing at a manifest the walk had just matched. They were missing from the
// inventory only because a component's Tag comes from its `ref.name`, which is a
// different string for the same content.
//
// So every listed tag is RESOLVED, and accounted for if what it resolves to is
// in this release. That costs one HEAD per tag and needs no knowledge of any
// vendor's spelling, which is the trade this makes deliberately: the cheap
// answer was the wrong answer.
func extraTags(
	ctx context.Context, root registry.Repository, inv inventory, spec SideSpec,
	alsoAccounted []string, concurrency int, report *sideReporter,
) (extra []string, truncated bool) {
	lister, ok := root.(registry.TagLister)
	if !ok {
		return nil, false
	}
	accounted := accountedDigests(inv, alsoAccounted)

	var listed []string
	last := ""
	for range 20 { // bounded: a bundle repository holding 4000 tags is not one
		tags, next, err := lister.ListTags(ctx, last, 200)
		if err != nil {
			return nil, false
		}
		listed = append(listed, tags...)
		if next == "" {
			break
		}
		last = next
	}

	if len(listed) > maxResolvedTags {
		// REPORTED rather than absorbed. Silently treating the remainder as
		// unexplained would invent findings, and silently dropping it would
		// hide them; saying the question was not fully asked is the only
		// answer that is true.
		listed, truncated = listed[:maxResolvedTags], true
	}

	// THE PHASE NOBODY COULD SEE. One resolve per tag in the repository, and a
	// vendor's bundle repository holds every version it has ever published —
	// which on a real catalogue is more requests than the manifest walk. It ran
	// silently, so a comparison that had finished walking appeared to stop.
	report.expect(PhaseTags, len(listed))

	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, concurrency)
	)
	for _, tag := range listed {
		wg.Add(1)
		go func(tag string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			defer report.did(PhaseTags, 1)

			desc, err := root.ResolveTag(ctx, tag)
			// A tag the registry listed and will not resolve is KEPT. It
			// exists, so dropping it because a second request failed would
			// quietly shrink the finding.
			if err == nil && accounted[string(desc.Digest)] {
				return
			}

			mu.Lock()
			defer mu.Unlock()
			extra = append(extra, tag)
		}(tag)
	}
	wg.Wait()

	sort.Strings(extra)
	return extra, truncated
}

// maxResolvedTags bounds the requests one side's extra-content check may make.
//
// Generous, because the cost is one HEAD each and they run in parallel, and
// because the alternative to asking is guessing. A repository holding more tags
// than this is not a bundle repository, and the truncation is reported.
const maxResolvedTags = 2000

// accountedDigests is every piece of content this release explains.
//
// Layers as well as manifests: a vendor that tags a component's LAYER inside the
// bundle repository has still not put anything there the release does not
// account for.
func accountedDigests(inv inventory, also []string) map[string]bool {
	out := map[string]bool{}
	for _, item := range inv {
		out[item.Digest] = true
		for _, layer := range item.Layers {
			out[layer.Digest] = true
		}
	}
	for _, digest := range also {
		out[digest] = true
	}
	return out
}

// align pairs the two inventories by key.
//
// rootNotes are findings about the ROOTS themselves — which references each
// side holds — and they are attached to the root's row before anything is
// sorted, so a root the notes have just made interesting sorts with the other
// differences rather than under the agreements.
func align(a, b inventory, rootNotes []string) []Row {
	keys := make(map[string]bool, len(a)+len(b))
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}

	rows := make([]Row, 0, len(keys))
	for key := range keys {
		row := compareItems(a[key], b[key])
		if key == rootKey && len(rootNotes) > 0 {
			// A reference one side holds and the other does not IS a
			// difference — the release is not addressable the same way in both
			// places — so it counts as one rather than being a footnote.
			row.Differences = append(row.Differences, rootNotes...)
			if row.Verdict == VerdictSame {
				row.Verdict = VerdictChanged
			}
		}
		rows = append(rows, row)
	}

	// Differences first, then by type, then by name. The reason somebody runs
	// this is to find what is different; making them scroll past two thousand
	// identical rows to reach three defeats it.
	sort.SliceStable(rows, func(i, j int) bool {
		if pi, pj := rank(rows[i].Verdict), rank(rows[j].Verdict); pi != pj {
			return pi < pj
		}
		if rows[i].Type != rows[j].Type {
			return rows[i].Type < rows[j].Type
		}
		return rows[i].Name < rows[j].Name
	})
	return rows
}

func rank(v Verdict) int {
	switch v {
	case VerdictOnlyA:
		return 0
	case VerdictOnlyB:
		return 1
	case VerdictChanged:
		return 2
	default:
		return 3
	}
}

// compareItems states how one component's two sides relate.
func compareItems(a, b *Item) Row {
	switch {
	case a != nil && b == nil:
		return Row{
			Key: a.Key, Type: a.Type, Name: a.Name, Verdict: VerdictOnlyA, A: a,
			Differences: []string{"present on the first side only"},
		}
	case a == nil && b != nil:
		return Row{
			Key: b.Key, Type: b.Type, Name: b.Name, Verdict: VerdictOnlyB, B: b,
			Differences: []string{"present on the second side only"},
		}
	}

	row := Row{Key: a.Key, Type: a.Type, Name: a.Name, Verdict: VerdictSame, A: a, B: b}

	for _, side := range []struct {
		label string
		item  *Item
	}{{"the first side", a}, {"the second side", b}} {
		if side.item.Unreachable != "" {
			row.Differences = append(row.Differences, fmt.Sprintf(
				"referenced by the index on %s but not served there: %s",
				side.label, side.item.Unreachable))
		}
	}

	if a.Digest != b.Digest {
		row.Verdict = VerdictChanged
		row.Differences = append(row.Differences, fmt.Sprintf(
			"content differs: %s and %s", short(a.Digest), short(b.Digest)))
	}
	if a.Tag != b.Tag {
		row.Verdict = VerdictChanged
		row.Differences = append(row.Differences, fmt.Sprintf(
			"named %s and %s", quoteTag(a.Tag), quoteTag(b.Tag)))
	}

	// The component's own site, checked per side. A bundle that is byte-perfect
	// while its components are not pullable under their own names is the exact
	// failure this system shipped with, and it is invisible to any digest
	// comparison.
	row.Differences = append(row.Differences, siteDifferences(a, b)...)
	if len(row.Differences) > 0 {
		row.Verdict = VerdictChanged
	}
	return row
}

// siteDifferences reports a component that is not pullable under its own name
// on one side.
func siteDifferences(a, b *Item) []string {
	var out []string
	for _, side := range []struct {
		label string
		item  *Item
	}{{"the first side", a}, {"the second side", b}} {
		site := side.item.Named
		// An unreachable component has already been reported as such; adding
		// "and it is not published under its own name either" is a second
		// sentence about one fact.
		if site == nil || side.item.Unreachable != "" {
			continue
		}
		switch {
		case site.Error != "":
			out = append(out, fmt.Sprintf("%s could not be asked about %s: %s",
				side.label, site.Repository, site.Error))
		case !site.Present:
			out = append(out, fmt.Sprintf("not published as %s on %s",
				site.Repository, side.label))
		case side.item.Tag != "" && site.TagDigest == "":
			out = append(out, fmt.Sprintf("%s is not tagged %s on %s",
				site.Repository, side.item.Tag, side.label))
		case site.TagDigest != "" && site.TagDigest != side.item.Digest:
			out = append(out, fmt.Sprintf("%s:%s points at %s on %s, not %s",
				site.Repository, side.item.Tag, short(site.TagDigest),
				side.label, short(side.item.Digest)))
		}
	}
	return out
}

// kindOf is the specification's own answer to what an artifact is.
//
// `artifactType` where the artifact declares one — OCI 1.1 reserves it for
// precisely this — and the media type otherwise, which is what every artifact
// predating 1.1 has instead. Neither is interpreted here: this is an identity,
// and interpreting it is how an identity stops distinguishing things.
func kindOf(desc registry.Descriptor) string {
	if desc.ArtifactType != "" {
		return desc.ArtifactType
	}
	return desc.MediaType
}

// classify says what an artifact IS, in the words somebody uses about it.
//
// Delegated rather than answered here, because a transfer summary and a release
// page answer the same question about the same content: two classifiers that
// disagree describe one registry as two, and leave the reader no way to tell
// which of them is wrong.
//
// The CONFIG media type is passed because the descriptor alone cannot tell a
// Helm chart from an image — both are image manifests, and only the config says
// which. The walk has already fetched it. The ANNOTATIONS are passed for the
// vendor whose config does not say either, which is the case that made a NEAR
// orb's 97 charts invisible everywhere they were counted.
func classify(with vendors.Classifier, desc registry.Descriptor, configMediaType string) string {
	if with == nil {
		with = vendors.OCIOnly
	}
	return with(desc.MediaType, desc.ArtifactType, configMediaType, desc.Annotations)
}

// contentSize is what an artifact weighs, from the manifest the walk fetched.
//
// Manifest plus config plus layers, which is what a person means by the size of
// an image or a chart. An artifact the walk only LISTED — a child of an index
// on a side that could not be read further — has no blob list, and its
// descriptor size is the honest answer for it: the pointer is all we have.
func contentSize(a oci.Artifact) int64 {
	if len(a.Blobs) == 0 {
		return a.Descriptor.Size
	}
	total := int64(len(a.Raw))
	for _, b := range a.Blobs {
		total += b.Descriptor.Size
	}
	return total
}

// refName is a parsed org.opencontainers.image.ref.name.
type refName struct {
	repository string
	tag        string
}

// parseRefName splits `orbs/CFX-5000-k8s/nginx:1.2.3` into path and tag.
//
// The colon must come after the last slash to be a tag separator: a registry
// host may carry a port, and `near.example.com:5000/orbs/x` is a path with no
// tag in it at all.
func parseRefName(v string) refName {
	v = strings.TrimSpace(v)
	if v == "" {
		return refName{}
	}
	i := strings.LastIndex(v, ":")
	if i < 0 || i < strings.LastIndex(v, "/") {
		return refName{repository: v}
	}
	return refName{repository: v[:i], tag: v[i+1:]}
}

func quoteTag(tag string) string {
	if tag == "" {
		return "nothing"
	}
	return tag
}

// summarise trims a registry error to the part that says what went wrong.
func summarise(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i > 0 && len(msg)-i < 80 {
		return strings.TrimSpace(msg[i+2:])
	}
	return msg
}

func short(digest string) string {
	algo, hex, ok := strings.Cut(digest, ":")
	if !ok || len(hex) < 12 {
		return digest
	}
	return algo + ":" + hex[:12]
}
