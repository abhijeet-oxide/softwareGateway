// Package source acquires the chart bytes a compliance run judges.
//
// # Why the Coordinator may read these bytes at all
//
// The founding invariant is that artifact bytes never traverse the
// Coordinator: a release is 30 to 60 GB and the whole transfer plane exists so
// that those bytes go worker-to-registry. This package is a deliberate,
// bounded exception, and the bound is what makes it one.
//
//	what it reads     Helm chart layers, and nothing else
//	how it chooses    store.ChartArtifacts, which cannot return an image layer
//	how much          a per-chart and a per-release byte budget, enforced before
//	                  the fetch and again while streaming
//
// A chart is YAML: hundreds of kilobytes, occasionally a few megabytes. Ninety
// seven of them is still smaller than one container image layer. The invariant
// is about not moving a release through a control plane, and reading the
// manifests is not moving the release.
//
// # Why it is unpacked rather than rendered from memory
//
// `helm template` takes a directory. Reimplementing chart loading to avoid a
// temporary directory would mean reimplementing subchart resolution, .helmignore
// and the file ordering helm applies - all so a run could avoid writing files
// it deletes moments later.
package source

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// BlobReader opens one blob of a package from the registry that published it.
//
// The interface the Coordinator already implements for the file-content
// endpoint, reused unchanged. It reaches a vendor registry with credentials,
// which is why it lives in the composition root and why this package takes it
// as a dependency rather than building one.
type BlobReader interface {
	ReadBlob(ctx context.Context, productName string, pkg store.PackageRow, digest string) (io.ReadCloser, error)
}

// Budgets bound what one run may read.
//
// # Why two numbers and not one
//
// A single 500 MB "chart" is a different problem from four hundred ordinary
// charts, and they need different answers. The per-chart limit refuses the
// mislabelled artifact; the per-release limit refuses the release that would
// take an hour to unpack. Without the first, one bad artifact consumes the
// whole budget and every chart after it is skipped for a reason that has
// nothing to do with it.
type Budgets struct {
	PerChart   int64
	PerRelease int64
	MaxCharts  int
	// MaxFiles bounds entries per archive, because a tar with a million empty
	// files is small on the wire and expensive on disk.
	MaxFiles int
}

// Defaults sized from what charts actually weigh: a large one is a few
// megabytes, and a release of a hundred is comfortably inside the total.
func (b Budgets) WithDefaults() Budgets {
	if b.PerChart == 0 {
		b.PerChart = 64 << 20
	}
	if b.PerRelease == 0 {
		b.PerRelease = 512 << 20
	}
	if b.MaxCharts == 0 {
		b.MaxCharts = 500
	}
	if b.MaxFiles == 0 {
		b.MaxFiles = 20_000
	}
	return b
}

// Fetcher unpacks a release's charts into a working directory.
type Fetcher struct {
	Blobs   BlobReader
	Budgets Budgets
}

// Chart is one unpacked chart, ready to render.
type Chart struct {
	// Dir is the chart directory - the one containing Chart.yaml.
	Dir string
	// Digest and Ref identify the artifact it came from, so a finding can name
	// what a vendor should pull to reproduce it.
	Digest string
	Ref    string
	// Err is why this chart is not usable, when it is not. A chart that could
	// not be fetched is recorded rather than dropped: dropping it would shrink
	// the denominator silently, and a check that never ran looks like a check
	// that passed.
	Err error
}

// Result is what one release's acquisition produced.
type Result struct {
	Root   string
	Charts []Chart
	// Skipped names charts that were not fetched, and why - a budget, a
	// truncation, a registry error. Reported on the run, never swallowed.
	Skipped []string
	Bytes   int64
}

// Fetch downloads and unpacks every chart of a package.
//
// The caller removes Root when the run is over. A failure to fetch one chart is
// recorded on that chart and does not stop the others: one unreadable artifact
// in a ninety-seven chart release must not lose the other ninety-six.
func (f Fetcher) Fetch(ctx context.Context, productName string, pkg store.PackageRow, charts []store.ChartArtifact) (*Result, error) {
	budgets := f.Budgets.WithDefaults()

	root, err := os.MkdirTemp("", "sgw-compliance-")
	if err != nil {
		return nil, fmt.Errorf("creating a working directory: %w", err)
	}
	res := &Result{Root: root}

	for i, ca := range charts {
		if err := ctx.Err(); err != nil {
			res.Skipped = append(res.Skipped, fmt.Sprintf("%d chart(s) not fetched: %v", len(charts)-i, err))
			return res, nil
		}
		if i >= budgets.MaxCharts {
			res.Skipped = append(res.Skipped, fmt.Sprintf(
				"%d chart(s) beyond the limit of %d were not fetched", len(charts)-i, budgets.MaxCharts))
			break
		}
		if ca.LayerSize > budgets.PerChart {
			res.Skipped = append(res.Skipped, fmt.Sprintf(
				"%s is %d bytes, over the per-chart limit of %d", displayOf(ca), ca.LayerSize, budgets.PerChart))
			continue
		}
		if res.Bytes+ca.LayerSize > budgets.PerRelease {
			res.Skipped = append(res.Skipped, fmt.Sprintf(
				"%d chart(s) not fetched: the release byte budget of %d was reached",
				len(charts)-i, budgets.PerRelease))
			break
		}

		dir, n, err := f.fetchOne(ctx, productName, pkg, ca, root, i, budgets)
		res.Bytes += n
		res.Charts = append(res.Charts, Chart{Dir: dir, Digest: ca.Digest, Ref: ca.Ref, Err: err})
	}
	return res, nil
}

