// Package promote is the plugin registry for NATIVE promotion.
//
// See docs/design/22-promotion.md.
//
// # What this is not
//
// It is not "promotion". A promotion is already a transfer whose origin is a
// TargetRepository ([01] §3.4), it already runs on the ordinary engine, and
// that stays true: nothing here is required for a promotion to work, and an
// estate with no plugin registered promotes exactly as it did before this
// package existed.
//
// What this adds is the case where our engine is the WRONG WAY to do it. Lab
// and production are routinely two repositories on one Artifactory, and JFrog
// can relocate content between them server-side, in one call, with no bytes
// crossing the wire and no manifest walk at all. Doing that through the
// planner - walking the tree, opening a transfer, leasing jobs and asking the
// registry to mount every blob one at a time - reaches the same end state by
// several thousand round trips.
//
// # The shape, and why it is a registry rather than an if
//
// The obvious implementation is a branch in the expander: "if both ends are
// JFrog on one host, call JFrog". That branch is correct exactly once. Quay
// has its own copy, ACR has import, a site with a shared filesystem behind two
// registries has something cheaper than either, and every one of them is the
// same shape - a hop somebody else can do better than we can. Each would add a
// clause to that branch, and the branch is in the expander, which has no
// business knowing what Artifactory is.
//
// So a promoter is a PLUGIN, registered by name, exactly like a registry
// backend (internal/registry/factory.go). Adding one is a directory and one
// line in a composition root. Nothing in internal/transfer, internal/queue or
// internal/store learns a vendor's name.
//
// # Two calls, and the split is load-bearing
//
//   - Claim reads CONFIGURATION and answers whether this hop is the plugin's.
//     It is asked on every promotion, so it must not touch the network.
//   - Promote does the work, and is the only half that needs a credential.
//
// A plugin that answered both in one call would have to be constructed - and
// therefore to resolve credentials and build a transport - before it could say
// "not mine", which would make a hop between two Quay repositories fail on a
// missing JFrog password.
//
// # A refusal is a SENTENCE, never a silence
//
// Verdict carries a Reason whether it claimed or not, and every caller shows
// it. "JFrog could not promote this" is useless; "lab is on
// artifacts.example.com and production is on dr.example.com, so JFrog cannot
// relocate between them - the bytes will be copied instead" is the whole
// diagnosis. An operator who configured two hosts by mistake finds out from
// the promotion dialog rather than from a 45 GB transfer.
package promote

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
)

// ---------------------------------------------------------------------------
// The hop
// ---------------------------------------------------------------------------

// Endpoint is one end of a hop, as a promoter sees it.
//
// Deliberately free of configuration types: this package must not import
// internal/product, for the same reason registry.ClientConfig does not - a
// promoter speaks to a registry and has no business knowing about ConfigMaps,
// secret references or reload semantics. The translation happens in the caller.
type Endpoint struct {
	// Name is the configured target name, used in messages and nowhere else.
	Name string
	// Registry is the host.
	Registry string
	// Repository is the target's configured BASE path. Everything this target
	// holds sits beneath it.
	Repository string
	// RegistryType is the configured backend: generic, artifactory, jfrog,
	// quay, acr.
	RegistryType string

	// Options are the promoter-specific settings this endpoint declares, by
	// key. Untyped on purpose: a plugin's own configuration must not require a
	// field on a shared struct, or adding the second plugin means editing the
	// first plugin's types.
	Options map[string]string
}

// Name is one thing that must be reachable at the destination when the hop is
// done: a repository path and a tag.
//
// NAMES rather than blobs, and that is the whole reason a native promotion is
// cheap. Our engine moves content, which is a tree of manifests and blobs; a
// registry relocating within itself already holds every blob and only has to
// publish the names. So a hop is expressed as the set of names, and the
// promoter's progress is counted in them.
//
// Repository is relative to each end's own base path, so the same value
// re-bases under either. That is what stops production ending up holding lab's
// prefix nested inside its own.
type Name struct {
	// Repository is the path BENEATH the endpoint's base. Empty means the
	// base itself.
	Repository string
	Tag        string
	// Digest is what the tag must resolve to at the destination. It is what
	// makes a promotion verifiable rather than merely reported.
	Digest string
}

