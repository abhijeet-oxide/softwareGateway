package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// The evidence endpoints: the manifests a run judged, and the lines a finding
// is about.
//
// # Why a finding needs to be shown and not only stated
//
// "Deployment cfx-crds container main: securityContext.runAsNonRoot - runAsUser
// 0" is a precise claim, and a vendor's first question about a precise claim is
// whether it is true. Answering it from the run's recorded inputs means pulling
// the chart, installing the same helm and rendering it again with the same
// pinned versions. Nobody does that, so every disputed finding is settled by
// whether the vendor trusts the tool - which is not a technical conversation
// and does not converge.
//
// These serve the exact bytes the checks were evaluated against, kept with the
// run. Two shapes because there are two moments: an EXCERPT while reading one
// row, and a DOWNLOAD when the conversation moves to email.

// ComplianceEvidence is the stored manifest text of a run.
//
// Separate from ComplianceStore because it is separately absent: a deployment
// may turn evidence off, and a run recorded before this existed has none. The
// routes are registered either way and answer "this run kept no manifests",
// which is a different and more useful answer than a 404 on the whole feature.
type ComplianceEvidence interface {
	ComplianceRenderedIndex(ctx context.Context, runID string) ([]store.ComplianceRenderedRow, error)
	ComplianceRendered(ctx context.Context, runID, key string) (store.ComplianceRenderedRow, error)
	ComplianceRenderedAll(ctx context.Context, runID string) ([]store.ComplianceRenderedRow, error)
}

// RenderedDocumentView is one document in the index.
type RenderedDocumentView struct {
	// Document is what the content and excerpt endpoints take: a chart's name,
	// or the path of a manifest the release ships as-is.
	Document     string `json:"document"`
	Chart        string `json:"chart,omitempty"`
	ChartVersion string `json:"chartVersion,omitempty"`
	SourceFile   string `json:"sourceFile,omitempty"`

	Lines int `json:"lines"`
	Bytes int `json:"bytes"`
	// Truncated says the document was cut at this deployment's evidence budget,
	// so line numbers past the cut do not exist. An excerpt beyond it is
	// refused rather than approximated.
	Truncated bool `json:"truncated,omitempty"`
}

// ListRenderedResponse is GET .../compliance/rendered.
type ListRenderedResponse struct {
	Product string `json:"product"`
	Release string `json:"release"`
	RunID   string `json:"runId,omitempty"`

	Documents []RenderedDocumentView `json:"documents"`
	// TotalBytes is what a whole-release download would weigh, so the button
	// offering it can say so before somebody presses it.
	TotalBytes int `json:"totalBytes"`
}

// ComplianceExcerptResponse is GET .../compliance/rendered/excerpt.
type ComplianceExcerptResponse struct {
	compliance.Excerpt

	// Document is the key the content endpoint takes for this excerpt's
	// document, so the "download the whole thing" link needs no assembly.
	Document string `json:"document"`
	// Locus is the path the check was looking at, echoed so a reader can see
	// what FocusLine is pointing at - or, when it is zero, what was looked for
	// and not found.
	Locus string `json:"locus,omitempty"`
}

// handleComplianceRendered serves
// GET /api/v1/products/{product}/packages/{package}/compliance/rendered.
func (s *Server) handleComplianceRendered(w http.ResponseWriter, r *http.Request) {
	run, productName, ok := s.evidenceRun(w, r)
	if !ok {
		return
	}

	docs, err := s.deps.ComplianceEvidence.ComplianceRenderedIndex(r.Context(), run.ID)
	if err != nil {
		s.internal(w, r, "list the rendered manifests", err)
		return
	}

	out := ListRenderedResponse{
		Product: productName, Release: chi.URLParam(r, "package"), RunID: run.ID,
		Documents: make([]RenderedDocumentView, 0, len(docs)),
	}
	for _, d := range docs {
		out.Documents = append(out.Documents, renderedDocumentView(d))
		out.TotalBytes += d.Bytes
	}
	w.Header().Set("Cache-Control", "private, no-store")
	WriteJSON(w, r, http.StatusOK, out)
}

