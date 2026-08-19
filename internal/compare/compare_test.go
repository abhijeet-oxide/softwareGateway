package compare_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/compare"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/generic"
	"github.com/abhijeet-oxide/softwareGateway/test/fakeregistry"
)

// Five questions, one mechanism.
//
// The tests are grouped by the question an operator is asking, because that is
// what has to keep working - not by which function is being called. Every one of
// them is `walk two bundles and align their components`, and if any of them
// needed a special case in the engine, the engine would be wrong.

const (
	sourcePath = "orbs/cfx-5000-k8s"
	targetBase = "nokia-lab"
	release    = "orb_23.8.1076"
	older      = "orb_23.7.900"
	wrapped    = "signed_orb_23.8.1076"

	// componentPath is where a component's `ref.name` says it belongs, and it
	// is DELIBERATELY NOT UNDER sourcePath.
	//
	// That is the real shape and the fixture used to get it wrong. A NEAR
	// component is annotated with the coordinate the vendor BUILT it from -
	// `cfx-5000-product/admin:2507.2131.0` - which is a different tree from the
	// `orbs/…` repository the orb is served out of. Modelling the name as a
	// path beneath the bundle made the source look like a place that could
	// plausibly serve it, which is exactly the assumption that shipped a
	// comparison reporting 253 false differences.
	componentPath = "cfx-5000-product"
)

// ---------------------------------------------------------------------------
// Did the transfer land?  source vs target, same version
// ---------------------------------------------------------------------------

func TestAFaithfulCopyIsIdentical(t *testing.T) {
	f := newFixture(t)
	f.publish(f.src, sourcePath, release, componentsOf(release))
	f.copyToTarget(release)

	report := f.compare(f.sourceSide(release), f.targetSide(release))

	if !report.Identical() {
		t.Fatalf("a faithful copy reported %d differences:\n%s",
			report.Differences(), describe(report))
	}
	if report.Same == 0 {
		t.Fatal("nothing was compared; the fixture is not producing rows")
	}
}

// The failure this system actually shipped with: every blob and every manifest
// pushed, and the component's own tag never applied. Every digest agrees, so
// nothing content-addressed can catch it - the release is simply not pullable.
func TestAComponentThatIsNotPullableUnderItsOwnNameIsReported(t *testing.T) {
	f := newFixture(t)
	f.publish(f.src, sourcePath, release, componentsOf(release))
	f.copyToTarget(release)
	f.dst.RemoveTag(targetBase+"/"+componentPath+"/nginx", "1.2.3")

	report := f.compare(f.sourceSide(release), f.targetSide(release))

	row, ok := rowFor(report, "nginx")
	if !ok {
		t.Fatalf("no row for the component:\n%s", describe(report))
	}
	if row.Verdict != compare.VerdictChanged {
		t.Fatalf("verdict = %q, want changed:\n%s", row.Verdict, describe(report))
	}
	if !mentions(row, "not tagged 1.2.3") {
		t.Errorf("the difference does not say the tag is missing: %v", row.Differences)
	}
}

// A PARTIAL TRANSFER: the bundle's index arrived and one of the components it
// names did not.
//
// The walk of that side cannot simply fail. An index naming children the
// registry will not serve is exactly what a transfer that stopped part-way
// leaves behind, and a comparison that aborted on the first of them could not
// report the other nineteen.
func TestAPartialTransferIsReportedComponentByComponent(t *testing.T) {
	f := newFixture(t)
	f.publish(f.src, sourcePath, release, componentsOf(release))
	f.copyToTarget(release)
	// A partial transfer: the bundle's index arrived, one component did not.
	f.dst.RemoveManifest(targetBase+"/"+sourcePath, f.digestOf("nginx"))
	f.dst.RemoveManifest(targetBase+"/"+componentPath+"/nginx", f.digestOf("nginx"))

	report := f.compare(f.sourceSide(release), f.targetSide(release))

	if report.Differences() == 0 {
		t.Fatalf("a partial transfer reported no differences:\n%s", describe(report))
	}
	row, ok := rowFor(report, "nginx")
	if !ok {
		t.Fatalf("no row for the missing component:\n%s", describe(report))
	}
	if !mentions(row, "not served") {
		t.Errorf("the difference does not say the component is referenced and "+
			"not served: %v", row.Differences)
	}
	// And the REST of the bundle is still compared: a partial transfer must not
	// cost the reader every other finding.
	if report.Same == 0 {
		t.Errorf("nothing else was compared after the missing component:\n%s",
			describe(report))
	}
}

// THE 253 FALSE FINDINGS.
//
// A vendor does not publish its components under their own names - NEAR keeps
// them inside the orb repository - and a component's `ref.name` is the upstream
// coordinate it was built from, not a path the vendor serves. Probing the
// source for it returns 404 for every component of every orb, and the report
// that produced said `not published as cfx-5000-product/… on the first side`
// two hundred and fifty-three times about a transfer that had copied every byte
// correctly.
//
// The expectation belongs to the end this system WROTE. Asking it of a vendor
// is asking whether the vendor has adopted our layout.
func TestAVendorIsNotAskedToPublishComponentsOurWay(t *testing.T) {
	f := newFixture(t)
	f.publish(f.src, sourcePath, release, componentsOf(release))
	f.copyToTarget(release)

	report := f.compare(f.sourceSide(release), f.targetSide(release))

	for _, row := range report.Rows {
		for _, d := range row.Differences {
			if strings.Contains(d, "not published as") &&
				strings.Contains(d, "on the first side") {
				t.Errorf("the vendor's own layout is reported as a defect: %s", d)
			}
		}
	}
	if !report.Identical() {
		t.Fatalf("a faithful copy from a vendor-shaped source reported %d "+
			"differences:\n%s", report.Differences(), describe(report))
	}
}

