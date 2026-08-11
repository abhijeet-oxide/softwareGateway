package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// Turning "copy this package there" into a transfer request.
//
// # One path, two verbs
//
// `transfers create` and `transfers promote` differ only in how their origin
// and destinations are RESOLVED. Underneath they produce the same row: one
// `transfer_requests` with an operation, an origin repository and a set of
// destinations, which the expander then plans exactly as it plans a request an
// auto-download rule wrote.
//
// The operation is DERIVED from what the origin resolves to rather than
// declared, because a flag that can disagree with `--from` is a flag that
// eventually will:
//
//	origin is a configured source   ->  replicate
//	origin is a configured target   ->  promote
//
// # Why resolution lives here and not in the CLI
//
// The CLI is a pure API client (docs/design/13 §1), so it cannot see product
// configuration; and the same resolution has to serve the API, a future
// scheduler and any test. It reads through the narrow ProductView below rather
// than importing internal/product, which keeps this package testable with a
// literal instead of a configuration loader.

// RepoView is one configured repository as resolution sees it.
type RepoView struct {
	// RepositoryID is the catalog row. Zero means the repository is configured
	// but has no row yet, which for a target is normal until something has
	// been written to it.
	RepositoryID int64

	Name string
	// Role is "source" or "target".
	Role string
	// Environment is the stage a target represents. Empty for a source, and
	// for a target that never promotes.
	Environment string

	Registry     string
	Repository   string
	RegistryType string

	PromotionOnly bool
	Default       bool
}

// ProductView is everything resolution needs about one product.
type ProductView struct {
	Name    string
	Sources []RepoView
	Targets []RepoView
	// PromotionFrom and PromotionTo are the environments `promote` moves
	// between when a request names neither.
	PromotionFrom string
	PromotionTo   string
}

// Catalog supplies product configuration joined to catalog row IDs.
//
// A consumer-defined interface: this package needs three lookups, not the
// product loader, the secret resolver and the reconciler that produce them.
type Catalog interface {
	ProductView(ctx context.Context, productName string) (ProductView, error)

	// RepositoryByID resolves an existing catalog row.
	//
	// Needed because a configured SOURCE does not map to one row: a source that
	// enumerates the registry's catalog owns a row per repository it found,
	// named "<source>/<path>". A package therefore knows its own row, and that
	// row — not the configured name — is what a replication reads from.
	RepositoryByID(ctx context.Context, repositoryID int64) (RepoView, error)

	// EnsureTarget returns the catalog row for a configured target, creating it
	// if nothing has been written there yet.
	//
	// A target that has never received a transfer has no row, and a request
	// naming it must still work — the row is what the transfer points at.
	EnsureTarget(ctx context.Context, productName string, target RepoView) (int64, error)
}

// CreateRequest is a request as a person expressed it, before resolution.
type CreateRequest struct {
	Product string
	// Package is a tag or a digest, resolved by the caller into Row.
	Package string
	Row     store.PackageRow

	// From names the origin. Empty means the repository the package was
	// discovered in, which is what makes `create` need no --from in the
	// ordinary vendor-to-lab case.
	From string
	// To names destinations explicitly. Empty falls back to ToEnvironment,
	// then to the product's default target.
	To []string
	// ToEnvironment fans out to every target in one environment. The
	// deliberate form of "all of them", said rather than inferred.
	ToEnvironment string

	// Promote applies the promotion rules: the origin must be a target, and
	// an omitted From/To resolves through the product's promotion path.
	Promote bool

	Priority     int
	RequestedBy  string
	Origin       string // api | cli | schedule
	ValidateOnly bool
}

// Resolved is a request with every name turned into a repository.
type Resolved struct {
	Operation string
	Origin    RepoView
	Targets   []RepoView
	Priority  int
}

// CreateResult is what creating a request produced.
type CreateResult struct {
	RequestID string
	// Created is false when an identical request already existed. Not an
	// error: it is what makes a retried CLI invocation, or a rule firing
	// twice, produce one piece of work rather than two.
	Created bool

	Operation string
	Origin    RepoView
	Targets   []RepoView
	// TransferIDs are the transfers this request opened, one per destination,
	// in the same order. Empty on a dry run.
	TransferIDs []string
}

// DefaultPriority matches the column default.
const DefaultPriority = 50

// Requester turns a request into a `transfer_requests` row.
type Requester struct {
	packages *store.Packages
	catalog  Catalog
}

