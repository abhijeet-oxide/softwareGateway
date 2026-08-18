package api

import (
	"context"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/abhijeet-oxide/softwareGateway/internal/download"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// Downloader is the download surface the API needs.
//
// A consumer-defined interface: four calls, not the store and the chain
// arithmetic behind them.
type Downloader interface {
	Downloads(ctx context.Context, p *product.Product) []download.DownloadStatus
	Rules(ctx context.Context, p *product.Product) []download.RuleStatus
	Run(ctx context.Context, p *product.Product, tags []string,
		repoIDs map[string]int64, opts download.RunOptions) (*download.RunResult, error)
	Matches(ctx context.Context, p *product.Product, rule string) ([]string, error)
}

// TargetRows resolves configured target names to catalog rows.
//
// Separate from Downloader because it is a different question with a different
// failure: a download can be listed and described without it, and only running
// one needs the rows to exist.
type TargetRows interface {
	TargetRowIDs(ctx context.Context, productName string) (map[string]int64, error)
}

func (s *Server) handleListDownloads(w http.ResponseWriter, r *http.Request) {
	p, ok := s.resolveProduct(w, r)
	if !ok {
		return
	}
	out := v1.ListDownloadsResponse{}
	for _, d := range s.deps.Downloads.Downloads(r.Context(), p) {
		out.Downloads = append(out.Downloads, downloadView(d))
	}
	WriteJSON(w, r, http.StatusOK, out)
}

// handleRunDownload downloads named software.
//
// It takes tags and no pattern, which is the whole shape of the thing: a
// pattern decides what to download when nobody is asking, and here somebody is.
func (s *Server) handleRunDownload(w http.ResponseWriter, r *http.Request) {
	p, ok := s.resolveProduct(w, r)
	if !ok {
		return
	}

	var req v1.RunDownloadRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}
	if v := r.URL.Query().Get("validateOnly"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			req.ValidateOnly = b
		}
	}
	if len(req.Tags) == 0 {
		Error(w, r, v1.CodeInvalidArgument,
			"name at least one package to download; a download takes software, not a pattern")
		return
	}

	repoIDs := map[string]int64{}
	if s.deps.TargetRows != nil {
		found, err := s.deps.TargetRows.TargetRowIDs(r.Context(), p.Metadata.Name)
		if err != nil {
			Error(w, r, v1.CodeFailedPrecondition, err.Error())
			return
		}
		repoIDs = found
	}

	res, err := s.deps.Downloads.Run(r.Context(), p, req.Tags, repoIDs, download.RunOptions{
		Download: req.Download, ValidateOnly: req.ValidateOnly, Actor: actorOf(r),
	})
	if err != nil {
		Error(w, r, v1.CodeFailedPrecondition, err.Error())
		return
	}

	WriteJSON(w, r, http.StatusOK, v1.RunDownloadResponse{
		Product: p.Metadata.Name, Download: res.Download,
		Chain: res.Chain.Targets(), Requested: res.Requested,
		Created: res.Created, AlreadyRequested: res.AlreadyRequested,
		ValidateOnly: res.ValidateOnly,
	})
}

func (s *Server) handleListAutoDownloadRules(w http.ResponseWriter, r *http.Request) {
	p, ok := s.resolveProduct(w, r)
	if !ok {
		return
	}
	out := v1.ListAutoDownloadRulesResponse{Enabled: p.AutoDownloadEnabled()}
	for _, rule := range s.deps.Downloads.Rules(r.Context(), p) {
		out.Rules = append(out.Rules, autoRuleView(rule))
	}
	WriteJSON(w, r, http.StatusOK, out)
}

// handleRuleMatches answers "what would this rule pick up".
//
// A question about the RULE, not about downloading — which is why it is a GET
// on the rule and creates nothing.
func (s *Server) handleRuleMatches(w http.ResponseWriter, r *http.Request) {
	p, ok := s.resolveProduct(w, r)
	if !ok {
		return
	}
	name := chi.URLParam(r, "rule")

	matches, err := s.deps.Downloads.Matches(r.Context(), p, name)
	if err != nil {
		Error(w, r, v1.CodeNotFound, err.Error())
		return
	}
	WriteJSON(w, r, http.StatusOK, v1.MatchesResponse{
		Product: p.Metadata.Name, Rule: name, Matches: matches,
	})
}

func downloadView(d download.DownloadStatus) v1.DownloadView {
	return v1.DownloadView{
		Product: d.Product, Name: d.Download.Name,
		Targets: d.Download.Targets,
		Chain:   d.Chain.Targets(), ChainText: d.Chain.Describe(), ChainError: d.ChainError,
		Priority: d.Download.EffectivePriority(), Default: d.Default,
		VerifyBefore: tristate(d.Download.VerifiesBefore()),
		VerifyAfter:  tristate(d.Download.VerifiesAfter()),
		VerifyPolicy: string(d.Download.VerificationPolicy()),
		Revision:     d.Revision,
	}
}

func autoRuleView(r download.RuleStatus) v1.AutoDownloadRuleView {
	return v1.AutoDownloadRuleView{
		Product: r.Product, Name: r.Rule.Name,
		TagPattern: r.Rule.TagPattern, Sources: r.Rule.Sources,
		Download: r.DownloadName,
		Chain:    r.Chain.Targets(), ChainText: r.Chain.Describe(), ChainError: r.ChainError,
		Enabled: r.Rule.IsEnabled(),
		Inline:  r.Rule.UsesLegacyShape(),
	}
}

// tristate renders an inherited setting as "inherit" rather than as false.
//
// The distinction is the whole reason these are pointers: a download that says
// nothing about destination verification is not one that turned it off, and
// rendering both as `false` would tell a reader the opposite of the truth.
func tristate(b *bool) string {
	switch {
	case b == nil:
		return "inherit"
	case *b:
		return "true"
	default:
		return "false"
	}
}