// AND THE EXPECTATION STILL HOLDS WHERE IT BELONGS. Removing the flag entirely
// would trade 253 false findings for the loss of the one finding this check
// exists for.
func TestTheDestinationIsStillHeldToIt(t *testing.T) {
	f := newFixture(t)
	f.publish(f.src, sourcePath, release, componentsOf(release))
	f.copyToTarget(release)
	f.dst.RemoveManifest(targetBase+"/"+componentPath+"/nginx", f.digestOf("nginx"))

	report := f.compare(f.sourceSide(release), f.targetSide(release))

	row, ok := rowFor(report, "nginx")
	if !ok {
		t.Fatalf("no row for the component:\n%s", describe(report))
	}
	if !mentions(row, "not published as") {
		t.Errorf("a destination that did not publish a component under its own "+
			"name was not reported: %v", row.Differences)
	}
}

// ---------------------------------------------------------------------------
// Which reference is each side walked from?
// ---------------------------------------------------------------------------

// TWO SIDES, ONE ARTIFACT. The destination holds the signed wrapper but not the
// tag naming it - which is what a transfer whose final tag never landed leaves.
//
// Resolving each side independently walked the wrapper on one side and the
// payload on the other, then reported the two roots' digests as a content
// difference and the wrapper's signature as missing from a destination nobody
// had looked for it on. Both statements were artefacts of comparing two
// different things.
func TestBothSidesAreWalkedFromAReferenceTheyBothHold(t *testing.T) {
	f := newFixture(t)
	f.publish(f.src, sourcePath, release, componentsOf(release))
	wrapper := f.publishWrapper(f.src, sourcePath, wrapped, release)
	f.copyToTarget(wrapped)

	// The tag never landed. The content did.
	f.dst.RemoveTag(targetBase+"/"+sourcePath, wrapped)

	refs := []string{wrapped, wrapper, release}
	report := f.compare(f.sourceSide(refs...), f.targetSide(refs...))

	if report.ResolvedA != report.ResolvedB {
		t.Fatalf("the sides were walked from %q and %q; a comparison of two "+
			"different artifacts is not a comparison", report.ResolvedA, report.ResolvedB)
	}
	if report.ResolvedA != wrapper {
		t.Errorf("walked from %q, want the wrapper %q - the most complete "+
			"reference both sides hold", report.ResolvedA, wrapper)
	}

	root, ok := rootRow(report)
	if !ok {
		t.Fatalf("no row for the bundle root:\n%s", describe(report))
	}
	if !mentions(root, wrapped+" resolves on the first side and not on the second") {
		t.Errorf("the missing tag is not reported as a missing tag: %v", root.Differences)
	}
	if mentions(root, "content differs") {
		t.Errorf("two copies of one artifact are reported as differing content: %v",
			root.Differences)
	}
	// And the signature the wrapper carries is on BOTH sides, because both
	// sides were walked from the wrapper.
	for _, row := range report.Rows {
		if row.Verdict == compare.VerdictOnlyA {
			t.Errorf("%s is reported as missing from a destination that has it:\n%s",
				row.Name, describe(report))
		}
	}
}

// TWO CHILDREN, ONE NAME. NEAR's wrapper names both of its children after the
// bundle itself - `orbs/cfx-5000-k8s:orb_25.7…` and
// `orbs/cfx-5000-k8s:signature_orb_25.7…` - so a key built from the repository
// alone collided, one child silently won, and the release's SIGNATURE was
// absent from both sides of every comparison that walked a wrapper.
//
// It was worse than a missing row. The signature's tag was still in the
// destination's repository and no longer accounted for by any component, so it
// came back as unexplained content nobody had put there.
func TestTwoComponentsNamedAfterTheBundleAreBothCompared(t *testing.T) {
	f := newFixture(t)
	f.publish(f.src, sourcePath, release, componentsOf(release))
	wrapper := f.publishWrapper(f.src, sourcePath, wrapped, release)
	f.copyToTarget(wrapped)

	refs := []string{wrapped, wrapper}
	report := f.compare(f.sourceSide(refs...), f.targetSide(refs...))

	var payload, signature bool
	for _, row := range report.Rows {
		if row.IsRoot() || row.Name != sourcePath {
			continue
		}
		switch row.Type {
		case "index":
			payload = true
		case "image", "signature":
			signature = true
		}
	}
	if !payload {
		t.Errorf("the payload the wrapper names is not a row:\n%s", describe(report))
	}
	if !signature {
		t.Errorf("the signature the wrapper names is not a row - it collided with "+
			"the payload and was dropped:\n%s", describe(report))
	}
	if !report.Identical() {
		t.Fatalf("a faithful copy of a wrapped release reported %d differences "+
			"and %v/%v unexplained:\n%s", report.Differences(),
			report.ExtraTagsA, report.ExtraTagsB, describe(report))
	}
}

// Comparing two VERSIONS gives the ends disjoint candidate lists on purpose.
// There is nothing shared to prefer and nothing to report, and a note saying
// the two sides hold different references would fire on every delta.
func TestTwoVersionsAreNotReportedAsAReferenceDisagreement(t *testing.T) {
	f := newFixture(t)
	f.publish(f.src, sourcePath, older, componentsOf(older))
	f.publish(f.src, sourcePath, release, componentsOf(release))

	report := f.compare(f.sourceSide(older), f.sourceSide(release))

	root, ok := rootRow(report)
	if !ok {
		t.Fatalf("no row for the bundle root:\n%s", describe(report))
	}
	for _, d := range root.Differences {
		if strings.Contains(d, "resolves on the") {
			t.Errorf("comparing two versions reported their versions as a "+
				"reference disagreement: %s", d)
		}
	}
}