// NewRequester builds one.
func NewRequester(packages *store.Packages, catalog Catalog) *Requester {
	return &Requester{packages: packages, catalog: catalog}
}

// Resolve turns names into repositories without writing anything.
//
// Separate from Create so a dry run costs no row, and so the CLI's errors —
// which are nearly all resolution errors — are produced before anything is
// committed.
func (r *Requester) Resolve(ctx context.Context, req CreateRequest) (Resolved, error) {
	view, err := r.catalog.ProductView(ctx, req.Product)
	if err != nil {
		return Resolved{}, err
	}

	origin, err := resolveOrigin(ctx, r.catalog, view, req)
	if err != nil {
		return Resolved{}, err
	}

	operation := "replicate"
	if origin.Role == "target" {
		operation = "promote"
	}
	if req.Promote && operation != "promote" {
		return Resolved{}, fmt.Errorf(
			"cannot promote from %q: it is a source, and promotion moves between targets"+
				"\nuse `transfers create --from %s` to replicate from a vendor",
			origin.Name, origin.Name)
	}

	targets, err := r.resolveTargets(ctx, view, req, operation, origin)
	if err != nil {
		return Resolved{}, err
	}

	priority := req.Priority
	if priority <= 0 {
		priority = DefaultPriority
	}

	return Resolved{Operation: operation, Origin: origin, Targets: targets, Priority: priority}, nil
}

