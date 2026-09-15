package release

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// InputsDigest identifies the content a component would be built from.
//
// WHAT IT IS FOR. "Is the image already published?" is not the question a rerun
// needs answered - the registry can hold a tag whose bits came from source that
// has since moved, and it says nothing about whether building again would
// produce the same thing. This does: it is a digest of the git tree objects
// behind the component's inputs, so it changes when, and only when, the content
// those paths hold changes.
//
// Recorded on the image as a label at build time and read back from the
// registry on the next run, it answers the one question directly:
//
//	published label == this digest   these exact bits are already published
//	published label != this digest   the source moved; building again is a
//	                                 different image and the tag must not
//	                                 silently keep pointing at the old one
//	no label                         built before this existed; rebuild
//
// It is git's own hashing, so it costs one `rev-parse` per path and needs no
// checkout of anything, no registry pull and no image comparison.
//
// WHAT IT DOES NOT COVER, and this matters: the base images are pinned by tag,
// not by digest, so `golang:1.26.8` can be rebuilt upstream with the same name
// and different content. Two builds at one digest are the same SOURCE, not
// provably the same bits. Pinning bases by digest is what would close that,
// and is a deliberate trade this repository has not made (docs/design/30).
func InputsDigest(rev string, c Component) (string, error) {
	if rev == "" {
		rev = "HEAD"
	}
	paths := append([]string(nil), c.Inputs...)
	sort.Strings(paths)

	h := sha256.New()
	for _, p := range paths {
		// The tree or blob object at that path: git has already hashed the
		// content, recursively, and will not rehash it for us.
		out, err := exec.Command("git", "rev-parse", rev+":"+p).Output() // #nosec G204 -- paths come from Components().
		if err != nil {
			return "", fmt.Errorf("%s: cannot read %s at %s (is this a shallow clone?): %w", c.Name, p, rev, err)
		}
		fmt.Fprintf(h, "%s %s\n", p, strings.TrimSpace(string(out)))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// InputsLabel is the image label the digest is recorded under.
const InputsLabel = "io.softwaregateway.inputs"