// ---------------------------------------------------------------------------
// What changed in this release?  one place, two versions
// ---------------------------------------------------------------------------

// The delta case, and the one the previous design could not express at all.
// Both ends are the same registry; only the version differs.
func TestTwoVersionsInOnePlaceReportWhatChanged(t *testing.T) {
	f := newFixture(t)

	// 23.7 has nginx and a chart. 23.8 changes nginx, keeps the chart, and adds
	// a component that did not exist before.
	f.publish(f.src, sourcePath, older, []component{
		{name: "nginx", tag: "1.2.2", payload: "nginx v1.2.2"},
		{name: "charts/cfx", tag: "23.7.900", payload: "chart", chart: true},
	})
	f.publish(f.src, sourcePath, release, []component{
		{name: "nginx", tag: "1.2.3", payload: "nginx v1.2.3"},
		{name: "charts/cfx", tag: "23.7.900", payload: "chart", chart: true},
		{name: "mcc", tag: "25.7.2503", payload: "brand new"},
	})

	report := f.compare(f.sourceSide(older), f.sourceSide(release))

	nginx, ok := rowFor(report, "nginx")
	if !ok {
		t.Fatalf("no row for nginx:\n%s", describe(report))
	}
	if nginx.Verdict != compare.VerdictChanged {
		t.Errorf("nginx verdict = %q, want changed", nginx.Verdict)
	}
	if !mentions(nginx, "1.2.2") || !mentions(nginx, "1.2.3") {
		t.Errorf("the difference does not name both versions: %v", nginx.Differences)
	}

	mcc, ok := rowFor(report, "mcc")
	if !ok {
		t.Fatalf("no row for the component the new release added:\n%s", describe(report))
	}
	if mcc.Verdict != compare.VerdictOnlyB {
		t.Errorf("a component only the newer release has has verdict %q, want only-b",
			mcc.Verdict)
	}

	chart, ok := rowFor(report, "charts/cfx")
	if !ok {
		t.Fatalf("no row for the unchanged chart:\n%s", describe(report))
	}
	if chart.Verdict != compare.VerdictSame {
		t.Errorf("an unchanged chart has verdict %q, want same: %v",
			chart.Verdict, chart.Differences)
	}
}

// Which FILES changed, not merely which digests. A generic artifact's layers
// are files and the vendor titles them, which is what makes the question
// answerable literally.
func TestChangedFilesAreNamed(t *testing.T) {
	f := newFixture(t)

	f.publish(f.src, sourcePath, older, []component{
		{name: "custo", tag: "23.7.900", files: map[string]string{
			"DOCUMENTATION/readme":    "old readme",
			"CONFIGURATION/one.json":  `{"replicas":1}`,
			"CONFIGURATION/gone.json": "removed next release",
		}},
	})
	f.publish(f.src, sourcePath, release, []component{
		{name: "custo", tag: "23.8.1076", files: map[string]string{
			"DOCUMENTATION/readme":   "old readme",
			"CONFIGURATION/one.json": `{"replicas":3}`,
			"CONFIGURATION/new.json": "added this release",
		}},
	})

	report := f.compare(f.sourceSide(older), f.sourceSide(release))

	row, ok := rowFor(report, "custo")
	if !ok {
		t.Fatalf("no row for the generic artifact:\n%s", describe(report))
	}

	assertFileChange(t, row, fileChange{
		changed: []string{"CONFIGURATION/one.json"},
		added:   []string{"CONFIGURATION/new.json"},
		removed: []string{"CONFIGURATION/gone.json"},
		absent:  []string{"DOCUMENTATION/readme"},
	})
}

// A LAYER THAT NAMES NOTHING IS NOT A FILE.
//
// An image layer, or a tar somebody packed without titling it, holds an unknown
// number of paths and cannot be aligned with anything on the other side. It is
// excluded from the file account rather than listed as `layer sha256:…`, which
// would restate the digest difference in the column that exists to say more
// than the digest.
//
// This is also what makes the comparison free. It used to DOWNLOAD such layers
// to look inside them - every layer of every changed component, on both sides -
// which was the whole of the time a comparison took, and was spent to compute
// digest comparisons the manifests already answered.
func TestALayerWithNoNameIsNotReportedAsAFile(t *testing.T) {
	f := newFixture(t)

	f.publish(f.src, sourcePath, older, []component{
		{name: "custo", tag: "23.7.900", archive: map[string]string{
			"CONFIGURATION/one.json": `{"replicas":1}`,
		}},
	})
	f.publish(f.src, sourcePath, release, []component{
		{name: "custo", tag: "23.8.1076", archive: map[string]string{
			"CONFIGURATION/one.json": `{"replicas":3}`,
		}},
	})

	report := f.compare(f.sourceSide(older), f.sourceSide(release))

	row, ok := rowFor(report, "custo")
	if !ok {
		t.Fatalf("no row for the artifact:\n%s", describe(report))
	}
	if len(row.Files) != 0 {
		t.Errorf("an untitled layer is reported as a file: %v", row.Files)
	}
	// The component is still changed: the digests differ, and that is knowable
	// without opening anything at all.
	if row.Verdict != compare.VerdictChanged {
		t.Errorf("verdict = %q, want changed", row.Verdict)
	}
}