func (f Fetcher) fetchOne(
	ctx context.Context, productName string, pkg store.PackageRow,
	ca store.ChartArtifact, root string, index int, budgets Budgets,
) (string, int64, error) {
	rc, err := f.Blobs.ReadBlob(ctx, productName, pkg, ca.LayerDigest)
	if err != nil {
		return "", 0, fmt.Errorf("fetching %s: %w", displayOf(ca), err)
	}
	defer func() { _ = rc.Close() }()

	// A directory per artifact, named by position rather than by the chart's
	// own name: two charts in one release may legitimately share a name at
	// different versions, and one overwriting the other would make a finding
	// point at the wrong chart.
	dest := filepath.Join(root, fmt.Sprintf("%03d", index))
	if err := os.MkdirAll(dest, 0o750); err != nil {
		return "", 0, err
	}

	n, err := unpack(io.LimitReader(rc, budgets.PerChart+1), dest, budgets)
	if err != nil {
		return "", n, fmt.Errorf("unpacking %s: %w", displayOf(ca), err)
	}
	if n > budgets.PerChart {
		return "", n, fmt.Errorf("%s expands past the per-chart limit of %d bytes", displayOf(ca), budgets.PerChart)
	}

	chartDir, err := findChartRoot(dest)
	if err != nil {
		return "", n, fmt.Errorf("%s: %w", displayOf(ca), err)
	}
	return chartDir, n, nil
}

// ErrNoChartYaml is what a tarball that is not a chart produces.
var ErrNoChartYaml = errors.New("the archive holds no Chart.yaml, so it is not a Helm chart")

// findChartRoot locates the directory holding Chart.yaml.
//
// A packaged chart is a tarball with one top-level directory, but a chart built
// by hand or repackaged by a vendor's pipeline may have Chart.yaml at the root.
// Both shapes are common enough that guessing one would fail on real
// deliveries.
func findChartRoot(dir string) (string, error) {
	if _, err := os.Stat(filepath.Join(dir, "Chart.yaml")); err == nil {
		return dir, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		candidate := filepath.Join(dir, e.Name())
		if _, err := os.Stat(filepath.Join(candidate, "Chart.yaml")); err == nil {
			return candidate, nil
		}
	}
	return "", ErrNoChartYaml
}

// unpack extracts a gzipped tar into dir, refusing everything a chart has no
// business containing.
//
// # What is refused and why
//
// A tar entry names its own path, and the archive comes from a vendor. An entry
// called `../../etc/cron.d/x` would write outside the working directory; a
// symlink pointing at `/etc/passwd` would make a later read follow it; a device
// node or a setuid bit is meaningless in a chart and dangerous anywhere. None
// of these is hypothetical - they are the standard tar extraction bugs, and the
// only safe treatment is to refuse rather than sanitize, because a sanitized
// path is still an entry somebody wrote deliberately.
func unpack(r io.Reader, dir string, budgets Budgets) (int64, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, fmt.Errorf("not a gzip archive: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	var written int64
	files := 0

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return written, nil
		}
		if err != nil {
			return written, fmt.Errorf("reading the archive: %w", err)
		}
		files++
		if files > budgets.MaxFiles {
			return written, fmt.Errorf("the archive holds more than %d entries", budgets.MaxFiles)
		}

		target, err := safeJoin(dir, hdr.Name)
		if err != nil {
			return written, err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				return written, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return written, err
			}
			n, err := writeFile(target, tr, budgets.PerChart-written)
			written += n
			if err != nil {
				return written, err
			}
		case tar.TypeSymlink, tar.TypeLink:
			// A chart does not need links, and a link is how an extraction is
			// turned into a read of something else on the machine.
			return written, fmt.Errorf("the archive contains a link (%s), which a chart has no use for", hdr.Name)
		default:
			// Devices, FIFOs, and anything else. Skipped rather than fatal: a
			// tar written by an unusual tool may carry a pax header, and
			// failing on it would reject a chart that is otherwise fine.
			continue
		}
	}
}

// safeJoin resolves an archive entry to a path inside dir, or refuses.
func safeJoin(dir, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("the archive contains an entry with no name")
	}
	if filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("the archive contains an absolute path (%s)", name)
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	target := filepath.Join(dir, clean)
	// Compare against dir with a separator appended, so a sibling directory
	// whose name merely starts with dir's is not accepted.
	if !strings.HasPrefix(target, filepath.Clean(dir)+string(os.PathSeparator)) {
		return "", fmt.Errorf("the archive contains a path that escapes the chart directory (%s)", name)
	}
	return target, nil
}

func writeFile(path string, r io.Reader, remaining int64) (int64, error) {
	if remaining <= 0 {
		return 0, fmt.Errorf("the archive expands past the per-chart byte limit")
	}
	// 0o600 and no executable bit, ever. A chart is data; nothing here should
	// be runnable, and helm does not need it to be.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // path validated by safeJoin
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()

	n, err := io.Copy(f, io.LimitReader(r, remaining))
	if err != nil {
		return n, err
	}
	return n, nil
}

func displayOf(ca store.ChartArtifact) string {
	if ca.Ref != "" {
		return ca.Ref
	}
	return compliance.ShortDigest(ca.Digest)
}