// Hop is one package moving from one target to another.
type Hop struct {
	Product string
	// Package is the tag a person asked for, used in messages.
	Package string
	// ManifestDigest is the root manifest's digest.
	ManifestDigest string

	Origin      Endpoint
	Destination Endpoint

	// Names is every name THIS CALL must publish, root first. Never empty: a
	// hop with nothing to publish is not a hop, and a promoter handed one
	// should refuse rather than report success.
	//
	// A caller MAY narrow this to fewer names than the whole release - the
	// runner does, one at a time, so a promotion interrupted half way is
	// resumable at the exact name. A promoter must publish only these.
	Names []Name

	// AllNames is every name the OVERALL promotion will publish across every
	// call, root first - equal to Names when a caller does not narrow it.
	//
	// Read-only context, never a publish list. It exists because a promoter
	// may find a native shortcut that moves more than Names asks for (JFrog's
	// docker promote endpoint sometimes only accepts "every tag a repository
	// holds", not one) - and such a shortcut is safe only when everything it
	// would additionally move is something the release already owns. Names
	// alone cannot answer that once a caller has narrowed it to one entry;
	// AllNames is what still can.
	AllNames []Name
}

// Verdict is a promoter's answer to "is this hop yours".
type Verdict struct {
	// Promoter is the plugin that answered.
	Promoter string
	Claimed  bool
	// Reason says WHY, claimed or not, in words an operator can act on. Always
	// populated - see the package comment.
	Reason string
}

// Outcome is what a promoter did.
type Outcome struct {
	Promoter string
	// Promoted and Skipped count NAMES. Skipped is not a failure: a name the
	// destination already resolves to the right digest needed no call, and on
	// a re-promotion that is most of them.
	Promoted int
	Skipped  int
	// Detail is one line for the transfer record, e.g. which JFrog repository
	// keys were involved.
	Detail string
}

// Promoter carries out one class of hop natively.
type Promoter interface {
	// Name is the plugin's registered name. Recorded on the promotion row, so
	// it does not change once shipped.
	Name() string

	// Claim reports whether this promoter carries this hop. Configuration
	// only: no network call, no credential.
	Claim(h Hop) Verdict

	// Promote carries the hop out.
	//
	// It must be IDEMPOTENT. A Coordinator killed half way through leaves a
	// promotion to be picked up again, and the second run has to be able to
	// finish what the first started rather than fail on the names that already
	// arrived.
	Promote(ctx context.Context, h Hop) (Outcome, error)
}

// ---------------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------------

// Config is what a promoter needs to reach the two ends of a hop.
//
// The two ClientConfigs are the SAME ones a transfer would get - same
// credential, same CA bundle, same proxy, same timeouts - resolved by the
// caller through internal/regclient. That is not tidiness: a promotion path
// that resolved its own would be reaching a host by a different route from the
// one that replicates to it, and the day the two disagree is the day promotion
// fails against a registry every transfer reaches perfectly.
type Config struct {
	Origin      Endpoint
	Destination Endpoint

	OriginClient      registry.ClientConfig
	DestinationClient registry.ClientConfig

	Logger *slog.Logger
}

// Constructor builds a promoter for one hop.
//
// It must be CHEAP and must not fail for a hop that is not this plugin's: the
// chain constructs every registered promoter in order to ask Claim, so a
// constructor that validated credentials would make an unrelated hop fail on a
// missing password. Validation belongs in Claim (for configuration) and in
// Promote (for anything needing the network).
type Constructor func(Config) (Promoter, error)

var (
	mu           sync.RWMutex
	constructors = map[string]Constructor{}
)