// A file present on both sides with the same digest is the SAME file, and it is
// carried rather than dropped: the unchanged files of a component that changed
// are the context that makes the changed ones legible.
func TestUnchangedFilesAreCarriedAndMarkedUnchanged(t *testing.T) {
	f := newFixture(t)

	f.publish(f.src, sourcePath, older, []component{
		{name: "custo", tag: "23.7.900", files: map[string]string{
			"DOCUMENTATION/readme":   "old readme",
			"CONFIGURATION/one.json": `{"replicas":1}`,
		}},
	})
	f.publish(f.src, sourcePath, release, []component{
		{name: "custo", tag: "23.8.1076", files: map[string]string{
			"DOCUMENTATION/readme":   "old readme",
			"CONFIGURATION/one.json": `{"replicas":3}`,
		}},
	})

	row, _ := rowFor(f.compare(f.sourceSide(older), f.sourceSide(release)), "custo")

	var readme *compare.FileDiff
	for i := range row.Files {
		if row.Files[i].Path == "DOCUMENTATION/readme" {
			readme = &row.Files[i]
		}
	}
	if readme == nil {
		t.Fatalf("the untouched file is missing from the account: %v", row.Files)
	}
	if readme.Verdict != compare.VerdictSame {
		t.Errorf("an untouched file has verdict %q, want same", readme.Verdict)
	}
	if readme.DigestA == "" || readme.DigestA != readme.DigestB {
		t.Errorf("both content digests are not carried: %q / %q",
			readme.DigestA, readme.DigestB)
	}
}

// fileChange is what a row is expected to say about the files inside it.
type fileChange struct {
	changed, added, removed []string
	// absent must appear in none of the three: an untouched file reported as
	// changed makes a one-line edit read as a wholesale rewrite.
	absent []string
}