// Create resolves and records the request.
func (r *Requester) Create(ctx context.Context, req CreateRequest) (CreateResult, error) {
	resolved, err := r.Resolve(ctx, req)
	if err != nil {
		return CreateResult{}, err
	}

	out := CreateResult{
		Operation: resolved.Operation,
		Origin:    resolved.Origin,
		Targets:   resolved.Targets,
	}
	if req.ValidateOnly {
		// AIP-163: everything is checked and nothing is written.
		//
		// What this checks is RESOLUTION — that the product, package, origin
		// and destinations exist, that promotionOnly is respected, that an
		// ambiguous environment is refused. That is where nearly every real
		// mistake lives, and catching it costs no rows.
		//
		// It does NOT report what would move. Those numbers come from the
		// planner, and the planner's honest output requires walking the
		// manifest tree and writing job rows — so producing them here would
		// mean either a second implementation that would drift from the real
		// one, or real work rolled back. Neither is worth it while
		// `transfers describe` answers the same question seconds later from
		// the transfer that actually ran. See docs/design/05 §7 for the plan
		// rendering this owes.
		return out, nil
	}

	if resolved.Origin.RepositoryID == 0 {
		return out, fmt.Errorf("origin %q has no catalog row", resolved.Origin.Name)
	}
	targetIDs := make([]int64, 0, len(resolved.Targets))
	for _, t := range resolved.Targets {
		if t.RepositoryID == 0 {
			return out, fmt.Errorf("target %q has no catalog row", t.Name)
		}
		targetIDs = append(targetIDs, t.RepositoryID)
	}

	key := IdempotencyKey(resolved.Operation, req.Row.ID,
		resolved.Origin.RepositoryID, targetIDs, "", resolved.Priority)

	tx, err := r.packages.DB().BeginTx(ctx, nil)
	if err != nil {
		return out, fmt.Errorf("begin request: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	requestedBy := req.RequestedBy
	if requestedBy == "" {
		requestedBy = "system"
	}
	origin := req.Origin
	if origin == "" {
		origin = "api"
	}

	id, created, err := r.packages.CreateTransferRequest(ctx, tx, store.TransferRequestRow{
		ID:             uuid.NewString(),
		ProductID:      req.Row.ProductID,
		PackageID:      req.Row.ID,
		Operation:      resolved.Operation,
		SourceRepoID:   resolved.Origin.RepositoryID,
		Priority:       resolved.Priority,
		IdempotencyKey: key,
		RequestedBy:    requestedBy,
		RequestOrigin:  origin,
	})
	if err != nil {
		return out, err
	}

	// One transfer per destination, opened NOW rather than derived later.
	//
	// This is what makes a request's intent durable. Deriving destinations at
	// expansion time meant reading current configuration, which turned "copy
	// to lab" into "copy to every enabled target" and left promotion — whose
	// destination is one named target, not all of them — inexpressible.
	//
	// Planning still happens later, so docs/design/04 §10 holds: a transfer is
	// planned against the registry as it is when it runs, not as it was when
	// somebody asked.
	transferIDs := make([]string, 0, len(resolved.Targets))
	for _, t := range resolved.Targets {
		transferID := uuid.NewString()
		opened, err := r.packages.CreateTransfer(ctx, tx, store.TransferRow{
			ID:           transferID,
			RequestID:    id,
			PackageID:    req.Row.ID,
			SourceRepoID: resolved.Origin.RepositoryID,
			TargetRepoID: t.RepositoryID,
			Priority:     resolved.Priority,
		})
		if err != nil {
			return out, err
		}
		if !opened {
			// A replay of the same request. UNIQUE (request_id, target_repo_id)
			// caught it, which is the point of the constraint.
			existing, err := r.packages.TransferIDFor(ctx, id, t.RepositoryID)
			if err != nil {
				return out, err
			}
			transferID = existing
		}
		transferIDs = append(transferIDs, transferID)
	}

	if err := tx.Commit(); err != nil {
		return out, fmt.Errorf("commit request: %w", err)
	}

	out.RequestID, out.Created, out.TransferIDs = id, created, transferIDs
	return out, nil
}

// ---------------------------------------------------------------------------
// Resolution
// ---------------------------------------------------------------------------

// resolveOrigin finds where the bytes come from.
//
// # Why a replication does not look its origin up by name
//
// A configured source does not map to one repository row. A source that
// enumerates the registry's catalog owns a row per repository it found, named
// "<source>/<path>" — so resolving the configured name finds nothing, and a
// name lookup here silently produced a zero row ID and a foreign-key violation
// at insert.
//
// The package already knows the row it came from, and that row is
// authoritative: a package belongs to the repository that produced it. So a
// replication resolves its origin from the PACKAGE, and `--from` naming a
// source is a check against that rather than a lookup.
func resolveOrigin(
	ctx context.Context, catalog Catalog, view ProductView, req CreateRequest,
) (RepoView, error) {
	// A target named explicitly, or the promotion path's source environment:
	// both are targets, and a target is exactly one row.
	if req.From != "" {
		if t, ok := targetNamed(view, req.From); ok {
			return t, nil
		}
	} else if req.Promote {
		return promotionOrigin(view)
	}

	origin, err := catalog.RepositoryByID(ctx, req.Row.SourceRepoID)
	if err != nil {
		return RepoView{}, fmt.Errorf(
			"package %s names a repository that is no longer in the catalog: %w",
			req.Package, err)
	}

	if req.From == "" {
		return origin, nil
	}

	// `--from` named something. It was not a target, so it has to be the
	// configured source this package actually came from — checked rather than
	// looked up, so a typo is caught instead of silently ignored.
	if configuredName(origin.Name) == req.From || origin.Name == req.From {
		return origin, nil
	}
	return RepoView{}, fmt.Errorf(
		"package %s was not discovered under %q, but under %q"+
			"\nomit --from to copy from where it was discovered, or name a target to promote from",
		req.Package, req.From, configuredName(origin.Name))
}

// promotionOrigin resolves the target a promotion reads from.
func promotionOrigin(view ProductView) (RepoView, error) {
	if hasEnvironments(view) {
		return exactlyOneIn(view.Targets, view.PromotionFrom, "--from")
	}

	// No target declares an environment. Rather than refuse outright, take the
	// one deduction that is unambiguous: promotion moves OUT of a target that
	// accepts replication and INTO one that does not, so a product with a
	// single ordinary target and a promotionOnly one needs no configuration at
	// all to promote between them.
	ordinary := make([]RepoView, 0, len(view.Targets))
	for _, t := range view.Targets {
		if !t.PromotionOnly {
			ordinary = append(ordinary, t)
		}
	}
	if len(ordinary) == 1 {
		return ordinary[0], nil
	}
	return RepoView{}, fmt.Errorf(
		"cannot tell which target to promote from: no target of product %q declares an "+
			"environment, and %d could be the origin"+
			"\nname one with --from, or set `environment:` on the targets",
		view.Name, len(ordinary))
}

// configuredName recovers which configured entry a repository row belongs to.
//
// One configured source can own several rows — "<source>/<path>" per
// enumerated repository — and the configured name is everything before the
// first slash. A configured name may not itself contain one, which is what
// makes the split unambiguous rather than a guess.
func configuredName(rowName string) string {
	if i := strings.Index(rowName, "/"); i > 0 {
		return rowName[:i]
	}
	return rowName
}

func targetNamed(view ProductView, name string) (RepoView, bool) {
	for _, t := range view.Targets {
		if t.Name == name {
			return t, true
		}
	}
	return RepoView{}, false
}

func hasEnvironments(view ProductView) bool {
	for _, t := range view.Targets {
		if t.Environment != "" {
			return true
		}
	}
	return false
}

// resolveTargets finds where the bytes go.
func (r *Requester) resolveTargets(
	ctx context.Context, view ProductView, req CreateRequest, operation string, origin RepoView,
) ([]RepoView, error) {
	targets, err := selectTargets(view, req, operation)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("product %q has no enabled target to copy to", view.Name)
	}

	// A target that has never received a transfer has no catalog row yet, and
	// a transfer has to point at one. Created here rather than assumed: the
	// alternative silently produced a zero ID and a foreign-key violation on
	// insert, which named a constraint instead of the target.
	for i := range targets {
		if targets[i].RepositoryID != 0 {
			continue
		}
		id, err := r.catalog.EnsureTarget(ctx, view.Name, targets[i])
		if err != nil {
			return nil, fmt.Errorf("register target %q: %w", targets[i].Name, err)
		}
		targets[i].RepositoryID = id
	}

	for _, t := range targets {
		// A no-op that would still open a transfer, lease jobs and write rows.
		if t.RepositoryID == origin.RepositoryID {
			return nil, fmt.Errorf("%q is both the origin and a destination", t.Name)
		}
		// promotionOnly means reachable ONLY by promotion. Enforced here as
		// well as in the expander, because the useful place to say it is the
		// moment somebody asks for it.
		if t.PromotionOnly && operation != "promote" {
			return nil, fmt.Errorf(
				"target %q is promotionOnly and cannot receive a direct replication"+
					"\npromote into it from another target instead", t.Name)
		}
	}
	return targets, nil
}

// selectTargets applies the three ways a destination can be named.
func selectTargets(view ProductView, req CreateRequest, operation string) ([]RepoView, error) {
	switch {
	case len(req.To) > 0:
		out := make([]RepoView, 0, len(req.To))
		for _, name := range req.To {
			t, err := byName(view, name)
			if err != nil {
				return nil, err
			}
			if t.Role != "target" {
				return nil, fmt.Errorf("%q is a source and cannot be a destination", name)
			}
			out = append(out, t)
		}
		return out, nil

	case req.ToEnvironment != "":
		out := inEnvironment(view.Targets, req.ToEnvironment)
		if len(out) == 0 {
			return nil, noSuchEnvironment(view, req.ToEnvironment)
		}
		return out, nil

	case operation == "promote":
		if hasEnvironments(view) {
			t, err := exactlyOneIn(view.Targets, view.PromotionTo, "--to")
			if err != nil {
				return nil, err
			}
			return []RepoView{t}, nil
		}
		// No environments declared. A promotion goes INTO a target that
		// refuses direct replication, so one promotionOnly target is
		// unambiguous without any configuration at all.
		var only []RepoView
		for _, t := range view.Targets {
			if t.PromotionOnly {
				only = append(only, t)
			}
		}
		if len(only) == 1 {
			return only, nil
		}
		return nil, fmt.Errorf(
			"cannot tell which target to promote to: no target of product %q declares an "+
				"environment, and %d are promotionOnly"+
				"\nname one with --to, or set `environment:` on the targets",
			view.Name, len(only))

	default:
		// Nothing named. Ask only what cannot be deduced.
		//
		// One enabled target is not a choice, so `default: true` should not be
		// required to express it — a product with a single destination needs no
		// flag and no configuration to say the obvious.
		if len(view.Targets) == 1 {
			return []RepoView{view.Targets[0]}, nil
		}
		// Several: the one declared default, which exists precisely for this.
		for _, t := range view.Targets {
			if t.Default {
				return []RepoView{t}, nil
			}
		}
		// Several, none default — but if all but one are promotionOnly, the
		// remaining one is still the only thing a replication could mean.
		if candidates := replicable(view.Targets); len(candidates) == 1 {
			return candidates, nil
		}
		return nil, fmt.Errorf(
			"product %q has %d targets and declares no default"+
				"\nname one with --to, or set `default: true` on one of them",
			view.Name, len(view.Targets))
	}
}

// byName finds a configured source or target.
func byName(view ProductView, name string) (RepoView, error) {
	for _, list := range [][]RepoView{view.Targets, view.Sources} {
		for _, r := range list {
			if r.Name == name {
				return r, nil
			}
		}
	}
	return RepoView{}, fmt.Errorf("product %q has no source or target named %q\navailable: %s",
		view.Name, name, strings.Join(allNames(view), ", "))
}

// exactlyOneIn resolves an environment that must name a single target.
//
// The error when several match is the interesting one, and it is why promotion
// defaults are safe to have at all: the ambiguous case names every candidate
// and says which flag settles it, so the operator's next command is obvious
// rather than a guess at the flag's spelling.
func exactlyOneIn(targets []RepoView, environment, flag string) (RepoView, error) {
	if environment == "" {
		return RepoView{}, errors.New(
			"no promotion path is configured\nname the origin and destination with --from and --to")
	}

	matches := inEnvironment(targets, environment)
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return RepoView{}, fmt.Errorf(
			"no target declares environment %q\nname one explicitly with %s, "+
				"or set `environment: %s` on the targets in that stage",
			environment, flag, environment)
	default:
		names := make([]string, 0, len(matches))
		for _, m := range matches {
			names = append(names, m.Name)
		}
		hint := "name one with " + flag
		if flag == "--to" {
			hint += ", or copy to all of them with --to-environment " + environment
		}
		return RepoView{}, fmt.Errorf("%d targets are in environment %q: %s\n%s",
			len(matches), environment, strings.Join(names, ", "), hint)
	}
}

