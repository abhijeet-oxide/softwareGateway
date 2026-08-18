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
	errs = append(errs, p.validatePromotion()...)
	errs = append(errs, p.validatePhysicalRepositories()...)
	errs = append(errs, p.validateDownload()...)
	errs = append(errs, p.validateReplication(resolver)...)
	errs = append(errs, p.validateVerification(resolver)...)
	errs = append(errs, p.validateNotifications(resolver)...)
	errs = append(errs, validateNetwork("spec.network", &p.Spec.Network, resolver)...)
	errs = append(errs, p.validateEnablement()...)

	return errs.ErrOrNil()
}

// validateEnablement checks the relationships `enabled: false` can break.
//
// A DISABLED product is validated exactly like an enabled one — the point of
// disabling rather than deleting is that the configuration stays correct and
// re-enabling is safe. So these rules apply either way.
func (p *Product) validateEnablement() Errors {
	var errs Errors

	// Every source disabled means the product discovers nothing while looking
	// configured. If that is intended, disable the product and say so.
	if len(p.Spec.Sources) > 0 && len(p.EnabledSources()) == 0 && p.IsEnabled() {
		errs = append(errs, Error{
			"spec.sources",
			"every source is disabled",
			"this product would discover nothing while appearing active — " +
				"disable the product itself with `metadata.enabled: false` instead",
		})
	}

	if len(p.Spec.Targets) > 0 && len(p.EnabledTargets()) == 0 && p.IsEnabled() &&
		p.DownloadEnabled() {
		errs = append(errs, Error{
			"spec.targets",
			"every target is disabled but download rules are enabled",
			"rules would match packages and have nowhere to send them",
		})
	}

	// A rule pointing at a disabled target is the failure that only shows up
	// when a package matches — potentially weeks later.
	enabled := map[string]bool{}
	for _, t := range p.EnabledTargets() {
		enabled[t.Name] = true
	}
	declared := map[string]bool{}
	for _, t := range p.Spec.Targets {
		declared[t.Name] = true
	}

	base := p.DownloadBlockPath()
	for i, r := range p.DownloadRules() {
		for j, name := range r.Targets {
			if declared[name] && !enabled[name] {
				errs = append(errs, Error{
					fmt.Sprintf("%s.rules[%d].targets[%d]", base, i, j),
					fmt.Sprintf("%q is disabled", name),
					"re-enable the target, or point the rule somewhere else — " +
						"otherwise this rule fails the first time a package matches it",
				})
			}
		}
	}

	return errs
}

// validateSourceRepositories checks the repository set a source declares.
func validateSourceRepositories(path string, s Source) Errors {
	var errs Errors

	// Naming no repositories is VALID and is the interesting case: it means
	// "every repository on this registry", found from the catalog. A product
	// whose components each ship as a new repository cannot list them in
	// advance, and requiring it would mean a new component is silently not
	// replicated until somebody edits the ConfigMap.
	//
	// Scoping that with discovery.repositoryFilters is strongly advised on a
	// shared registry, but it is not an error — a registry dedicated to one
	// vendor needs no filter, and refusing to start would be wrong there.
	// `transferctl products check` reports how many repositories an unfiltered
	// source would actually adopt, which is the fact worth acting on.

	if n := s.Discovery.MaxRepositories; n < 0 {
		errs = append(errs, Error{path + ".discovery.maxRepositories", "must not be negative", ""})
	}

	for i, r := range s.Repositories {
		if strings.TrimSpace(r) == "" {
			errs = append(errs, Error{fmt.Sprintf("%s.repositories[%d]", path, i), "must not be empty", ""})
		}
	}

	// A repository listed twice is harmless — DeclaredRepositories dedupes —
	// but it is always a mistake worth surfacing, usually a copy-paste.
	seen := map[string]bool{}
	if s.Repository != "" {
		seen[strings.Trim(s.Repository, "/")] = true
	}
	for i, r := range s.Repositories {
		key := strings.Trim(strings.TrimSpace(r), "/")
		if key == "" {
			continue
		}
		if seen[key] {
			errs = append(errs, Error{
				fmt.Sprintf("%s.repositories[%d]", path, i),
				fmt.Sprintf("%q is declared more than once", key), ""})
			continue
		}
		seen[key] = true
	}

	return errs
}

