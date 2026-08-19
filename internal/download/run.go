package download

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// Opening a run: one code path for discovery and for a person.
//
// Both produce the same request, the same derived chain and the same ordered
// steps, because there is no path that skips a step because it was asked for by
// a human. That property is what makes the audit trail worth reading.

// downloadLabel names a download for an error message. A product with one
// download does not have to name it, so there is not always a name to use.
func downloadLabel(d product.Download) string {
	if d.Name != "" {
		return d.Name
	}
	return "the default download"
}

// Trigger records how a run started, so a request from a rule that fired on
// its own is distinguishable from one somebody asked for.
type Trigger string

const (
	TriggerDiscovery Trigger = "discovery"
	TriggerManual    Trigger = "manual"
	TriggerSchedule  Trigger = "schedule"
)

// Step resolution, shared with the caller that has already looked up rows.
type ResolvedStep struct {
	Name      string
	RepoID    int64
	Index     int
	DependsOn string // the NAME of the step this one waits for
}

// Resolve closes a download's destinations over `mirror.from`, orders them,
// and resolves each to its catalog row.
func Resolve(p *product.Product, d product.Download, repoIDs map[string]int64) ([]ResolvedStep, error) {
	chain, err := Derive(p, d.Targets)
	if err != nil {
		return nil, fmt.Errorf("download %q: %w", downloadLabel(d), err)
	}

	out := make([]ResolvedStep, 0, len(chain.Steps))
	for _, step := range chain.Steps {
		t, ok := p.Target(step.Target)
		if !ok {
			return nil, fmt.Errorf("download %q names unknown target %q", downloadLabel(d), step.Target)
		}
		if t.PromotionOnly {
			return nil, fmt.Errorf(
				"download %q reaches target %q, which is promotionOnly: it can only be reached by promotion from another target",
				downloadLabel(d), step.Target)
		}
		id, ok := repoIDs[step.Target]
		if !ok {
			return nil, fmt.Errorf("target %q has no catalog row: reconciliation has not run", step.Target)
		}
		out = append(out, ResolvedStep{
			Name: step.Target, RepoID: id, Index: step.Index, DependsOn: step.From,
		})
	}
	return out, nil
}

// Open records a request and one transfer per step, in one transaction.
//
// Returns created=false when the idempotency key already exists, which is the
// expected result of a re-scan and not an error anywhere.
func Open(
	ctx context.Context, tx *sql.Tx, packages *store.Packages,
	req Request,
) (requestID string, created bool, err error) {
	id := requestIDFrom(req.IdempotencyKey)

	requestID, created, err = packages.CreateTransferRequest(ctx, tx, store.TransferRequestRow{
		ID:             id,
		ProductID:      req.ProductID,
		PackageID:      req.PackageID,
		Operation:      "replicate",
		SourceRepoID:   req.SourceRepoID,
		Priority:       req.Priority,
		IdempotencyKey: req.IdempotencyKey,
		RequestedBy:    req.RequestedBy,
		RequestOrigin:  req.Origin,
		AutoRuleName:   req.RuleName,
	})
	if err != nil || !created {
		return requestID, created, err
	}

	// The IDs are assigned before any insert, because a step has to name the
	// transfer it waits for and that transfer is a row we are about to create.
	ids := make(map[string]string, len(req.Steps))
	for _, s := range req.Steps {
		ids[s.Name] = uuid.NewString()
	}
	for _, s := range req.Steps {
		if _, err := packages.CreateTransfer(ctx, tx, store.TransferRow{
			ID:           ids[s.Name],
			RequestID:    requestID,
			PackageID:    req.PackageID,
			SourceRepoID: req.SourceRepoID,
			TargetRepoID: s.RepoID,
			Priority:     req.Priority,
			StepIndex:    s.Index,
			DependsOn:    ids[s.DependsOn],
		}); err != nil {
			return requestID, false, err
		}
	}

	detail, _ := json.Marshal(map[string]any{
		"download": req.DownloadName, "rule": req.RuleName, "tag": req.Tag,
		"chain": Names(req.Steps), "priority": req.Priority,
		"trigger": string(req.Trigger),
	})
	if err := packages.InsertAudit(ctx, tx, store.AuditRow{
		EventType:   "DownloadRunRequested",
		Actor:       req.RequestedBy,
		ActorKind:   actorKindOf(req.Trigger),
		ProductName: req.ProductName,
		SubjectKind: "transfer_request",
		SubjectID:   requestID,
		Detail:      string(detail),
	}); err != nil {
		return requestID, false, err
	}
	return requestID, true, nil
}

