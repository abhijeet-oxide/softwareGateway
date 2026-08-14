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

// Report is the whole comparison.
type Report struct {
	A, B SideSpec

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
		invA, extraA, errA = readSide(ctx, clientA, opts.A, concurrency)
	}()
	go func() {
		defer wg.Done()
		invB, extB, errB = readSide(ctx, clientB, opts.B, concurrency)
	}()
	wg.Wait()

	if errA != nil {
		return Report{}, fmt.Errorf("read %s: %w", opts.A.Label, errA)
	}
	if errB != nil {
		return Report{}, fmt.Errorf("read %s: %w", opts.B.Label, errB)
	}

	report := Report{A: opts.A, B: opts.B, ExtraTagsA: extraA, ExtraTagsB: extB}
	report.Rows = align(invA, invB)

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

// readSide walks one bundle and probes each component's own site.
func readSide(
	ctx context.Context, client ClientFactory, spec SideSpec, concurrency int,
) (inventory, []string, error) {
	root, err := client(spec.Repository)
	if err != nil {
		return nil, nil, fmt.Errorf("build a client for %s: %w", spec.Repository, err)
	}

	desc, ref, err := resolveRoot(ctx, root, spec.References)
	if err != nil {
		return nil, nil, err
	}

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
		item := itemFrom(a, spec, ref, i == 0)
		// A digest appearing twice in one bundle is one component referenced
		// twice, not two components.
		if _, seen := inv[item.Key]; seen {
			continue
		}
		inv[item.Key] = item
	}

	// A component the index names and the registry will not serve. Recorded
	// from the REFERENCING descriptor, so it keeps the name its parent gave it
	// and aligns against its counterpart on the other side — which is what
	// turns "something is missing" into "cfx-5000-product/lms is missing".
	for _, m := range missing {
		item := itemFrom(oci.Artifact{Descriptor: m.Descriptor, Depth: m.Depth},
			spec, ref, false)
		item.Unreachable = summarise(m.Err)
		if _, seen := inv[item.Key]; seen {
			continue
		}
		inv[item.Key] = item
	}

	probeNamedSites(ctx, client, inv, concurrency)
	return inv, extraTags(ctx, root, inv, spec), nil
}

// resolveRoot finds the first candidate reference this side actually holds.
//
// Reports which one it used, because the answer is part of the finding: a side
// holding only the payload where the other holds the signed wrapper is worth
// seeing in the header rather than silently walking two different things.
func resolveRoot(
	ctx context.Context, root registry.Repository, references []string,
) (registry.Descriptor, string, error) {
	if len(references) == 0 {
		return registry.Descriptor{}, "", errors.New("no reference to compare from")
	}

	var firstErr error
	for _, reference := range references {
		desc, err := resolveOne(ctx, root, reference)
		if err == nil {
			return desc, reference, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return registry.Descriptor{}, "", firstErr
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
		item.Key = "\x00root"
		item.Name = spec.Repository
		if item.Tag == "" {
			item.Tag = rootRef
		}
	case ref.repository != "":
		item.Key = strings.ToLower(ref.repository)
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

	if ref.repository != "" && !isRoot {
		item.Named = &Site{
			Repository: transfer.DestinationPath(spec.BasePath, ref.repository),
		}
	}
	return item
}

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
func extraTags(
	ctx context.Context, root registry.Repository, inv inventory, spec SideSpec,
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

	var out []string
	last := ""
	for range 20 { // bounded: a bundle repository holding 4000 tags is not one
		tags, next, err := lister.ListTags(ctx, last, 200)
		if err != nil {
			return nil
		}
		for _, tag := range tags {
			if !known[tag] {
				out = append(out, tag)
			}
		}
		if next == "" {
			break
		}
		last = next
	}
	sort.Strings(out)
	return out
}

// align pairs the two inventories by key.
func align(a, b inventory) []Row {
	keys := make(map[string]bool, len(a)+len(b))
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}

	rows := make([]Row, 0, len(keys))
	for key := range keys {
		rows = append(rows, compareItems(a[key], b[key]))
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
