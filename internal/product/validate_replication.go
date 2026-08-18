package product

import (
	"fmt"
	"strings"
	"time"
)

// validateReplication checks the `replication` block on every target, plus the
// cross-target relationships only the whole document can see: the mirror chain
// is acyclic, a proxy target is nobody's destination, and a mirror target is
// nobody's promotion destination.
//
// The organising principle for what is an error here rather than a warning:
// a mode mistake is silent in production. A mirror with a regex where a glob
// belongs syncs successfully and delivers nothing; a proxy named as a transfer
// destination fails on the day a rule first matches, months after the edit.
// Both are cheap to reject at merge and expensive to discover.
func (p *Product) validateReplication(resolver *SecretResolver) Errors {
	var errs Errors

	declared := make(map[string]Target, len(p.Spec.Targets))
	for _, t := range p.Spec.Targets {
		declared[t.Name] = t
	}

	for i, t := range p.Spec.Targets {
		path := fmt.Sprintf("spec.targets[%d]", i)
		errs = append(errs, validateTargetReplication(path, t, declared, resolver)...)
		errs = append(errs, validateQuaySettings(path, t, resolver)...)
	}

	errs = append(errs, p.validateMirrorChain(declared)...)
	errs = append(errs, p.validatePromotionModes()...)
	errs = append(errs, p.validateRuleModes(declared)...)
	return errs
}

func validateTargetReplication(path string, t Target, declared map[string]Target, resolver *SecretResolver) Errors {
	var errs Errors
	if t.Replication == nil {
		return nil
	}
	rpath := path + ".replication"
	r := t.Replication

	switch r.Mode {
	case "", ReplicationCopy, ReplicationMirror, ReplicationProxy:
	default:
		errs = append(errs, Error{rpath + ".mode",
			fmt.Sprintf("%q is not one of copy, mirror, proxy", r.Mode), ""})
		return errs
	}

	mode := t.ReplicationMode()

	// Neither mechanism exists outside Quay, and a block that is silently
	// ignored is worse than an error: the operator believes delegation is
	// configured, and our workers keep pushing.
	if mode != ReplicationCopy && t.Type != RegistryQuay {
		errs = append(errs, Error{rpath + ".mode",
			fmt.Sprintf("%q requires type: quay", mode),
			"repository mirroring and proxy cache are Red Hat Quay features; other registries have no equivalent"})
	}

	// A block for a mode that is not selected would look configured and do
	// nothing.
	if r.Mirror != nil && mode != ReplicationMirror {
		errs = append(errs, Error{rpath + ".mirror",
			fmt.Sprintf("set, but mode is %q", mode),
			"remove the block, or set mode: mirror"})
	}
	if r.Proxy != nil && mode != ReplicationProxy {
		errs = append(errs, Error{rpath + ".proxy",
			fmt.Sprintf("set, but mode is %q", mode),
			"remove the block, or set mode: proxy"})
	}

	switch mode {
	case ReplicationMirror:
		if r.Mirror == nil {
			errs = append(errs, Error{rpath + ".mirror", "required when mode is mirror", ""})
			break
		}
		errs = append(errs, validateMirror(rpath+".mirror", t, r.Mirror, declared, resolver)...)
	case ReplicationProxy:
		if r.Proxy == nil {
			errs = append(errs, Error{rpath + ".proxy", "required when mode is proxy", ""})
			break
		}
		errs = append(errs, validateProxyCache(rpath+".proxy", t, r.Proxy, resolver)...)

		// A proxy organization cannot be pushed to at all, so a target that is
		// reachable as a destination is a contradiction rather than a risk.
		// This must fail at load: it would otherwise fail on the day a rule
		// first matched, which may be months after the edit.
		if t.Default {
			errs = append(errs, Error{path + ".default",
				"a proxy-cache target cannot be the default target",
				"content enters a proxy cache on a pull; nothing can be pushed to it"})
		}
		if t.PromotionOnly {
			errs = append(errs, Error{path + ".promotionOnly",
				"a proxy-cache target cannot be a promotion destination",
				"content enters a proxy cache on a pull; nothing can be pushed to it"})
		}
	}
	return errs
}