// Request is everything Open needs.
type Request struct {
	ProductID    int64
	ProductName  string
	PackageID    int64
	SourceRepoID int64
	Tag          string

	// DownloadName is which configured download ran, empty for the default.
	DownloadName string
	// RuleName is the auto-download rule that triggered it, empty for a
	// manual download. The two are separate fields because they answer
	// different questions: what ran, and what asked for it.
	RuleName string
	Trigger  Trigger
	Origin   string
	// RequestedBy is the actor: a rule name for a discovery run, a person for
	// a manual one.
	RequestedBy string

	Priority       int
	IdempotencyKey string
	Steps          []ResolvedStep
}

func actorKindOf(t Trigger) string {
	if t == TriggerManual {
		return "user"
	}
	return "auto_rule"
}

// Names renders a chain for a log line or an audit record.
func Names(steps []ResolvedStep) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.Name)
	}
	return out
}

// RepoIDs is the catalog rows a chain touches, for the idempotency key.
func RepoIDs(steps []ResolvedStep) []int64 {
	out := make([]int64, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.RepoID)
	}
	return out
}

// Revision fingerprints a DOWNLOAD's resolved fields.
//
// It exists so that editing a download creates a new run rather than being
// swallowed by the old one's idempotency key. The failure otherwise is
// somebody tightening verify.policy from warn to enforce, re-running, getting
// "already exists", and concluding the stricter policy was applied when
// nothing ran at all.
//
// It covers the download and NOT the rule that triggered it, because the run
// is the download: two rules pointing at the same download produce the same
// work, and a revision that included the rule's pattern would make them
// different for no reason a reader could act on.
func Revision(d product.Download) string {
	// Built from an explicit ordered shape rather than from the struct, so
	// adding a field to Download does not silently invalidate every stored
	// revision in the estate on the next deploy.
	parts := []string{
		"targets=" + strings.Join(sorted(d.Targets), ","),
		"priority=" + fmt.Sprint(d.EffectivePriority()),
		"before=" + boolPtr(d.VerifiesBefore()),
		"after=" + boolPtr(d.VerifiesAfter()),
		"policy=" + string(d.VerificationPolicy()),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])[:16]
}

func sorted(v []string) []string {
	out := append([]string(nil), v...)
	sort.Strings(out)
	return out
}

func boolPtr(b *bool) string {
	switch {
	case b == nil:
		return "inherit"
	case *b:
		return "true"
	default:
		return "false"
	}
}

// requestIDFrom derives a stable request ID from the idempotency key.
//
// Stable rather than random: a retry that races the unique constraint would
// otherwise insert with a different primary key and rely on the constraint to
// reject it. Deriving the ID means the two attempts are byte-identical.
func requestIDFrom(key string) string {
	if len(key) < 32 {
		return key
	}
	return "req_" + key[:32]
}

// KeyFor is the idempotency key of one package through one download.
//
// It covers the DERIVED chain, so a download naming only the tail and one
// naming every hop key identically - they are the same work. And it covers the
// download's revision, so an edit opens a new run rather than being swallowed
// by the old key.
//
// The rule is NOT in it. Two rules that trigger the same download for the same
// package are asking for one piece of work, and keying them apart would move
// the bytes twice.
func KeyFor(pkg store.PackageRow, d product.Download, steps []ResolvedStep) string {
	parts := []string{
		"replicate",
		fmt.Sprint(pkg.ID),
		fmt.Sprint(pkg.SourceRepoID),
		fmt.Sprint(RepoIDs(steps)),
		Revision(d),
		fmt.Sprint(d.EffectivePriority()),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}
