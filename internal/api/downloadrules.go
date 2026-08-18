package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/abhijeet-oxide/softwareGateway/internal/download"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// Ruler is the download-rule surface the API needs.
//
// A consumer-defined interface for the same reason as Replicator: four calls,
// not the store and the chain arithmetic behind them.
type Ruler interface {
	List(ctx context.Context, p *product.Product) ([]download.RuleStatus, error)
	Get(ctx context.Context, p *product.Product, name string) (download.RuleStatus, error)
	Run(ctx context.Context, p *product.Product, name string,
		repoIDs map[string]int64, opts download.RunOptions) (*download.RunResult, error)
	Suspend(ctx context.Context, p *product.Product, name, reason, actor string, until sql.NullString) error
	Resume(ctx context.Context, p *product.Product, name, actor string) (bool, error)
}

// TargetRows resolves configured target names to catalog rows.
//
// Separate from Ruler because it is a different question with a different
// failure: a rule can be listed and described without it, and only running one
// needs the rows to exist.
type TargetRows interface {
	TargetRowIDs(ctx context.Context, productName string) (map[string]int64, error)
}

func (s *Server) handleListDownloadRules(w http.ResponseWriter, r *http.Request) {
	p, ok := s.resolveProduct(w, r)
	if !ok {
		return
	}
	rules, err := s.deps.Rules.List(r.Context(), p)
	if err != nil {
		Error(w, r, v1.CodeInternal, err.Error())
		return
	}

	out := v1.ListDownloadRulesResponse{Rules: make([]v1.DownloadRuleView, 0, len(rules))}
	for _, rule := range rules {
		out.Rules = append(out.Rules, downloadRuleView(rule))
	}
	WriteJSON(w, r, http.StatusOK, out)
}

func (s *Server) handleGetDownloadRule(w http.ResponseWriter, r *http.Request) {
	p, ok := s.resolveProduct(w, r)
	if !ok {
		return
	}
	rule, err := s.deps.Rules.Get(r.Context(), p, chi.URLParam(r, "rule"))
	if err != nil {
		Error(w, r, v1.CodeNotFound, err.Error())
		return
	}
	WriteJSON(w, r, http.StatusOK, downloadRuleView(rule))
}

// handleRunDownloadRule triggers a rule by hand.
//
// It produces the same request, the same derived chain and the same ordered
// steps as a discovery run. What a manual trigger changes is who is recorded
// as the actor — there is no path that skips a step because a person asked.
func (s *Server) handleRunDownloadRule(w http.ResponseWriter, r *http.Request) {
	p, ok := s.resolveProduct(w, r)
	if !ok {
		return
	}
	name := chi.URLParam(r, "rule")

	var req v1.RunDownloadRuleRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}
	if v := r.URL.Query().Get("validateOnly"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			req.ValidateOnly = b
		}
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

	res, err := s.deps.Rules.Run(r.Context(), p, name, repoIDs, download.RunOptions{
		Tags: req.Tags, ValidateOnly: req.ValidateOnly, Actor: actorOf(r),
	})
	if err != nil {
		Error(w, r, v1.CodeFailedPrecondition, err.Error())
		return
	}

	WriteJSON(w, r, http.StatusOK, v1.RunDownloadRuleResponse{
		Product: p.Metadata.Name, Rule: name,
		Chain: res.Chain.Targets(), Matched: res.Matched,
		Created: res.Created, AlreadyRequested: res.AlreadyRequested,
		ValidateOnly: res.ValidateOnly,
	})
}

// handleSuspendDownloadRule stops a rule now, WITHOUT editing configuration.
//
// Git keeps intent. This is an operational override, and the response says so
// explicitly, because a caller who assumed it had edited the rule would be
// surprised twice — once when Git still said enabled, and again when somebody
// resumed it without a commit.
func (s *Server) handleSuspendDownloadRule(w http.ResponseWriter, r *http.Request) {
	p, ok := s.resolveProduct(w, r)
	if !ok {
		return
	}
	name := chi.URLParam(r, "rule")

	var req v1.SuspendDownloadRuleRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}
	if req.Reason == "" {
		Error(w, r, v1.CodeInvalidArgument,
			"a reason is required: a suspension nobody can explain is one nobody dares lift")
		return
	}

	var until sql.NullString
	if req.Until != "" {
		t, err := time.Parse(time.RFC3339, req.Until)
		if err != nil {
			Error(w, r, v1.CodeInvalidArgument,
				fmt.Sprintf("until %q is not an RFC 3339 timestamp", req.Until))
			return
		}
		until = sql.NullString{String: t.UTC().Format("2006-01-02T15:04:05.000Z"), Valid: true}
	}

	if err := s.deps.Rules.Suspend(r.Context(), p, name, req.Reason, actorOf(r), until); err != nil {
		Error(w, r, v1.CodeFailedPrecondition, err.Error())
		return
	}
	WriteJSON(w, r, http.StatusOK, v1.SuspendDownloadRuleResponse{
		Product: p.Metadata.Name, Rule: name, Suspended: true,
		Reason: req.Reason, Until: req.Until,
		Note: "configuration was not changed; `enabled` still says what Git says, and this override is reported alongside it",
	})
}

func (s *Server) handleResumeDownloadRule(w http.ResponseWriter, r *http.Request) {
	p, ok := s.resolveProduct(w, r)
	if !ok {
		return
	}
	name := chi.URLParam(r, "rule")

	lifted, err := s.deps.Rules.Resume(r.Context(), p, name, actorOf(r))
	if err != nil {
		Error(w, r, v1.CodeFailedPrecondition, err.Error())
		return
	}
	note := "the rule was not suspended"
	if lifted {
		note = "the override is lifted; the rule now does what configuration says"
	}
	WriteJSON(w, r, http.StatusOK, v1.SuspendDownloadRuleResponse{
		Product: p.Metadata.Name, Rule: name, Suspended: false, Note: note,
	})
}

func downloadRuleView(r download.RuleStatus) v1.DownloadRuleView {
	rule := r.Rule
	view := v1.DownloadRuleView{
		Product: r.Product, Name: rule.Name,
		TagPattern: rule.TagPattern, Sources: rule.Sources, Targets: rule.Targets,
		Chain: r.Chain.Targets(), ChainText: r.Chain.Describe(), ChainError: r.ChainError,
		Priority: rule.EffectivePriority(),
		Enabled:  rule.IsEnabled(), Suspended: r.Suspended, Runnable: r.Runnable(),
		SuspendedReason: r.SuspendedReason, SuspendedBy: r.SuspendedBy,
		SuspendedUntil: r.SuspendedUntil,
		VerifyBefore:   tristate(rule.VerifiesBefore()),
		VerifyAfter:    tristate(rule.VerifiesAfter()),
		VerifyPolicy:   string(rule.VerificationPolicy()),
		Revision:       r.Revision,
	}
	for _, t := range rule.Triggers() {
		view.Trigger = append(view.Trigger, string(t))
	}
	if w := rule.Window; w != nil {
		zone := w.TimeZone
		if zone == "" {
			zone = "UTC"
		}
		view.Window = fmt.Sprintf("%s-%s %s", w.Start, w.End, zone)
	}
	return view
}

// tristate renders an inherited setting as "inherit" rather than as false.
//
// The distinction is the whole reason these are pointers: a rule that says
// nothing about destination verification is not a rule that turned it off, and
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