// Register makes a promoter available to Resolve.
//
// Called from each plugin's init, so importing the plugin package is what
// enables it - the same arrangement registry backends use, and for the same
// reason: this package does not import jfrog, so a vendor promoter can be
// added without touching the abstraction.
func Register(name string, c Constructor) {
	mu.Lock()
	defer mu.Unlock()
	if _, exists := constructors[name]; exists {
		// Two packages claiming one name, with the winner decided by import
		// order. Loud is correct.
		panic("promote: promoter " + name + " registered twice")
	}
	constructors[name] = c
}

// Promoters lists the registered names, sorted.
func Promoters() []string {
	mu.RLock()
	defer mu.RUnlock()

	out := make([]string, 0, len(constructors))
	for name := range constructors {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Resolution
// ---------------------------------------------------------------------------

// Resolution is the answer to "who, if anyone, carries this hop".
type Resolution struct {
	// Promoter is nil when nothing claimed, which is ORDINARY: it means the
	// bytes are copied, which is what every hop did before this package
	// existed.
	Promoter Promoter
	Verdict  Verdict

	// Declined is every plugin that did not claim, with its reason. Carried so
	// a dry run and the promotion dialog can say why the fast path is not
	// available rather than merely that it is not.
	Declined []Verdict
}

// Resolve asks every registered promoter and returns the one that claimed.
//
// Asked in NAME ORDER rather than registration order, so the answer does not
// depend on which composition root imported what first - the single most
// annoying class of bug a plugin registry can have, because it reproduces
// everywhere except where it was reported.
//
// Two claims is a CONFIGURATION ERROR and is refused rather than resolved by
// precedence. A precedence rule would silently pick one, and the operator
// whose hop went the slower way would have nothing to read; refusing names
// both plugins and makes somebody decide. In practice it cannot happen without
// a deliberately overlapping plugin, which is exactly the case worth catching.
func Resolve(cfg Config, h Hop) (Resolution, error) {
	if len(h.Names) == 0 {
		return Resolution{}, fmt.Errorf(
			"promotion of %s has no names to publish: nothing to promote", h.Package)
	}

	mu.RLock()
	names := make([]string, 0, len(constructors))
	byName := make(map[string]Constructor, len(constructors))
	for name, c := range constructors {
		names = append(names, name)
		byName[name] = c
	}
	mu.RUnlock()
	sort.Strings(names)

	var out Resolution
	var claimedBy []string

	for _, name := range names {
		p, err := byName[name](cfg)
		if err != nil {
			return Resolution{}, fmt.Errorf("build promoter %q: %w", name, err)
		}
		v := p.Claim(h)
		v.Promoter = name

		if !v.Claimed {
			out.Declined = append(out.Declined, v)
			continue
		}
		claimedBy = append(claimedBy, name)
		if out.Promoter == nil {
			out.Promoter, out.Verdict = p, v
		}
	}

	if len(claimedBy) > 1 {
		return Resolution{}, fmt.Errorf(
			"%d promoters claim %s -> %s: %s"+
				"\nexactly one plugin may carry a hop; narrow one of them",
			len(claimedBy), h.Origin.Name, h.Destination.Name, strings.Join(claimedBy, ", "))
	}
	return out, nil
}

// DeclinedReason renders the declines as one line for a person.
//
// Empty when nothing declined, which with no plugins registered is the normal
// answer and reads correctly as "there is no fast path here" rather than as a
// fault.
//
// ONE decline is rendered without the plugin's name. The reason already says
// what happened - "lab is on eu.jfrog.io and production is on us.jfrog.io" -
// and prefixing it with `jfrog: ` in a deployment where jfrog is the only
// promoter adds a word the reader has to parse before the sentence they came
// for. The name comes back the moment there is a second plugin to tell apart,
// which is exactly when it starts carrying information.
func (r Resolution) DeclinedReason() string {
	switch len(r.Declined) {
	case 0:
		return ""
	case 1:
		return r.Declined[0].Reason
	}
	parts := make([]string, 0, len(r.Declined))
	for _, d := range r.Declined {
		parts = append(parts, d.Promoter+": "+d.Reason)
	}
	return strings.Join(parts, "; ")
}
