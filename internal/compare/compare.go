// Package compare answers the question a transfer cannot: is the destination
// actually a faithful copy of the source?
//
// # Why a transfer's own report is not the answer
//
// A transfer reports what it DID. It says 2489 jobs succeeded and 63.7 GiB
// moved, and every one of those numbers can be true while the destination is
// wrong — a tag that failed to apply, a manifest a registry accepted and
// garbage-collected, content deleted by hand afterwards, or a bundle assembled
// by two transfers of which one was stopped. Each of those was seen in practice
// during this system's first real runs.
//
// So this asks the destination, artifact by artifact, and compares its answers
// against the source's own structure. Nothing here trusts a record: the source
// tree supplies what SHOULD be there, and every fact about the target comes
// from the target registry in this call.
//
// # What it compares, and what it deliberately does not
//
// It compares the PACKAGE — everything the source published under one release,
// in every place the layout says it lands. It does not enumerate the
// destination looking for extra content, and that is a scope decision rather
// than an omission: a destination repository legitimately holds every other
// version of the same component, so "present at the target and not at the
// source" over a whole repository would report every previous release as a
// discrepancy. Where the question IS well defined — a tag this package claims,
// pointing at something else — it is reported, and that is the case that
// actually catches a bad copy.
package compare

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/transfer"
)

// ClientFactory builds a repository handle for one path on a registry.
type ClientFactory func(repository string) (registry.Repository, error)

// Options is what to compare.
type Options struct {
	// Tree is the source's own structure, already expanded. What SHOULD be at
	// the destination is derived from it and from nothing else.
	Tree store.ExpandedTree
	// SourceRepository is where the package is read from.
	SourceRepository string
	// TargetBasePath is the destination target's configured `repository` — a
	// PREFIX, beneath which the source's structure is reproduced.
	TargetBasePath string
	// RootTags are the tags the package's root answers to.
	RootTags []string
	// Concurrency bounds how many artifacts are checked at once.
	Concurrency int
}

// Side is one end's account of an artifact.
type Side struct {
	// Present is whether the registry served it. False with a Digest set means
	// we expected it there and it was not.
	Present    bool
	Repository string
	Digest     string
	Size       int64
	// Tags is what this side has it under. On the source side these are the
	// names the vendor gave it; on the target side, the ones that actually
	// resolve to this digest.
	Tags []string
	// Error is why the target could not be asked, when it could not.
	Error string
}

// Row is one artifact in one place.
//
// Per SITE rather than per artifact, because a bundle publishes each component
// twice — once inside the bundle so the index still resolves, and once under
// the component's own name — and those are two places that can independently be
// wrong.
type Row struct {
	// Type is what the artifact IS, in the words somebody uses about it:
	// image, chart, index, file, signature.
	Type string
	// Name is what to call this row: the component's own reference where it has
	// one, the destination path otherwise.
	Name   string
	Source Side
	Target Side
	// Differences is empty when the two sides agree. Each entry is one concrete
	// disagreement, stated as a fact.
	Differences []string
}

// Match reports that the two sides agree completely.
func (r Row) Match() bool { return len(r.Differences) == 0 }

// Report is the whole comparison.
type Report struct {
	Rows []Row
	// Matched and Mismatched partition Rows.
	Matched    int
	Mismatched int
}

// Run compares a package's source structure against a destination registry.
func Run(ctx context.Context, target ClientFactory, opts Options) (Report, error) {
	if len(opts.Tree.Artifacts) == 0 {
		return Report{}, errors.New("compare: the package has not been expanded, " +
			"so there is nothing to compare against")
	}

	layout := transfer.ResolveLayout(opts.Tree, transfer.LayoutOptions{
		BasePath:         opts.TargetBasePath,
		SourceRepository: opts.SourceRepository,
		RootTags:         opts.RootTags,
	})

	rows := plannedRows(opts, layout)
	checkTargets(ctx, target, rows, opts.Concurrency)

	// Mismatches first, then matches: the reason somebody runs this is to find
	// what is wrong, and making them scroll past two thousand agreeing rows to
	// reach three disagreeing ones defeats it.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Match() != rows[j].Match() {
			return !rows[i].Match()
		}
		if rows[i].Type != rows[j].Type {
			return rows[i].Type < rows[j].Type
		}
		return rows[i].Name < rows[j].Name
	})

	out := Report{Rows: rows}
	for _, r := range rows {
		if r.Match() {
			out.Matched++
			continue
		}
		out.Mismatched++
	}
	return out, nil
}

// plannedRows turns the source's structure into one row per artifact per site.
func plannedRows(opts Options, layout map[string]transfer.Placement) []Row {
	var rows []Row

	for _, a := range opts.Tree.Artifacts {
		place, ok := layout[a.Row.Digest]
		if !ok {
			continue
		}
		for _, site := range place.Sites {
			rows = append(rows, Row{
				Type: classify(a.Row),
				Name: nameFor(site),
				Source: Side{
					Present:    true,
					Repository: opts.SourceRepository,
					Digest:     a.Row.Digest,
					Size:       a.Row.SizeBytes,
					Tags:       append([]string(nil), site.Tags...),
				},
				Target: Side{Repository: site.Repository, Digest: a.Row.Digest},
			})
		}
	}
	return rows
}

