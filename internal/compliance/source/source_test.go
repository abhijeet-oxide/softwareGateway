package source_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
	"github.com/abhijeet-oxide/softwareGateway/internal/compliance/source"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// tarball builds a gzipped tar from a map of path to content.
func tarball(t *testing.T, files map[string]string, extra ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	// Deterministic order so a failure is reproducible.
	for i := 1; i < len(paths); i++ {
		for j := i; j > 0 && paths[j] < paths[j-1]; j-- {
			paths[j], paths[j-1] = paths[j-1], paths[j]
		}
	}
	for _, p := range paths {
		body := files[p]
		if err := tw.WriteHeader(&tar.Header{
			Name: p, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range extra {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// blobs serves prepared tarballs by digest.
type blobs map[string][]byte

func (b blobs) ReadBlob(_ context.Context, _ string, _ store.PackageRow, digest string) (io.ReadCloser, error) {
	body, ok := b[digest]
	if !ok {
		return nil, errors.New("no such blob")
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

func chartOf(t *testing.T, name string) []byte {
	t.Helper()
	return tarball(t, map[string]string{
		name + "/Chart.yaml":                "apiVersion: v2\nname: " + name + "\nversion: 1.0.0\n",
		name + "/values.yaml":               "replicas: 1\n",
		name + "/templates/deployment.yaml": "apiVersion: apps/v1\nkind: Deployment\n",
	})
}

func TestFetchUnpacksCharts(t *testing.T) {
	b := blobs{
		"sha256:aaa": chartOf(t, "alpha"),
		"sha256:bbb": chartOf(t, "beta"),
	}
	f := source.Fetcher{Blobs: b}
	res, err := f.Fetch(context.Background(), "p", store.PackageRow{}, []store.ChartCandidate{
		{Digest: "sha256:1", LayerDigest: "sha256:aaa", LayerSize: int64(len(b["sha256:aaa"])), Ref: "charts/alpha", LayerCount: 1},
		{Digest: "sha256:2", LayerDigest: "sha256:bbb", LayerSize: int64(len(b["sha256:bbb"])), Ref: "charts/beta", LayerCount: 1},
	}, compliance.NopReporter{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(res.Root) }()

	if len(res.Charts) != 2 {
		t.Fatalf("want 2 charts, got %d", len(res.Charts))
	}
	for _, c := range res.Charts {
		if c.Err != nil {
			t.Fatalf("%s: %v", c.Ref, c.Err)
		}
		if _, err := os.Stat(filepath.Join(c.Dir, "Chart.yaml")); err != nil {
			t.Errorf("%s has no Chart.yaml at %s", c.Ref, c.Dir)
		}
	}
	// Two charts sharing a name must not overwrite one another, which is why
	// the directory is named by position rather than by the chart.
	if res.Charts[0].Dir == res.Charts[1].Dir {
		t.Error("two charts unpacked into the same directory")
	}
}

// A chart whose Chart.yaml is at the archive root, which is what a chart built
// by hand or repackaged by a pipeline looks like.
func TestFetchAcceptsAFlatArchive(t *testing.T) {
	body := tarball(t, map[string]string{
		"Chart.yaml":  "apiVersion: v2\nname: flat\nversion: 1.0.0\n",
		"values.yaml": "{}\n",
	})
	f := source.Fetcher{Blobs: blobs{"sha256:flat": body}}
	res, err := f.Fetch(context.Background(), "p", store.PackageRow{},
		[]store.ChartCandidate{{Digest: "sha256:1", LayerDigest: "sha256:flat", LayerCount: 1}}, compliance.NopReporter{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(res.Root) }()
	if res.Charts[0].Err != nil {
		t.Fatalf("a flat chart was rejected: %v", res.Charts[0].Err)
	}
}

// The standard tar extraction bugs. Each of these would write outside the
// working directory or make a later read follow a link somewhere else, and the
// archive comes from a vendor.
func TestUnpackRefusesEscapes(t *testing.T) {
	cases := []struct {
		name    string
		archive []byte
		want    string
	}{
		{
			"traversal",
			tarball(t, map[string]string{"../../etc/cron.d/evil": "x"}),
			"escapes",
		},
		{
			"absolute path",
			tarball(t, map[string]string{"/etc/passwd": "x"}),
			"absolute",
		},
		{
			"symlink",
			tarball(t, map[string]string{"c/Chart.yaml": "name: c\n"},
				&tar.Header{Name: "c/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777}),
			"link",
		},
		{
			"hard link",
			tarball(t, map[string]string{"c/Chart.yaml": "name: c\n"},
				&tar.Header{Name: "c/hard", Typeflag: tar.TypeLink, Linkname: "/etc/passwd", Mode: 0o644}),
			"link",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := source.Fetcher{Blobs: blobs{"sha256:x": c.archive}}
			res, err := f.Fetch(context.Background(), "p", store.PackageRow{},
				[]store.ChartCandidate{{Digest: "sha256:1", LayerDigest: "sha256:x", LayerCount: 1}}, compliance.NopReporter{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = os.RemoveAll(res.Root) }()

			if res.Charts[0].Err == nil {
				t.Fatalf("%s was accepted", c.name)
			}
			if !strings.Contains(res.Charts[0].Err.Error(), c.want) {
				t.Errorf("error %q does not name the problem (%s)", res.Charts[0].Err, c.want)
			}
			// Nothing may exist outside the working directory.
			if _, err := os.Stat("/etc/cron.d/evil"); err == nil {
				t.Fatal("the extraction wrote outside the working directory")
			}
		})
	}
}

// A budget must refuse before the fetch, not after, and must name what was
// skipped: a chart nobody checked is not a chart that passed.
func TestBudgetsSkipAndSaySo(t *testing.T) {
	body := chartOf(t, "big")
	f := source.Fetcher{
		Blobs:   blobs{"sha256:big": body},
		Budgets: source.Budgets{PerChart: 10},
	}
	res, err := f.Fetch(context.Background(), "p", store.PackageRow{},
		[]store.ChartCandidate{{Digest: "sha256:1", LayerDigest: "sha256:big", LayerSize: 1 << 20, Ref: "charts/big", LayerCount: 1}}, compliance.NopReporter{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(res.Root) }()

	if len(res.Charts) != 0 {
		t.Error("an over-budget chart was fetched")
	}
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0], "charts/big") {
		t.Errorf("the skip does not name the chart: %v", res.Skipped)
	}
}

// A tarball that is not a chart is reported as such rather than rendered.
func TestArchiveWithoutChartYamlIsRejected(t *testing.T) {
	body := tarball(t, map[string]string{"README.md": "hello"})
	f := source.Fetcher{Blobs: blobs{"sha256:x": body}}
	res, err := f.Fetch(context.Background(), "p", store.PackageRow{},
		[]store.ChartCandidate{{Digest: "sha256:1", LayerDigest: "sha256:x", LayerCount: 1}}, compliance.NopReporter{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(res.Root) }()
	if !errors.Is(res.Charts[0].Err, source.ErrNoChartYaml) {
		t.Errorf("error = %v, want ErrNoChartYaml", res.Charts[0].Err)
	}
}

// One unreadable artifact in a release must not lose the others.
func TestOneBadChartDoesNotLoseTheRest(t *testing.T) {
	f := source.Fetcher{Blobs: blobs{"sha256:ok": chartOf(t, "ok")}}
	res, err := f.Fetch(context.Background(), "p", store.PackageRow{}, []store.ChartCandidate{
		{Digest: "sha256:1", LayerDigest: "sha256:missing", Ref: "charts/missing", LayerCount: 1},
		{Digest: "sha256:2", LayerDigest: "sha256:ok", Ref: "charts/ok", LayerCount: 1},
	}, compliance.NopReporter{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(res.Root) }()

	if len(res.Charts) != 2 {
		t.Fatalf("want both charts recorded, got %d", len(res.Charts))
	}
	if res.Charts[0].Err == nil {
		t.Error("the missing chart reported no error")
	}
	if res.Charts[1].Err != nil {
		t.Errorf("the good chart was lost: %v", res.Charts[1].Err)
	}
}
