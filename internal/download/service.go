package download

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// Service is the download use case: look at what is configured, and run it.
//
// The API and the CLI are both clients of it, and it shares Resolve and Open
// with discovery — so a download somebody types and a download a rule fires are
// the same operation, planned by the same code, gated the same way.

type Service struct {
	packages *store.Packages
	products ProductIDs
	log      *slog.Logger
}

// ProductIDs resolves a configured product name to its catalog row.
type ProductIDs interface {
	ProductID(ctx context.Context, name string) (int64, error)
}

// NewService builds one.
func NewService(packages *store.Packages, products ProductIDs, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{packages: packages, products: products, log: log}
}

// DownloadStatus is one configured download and the chain it resolves to.
type DownloadStatus struct {
	Product  string
	Download product.Download

	// Chain is the destinations closed over `mirror.from` and ordered.
	Chain Chain
	// ChainError is why it could not be derived. Reported rather than
	// returned, so one broken download does not blank a listing.
	ChainError string

	Default  bool
	Revision string
}

// Downloads returns every configured download with its resolved chain.
//
// A product that declares none still gets one entry: the implicit download to
// its default target, which is what a rule naming nothing has always meant.
// Showing it beats showing an empty list and leaving the reader to wonder what
// `transferctl download` would actually do.
func (s *Service) Downloads(_ context.Context, p *product.Product) []DownloadStatus {
	declared := p.Downloads()
	if len(declared) == 0 {
		if t, ok := p.DefaultTarget(); ok {
			declared = []product.Download{{Targets: []string{t.Name}}}
		}
	}

	out := make([]DownloadStatus, 0, len(declared))
	for _, d := range declared {
		status := DownloadStatus{
			Product: p.Metadata.Name, Download: d,
			Default: d.Default || len(declared) == 1, Revision: Revision(d),
		}
		chain, err := Derive(p, d.Targets)
		if err != nil {
			status.ChainError = err.Error()
		} else {
			status.Chain = chain
		}
		out = append(out, status)
	}
	return out
}

// DownloadNamed returns one download's status, or the default when name is
// empty.
func (s *Service) DownloadNamed(ctx context.Context, p *product.Product, name string) (DownloadStatus, error) {
	all := s.Downloads(ctx, p)
	for _, d := range all {
		switch {
		case name != "" && d.Download.Name == name:
			return d, nil
		case name == "" && d.Default:
			return d, nil
		}
	}
	if name == "" {
		return DownloadStatus{}, fmt.Errorf(
			"product %q declares no default download and no default target", p.Metadata.Name)
	}
	return DownloadStatus{}, fmt.Errorf("product %q has no download %q", p.Metadata.Name, name)
}

// RuleStatus is one auto-download rule and the download it triggers.
type RuleStatus struct {
	Product string
	Rule    product.Rule

	// Download is what the rule triggers, resolved. A rule carrying the older
	// inline `targets` describes its own, which is why this is a value rather
	// than a name.
	Download product.Download
	// DownloadName is what to call it on screen. Empty for an inline one.
	DownloadName string

	Chain      Chain
	ChainError string
}

// Rules returns every auto-download rule with the download it triggers.
func (s *Service) Rules(_ context.Context, p *product.Product) []RuleStatus {
	out := make([]RuleStatus, 0, len(p.Spec.AutoDownload.Rules))
	for _, r := range p.Spec.AutoDownload.Rules {
		status := RuleStatus{Product: p.Metadata.Name, Rule: r, DownloadName: r.Download}
		d, err := p.DownloadFor(r)
		if err != nil {
			status.ChainError = err.Error()
			out = append(out, status)
			continue
		}
		status.Download = d
		if d.Name != "" {
			status.DownloadName = d.Name
		}
		chain, derr := Derive(p, d.Targets)
		if derr != nil {
			status.ChainError = derr.Error()
		} else {
			status.Chain = chain
		}
		out = append(out, status)
	}
	return out
}

// RunOptions govern a manual download.
type RunOptions struct {
	// Download names which configured download to use. Empty means the
	// default.
	Download string
	// ValidateOnly renders the plan and creates nothing.
	ValidateOnly bool
	Actor        string
}