// checkTargets asks the destination about every row, in parallel.
//
// Bounded, and bounded by the caller's configured concurrency rather than by a
// number chosen here: this opens the same connections a transfer does, against
// the same registry, and a comparison that ignored the ceiling an operator set
// would be the one request that overruns it.
func checkTargets(ctx context.Context, target ClientFactory, rows []Row, concurrency int) {
	if concurrency <= 0 {
		concurrency = 4
	}

	// One client per destination repository, not per row: a bundle's components
	// share a handful of paths, and a handle per row would mean a token
	// exchange per row.
	var (
		mu      sync.Mutex
		clients = map[string]registry.Repository{}
	)
	clientFor := func(path string) (registry.Repository, error) {
		mu.Lock()
		defer mu.Unlock()
		if c, ok := clients[path]; ok {
			return c, nil
		}
		c, err := target(path)
		if err != nil {
			return nil, err
		}
		clients[path] = c
		return c, nil
	}

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i := range rows {
		wg.Add(1)
		go func(r *Row) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			client, err := clientFor(r.Target.Repository)
			if err != nil {
				r.Target.Error = err.Error()
				r.Differences = []string{"the destination could not be reached: " + err.Error()}
				return
			}
			inspect(ctx, client, r)
		}(&rows[i])
	}
	wg.Wait()
}

// inspect fills in one row's target side and states every disagreement.
func inspect(ctx context.Context, client registry.Repository, r *Row) {
	desc, raw, err := client.FetchManifest(ctx, r.Source.Digest)
	switch {
	case err == nil:
		r.Target.Present = true
		r.Target.Digest = r.Source.Digest
		// The bytes we were served, not the size the source claimed. A
		// destination that returns a different length for the same digest is
		// impossible under content addressing, so measuring it is how we would
		// find out that it happened.
		r.Target.Size = int64(len(raw))
		if r.Target.Size == 0 && desc.Size > 0 {
			r.Target.Size = desc.Size
		}
	case errors.Is(err, registry.ErrNotFound):
		r.Differences = append(r.Differences, "the content is not at the destination")
	default:
		r.Target.Error = err.Error()
		r.Differences = append(r.Differences,
			"the destination could not be asked: "+summarise(err))
		return
	}

	// Every tag the source says this should answer to, resolved at the
	// destination. A tag is the only part of a copy that is not
	// content-addressed, so it is the only part that can be wrong while every
	// digest agrees — and it is exactly what failed in practice.
	for _, tag := range r.Source.Tags {
		got, err := client.ResolveTag(ctx, tag)
		switch {
		case err == nil && string(got.Digest) == r.Source.Digest:
			r.Target.Tags = append(r.Target.Tags, tag)
		case err == nil:
			r.Target.Tags = append(r.Target.Tags, tag)
			r.Differences = append(r.Differences, fmt.Sprintf(
				"%s points at %s, not %s", tag, short(string(got.Digest)), short(r.Source.Digest)))
		case errors.Is(err, registry.ErrNotFound):
			r.Differences = append(r.Differences, tag+" is not at the destination")
		default:
			r.Differences = append(r.Differences,
				tag+" could not be resolved: "+summarise(err))
		}
	}

	if r.Target.Present && r.Source.Size > 0 && r.Target.Size > 0 &&
		r.Target.Size != r.Source.Size {
		r.Differences = append(r.Differences, fmt.Sprintf(
			"the manifest is %d bytes here and %d bytes at the source",
			r.Target.Size, r.Source.Size))
	}
}

// nameFor is what to call a row.
//
// The destination path plus the tag where there is one, because that is a
// reference somebody can go and pull. Without a tag the path alone is the
// honest answer: the artifact is addressed by digest there, and inventing a
// reference that does not resolve would be worse than saying less.
func nameFor(site transfer.Site) string {
	if len(site.Tags) == 0 {
		return site.Repository
	}
	tags := append([]string(nil), site.Tags...)
	sort.Strings(tags)
	return site.Repository + ":" + strings.Join(tags, ",")
}

// classify says what an artifact IS, in the words somebody uses about it.
//
// Media type first, because that is what the specification defines and what the
// registry actually serves. `artifactType` second, for the OCI 1.1 artifacts
// that use it. Neither is guessed at from a name: a repository called `charts/`
// holding an image is a mislabelled repository, not a chart.
func classify(row store.ArtifactRow) string {
	switch {
	case registry.IsIndex(row.MediaType):
		return "index"
	case strings.Contains(row.ArtifactType, "helm"),
		strings.Contains(row.MediaType, "helm"):
		return "chart"
	case strings.Contains(row.ArtifactType, "signature"),
		strings.Contains(row.ArtifactType, "sig"):
		return "signature"
	case strings.Contains(row.ArtifactType, "generic"):
		return "file"
	case row.MediaType == "":
		return "artifact"
	default:
		return "image"
	}
}

// summarise trims a registry error to the part that says what went wrong.
func summarise(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i > 0 && len(msg)-i < 80 {
		return strings.TrimSpace(msg[i+2:])
	}
	return msg
}

func short(digest string) string {
	algo, hex, ok := strings.Cut(digest, ":")
	if !ok || len(hex) < 12 {
		return digest
	}
	return algo + ":" + hex[:12]
}
