package compliance

import (
	"bytes"
	"strings"
)

// Evidence: the text a finding was actually derived from.
//
// # Why a report keeps the manifests it read
//
// Rule 5 says reproducible or it is an opinion, and until now that was served
// by recording the INPUTS - the chart digest, the helm version, the Kubernetes
// version, the rulebook digest - so a finding could be re-derived by somebody
// who ran the same render again. That is the right guarantee and it is not the
// one a vendor engineer needs at the moment they are reading the row. Their
// question is smaller and more immediate: show me.
//
// A finding says "Deployment cfx-crds container main:
// securityContext.runAsNonRoot - runAsUser 0". Answering "is that true?" today
// means pulling the chart out of the registry, installing the same helm, and
// rendering it with the same pinned inputs. Everyone skips that, which means
// every disputed finding is settled by whether the vendor trusts the tool. The
// rendered manifest settles it in one glance instead, and it settles it the
// same way for both sides of the conversation because it is one artifact.
//
// So a run keeps the manifests it read, and the report shows the lines the
// finding is about. This is not a re-render performed when somebody clicks: it
// is the exact bytes the checks were evaluated against, kept from the run. A
// re-render could differ - a chart that reads the clock, a helm upgraded since -
// and evidence that can differ from what was judged is not evidence.
//
// # What this is NOT
//
// Not the chart. The chart's own tarball is in the registry and is downloadable
// from the release page; what is here is the OUTPUT of rendering it with the
// run's pinned inputs, which is the thing the checks looked at and the thing
// that gets installed. Not the values a site would apply either - tier 1 has no
// site values, which is why determinacy is measured rather than assumed.

// RenderedDoc is one document a run's results were derived from.
//
// Two shapes, one type. A CHART's document is the whole stream `helm template`
// produced for it, `# Source:` markers intact, because that is the unit line
// numbers are counted in: a result's RenderedLine is an offset into this, and
// slicing the stream per object would make every one of those offsets wrong. A
// PLAIN manifest's document is the file as shipped, which is the same statement
// for content that was never rendered at all.
type RenderedDoc struct {
	// Chart names the chart this rendered from. Empty for a manifest the
	// release ships as-is, which is what SourceFile then identifies.
	Chart        string `json:"chart,omitempty"`
	ChartVersion string `json:"chartVersion,omitempty"`
	// SourceFile is set only for a plain manifest. A chart's stream covers many
	// source files at once and names each of them inline.
	SourceFile string `json:"sourceFile,omitempty"`

	Content []byte `json:"-"`
	Lines   int    `json:"lines"`
	Bytes   int    `json:"bytes"`

	// Truncated says the document was cut at the evidence budget. Said rather
	// than implied: line numbers past the cut do not exist, and an excerpt that
	// silently showed the wrong lines would be worse than no excerpt.
	Truncated bool `json:"truncated,omitempty"`
}

// Key identifies a document within a run, and is what a request asks for.
//
// A chart is named by its chart name; a plain manifest by its path. They cannot
// collide: a chart name is a single DNS label and a path has separators.
func (d RenderedDoc) Key() string {
	if d.Chart != "" {
		return d.Chart
	}
	return d.SourceFile
}

// Excerpt is a window on a rendered document, cut around one result.
type Excerpt struct {
	Chart        string `json:"chart,omitempty"`
	ChartVersion string `json:"chartVersion,omitempty"`
	SourceFile   string `json:"sourceFile,omitempty"`

	// StartLine is the 1-based line number of Lines[0] in the whole document,
	// so the window can be numbered as it really is rather than from 1. A
	// reader quoting "line 12" of an excerpt into a bug report has quoted
	// nothing; the real number is what the download agrees with.
	StartLine int      `json:"startLine"`
	Lines     []string `json:"lines"`

	// Where the window is pointing, in decreasing order of certainty.
	//
	// ObjectLine is where this result's object begins. FocusLine is the line
	// the locus NAMES and is 0 unless the whole path resolved - an absent field
	// has no line, and half the findings in any run are about something that is
	// not there. NearLine is the deepest part of the locus that does exist, and
	// means only that: the caller must show it as "as far as this path goes",
	// never as the finding's line.
	//
	// Zero is a real answer in both. Pointing at the wrong line is how evidence
	// stops being evidence.
	ObjectLine int `json:"objectLine"`
	FocusLine  int `json:"focusLine,omitempty"`
	NearLine   int `json:"nearLine,omitempty"`

	TotalLines int  `json:"totalLines"`
	Truncated  bool `json:"truncated,omitempty"`
}

// Anchor is where in a document a result points, as far as is known.
//
// A struct rather than three int arguments, because three ints of the same type
// in a row is a call nobody can read and a bug nobody sees.
type Anchor struct {
	// ObjectLine is the first line of the object the result is about.
	ObjectLine int
	// FocusLine is the line the locus names, or 0.
	FocusLine int
	// NearLine is the deepest resolved part of the locus, or 0.
	NearLine int
}

