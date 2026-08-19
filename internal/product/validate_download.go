package product

import (
	"fmt"
	"regexp"
)

// validateDownloads checks the downloads a product declares.
//
// The rule that carries the weight: one download is the default by being the
// only one; several require exactly one to say so. Leaving it implicit with
// two declared would mean `transferctl download <tag>` picked one by list
// order, which is a decision no reader could see.
func (p *Product) validateDownloads() Errors {
	var errs Errors

	seen := map[string]int{}
	defaults := 0
	for i, d := range p.Spec.Download {
		path := fmt.Sprintf("spec.download[%d]", i)

		switch {
		case d.Name == "" && len(p.Spec.Download) > 1:
			errs = append(errs, Error{path + ".name", "required when a product declares several downloads",
				"a rule names the download it triggers, and an unnamed one cannot be named"})
		case d.Name != "" && !nameRE.MatchString(d.Name):
			errs = append(errs, Error{path + ".name",
				fmt.Sprintf("%q is not a valid resource ID", d.Name), "lowercase alphanumeric and hyphens"})
		}
		if d.Name != "" {
			if prev, dup := seen[d.Name]; dup {
				errs = append(errs, Error{path + ".name",
					fmt.Sprintf("%q duplicates spec.download[%d]", d.Name, prev), ""})
			} else {
				seen[d.Name] = i
			}
		}

		if d.Default {
			defaults++
		}
		if pr := d.EffectivePriority(); pr < 0 || pr > 1000 {
			errs = append(errs, Error{path + ".priority",
				fmt.Sprintf("%d is outside the range 0-1000", pr), ""})
		}
		if v := d.Verify; v != nil {
			switch v.Policy {
			case "", PolicyEnforce, PolicyWarn:
			default:
				errs = append(errs, Error{path + ".verify.policy",
					fmt.Sprintf("%q is not one of enforce, warn", v.Policy), ""})
			}
		}
		errs = append(errs, p.validateDownloadTargets(path, d)...)
	}

	switch {
	case len(p.Spec.Download) > 1 && defaults == 0:
		errs = append(errs, Error{"spec.download",
			"several downloads are declared and none is marked default",
			"`transferctl download <tag>` has to pick one, and picking by list order would be a decision no reader could see. Mark one `default: true`"})
	case defaults > 1:
		errs = append(errs, Error{"spec.download",
			fmt.Sprintf("%d downloads are marked default, at most one is permitted", defaults), ""})
	}
	return errs
}

// validateDownloadTargets checks a download's destinations AND the hops they
// pull in.
//
// A download names the destinations somebody cares about; the chain is closed
// over `mirror.from`. So a hop the closure reaches is as load-bearing as a
// destination named outright, and a disabled one is as fatal.
func (p *Product) validateDownloadTargets(path string, d Download) Errors {
	var errs Errors

	declared := make(map[string]Target, len(p.Spec.Targets))
	for _, t := range p.Spec.Targets {
		declared[t.Name] = t
	}

	if len(d.Targets) == 0 {
		if _, ok := p.DefaultTarget(); !ok {
			errs = append(errs, Error{path + ".targets", "required",
				"the product declares no default target, so a download must name where it puts things"})
		}
		return errs
	}

	for j, name := range d.Targets {
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
		if !t.IsEnabled() {
			errs = append(errs, Error{tpath, fmt.Sprintf("%q is disabled", name),
				"re-enable the target, or point the download somewhere else - otherwise this fails the first time it runs"})
			continue
		}

		for _, hop := range p.chainOf(name, declared) {
			if hop == name {
				continue
			}
			h := declared[hop]
			switch {
			case !h.IsEnabled():
				errs = append(errs, Error{tpath,
					fmt.Sprintf("%q pulls from %q, which is disabled", name, hop),
					"the chain is planned in full, so a hop it cannot use is as fatal as a destination it cannot use"})
			case h.PromotionOnly:
				errs = append(errs, Error{tpath,
					fmt.Sprintf("%q pulls from %q, which is promotionOnly", name, hop),
					"a download cannot write to a promotionOnly target, even as a hop"})
			}
		}
	}
	return errs
}

