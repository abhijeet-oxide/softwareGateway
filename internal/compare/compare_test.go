package compare_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/compare"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/generic"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/test/fakeregistry"
)

// What a transfer's own report cannot tell you.
//
// A transfer says 2489 jobs succeeded and 63.7 GiB moved, and every one of
// those numbers can be true while the destination is wrong. These are the four
// ways it was wrong in practice: a tag that never applied, a tag pointing at
// the wrong content, content that is simply not there, and — the case that has
// to keep working — a destination that is genuinely correct.

const (
	sourcePath = "orbs/cfx-5000-k8s"
	targetBase = "nokia-lab"
)

// A faithful copy reports no differences at all. Everything below depends on
// this being the baseline rather than on luck.
func TestAFaithfulCopyMatchesEverywhere(t *testing.T) {
	f := newFixture(t)
	f.copyEverything()

	report := f.compare()

	if report.Mismatched != 0 {
		t.Fatalf("%d of %d rows disagreed about a faithful copy:\n%s",
			report.Mismatched, len(report.Rows), describe(report))
	}
	if report.Matched == 0 {
		t.Fatal("nothing was compared; the fixture is not producing rows")
	}
}

// The failure this system actually hit: every blob and every manifest pushed,
// and the tag never applied. Content-addressed checks all pass; the release is
// invisible to anybody who does not know its digest.
func TestATagThatNeverAppliedIsReported(t *testing.T) {
	f := newFixture(t)
	f.copyEverything()
	f.dst.RemoveTag(targetBase+"/"+sourcePath+"/nginx", "1.2.3")

	report := f.compare()

	row, ok := rowFor(report, "nginx")
	if !ok {
		t.Fatalf("no row for the component:\n%s", describe(report))
	}
	if row.Match() {
		t.Fatal("a component whose tag never applied was reported as matching")
	}
	if !mentions(row, "1.2.3") {
		t.Errorf("the difference does not name the missing tag: %v", row.Differences)
	}
	// And the content is still there, so the row must not claim it is missing.
	if !row.Target.Present {
		t.Error("the row says the content is absent when only the tag is")
	}
}

// A tag pointing at the WRONG content. Every digest in the bundle is present,
// so nothing content-addressed can catch it — only asking what the tag resolves
// to does.
func TestATagPointingAtTheWrongContentIsReported(t *testing.T) {
	f := newFixture(t)
	f.copyEverything()

	// Something else entirely, published under the component's tag.
	other := f.dst.AddImage(targetBase+"/"+sourcePath+"/nginx", "1.2.3",
		fakeregistry.NewLayer("a completely different image"))

	report := f.compare()

	row, ok := rowFor(report, "nginx")
	if !ok {
		t.Fatalf("no row for the component:\n%s", describe(report))
	}
	if row.Match() {
		t.Fatalf("a tag pointing at %s rather than the source's digest was "+
			"reported as matching", other)
	}
	if !mentions(row, "points at") {
		t.Errorf("the difference does not say the tag resolves elsewhere: %v", row.Differences)
	}
}

// Content that is not at the destination at all.
func TestContentMissingFromTheDestinationIsReported(t *testing.T) {
	f := newFixture(t)
	f.copyEverything()
	f.dst.RemoveManifest(targetBase+"/"+sourcePath+"/nginx", f.nginx)

	report := f.compare()

	row, ok := rowFor(report, "nginx")
	if !ok {
		t.Fatalf("no row for the component:\n%s", describe(report))
	}
	if row.Match() {
		t.Fatal("a component absent from the destination was reported as matching")
	}
	if row.Target.Present {
		t.Error("the row says the content is present at a destination that does not have it")
	}
}

// Nothing copied at all — the shape of a transfer that never ran, or ran
// against the wrong target.
func TestADestinationWithNothingReportsEveryRow(t *testing.T) {
	f := newFixture(t)

	report := f.compare()

	if report.Matched != 0 {
		t.Errorf("%d rows matched against an empty destination", report.Matched)
	}
	if report.Mismatched != len(report.Rows) || len(report.Rows) == 0 {
		t.Errorf("%d of %d rows disagreed, want all of them",
			report.Mismatched, len(report.Rows))
	}
}

// Mismatches come first. Somebody runs this to find what is wrong, and making
// them scroll past two thousand agreeing rows to reach three disagreeing ones
// defeats the command.
func TestDisagreementsSortToTheTop(t *testing.T) {
	f := newFixture(t)
	f.copyEverything()
	f.dst.RemoveTag(targetBase+"/"+sourcePath+"/nginx", "1.2.3")

	report := f.compare()

	if len(report.Rows) < 2 {
		t.Fatalf("only %d rows; the ordering is not exercised", len(report.Rows))
	}
	if report.Rows[0].Match() {
		t.Errorf("the first row matches while %d rows do not:\n%s",
			report.Mismatched, describe(report))
	}
	// And every disagreement is above every match, not merely the first.
	seenMatch := false
	for _, row := range report.Rows {
		if row.Match() {
			seenMatch = true
			continue
		}
		if seenMatch {
			t.Errorf("a disagreement sorts below a match:\n%s", describe(report))
			break
		}
	}
}