// handleComplianceRenderedContent serves
// GET .../compliance/rendered/content?document=&download=
//
// Text, not JSON. What comes back is a manifest stream a person opens in an
// editor, feeds to `kubectl diff`, or attaches to a mail to the vendor, and
// wrapping it in a JSON string would make every one of those a decoding step
// first. `document` omitted means the whole release, concatenated in one file -
// which is the artifact a vendor conversation actually needs.
func (s *Server) handleComplianceRenderedContent(w http.ResponseWriter, r *http.Request) {
	run, _, ok := s.evidenceRun(w, r)
	if !ok {
		return
	}

	key := strings.TrimSpace(r.URL.Query().Get("document"))
	download := r.URL.Query().Get("download") != ""

	var (
		body     string
		filename string
	)
	if key == "" {
		docs, err := s.deps.ComplianceEvidence.ComplianceRenderedAll(r.Context(), run.ID)
		if err != nil {
			s.internal(w, r, "read the rendered manifests", err)
			return
		}
		if len(docs) == 0 {
			Error(w, r, v1.CodeNotFound, noEvidence)
			return
		}
		body = concatRendered(docs, run)
		filename = evidenceFilename(chi.URLParam(r, "package"), "", true)
	} else {
		doc, err := s.deps.ComplianceEvidence.ComplianceRendered(r.Context(), run.ID, key)
		if errors.Is(err, store.ErrNotFound) {
			Error(w, r, v1.CodeNotFound,
				"this run kept no rendered manifest called "+key+
					"; a chart that did not render produced none")
			return
		}
		if err != nil {
			s.internal(w, r, "read the rendered manifest", err)
			return
		}
		body = doc.Content
		filename = evidenceFilename(chi.URLParam(r, "package"), doc.Chart+doc.SourceFile, false)
	}

	// text/yaml with a charset, because these are UTF-8 and a browser that
	// guesses gets a chart's non-ASCII annotations wrong.
	w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	// nosniff, because this is vendor-authored text served from our origin and
	// a browser that content-sniffed it into HTML would run it.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if download {
		w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// handleComplianceExcerpt serves
// GET .../compliance/rendered/excerpt?seq=&context=
//
// # Why this takes a result and not an address
//
// The caller could pass the chart, the line and the locus - it has all three on
// the row it is displaying - and then the excerpt would be a claim assembled by
// whoever asked for it. It takes the result's `seq` instead, reads the address
// off the stored run, and resolves the line here. What comes back is then a
// statement about what the run found, which is what evidence has to be.
func (s *Server) handleComplianceExcerpt(w http.ResponseWriter, r *http.Request) {
	run, _, ok := s.evidenceRun(w, r)
	if !ok {
		return
	}

	seq, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("seq")))
	if err != nil || seq < 0 {
		Error(w, r, v1.CodeInvalidArgument,
			"seq must be the position of a result in this run")
		return
	}

	rows, _, err := s.deps.ComplianceStore.ComplianceResults(
		r.Context(), run.ID, store.ComplianceFilter{Seq: &seq, Limit: 1})
	if err != nil {
		s.internal(w, r, "read the compliance result", err)
		return
	}
	if len(rows) == 0 {
		Error(w, r, v1.CodeNotFound, "this run has no result at that position")
		return
	}
	res := rows[0]

	key := res.Chart
	if key == "" {
		key = res.SourceFile
	}
	if key == "" {
		Error(w, r, v1.CodeNotFound, noEvidenceFor(res))
		return
	}

	row, err := s.deps.ComplianceEvidence.ComplianceRendered(r.Context(), run.ID, key)
	if errors.Is(err, store.ErrNotFound) {
		Error(w, r, v1.CodeNotFound, noEvidenceFor(res))
		return
	}
	if err != nil {
		s.internal(w, r, "read the rendered manifest", err)
		return
	}

	doc := compliance.RenderedDoc{
		Chart: row.Chart, ChartVersion: row.ChartVersion, SourceFile: row.SourceFile,
		Content: []byte(row.Content), Lines: row.Lines, Bytes: row.Bytes,
		Truncated: row.Truncated,
	}
	// Past the cut there is nothing to show, and showing the lines that ARE
	// there under this result's number would be showing a different object.
	if res.RenderedLine > doc.Lines {
		Error(w, r, v1.CodeNotFound,
			fmt.Sprintf("this result is at line %d of %s, and only the first %d lines were kept: "+
				"the rendered manifests of this release exceeded the evidence budget",
				res.RenderedLine, key, doc.Lines))
		return
	}

	anchor := compliance.AnchorFor(doc.Content, res.RenderedLine, res.Locus)
	out := ComplianceExcerptResponse{
		Excerpt:  doc.Slice(anchor, excerptContext(r)),
		Document: key,
		Locus:    res.Locus,
	}
	w.Header().Set("Cache-Control", "private, no-store")
	WriteJSON(w, r, http.StatusOK, out)
}

const noEvidence = "this run kept no rendered manifests: either it predates them being kept, " +
	"a later run has superseded it, or this Coordinator has evidence turned off"

// noEvidenceFor says which of the ordinary reasons applies to ONE result, since
// the commonest by far is not a fault at all: a check that could not be decided
// is addressed to a chart that never rendered, so there is nothing to show.
func noEvidenceFor(res store.ComplianceResultRow) string {
	if res.Outcome == string(compliance.OutcomeError) {
		return "there is no rendered manifest for this result: the chart did not render, " +
			"which is why the check could not be decided"
	}
	return noEvidence
}