// AnchorFor works out where a result points inside a document.
func AnchorFor(content []byte, objectLine int, locus string) Anchor {
	exact, near := LocusLines(content, objectLine, locus)
	return Anchor{ObjectLine: objectLine, FocusLine: exact, NearLine: near}
}

// DefaultExcerptContext is how much of the document to show around a finding.
//
// Enough to see the object's shape - its kind, its name, the block the field
// belongs to - and not so much that the answer is "read the file". A container
// with a securityContext and a resources block is about this tall.
const DefaultExcerptContext = 14

// MaxExcerptContext bounds what a caller may ask for, so the excerpt endpoint
// cannot be used to pull a whole document a line at a time.
const MaxExcerptContext = 200

// Slice cuts the window a reader sees.
//
// Centred on the most certain line available: the field itself when the locus
// resolved, the deepest part of the path that exists when it did not - which
// for a missing memory limit on the third container of four is that container,
// not the top of a Deployment eighty lines up - and the object otherwise.
//
// Where nothing but the object is known the window LEADS with it rather than
// centring on it, because the lines above an object are the `---` and the
// `# Source:` marker and the budget is better spent on its body.
func (d RenderedDoc) Slice(a Anchor, context int) Excerpt {
	if context <= 0 {
		context = DefaultExcerptContext
	}
	if context > MaxExcerptContext {
		context = MaxExcerptContext
	}

	all := splitLines(d.Content)
	ex := Excerpt{
		Chart: d.Chart, ChartVersion: d.ChartVersion, SourceFile: d.SourceFile,
		ObjectLine: a.ObjectLine, FocusLine: a.FocusLine, NearLine: a.NearLine,
		TotalLines: len(all), Truncated: d.Truncated,
	}
	if len(all) == 0 {
		return ex
	}

	centre, leading := a.FocusLine, false
	if centre <= 0 {
		centre = a.NearLine
	}
	if centre <= 0 {
		centre, leading = a.ObjectLine, true
	}
	if centre <= 0 {
		centre, leading = 1, true
	}

	from, to := centre-context, centre+context
	if leading {
		from, to = centre-2, centre+(2*context)-2
	}
	if from < 1 {
		from = 1
	}
	if to > len(all) {
		to = len(all)
	}

	ex.StartLine = from
	ex.Lines = all[from-1 : to]
	return ex
}

// LocusLine resolves a check's locus to the line of the rendered document that
// the locus names, or 0 when that field is not in the document.
//
// See LocusLines for how the walk works and why it is a text walk.
func LocusLine(content []byte, objectLine int, locus string) int {
	exact, _ := LocusLines(content, objectLine, locus)
	return exact
}

// LocusLines resolves a check's locus to two lines: the one it NAMES, and the
// line of the deepest part of the path that actually exists.
//
// # Why two numbers
//
// Half the findings in any run are about something ABSENT - a memory limit
// nobody set, a securityContext nobody wrote - and an absent field has no line.
// Answering only "not found" there would leave the excerpt anchored at the top
// of the object, which for the third container of four is the wrong screenful.
// The deepest resolved ancestor is `- name: sidecar`, and showing that with the
// missing field's parent block in view is what a reviewer actually needs.
//
// So `exact` is the claim - this line is the field - and is 0 unless the whole
// path resolved. `anchor` is only ever "here is as far as this path exists",
// which the caller must present as that and not as the finding's line.
//
// # Why this is a text walk and not a YAML query
//
// The answer has to be a LINE, and parsing YAML throws lines away: a decoded
// map knows the value and not where it was written. Round-tripping through a
// line-preserving parser would add a dependency for one screen and would still
// answer "not found" for the absent-field case, which is the case that matters
// most. A text walk answers "not found, and here is how far it got".
//
// `containers[0]` counts sequence items. `tolerations[]` - the shape a check
// writes when it means "any of them" - stops AT the key, because a collection
// has no one item to point at, and it is the collection the finding is about.
//
// Both returns are 1-based, and 0 means there is no line. Zero is a real
// answer: pointing at the wrong line is how evidence stops being evidence.
func LocusLines(content []byte, objectLine int, locus string) (exact, anchor int) {
	locus = strings.TrimSpace(locus)
	if locus == "" || objectLine <= 0 {
		return 0, 0
	}
	lines := splitLines(content)
	if objectLine > len(lines) {
		return 0, 0
	}

	// Bounded to this object's own document. Without it a locus absent from
	// this object would happily match the next object in the stream, and the
	// excerpt would show a completely different resource with a highlight on it.
	end := documentEnd(lines, objectLine)

	cur := objectLine - 1 // 0-based index of the line we last matched
	parent := -1          // indentation of the block we are searching inside
	deepest := 0

	segments := splitLocus(locus)
	for n, seg := range segments {
		key, index, indexed := parseSegment(seg)
		if key == "" {
			return 0, deepest
		}
		at := findKey(lines, cur, end, parent, key)
		if at < 0 {
			return 0, deepest
		}
		deepest = at + 1
		parent = indentOf(lines[at])
		cur = at

		if indexed && index < 0 {
			// `tolerations[]`: the check means the collection, so the key is
			// both the answer and the end of the walk - the segments after it
			// name a field of no particular item.
			return deepest, deepest
		}
		if !indexed {
			if n == len(segments)-1 {
				return deepest, deepest
			}
			continue
		}

		item := findItem(lines, at, end, parent, index)
		if item < 0 {
			// The path names an item this document does not have, so nothing
			// below it can resolve either. The collection is as far as it got.
			return 0, deepest
		}
		deepest = item + 1
		// A sequence item's own keys sit deeper than its dash, whether they
		// share its line (`- name: main`) or follow it.
		parent = indentOf(lines[item])
		cur = item
		if n == len(segments)-1 {
			return deepest, deepest
		}
	}
	return deepest, deepest
}

