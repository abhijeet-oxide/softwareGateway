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
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/abhijeet-oxide/softwareGateway/internal/oci"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/transfer"
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
	// References are candidate roots, most complete first. The first that
	// resolves is walked.
	//
	// Plural because a vendor may bundle the payload and its signature under a
	// wrapper index: walking the wrapper reaches both, so it is preferred — but
	// a destination that holds only the payload is a real and ordinary state,
	// and falling back to it beats reporting the whole side missing.
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
	// FileBudget is how many bytes of LAYER CONTENT this comparison may
	// download in order to say which files changed rather than which layers.
	//
	// Zero leaves every layer opaque. It is a byte budget rather than a list of
	// artifact types worth opening, because a budget needs no vendor plugin and
	// degrades correctly for a vendor nobody has written one for: a four-kilobyte
	// configuration bundle is opened, a two-gigabyte image layer is not.
	FileBudget int64
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
	// file, signature.
	Type string
	// Name is the vendor's name for it, or the bundle path where it has none.
	Name string
	// Tag is the name it answers to, from `ref.name` or from the root
	// reference.
	Tag        string
	Digest     string
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
	// FilesAdded, FilesRemoved and FilesChanged name the FILES inside the
	// component's layers — the answer to "one line of one config file moved",
	// which "two layers changed" cannot give.
	//
	// Three lists rather than two: an edited file is CHANGED, not added and
	// removed, and reporting it as both would double every finding.
	FilesAdded   []string
	FilesRemoved []string
	FilesChanged []string
	// FilesTruncated says a layer was left unopened — past the budget, or not
	// an archive — so the lists above are a partial account rather than the
	// whole one.
	FilesTruncated bool
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

	// ExtraTagsA and ExtraTagsB are tags in each side's BUNDLE repository that
	// this bundle does not account for.
	//
	// Reported only for the bundle's own repository, where the question is well
	// defined: a NEAR orb gets a repository to itself, so anything else in it
	// is genuinely unexplained. It is deliberately not asked of a component's
	// repository, which legitimately holds every other version of that
	// component and would report each one as a discrepancy.
	ExtraTagsA []string
	ExtraTagsB []string
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
	var (
		wg           sync.WaitGroup
		invA, invB   inventory
		errA, errB   error
		extraA, extB []string
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		invA, extraA, errA = readSide(ctx, clientA, opts.A, rootA, concurrency)
	}()
	go func() {
		defer wg.Done()
		invB, extB, errB = readSide(ctx, clientB, opts.B, rootB, concurrency)
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
		ExtraTagsA: extraA, ExtraTagsB: extB,
	}
	report.Rows = align(invA, invB, notes)

	// The files inside whatever changed. Only for rows that already disagree:
	// a component whose digest matches on both sides is byte-identical, and
	// opening it could not produce a finding.
	inspectFiles(ctx, clientA, clientB, report.Rows, opts.FileBudget, concurrency)

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
	concurrency int,
) (inventory, []string, error) {
	root, desc, ref := choice.repo, choice.desc, choice.chosen

	// TOLERANT, and this is the difference between a comparison and an error
	// message. A transfer that stopped part-way leaves an index naming children
	// the destination does not have; a walk that aborted on the first of them
	// could not report the other nineteen — and "this side could not be walked"
	// is the least useful possible answer to "what is missing?".
	tree, missing, _, err := oci.WalkPartial(ctx, root, desc, concurrency)
	if err != nil {
		return nil, nil, fmt.Errorf("walk %s: %w", spec, err)
	}

	inv := make(inventory, len(tree.Artifacts)+len(missing))
	for i, a := range tree.Artifacts {
		inv.add(itemFrom(a, spec, ref, i == 0))
	}

	// A component the index names and the registry will not serve. Recorded
	// from the REFERENCING descriptor, so it keeps the name its parent gave it
	// and aligns against its counterpart on the other side — which is what
	// turns "something is missing" into "cfx-5000-product/lms is missing".
	for _, m := range missing {
		item := itemFrom(oci.Artifact{Descriptor: m.Descriptor, Depth: m.Depth},
			spec, ref, false)
		item.Unreachable = summarise(m.Err)
		inv.add(item)
	}

	probeNamedSites(ctx, client, inv, concurrency)
	return inv, extraTags(ctx, root, inv, spec, unwalkedRoots(ctx, root, choice),
		concurrency), nil
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
	ctx context.Context, root registry.Repository, choice rootChoice,
) []string {
	var out []string
	for _, ref := range choice.order {
		desc, held := choice.holds[ref]
		if !held || ref == choice.chosen {
			continue
		}

		out = append(out, string(desc.Digest))
		_, raw, err := root.FetchManifest(ctx, string(desc.Digest))
		if err != nil {
			continue
		}
		var body struct {
			Manifests []struct {
				Digest string `json:"digest"`
			} `json:"manifests"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			continue
		}
		for _, child := range body.Manifests {
			out = append(out, child.Digest)
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
func itemFrom(a oci.Artifact, spec SideSpec, rootRef string, isRoot bool) *Item {
	ref := parseRefName(a.Descriptor.Annotations[registry.AnnotationRefName])

	item := &Item{
		Type:       classify(a.Descriptor),
		Digest:     string(a.Descriptor.Digest),
		Size:       a.Descriptor.Size,
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
		// The repository AND what the artifact is.
		//
		// The repository alone is not an identity wherever a vendor names two
		// children after the bundle itself, and NEAR does exactly that: its
		// wrapper's two children are `orbs/cfx-5000-k8s:orb_25.7…` and
		// `orbs/cfx-5000-k8s:signature_orb_25.7…`, one repository and two
		// artifacts. Keyed by repository they collided, one silently won, and
		// the release's signature vanished from both sides of every comparison
		// — while the tag it left behind was then reported as unexplained
		// content in the destination.
		//
		// The type is stable across releases in the way the TAG is not, so it
		// separates them without costing the version-to-version alignment the
		// tag is deliberately kept out of the key for.
		item.Key = strings.ToLower(ref.repository) + "\x00" + item.Type
	default:
		// Named nothing. It can only ever match itself, which is the honest
		// answer for content whose only identity is its bytes.
		item.Key = "\x00digest/" + string(a.Descriptor.Digest)
		item.Name = a.Descriptor.Digest.Short()
	}

	for _, b := range a.Blobs {
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

// extraTags lists tags in the BUNDLE's repository that this bundle does not
// account for.
//
// Asked only of the bundle's own repository, where the question is well
// defined: a NEAR orb gets a repository to itself, so anything else in it is
// unexplained and worth naming. Asking it of a component's repository would
// report every other version of that component as a discrepancy, which is the
// opposite of useful.
//
// A registry that will not list tags yields nothing rather than an error. The
// comparison's findings do not depend on it.
//
// # A tag is not extra merely because the bundle does not NAME it
//
// This is a listing of the repository, not a reading of the manifest, and the
// two answer different questions. The walk above already accumulated every
// image, chart and file the orb's index references; this asks the separate
// question of what ELSE is in the repository, and its first version answered it
// by name alone.
//
// That was wrong wherever a vendor tags its components inside the bundle's own
// repository. NEAR does exactly that — `docker_<digest>`, `helmoci_<digest>`,
// one per component — so a correct orb reported two hundred and fifty
// "unexplained" tags, every one of them naming content the comparison had just
// walked and matched. The tag names are not in the inventory because a
// component's `Tag` comes from its `ref.name` (`2507.2131.0`), which is a
// different string for the same manifest.
//
// So a tag is checked against the CONTENT the bundle accounts for, twice: by
// the digest its name embeds, which is free, and then by what it actually
// resolves to, which is not and is therefore bounded. Only a tag that survives
// both is genuinely unexplained.
func extraTags(
	ctx context.Context, root registry.Repository, inv inventory, spec SideSpec,
	alsoAccounted []string, concurrency int,
) []string {
	lister, ok := root.(registry.TagLister)
	if !ok {
		return nil
	}

	known := map[string]bool{}
	for _, item := range inv {
		if item.Tag != "" && sameRepo(item.Repository, spec.Repository) {
			known[item.Tag] = true
		}
	}
	for _, ref := range spec.References {
		known[ref] = true
	}
	accounted := accountedDigests(inv, alsoAccounted)

	var out []string
	last := ""
	for range 20 { // bounded: a bundle repository holding 4000 tags is not one
		tags, next, err := lister.ListTags(ctx, last, 200)
		if err != nil {
			return nil
		}
		for _, tag := range tags {
			if known[tag] || namesAccountedDigest(tag, accounted) {
				continue
			}
			out = append(out, tag)
		}
		if next == "" {
			break
		}
		last = next
	}

	out = dropTagsIntoBundle(ctx, root, out, accounted, concurrency)
	sort.Strings(out)
	return out
}

// accountedDigests is every piece of content this bundle explains, indexed by
// the first digits of its digest.
//
// Layers as well as manifests: a vendor that tags a component's LAYER inside
// the bundle repository has still not put anything there the bundle does not
// account for.
//
// Indexed by prefix rather than held as whole digests so a tag naming a
// SHORTENED digest still matches — the convention is the vendor's and nothing
// obliges it to use all sixty-four characters.
func accountedDigests(inv inventory, also []string) map[string][]string {
	out := map[string][]string{}
	add := func(digest string) {
		hex := hexOf(digest)
		if len(hex) < minDigestHex {
			return
		}
		key := hex[:minDigestHex]
		out[key] = append(out[key], hex)
	}
	for _, item := range inv {
		add(item.Digest)
		for _, layer := range item.Layers {
			add(layer.Digest)
		}
	}
	for _, digest := range also {
		add(digest)
	}
	return out
}

// minDigestHex is how much of a digest a tag must embed before that is
// evidence rather than coincidence. Sixteen bytes: long enough that no version
// number reaches it, short enough to match a vendor's abbreviation.
const minDigestHex = 32

// namesAccountedDigest reports whether a tag NAMES content the bundle
// accounts for.
func namesAccountedDigest(tag string, accounted map[string][]string) bool {
	for _, run := range hexRuns(tag) {
		for _, full := range accounted[run[:minDigestHex]] {
			if strings.HasPrefix(full, run) || strings.HasPrefix(run, full) {
				return true
			}
		}
	}
	return false
}

// hexRuns returns the maximal hexadecimal stretches of a tag that are long
// enough to be a digest rather than a version.
//
// Maximal runs rather than a prefix match on a known separator, because the
// separator is the vendor's: `docker_<digest>` and `helmoci_<digest>` are
// NEAR's spelling, and nothing here should have to be taught the next one.
func hexRuns(tag string) []string {
	var (
		out   []string
		start = -1
	)
	for i := 0; i <= len(tag); i++ {
		if i < len(tag) && isHexDigit(tag[i]) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 && i-start >= minDigestHex {
			out = append(out, tag[start:i])
		}
		start = -1
	}
	return out
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func hexOf(digest string) string {
	_, hex, ok := strings.Cut(digest, ":")
	if !ok {
		return digest
	}
	return hex
}

// maxProbedTags bounds what the name check could not settle.
//
// A repository holding more than this many genuinely unexplained tags has a
// problem no number of requests will clarify, and the listing is already the
// finding at that point.
const maxProbedTags = 128

// dropTagsIntoBundle removes the tags that turn out to point at content this
// bundle already accounts for.
//
// The name check above is free and settles the vendor conventions we have seen;
// this settles the rest by asking the registry what the tag actually resolves
// to, which is the only answer that does not depend on a naming convention.
//
// A tag that cannot be resolved is KEPT. It was listed by the registry, so it
// exists, and dropping it because a second request failed would quietly shrink
// the finding.
func dropTagsIntoBundle(
	ctx context.Context, root registry.Repository, tags []string,
	accounted map[string][]string, concurrency int,
) []string {
	if len(tags) == 0 || len(tags) > maxProbedTags {
		return tags
	}

	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		sem  = make(chan struct{}, concurrency)
		keep = make([]string, 0, len(tags))
	)
	for _, tag := range tags {
		wg.Add(1)
		go func(tag string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			desc, err := root.ResolveTag(ctx, tag)
			if err == nil && namesAccountedDigest(hexOf(string(desc.Digest)), accounted) {
				return
			}

			mu.Lock()
			defer mu.Unlock()
			keep = append(keep, tag)
		}(tag)
	}
	wg.Wait()
	return keep
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

// classify says what an artifact IS, in the words somebody uses about it.
//
// Media type first, because that is what the specification defines and what the
// registry actually serves; `artifactType` second, for the OCI 1.1 artifacts
// that use it. Never guessed from a name: a repository called `charts/` holding
// an image is a mislabelled repository, not a chart.
func classify(desc registry.Descriptor) string {
	switch {
	case registry.IsIndex(desc.MediaType):
		return "index"
	case strings.Contains(desc.ArtifactType, "helm"),
		strings.Contains(desc.MediaType, "helm"):
		return "chart"
	case strings.Contains(desc.ArtifactType, "signature"),
		strings.Contains(desc.ArtifactType, "sig"):
		return "signature"
	case strings.Contains(desc.ArtifactType, "generic"):
		return "file"
	case desc.MediaType == "":
		return "artifact"
	default:
		return "image"
	}
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

func sameRepo(a, b string) bool {
	return strings.EqualFold(strings.Trim(a, "/"), strings.Trim(b, "/"))
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
