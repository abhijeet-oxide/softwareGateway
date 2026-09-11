// Package chartstage copies the files a Helm chart must carry into the chart
// directory, and tells you when the copy is stale.
//
// # Why this exists at all
//
// `config/` is the one directory an administrator manages, and it is read by
// three runtimes: `task run` reads it in place, `docker-compose.yml`
// bind-mounts it, and a cluster gets it as ConfigMaps. The first two need no
// help. The third does, and for a mechanical reason: Helm packages a chart
// DIRECTORY, and `.Files` cannot reach outside it. `.Files.Glob("../../config/**")`
// returns nothing - not an error, nothing - so a chart that tried it would
// package cleanly, install cleanly, and mount empty ConfigMaps.
//
// The alternatives were worse. A committed second copy under the chart is two
// files to keep in step and a reviewer who cannot see the drift. A symlink is
// not followed by `helm package`. Rendering config into values at package time
// makes the values file the source of truth and the directory a stale echo of
// it.
//
// So the copy is a BUILD STEP with a verifier: `task chart:stage` writes it,
// the staged tree is not committed (see the chart's .gitignore), and
// TestStagedChartIsReproducible re-runs it in a temporary directory and
// compares. A drift is a failed test rather than a deployment that mounted
// yesterday's products.
//
// # Why Go rather than a shell script
//
// `cp -r` does not exist in PowerShell, and Taskfile.yml is asserted in CI to
// run with only go, gofmt, git and task on PATH - see the `portable` job. Go
// is already required to build this repository, so `go run ./deploy/chartstage/cmd/chartstage`
// costs nothing and behaves identically on all three platforms.
package chartstage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// Source is one tree or file copied into the chart, and where it lands.
type Source struct {
	// From is a path relative to the repository root.
	From string
	// To is a path relative to the chart's files/ directory.
	To string
}

// Sources is everything the chart carries that lives elsewhere in the tree.
//
// CONTENT first, then the machinery that reads it. The split is the one
// docs/design/27 draws: config/ changes without a release, deploy/ changes with
// the code - and both have to reach a cluster, so both are staged. What must
// never appear here is a VALUE: config/secrets/local is excluded below and the
// chart has no way to reach it.
var Sources = []Source{
	// The administrator's directory, verbatim. Products, users, roles, Cerbos
	// policies and the secret inventory - the same bytes docker-compose.yml
	// bind-mounts.
	{From: "config", To: "config"},

	// The seeder. A 120 kB script rather than a fourth image: it is the same
	// file `docker compose run --rm zitadel-init` runs, it has no dependencies
	// by design (see its header), and an image would be a fourth artifact to
	// version, push and keep in step for no behaviour that differs.
	{From: "deploy/zitadel/bootstrap.mjs", To: "zitadel/bootstrap.mjs"},

	// The one file the cluster needs and compose does not: it moves the seeder's
	// four shared-volume outputs in and out of a Kubernetes Secret. A Job has no
	// named volume that outlives it, and bootstrap.mjs stays a program that reads
	// and writes files.
	{From: "deploy/zitadel/k8s-state.mjs", To: "zitadel/k8s-state.mjs"},

	// ZITADEL's front door: the core and the sign-in screens on one origin.
	// An Ingress could route the two paths, and it would silently drop the two
	// sub_filter rules that centre the sign-in button and name who to ask on
	// the Account Not Found page. Same file, same nginx, both deployments.
	{From: "deploy/zitadel/nginx.conf", To: "zitadel/nginx.conf"},
	{From: "deploy/zitadel/docker-entrypoint.sh", To: "zitadel/docker-entrypoint.sh"},
	{From: "deploy/zitadel/branding", To: "zitadel/branding"},

	// The PDP's own configuration. `cerbos healthcheck` derives what to probe
	// from it, so it is a file in both deployments rather than flags in one.
	{From: "deploy/cerbos/config.yaml", To: "cerbos/config.yaml"},

	// ZITADEL gets its own database in the same instance, never a shared one:
	// it claims `public` and about 150 tables. One statement, and it is the
	// same statement compose feeds to docker-entrypoint-initdb.d.
	{From: "deploy/postgres/init-zitadel.sql", To: "postgres/init-zitadel.sql"},
}

