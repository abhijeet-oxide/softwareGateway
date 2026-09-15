// Package release says what this product consists of.
//
// ONE LIST, AND IT IS NOT CONFIGURATION. Which images a release contains is a
// fact about this repository - there is a Dockerfile for each and a deployment
// that runs it - so it is written here once and read by everything that needs
// it: the build matrix, the Build Info document, and the change detection that
// decides what a release has to rebuild. A repository variable would have made
// a derivable fact into a setting somebody has to keep in step, which is what
// `config/` exists to avoid and what this package replaces.
package release

// Component is one image this product publishes.
type Component struct {
	// Name is what a reader calls it, and the suffix of its repository path:
	// `worker` is published as `software-gateway-worker`.
	Name string
	// Dockerfile builds it.
	Dockerfile string
	// Inputs are the paths whose content decides what this image contains. A
	// commit that touches none of them cannot change this image, which is what
	// lets a release reuse it instead of republishing the same bits under a new
	// tag.
	//
	// Shared paths are shared on purpose: `internal/` is in two of these,
	// because a change there really does change both binaries.
	Inputs []string
}

// Image is the repository path, without registry or tag.
func (c Component) Image() string { return "software-gateway-" + c.Name }

// Components are the images, in the order a reader meets them.
//
// transferctl is deliberately absent: it has a Dockerfile because `docker
// compose` builds it for development, and it is shipped as a binary from the
// cross-compile job rather than as an image anything deploys.
func Components() []Component {
	goCommon := []string{"internal", "pkg", "go.mod", "go.sum", "deploy/go"}
	return []Component{
		{
			Name:       "coordinator",
			Dockerfile: "deploy/build/Dockerfile.coordinator",
			Inputs: append([]string{
				"cmd/coordinator",
				"deploy/build/Dockerfile.coordinator",
				"deploy/certs",
			}, goCommon...),
		},
		{
			Name:       "worker",
			Dockerfile: "deploy/build/Dockerfile.worker",
			Inputs: append([]string{
				"cmd/worker",
				"deploy/build/Dockerfile.worker",
			}, goCommon...),
		},
		{
			Name:       "web",
			Dockerfile: "deploy/build/Dockerfile.web",
			Inputs: []string{
				"web",
				"deploy/build/Dockerfile.web",
				"deploy/web",
				"deploy/npm",
			},
		},
	}
}

// Names returns the component names.
func Names() []string {
	comps := Components()
	out := make([]string, 0, len(comps))
	for _, c := range comps {
		out = append(out, c.Name)
	}
	return out
}
