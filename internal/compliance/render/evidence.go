package render

import (
	"bytes"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
)

// Keeping the rendered text a run judged, within a budget.

// EvidenceKeeper accumulates rendered documents against a release-wide budget.
//
// # Why the caller can hold one across several Loads
//
// A release is fetched one chart artifact at a time and each is loaded on its
// own, so a keeper made per Load would give every chart the whole RELEASE
// budget - which is not a budget. The Preparer makes one for the run and hands
// it to every Load, and a Loader given none makes its own, which is right for
// the single-directory case the CLI and the tests use.
//
// Not concurrency-safe, and does not need to be: charts are rendered in
// sequence, because two helm subprocesses per chart on a ninety-seven-chart
// release is a way to run a Coordinator out of memory rather than a way to make
// it faster.
type EvidenceKeeper struct {
	budget    EvidenceBudget
	remaining int64
	off       bool
}

// NewEvidenceKeeper starts a budget for one release.
func NewEvidenceKeeper(b EvidenceBudget) *EvidenceKeeper {
	return &EvidenceKeeper{budget: b, remaining: b.perRelease(), off: b.off()}
}

// Keep appends one document, truncated to whatever budget is left.
//
// # Why a truncated document rather than a dropped one
//
// A dropped document takes every finding in that chart with it, including the
// ones in its first hundred lines that the budget was never in danger of
// reaching. Truncation costs only the findings past the cut, and those are told
// that the text stops there - the excerpt for a line beyond the cut is refused,
// never approximated.
//
// A document with nothing left of it at all is not kept: a zero-length record
// says a chart rendered to nothing, which is a different and false statement.
func (e *EvidenceKeeper) Keep(into *[]compliance.RenderedDoc, doc compliance.RenderedDoc, content []byte) {
	if e.off || len(content) == 0 || e.remaining <= 0 {
		return
	}

	limit := e.budget.perDocument()
	if e.remaining < limit {
		limit = e.remaining
	}
	if int64(len(content)) > limit {
		content = cutAtLine(content, limit)
		doc.Truncated = true
	}
	if len(content) == 0 {
		return
	}

	// Copied, because the caller's slice is the subprocess buffer and this
	// outlives it.
	doc.Content = bytes.Clone(content)
	doc.Bytes = len(doc.Content)
	doc.Lines = bytes.Count(doc.Content, []byte("\n"))
	if doc.Content[len(doc.Content)-1] != '\n' {
		doc.Lines++
	}

	e.remaining -= int64(doc.Bytes)
	*into = append(*into, doc)
}

// cutAtLine truncates on a line boundary, so the last line kept is a whole one.
//
// A document cut mid-line ends in a fragment that looks like a malformed
// manifest, and somebody would eventually report the fragment as a defect in
// the vendor's chart.
func cutAtLine(content []byte, limit int64) []byte {
	if limit <= 0 || int64(len(content)) <= limit {
		return content
	}
	head := content[:limit]
	if i := bytes.LastIndexByte(head, '\n'); i >= 0 {
		return head[:i+1]
	}
	return nil
}
