package product

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Validation is hand-rolled rather than tag-driven.
//
// The three error classes that matter most — a non-compiling tagPattern, a
// rule naming an undeclared target, and keyless verification without an
// identity constraint — are all semantic or cross-field, and none is
// expressible as a struct tag. Since a validator library would help only with
// the trivial field checks and would still need its messages translated into
// the `spec.path[i].field` form, hand-rolling produces better errors and one
// fewer dependency. See docs/design/13 section 9 for the target output.

// nameRE is the AIP-122 resource ID form.
var nameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Error is one validation failure, located by field path.
type Error struct {
	Field   string
	Message string
	// Hint explains why the rule exists, when the reason is not obvious. It
	// is what turns "certificateIdentity is required" into a message the
	// reader can act on.
	Hint string
}

func (e Error) Error() string {
	if e.Hint != "" {
		return fmt.Sprintf("%s: %s — %s", e.Field, e.Message, e.Hint)
	}
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// Errors is an ordered collection of validation failures. All problems in a
// document are reported at once; fixing them one round-trip at a time is a
// poor experience in CI.
type Errors []Error

func (e Errors) Error() string {
	if len(e) == 0 {
		return "no validation errors"
	}
	parts := make([]string, len(e))
	for i, err := range e {
		parts[i] = err.Error()
	}
	return strings.Join(parts, "; ")
}

// ErrOrNil returns nil when empty, so callers can `return errs.ErrOrNil()`.
func (e Errors) ErrOrNil() error {
	if len(e) == 0 {
		return nil
	}
	return e
}

// Validate checks a product document.
//
// resolver may be nil, in which case secret existence is not checked — used by
// `transferctl config validate` running offline in CI, where the cluster's
// Secrets are legitimately unavailable.
func (p *Product) Validate(resolver *SecretResolver) error {
	var errs Errors

	if p.APIVersion != APIVersion {
		errs = append(errs, Error{"apiVersion", fmt.Sprintf("expected %q, got %q", APIVersion, p.APIVersion), ""})
	}
	if p.Kind != Kind {
		errs = append(errs, Error{"kind", fmt.Sprintf("expected %q, got %q", Kind, p.Kind), ""})
	}

	errs = append(errs, p.validateMetadata()...)
	errs = append(errs, p.validateSources(resolver)...)
	errs = append(errs, p.validateTargets(resolver)...)
	errs = append(errs, p.validateAutoDownload()...)
	errs = append(errs, p.validateVerification(resolver)...)
	errs = append(errs, p.validateNotifications(resolver)...)

	return errs.ErrOrNil()
}

func (p *Product) validateMetadata() Errors {
	var errs Errors
	switch {
	case p.Metadata.Name == "":
		errs = append(errs, Error{"metadata.name", "required", ""})
	case len(p.Metadata.Name) > 63:
		errs = append(errs, Error{"metadata.name", fmt.Sprintf("%d characters, maximum 63", len(p.Metadata.Name)), ""})
	case !nameRE.MatchString(p.Metadata.Name):
		errs = append(errs, Error{
			"metadata.name",
			fmt.Sprintf("%q is not a valid resource ID", p.Metadata.Name),
			"lowercase alphanumeric and hyphens, starting and ending alphanumeric",
		})
	}
	if len(p.Metadata.Description) > 2000 {
		errs = append(errs, Error{"metadata.description", "exceeds 2000 characters", ""})
	}
	return errs
}

func (p *Product) validateSources(resolver *SecretResolver) Errors {
	var errs Errors
	if len(p.Spec.Sources) == 0 {
		errs = append(errs, Error{"spec.sources", "at least one source is required", ""})
	}

	seen := map[string]int{}
	for i, s := range p.Spec.Sources {
		path := fmt.Sprintf("spec.sources[%d]", i)
		errs = append(errs, validateRepoCommon(path, s.Name, s.Registry, s.Repository, s.Type, s.Anonymous, s.CredentialsRef, resolver)...)

		if prev, dup := seen[s.Name]; dup && s.Name != "" {
			errs = append(errs, Error{path + ".name", fmt.Sprintf("%q duplicates spec.sources[%d]", s.Name, prev), ""})
		} else if s.Name != "" {
			seen[s.Name] = i
		}

		for j, pat := range s.Discovery.TagFilters.Include {
			if _, err := regexp.Compile(pat); err != nil {
				errs = append(errs, Error{fmt.Sprintf("%s.discovery.tagFilters.include[%d]", path, j), invalidRegexpMessage(pat, err), ""})
			}
		}
		for j, pat := range s.Discovery.TagFilters.Exclude {
			if _, err := regexp.Compile(pat); err != nil {
				errs = append(errs, Error{fmt.Sprintf("%s.discovery.tagFilters.exclude[%d]", path, j), invalidRegexpMessage(pat, err), ""})
			}
		}

		if d := s.Discovery.Interval.Duration(); d < 0 {
			errs = append(errs, Error{path + ".discovery.interval", "must not be negative", ""})
		}
		errs = append(errs, validateRateLimits(path+".rateLimits", s.RateLimits)...)
	}
	return errs
}

func (p *Product) validateTargets(resolver *SecretResolver) Errors {
	var errs Errors
	if len(p.Spec.Targets) == 0 {
		errs = append(errs, Error{"spec.targets", "at least one target is required", ""})
	}

	seen := map[string]int{}
	defaults := 0
	for i, t := range p.Spec.Targets {
		path := fmt.Sprintf("spec.targets[%d]", i)
		errs = append(errs, validateRepoCommon(path, t.Name, t.Registry, t.Repository, t.Type, t.Anonymous, t.CredentialsRef, resolver)...)

		if prev, dup := seen[t.Name]; dup && t.Name != "" {
			errs = append(errs, Error{path + ".name", fmt.Sprintf("%q duplicates spec.targets[%d]", t.Name, prev), ""})
		} else if t.Name != "" {
			seen[t.Name] = i
		}

		if t.Default {
			defaults++
			if t.PromotionOnly {
				errs = append(errs, Error{
					path, "cannot be both default and promotionOnly",
					"a promotionOnly target may never be a replication destination, so it can never be the default",
				})
			}
		}
		errs = append(errs, validateRateLimits(path+".rateLimits", t.RateLimits)...)
	}

	if defaults > 1 {
		errs = append(errs, Error{"spec.targets", fmt.Sprintf("%d targets marked default, at most one is permitted", defaults), ""})
	}
	return errs
}

// validateAutoDownload catches two of the three headline error classes:
// a tagPattern that does not compile, and a rule naming an undeclared target.
func (p *Product) validateAutoDownload() Errors {
	var errs Errors
	if !p.Spec.AutoDownload.Enabled && len(p.Spec.AutoDownload.Rules) == 0 {
		return nil
	}

	declared := make(map[string]Target, len(p.Spec.Targets))
	for _, t := range p.Spec.Targets {
		declared[t.Name] = t
	}
	_, hasDefault := p.DefaultTarget()

	seen := map[string]int{}
	for i, r := range p.Spec.AutoDownload.Rules {
		path := fmt.Sprintf("spec.autoDownload.rules[%d]", i)

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

		if len(r.Targets) == 0 && !hasDefault {
			errs = append(errs, Error{
				path + ".targets",
				"required",
				"the product declares no default target, so every rule must name its targets explicitly",
			})
		}

		for j, name := range r.Targets {
			t, ok := declared[name]
			if !ok {
				errs = append(errs, Error{
					fmt.Sprintf("%s.targets[%d]", path, j),
					fmt.Sprintf("%q is not a declared target", name),
					"add it under spec.targets, or correct the name",
				})
				continue
			}
			if t.PromotionOnly {
				errs = append(errs, Error{
					fmt.Sprintf("%s.targets[%d]", path, j),
					fmt.Sprintf("%q is promotionOnly and cannot be an auto-download target", name),
					"promotionOnly targets are reachable only by promotion from another target",
				})
			}
		}

		if pr := r.EffectivePriority(); pr < 0 || pr > 1000 {
			errs = append(errs, Error{path + ".priority", fmt.Sprintf("%d is outside the range 0-1000", pr), ""})
		}
	}
	return errs
}

// validateVerification catches the third headline error class: a keyless
// policy with no identity constraint.
func (p *Product) validateVerification(resolver *SecretResolver) Errors {
	var errs Errors
	v := p.Spec.Verification
	if !v.Enabled {
		return nil
	}

	switch v.Policy {
	case PolicyEnforce, PolicyWarn:
	case "":
		errs = append(errs, Error{"spec.verification.policy", "required when verification is enabled", "one of enforce, warn"})
	default:
		errs = append(errs, Error{"spec.verification.policy", fmt.Sprintf("%q is not one of enforce, warn", v.Policy), ""})
	}

	if !v.AtSource && !v.AtDestination {
		errs = append(errs, Error{
			"spec.verification",
			"neither atSource nor atDestination is set",
			"verification is enabled but would never run",
		})
	}

	switch v.Cosign.Mode {
	case CosignKeylessMode:
		if v.Cosign.Keyless == nil {
			errs = append(errs, Error{"spec.verification.cosign.keyless", "required in keyless mode", ""})
			break
		}
		// THE important one. A keyless policy without an identity constraint
		// is syntactically fine and semantically useless: it verifies that
		// *someone* signed the artifact, not that the vendor did. Rejecting it
		// here prevents a configuration that looks secure and is not.
		if v.Cosign.Keyless.CertificateIdentity == "" {
			errs = append(errs, Error{
				"spec.verification.cosign.keyless.certificateIdentity",
				"required in keyless mode",
				"without it, any valid Sigstore signature would be accepted — it would prove someone signed the artifact, not that the vendor did",
			})
		}
		if v.Cosign.Keyless.CertificateOidcIssuer == "" {
			errs = append(errs, Error{
				"spec.verification.cosign.keyless.certificateOidcIssuer",
				"required in keyless mode",
				"without it, an identity from any OIDC issuer would be accepted",
			})
		}
		errs = append(errs, validateSecretRef("spec.verification.cosign.keyless.rekorPublicKeysRef", v.Cosign.Keyless.RekorPublicKeysRef, resolver)...)
		errs = append(errs, validateSecretRef("spec.verification.cosign.keyless.fulcioCertsRef", v.Cosign.Keyless.FulcioCertsRef, resolver)...)

	case CosignKeyMode:
		if v.Cosign.Key == nil || v.Cosign.Key.PublicKeyRef == nil {
			errs = append(errs, Error{"spec.verification.cosign.key.publicKeyRef", "required in key mode", ""})
		} else {
			errs = append(errs, validateSecretRef("spec.verification.cosign.key.publicKeyRef", v.Cosign.Key.PublicKeyRef, resolver)...)
		}

	case "":
		errs = append(errs, Error{"spec.verification.cosign.mode", "required when verification is enabled", "one of keyless, key"})
	default:
		errs = append(errs, Error{"spec.verification.cosign.mode", fmt.Sprintf("%q is not one of keyless, key", v.Cosign.Mode), ""})
	}

	return errs
}

func (p *Product) validateNotifications(resolver *SecretResolver) Errors {
	var errs Errors
	n := p.Spec.Notifications
	if !n.Enabled {
		return nil
	}

	declared := map[string]Channel{}
	for i, c := range n.Channels {
		path := fmt.Sprintf("spec.notifications.channels[%d]", i)
		if c.Name == "" {
			errs = append(errs, Error{path + ".name", "required", ""})
		} else if _, dup := declared[c.Name]; dup {
			errs = append(errs, Error{path + ".name", fmt.Sprintf("%q is declared more than once", c.Name), ""})
		} else {
			declared[c.Name] = c
		}

		switch c.Type {
		case ChannelEmail:
			if c.Email == nil || len(c.Email.Recipients) == 0 {
				errs = append(errs, Error{path + ".email.recipients", "at least one recipient is required", ""})
			}
		case ChannelTeams:
			if c.Teams == nil || c.Teams.WebhookURLRef == nil {
				errs = append(errs, Error{
					path + ".teams.webhookUrlRef", "required",
					"must reference a Power Automate workflow URL; legacy O365 connector webhooks are retired",
				})
			} else {
				errs = append(errs, validateSecretRef(path+".teams.webhookUrlRef", c.Teams.WebhookURLRef, resolver)...)
			}
		case "":
			errs = append(errs, Error{path + ".type", "required", "one of email, teams"})
		default:
			errs = append(errs, Error{path + ".type", fmt.Sprintf("%q is not one of email, teams", c.Type), ""})
		}
	}

	known := make(map[string]bool, len(KnownEvents))
	for _, e := range KnownEvents {
		known[e] = true
	}

	for i, s := range n.Subscriptions {
		path := fmt.Sprintf("spec.notifications.subscriptions[%d]", i)
		if len(s.Events) == 0 {
			errs = append(errs, Error{path + ".events", "at least one event is required", ""})
		}
		for j, e := range s.Events {
			if !known[e] {
				errs = append(errs, Error{
					fmt.Sprintf("%s.events[%d]", path, j),
					fmt.Sprintf("%q is not a known event", e),
					"one of " + strings.Join(KnownEvents, ", "),
				})
			}
		}
		if len(s.Channels) == 0 {
			errs = append(errs, Error{path + ".channels", "at least one channel is required", ""})
		}
		for j, name := range s.Channels {
			if _, ok := declared[name]; !ok {
				errs = append(errs, Error{
					fmt.Sprintf("%s.channels[%d]", path, j),
					fmt.Sprintf("%q is not a declared channel", name),
					"add it under spec.notifications.channels, or correct the name",
				})
			}
		}
	}
	return errs
}

func validateRepoCommon(
	path, name, registry, repository string,
	typ RegistryType, anonymous bool, creds *CredentialsRef, resolver *SecretResolver,
) Errors {
	var errs Errors

	switch {
	case name == "":
		errs = append(errs, Error{path + ".name", "required", ""})
	case !nameRE.MatchString(name):
		errs = append(errs, Error{path + ".name", fmt.Sprintf("%q is not a valid resource ID", name),
			"lowercase alphanumeric and hyphens"})
	}

	switch {
	case registry == "":
		errs = append(errs, Error{path + ".registry", "required", ""})
	case strings.Contains(registry, "://"):
		errs = append(errs, Error{path + ".registry", "must not include a scheme", "use the host, optionally with a port; HTTPS is assumed"})
	case strings.HasSuffix(registry, "/"):
		errs = append(errs, Error{path + ".registry", "must not end with '/'", ""})
	}

	switch {
	case repository == "":
		errs = append(errs, Error{path + ".repository", "required", ""})
	case strings.ContainsAny(repository, ":@"):
		errs = append(errs, Error{path + ".repository", "must not include a tag or digest", "name the repository only"})
	case strings.HasPrefix(repository, "/") || strings.HasSuffix(repository, "/"):
		errs = append(errs, Error{path + ".repository", "must not start or end with '/'", ""})
	}

	if typ != "" {
		valid := false
		for _, t := range ValidRegistryTypes {
			if typ == t {
				valid = true
				break
			}
		}
		if !valid {
			names := make([]string, len(ValidRegistryTypes))
			for i, t := range ValidRegistryTypes {
				names[i] = string(t)
			}
			errs = append(errs, Error{path + ".type", fmt.Sprintf("%q is not one of %s", typ, strings.Join(names, ", ")), ""})
		}
	}

	switch {
	case anonymous && creds != nil:
		errs = append(errs, Error{path, "anonymous and credentialsRef are mutually exclusive", ""})
	case !anonymous && creds == nil:
		errs = append(errs, Error{path + ".credentialsRef", "required unless anonymous is true", ""})
	case creds != nil:
		if creds.SecretName == "" {
			errs = append(errs, Error{path + ".credentialsRef.secretName", "required", ""})
		} else if resolver != nil {
			for _, key := range []string{creds.UsernameKeyOrDefault(), creds.PasswordKeyOrDefault()} {
				if !resolver.Exists(creds.SecretName, key) {
					errs = append(errs, Error{
						path + ".credentialsRef",
						fmt.Sprintf("secret %q has no key %q", creds.SecretName, key),
						"expected at " + resolver.Dir() + "/" + creds.SecretName + "/" + key,
					})
				}
			}
		}
	}

	return errs
}

func validateRateLimits(path string, r RateLimits) Errors {
	var errs Errors
	for _, f := range []struct {
		name string
		v    int
	}{
		{"maxConcurrentDownloads", r.MaxConcurrentDownloads},
		{"maxConcurrentUploads", r.MaxConcurrentUploads},
		{"maxConnections", r.MaxConnections},
		{"requestsPerSecond", r.RequestsPerSecond},
		{"burst", r.Burst},
	} {
		if f.v < 0 {
			errs = append(errs, Error{path + "." + f.name, fmt.Sprintf("%d must not be negative", f.v), ""})
		}
	}
	if r.RequestsPerSecond > 0 && r.Burst > 0 && r.Burst < r.RequestsPerSecond {
		errs = append(errs, Error{
			path + ".burst",
			fmt.Sprintf("%d is below requestsPerSecond (%d)", r.Burst, r.RequestsPerSecond),
			"a burst smaller than the sustained rate throttles below the configured rate",
		})
	}
	return errs
}

func validateSecretRef(path string, ref *SecretRef, resolver *SecretResolver) Errors {
	if ref == nil {
		return nil
	}
	var errs Errors
	if ref.SecretName == "" {
		errs = append(errs, Error{path + ".secretName", "required", ""})
		return errs
	}
	key := ref.Key
	if key == "" {
		errs = append(errs, Error{path + ".key", "required", ""})
		return errs
	}
	if resolver != nil && !resolver.Exists(ref.SecretName, key) {
		errs = append(errs, Error{
			path,
			fmt.Sprintf("secret %q has no key %q", ref.SecretName, key),
			"expected at " + resolver.Dir() + "/" + ref.SecretName + "/" + key,
		})
	}
	return errs
}

// invalidRegexpMessage renders a compile failure in a form that points at the
// actual problem. Go's own error ("missing closing )") is good; the value is
// in showing it next to the pattern.
func invalidRegexpMessage(pattern string, err error) string {
	msg := err.Error()
	// regexp errors read "error parsing regexp: missing closing ): `...`" —
	// strip the framing so the message is not doubled up with the pattern.
	if i := strings.Index(msg, ": "); i >= 0 {
		if rest := msg[i+2:]; rest != "" {
			if j := strings.Index(rest, ": `"); j >= 0 {
				rest = rest[:j]
			}
			msg = rest
		}
	}
	return fmt.Sprintf("invalid regexp %q — %s", pattern, msg)
}

// AsErrors extracts the structured errors from a Validate result.
func AsErrors(err error) (Errors, bool) {
	var e Errors
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
