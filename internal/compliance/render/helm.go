// Package render turns delivered artifacts into the Kubernetes objects a check
// judges.
//
// # Why a subprocess and not the Helm Go SDK
//
// helm.sh/helm/v3 is a large dependency that pulls in most of client-go and
// pins a Kubernetes API version into this binary. More decisively, it renders
// with the SDK's template engine at the version this binary was built against,
// which is not necessarily the version the operator's own `helm template`
// would use - so a finding could be unreproducible by the person receiving it.
//
// The subprocess renders with the binary the operator has. `helm template` is a
// stable, documented interface, its version is recorded on every run, and a
// vendor can reproduce any finding with one command and no access to this
// platform. That last property is what the whole feature is for.
//
// # What happens when helm is absent
//
// Chart-structure checks still run. Everything needing rendered manifests
// reports `error`, and the run is inconclusive. It is never a pass: a release
// whose charts were never rendered has not been shown to comply with anything,
// and reporting it green is the single most damaging thing this package could
// do.
package render

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// Helm renders charts by invoking the helm binary.
type Helm struct {
	// Binary is the executable, looked up on PATH when empty.
	Binary string

	// The pinned inputs. Every one of these appears in rendered output, so a
	// value that varied between runs would make results vary between runs -
	// which Rule 5 forbids. They are configuration rather than constants
	// because the right Kubernetes version is a property of the estate.
	KubeVersion string
	APIVersions []string
	ReleaseName string
	Namespace   string

	// Timeout bounds one render. A chart with a template loop that does not
	// terminate must not take the Coordinator with it.
	Timeout time.Duration
}

// Defaults chosen so a run is reproducible rather than convenient.
const (
	// DefaultReleaseName and DefaultNamespace are fixed because .Release.Name
	// and .Release.Namespace appear in rendered names, labels and selectors. A
	// varying release name produces varying findings for an unchanged chart.
	DefaultReleaseName = "sgw-compliance"
	DefaultNamespace   = "sgw-compliance"
	DefaultKubeVersion = "1.30.0"
	DefaultTimeout     = 90 * time.Second
)

// WithDefaults fills anything the operator did not set.
func (h Helm) WithDefaults() Helm {
	if h.Binary == "" {
		h.Binary = "helm"
	}
	if h.ReleaseName == "" {
		h.ReleaseName = DefaultReleaseName
	}
	if h.Namespace == "" {
		h.Namespace = DefaultNamespace
	}
	if h.KubeVersion == "" {
		h.KubeVersion = DefaultKubeVersion
	}
	if h.Timeout == 0 {
		h.Timeout = DefaultTimeout
	}
	return h
}

// ErrHelmUnavailable is returned when the binary is not usable. Callers turn it
// into `error` results naming the charts that could not be rendered - not into
// a silent absence of findings.
var ErrHelmUnavailable = errors.New("compliance: the helm binary is not available, so charts cannot be rendered")

