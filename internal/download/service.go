package download

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// Service is the download-rule use case: look, run, suspend.
//
// The API and the CLI are both clients of it, which is what keeps them from
// disagreeing about what running a rule means — and it shares Resolve and Open
// with discovery, so there is no path that skips a step because a person asked
// for it rather than a scan.
type Service struct {
	packages *store.Packages
	rules    RuleStore
	log      *slog.Logger
	now      func() time.Time
}

// RuleStore is the persistence a rule's operational state needs.
//
// A consumer-defined interface: this package uses four calls, not the whole
// replication store that happens to hold them.
type RuleStore interface {
	ProductID(ctx context.Context, name string) (int64, error)
	Suspensions(ctx context.Context, productName string) (map[string]store.Suspension, error)
	Suspend(ctx context.Context, productID int64, rule, reason, actor string, until sql.NullString) error
	Resume(ctx context.Context, productID int64, rule, actor string) (bool, error)
}

// NewService builds one.
func NewService(packages *store.Packages, rules RuleStore, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{packages: packages, rules: rules, log: log, now: time.Now}
}

// WithClock replaces the clock. Tests only.
func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

// RuleStatus is everything known about one rule.
type RuleStatus struct {
	Product string
	Rule    product.Rule

	// Chain is the rule's destinations, closed and ordered. Present even when
	// the rule is suspended, because "what would this do" is the question a
	// suspended rule most needs to answer.
	Chain Chain
	// ChainError is why the chain could not be derived, if it could not.
	// Reported rather than returned, so one broken rule does not blank a
	// listing of ten.
	ChainError string

	// Suspended is the operational override, which is NOT the configured
	// `enabled`. Both are reported, because both are true and a page showing
	// only one of them is lying by omission.
	Suspended       bool
	SuspendedReason string
	SuspendedBy     string
	SuspendedUntil  string

	Revision string
}

// Runnable reports whether the rule would act if asked.
func (r RuleStatus) Runnable() bool { return r.Rule.IsEnabled() && !r.Suspended }

// List returns every rule of a product with its derived chain and its
// operational state.
func (s *Service) List(ctx context.Context, p *product.Product) ([]RuleStatus, error) {
	suspensions := map[string]store.Suspension{}
	if s.rules != nil {
		found, err := s.rules.Suspensions(ctx, p.Metadata.Name)
		if err != nil {
			// Reported rather than fatal: the rules are configuration and are
			// readable without the database. Hiding them because an override
			// lookup failed would answer a question nobody asked.
			s.log.WarnContext(ctx, "could not read rule suspensions",
				"product", p.Metadata.Name, "error", err)
		} else {
			suspensions = found
		}
	}

	out := make([]RuleStatus, 0, len(p.DownloadRules()))
	for _, r := range p.DownloadRules() {
		status := RuleStatus{Product: p.Metadata.Name, Rule: r, Revision: Revision(r)}
		chain, err := Derive(p, r.Targets)
		if err != nil {
			status.ChainError = err.Error()
		} else {
			status.Chain = chain
		}
		if sus, ok := suspensions[r.Name]; ok {
			status.Suspended = true
			status.SuspendedReason = sus.Reason
			status.SuspendedBy = sus.Actor
			status.SuspendedUntil = sus.Until.String
		}
		out = append(out, status)
	}
	return out, nil
}

// Get returns one rule's status.
func (s *Service) Get(ctx context.Context, p *product.Product, name string) (RuleStatus, error) {
	all, err := s.List(ctx, p)
	if err != nil {
		return RuleStatus{}, err
	}
	for _, r := range all {
		if r.Rule.Name == name {
			return r, nil
		}
	}
	return RuleStatus{}, fmt.Errorf("product %q has no download rule %q", p.Metadata.Name, name)
}

// RunOptions govern a manual run.
type RunOptions struct {
	// Tags names what to run. Empty means every discovered package the rule's
	// pattern matches.
	Tags []string
	// ValidateOnly renders the plan and creates nothing.
	ValidateOnly bool
	Actor        string
}

// RunResult is what a run did, or would do.
type RunResult struct {
	Rule    string
	Chain   Chain
	Matched []string
	// Created names the requests opened. Empty for a dry run, and empty when
	// every match was already requested — which is a normal outcome, not a
	// failure.
	Created []string
	// AlreadyRequested is the matches whose idempotency key already existed.
	AlreadyRequested []string
	ValidateOnly     bool
}

