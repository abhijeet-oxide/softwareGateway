package registry

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// ClientConfig is everything a backend needs to build a repository client.
//
// Deliberately flat and free of configuration types: this package must not
// import internal/product, or the registry layer would know about ConfigMaps,
// secrets and reload semantics that have nothing to do with speaking HTTP to a
// registry. The translation from a configured product to this struct happens in
// the caller.
type ClientConfig struct {
	// Type selects the backend: "generic", "acr", "artifactory", "quay".
	// Empty means generic.
	Type string

	Registry   string
	Repository string

	Username string
	Password string

	RequestsPerSecond int
	Burst             int
	MaxConnections    int

	CABundle   []byte
	HTTPSProxy string
	NoProxy    []string

	ConnectTimeout        time.Duration
	ResponseHeaderTimeout time.Duration

	UserAgent string
	// PlainHTTP talks http:// rather than https://. Development registries
	// only.
	PlainHTTP bool

	MaxRetryAttempts int
	RetryBaseDelay   time.Duration
	RetryMaxDelay    time.Duration
}

// Constructor builds a Source for one repository.
//
// Source rather than Repository: M2 reads. A backend that cannot yet write must
// not be forced to claim it can (see the interface split in registry.go).
type Constructor func(ClientConfig) (Source, error)

var (
	registryMu   sync.RWMutex
	constructors = map[string]Constructor{}
)

// Register makes a backend available to New.
//
// Called from each backend's init, so importing the backend package is what
// enables it. That keeps the dependency pointing the right way — this package
// does not import generic, acr or quay, so a vendor backend can be added
// without touching the abstraction.
func Register(name string, c Constructor) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := constructors[name]; exists {
		// A duplicate registration means two packages claim the same name, and
		// which one wins would depend on import order. Loud is correct.
		panic("registry: backend " + name + " registered twice")
	}
	constructors[name] = c
}

// DefaultType is the backend used when configuration names none.
//
// Generic is the expected path for ALL four supported registries. The vendor
// types exist for genuine deviations, not as the normal case — a configuration
// that names `acr` when generic works is choosing a narrower code path for no
// benefit.
const DefaultType = "generic"

// New builds a client for the configured backend.
func New(cfg ClientConfig) (Source, error) {
	name := cfg.Type
	if name == "" {
		name = DefaultType
	}

	registryMu.RLock()
	c, ok := constructors[name]
	registryMu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("no registry backend %q is registered (available: %v)", name, Backends())
	}
	return c(cfg)
}

// Backends lists the registered backend names, sorted.
func Backends() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()

	out := make([]string, 0, len(constructors))
	for name := range constructors {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