// splitLocus splits a dotted path, keeping bracketed indices attached to the
// segment they qualify.
func splitLocus(locus string) []string {
	parts := strings.Split(locus, ".")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseSegment reads `containers[0]`, `tolerations[]` or `runAsNonRoot`.
func parseSegment(seg string) (key string, index int, indexed bool) {
	open := strings.IndexByte(seg, '[')
	if open < 0 || !strings.HasSuffix(seg, "]") {
		return seg, -1, false
	}
	key = seg[:open]
	inner := seg[open+1 : len(seg)-1]
	if inner == "" {
		return key, -1, true
	}
	n := 0
	for _, r := range inner {
		if r < '0' || r > '9' {
			return key, -1, true
		}
		n = n*10 + int(r-'0')
	}
	return key, n, true
}

// findKey looks for `key:` inside the block that starts after `from`.
//
// The search ends when the text dedents to the parent's level or shallower,
// which is what leaving the block looks like in YAML. Without that bound a
// locus whose field is missing would match the same-named field of a sibling -
// `resources.limits.memory` absent from one container matching the next
// container's - and the highlight would be on a line that is not the finding.
func findKey(lines []string, from, end, parent int, key string) int {
	for i := from; i < end; i++ {
		text := lines[i]
		trimmed := strings.TrimSpace(text)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		ind := indentOf(text)
		if i > from && parent >= 0 && ind <= parent {
			return -1
		}
		// A key on a sequence item's own line: `- name: main`. Its effective
		// indentation is where the key starts, not where the dash does.
		if strings.HasPrefix(trimmed, "- ") {
			if matchesKey(strings.TrimSpace(trimmed[2:]), key) {
				return i
			}
			continue
		}
		if matchesKey(trimmed, key) {
			return i
		}
	}
	return -1
}

// matchesKey reports whether a line begins with exactly this mapping key.
//
// Exactly, because `runAsUser` must not match `runAsUserName`, and a value
// containing a colon must not be read as a key.
func matchesKey(trimmed, key string) bool {
	if !strings.HasPrefix(trimmed, key) {
		return false
	}
	rest := trimmed[len(key):]
	return strings.HasPrefix(rest, ":") &&
		(len(rest) == 1 || rest[1] == ' ' || rest[1] == '\t')
}

// findItem finds the nth sequence item under a key line.
//
// YAML permits the dashes at the key's own indentation or deeper and both are
// common in charts, so this takes the indentation of the FIRST dash it sees as
// the level of the sequence and counts only dashes at that level. Counting
// every dash would include the items of a nested list.
func findItem(lines []string, keyLine, end, keyIndent, index int) int {
	level, n := -1, 0
	for i := keyLine + 1; i < end; i++ {
		text := lines[i]
		trimmed := strings.TrimSpace(text)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		ind := indentOf(text)
		if ind < keyIndent || (ind == keyIndent && !strings.HasPrefix(trimmed, "-")) {
			return -1 // out of the key's block
		}
		if !strings.HasPrefix(trimmed, "-") {
			continue
		}
		if level < 0 {
			level = ind
		}
		if ind != level {
			continue
		}
		if n == index {
			return i
		}
		n++
	}
	return -1
}

// documentEnd is the index one past the last line of the document beginning at
// objectLine, which is the next `---` at column zero or the end of the stream.
func documentEnd(lines []string, objectLine int) int {
	for i := objectLine; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t\r") == "---" {
			return i
		}
	}
	return len(lines)
}

func indentOf(line string) int {
	for i := 0; i < len(line); i++ {
		if line[i] != ' ' && line[i] != '\t' {
			return i
		}
	}
	return len(line)
}

// splitLines splits on newlines WITHOUT a trailing empty element, so a document
// ending in a newline does not report one more line than it has.
func splitLines(content []byte) []string {
	if len(content) == 0 {
		return nil
	}
	body := bytes.TrimSuffix(content, []byte("\n"))
	out := strings.Split(string(body), "\n")
	for i := range out {
		out[i] = strings.TrimSuffix(out[i], "\r")
	}
	return out
}