// ChartFilesDir is where staged files land, relative to the repository root.
const ChartFilesDir = "deploy/charts/software-gateway/files"

// excluded reports whether a path inside a staged tree must not be copied.
//
// Two rules, and the first one is the important one.
func excluded(rel string) bool {
	rel = filepath.ToSlash(rel)

	// SECRET VALUES. config/secrets/local is the projected layout filled in by
	// hand and is not committed; it is also not something a chart may carry.
	// The chart renders the REFERENCES (config/secrets/secrets.yaml) and the
	// operator produces the Secret in the cluster.
	if rel == "secrets/local" || strings.HasPrefix(rel, "secrets/local/") {
		return true
	}

	// Prose. READMEs explain the directory to whoever edits it and are read
	// from the repository, not from a ConfigMap. Leaving them out keeps the
	// mounted directories to exactly what a process reads.
	base := filepath.Base(rel)
	return base == "README.md" || base == ".gitkeep" || base == ".gitignore"
}

// Stage copies every Source into the chart's files/ directory under root,
// removing whatever was there first so a deleted product does not survive.
func Stage(root string) error {
	dest := filepath.Join(root, filepath.FromSlash(ChartFilesDir))
	if err := os.RemoveAll(dest); err != nil {
		return fmt.Errorf("clear %s: %w", dest, err)
	}
	for _, s := range Sources {
		from := filepath.Join(root, filepath.FromSlash(s.From))
		to := filepath.Join(dest, filepath.FromSlash(s.To))
		info, err := os.Stat(from)
		if err != nil {
			return fmt.Errorf("stage %s: %w", s.From, err)
		}
		if info.IsDir() {
			err = copyTree(from, to)
		} else {
			err = copyFile(from, to, info.Mode())
		}
		if err != nil {
			return fmt.Errorf("stage %s: %w", s.From, err)
		}
	}
	return nil
}

func copyTree(from, to string) error {
	return filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(to, 0o755)
		}
		if excluded(rel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		target := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return copyFile(path, target, info.Mode())
	})
}

func copyFile(from, to string, mode fs.FileMode) error {
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	// The mode is narrowed to the executable bit. A chart's files are read by
	// Helm and rendered into ConfigMaps; carrying a source file's ownership
	// bits into a package would make the archive differ between checkouts.
	perm := fs.FileMode(0o644)
	if mode&0o111 != 0 {
		perm = 0o755
	}
	return os.WriteFile(to, b, perm)
}

// Fingerprint hashes the staged tree so two copies can be compared without
// reading them. The path is included with the bytes, so a renamed file changes
// the answer.
func Fingerprint(dir string) (string, error) {
	var paths []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)

	h := sha256.New()
	for _, p := range paths {
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return "", err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		// CRLF is normalised. A Windows checkout with core.autocrlf on holds
		// the same file with different bytes, and this must not report that as
		// a stale stage - the chart's own line endings are decided by
		// .gitattributes, not by this comparison.
		b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
		fmt.Fprintf(h, "%s\x00%d\x00", filepath.ToSlash(rel), len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// EnvironmentValues returns the `spec.values` block of an environment's
// HelmRelease, as YAML.
//
// It is here rather than as a line of yq in the pipeline for the reason the
// rest of this repository moved off make: a pipeline that reimplements
// something drifts from what a developer runs, and the drift is found when CI
// passes and the laptop does not. `task chart:template -- prod` and the CD
// workflow's render step call this same function, so what CI proves is what a
// developer sees.
func EnvironmentValues(root, env string) ([]byte, error) {
	rel := filepath.ToSlash(filepath.Join("deploy/environments", env, "platform", "helmrelease.yaml"))
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rel, err)
	}
	var hr struct {
		Spec struct {
			Values map[string]any `json:"values"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(b, &hr); err != nil {
		return nil, fmt.Errorf("parse %s: %w", rel, err)
	}
	if len(hr.Spec.Values) == 0 {
		return nil, fmt.Errorf("%s has no spec.values - an environment that states no "+
			"differences is one nobody can read the differences of", rel)
	}
	return yaml.Marshal(hr.Spec.Values)
}