// validateNetwork checks a network block's secret references.
//
// Previously unchecked, which meant a typo in caBundleRef surfaced as a TLS
// failure against the vendor at the first scan rather than as a configuration
// error at load — a long way from the cause.
func validateNetwork(path string, n *Network, resolver *SecretResolver) Errors {
	if n == nil {
		return nil
	}
	var errs Errors

	if n.CABundleRef != nil {
		ref := n.CABundleRef
		key := ref.Key
		if key == "" {
			key = DefaultCABundleKey
		}
		switch {
		case ref.SecretName == "":
			errs = append(errs, Error{path + ".caBundleRef.secretName", "required", ""})
		case resolver != nil && !resolver.Exists(ref.SecretName, key):
			errs = append(errs, Error{
				path + ".caBundleRef",
				fmt.Sprintf("secret %q has no key %q", ref.SecretName, key),
				"the CA bundle is a PEM file projected by VSO; the default key is " + DefaultCABundleKey,
			})
		}
	}

	if n.Proxy != nil && n.Proxy.HTTPSProxy != "" {
		if !strings.Contains(n.Proxy.HTTPSProxy, "://") {
			errs = append(errs, Error{
				path + ".proxy.httpsProxy",
				fmt.Sprintf("%q is not a URL", n.Proxy.HTTPSProxy),
				"include the scheme, e.g. http://proxy.example.com:3128",
			})
		}
	}

	// A CA bundle and skipped verification in the SAME block is dead
	// configuration: nothing verifies the chain, so the bundle is never
	// consulted. Rejected rather than tolerated, because the shape it usually
	// takes is someone adding insecureSkipVerify to debug a TLS failure, fixing
	// the real cause with the bundle, and never taking the escape hatch out —
	// leaving a product that looks like it verifies and does not.
	//
	// Across levels this is legitimate and allowed: a product-wide caBundleRef
	// with one source overriding it is exactly what the override exists for.
	if n.TLS.SkipsVerify() && n.CABundleRef != nil {
		errs = append(errs, Error{
			path + ".tls.insecureSkipVerify",
			"set alongside caBundleRef in the same network block",
			"skipping verification means the CA bundle is never consulted. Remove " +
				"one: keep caBundleRef if the bundle fixes the chain, keep " +
				"insecureSkipVerify only if it does not",
		})
	}

	return errs
}

// validatePhysicalRepositories rejects two declarations of the same physical
// repository within one product.
//
// `repositories` carries a unique index on (registry_host, repository_path)
// and a NOT NULL product_id, so one physical repository is owned by exactly
// one declaration. Two entries naming the same registry and path — whether two
// sources, two targets, or one of each — cannot both be stored, and reconciling
// them would make one silently overwrite the other on every config reload.
//
// Catching it here turns a confusing runtime behaviour into a clear
// pre-merge error. See also Loader.Load, which enforces the same rule ACROSS
// products.
func (p *Product) validatePhysicalRepositories() Errors {
	var errs Errors

	type origin struct {
		path string
		role Role
	}
	seen := map[string]origin{}

	for i, s := range p.Spec.Sources {
		for _, repo := range s.DeclaredRepositories() {
			key := physicalKey(s.Registry, repo)
			path := fmt.Sprintf("spec.sources[%d]", i)
			if prev, dup := seen[key]; dup {
				errs = append(errs, Error{
					path,
					fmt.Sprintf("%s/%s is already declared by %s", s.Registry, repo, prev.path),
					"one physical repository may be declared once per product",
				})
				continue
			}
			seen[key] = origin{path, RoleSource}
		}
	}

	for i, t := range p.Spec.Targets {
		key := physicalKey(t.Registry, t.Repository)
		path := fmt.Sprintf("spec.targets[%d]", i)
		if prev, dup := seen[key]; dup {
			hint := "one physical repository may be declared once per product"
			if prev.role == RoleSource {
				hint = "a repository cannot be both a source and a target — " +
					"replication would read from and write to the same place"
			}
			errs = append(errs, Error{
				path,
				fmt.Sprintf("%s/%s is already declared by %s", t.Registry, t.Repository, prev.path),
				hint,
			})
			continue
		}
		seen[key] = origin{path, RoleTarget}
	}

	return errs
}

