package chartstage

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"
)

// An estate with no egress copies the third-party images into its own registry
// once, and then has to reproduce the paths the chart will ask for. Working
// those out by hand means reading a template; getting one wrong means a pod in
// ImagePullBackOff naming a path nobody wrote down.
//
// So the list is derived: the versions come from the chart's own defaults, the
// destination from the instance's `images.mirror`, and the answer is the command.

// MirroredImage is one third-party image and where it has to be copied to.
type MirroredImage struct {
	// Key is the values key it comes from, so a reader can find it.
	Key         string
	Source      string
	Destination string
}

type imagesValues struct {
	// `any` because the block also holds pullSecrets, which is a list. Only the
	// string entries are image references.
	Images map[string]any `json:"images"`
}

func stringsOnly(m map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// chartImageKeys are the third-party images, in the order they are pulled during
// a first install. `repository` and `tag` are this product's own and are pushed
// by the pipeline rather than copied.
var chartImageKeys = []string{"zitadel", "zitadelLogin", "nginx", "cerbos", "node", "postgres"}

// ImagesToMirror reports the copies an instance needs made before it can pull
// anything, and is empty when the instance pulls from upstream.
func ImagesToMirror(root, instance string) ([]MirroredImage, string, error) {
	if err := checkSegment("instance", instance); err != nil {
		return nil, "", err
	}

	defaults, err := chartImages(root)
	if err != nil {
		return nil, "", err
	}
	raw, err := InstanceValues(root, instance)
	if err != nil {
		return nil, "", err
	}
	var override imagesValues
	if err := yaml.Unmarshal(raw, &override); err != nil {
		return nil, "", fmt.Errorf("parse %s values: %w", instance, err)
	}
	for k, v := range stringsOnly(override.Images) {
		if v != "" {
			defaults[k] = v
		}
	}

	mirror := strings.TrimSuffix(defaults["mirror"], "/")
	if mirror == "" {
		return nil, "", nil
	}

	var out []MirroredImage
	for _, key := range chartImageKeys {
		source := defaults[key]
		if source == "" {
			return nil, "", fmt.Errorf("the chart declares no images.%s", key)
		}
		out = append(out, MirroredImage{
			Key:         key,
			Source:      source,
			Destination: mirroredPath(mirror, source),
		})
	}
	return out, mirror, nil
}

// mirroredPath is the Go copy of the chart's swgw.mirroredImage helper. The two
// have to agree, and TestMirrorPathsMatchTheChart is what makes sure they do.
func mirroredPath(mirror, image string) string {
	parts := strings.Split(image, "/")
	if len(parts) > 1 && (strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":")) {
		parts = parts[1:]
	}
	return mirror + "/" + strings.Join(parts, "/")
}

func chartImages(root string) (map[string]string, error) {
	rel := "deploy/charts/software-gateway/values.yaml"
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))) // #nosec G304 -- fixed path.
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rel, err)
	}
	var v imagesValues
	if err := yaml.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("parse %s: %w", rel, err)
	}
	if v.Images == nil {
		return nil, fmt.Errorf("%s declares no images", rel)
	}
	return stringsOnly(v.Images), nil
}

// WriteMirrorSetup prints the copies for one instance.
func WriteMirrorSetup(w io.Writer, root, instance string) error {
	images, mirror, err := ImagesToMirror(root, instance)
	if err != nil {
		return err
	}
	if len(images) == 0 {
		fmt.Fprintf(w, "# %s sets no images.mirror, so every third-party image is pulled from\n", instance)
		fmt.Fprintf(w, "# upstream and there is nothing to copy.\n")
		return nil
	}

	fmt.Fprintf(w, "# %s - %d third-party images to copy into %s.\n", instance, len(images), mirror)
	fmt.Fprintf(w, "# Versions come from the chart, the destination from this instance's\n")
	fmt.Fprintf(w, "# images.mirror, so these paths are the ones the pods will ask for.\n")
	fmt.Fprintf(w, "#\n")
	fmt.Fprintf(w, "# This product's own three images are PUSHED BY THE PIPELINE and are not here.\n")
	fmt.Fprintf(w, "#\n")
	fmt.Fprintf(w, "# crane, skopeo and `az acr import` all take source then destination:\n")
	fmt.Fprintf(w, "#\n")
	fmt.Fprintf(w, "#   skopeo copy docker://<source> docker://<destination>\n")
	fmt.Fprintf(w, "#   az acr import --name <registry> --source <source> --image <destination path>\n")
	fmt.Fprintf(w, "\n")

	for _, i := range images {
		fmt.Fprintf(w, "# images.%s\n", i.Key)
		fmt.Fprintf(w, "crane copy %s %s\n", i.Source, i.Destination)
	}
	return nil
}