func assertFileChange(t *testing.T, row compare.Row, want fileChange) {
	t.Helper()

	verdicts := map[string]compare.Verdict{}
	for _, f := range row.Files {
		verdicts[f.Path] = f.Verdict
	}

	expect := func(paths []string, want compare.Verdict) {
		for _, path := range paths {
			if got := verdicts[path]; got != want {
				t.Errorf("%s has verdict %q, want %q: %v", path, got, want, row.Files)
			}
		}
	}
	expect(want.changed, compare.VerdictChanged)
	expect(want.added, compare.VerdictOnlyB)
	expect(want.removed, compare.VerdictOnlyA)
	// An untouched file is CARRIED, and must be marked as untouched: reporting
	// it as a change makes a one-line edit read as a wholesale rewrite.
	expect(want.absent, compare.VerdictSame)
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Did the promotion land?  target vs target
// ---------------------------------------------------------------------------

func TestTwoTargetsAreComparedTheSameWay(t *testing.T) {
	f := newFixture(t)
	f.publish(f.src, sourcePath, release, componentsOf(release))
	f.copyToTarget(release)

	// A second destination on the same registry, as a promotion produces.
	prodBase := "nokia-prod"
	f.copyTo(f.dst, prodBase, release)

	lab := f.targetSide(release)
	prod := f.targetSide(release)
	prod.Label = "prod"
	prod.BasePath = prodBase
	prod.Repository = prodBase + "/" + sourcePath

	if report := f.compare(lab, prod); !report.Identical() {
		t.Fatalf("two identical destinations reported %d differences:\n%s",
			report.Differences(), describe(report))
	}

	// Now mutate one of them, which is what a promotion check has to catch.
	f.dst.RemoveTag(prodBase+"/"+componentPath+"/nginx", "1.2.3")
	report := f.compare(lab, prod)
	if report.Differences() == 0 {
		t.Fatal("a destination missing a tag matched one that has it")
	}
}

// ---------------------------------------------------------------------------
// Is there anything there nobody put?
// ---------------------------------------------------------------------------

// A NEAR orb gets a repository to itself, so a tag in it that this release does
// not account for is genuinely unexplained - an old version left behind, or
// something published by hand.
func TestUnexplainedTagsInTheBundleRepositoryAreReported(t *testing.T) {
	f := newFixture(t)
	f.publish(f.src, sourcePath, release, componentsOf(release))
	f.copyToTarget(release)

	// Something else in the destination's orb folder.
	f.dst.AddImage(targetBase+"/"+sourcePath, "orb_23.6.500",
		fakeregistry.NewLayer("a release nobody asked this tool to copy"))

	report := f.compare(f.sourceSide(release), f.targetSide(release))

	if len(report.ExtraTagsB) == 0 {
		t.Fatalf("an unexplained tag in the destination's bundle repository was "+
			"not reported:\n%s", describe(report))
	}
	if report.ExtraTagsB[0] != "orb_23.6.500" {
		t.Errorf("reported %v, want orb_23.6.500", report.ExtraTagsB)
	}
	// And it is not a component difference: nothing about the release changed.
	if report.Differences() != 0 {
		t.Errorf("an unrelated tag was counted as a component difference:\n%s",
			describe(report))
	}
	if report.Identical() {
		t.Error("a side with unexplained content reported itself identical")
	}
}

// THE OTHER HALF OF THE 253.
//
// NEAR tags every component inside the orb's own repository - one tag per
// component, spelled from that component's digest - so a listing of that
// repository returns hundreds of names the inventory does not contain. It does
// not contain them because a component's Tag comes from its `ref.name`
// (`2507.2131.0`), a different string for the same manifest.
//
// Judged by NAME they were all unexplained, and a correct orb printed a wall of
// them under "not part of this release". Judged by what they point AT - which
// is the only question OCI lets you ask about a tag - there is nothing there
// the release does not account for.
func TestTagsNamingContentTheBundleAccountsForAreNotUnexplained(t *testing.T) {
	f := newFixture(t)
	f.publish(f.src, sourcePath, release, componentsOf(release))
	f.copyToTarget(release)

	// The vendor's own spelling, for content the walk has already matched.
	for _, name := range []string{"nginx", "charts/cfx"} {
		digest := f.digestOf(name)
		f.src.AddManifest(sourcePath, "docker_"+strings.TrimPrefix(digest, "sha256:"),
			f.src.Manifest(sourcePath, digest), registry.MediaTypeOCIManifest)
	}
	// AND A SPELLING NOBODY HAS SEEN, for the same content. A check that reads
	// tag names passes the two above and fails this one; a check that resolves
	// them cannot tell the difference, which is the whole point.
	f.src.AddManifest(sourcePath, "whatever-the-next-vendor-calls-it",
		f.src.Manifest(sourcePath, f.digestOf("nginx")), registry.MediaTypeOCIManifest)

	report := f.compare(f.sourceSide(release), f.targetSide(release))

	if len(report.ExtraTagsA) != 0 {
		t.Errorf("the vendor's own tags for content in this release are reported "+
			"as unexplained: %v", report.ExtraTagsA)
	}
	if !report.Identical() {
		t.Fatalf("a faithful copy reported %d differences:\n%s",
			report.Differences(), describe(report))
	}
}

// A SIDE MAY HOLD MORE OF THE RELEASE THAN WAS COMPARED, and what it holds is
// still part of the release.
//
// Where a destination has neither the wrapper's tag nor the wrapper, the
// payload is what both sides can be compared from - so the signature is not in
// the walked tree, and the vendor's `signature_orb_…` tag was reported back to
// the vendor as content nobody had put there. The release is precisely where
// that tag came from.
func TestWhatAnUnwalkedRootReferencesIsStillPartOfTheRelease(t *testing.T) {
	f := newFixture(t)
	f.publish(f.src, sourcePath, release, componentsOf(release))
	wrapper := f.publishWrapper(f.src, sourcePath, wrapped, release)

	// The destination got the payload and its components, and nothing of the
	// wrapper - neither the tag nor the artifact.
	f.copyToTarget(release)

	refs := []string{wrapped, wrapper, release}
	report := f.compare(f.sourceSide(refs...), f.targetSide(refs...))

	if report.ResolvedA != release || report.ResolvedB != release {
		t.Fatalf("walked %q and %q, want the payload %q - the only reference "+
			"both sides hold", report.ResolvedA, report.ResolvedB, release)
	}
	for _, tag := range report.ExtraTagsA {
		if strings.HasPrefix(tag, "signature_") || tag == wrapped {
			t.Errorf("%s is reported as not part of the release that publishes "+
				"it: %v", tag, report.ExtraTagsA)
		}
	}
}

// A tag whose NAME says nothing, pointing at content the bundle has. The name
// check cannot settle this one, so it is settled by asking the registry what
// the tag resolves to - the only answer that does not depend on a convention.
func TestATagResolvingIntoTheBundleIsNotUnexplained(t *testing.T) {
	f := newFixture(t)
	f.publish(f.src, sourcePath, release, componentsOf(release))
	f.copyToTarget(release)

	digest := f.digestOf("nginx")
	f.src.AddManifest(sourcePath, "latest", f.src.Manifest(sourcePath, digest),
		registry.MediaTypeOCIManifest)

	report := f.compare(f.sourceSide(release), f.targetSide(release))

	if len(report.ExtraTagsA) != 0 {
		t.Errorf("a tag pointing at content in this release is reported as "+
			"unexplained: %v", report.ExtraTagsA)
	}
}

// ---------------------------------------------------------------------------
// Was anything mutated?
// ---------------------------------------------------------------------------

// A tag pointing at content that is not what the source published. Every digest
// in the bundle is present, so only asking what the tag RESOLVES to catches it.
func TestATagPointingAtSomethingElseIsReported(t *testing.T) {
	f := newFixture(t)
	f.publish(f.src, sourcePath, release, componentsOf(release))
	f.copyToTarget(release)

	f.dst.AddImage(targetBase+"/"+componentPath+"/nginx", "1.2.3",
		fakeregistry.NewLayer("a completely different image"))

	report := f.compare(f.sourceSide(release), f.targetSide(release))

	row, ok := rowFor(report, "nginx")
	if !ok {
		t.Fatalf("no row for the component:\n%s", describe(report))
	}
	if !mentions(row, "points at") {
		t.Errorf("the difference does not say the tag resolves elsewhere: %v",
			row.Differences)
	}
}

// ---------------------------------------------------------------------------
// Ordering
// ---------------------------------------------------------------------------

// Differences first. Somebody runs this to find what is different, and making
// them scroll past two thousand agreeing rows defeats the command.
func TestDifferencesSortToTheTop(t *testing.T) {
	f := newFixture(t)
	f.publish(f.src, sourcePath, release, componentsOf(release))
	f.copyToTarget(release)
	f.dst.RemoveTag(targetBase+"/"+componentPath+"/nginx", "1.2.3")

	report := f.compare(f.sourceSide(release), f.targetSide(release))

	if len(report.Rows) < 2 {
		t.Fatalf("only %d rows; the ordering is not exercised", len(report.Rows))
	}
	seenSame := false
	for _, row := range report.Rows {
		if row.Verdict == compare.VerdictSame {
			seenSame = true
			continue
		}
		if seenSame {
			t.Errorf("a difference sorts below an agreement:\n%s", describe(report))
			break
		}
	}
}

// ---------------------------------------------------------------------------
// fixture
// ---------------------------------------------------------------------------

type component struct {
	name    string
	tag     string
	payload string
	chart   bool
	// files, when set, makes this an artifact with ONE TITLED LAYER PER FILE -
	// the ORAS convention, used by Helm and cosign and everything else that
	// publishes non-image artifacts.
	files map[string]string
	// archive, when set, makes this an artifact with ONE LAYER CONTAINING ALL
	// THE FILES - a tar, which is what the OCI image specification says a layer
	// is. This is the shape whose digest tells you nothing.
	archive map[string]string
	// gzip compresses that archive, as `+gzip` in the media type declares.
	gzip bool
}

func componentsOf(version string) []component {
	return []component{
		{name: "nginx", tag: "1.2.3", payload: "nginx payload " + version},
		{name: "charts/cfx", tag: "23.8.1076", payload: "chart tarball", chart: true},
	}
}

type fixture struct {
	t        *testing.T
	src, dst *fakeregistry.Registry
	// digests maps a component name to what it was published as, so a test can
	// remove exactly one thing.
	digests map[string]string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	src := fakeregistry.New()
	t.Cleanup(src.Close)
	dst := fakeregistry.New()
	t.Cleanup(dst.Close)

	return &fixture{t: t, src: src, dst: dst, digests: map[string]string{}}
}

// publish writes a bundle: each component under its own name, and an index
// naming them with the reserved OCI annotation.
func (f *fixture) publish(
	reg *fakeregistry.Registry, repo, tag string, components []component,
) {
	f.t.Helper()

	children := make([]map[string]any, 0, len(components))
	for _, c := range components {
		digest := f.publishComponent(reg, repo, c)
		f.digests[c.name] = digest
		children = append(children, map[string]any{
			"mediaType": registry.MediaTypeOCIManifest,
			"digest":    digest,
			"size":      100,
			"annotations": map[string]string{
				registry.AnnotationRefName: componentPath + "/" + c.name + ":" + c.tag,
			},
		})
	}

	raw, err := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": registry.MediaTypeOCIIndex,
		"manifests": children,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	reg.AddManifest(repo, tag, raw, registry.MediaTypeOCIIndex)
}

func (f *fixture) publishComponent(
	reg *fakeregistry.Registry, repo string, c component,
) string {
	f.t.Helper()

	var layers []fakeregistry.Layer
	switch {
	case c.archive != nil:
		layers = append(layers, fakeregistry.NewLayer(archiveOf(c.archive, c.gzip)))
	case c.files != nil:
		for path, content := range c.files {
			layer := fakeregistry.NewLayer(content)
			layer.Annotations = map[string]string{
				"org.opencontainers.image.title": path,
			}
			layers = append(layers, layer)
		}
	case c.chart:
		layer := fakeregistry.NewLayer(c.payload)
		layer.MediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
		layers = append(layers, layer)
	default:
		layers = append(layers, fakeregistry.NewLayer(c.payload))
	}

	// INSIDE THE BUNDLE, UNTAGGED, AND NOWHERE ELSE - which is all a vendor
	// does.
	//
	// This used to also publish the component under its own name, and that one
	// line was the reason a whole class of false finding was invisible to every
	// test here. Publishing each component separately is what a TRANSFER does
	// at a DESTINATION (internal/transfer/layout.go). A source does not do it:
	// NEAR keeps its components in the orb repository and tags them
	// `docker_<digest>`. A fixture that pre-satisfied the invariant on the
	// source made compare's probe of the source look correct, and it is not.
	return reg.AddImage(repo, "", layers...)
}

// publishWrapper adds the signed wrapper NEAR puts over a release: an index
// naming the payload and a detached signature, each by the reserved OCI
// annotation.
//
// The children name the BUNDLE repository, not a repository of their own,
// which is what the vendor writes - `orbs/cfx-5000-k8s:orb_23.8.1076`.
func (f *fixture) publishWrapper(
	reg *fakeregistry.Registry, repo, tag, payloadTag string,
) string {
	f.t.Helper()

	payload := reg.TagDigest(repo, payloadTag)
	if payload == "" {
		f.t.Fatalf("no payload %s:%s to wrap", repo, payloadTag)
	}
	// THREE TAGS PER RELEASE, which is what NEAR publishes: the payload, the
	// signature, and the wrapper binding them. The signature carrying a tag of
	// its own is the whole reason it can be reported as unexplained content
	// when the comparison did not walk the wrapper.
	signature := reg.AddImage(repo, "signature_"+payloadTag,
		fakeregistry.NewLayer("a pkcs7 signature"))

	children := []map[string]any{
		{
			"mediaType": registry.MediaTypeOCIIndex,
			"digest":    payload,
			"size":      len(reg.Manifest(repo, payload)),
			"annotations": map[string]string{
				registry.AnnotationRefName: repo + ":" + payloadTag,
			},
		},
		{
			"mediaType": registry.MediaTypeOCIManifest,
			"digest":    signature,
			"size":      len(reg.Manifest(repo, signature)),
			"annotations": map[string]string{
				registry.AnnotationRefName: repo + ":signature_" + payloadTag,
			},
		},
	}

	raw, err := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": registry.MediaTypeOCIIndex,
		"manifests": children,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return reg.AddManifest(repo, tag, raw, registry.MediaTypeOCIIndex)
}

// copyToTarget reproduces at the destination exactly what a correct transfer
// leaves there.
func (f *fixture) copyToTarget(tag string) {
	f.t.Helper()
	f.copyTo(f.dst, targetBase, tag)
}

// copyTo mirrors the source bundle beneath a base path, the way the layout puts
// it: every artifact inside the bundle repository, and each component under its
// own name too.
func (f *fixture) copyTo(reg *fakeregistry.Registry, base, tag string) {
	f.t.Helper()

	root := f.src.TagDigest(sourcePath, tag)
	if root == "" {
		f.t.Fatalf("the source has no %s:%s", sourcePath, tag)
	}
	f.copyArtifact(reg, base, root, tag)
}

// copyArtifact copies one artifact and everything beneath it.
//
// Children first, and recursively: a wrapper index names a payload index which
// names the components, and a copy that only went one level deep would leave
// the destination unable to resolve its own root. It is also what the registry
// requires - an index may only be pushed once its children are servable.
//
// The named publication happens HERE, from the referencing descriptor, which is
// where the name lives. That mirrors internal/transfer/layout.go exactly: the
// artifact goes in its parent's repository so the bundle resolves, and again
// under whatever its `ref.name` calls it.
func (f *fixture) copyArtifact(
	reg *fakeregistry.Registry, base, digest, tag string,
) {
	f.t.Helper()

	raw := f.src.Manifest(sourcePath, digest)
	if raw == nil {
		f.t.Fatalf("the source has no manifest %s", digest)
	}

	var body struct {
		MediaType string `json:"mediaType"`
		Manifests []struct {
			Digest      string            `json:"digest"`
			MediaType   string            `json:"mediaType"`
			Annotations map[string]string `json:"annotations"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		f.t.Fatal(err)
	}

	dstRepo := base + "/" + sourcePath
	for _, child := range body.Manifests {
		f.copyArtifact(reg, base, child.Digest, "")

		repo, refTag := splitRefName(child.Annotations[registry.AnnotationRefName])
		if repo == "" {
			continue
		}
		reg.AddManifest(base+"/"+repo, refTag, f.src.Manifest(sourcePath, child.Digest),
			mediaTypeOr(child.MediaType))
	}
	reg.AddManifest(dstRepo, tag, raw, mediaTypeOr(body.MediaType))
}

func mediaTypeOr(mediaType string) string {
	if mediaType == "" {
		return registry.MediaTypeOCIManifest
	}
	return mediaType
}

func (f *fixture) digestOf(component string) string {
	f.t.Helper()
	digest, ok := f.digests[component]
	if !ok {
		f.t.Fatalf("the fixture published no component %q", component)
	}
	return digest
}

// sourceSide is the vendor: it holds the vendor's paths unprefixed and owes
// nothing under a component's own name.
func (f *fixture) sourceSide(references ...string) compare.SideSpec {
	return compare.SideSpec{
		Label: "near", Repository: sourcePath, References: references,
	}
}

// targetSide is a place this system wrote, and therefore the one end that owes
// each component under the name its `ref.name` gives it.
func (f *fixture) targetSide(references ...string) compare.SideSpec {
	return compare.SideSpec{
		Label:                     "lab",
		Repository:                targetBase + "/" + sourcePath,
		References:                references,
		BasePath:                  targetBase,
		PublishesComponentsByName: true,
	}
}

func (f *fixture) compare(a, b compare.SideSpec) compare.Report {
	f.t.Helper()

	report, err := compare.Run(context.Background(),
		f.clientFor(a), f.clientFor(b),
		compare.Options{A: a, B: b, Concurrency: 4})
	if err != nil {
		f.t.Fatal(err)
	}
	return report
}

// clientFor picks the registry a side lives on: the source's own path is on the
// source registry, anything beneath a base path is on the destination.
func (f *fixture) clientFor(spec compare.SideSpec) compare.ClientFactory {
	reg := f.src
	if spec.BasePath != "" {
		reg = f.dst
	}
	return func(repository string) (registry.Repository, error) {
		return generic.New(generic.Config{
			Registry: reg.Host(), Repository: repository, PlainHTTP: true,
		})
	}
}

// ---------------------------------------------------------------------------
// assertions
// ---------------------------------------------------------------------------

// rootRow is the bundle itself, which is the row reference findings land on.
func rootRow(r compare.Report) (compare.Row, bool) {
	for _, row := range r.Rows {
		if row.IsRoot() {
			return row, true
		}
	}
	return compare.Row{}, false
}

func rowFor(r compare.Report, component string) (compare.Row, bool) {
	for _, row := range r.Rows {
		if strings.HasSuffix(row.Name, "/"+component) {
			return row, true
		}
	}
	return compare.Row{}, false
}

func mentions(row compare.Row, want string) bool {
	for _, d := range row.Differences {
		if strings.Contains(d, want) {
			return true
		}
	}
	return false
}

// describe renders a report for a failure message, so a broken assertion says
// what the comparison actually found.
func describe(r compare.Report) string {
	var b strings.Builder
	for _, row := range r.Rows {
		b.WriteString("  " + string(row.Verdict) + " " + row.Type + " " + row.Name)
		if len(row.Differences) > 0 {
			b.WriteString(" - " + strings.Join(row.Differences, "; "))
		}
		b.WriteString("\n")
	}
	for _, tag := range r.ExtraTagsA {
		b.WriteString("  extra in A: " + tag + "\n")
	}
	for _, tag := range r.ExtraTagsB {
		b.WriteString("  extra in B: " + tag + "\n")
	}
	return b.String()
}

// archiveOf packs files into a tar, optionally gzipped - the format the OCI
// image specification names for a layer.
func archiveOf(files map[string]string, compress bool) string {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	for _, path := range paths {
		content := files[path]
		if err := tw.WriteHeader(&tar.Header{
			Name: path, Size: int64(len(content)), Mode: 0o644, Format: tar.FormatGNU,
		}); err != nil {
			panic(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			panic(err)
		}
	}
	if err := tw.Close(); err != nil {
		panic(err)
	}
	if !compress {
		return buf.String()
	}

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(buf.Bytes()); err != nil {
		panic(err)
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return gz.String()
}

func splitRefName(ref string) (repository, tag string) {
	i := strings.LastIndex(ref, ":")
	if i < 0 || i < strings.LastIndex(ref, "/") {
		return ref, ""
	}
	return ref[:i], ref[i+1:]
}

// A vendor's named layers are named files, and comparing them is free.
//
// `org.opencontainers.image.title` is the vendor saying "this layer is this
// file" - every configuration bundle and release note in a vendor artifact is
// one of these, and the name and the content digest are both in the manifest
// the walk has already read.
//
// So the comparison downloads nothing and the fixture serves no blob for it.
// This is the whole of the file-level answer.
func TestNamedLayersAreComparedFromTheManifestAlone(t *testing.T) {
	f := newFixture(t)

	f.publish(f.src, sourcePath, older, []component{
		{name: "custo", tag: "23.7.900", files: map[string]string{
			"CONFIGURATION/one.json":  `{"replicas":1}`,
			"CONFIGURATION/gone.json": `{}`,
		}},
	})
	f.publish(f.src, sourcePath, release, []component{
		{name: "custo", tag: "23.8.1076", files: map[string]string{
			"CONFIGURATION/one.json": `{"replicas":3}`,
			"CONFIGURATION/new.json": `{}`,
		}},
	})

	report, err := compare.Run(context.Background(),
		f.clientFor(f.sourceSide(older)), f.clientFor(f.sourceSide(release)),
		compare.Options{
			A: f.sourceSide(older), B: f.sourceSide(release), Concurrency: 4,
		})
	if err != nil {
		t.Fatal(err)
	}

	row, ok := rowFor(report, "custo")
	if !ok {
		t.Fatalf("no row for the artifact:\n%s", describe(report))
	}
	want := map[string]compare.Verdict{
		"CONFIGURATION/one.json":  compare.VerdictChanged,
		"CONFIGURATION/new.json":  compare.VerdictOnlyB,
		"CONFIGURATION/gone.json": compare.VerdictOnlyA,
	}
	if len(row.Files) != len(want) {
		t.Fatalf("files = %v, want exactly %d", row.Files, len(want))
	}
	for _, f := range row.Files {
		if got := want[f.Path]; got != f.Verdict {
			t.Errorf("%s has verdict %q, want %q", f.Path, f.Verdict, got)
		}
		if f.Verdict != compare.VerdictOnlyB && f.DigestA == "" {
			t.Errorf("%s carries no digest for the first side", f.Path)
		}
		if f.Verdict != compare.VerdictOnlyA && f.DigestB == "" {
			t.Errorf("%s carries no digest for the second side", f.Path)
		}
	}
}

// Progress never goes backwards.
//
// A comparison is four kinds of work over four different populations -
// manifests walked, names probed, tags resolved, layers read - and reporting
// each phase's own count meant the number reset to zero at every boundary. On
// screen that is a bar filling, emptying and filling again, which reads as the
// thing having restarted; the report was "it went to 251, the bar went to the
// end, and then it went to zero again".
//
// So the unit is round trips completed, and the assertion is the property that
// makes it legible rather than any particular number.
func TestProgressOnlyEverGoesUp(t *testing.T) {
	f := newFixture(t)
	f.publish(f.src, sourcePath, older, componentsOf(older))
	f.publish(f.src, sourcePath, release, componentsOf(release))

	var (
		mu   sync.Mutex
		last = map[string]compare.Progress{}
		seen int
	)

	if _, err := compare.Run(context.Background(),
		f.clientFor(f.sourceSide(older)), f.clientFor(f.sourceSide(release)),
		compare.Options{
			A: f.sourceSide(older), B: f.sourceSide(release), Concurrency: 4,
			Progress: func(p compare.Progress) {
				mu.Lock()
				defer mu.Unlock()
				seen++
				if before, ok := last[p.Key]; ok {
					if p.Done < before.Done {
						t.Errorf("%s went backwards: %d done, was %d (phase %q, was %q)",
							p.Key, p.Done, before.Done, p.Phase, before.Phase)
					}
					if p.Total < before.Total {
						t.Errorf("%s's total shrank: %d, was %d - a denominator that "+
							"drops makes the bar jump forwards for no reason",
							p.Key, p.Total, before.Total)
					}
				}
				last[p.Key] = p
			},
		}); err != nil {
		t.Fatal(err)
	}

	if seen == 0 {
		t.Fatal("no progress was reported at all")
	}
	if len(last) != 2 {
		t.Errorf("progress came back for %d ends, want both - the two sides of a "+
			"version comparison share a label, so anything keyed by it loses one",
			len(last))
	}
	for side, p := range last {
		if p.Estimated {
			t.Errorf("%s still reads as an estimate after the comparison finished", side)
		}
		if p.Done != p.Total {
			t.Errorf("%s finished at %d of %d, want the two equal once it is done",
				side, p.Done, p.Total)
		}
	}
}

// Unaccounted tags are a question about a DESTINATION.
//
// The pass resolves every tag in the bundle's repository looking for content
// this release does not account for. At a target that is a real finding -
// something landed there that nobody in this comparison put there. At a source
// it is the vendor's own catalogue, every other release it has ever published,
// reported as unaccounted content on every version-to-version comparison
// anybody runs.
//
// It was also the most expensive thing a comparison did: one resolve per tag in
// a repository holding years of releases, on both sides, which is why a
// comparison that had finished walking appeared to stop for minutes.
func TestASourceIsNotAskedAboutUnaccountedTags(t *testing.T) {
	f := newFixture(t)
	f.publish(f.src, sourcePath, older, componentsOf(older))
	f.publish(f.src, sourcePath, release, componentsOf(release))

	report, err := compare.Run(context.Background(),
		f.clientFor(f.sourceSide(older)), f.clientFor(f.sourceSide(release)),
		compare.Options{A: f.sourceSide(older), B: f.sourceSide(release), Concurrency: 4})
	if err != nil {
		t.Fatal(err)
	}

	// The other version's tags are in this repository and are not a finding.
	if len(report.ExtraTagsA) != 0 || len(report.ExtraTagsB) != 0 {
		t.Errorf("a version comparison reported the vendor's other releases as "+
			"unaccounted content: %v %v", report.ExtraTagsA, report.ExtraTagsB)
	}
}
