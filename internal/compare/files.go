package compare

import "sort"

// Comparing the FILES inside two releases, without downloading either of them.
//
// # Why a layer digest is not an answer
//
// "Two layers changed" is true and useless. A release that edited one line of
// one configuration file and a release that rewrote everything produce the same
// sentence, because a layer is an archive and its digest changes when anything
// inside it does.
//
// # Why this needs no bytes
//
// Because the manifests already say it. An OCI 1.1 artifact carries ONE FILE
// PER LAYER with `org.opencontainers.image.title` naming it - the ORAS
// convention, used by Helm, by cosign and by every vendor bundle this system
// handles - so a component's manifest is already a list of file names, each
// with the content digest and the size of the file under it.
//
// A comparison is therefore two of those lists, aligned by path:
//
//   - the same path with the same digest is the same file. Content addressing
//     is what makes that a proof rather than an assumption.
//   - the same path with a different digest changed.
//   - a path on one side only was added or removed.
//
// Nothing here needs the file's CONTENT, and this used to download all of it:
// every layer of every changed component, on both sides, hundreds of megabytes,
// to compute a comparison of digests it already held. That was the whole of the
// time a comparison took, it was the reason analysing a release did not make
// comparing it faster, and it is gone.
//
// # What is deliberately not answered
//
// WHICH LINES changed inside a file that changed. That genuinely needs both
// copies, and it is a question about one file that somebody can ask about one
// file - not a reason to download a release to render a table.
//
// An image layer carries no title, names nothing, and is not a file. It is
// excluded rather than listed as `layer sha256:…`, which would restate the
// digest difference in the column that exists to say more than the digest.

// FileDiff is one named file inside a component, and what became of it.
//
// Both digests and both sizes are carried, because "changed" is the answer that
// prompts the next question: a reader looking at a changed file wants what it
// was and what it is, and a row that carried one of them would send them back
// to the other side of the comparison to find the other.
type FileDiff struct {
	// Path is the publisher's own name for the file, which for a titled layer
	// IS the path - `CONFIGURATION/cfx_sample_ns_input.json`.
	Path string
	// Verdict is same, changed, only-a or only-b, in the same vocabulary the
	// component rows use.
	Verdict Verdict
	// SizeA and SizeB are what the file weighs on each side. Zero where that
	// side does not have it.
	SizeA, SizeB int64
	// DigestA and DigestB are the file's content digests. Empty where that side
	// does not have it.
	DigestA, DigestB string
}

// diffFiles aligns the named files of two sides of one component.
//
// Either side may be nil, which is how an added or a removed component is
// reported: every file it carries is added or removed with it.
//
// Sorted by path. The order is the reader's - a directory listing - rather than
// the order the publisher happened to pack the layers in.
func diffFiles(a, b *Item) []FileDiff {
	byPath := func(item *Item) map[string]Layer {
		out := map[string]Layer{}
		if item == nil {
			return out
		}
		for _, layer := range item.Layers {
			if layer.Title == "" {
				// No name, so not a file: an image layer is a tar of an unknown
				// number of paths and cannot be aligned with anything.
				continue
			}
			// LAST WINS, because layers stack: a later layer replacing a path
			// is what a consumer would actually get.
			out[layer.Title] = layer
		}
		return out
	}

	inA, inB := byPath(a), byPath(b)
	if len(inA) == 0 && len(inB) == 0 {
		return nil
	}

	paths := make([]string, 0, len(inA)+len(inB))
	for path := range inA {
		paths = append(paths, path)
	}
	for path := range inB {
		if _, both := inA[path]; !both {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)

	out := make([]FileDiff, 0, len(paths))
	for _, path := range paths {
		left, hasA := inA[path]
		right, hasB := inB[path]

		diff := FileDiff{
			Path:    path,
			SizeA:   left.Size,
			SizeB:   right.Size,
			DigestA: left.Digest,
			DigestB: right.Digest,
		}
		switch {
		case !hasB:
			diff.Verdict = VerdictOnlyA
		case !hasA:
			diff.Verdict = VerdictOnlyB
		case left.Digest != right.Digest:
			diff.Verdict = VerdictChanged
		default:
			diff.Verdict = VerdictSame
		}
		out = append(out, diff)
	}
	return out
}

// inspectFiles fills in the file-level account of every row that differs.
//
// Not for rows that agree: a component whose own digest matches on both sides
// is byte-identical, so every file in it is too, and listing them would be
// thousands of rows saying nothing. The files of a component that DID change
// are all listed, unchanged ones included - that is the context that makes the
// changed ones legible.
func inspectFiles(rows []Row) {
	for i := range rows {
		row := &rows[i]
		if row.Verdict == VerdictSame {
			continue
		}
		row.Files = diffFiles(row.A, row.B)
	}
}
