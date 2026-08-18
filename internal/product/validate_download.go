package product

import (
	"fmt"
	"regexp"
	"strings"
)

// validateDownload checks the download rules, in whichever spelling the
// document uses.
//
// Paths are built from the block actually written, so a message about
// `spec.download.rules[0]` never appears in a document that says
// `autoDownload` — an error pointing at a line the reader does not have is
// worse than no error at all.
func (p *Product) validateDownload() Errors {
	var errs Errors

	if len(p.Spec.Download.Rules) > 0 && len(p.Spec.AutoDownload.Rules) > 0 {
		errs = append(errs, Error{"spec.download",
			"both `download` and `autoDownload` are declared",
			"they are the same block under two names; keep `download` and delete the other. Resolving two blocks by precedence would make a configuration nobody can read out loud"})
		return errs
	}

	base := p.DownloadBlockPath()
	rules := p.DownloadRules()
	if !p.DownloadEnabled() && len(rules) == 0 {
		return nil
	}

	declared := make(map[string]Target, len(p.Spec.Targets))
	for _, t := range p.Spec.Targets {
		declared[t.Name] = t
	}
	sources := make(map[string]Source, len(p.Spec.Sources))
	for _, s := range p.Spec.Sources {
		sources[s.Name] = s
	}
	_, hasDefault := p.DefaultTarget()

	seen := map[string]int{}
	for i, r := range rules {
		path := fmt.Sprintf("%s.rules[%d]", base, i)

		if r.Name == "" {
			errs = append(errs, Error{path + ".name", "required", "rule names appear in audit records"})
		} else if prev, dup := seen[r.Name]; dup {
			errs = append(errs, Error{path + ".name", fmt.Sprintf("%q duplicates rules[%d]", r.Name, prev), ""})
		} else {
			seen[r.Name] = i
		}

		if r.TagPattern == "" {
			errs = append(errs, Error{path + ".tagPattern", "required", ""})
		} else if _, err := regexp.Compile(r.TagPattern); err != nil {
			errs = append(errs, Error{path + ".tagPattern", invalidRegexpMessage(r.TagPattern, err), ""})
		}

		errs = append(errs, validateRuleTriggers(path, r)...)
		errs = append(errs, validateRuleWindow(path, r)...)
		errs = append(errs, validateRuleSources(path, r, sources)...)
		errs = append(errs, p.validateRuleTargets(path, r, declared, hasDefault)...)

		if v := r.Verify; v != nil {
			switch v.Policy {
			case "", PolicyEnforce, PolicyWarn:
			default:
				errs = append(errs, Error{path + ".verify.policy",
					fmt.Sprintf("%q is not one of enforce, warn", v.Policy), ""})
			}
		}
		if r.Verify != nil && r.Verify.Before != nil && r.VerifyBeforeTransfer != nil {
			errs = append(errs, Error{path + ".verifyBeforeTransfer",
				"set alongside verify.before, which supersedes it",
				"delete verifyBeforeTransfer; keeping both makes the document state one thing twice and eventually disagree with itself"})
		}

		if pr := r.EffectivePriority(); pr < 0 || pr > 1000 {
			errs = append(errs, Error{path + ".priority", fmt.Sprintf("%d is outside the range 0-1000", pr), ""})
		}
	}

	errs = append(errs, p.validateRuleVerificationReachesSignatures()...)
	return errs
}

func validateRuleTriggers(path string, r Rule) Errors {
	var errs Errors
	// A DECLARED empty list is a typo, not a configuration: it says the rule
	// can never run. An absent list means the default, which is discovery.
	if r.Trigger != nil && len(r.Trigger) == 0 {
		errs = append(errs, Error{path + ".trigger",
			"is empty, so the rule can never run",
			"omit the field for the default (discovery), or name at least one of: discovery, manual"})
		return errs
	}
	for i, t := range r.Trigger {
		switch t {
		case TriggerDiscovery, TriggerManual:
		default:
			errs = append(errs, Error{fmt.Sprintf("%s.trigger[%d]", path, i),
				fmt.Sprintf("%q is not one of discovery, manual", t), ""})
		}
	}
	return errs
}

func validateRuleWindow(path string, r Rule) Errors {
	if r.Window == nil {
		return nil
	}
	wpath := path + ".window"
	if _, _, _, err := r.Window.Parse(); err != nil {
		return Errors{{wpath, err.Error(),
			`start and end are HH:MM; timeZone is an IANA name such as "Europe/London"`}}
	}
	if strings.TrimSpace(r.Window.Start) == strings.TrimSpace(r.Window.End) {
		return Errors{{wpath, "start and end are the same, so the window never opens",
			"a window that crosses midnight is fine — 22:00 to 06:00 is one window, not two"}}
	}
	return nil
}