// RunResult is what a download did, or would do.
type RunResult struct {
	Download  string
	Chain     Chain
	Requested []string
	// Created names the requests opened; AlreadyRequested is the packages
	// whose idempotency key already existed, which is a normal outcome rather
	// than a failure.
	Created          []string
	AlreadyRequested []string
	ValidateOnly     bool
}

// Run downloads named software, by hand.
//
// It consults NO pattern. Patterns belong to auto-download rules, which decide
// what to download when nobody is asking; here somebody is asking, and they
// named the software. A pattern checked at this point could only disagree with
// the person typing.
func (s *Service) Run(
	ctx context.Context, p *product.Product, tags []string,
	repoIDs map[string]int64, opts RunOptions,
) (*RunResult, error) {
	if len(tags) == 0 {
		return nil, fmt.Errorf("name at least one package to download")
	}

	status, err := s.DownloadNamed(ctx, p, opts.Download)
	if err != nil {
		return nil, err
	}
	if status.ChainError != "" {
		return nil, fmt.Errorf("%s: %s", downloadLabel(status.Download), status.ChainError)
	}

	steps, err := Resolve(p, status.Download, repoIDs)
	if err != nil {
		return nil, err
	}

	packages, err := s.find(ctx, p, tags)
	if err != nil {
		return nil, err
	}

	res := &RunResult{
		Download: downloadLabel(status.Download), Chain: status.Chain,
		ValidateOnly: opts.ValidateOnly,
	}
	for _, pkg := range packages {
		res.Requested = append(res.Requested, pkg.Tag)
	}
	if opts.ValidateOnly {
		return res, nil
	}

	productID, err := s.products.ProductID(ctx, p.Metadata.Name)
	if err != nil {
		return nil, err
	}
	for _, pkg := range packages {
		id, created, err := s.open(ctx, p, productID, status.Download, steps, pkg, opts.Actor)
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
	d product.Download, steps []ResolvedStep, pkg store.PackageRow, actor string,
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
		DownloadName:   d.Name,
		Trigger:        TriggerManual,
		Origin:         "manual",
		RequestedBy:    actor,
		Priority:       d.EffectivePriority(),
		IdempotencyKey: KeyFor(pkg, d, steps),
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

// find resolves named tags to discovered packages.
//
// A tag nobody has discovered is an error rather than a silent skip: somebody
// typed it, and telling them it moved nothing without saying why is the least
// useful possible answer.
func (s *Service) find(ctx context.Context, p *product.Product, tags []string) ([]store.PackageRow, error) {
	rows, err := s.packages.ListPackages(ctx, store.ListPackagesFilter{ProductName: p.Metadata.Name})
	if err != nil {
		return nil, err
	}

	byTag := map[string]store.PackageRow{}
	for _, row := range rows {
		byTag[row.Tag] = row
		if row.DisplayTag != "" {
			// Both spellings resolve, so anything a listing showed can be
			// pasted back.
			byTag[row.DisplayTag] = row
		}
	}

	out := make([]store.PackageRow, 0, len(tags))
	for _, tag := range tags {
		row, ok := byTag[tag]
		if !ok {
			return nil, fmt.Errorf(
				"no discovered package %q in product %q; `transferctl packages list %s` shows what there is",
				tag, p.Metadata.Name, p.Metadata.Name)
		}
		out = append(out, row)
	}
	return out, nil
}

// Matches reports which discovered packages an auto-download rule would select.
//
// Read-only, and the only place a pattern is evaluated outside discovery. It
// backs "what would this rule pick up", which is a question about the RULE
// rather than about downloading.
func (s *Service) Matches(ctx context.Context, p *product.Product, ruleName string) ([]string, error) {
	rule, ok := p.Rule(ruleName)
	if !ok {
		return nil, fmt.Errorf("product %q has no auto-download rule %q", p.Metadata.Name, ruleName)
	}
	pattern, err := regexp.Compile(rule.TagPattern)
	if err != nil {
		return nil, fmt.Errorf("rule %q pattern %q: %w", ruleName, rule.TagPattern, err)
	}

	rows, err := s.packages.ListPackages(ctx, store.ListPackagesFilter{ProductName: p.Metadata.Name})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, row := range rows {
		if pattern.MatchString(row.Tag) {
			out = append(out, row.Tag)
		}
	}
	return out, nil
}
