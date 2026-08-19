package vendors

import "github.com/abhijeet-oxide/softwareGateway/internal/oci"

// Classifier names what ONE artifact is, in the words a person uses about it -
// image, chart, file, index, signature.
//
// # Why this is a function and not a call to oci.Classify
//
// Three readers answer this question about the same content: the artifact
// listing behind a release page, the content breakdown of a transfer, and a
// comparison of two releases. oci.Classify exists so that they agree, and for a
// conformant registry it is enough on its own.
//
// It is not enough for every vendor. A NEAR orb's Helm charts are plain OCI
// image manifests carrying an ordinary image config: media type, artifact type
// and config media type are IDENTICAL for its charts and its images, and the
// only evidence anywhere is the annotation the vendor wrote on them. A reader
// with the OCI rules alone reports an orb of 160 images, 97 charts and 2 files
// as 260 images - which is what the download page said, beside a release page
// that had already named them correctly.
//
// So the three readers take a Classifier, and the composition root builds it
// from the product's configured layouts. Nothing downstream names a vendor, and
// a reader given no layouts still gets the OCI rules.
type Classifier func(mediaType, artifactType, configMediaType string, annotations map[string]string) string

// OCIOnly is the classifier for content whose registry conforms - and the
// fallback for a caller that has no layouts.
//
// A nil Classifier means this one. That is deliberate: the failure mode of
// forgetting to pass one is content named by the standard rules, not a panic
// and not an unnamed component.
func OCIOnly(mediaType, artifactType, configMediaType string, _ map[string]string) string {
	return oci.Classify(mediaType, artifactType, configMediaType)
}

// ClassifierFor builds the classifier for a product from the layouts its
// sources declare.
//
// The vendor's annotations are consulted FIRST and the OCI fields second, which
// is the opposite of the usual precedence and is deliberate. The OCI rules are
// authoritative when the fields they read are populated; for an index's
// children they are not, because discovery records what the index LISTED
// without fetching each child, so the config media type is missing and every
// chart looks like an image. The annotation is the only evidence available at
// that point, and it is evidence the vendor wrote about its own content.
//
// A product may in principle declare several layouts. Every one that names a
// real layout gets a say, in order, and the first to recognise an artifact
// wins - which for the overwhelmingly common single-vendor product is just
// that vendor. An unresolvable name is skipped rather than failing the read:
// the caller is rendering a page, and a page with standard names on it is a
// better answer than an error.
func ClassifierFor(reg *Registry, layoutNames []string) Classifier {
	if reg == nil {
		return OCIOnly
	}

	var layouts []Layout
	seen := map[string]bool{}
	for _, name := range layoutNames {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if l, err := reg.Get(name); err == nil {
			layouts = append(layouts, l)
		}
	}
	if len(layouts) == 0 {
		return OCIOnly
	}

	return func(mediaType, artifactType, configMediaType string, annotations map[string]string) string {
		for _, l := range layouts {
			if kind := l.ClassifyArtifact(annotations); kind != "" {
				return kind
			}
		}
		return OCIOnly(mediaType, artifactType, configMediaType, annotations)
	}
}