func validateMirror(path string, t Target, m *MirrorConfig, declared map[string]Target, resolver *SecretResolver) Errors {
	var errs Errors

	// Exactly one upstream. Both would be two answers to one question; neither
	// leaves Quay with nothing to pull from.
	switch {
	case m.From != "" && m.ExternalReference != "":
		errs = append(errs, Error{path,
			"from and externalReference are mutually exclusive",
			"name a target in this product, or an external `host/path`, not both"})
	case m.From == "" && m.ExternalReference == "":
		errs = append(errs, Error{path,
			"one of from or externalReference is required",
			"Quay pulls from an upstream; it has to be told which"})
	case m.From != "":
		errs = append(errs, validateMirrorFrom(path+".from", t, m.From, declared)...)
	case m.ExternalReference != "":
		errs = append(errs, validateExternalReference(path+".externalReference", m.ExternalReference)...)
	}

	if len(m.Tags) == 0 {
		errs = append(errs, Error{path + ".tags", "at least one tag glob is required",
			"Quay's mirror selects by tag glob; with none, it would select nothing"})
	}
	for i, g := range m.Tags {
		gpath := fmt.Sprintf("%s.tags[%d]", path, i)
		if g == "" {
			errs = append(errs, Error{gpath, "must not be empty", ""})
			continue
		}
		// Quay's field is `tag_glob_csv`, so a comma inside one entry would be
		// split by Quay and mean something other than what it says here. The
		// list is the list; one entry is one glob.
		if strings.Contains(g, ",") {
			errs = append(errs, Error{gpath,
				fmt.Sprintf("%q contains a comma", g),
				"write one glob per list entry; Quay splits this field on commas and would read this as two rules"})
			continue
		}
		if msg := GlobDialectError(g); msg != "" {
			errs = append(errs, Error{gpath, msg,
				"every other pattern in this schema is RE2; this one is not, and there is no faithful translation — `v3.*` matches different things in the two dialects"})
		}
	}

	if m.Robot == "" {
		errs = append(errs, Error{path + ".robot", "required",
			"Quay allows only a robot account to push into a mirrored repository, and there is no default we could invent"})
	} else if org, ok := RobotOrganization(m.Robot); !ok {
		errs = append(errs, Error{path + ".robot",
			fmt.Sprintf("%q is not in `org+name` form", m.Robot), ""})
	} else if want := t.Organization(); want != "" && org != want {
		errs = append(errs, Error{path + ".robot",
			fmt.Sprintf("robot is in organization %q but the target repository is in %q", org, want),
			"a robot can only push within its own organization"})
	}

	if m.Interval != 0 && m.Interval.Duration() < MinMirrorInterval {
		errs = append(errs, Error{path + ".interval",
			fmt.Sprintf("%s is below the %s minimum", m.Interval, MinMirrorInterval), ""})
	}
	if m.SkopeoTimeout != 0 && m.SkopeoTimeout.Duration() > MaxSkopeoTimeout {
		errs = append(errs, Error{path + ".skopeoTimeout",
			fmt.Sprintf("%s exceeds Quay's maximum of %s", m.SkopeoTimeout, MaxSkopeoTimeout), ""})
	}
	if m.StartAt != "" {
		if _, err := time.Parse(time.RFC3339, m.StartAt); err != nil {
			errs = append(errs, Error{path + ".startAt",
				fmt.Sprintf("%q is not an RFC 3339 timestamp", m.StartAt),
				`for example "2026-09-01T02:00:00Z"`})
		}
	}
	errs = append(errs, validateManage(path+".manage", m.Manage)...)
	errs = append(errs, validateCredentialsRef(path+".sourceCredentialsRef", m.SourceCredentialsRef, resolver)...)
	return errs
}