// physicalKey identifies a repository by registry host and path — the same
// identity the repositories_physical_idx unique index uses.
func physicalKey(registry, repository string) string {
	return strings.ToLower(registry) + "/" + strings.Trim(repository, "/")
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
		// A source may name one repository, several, or none at all when it
		// enumerates them from the catalog. validateRepoCommon checks the
		// single-repository field, so pass the first declared one — or "" when
		// discovery supplies them — and check the rest separately.
		declared := s.DeclaredRepositories()
		primary := ""
		if len(declared) > 0 {
			primary = declared[0]
		}
		errs = append(errs, validateRepoCommon(path, s.Name, s.Registry, primary,
			s.Type, s.Anonymous, s.CredentialsRef, resolver, false)...)
		errs = append(errs, validateSourceRepositories(path, s)...)

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
		for j, pat := range s.Discovery.RepositoryFilters.Include {
			if _, err := regexp.Compile(pat); err != nil {
				errs = append(errs, Error{fmt.Sprintf("%s.discovery.repositoryFilters.include[%d]", path, j), invalidRegexpMessage(pat, err), ""})
			}
		}
		for j, pat := range s.Discovery.RepositoryFilters.Exclude {
			if _, err := regexp.Compile(pat); err != nil {
				errs = append(errs, Error{fmt.Sprintf("%s.discovery.repositoryFilters.exclude[%d]", path, j), invalidRegexpMessage(pat, err), ""})
			}
		}
		errs = append(errs, validateNetwork(path+".network", s.Network, resolver)...)
		errs = append(errs, validateCosign(path+".verification", s.Verification, resolver)...)

		if d := s.Discovery.Interval.Duration(); d < 0 {
			errs = append(errs, Error{path + ".discovery.interval", "must not be negative", ""})
		}
		errs = append(errs, validateConcurrency(path, s.Concurrency, s.RateLimits, s.Discovery.Concurrency)...)
		errs = append(errs, validateSignatures(path+".signatures", s.Signatures, resolver)...)
		errs = append(errs, validateVendor(path, s)...)
	}
	return errs
}

// validateVendor rejects a source that names its vendor twice, differently.
//
// `vendor` supersedes `signatures.layout` and they are the same setting, so
// both present and agreeing is harmless — a document mid-migration. Both
// present and DISAGREEING has no correct reading, and picking one silently
// would mean the operator's other spelling does nothing while looking as though
// it does.
//
// The NAME is not checked here: the set of valid names depends on which vendor
// plugins the binary registers, which this package deliberately does not know.
// The composition root resolves it and fails loudly on an unknown value.
func validateVendor(path string, s Source) Errors {
	vendor := strings.TrimSpace(s.Vendor)
	layout := strings.TrimSpace(s.Signatures.Layout)
	if vendor == "" || layout == "" || vendor == layout {
		return nil
	}
	return Errors{{
		path + ".vendor",
		fmt.Sprintf("%q disagrees with signatures.layout %q", vendor, layout),
		"they are the same setting under two names; keep `vendor` and delete `signatures.layout`",
	}}
}