// ---------------------------------------------------------------------------
// fixture
// ---------------------------------------------------------------------------

// fixture publishes a bundle at a source and gives the comparison a real
// destination registry to interrogate.
type fixture struct {
	t        *testing.T
	src, dst *fakeregistry.Registry
	tree     store.ExpandedTree
	nginx    string
	root     string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	src := fakeregistry.New()
	t.Cleanup(src.Close)
	dst := fakeregistry.New()
	t.Cleanup(dst.Close)

	f := &fixture{t: t, src: src, dst: dst}
	f.seed()
	return f
}

// seed publishes a bundle shaped like the real thing: an index naming its
// children with the reserved OCI annotation, so the layout puts each component
// both inside the bundle and under its own name.
func (f *fixture) seed() {
	f.t.Helper()

	f.nginx = f.src.AddImage(sourcePath, "", fakeregistry.NewLayer("nginx payload"))
	chartLayer := fakeregistry.NewLayer("chart tarball")
	chartLayer.MediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
	chart := f.src.AddImage(sourcePath, "", chartLayer)

	children := []map[string]any{
		{
			"mediaType": registry.MediaTypeOCIManifest, "digest": f.nginx, "size": 100,
			"annotations": map[string]string{
				registry.AnnotationRefName: sourcePath + "/nginx:1.2.3",
			},
		},
		{
			"mediaType": registry.MediaTypeOCIManifest, "digest": chart, "size": 100,
			"annotations": map[string]string{
				registry.AnnotationRefName: sourcePath + "/charts/cfx:23.8.1076",
			},
		},
	}
	raw, err := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": registry.MediaTypeOCIIndex, "manifests": children,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.root = f.src.AddManifest(sourcePath, "orb_23.8.1076", raw, registry.MediaTypeOCIIndex)

	f.tree = store.ExpandedTree{Artifacts: []store.ExpandedArtifact{
		{Row: store.ArtifactRow{
			Digest: f.root, MediaType: registry.MediaTypeOCIIndex,
			SizeBytes: int64(len(raw)), Depth: 0,
		}, Parent: -1},
		{Row: store.ArtifactRow{
			Digest: f.nginx, MediaType: registry.MediaTypeOCIManifest, Depth: 1,
			Annotations: map[string]string{
				registry.AnnotationRefName: sourcePath + "/nginx:1.2.3",
			},
		}, Parent: 0},
		{Row: store.ArtifactRow{
			Digest: chart, MediaType: registry.MediaTypeOCIManifest, Depth: 1,
			ArtifactType: "application/vnd.cncf.helm.chart.v1",
			Annotations: map[string]string{
				registry.AnnotationRefName: sourcePath + "/charts/cfx:23.8.1076",
			},
		}, Parent: 0},
	}}
}

// copyEverything reproduces at the destination exactly what a correct transfer
// would leave there — every artifact in every place the layout names, tagged as
// the source says.
//
// Built by asking the comparison itself where things go, so the fixture cannot
// drift away from the layout it is meant to satisfy.
func (f *fixture) copyEverything() {
	f.t.Helper()

	for _, row := range f.compare().Rows {
		raw := f.src.Manifest(sourcePath, row.Source.Digest)
		if raw == nil {
			f.t.Fatalf("the source has no manifest %s", row.Source.Digest)
		}
		mediaType := registry.MediaTypeOCIManifest
		if row.Type == "index" {
			mediaType = registry.MediaTypeOCIIndex
		}

		f.dst.AddManifest(row.Target.Repository, "", raw, mediaType)
		for _, tag := range row.Source.Tags {
			f.dst.AddManifest(row.Target.Repository, tag, raw, mediaType)
		}
	}
}

func (f *fixture) compare() compare.Report {
	f.t.Helper()

	report, err := compare.Run(context.Background(), f.targetClient, compare.Options{
		Tree:             f.tree,
		SourceRepository: sourcePath,
		TargetBasePath:   targetBase,
		RootTags:         []string{"orb_23.8.1076"},
		Concurrency:      4,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return report
}

func (f *fixture) targetClient(repository string) (registry.Repository, error) {
	return generic.New(generic.Config{
		Registry: f.dst.Host(), Repository: repository, PlainHTTP: true,
	})
}

// ---------------------------------------------------------------------------
// assertions
// ---------------------------------------------------------------------------

// rowFor finds the row for a component under its OWN name — the site a person
// would pull from, rather than the untagged copy inside the bundle.
func rowFor(r compare.Report, component string) (compare.Row, bool) {
	for _, row := range r.Rows {
		if strings.Contains(row.Name, component) && len(row.Source.Tags) > 0 {
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
		b.WriteString("  " + row.Type + " " + row.Name)
		if len(row.Differences) > 0 {
			b.WriteString(" — " + strings.Join(row.Differences, "; "))
		}
		b.WriteString("\n")
	}
	return b.String()
}