func validateMirrorFrom(path string, t Target, from string, declared map[string]Target) Errors {
	if from == t.Name {
		return Errors{{path, "a target cannot mirror from itself", ""}}
	}
	upstream, ok := declared[from]
	if !ok {
		return Errors{{path, fmt.Sprintf("%q is not a declared target", from),
			"add it under spec.targets, or use externalReference for an upstream this product does not manage"}}
	}
	if !upstream.IsEnabled() {
		return Errors{{path, fmt.Sprintf("%q is disabled", from),
			"Quay would pull from a target nothing is being written to"}}
	}
	if upstream.ReplicationMode() == ReplicationProxy {
		return Errors{{path, fmt.Sprintf("%q is a proxy cache and cannot be a mirror upstream", from),
			"a proxy cache holds only what somebody has already pulled and cannot be enumerated, so a mirror would have nothing to list"}}
	}
	return nil
}

// validateExternalReference rejects the two forms that look right and are not:
// a URL scheme, which Quay does not accept, and a reference carrying a tag or
// digest, which would pin the mirror to one version forever.
func validateExternalReference(path, ref string) Errors {
	var errs Errors
	if strings.Contains(ref, "://") {
		errs = append(errs, Error{path, fmt.Sprintf("%q must not include a scheme", ref),
			"Quay takes `host/path`, for example artifactory.example.com/docker-local/vendor-a"})
	}
	if strings.Contains(ref, "@") {
		errs = append(errs, Error{path, fmt.Sprintf("%q must not include a digest", ref),
			"the mirror selects versions by tag glob, not by pinning one"})
	}
	if last := ref[strings.LastIndex(ref, "/")+1:]; strings.Contains(last, ":") {
		errs = append(errs, Error{path, fmt.Sprintf("%q must not include a tag", ref),
			"the mirror selects versions by tag glob, not by pinning one"})
	}
	if !strings.Contains(ref, "/") {
		errs = append(errs, Error{path, fmt.Sprintf("%q names a host but no path", ref),
			"Quay mirrors one repository, so the reference needs the repository path too"})
	}
	return errs
}

func validateProxyCache(path string, t Target, c *ProxyCacheConfig, resolver *SecretResolver) Errors {
	var errs Errors

	if c.Organization == "" {
		errs = append(errs, Error{path + ".organization", "required",
			"a Quay proxy cache is a property of an organization, not of a repository"})
	} else if org := t.Organization(); org != "" && org != c.Organization {
		errs = append(errs, Error{path + ".organization",
			fmt.Sprintf("%q disagrees with the target repository, which is in %q", c.Organization, org),
			"the repository path IS the proxy layout: <proxy-org>/<upstream-path>"})
	}

	if c.UpstreamRegistry == "" {
		errs = append(errs, Error{path + ".upstreamRegistry", "required", ""})
	} else {
		if strings.Contains(c.UpstreamRegistry, "://") {
			errs = append(errs, Error{path + ".upstreamRegistry",
				fmt.Sprintf("%q must not include a scheme", c.UpstreamRegistry), ""})
		}
		if strings.Contains(c.UpstreamRegistry, "@") {
			errs = append(errs, Error{path + ".upstreamRegistry",
				fmt.Sprintf("%q must not include a digest", c.UpstreamRegistry), ""})
		}
	}

	errs = append(errs, validateManage(path+".manage", c.Manage)...)
	errs = append(errs, validateCredentialsRef(path+".upstreamCredentialsRef", c.UpstreamCredentialsRef, resolver)...)
	return errs
}

func validateManage(path string, m ManageMode) Errors {
	switch m {
	case "", ManageApply, ManageDetect:
		return nil
	default:
		return Errors{{path, fmt.Sprintf("%q is not one of apply, detect", m), ""}}
	}
}

// validateQuaySettings checks the control-API credential, which is the one
// people miss: a robot account authenticates /v2 and is rejected by /api/v1.
func validateQuaySettings(path string, t Target, resolver *SecretResolver) Errors {
	var errs Errors
	qpath := path + ".quay"

	if t.Quay != nil {
		errs = append(errs, validateSecretRef(qpath+".apiTokenRef", t.Quay.APITokenRef, resolver)...)
		if ep := t.Quay.APIEndpoint; ep != "" && !strings.HasPrefix(ep, "https://") && !strings.HasPrefix(ep, "http://") {
			errs = append(errs, Error{qpath + ".apiEndpoint",
				fmt.Sprintf("%q must be an absolute URL", ep),
				"for example https://quay.apps.ocp.example.com"})
		}
	}

	if !t.Delegated() {
		return errs
	}
	if t.Quay == nil || t.Quay.APITokenRef == nil {
		errs = append(errs, Error{qpath + ".apiTokenRef",
			fmt.Sprintf("required when replication.mode is %q", t.ReplicationMode()),
			"mirror and proxy-cache configuration live on Quay's /api/v1 endpoint, which takes an OAuth 2 bearer token with repo:admin or org:admin scope — the robot account in credentialsRef authenticates /v2 only"})
	}
	return errs
}

