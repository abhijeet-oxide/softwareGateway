package store

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Why a transfer is failing, in the terms somebody can act on.
//
// # The problem this exists for
//
// A bundle has five hundred manifests. When the destination rejects them it
// rejects all five hundred, for the same reason, and the job listing renders
// that as five hundred rows of wrapped repository paths and truncated error
// text. Every row is different — a different digest, a different destination —
// and every row says the same thing. There is no width of terminal at which
// that is readable, and no amount of scrolling that turns it into a diagnosis.
//
// What an operator needs is the opposite shape: the distinct CAUSES, how many
// jobs each one is holding, one example to go and look at, and whether
// retrying could possibly help. Usually there are two or three.
//
// # Why the grouping is done here and not in SQL
//
// GROUP BY last_error would produce one group per job, because the message
// contains the digest and the destination path — the two things that differ
// between otherwise identical failures. The normalisation has to happen
// against the row, where the digest and the repository are known exactly and
// can be substituted rather than guessed at.
//
// The row count is bounded by what is actually failing, and a transfer with
// more than a few thousand failing jobs has one cause, not a paging problem.

// FailureGroup is one distinct cause, and everything failing because of it.
type FailureGroup struct {
	// Class is the retry classification: auth, unsupported, timeout and so on.
	// It is what decides whether a retry could help, so it leads.
	Class string
	// Message is the failure with the per-job parts replaced — one sentence
	// standing for every job in the group.
	Message string

	// Failed is how many have exhausted their attempts. Retrying is how many
	// are still going to try again on their own. The distinction is the
	// difference between "act now" and "wait".
	Failed   int
	Retrying int

	// Kinds are the job kinds affected — "blob", "manifest", or both.
	Kinds []string
	// Waves are the waves affected, lowest first.
	Waves []int

	// One example, so the reader has something concrete to go and inspect.
	ExampleJobID      int64
	ExampleDigest     string
	ExampleRepository string
	// ExampleError is that example's message VERBATIM, digest and path intact.
	// The normalised message says what went wrong; this says where.
	ExampleError string

	// Retryable reports whether a retry could plausibly succeed. Derived from
	// the class rather than from the message, because the class is the thing
	// the retry policy actually keys off.
	Retryable bool
}

// FailureGroups summarises everything failing in one transfer.
//
// Ordered by blast radius: the cause holding the most jobs first, because that
// is the one worth fixing.
func (p *Packages) FailureGroups(ctx context.Context, transferID string) ([]FailureGroup, error) {
	rows, err := p.db.QueryContext(ctx, p.dialect.Rewrite(`
		SELECT id, kind, digest, wave, state,
		       COALESCE(last_error_class, ''), COALESCE(last_error, ''),
		       COALESCE(target_repository, '')
		  FROM jobs
		 WHERE transfer_id = ?
		   AND last_error IS NOT NULL AND last_error <> ''
		   AND state IN ('failed','pending','leased','blocked')
		 ORDER BY id`), transferID)
	if err != nil {
		return nil, fmt.Errorf("read the failures of transfer %s: %w", transferID, err)
	}
	defer func() { _ = rows.Close() }()

	byCause := map[string]*FailureGroup{}
	var order []string

	for rows.Next() {
		var (
			id                       int64
			kind, digest, state      string
			wave                     int
			class, message, repoPath string
		)
		if err := rows.Scan(&id, &kind, &digest, &wave, &state,
			&class, &message, &repoPath); err != nil {
			return nil, fmt.Errorf("scan a failure of transfer %s: %w", transferID, err)
		}

		normalised := normaliseFailure(message, digest, repoPath)
		key := class + "\x00" + normalised

		g, seen := byCause[key]
		if !seen {
			g = &FailureGroup{
				Class:             class,
				Message:           normalised,
				ExampleJobID:      id,
				ExampleDigest:     digest,
				ExampleRepository: repoPath,
				ExampleError:      message,
				Retryable:         classIsRetryable(class),
			}
			byCause[key] = g
			order = append(order, key)
		}

		if state == "failed" {
			g.Failed++
		} else {
			// `pending` with an error on it is a job waiting out its backoff,
			// which is a transfer that is still trying rather than one that has
			// given up. Counting the two together is what made a stalled
			// transfer indistinguishable from a working one.
			g.Retrying++
		}
		g.Kinds = addOnce(g.Kinds, kind)
		g.Waves = addWave(g.Waves, wave)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]FailureGroup, 0, len(order))
	for _, key := range order {
		out = append(out, *byCause[key])
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Failed+out[i].Retrying > out[j].Failed+out[j].Retrying
	})
	return out, nil
}