// validateAutoDownload checks the rules that fire a download without being
// asked.
//
// A rule holds a PATTERN and nothing about where things go. Where they go is
// the download's business, and the two are checked separately because they are
// separately wrong.
func (p *Product) validateAutoDownload() Errors {
	var errs Errors
	rules := p.Spec.AutoDownload.Rules
	if !p.Spec.AutoDownload.Enabled && len(rules) == 0 {
		return nil
	}

	sources := make(map[string]Source, len(p.Spec.Sources))
	for _, s := range p.Spec.Sources {
		sources[s.Name] = s
	}

	seen := map[string]int{}
	for i, r := range rules {
		path := fmt.Sprintf("spec.autoDownload.rules[%d]", i)

		if r.Name == "" {
			errs = append(errs, Error{path + ".name", "required", "rule names appear in audit records"})
		} else if prev, dup := seen[r.Name]; dup {
			errs = append(errs, Error{path + ".name", fmt.Sprintf("%q duplicates rules[%d]", r.Name, prev), ""})
		} else {
			seen[r.Name] = i
		}

		if r.TagPattern == "" {
			errs = append(errs, Error{path + ".tagPattern", "required",
				"a rule with no pattern selects nothing; if you meant to download by hand, that needs no rule at all"})
		} else if _, err := regexp.Compile(r.TagPattern); err != nil {
			errs = append(errs, Error{path + ".tagPattern", invalidRegexpMessage(r.TagPattern, err), ""})
		}

		for j, name := range r.Sources {
			s, ok := sources[name]
			if !ok {
				errs = append(errs, Error{fmt.Sprintf("%s.sources[%d]", path, j),
					fmt.Sprintf("%q is not a declared source", name),
					"add it under spec.sources, or correct the name"})
				continue
			}
			if !s.IsEnabled() {
				errs = append(errs, Error{fmt.Sprintf("%s.sources[%d]", path, j),
					fmt.Sprintf("%q is disabled, so this rule would never match anything", name), ""})
			}
		}

		// Naming a download AND carrying inline targets is two answers to one
		// question. The inline form is the older spelling and still works; the
		// two together do not.
		if r.Download != "" && r.UsesLegacyShape() {
			errs = append(errs, Error{path,
				"names a download and also carries its own targets",
				"`targets`, `priority` and `verifyBeforeTransfer` on a rule are the older spelling of a download. Keep one or the other"})
		}
		if r.Download != "" {
			if _, ok := p.DownloadNamed(r.Download); !ok {
				errs = append(errs, Error{path + ".download",
					fmt.Sprintf("%q is not a declared download", r.Download),
					"add it under spec.download, or correct the name"})
			}
		}

		// The legacy inline form is checked as though it were a download,
		// because that is exactly what it is.
		if r.UsesLegacyShape() {
			inline, err := p.DownloadFor(r)
			if err == nil {
				errs = append(errs, p.validateDownloadTargets(path, inline)...)
			}
			if pr := inline.EffectivePriority(); pr < 0 || pr > 1000 {
				errs = append(errs, Error{path + ".priority",
					fmt.Sprintf("%d is outside the range 0-1000", pr), ""})
			}
		}

		// A rule that resolves to nothing at all is a rule that fails the
		// first time a package matches, which may be weeks after the edit.
		if _, err := p.DownloadFor(r); err != nil {
			errs = append(errs, Error{path + ".download", err.Error(),
				"declare a download under spec.download, or give the product a default target"})
		}
	}

	errs = append(errs, p.validateVerificationReachesSignatures()...)
	return errs
}

// validateVerificationReachesSignatures is the check neither block can make
// alone.
//
// A download that verifies at the destination, whose chain ends at a mirror
// whose tag globs cannot match `sha256-<hex>.sig`, is asking for something that
// cannot happen: Quay copies the images, leaves every signature behind, reports
// success, and destination verification then fails for every package forever.
//
// An ERROR rather than the warning in warnings.go, because the download makes
// the intent explicit. Without one, "verify at the destination" is a product
// default that may or may not apply to this target; with a download that says
// `verify.after: true` and names it, the document has stated a requirement its
// own mirror configuration makes impossible.
func (p *Product) validateVerificationReachesSignatures() Errors {
	var errs Errors

	declared := make(map[string]Target, len(p.Spec.Targets))
	for _, t := range p.Spec.Targets {
		declared[t.Name] = t
	}

	check := func(path string, d Download) {
		after := d.VerifiesAfter()
		if after == nil || !*after {
			return
		}
		for j, name := range d.Targets {
			for _, hop := range p.chainOf(name, declared) {
				t, ok := declared[hop]
				if !ok {
					continue
				}
				m, isMirror := t.MirrorSettings()
				if !isMirror || len(m.Tags) == 0 || SelectsSignatures(m.Tags) {
					continue
				}
				if v := p.VerificationFor(t.Verification); !v.ShouldTransferSignatures() {
					continue
				}
				errs = append(errs, Error{
					fmt.Sprintf("%s.targets[%d]", path, j),
					fmt.Sprintf(
						"this download verifies at the destination, but %q mirrors with globs that cannot match a signature tag", hop),
					"cosign publishes a signature as the tag `sha256-<hex>.sig`, so the mirror would copy the images, " +
						"leave every signature behind, report success, and fail verification for every package. " +
						"Add 'sha256-*.sig' to that target's replication.mirror.tags, or set verify.after: false",
				})
			}
		}
	}

	for i, d := range p.Spec.Download {
		check(fmt.Sprintf("spec.download[%d]", i), d)
	}
	for i, r := range p.Spec.AutoDownload.Rules {
		if !r.UsesLegacyShape() {
			continue
		}
		if inline, err := p.DownloadFor(r); err == nil {
			check(fmt.Sprintf("spec.autoDownload.rules[%d]", i), inline)
		}
	}
	return errs
}

// chainOf returns a target and every hop it transitively pulls from.
//
// Bounded by the number of targets rather than by a depth limit, because
// `mirror.from` names exactly one upstream and a cycle is rejected separately
// - this must terminate even when called on a document that has one.
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
