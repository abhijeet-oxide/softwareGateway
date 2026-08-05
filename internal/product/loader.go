package product

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// Loader reads product documents from a directory of YAML files.
//
// sigs.k8s.io/yaml rather than gopkg.in/yaml.v3: it converts YAML to JSON and
// then uses encoding/json, so a document behaves identically whether it
// arrives as a ConfigMap or as a file, and the struct tags are `json`.
type Loader struct {
	dir      string
	resolver *SecretResolver

	// defaults are the application-level concurrency settings a product
	// inherits when it says nothing.
	//
	// Resolved HERE, at load, rather than at each point of use. Every consumer
	// then reads a number that is already final, which is what removes the
	// "was this set?" question from the scanner, the client factory and
	// preflight alike — three places that previously each had their own idea of
	// what a zero meant.
	defaults Concurrency
}

// NewLoader reads *.yaml and *.yml from dir. resolver may be nil to skip
// secret-existence checks, which is what `transferctl config validate` does
// when running offline in CI.
//
// Concurrency defaults to the shipped values; WithConcurrency overrides them
// from system configuration.
func NewLoader(dir string, resolver *SecretResolver) *Loader {
	return &Loader{
		dir:      dir,
		resolver: resolver,
		defaults: Concurrency{PerRegistry: DefaultPerRegistry},
	}
}

// WithConcurrency sets the application-level defaults products inherit.
func (l *Loader) WithConcurrency(c Concurrency) *Loader {
	l.defaults = c.Resolve(Concurrency{PerRegistry: DefaultPerRegistry})
	return l
}

// LoadResult is the outcome of loading a directory.
//
// Valid and Invalid are both populated: loading is fail-closed PER PRODUCT,
// not per directory. A syntax error in vendor-b.yaml must never stop vendor-a
// from replicating. See docs/design/02 section 7.
type LoadResult struct {
	Valid   []*Product
	Invalid []InvalidProduct
}

// InvalidProduct records a document that failed to load or validate.
type InvalidProduct struct {
	File string
	Name string // best-effort; may be empty if parsing failed early
	Err  error
}

// Load reads and validates every document in the directory.
//
// A missing directory is not an error — it yields an empty result. That keeps
// the zero-setup development path working before any product exists.
func (l *Loader) Load() (LoadResult, error) {
	var res LoadResult

	entries, err := os.ReadDir(l.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return res, nil
		}
		return res, fmt.Errorf("read products directory %s: %w", l.dir, err)
	}

	names := make(map[string]string) // product name -> file, for duplicate detection
	// physical repository -> "product/repositoryName", so a repository claimed
	// by two products is rejected. The repositories table has a unique index on
	// (registry_host, repository_path) and a NOT NULL product_id, so a shared
	// repository cannot be represented: reconciliation would flip ownership
	// between the two products on every config reload.
	physical := make(map[string]string)

	files := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		files = append(files, e.Name())
	}
	// Deterministic order so duplicate-name reporting is stable across runs.
	sort.Strings(files)

	for _, name := range files {
		path := filepath.Join(l.dir, name)
		p, err := l.LoadFile(path)
		if err != nil {
			productName := ""
			if p != nil {
				productName = p.Metadata.Name
			}
			res.Invalid = append(res.Invalid, InvalidProduct{File: path, Name: productName, Err: err})
			continue
		}

		if prev, dup := names[p.Metadata.Name]; dup {
			res.Invalid = append(res.Invalid, InvalidProduct{
				File: path,
				Name: p.Metadata.Name,
				Err: Errors{{
					Field:   "metadata.name",
					Message: fmt.Sprintf("%q is already declared in %s", p.Metadata.Name, prev),
					Hint:    "product names must be unique across the directory",
				}},
			})
			continue
		}

		if clashes := crossProductRepositoryClashes(p, physical); len(clashes) > 0 {
			res.Invalid = append(res.Invalid, InvalidProduct{File: path, Name: p.Metadata.Name, Err: clashes})
			continue
		}

		names[p.Metadata.Name] = path
		for _, ref := range p.Repositories() {
			physical[physicalKey(ref.Registry, ref.Repository)] = p.Metadata.Name + "/" + ref.Name
		}
		res.Valid = append(res.Valid, p)
	}

	return res, nil
}