// validateMirrorChain rejects a cycle in the mirror.from graph.
//
// Because mirror.from names exactly one upstream, the graph is a forest and a
// cycle is the only structural error possible. It is also the one a reader
// cannot see: a -> b -> c -> a is three correct-looking blocks in three
// different places.
func (p *Product) validateMirrorChain(declared map[string]Target) Errors {
	const (
		white = 0
		grey  = 1
		black = 2
	)
	colour := make(map[string]int, len(declared))

	var walk func(name string, path []string) Errors
	walk = func(name string, path []string) Errors {
		switch colour[name] {
		case black:
			return nil
		case grey:
			return Errors{{
				"spec.targets",
				fmt.Sprintf("mirror.from forms a cycle: %s", strings.Join(append(path, name), " -> ")),
				"a mirror chain has to end at a target something actually writes to",
			}}
		}
		colour[name] = grey
		var errs Errors
		if m, ok := declared[name].MirrorSettings(); ok && m.From != "" {
			if _, exists := declared[m.From]; exists {
				errs = walk(m.From, append(path, name))
			}
		}
		colour[name] = black
		return errs
	}

	var errs Errors
	for _, t := range p.Spec.Targets {
		if t.Name == "" {
			continue
		}
		errs = append(errs, walk(t.Name, nil)...)
	}
	return errs
}

// validatePromotionModes rejects a delegated target as a promotion
// destination. Quay allows only a robot to push into a mirrored repository, so
// a promotion would be refused by the registry at the moment it mattered most.
func (p *Product) validatePromotionModes() Errors {
	var errs Errors
	_, to := p.PromotionPath()
	for i, t := range p.Spec.Targets {
		if !t.Delegated() || t.Environment != to {
			continue
		}
		errs = append(errs, Error{
			fmt.Sprintf("spec.targets[%d].replication.mode", i),
			fmt.Sprintf("%q cannot be the promotion destination environment %q", t.ReplicationMode(), to),
			"a mirrored repository is read-only to everyone but its robot, and a proxy cache cannot be pushed to at all — promote into a copy target, and let this one pull from it",
		})
	}
	return errs
}

// validateRuleModes rejects the two rule/mode combinations that would create
// work with nowhere to go.
func (p *Product) validateRuleModes(declared map[string]Target) Errors {
	var errs Errors
	for i, r := range p.Spec.AutoDownload.Rules {
		targets := r.Targets
		if d, err := p.DownloadFor(r); err == nil {
			targets = d.Targets
		}
		for j, name := range targets {
			t, ok := declared[name]
			if !ok {
				continue // already reported by validateAutoDownload
			}
			path := fmt.Sprintf("spec.autoDownload.rules[%d].targets[%d]", i, j)
			switch t.ReplicationMode() {
			case ReplicationProxy:
				errs = append(errs, Error{path,
					fmt.Sprintf("%q is a proxy cache and cannot be a download target", name),
					"content enters a proxy cache when something pulls through it; use `transferctl warm` to populate one deliberately"})
			case ReplicationMirror:
				if m, ok := t.MirrorSettings(); ok && !m.SyncsOnRequest() {
					errs = append(errs, Error{path,
						fmt.Sprintf("%q is a mirror with syncOnRequest disabled, so a rule matching it would create transfers with nothing to do", name),
						"set replication.mirror.syncOnRequest: true, or remove this target from the rule and let Quay's own schedule do the work"})
				}
			}
		}
	}
	return errs
}