func validateRuleSources(path string, r Rule, sources map[string]Source) Errors {
	var errs Errors
	for i, name := range r.Sources {
		s, ok := sources[name]
		if !ok {
			errs = append(errs, Error{fmt.Sprintf("%s.sources[%d]", path, i),
				fmt.Sprintf("%q is not a declared source", name),
				"add it under spec.sources, or correct the name"})
			continue
		}
		if !s.IsEnabled() {
			errs = append(errs, Error{fmt.Sprintf("%s.sources[%d]", path, i),
				fmt.Sprintf("%q is disabled, so this rule would never match anything", name), ""})
		}
	}
	return errs
}

// validateRuleTargets checks the rule's destinations AND the hops they pull
// them in.
//
// A rule names the destinations it cares about; the chain is closed over
// `mirror.from`. So a hop the closure reaches is as load-bearing as a
// destination named outright, and a disabled one is as fatal.
func (p *Product) validateRuleTargets(
	path string, r Rule, declared map[string]Target, hasDefault bool,
) Errors {
	var errs Errors

	if len(r.Targets) == 0 && !hasDefault {
		errs = append(errs, Error{
			path + ".targets", "required",
			"the product declares no default target, so every rule must name its targets explicitly",
		})
		return errs
	}

	for j, name := range r.Targets {
		tpath := fmt.Sprintf("%s.targets[%d]", path, j)
		t, ok := declared[name]
		if !ok {
			errs = append(errs, Error{tpath,
				fmt.Sprintf("%q is not a declared target", name),
				"add it under spec.targets, or correct the name"})
			continue
		}
		if t.PromotionOnly {
			errs = append(errs, Error{tpath,
				fmt.Sprintf("%q is promotionOnly and cannot be a download target", name),
				"promotionOnly targets are reachable only by promotion from another target"})
			continue
		}

		// The hops this destination pulls in. Reported against the target that
		// named them, because that is the line the reader would have to change.
		for _, hop := range p.chainOf(name, declared) {
			if hop == name {
				continue
			}
			h := declared[hop]
			switch {
			case !h.IsEnabled():
				errs = append(errs, Error{tpath, fmt.Sprintf(
					"%q pulls from %q, which is disabled", name, hop),
					"the chain is planned in full, so a hop it cannot use is as fatal as a destination it cannot use"})
			case h.PromotionOnly:
				errs = append(errs, Error{tpath, fmt.Sprintf(
					"%q pulls from %q, which is promotionOnly", name, hop),
					"a rule cannot write to a promotionOnly target, even as a hop"})
			}
		}
	}
	return errs
}

// chainOf returns a target and every hop it transitively pulls from.
//
// Bounded by the number of targets rather than by a depth limit, because
// `mirror.from` names exactly one upstream and a cycle is rejected separately
// — this must terminate even when called on a document that has one.
func (p *Product) chainOf(name string, declared map[string]Target) []string {
	var out []string
	seen := map[string]bool{}
	for cur := name; cur != "" && !seen[cur]; {
		seen[cur] = true
		out = append(out, cur)
		t, ok := declared[cur]
		if !ok {
			break
		}
		m, isMirror := t.MirrorSettings()
		if !isMirror || m.From == "" {
			break
		}
		cur = m.From
	}
	return out
}

// validateRuleVerificationReachesSignatures is the check neither block can
// make alone.
//
// A rule that verifies at the destination, whose chain ends at a mirror whose
// tag globs cannot match `sha256-<hex>.sig`, is asking for something that
// cannot happen: Quay copies the images, leaves every signature behind,
// reports success, and destination verification then fails for every package
// forever.
//
// An ERROR here rather than the warning in warnings.go, because the rule makes
// the intent explicit. Without a rule, "verify at the destination" is a
// product default that may or may not apply to this target; with one that says
// `verify.after: true` and names the target, the document has stated a
// requirement that its own mirror configuration makes impossible.
func (p *Product) validateRuleVerificationReachesSignatures() Errors {
	var errs Errors
	base := p.DownloadBlockPath()

	declared := make(map[string]Target, len(p.Spec.Targets))
	for _, t := range p.Spec.Targets {
		declared[t.Name] = t
	}

	for i, r := range p.DownloadRules() {
		after := r.VerifiesAfter()
		if after == nil || !*after {
			continue
		}
		for j, name := range r.Targets {
			for _, hop := range p.chainOf(name, declared) {
				t, ok := declared[hop]
				if !ok {
					continue
				}
				m, isMirror := t.MirrorSettings()
				if !isMirror || len(m.Tags) == 0 || SelectsSignatures(m.Tags) {
					continue
				}
				v := p.VerificationFor(t.Verification)
				if !v.ShouldTransferSignatures() {
					continue
				}
				errs = append(errs, Error{
					fmt.Sprintf("%s.rules[%d].targets[%d]", base, i, j),
					fmt.Sprintf(
						"this rule verifies at the destination, but %q mirrors with globs that cannot match a signature tag",
						hop),
					"cosign publishes a signature as the tag `sha256-<hex>.sig`, so the mirror would copy the images, " +
						"leave every signature behind, report success, and fail verification for every package. " +
						"Add 'sha256-*.sig' to that target's replication.mirror.tags, or set verify.after: false",
				})
			}
		}
	}
	return errs
}
