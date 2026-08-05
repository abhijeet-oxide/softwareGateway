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
}

// NewLoader reads *.yaml and *.yml from dir. resolver may be nil to skip
// secret-existence checks, which is what `transferctl config validate` does
// when running offline in CI.
func NewLoader(dir string, resolver *SecretResolver) *Loader {
	return &Loader{dir: dir, resolver: resolver}
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
		names[p.Metadata.Name] = path
		res.Valid = append(res.Valid, p)
	}

	return res, nil
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

	p.applyDefaults()

	if err := p.Validate(l.resolver); err != nil {
		return &p, err
	}
	return &p, nil
}

// applyDefaults fills values that the schema documents as defaulted, so that
// downstream code never has to ask "was this set?".
func (p *Product) applyDefaults() {
	for i := range p.Spec.Sources {
		s := &p.Spec.Sources[i]
		if s.Type == "" {
			s.Type = RegistryGeneric
		}
		if s.Discovery.Interval == 0 {
			s.Discovery.Interval = Duration(DefaultDiscoveryInterval)
		}
		s.RateLimits = s.RateLimits.WithDefaults()
		// Sources are read-only; an upload budget would be meaningless.
		s.RateLimits.MaxConcurrentUploads = 0
	}
	for i := range p.Spec.Targets {
		t := &p.Spec.Targets[i]
		if t.Type == "" {
			t.Type = RegistryGeneric
		}
		t.RateLimits = t.RateLimits.WithDefaults()
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
