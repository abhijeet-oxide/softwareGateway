package buildinfo

import (
	"context"
	"fmt"
	"strings"
)

// Manifest is what a registry knows about one published artifact.
type Manifest struct {
	// Digest is the manifest digest, `sha256:` included.
	Digest string
	// Config and Layers are the blobs it points at, in manifest order.
	Config Blob
	Layers []Blob
}

// Blob is one addressable piece of an artifact. Its digest and nothing else:
// Artifactory's artifact record is keyed on the sha256, and a size carried here
// would be a field read from every manifest and put nowhere.
type Blob struct {
	Digest string
}

// Resolver reads a manifest from a registry. An interface because the useful
// tests for this package are the ones that do not need a registry.
type Resolver interface {
	Manifest(ctx context.Context, ref string) (Manifest, error)
}

// Module is one component, as Artifactory renders it.
type Module struct {
	ID         string            `json:"id"`
	Type       string            `json:"type"`
	Properties map[string]string `json:"properties,omitempty"`
	Artifacts  []Artifact        `json:"artifacts"`
}

// Artifact is one file inside a module.
type Artifact struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	SHA256 string `json:"sha256"`
	Path   string `json:"path,omitempty"`
}

// BuildInfo is the document Artifactory stores.
type BuildInfo struct {
	Version    string            `json:"version"`
	Name       string            `json:"name"`
	Number     string            `json:"number"`
	Started    string            `json:"started"`
	Agent      Agent             `json:"agent"`
	BuildAgent Agent             `json:"buildAgent"`
	URL        string            `json:"url,omitempty"`
	Principal  string            `json:"principal,omitempty"`
	VCS        []VCS             `json:"vcs,omitempty"`
	Modules    []Module          `json:"modules"`
	Properties map[string]string `json:"properties,omitempty"`
}

// Agent names what produced the document.
type Agent struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// VCS is the revision the release was cut from.
type VCS struct {
	Revision string `json:"revision"`
	URL      string `json:"url,omitempty"`
}

// The timestamp format Artifactory accepts. Not RFC3339: it wants exactly three
// fractional digits and no colon in the offset, and rejects the document with
// a 400 that does not say which field is wrong.
const startedFormat = "2006-01-02T15:04:05.000-0700"

// Collect resolves every component and returns the release as one document.
//
// IT RESOLVES ALL OF THEM OR NONE. A component the registry does not have is an
// error naming that component, not a module quietly left out: a Build Info
// listing two images of three is a release description that is wrong in the
// direction nobody checks - it looks complete, and the missing image is the one
// that failed to publish.
func Collect(ctx context.Context, r Resolver, rel Release, comps []Component) (*BuildInfo, error) {
	if len(comps) == 0 {
		return nil, fmt.Errorf("a release with no components is not a release")
	}
	if rel.Name == "" || rel.Number == "" {
		return nil, fmt.Errorf("build name and number are required; got %q and %q", rel.Name, rel.Number)
	}

	bi := &BuildInfo{
		Version:    "1.0.1",
		Name:       rel.Name,
		Number:     rel.Number,
		Started:    rel.Started.Format(startedFormat),
		Agent:      Agent{Name: "softwareGateway-cd", Version: rel.Number},
		BuildAgent: Agent{Name: "GitHub Actions", Version: rel.Number},
		URL:        rel.BuildURL,
		Principal:  rel.Principal,
		Properties: map[string]string{},
	}
	if rel.Revision != "" {
		bi.VCS = []VCS{{Revision: rel.Revision, URL: rel.URL}}
	}

	var missing []string
	for _, c := range comps {
		ref := c.Ref(rel.Registry, rel.Repository)
		m, err := r.Manifest(ctx, ref)
		if err != nil {
			missing = append(missing, fmt.Sprintf("  %s (%s): %v", c.Name, ref, err))
			continue
		}
		bi.Modules = append(bi.Modules, module(c, ref, m))
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf(
			"the registry does not have %d of this release's %d components:\n%s\n\n"+
				"A release is every component at its published version, not the ones this run\n"+
				"built. Publishing Build Info now would describe a release that was never\n"+
				"shipped, so nothing is published until these resolve.",
			len(missing), len(comps), strings.Join(missing, "\n"))
	}
	return bi, nil
}

func module(c Component, ref string, m Manifest) Module {
	trim := func(d string) string { return strings.TrimPrefix(d, "sha256:") }

	mod := Module{
		ID:   c.Path + ":" + c.Version,
		Type: string(c.Kind),
		Properties: map[string]string{
			"docker.image.tag": ref,
			"docker.image.id":  m.Digest,
		},
	}
	base := c.Path + "/" + c.Version + "/"
	mod.Artifacts = append(mod.Artifacts, Artifact{
		Name: "manifest.json", Type: "json",
		SHA256: trim(m.Digest), Path: base + "manifest.json",
	})
	if m.Config.Digest != "" {
		mod.Artifacts = append(mod.Artifacts, Artifact{
			Name: "sha256__" + trim(m.Config.Digest), Type: "config",
			SHA256: trim(m.Config.Digest), Path: base + "sha256__" + trim(m.Config.Digest),
		})
	}
	for _, l := range m.Layers {
		mod.Artifacts = append(mod.Artifacts, Artifact{
			Name: "sha256__" + trim(l.Digest), Type: "layer",
			SHA256: trim(l.Digest), Path: base + "sha256__" + trim(l.Digest),
		})
	}
	return mod
}
