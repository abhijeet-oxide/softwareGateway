package compliance

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"
)

// Parsing a rendered manifest stream into addressed resources.
//
// This is the boundary between "helm produced some text" and "the engine has
// subjects". It is here rather than in the renderer because the fixture corpus
// uses it too: a test that fed the engine hand-built Go maps would not exercise
// the same path a release does, and the differences - a document that is just a
// comment, a `---` inside a string, a list at the top level - are exactly where
// a parser is wrong.

// SourceMarker is the comment helm writes above each document naming the
// template it came from.
//
// It is the only reliable way to attribute a rendered object to a file. The
// alternative - re-rendering one template at a time - is n times slower and
// still wrong for a template that emits several documents. This is why the
// renderer must not strip comments.
const SourceMarker = "# Source: "

// ParseManifests splits a rendered stream into resources, attributing each to
// the template it came from.
//
// # Why errors are collected rather than returned
//
// One unparseable document in a 600-object release must not lose the other
// 599. Each failure becomes a check-independent error the caller records, so
// the run is inconclusive and says which document could not be read - rather
// than green because the parse gave up, or empty because it returned early.
func ParseManifests(stream []byte, base Address) ([]Resource, []error) {
	var (
		out  []Resource
		errs []error
	)
	for _, doc := range splitDocuments(stream) {
		if doc.empty() {
			continue
		}
		var obj map[string]any
		if err := yaml.Unmarshal(doc.body, &obj); err != nil {
			errs = append(errs, fmt.Errorf("%s line %d: %w", orUnknown(doc.source), doc.line, err))
			continue
		}
		if len(obj) == 0 {
			continue
		}
		res := Resource{Object: obj, Address: base}
		if obj["kind"] == nil || obj["apiVersion"] == nil {
			// Not a Kubernetes object. Helm charts legitimately render
			// fragments - a NOTES file, a values dump - and failing on them
			// would make a compliant chart inconclusive.
			continue
		}
		res.Address.SourceFile = doc.source
		res.Address.RenderedLine = doc.line
		res.Address.APIVersion = res.APIVersion()
		res.Address.Kind = res.Kind()
		res.Address.Namespace = res.Namespace()
		res.Address.Name = res.Name()
		out = append(out, res)
	}
	return out, errs
}

type document struct {
	body   []byte
	source string
	line   int
}

func (d document) empty() bool { return len(bytes.TrimSpace(d.body)) == 0 }

// splitDocuments divides a stream on YAML document separators, carrying the
// most recent `# Source:` marker onto each.
//
// A separator is a line that is exactly `---` (optionally with trailing
// whitespace). Matching a prefix instead would split inside a block scalar
// containing a line of dashes, which is what a chart embedding a PEM
// certificate or a markdown document looks like.
func splitDocuments(stream []byte) []document {
	var (
		docs    []document
		buf     bytes.Buffer
		source  string
		pending string
		start   = 1
		lineNo  = 0
	)
	flush := func() {
		docs = append(docs, document{body: append([]byte(nil), buf.Bytes()...), source: source, line: start})
		buf.Reset()
	}

	sc := bufio.NewScanner(bytes.NewReader(stream))
	// Rendered manifests contain long single lines - a base64 secret, an
	// inlined config file - and the default 64 KiB limit truncates them into a
	// parse error that names the wrong problem.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if strings.TrimRight(line, " \t\r") == "---" {
			flush()
			source = pending
			pending = ""
			start = lineNo + 1
			continue
		}
		if s, ok := strings.CutPrefix(strings.TrimSpace(line), SourceMarker); ok {
			// The marker sits after the separator that begins its document, so
			// it applies to the document being accumulated now.
			source = strings.TrimSpace(s)
			pending = source
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	flush()
	return docs
}

func orUnknown(s string) string {
	if s == "" {
		return "(unknown template)"
	}
	return s
}