// validatePromotion checks the declared promotion hop against reality.
//
// An environment naming no target is rejected HERE rather than at promote
// time, because the failure is a configuration one and the person who can fix
// it is the person editing this file — not the operator who runs `promote` six
// weeks later and gets told their product has no lab.
func (p *Product) validatePromotion() Errors {
	if p.Spec.Promotion == nil {
		return nil
	}

	environments := map[string]bool{}
	for _, t := range p.Spec.Targets {
		if t.Environment != "" {
			environments[t.Environment] = true
		}
	}

	var errs Errors
	for _, f := range []struct{ path, value string }{
		{"spec.promotion.from", p.Spec.Promotion.From},
		{"spec.promotion.to", p.Spec.Promotion.To},
	} {
		if f.value == "" {
			continue
		}
		if !nameRE.MatchString(f.value) {
			errs = append(errs, Error{f.path,
				fmt.Sprintf("%q is not a valid environment name", f.value),
				"lowercase alphanumeric and hyphens"})
			continue
		}
		if !environments[f.value] {
			errs = append(errs, Error{f.path,
				fmt.Sprintf("no target declares environment %q", f.value),
				"set `environment: " + f.value + "` on the targets in that stage"})
		}
	}

	if p.Spec.Promotion.From != "" && p.Spec.Promotion.From == p.Spec.Promotion.To {
		errs = append(errs, Error{"spec.promotion",
			"from and to name the same environment",
			"a promotion that does not change environment moves nothing"})
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
		// A target's repository is OPTIONAL, and that is a deliberate revision
		// of "a target is always exactly one repository".
		//
		// A bundle is not one artifact: an ORB's images, charts and generic
		// artifacts each carry their own repository path, and the destination
		// reproduces that structure rather than flattening it. The target's
		// `repository` is therefore a PREFIX to nest that structure under —
		// useful for keeping two vendors apart in one registry — and omitting
		// it mirrors the source paths at the destination's root, so a consumer
		// changes the hostname and nothing else.
		errs = append(errs, validateRepoCommon(path, t.Name, t.Registry, t.Repository,
			t.Type, t.Anonymous, t.CredentialsRef, resolver, false)...)
		errs = append(errs, validateNetwork(path+".network", t.Network, resolver)...)
		errs = append(errs, validateCosign(path+".verification", t.Verification, resolver)...)

		if prev, dup := seen[t.Name]; dup && t.Name != "" {
			errs = append(errs, Error{path + ".name", fmt.Sprintf("%q duplicates spec.targets[%d]", t.Name, prev), ""})
		} else if t.Name != "" {
			seen[t.Name] = i
		}

		if t.Environment != "" && !nameRE.MatchString(t.Environment) {
			errs = append(errs, Error{path + ".environment",
				fmt.Sprintf("%q is not a valid environment name", t.Environment),
				"lowercase alphanumeric and hyphens; several targets may share one"})
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
		errs = append(errs, validateConcurrency(path, t.Concurrency, t.RateLimits, LegacyScanConcurrency{})...)
	}

	if defaults > 1 {
		errs = append(errs, Error{"spec.targets", fmt.Sprintf("%d targets marked default, at most one is permitted", defaults), ""})
	}
	return errs
}

// validateSignatures checks the layout and format axes.
//
// The layout NAME is not checked here: the set of valid names depends on which
// vendor plugins the binary registers, which this package deliberately does not
// know. The composition root resolves it and fails loudly on an unknown value.
func validateSignatures(path string, s Signatures, resolver *SecretResolver) Errors {
	var errs Errors

	switch s.Format {
	case "", "auto", "cosign", "pkcs7":
	default:
		errs = append(errs, Error{
			path + ".format",
			fmt.Sprintf("%q is not one of auto, cosign, pkcs7", s.Format), ""})
	}

	errs = append(errs, validateSecretRef(path+".trustBundleRef", s.TrustBundleRef, resolver)...)
	return errs
}

// validateVerification catches the third headline error class: a keyless
// policy with no identity constraint.
func (p *Product) validateVerification(resolver *SecretResolver) Errors {
	return validateVerificationAt("spec.verification", p.Spec.Verification, resolver)
}

// validateCosign checks a per-repository verification override.
//
// The same rules as the product's, at a different path — extracted rather than
// duplicated, because the keyless-identity check below is the one that stops a
// trust configuration that looks secure and is not, and two copies of it would
// eventually differ.
func validateCosign(path string, v *Verification, resolver *SecretResolver) Errors {
	if v == nil {
		return nil
	}
	return validateVerificationAt(path, *v, resolver)
}

func validateVerificationAt(path string, v Verification, resolver *SecretResolver) Errors {
	var errs Errors
	if !v.Enabled {
		return nil
	}

	switch v.Policy {
	case PolicyEnforce, PolicyWarn:
	case "":
		errs = append(errs, Error{path + ".policy", "required when verification is enabled", "one of enforce, warn"})
	default:
		errs = append(errs, Error{path + ".policy", fmt.Sprintf("%q is not one of enforce, warn", v.Policy), ""})
	}

	if !v.AtSource && !v.AtDestination {
		errs = append(errs, Error{
			path,
			"neither atSource nor atDestination is set",
			"verification is enabled but would never run",
		})
	}

	switch v.Cosign.Mode {
	case CosignKeylessMode:
		if v.Cosign.Keyless == nil {
			errs = append(errs, Error{path + ".cosign.keyless", "required in keyless mode", ""})
			break
		}
		// THE important one. A keyless policy without an identity constraint
		// is syntactically fine and semantically useless: it verifies that
		// *someone* signed the artifact, not that the vendor did. Rejecting it
		// here prevents a configuration that looks secure and is not.
		if v.Cosign.Keyless.CertificateIdentity == "" {
			errs = append(errs, Error{
				path + ".cosign.keyless.certificateIdentity",
				"required in keyless mode",
				"without it, any valid Sigstore signature would be accepted — it would prove someone signed the artifact, not that the vendor did",
			})
		}
		if v.Cosign.Keyless.CertificateOidcIssuer == "" {
			errs = append(errs, Error{
				path + ".cosign.keyless.certificateOidcIssuer",
				"required in keyless mode",
				"without it, an identity from any OIDC issuer would be accepted",
			})
		}
		errs = append(errs, validateSecretRef(path+".cosign.keyless.rekorPublicKeysRef", v.Cosign.Keyless.RekorPublicKeysRef, resolver)...)
		errs = append(errs, validateSecretRef(path+".cosign.keyless.fulcioCertsRef", v.Cosign.Keyless.FulcioCertsRef, resolver)...)

	case CosignKeyMode:
		if v.Cosign.Key == nil || v.Cosign.Key.PublicKeyRef == nil {
			errs = append(errs, Error{path + ".cosign.key.publicKeyRef", "required in key mode", ""})
		} else {
			errs = append(errs, validateSecretRef(path+".cosign.key.publicKeyRef", v.Cosign.Key.PublicKeyRef, resolver)...)
		}

	case "":
		errs = append(errs, Error{path + ".cosign.mode", "required when verification is enabled", "one of keyless, key"})
	default:
		errs = append(errs, Error{path + ".cosign.mode", fmt.Sprintf("%q is not one of keyless, key", v.Cosign.Mode), ""})
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

// validateRepoCommon checks the fields a source and a target share.
//
// repositoryRequired is false for a source, whose repository set may come from
// `repositories` or from catalog enumeration. validateSourceRepositories owns
// that check, so requiring it here too would report a source using
// `repositoryDiscovery` as missing a field it deliberately does not set.
func validateRepoCommon(
	path, name, registry, repository string,
	typ RegistryType, anonymous bool, creds *CredentialsRef, resolver *SecretResolver,
	repositoryRequired bool,
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
		if repositoryRequired {
			errs = append(errs, Error{path + ".repository", "required", ""})
		}
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
		errs = append(errs, validateCredentialsRef(path+".credentialsRef", creds, resolver)...)
	}

	return errs
}

// validateCredentialsRef checks a username/password Secret reference and that
// the resolver can actually see both keys.
//
// Separate from validateRepoCommon because a credential reference is no longer
// only a repository's own: a Quay mirror carries the credentials QUAY will use
// to reach its upstream, which is a different credential in a different block
// checked the same way.
func validateCredentialsRef(path string, creds *CredentialsRef, resolver *SecretResolver) Errors {
	if creds == nil {
		return nil
	}
	if creds.SecretName == "" {
		return Errors{{path + ".secretName", "required", ""}}
	}
	if resolver == nil {
		return nil
	}
	var errs Errors
	for _, key := range []string{creds.UsernameKeyOrDefault(), creds.PasswordKeyOrDefault()} {
		if !resolver.Exists(creds.SecretName, key) {
			errs = append(errs, Error{
				path,
				fmt.Sprintf("secret %q has no key %q", creds.SecretName, key),
				"expected at " + resolver.Dir() + "/" + creds.SecretName + "/" + key,
			})
		}
	}
	return errs
}

// validateConcurrency checks the new block and the superseded ones it replaced.
//
// Both are checked because both are still honoured. A negative number in a
// `rateLimits` block that is folded forward would otherwise be silently
// discarded by the fold and never reported.
func validateConcurrency(path string, c Concurrency, r LegacyRateLimits, scan LegacyScanConcurrency) Errors {
	var errs Errors

	negative := func(name string, v int) {
		if v < 0 {
			errs = append(errs, Error{path + "." + name, fmt.Sprintf("%d must not be negative", v), ""})
		}
	}

	negative("concurrency.perRegistry", c.PerRegistry)
	negative("concurrency.requestsPerSecond", c.RequestsPerSecond)

	negative("rateLimits.maxConcurrentDownloads", r.MaxConcurrentDownloads)
	negative("rateLimits.maxConcurrentUploads", r.MaxConcurrentUploads)
	negative("rateLimits.maxConnections", r.MaxConnections)
	negative("rateLimits.requestsPerSecond", r.RequestsPerSecond)
	negative("rateLimits.burst", r.Burst)

	negative("discovery.concurrency.repositories", scan.Repositories)
	negative("discovery.concurrency.tags", scan.Tags)

	// A migrated document with a stale block is a real trap: the new value wins,
	// so the old one reads as configuration that is quietly doing nothing.
	if c.PerRegistry > 0 && (r.set() || scan.set()) {
		errs = append(errs, Error{
			path + ".rateLimits",
			"is ignored because concurrency.perRegistry is set",
			"delete the superseded block — leaving it in place makes the document " +
				"claim a limit that is not in force",
		})
	}

	if c.PerRegistry > maxPerRegistry {
		errs = append(errs, Error{
			path + ".concurrency.perRegistry",
			fmt.Sprintf("%d exceeds the maximum of %d", c.PerRegistry, maxPerRegistry),
			"this is the number of simultaneous connections to someone else's registry",
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