// digestPattern matches a digest in every form an error message carries it.
//
// Three, and the third is the one that mattered: `sha256:<hex>` as the OCI
// spec writes it, a bare hex run as the shortened forms use, and
// `sha256__<hex>` — which is how Artifactory names a blob in its own storage
// layout, and which the bare-hex branch cannot catch because `_` is a word
// character and there is therefore no `\b` in front of the hex. That one
// omission left every rejection in a five-hundred-manifest bundle in a group of
// its own, which is the exact failure this file exists to prevent.
var digestPattern = regexp.MustCompile(
	`(?:sha256|sha512)[:_]{1,2}[0-9a-f]{6,}|\b[0-9a-f]{12,}\b`)

// normaliseFailure turns one job's message into the sentence that stands for
// every job failing the same way.
//
// The job's OWN digest and destination path are substituted first, because
// those are known exactly rather than guessed at. The patterns afterwards are
// the backstop for what a message names that is not the job's own — a manifest
// push rejected for a missing LAYER names the layer, and that layer differs per
// manifest while the cause does not.
func normaliseFailure(message, digest, repository string) string {
	out := strings.TrimSpace(message)
	if out == "" {
		return ""
	}

	// Every form, longest first, and NOT stopping at the first match: one
	// message can name the destination twice in two different forms. The
	// production rejection does exactly that — the fully qualified path in the
	// request line, and Artifactory's key-stripped version in its description —
	// so stopping early normalised the first and left the second to fragment
	// the grouping. Longest-first means a longer form is already replaced
	// before a shorter one could match inside it.
	for _, form := range repositoryForms(repository) {
		out = strings.ReplaceAll(out, form, "<repository>")
	}
	if digest != "" {
		out = strings.ReplaceAll(out, digest, "<digest>")
		if _, hex, found := strings.Cut(digest, ":"); found && len(hex) >= 12 {
			out = strings.ReplaceAll(out, hex[:12], "<digest>")
		}
	}
	return digestPattern.ReplaceAllString(out, "<digest>")
}

// repositoryForms lists the ways a registry might echo a destination path,
// longest first.
//
// A registry does not necessarily quote the path we sent it. Artifactory strips
// its own repository key and reports what is left:
//
//	we sent      apm0014228-oci-stage/orbs/cfx-5000-product/nokia-ims-sas
//	it said      cfx-5000-product/nokia-ims-sas
//
// So the exact string is not present in the message and a whole-string
// substitution finds nothing — leaving the path in the grouping key and one
// group per repository. Trying successively shorter suffixes finds the form the
// registry actually used, and longest-first means the most specific match wins
// rather than the last path segment swallowing a longer one.
func repositoryForms(repository string) []string {
	repository = strings.Trim(strings.TrimSpace(repository), "/")
	if repository == "" {
		return nil
	}

	segments := strings.Split(repository, "/")
	out := make([]string, 0, len(segments))
	for i := range segments {
		form := strings.Join(segments[i:], "/")
		// A single segment shorter than this is too generic to substitute
		// safely: replacing "ncm" everywhere would corrupt the message rather
		// than normalise it.
		if len(form) < 4 {
			break
		}
		out = append(out, form)
	}
	return out
}

// classIsRetryable mirrors registry.Retryable without importing it.
//
// The store must not depend on the registry package — it holds rows, not
// protocol knowledge — and the class strings are a stable vocabulary written
// down in docs/design/10 §6 rather than an implementation detail. An unknown
// class reads as retryable, which is the same benefit of the doubt the retry
// policy itself gives.
func classIsRetryable(class string) bool {
	switch class {
	case "auth", "unsupported", "not_found", "configuration":
		return false
	default:
		return true
	}
}

func addOnce(list []string, v string) []string {
	if v == "" {
		return list
	}
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}

func addWave(list []int, v int) []int {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	list = append(list, v)
	sort.Ints(list)
	return list
}