// crossProductRepositoryClashes reports repositories this product declares that
// another product has already claimed.
//
// A physical repository belongs to exactly one product: `repositories` is
// uniquely indexed on (registry_host, repository_path) and its product_id is
// NOT NULL. Two products naming the same registry and path cannot both be
// stored, and reconciling them would flip ownership back and forth on every
// configuration reload — a silent, confusing failure. It is rejected here
// instead, before merge.
func crossProductRepositoryClashes(p *Product, claimed map[string]string) Errors {
	var errs Errors

	for i, s := range p.Spec.Sources {
		if owner, taken := claimed[physicalKey(s.Registry, s.Repository)]; taken {
			errs = append(errs, Error{
				fmt.Sprintf("spec.sources[%d]", i),
				fmt.Sprintf("%s/%s is already declared by %s", s.Registry, s.Repository, owner),
				"one physical repository belongs to exactly one product; give each product its own repository path",
			})
		}
	}
	for i, t := range p.Spec.Targets {
		if owner, taken := claimed[physicalKey(t.Registry, t.Repository)]; taken {
			errs = append(errs, Error{
				fmt.Sprintf("spec.targets[%d]", i),
				fmt.Sprintf("%s/%s is already declared by %s", t.Registry, t.Repository, owner),
				"one physical repository belongs to exactly one product; give each product its own repository path",
			})
		}
	}
	return errs
}

// LoadFile reads and validates one document. The returned Product is non-nil
// whenever parsing succeeded, even if validation failed, so callers can report
// which product was at fault.
func (l *Loader) LoadFile(path string) (*Product, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied config path
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	return l.Parse(b, path)
}

// Parse decodes and validates document bytes.
func (l *Loader) Parse(b []byte, source string) (*Product, error) {
	var p Product
	// UnmarshalStrict rejects unknown fields. A typo such as `tagPatern`
	// would otherwise be silently ignored, leaving a rule that never matches
	// and a user with no indication why.
	if err := yaml.UnmarshalStrict(b, &p); err != nil {
		return nil, fmt.Errorf("parse %s: %w", filepath.Base(source), err)
	}

	p.SourceFile = source
	sum := sha256.Sum256(b)
	p.ConfigHash = hex.EncodeToString(sum[:])

	// Validated BEFORE defaults are applied, so every complaint is about
	// something the operator actually wrote and every path in the message points
	// at a line in their file. Defaulting first would hide a value out of range
	// behind the clamp that fixed it, and would report a conflict between two
	// blocks that defaulting had just manufactured.
	if err := p.Validate(l.resolver); err != nil {
		return &p, err
	}

	p.applyDefaults(l.defaults)
	return &p, nil
}

// applyDefaults fills values that the schema documents as defaulted, so that
// downstream code never has to ask "was this set?".
//
// app is the application-level concurrency, from system configuration.
func (p *Product) applyDefaults(app Concurrency) {
	p.Deprecations = nil

	for i := range p.Spec.Sources {
		s := &p.Spec.Sources[i]
		if s.Type == "" {
			s.Type = RegistryGeneric
		}
		if s.Discovery.Interval == 0 {
			s.Discovery.Interval = Duration(DefaultDiscoveryInterval)
		}

		folded, notes := foldLegacy(s.Concurrency, s.RateLimits, s.Discovery.Concurrency,
			"sources["+s.Name+"]")
		s.Concurrency = folded.Resolve(app)
		p.Deprecations = append(p.Deprecations, notes...)
	}

	for i := range p.Spec.Targets {
		t := &p.Spec.Targets[i]
		if t.Type == "" {
			t.Type = RegistryGeneric
		}

		folded, notes := foldLegacy(t.Concurrency, t.RateLimits, LegacyScanConcurrency{},
			"targets["+t.Name+"]")
		t.Concurrency = folded.Resolve(app)
		p.Deprecations = append(p.Deprecations, notes...)
	}
}

// Credentials resolves a source's credentials.
func (l *Loader) Credentials(s Source) (Credentials, error) {
	if s.Anonymous || s.CredentialsRef == nil {
		return Credentials{}, nil
	}
	if l.resolver == nil {
		return Credentials{}, fmt.Errorf("no secret resolver configured")
	}
	return l.resolver.Credentials(*s.CredentialsRef)
}

// TargetCredentials resolves a target's credentials.
func (l *Loader) TargetCredentials(t Target) (Credentials, error) {
	if t.Anonymous || t.CredentialsRef == nil {
		return Credentials{}, nil
	}
	if l.resolver == nil {
		return Credentials{}, fmt.Errorf("no secret resolver configured")
	}
	return l.resolver.Credentials(*t.CredentialsRef)
}