// replicable is the targets a direct replication could legally reach.
func replicable(targets []RepoView) []RepoView {
	var out []RepoView
	for _, t := range targets {
		if !t.PromotionOnly {
			out = append(out, t)
		}
	}
	return out
}

func inEnvironment(targets []RepoView, environment string) []RepoView {
	var out []RepoView
	for _, t := range targets {
		if t.Environment == environment {
			out = append(out, t)
		}
	}
	return out
}

func noSuchEnvironment(view ProductView, environment string) error {
	seen := map[string]bool{}
	var known []string
	for _, t := range view.Targets {
		if t.Environment != "" && !seen[t.Environment] {
			seen[t.Environment] = true
			known = append(known, t.Environment)
		}
	}
	if len(known) == 0 {
		return fmt.Errorf("no target of product %q declares an environment", view.Name)
	}
	sort.Strings(known)
	return fmt.Errorf("no target of product %q is in environment %q\nconfigured: %s",
		view.Name, environment, strings.Join(known, ", "))
}

func allNames(view ProductView) []string {
	out := make([]string, 0, len(view.Sources)+len(view.Targets))
	for _, s := range view.Sources {
		out = append(out, s.Name)
	}
	for _, t := range view.Targets {
		out = append(out, t.Name)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Idempotency
// ---------------------------------------------------------------------------

// IdempotencyKey derives the key that makes a repeated request one piece of
// work rather than two.
//
// Structural rather than procedural: it becomes a UNIQUE constraint, so there
// is no check-then-insert race. That matters most for auto-download, which is
// the one path creating tens of gigabytes of work with no human in the loop
// (docs/design/04 §7).
//
// # The origin is part of the key, and was not
//
// The derivation in docs/design/04 §7 omits the source. That was harmless while
// every request replicated from the one repository a package was discovered in,
// and wrong the moment promotion arrived: `lab-eu -> production` and
// `lab-us -> production` are the same operation, package, targets and priority,
// so they derived the same key — and the second silently returned the first's
// request instead of promoting from the region asked for.
//
// Priority participates for the reason it always did: deliberately re-requesting
// the same transfer at a different priority is a distinct intent.
func IdempotencyKey(
	operation string, packageID, originRepoID int64,
	targetRepoIDs []int64, scheduledAt string, priority int,
) string {
	sorted := make([]int64, len(targetRepoIDs))
	copy(sorted, targetRepoIDs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	parts := make([]string, 0, len(sorted))
	for _, id := range sorted {
		parts = append(parts, strconv.FormatInt(id, 10))
	}

	// "|" separates fields and "," separates targets; neither can appear
	// inside a numeric field, so no two distinct inputs collide.
	material := strings.Join([]string{
		operation,
		strconv.FormatInt(packageID, 10),
		strconv.FormatInt(originRepoID, 10),
		strings.Join(parts, ","),
		scheduledAt,
		strconv.Itoa(priority),
	}, "|")

	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:])
}