// Run triggers a rule by hand.
//
// The same request, the same derived chain and the same ordered steps as a
// discovery run. A manual trigger changes who is recorded as the actor and
// nothing else — there is no path that skips a step because a person asked.
func (s *Service) Run(
	ctx context.Context, p *product.Product, name string, repoIDs map[string]int64, opts RunOptions,
) (*RunResult, error) {
	status, err := s.Get(ctx, p, name)
	if err != nil {
		return nil, err
	}
	rule := status.Rule

	if !rule.TriggeredBy(product.TriggerManual) {
		return nil, fmt.Errorf(
			"rule %q does not accept a manual trigger; add `manual` to its trigger list", name)
	}
	if !rule.IsEnabled() {
		return nil, fmt.Errorf("rule %q is disabled in configuration", name)
	}
	if status.Suspended {
		return nil, fmt.Errorf("rule %q is suspended: %s", name, status.SuspendedReason)
	}
	if status.ChainError != "" {
		return nil, fmt.Errorf("rule %q: %s", name, status.ChainError)
	}

	steps, err := Resolve(p, rule, repoIDs)
	if err != nil {
		return nil, err
	}

	matches, err := s.match(ctx, p, rule, opts.Tags)
	if err != nil {
		return nil, err
	}

	res := &RunResult{
		Rule: name, Chain: status.Chain, ValidateOnly: opts.ValidateOnly,
	}
	for _, m := range matches {
		res.Matched = append(res.Matched, m.Tag)
	}
	if opts.ValidateOnly || len(matches) == 0 {
		return res, nil
	}

	productID, err := s.rules.ProductID(ctx, p.Metadata.Name)
	if err != nil {
		return nil, err
	}

	for _, pkg := range matches {
		id, created, err := s.open(ctx, p, productID, rule, steps, pkg, opts.Actor)
		if err != nil {
			return nil, err
		}
		if created {
			res.Created = append(res.Created, id)
		} else {
			res.AlreadyRequested = append(res.AlreadyRequested, pkg.Tag)
		}
	}
	return res, nil
}

func (s *Service) open(
	ctx context.Context, p *product.Product, productID int64,
	rule product.Rule, steps []ResolvedStep, pkg store.PackageRow, actor string,
) (string, bool, error) {
	tx, err := s.packages.DB().BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = tx.Rollback() }()

	id, created, err := Open(ctx, tx, s.packages, Request{
		ProductID:      productID,
		ProductName:    p.Metadata.Name,
		PackageID:      pkg.ID,
		SourceRepoID:   pkg.SourceRepoID,
		Tag:            pkg.Tag,
		RuleName:       rule.Name,
		Trigger:        TriggerManual,
		Origin:         "manual",
		RequestedBy:    actor,
		Priority:       rule.EffectivePriority(),
		IdempotencyKey: KeyFor(pkg, rule, steps),
		Steps:          steps,
	})
	if err != nil {
		return "", false, err
	}
	if err := tx.Commit(); err != nil {
		return "", false, err
	}
	return id, created, nil
}

// match finds the packages a run would act on.
func (s *Service) match(
	ctx context.Context, p *product.Product, rule product.Rule, tags []string,
) ([]store.PackageRow, error) {
	pattern, err := regexp.Compile(rule.TagPattern)
	if err != nil {
		return nil, fmt.Errorf("rule %q pattern %q: %w", rule.Name, rule.TagPattern, err)
	}

	filter := store.ListPackagesFilter{ProductName: p.Metadata.Name}
	rows, err := s.packages.ListPackages(ctx, filter)
	if err != nil {
		return nil, err
	}

	wanted := map[string]bool{}
	for _, t := range tags {
		wanted[t] = true
	}

	var out []store.PackageRow
	for _, row := range rows {
		// An explicitly named tag STILL has to match the rule's pattern.
		// Running `ga-releases` for an RC would otherwise put a release
		// candidate through the GA chain — which is exactly what the pattern
		// is for, and a manual trigger is not a licence to bypass it.
		if !pattern.MatchString(row.Tag) {
			continue
		}
		if len(wanted) > 0 && !wanted[row.Tag] {
			continue
		}
		out = append(out, row)
	}
	return out, nil
}

// Suspend stops a rule now, without editing Git.
func (s *Service) Suspend(ctx context.Context, p *product.Product, name, reason, actor string, until sql.NullString) error {
	if _, err := s.Get(ctx, p, name); err != nil {
		return err
	}
	productID, err := s.rules.ProductID(ctx, p.Metadata.Name)
	if err != nil {
		return err
	}
	if err := s.rules.Suspend(ctx, productID, name, reason, actor, until); err != nil {
		return err
	}
	s.log.InfoContext(ctx, "download rule suspended",
		"product", p.Metadata.Name, "rule", name, "actor", actor, "reason", reason)
	return nil
}

// Resume lifts a suspension.
func (s *Service) Resume(ctx context.Context, p *product.Product, name, actor string) (bool, error) {
	productID, err := s.rules.ProductID(ctx, p.Metadata.Name)
	if err != nil {
		return false, err
	}
	lifted, err := s.rules.Resume(ctx, productID, name, actor)
	if err != nil {
		return false, err
	}
	if lifted {
		s.log.InfoContext(ctx, "download rule resumed",
			"product", p.Metadata.Name, "rule", name, "actor", actor)
	}
	return lifted, nil
}
