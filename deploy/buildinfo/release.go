// Package buildinfo describes a published release to JFrog Artifactory.
//
// This is the pipeline describing ITSELF - what CD published, so it can be
// found, promoted and scanned in the Builds view like anything else this
// organisation ships. It is not the product feature of the same name: that one
// (docs/design/29) publishes Build Info for a transfer a customer ran, from the
// Coordinator, against their registry. Same API, different subject, no shared
// code - and the two must not be read as one thing.
package buildinfo

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Kind is how Artifactory renders a module.
type Kind string

const (
	// Docker is an OCI image: the Builds view drills into its layers.
	Docker Kind = "docker"
	// Helm is an OCI chart, which is an image with one layer and a config.
	Helm Kind = "helm"
)

// Component is one artifact a release consists of.
//
// THE LIST IS THE RELEASE. A release is every component at its published
// version, not the subset this run happened to build: an image reused because
// nothing touched its source is still part of what was released, and a Build
// Info that omits it describes a product that was never shipped. Collect
// resolves every one of these and fails if any is missing.
type Component struct {
	// Name is what a reader calls it: `coordinator`, `chart`.
	Name string
	// Path is its repository path under Repository, without a tag.
	Path string
	Kind Kind
	// Version is the tag. Images and the chart version apart: a
	// configuration-only release publishes a new chart naming older images.
	Version string
}

// Ref is the full registry reference, the way a registry names it.
func (c Component) Ref(registry, repository string) string {
	return fmt.Sprintf("%s/%s/%s:%s",
		strings.TrimSuffix(registry, "/"),
		strings.Trim(repository, "/"),
		c.Path, c.Version)
}

// Release is one publication of the product.
type Release struct {
	// Name is the Build Info name, stable across every release.
	Name string
	// Number is the Build Info number. The chart version, because that is what
	// a deployment names and what a person asks about; it makes a retry update
	// the same record rather than opening a second one.
	Number  string
	Started time.Time

	Registry   string
	Repository string

	// Where the release came from, for the Builds view's VCS panel.
	Revision  string
	URL       string
	BuildURL  string
	Principal string
}

// Components returns the release's components in a stable order.
//
// `images` are the product's own, all at imageVersion; the chart carries its
// own version. Both come from the caller rather than from this package, so the
// pipeline and this tool cannot disagree about what a release is.
func Components(images []string, imageVersion, chartName, chartVersion string) []Component {
	out := make([]Component, 0, len(images)+1)
	for _, name := range images {
		out = append(out, Component{
			Name:    name,
			Path:    "software-gateway-" + name,
			Kind:    Docker,
			Version: imageVersion,
		})
	}
	if chartName != "" {
		out = append(out, Component{
			Name:    "chart",
			Path:    chartName,
			Kind:    Helm,
			Version: chartVersion,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