// Version reports the helm version, and whether it can be used at all.
//
// Called once at start-up and recorded on every run. "Which helm rendered
// this" is part of what makes a finding reproducible, and a run that cannot
// answer it is an opinion.
func (h Helm) Version(ctx context.Context) (string, error) {
	h = h.WithDefaults()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// The binary name is operator configuration, from the same document as
	// every other path this process opens. It is not user input and there is no
	// shell: exec.CommandContext passes arguments directly.
	cmd := exec.CommandContext(ctx, h.Binary, "version", "--short") //nolint:gosec // operator-configured binary, no shell
	cmd.Env = sandboxEnv()
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrHelmUnavailable, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Output is one chart's rendered manifests.
type Output struct {
	// Manifests is the rendered stream, with helm's own `# Source:` markers
	// intact. They are the only reliable way to attribute an object to the
	// template it came from, so nothing here strips comments.
	Manifests []byte
	// Stderr carries helm's warnings even on success - a deprecated API
	// version, a missing dependency it worked around. Recorded because a
	// warning about the render is part of what the run knows.
	Stderr string
}

// Render runs `helm template` against a chart directory.
//
// # Why these flags and not others
//
//	--kube-version, --api-versions   pinned, so a chart branching on the
//	                                 cluster version renders the same way twice
//	--include-crds                   a CRD the release ships is a resource the
//	                                 release ships; excluding it would let
//	                                 UPG-07 report a custom resource as orphaned
//	--no-hooks NOT passed            hook Jobs are objects that run in the
//	                                 cluster, and MTA-08 exists because an
//	                                 unbounded one breaks the second upgrade
//	--dependency-update NOT passed   it reaches the network. A chart whose
//	                                 dependencies are not vendored cannot be
//	                                 rendered here, which is a finding (SUP-07)
//	                                 rather than something to work around.
func (h Helm) Render(ctx context.Context, chartDir string, valuesFiles ...string) (Output, error) {
	h = h.WithDefaults()
	ctx, cancel := context.WithTimeout(ctx, h.Timeout)
	defer cancel()

	// Absolute, because the command runs with the chart directory as its
	// working directory: helm would otherwise resolve a relative path against
	// that, fail to find it, and report it as a missing chart REPOSITORY -
	// an error message that sends the reader somewhere else entirely.
	chartDir, err := filepath.Abs(chartDir)
	if err != nil {
		return Output{}, fmt.Errorf("resolving chart path: %w", err)
	}

	args := []string{
		"template", h.ReleaseName, chartDir,
		"--namespace", h.Namespace,
		"--kube-version", h.KubeVersion,
		"--include-crds",
	}
	for _, v := range h.APIVersions {
		args = append(args, "--api-versions", v)
	}
	for _, f := range valuesFiles {
		args = append(args, "--values", f)
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, h.Binary, args...) //nolint:gosec // operator-configured binary, arguments built here and passed without a shell
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// The chart directory is the working directory, so a template referring to
	// a relative path cannot reach outside it.
	cmd.Dir = chartDir
	cmd.Env = sandboxEnv()

	err = cmd.Run()
	out := Output{Manifests: stdout.Bytes(), Stderr: strings.TrimSpace(stderr.String())}
	if err != nil {
		if ctx.Err() != nil {
			return out, fmt.Errorf("rendering %s timed out after %s: %w", filepath.Base(chartDir), h.Timeout, ctx.Err())
		}
		// helm's stderr is the message a vendor needs. Passing our own instead
		// would hide the line and column of the template that failed.
		//
		// Its BOILERPLATE is dropped, though. helm ends a great many errors
		// with "Use --debug flag to render out invalid YAML" - advice about a
		// flag nobody reading a compliance report can pass, on a render that
		// already happened. It is two lines of noise on every failed row of a
		// coverage table, it is stored and exported with the finding, and its
		// words "invalid YAML" once made this code classify a nil dereference
		// as a YAML parse error.
		return out, fmt.Errorf("helm template failed for %s: %s",
			filepath.Base(chartDir), firstLines(stripHelmAdvice(out.Stderr), 8))
	}
	return out, nil
}

// sandboxEnv is the environment a render runs with.
//
// # Why the environment is emptied rather than inherited
//
// A chart cannot read the environment, but helm can: KUBECONFIG would let it
// contact a cluster, HELM_REPOSITORY_CONFIG and the plugin directory would let
// it load code from the operator's home, and a proxy variable would give a
// render network access it must not have. A rendering step that behaves
// differently on two machines is a rendering step whose findings are not
// reproducible.
//
// HOME points at a temporary directory rather than being unset, because helm
// derives its cache and config paths from it and fails outright without one.
func sandboxEnv() []string {
	home := os.TempDir()
	return []string{
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
		"HELM_CACHE_HOME=" + filepath.Join(home, "sgw-helm-cache"),
		"HELM_CONFIG_HOME=" + filepath.Join(home, "sgw-helm-config"),
		"HELM_DATA_HOME=" + filepath.Join(home, "sgw-helm-data"),
		// No plugins. A plugin is code from the operator's machine running
		// inside what is meant to be a pure function of the chart.
		"HELM_PLUGINS=" + filepath.Join(home, "sgw-helm-noplugins"),
	}
}

// ChartMeta is what Chart.yaml declares.
type ChartMeta struct {
	Name         string        `json:"name"`
	Version      string        `json:"version"`
	AppVersion   string        `json:"appVersion,omitempty"`
	Description  string        `json:"description,omitempty"`
	Type         string        `json:"type,omitempty"`
	Dependencies []ChartDepend `json:"dependencies,omitempty"`
}

// ChartDepend is one declared dependency. SUP-07 reads these: a version range
// makes the render unreproducible, and an unvendored dependency makes it
// impossible in an air-gapped install.
type ChartDepend struct {
	Name       string `json:"name"`
	Version    string `json:"version,omitempty"`
	Repository string `json:"repository,omitempty"`
	Condition  string `json:"condition,omitempty"`
	Alias      string `json:"alias,omitempty"`
}

// ReadChartMeta parses a chart's Chart.yaml.
func ReadChartMeta(chartDir string) (ChartMeta, error) {
	var meta ChartMeta
	b, err := os.ReadFile(filepath.Join(chartDir, "Chart.yaml")) //nolint:gosec // path derived from an unpacked artifact
	if err != nil {
		return meta, fmt.Errorf("reading Chart.yaml: %w", err)
	}
	if err := yaml.Unmarshal(b, &meta); err != nil {
		return meta, fmt.Errorf("parsing Chart.yaml: %w", err)
	}
	return meta, nil
}

// ReadValues parses a chart's default values.yaml. A chart with none is
// normal, and produces an empty map rather than an error.
func ReadValues(chartDir string) (map[string]any, error) {
	b, err := os.ReadFile(filepath.Join(chartDir, "values.yaml")) //nolint:gosec // path derived from an unpacked artifact
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("reading values.yaml: %w", err)
	}
	return ParseValues(b)
}

// ParseValues turns values.yaml text into the map a chart carries.
//
// Split out of ReadValues so the render cache can rebuild a chart's values from
// the bytes it stored without a directory. The two must never diverge: a check
// reading `chart.values` on a cache hit has to see exactly what it would have
// seen on a miss, or the cache has changed an answer.
func ParseValues(b []byte) (map[string]any, error) {
	var v map[string]any
	if err := yaml.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("parsing values.yaml: %w", err)
	}
	if v == nil {
		v = map[string]any{}
	}
	return v, nil
}

// ReadValuesFile returns values.yaml as it was shipped, for the render cache.
func ReadValuesFile(chartDir string) []byte {
	b, err := os.ReadFile(filepath.Join(chartDir, "values.yaml")) //nolint:gosec // path derived from an unpacked artifact
	if err != nil {
		return nil
	}
	return b
}

// IsChart reports whether a directory is a Helm chart.
func IsChart(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, "Chart.yaml"))
	return err == nil && !st.IsDir()
}

// firstLines truncates a multi-line error so one broken chart does not fill a
// database column, while keeping the part that names the template.
func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n") + fmt.Sprintf("\n… (%d more lines)", len(lines)-n)
}
