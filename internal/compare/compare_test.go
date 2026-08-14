package compare_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/compare"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/generic"
	"github.com/abhijeet-oxide/softwareGateway/test/fakeregistry"
)

// Five questions, one mechanism.
//
// The tests are grouped by the question an operator is asking, because that is
// what has to keep working — not by which function is being called. Every one of
// them is `walk two bundles and align their components`, and if any of them
// needed a special case in the engine, the engine would be wrong.

const (
	sourcePath = "orbs/cfx-5000-k8s"
	targetBase = "nokia-lab"
	release    = "orb_23.8.1076"
	older      = "orb_23.7.900"
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
// nothing content-addressed can catch it — the release is simply not pullable.
func TestAComponentThatIsNotPullableUnderItsOwnNameIsReported(t *testing.T) {
	f := newFixture(t)
	f.publish(f.src, sourcePath, release, componentsOf(release))
	f.copyToTarget(release)
	f.dst.RemoveTag(targetBase+"/"+sourcePath+"/nginx", "1.2.3")

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
	f.dst.RemoveManifest(targetBase+"/"+sourcePath+"/nginx", f.digestOf("nginx"))

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

// THE CASE THIS WAS BUILT FOR: a component whose layer is an ARCHIVE, with the
// files inside it.
//
// "Two layers changed" is true and useless — a release that edited one line and
// a release that rewrote everything produce the same sentence, because a layer's
// digest changes when anything inside it does. So the layer is opened and the
// answer is given in files.
//
// This is OCI rather than one vendor's convention: the image specification
// defines a layer as a tar archive and the media types say so out loud.
func TestFilesInsideAnArchivedLayerAreCompared(t *testing.T) {
	f := newFixture(t)

	f.publish(f.src, sourcePath, older, []component{
		{name: "custo", tag: "23.7.900", archive: map[string]string{
			"DOCUMENTATION/readme":    "how to deploy",
			"CONFIGURATION/one.json":  `{"replicas":1}`,
			"CONFIGURATION/gone.json": "removed next release",
		}},
	})
	f.publish(f.src, sourcePath, release, []component{
		{name: "custo", tag: "23.8.1076", archive: map[string]string{
			"DOCUMENTATION/readme":   "how to deploy",
			"CONFIGURATION/one.json": `{"replicas":3}`,
			"CONFIGURATION/new.json": "added this release",
		}},
	})

	report := f.compare(f.sourceSide(older), f.sourceSide(release))

	row, ok := rowFor(report, "custo")
	if !ok {
		t.Fatalf("no row for the archived artifact:\n%s", describe(report))
	}
	assertFileChange(t, row, fileChange{
		changed: []string{"CONFIGURATION/one.json"},
		added:   []string{"CONFIGURATION/new.json"},
		removed: []string{"CONFIGURATION/gone.json"},
		absent:  []string{"DOCUMENTATION/readme"},
	})
	// And the LAYER digest must not leak into the answer. The whole point is to
	// stop saying "one layer changed" where a file changed.
	if strings.Contains(strings.Join(row.FilesChanged, " "), "sha256:") {
		t.Errorf("a layer digest is reported where a file path belongs: %v",
			row.FilesChanged)
	}
}

// A gzipped tar is the common case — `+gzip` is in the media type — and must be
// transparent.
func TestAGzippedArchiveIsOpenedToo(t *testing.T) {
	f := newFixture(t)

	f.publish(f.src, sourcePath, older, []component{
		{name: "custo", tag: "23.7.900", gzip: true,
			archive: map[string]string{"CONFIGURATION/one.json": `{"replicas":1}`}},
	})
	f.publish(f.src, sourcePath, release, []component{
		{name: "custo", tag: "23.8.1076", gzip: true,
			archive: map[string]string{"CONFIGURATION/one.json": `{"replicas":3}`}},
	})

	report := f.compare(f.sourceSide(older), f.sourceSide(release))

	row, _ := rowFor(report, "custo")
	assertFileChange(t, row, fileChange{changed: []string{"CONFIGURATION/one.json"}})
}

// WITHOUT A BUDGET nothing is opened, and the answer degrades to the layer —
// which is the behaviour that existed before, and is what stops a comparison
// downloading a two-gigabyte image layer.
func TestWithoutABudgetLayersStayOpaque(t *testing.T) {
	f := newFixture(t)

	f.publish(f.src, sourcePath, older, []component{
		{name: "custo", tag: "23.7.900",
			archive: map[string]string{"CONFIGURATION/one.json": `{"replicas":1}`}},
	})
	f.publish(f.src, sourcePath, release, []component{
		{name: "custo", tag: "23.8.1076",
			archive: map[string]string{"CONFIGURATION/one.json": `{"replicas":3}`}},
	})

	report, err := compare.Run(context.Background(),
		f.clientFor(f.sourceSide(older)), f.clientFor(f.sourceSide(release)),
		compare.Options{
			A: f.sourceSide(older), B: f.sourceSide(release), Concurrency: 4,
			// No budget.
		})
	if err != nil {
		t.Fatal(err)
	}

	row, ok := rowFor(report, "custo")
	if !ok {
		t.Fatalf("no row for the artifact:\n%s", describe(report))
	}
	if len(row.FilesChanged)+len(row.FilesAdded)+len(row.FilesRemoved) != 0 {
		t.Errorf("layers were opened with no budget: %v %v %v",
			row.FilesChanged, row.FilesAdded, row.FilesRemoved)
	}
	// The component is still reported as changed: the digests differ, and that
	// is knowable without opening anything.
	if row.Verdict != compare.VerdictChanged {
		t.Errorf("verdict = %q, want changed", row.Verdict)
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

	for _, path := range want.changed {
		if !contains(row.FilesChanged, path) {
			t.Errorf("%s is not reported as changed: changed=%v added=%v removed=%v",
				path, row.FilesChanged, row.FilesAdded, row.FilesRemoved)
		}
	}
	for _, path := range want.added {
		if !contains(row.FilesAdded, path) {
			t.Errorf("%s is not reported as added: %v", path, row.FilesAdded)
		}
	}
	for _, path := range want.removed {
		if !contains(row.FilesRemoved, path) {
			t.Errorf("%s is not reported as removed: %v", path, row.FilesRemoved)
		}
	}
	for _, path := range want.absent {
		all := append(append(append([]string(nil), row.FilesChanged...),
			row.FilesAdded...), row.FilesRemoved...)
		if contains(all, path) {
			t.Errorf("the untouched %s is reported as a change: %v", path, all)
		}
	}
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
	f.dst.RemoveTag(prodBase+"/"+sourcePath+"/nginx", "1.2.3")
	report := f.compare(lab, prod)
	if report.Differences() == 0 {
		t.Fatal("a destination missing a tag matched one that has it")
	}
}

// ---------------------------------------------------------------------------
// Is there anything there nobody put?
// ---------------------------------------------------------------------------

// A NEAR orb gets a repository to itself, so a tag in it that this release does
// not account for is genuinely unexplained — an old version left behind, or
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

// ---------------------------------------------------------------------------
// Was anything mutated?
// ---------------------------------------------------------------------------

// A tag pointing at content that is not what the source published. Every digest
// in the bundle is present, so only asking what the tag RESOLVES to catches it.
func TestATagPointingAtSomethingElseIsReported(t *testing.T) {
	f := newFixture(t)
	f.publish(f.src, sourcePath, release, componentsOf(release))
	f.copyToTarget(release)

	f.dst.AddImage(targetBase+"/"+sourcePath+"/nginx", "1.2.3",
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
	f.dst.RemoveTag(targetBase+"/"+sourcePath+"/nginx", "1.2.3")

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
	// files, when set, makes this an artifact with ONE TITLED LAYER PER FILE —
	// the ORAS convention, used by Helm and cosign and everything else that
	// publishes non-image artifacts.
	files map[string]string
	// archive, when set, makes this an artifact with ONE LAYER CONTAINING ALL
	// THE FILES — a tar, which is what the OCI image specification says a layer
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
				registry.AnnotationRefName: repo + "/" + c.name + ":" + c.tag,
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

	// Inside the bundle, untagged — the index names it by digest — and under
	// its own name with its own tag, which is where a consumer pulls it.
	digest := reg.AddImage(repo, "", layers...)
	reg.AddManifest(repo+"/"+c.name, c.tag, reg.Manifest(repo, digest),
		registry.MediaTypeOCIManifest)
	return digest
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
	raw := f.src.Manifest(sourcePath, root)

	var index struct {
		Manifests []struct {
			Digest      string            `json:"digest"`
			Annotations map[string]string `json:"annotations"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(raw, &index); err != nil {
		f.t.Fatal(err)
	}

	dstRepo := base + "/" + sourcePath
	for _, child := range index.Manifests {
		body := f.src.Manifest(sourcePath, child.Digest)
		reg.AddManifest(dstRepo, "", body, registry.MediaTypeOCIManifest)

		ref := child.Annotations[registry.AnnotationRefName]
		repo, refTag := splitRefName(ref)
		if repo == "" {
			continue
		}
		reg.AddManifest(base+"/"+repo, refTag, body, registry.MediaTypeOCIManifest)
	}
	reg.AddManifest(dstRepo, tag, raw, registry.MediaTypeOCIIndex)
}

func (f *fixture) digestOf(component string) string {
	f.t.Helper()
	digest, ok := f.digests[component]
	if !ok {
		f.t.Fatalf("the fixture published no component %q", component)
	}
	return digest
}

func (f *fixture) sourceSide(tag string) compare.SideSpec {
	return compare.SideSpec{
		Label: "near", Repository: sourcePath, References: []string{tag},
	}
}

func (f *fixture) targetSide(tag string) compare.SideSpec {
	return compare.SideSpec{
		Label:      "lab",
		Repository: targetBase + "/" + sourcePath,
		References: []string{tag},
		BasePath:   targetBase,
	}
}

func (f *fixture) compare(a, b compare.SideSpec) compare.Report {
	f.t.Helper()

	report, err := compare.Run(context.Background(),
		f.clientFor(a), f.clientFor(b),
		compare.Options{A: a, B: b, Concurrency: 4, FileBudget: 8 << 20})
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
			b.WriteString(" — " + strings.Join(row.Differences, "; "))
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

// archiveOf packs files into a tar, optionally gzipped — the format the OCI
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