// evidenceRun resolves the release and its latest run, or writes the reason.
//
// The LATEST run deliberately: the manifests are kept for it alone, because
// they are the one part of a run whose size the vendor sets and every run of
// every release would otherwise keep a copy. Nothing reads an older run's
// evidence because nothing displays an older run.
func (s *Server) evidenceRun(w http.ResponseWriter, r *http.Request) (
	store.ComplianceRunRow, string, bool,
) {
	productName := chi.URLParam(r, "product")
	ref := chi.URLParam(r, "package")

	var zero store.ComplianceRunRow
	if !s.productExists(w, r, productName) {
		return zero, "", false
	}
	if s.deps.ComplianceStore == nil || s.deps.ComplianceEvidence == nil {
		Error(w, r, v1.CodeUnavailable, "compliance is not configured on this Coordinator")
		return zero, "", false
	}
	pkg, ok := s.resolvePackage(w, r, productName, ref)
	if !ok {
		return zero, "", false
	}

	run, err := s.deps.ComplianceStore.LatestComplianceRun(r.Context(), pkg.ID)
	if errors.Is(err, store.ErrNotFound) {
		Error(w, r, v1.CodeNotFound,
			"this release has not been checked, so there are no manifests to show")
		return zero, "", false
	}
	if err != nil {
		s.internal(w, r, "read compliance run", err)
		return zero, "", false
	}
	return run, productName, true
}

func excerptContext(r *http.Request) int {
	n, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("context")))
	if err != nil || n <= 0 {
		return compliance.DefaultExcerptContext
	}
	return n
}

func renderedDocumentView(d store.ComplianceRenderedRow) RenderedDocumentView {
	key := d.Chart
	if key == "" {
		key = d.SourceFile
	}
	return RenderedDocumentView{
		Document: key, Chart: d.Chart, ChartVersion: d.ChartVersion,
		SourceFile: d.SourceFile,
		Lines:      d.Lines, Bytes: d.Bytes, Truncated: d.Truncated,
	}
}

// concatRendered assembles the whole release into one file.
//
// Every document keeps a banner naming what it is, because the file is read by
// somebody who did not run it - and helm's own `# Source:` markers name the
// template inside a chart, never the chart. The run's identity is at the top
// for the same reason: a manifest set with no statement of what produced it is
// exactly the artifact this whole feature exists to replace.
func concatRendered(docs []store.ComplianceRenderedRow, run store.ComplianceRunRow) string {
	var b strings.Builder
	b.WriteString("# Rendered manifests as checked by Software Gateway.\n")
	b.WriteString("#\n")
	b.WriteString("# These are the exact manifests the compliance run evaluated, not a\n")
	b.WriteString("# re-render: a chart rendered again could differ from what was judged.\n")
	fmt.Fprintf(&b, "# run:          %s\n", run.ID)
	if !run.FinishedAt.IsZero() {
		fmt.Fprintf(&b, "# checked:      %s\n", run.FinishedAt.UTC().Format("2006-01-02T15:04:05Z"))
	}
	if run.HelmVersion != "" {
		fmt.Fprintf(&b, "# helm:         %s\n", run.HelmVersion)
	}
	if run.KubeVersion != "" {
		fmt.Fprintf(&b, "# kubeVersion:  %s\n", run.KubeVersion)
	}
	if run.BundleDigest != "" {
		fmt.Fprintf(&b, "# rulebook:     %s\n", run.BundleDigest)
	}

	for _, d := range docs {
		b.WriteString("\n# ")
		b.WriteString(strings.Repeat("-", 68))
		b.WriteString("\n")
		switch {
		case d.Chart != "" && d.ChartVersion != "":
			fmt.Fprintf(&b, "# chart: %s %s\n", d.Chart, d.ChartVersion)
		case d.Chart != "":
			fmt.Fprintf(&b, "# chart: %s\n", d.Chart)
		default:
			fmt.Fprintf(&b, "# file: %s (shipped as-is, not rendered)\n", d.SourceFile)
		}
		if d.Truncated {
			b.WriteString("# TRUNCATED at this deployment's evidence budget: " +
				"the lines below are not the whole document.\n")
		}
		b.WriteString("# ")
		b.WriteString(strings.Repeat("-", 68))
		b.WriteString("\n")
		b.WriteString(d.Content)
		if !strings.HasSuffix(d.Content, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// evidenceFilename names the download after what is in it, because a vendor
// receives several of these and "rendered.yaml" four times is four files
// nobody can tell apart.
func evidenceFilename(release, document string, whole bool) string {
	name := safeFilenamePart(release)
	if name == "" {
		name = "release"
	}
	if whole || document == "" {
		return name + "-rendered.yaml"
	}
	doc := safeFilenamePart(document)
	if doc == "" {
		doc = "document"
	}
	return name + "-" + doc + "-rendered.yaml"
}

// safeFilenamePart keeps a header value to characters that cannot end a quoted
// string early or name a directory. A chart called `../../etc` is not a header
// this serves.
func safeFilenamePart(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-.")
}
