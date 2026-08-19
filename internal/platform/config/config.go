// Package config loads deployment-scoped system configuration.
//
// See docs/design/02-configuration.md section 8. Precedence is
// flag -> SWGW_ env -> file -> default.
//
// This is distinct from product configuration (internal/product), which is
// GitOps-managed data. System config is an operator concern: addresses,
// database DSN, tick intervals, log level.
package config

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
)

// EnvPrefix namespaces environment overrides: SWGW_DATABASE_DSN maps to
// database.dsn.
const EnvPrefix = "SWGW_"

// SystemConfig is the deployment-scoped configuration document.
type SystemConfig struct {
	ConfigDir string `koanf:"configDir"`

	Server        ServerConfig        `koanf:"server"`
	Database      DatabaseConfig      `koanf:"database"`
	Coordinator   CoordinatorConfig   `koanf:"coordinator"`
	Worker        WorkerConfig        `koanf:"worker"`
	Observability ObservabilityConfig `koanf:"observability"`
	Retention     RetentionConfig     `koanf:"retention"`
	TLS           TLSConfig           `koanf:"tls"`
	Concurrency   ConcurrencyConfig   `koanf:"concurrency"`
}

// TLSConfig relaxes certificate handling for the WHOLE PROCESS.
//
// Deliberately here and not in product configuration, unlike
// network.tls.insecureSkipVerify. These settings are implemented by Go's
// GODEBUG mechanism, which is per process and cannot be scoped to one
// connection - so pretending they were per repository would be a lie about
// their blast radius. An operator concern, in the operator's config file.
type TLSConfig struct {
	// AllowNegativeSerialNumbers accepts certificates whose serial number is
	// negative, which crypto/x509 has rejected since Go 1.23.
	//
	// This is the fix for:
	//
	//	tls: failed to parse certificate from server: x509: negative serial number
	//
	// And it is the ONLY fix. That error happens while PARSING the server's
	// certificate - before any verification runs - so
	// network.tls.insecureSkipVerify does not help, and neither does a CA
	// bundle. Measured on Go 1.25.7, not reasoned about: with
	// insecureSkipVerify alone the handshake fails with the identical message.
	//
	// RFC 5280 §4.1.2.2 requires a positive serial number, so a certificate
	// with a negative one is malformed. Some appliance and enterprise CAs emit
	// them anyway by encoding a random 20-byte value without clearing the high
	// bit. The certificate is otherwise fine; the standard library is simply
	// stricter than the estate.
	//
	// Process-wide, and logged as such at startup.
	AllowNegativeSerialNumbers bool `koanf:"allowNegativeSerialNumbers"`
}

// ConcurrencyConfig is how hard this installation works any one registry.
//
// It lives here, at the application level, because it is an operational
// property of the DEPLOYMENT - the bandwidth it has, the proxy it sits behind,
// the politeness its vendors expect - and not of any one product. Every product
// inherits it; a product may override it per source or per target for the case
// that genuinely differs, which is one fragile vendor rather than the rule.
//
// See product.Concurrency for why this is one number rather than seven.
type ConcurrencyConfig struct {
	// PerRegistry is the number of requests in flight against one registry, and
	// the size of the connection pool serving them.
	PerRegistry int `koanf:"perRegistry"`

	// RequestsPerSecond is an optional politeness ceiling on top of it. Zero,
	// the default, means no artificial limit.
	RequestsPerSecond int `koanf:"requestsPerSecond"`
}

type ServerConfig struct {
	Address             string        `koanf:"address"`
	ShutdownGracePeriod time.Duration `koanf:"shutdownGracePeriod"`
}

type DatabaseConfig struct {
	Driver          string        `koanf:"driver"` // postgres | sqlite
	DSN             string        `koanf:"dsn"`
	MaxOpenConns    int           `koanf:"maxOpenConns"`
	MaxIdleConns    int           `koanf:"maxIdleConns"`
	ConnMaxLifetime time.Duration `koanf:"connMaxLifetime"`
}

type CoordinatorConfig struct {
	LeaderElection LeaderElectionConfig `koanf:"leaderElection"`
	Scheduler      TickConfig           `koanf:"scheduler"`
	Reaper         ReaperConfig         `koanf:"reaper"`
	Queue          QueueConfig          `koanf:"queue"`
	GC             GCConfig             `koanf:"gc"`
	ManifestCache  ManifestCacheConfig  `koanf:"manifestCache"`
}

// ManifestCacheConfig bounds the cached manifest bodies.
//
// A package's manifest BODIES are the only thing this system records that grows
// without limit and can be discarded without losing a fact. Everything else it
// knows about a package - the artifacts, their digests and sizes, the blobs
// they reference, the totals - is a few kilobytes and is kept forever.
//
// The bodies are large, are read only when a manifest is PUSHED, and are
// exactly recoverable, because a manifest is addressed by the hash of its own
// bytes. So they are a cache, and this is its size. Over a vendor catalogue
// accumulated across years, an unbounded one would be the largest thing in the
// database by a wide margin.
//
// See internal/store/manifestcache.go.
type ManifestCacheConfig struct {
	// BudgetBytes is the ceiling. Zero disables the budget entirely, for a
	// deployment that would rather spend disk than ever re-fetch.
	BudgetBytes int64 `koanf:"budgetBytes"`
	// TTL evicts bodies untouched for longer than this, whatever the budget
	// says. It is what keeps a deployment that discovers far more than it
	// transfers from carrying a full budget of manifests nobody will push.
	// Zero disables the age pass.
	TTL time.Duration `koanf:"ttl"`
	// SweepInterval is how often the sweeper runs. It is a cheap query against
	// a partial index, so this is minutes rather than hours; the point of
	// sweeping often is that the cache stays near its budget instead of sawing
	// between empty and over.
	SweepInterval time.Duration `koanf:"sweepInterval"`
}

type LeaderElectionConfig struct {
	Enabled bool  `koanf:"enabled"`
	LockID  int64 `koanf:"lockID"`
	// RetryInterval is how often a follower attempts to acquire leadership.
	RetryInterval time.Duration `koanf:"retryInterval"`
}

type TickConfig struct {
	TickInterval time.Duration `koanf:"tickInterval"`
}

type ReaperConfig struct {
	TickInterval  time.Duration `koanf:"tickInterval"`
	LeaseDuration time.Duration `koanf:"leaseDuration"`
}

type QueueConfig struct {
	MaxLeaseBatchSize int `koanf:"maxLeaseBatchSize"`
}

// GCConfig bounds how much HISTORY the database keeps.
//
// Three tables grow with USE rather than with the size of the catalogue, and
// they are the whole problem: `jobs` at roughly 2500 rows per transfer, and one
// transfer per release per target; `worker_logs`, a row per interesting thing a
// worker did; `audit_events`, a row per state transition. Everything else grows
// with the CATALOGUE and is the answer to "what does this vendor publish",
// which is the point of the system - none of it expires.
//
// Every duration is zero-means-keep-forever, so a deployment that wants an
// unbounded audit trail gets one by leaving the field unset rather than by
// finding a switch to turn something off.
//
// See internal/store/retention.go for what each sweep may and may not touch.
type GCConfig struct {
	TickInterval time.Duration `koanf:"tickInterval"`
	// BatchSize bounds one pass, so the first sweep of a database that has run
	// unbounded for a year does not hold a lock for minutes.
	BatchSize int `koanf:"batchSize"`

	// Transfers deletes SETTLED transfers, and their jobs with them, once they
	// have been finished for this long.
	//
	// Settled rather than merely old: a transfer running for a month is one
	// somebody is watching, and deleting it out from under them would look
	// exactly like the data loss this sweep exists to avoid being blamed for.
	Transfers time.Duration `koanf:"transfers"`
	// WorkerLogs expires the convenience tail. It is not a log store - cluster
	// log aggregation remains the system of record.
	WorkerLogs time.Duration `koanf:"workerLogs"`
	// AuditEvents expires the audit trail. Longest of the three by default, and
	// reasonably set to zero: an audit trail with a short retention is not an
	// audit trail.
	AuditEvents time.Duration `koanf:"auditEvents"`
	// Placements expires blob placement records not confirmed in this long.
	//
	// Zero, and worth leaving zero. This table is the memory that makes a
	// second transfer of a product line nearly free, losing a row costs a HEAD
	// per blob on the next transfer, and the whole table is measured in tens of
	// thousands of rows.
	Placements time.Duration `koanf:"placements"`
}

type WorkerConfig struct {
	CoordinatorEndpoint string        `koanf:"coordinatorEndpoint"`
	WorkerID            string        `koanf:"workerID"`
	Address             string        `koanf:"address"`
	MaxConcurrentJobs   int           `koanf:"maxConcurrentJobs"`
	CopyBufferSize      int64         `koanf:"copyBufferSize"`
	HeartbeatInterval   time.Duration `koanf:"heartbeatInterval"`
	// StallTimeout is how long one job may make no progress before this worker
	// abandons it so another attempt can run. Zero uses the default; negative
	// disables the check.
	//
	// It is not a deadline on the job: a job that is transferring resets the
	// clock on every progress report, so a large blob may legitimately run for
	// hours. It bounds only silence.
	StallTimeout time.Duration `koanf:"stallTimeout"`
}

type ObservabilityConfig struct {
	Log     LogConfig     `koanf:"log"`
	Metrics MetricsConfig `koanf:"metrics"`
	Tracing TracingConfig `koanf:"tracing"`
}

type LogConfig struct {
	Level  string `koanf:"level"`
	Format string `koanf:"format"`
}

type MetricsConfig struct {
	Enabled bool   `koanf:"enabled"`
	Path    string `koanf:"path"`
}

type TracingConfig struct {
	Enabled     bool    `koanf:"enabled"`
	Endpoint    string  `koanf:"endpoint"`
	SampleRatio float64 `koanf:"sampleRatio"`
}

type RetentionConfig struct {
	CompletedJobs       time.Duration `koanf:"completedJobs"`
	QueueHistory        time.Duration `koanf:"queueHistory"`
	DiscoveryHistory    time.Duration `koanf:"discoveryHistory"`
	NotificationHistory time.Duration `koanf:"notificationHistory"`
	AuditHistory        time.Duration `koanf:"auditHistory"`
}

// Defaults returns the shipped defaults.
//
// SQLite is the development default so `go run ./cmd/coordinator` works with
// no setup at all - see docs/design/14 section 5.1. It is explicitly not
// supported in production, and the Coordinator warns at startup.
func Defaults() SystemConfig {
	return SystemConfig{
		ConfigDir: "/etc/softwaregateway",
		Server: ServerConfig{
			Address:             ":8080",
			ShutdownGracePeriod: 30 * time.Second,
		},
		Database: DatabaseConfig{
			Driver:          "sqlite",
			DSN:             "./dev/swgw.db",
			MaxOpenConns:    25,
			MaxIdleConns:    10,
			ConnMaxLifetime: time.Hour,
		},
		Coordinator: CoordinatorConfig{
			LeaderElection: LeaderElectionConfig{
				Enabled:       true,
				LockID:        1,
				RetryInterval: 10 * time.Second,
			},
			Scheduler: TickConfig{TickInterval: 10 * time.Second},
			Reaper: ReaperConfig{
				TickInterval:  30 * time.Second,
				LeaseDuration: 2 * time.Minute,
			},
			Queue: QueueConfig{MaxLeaseBatchSize: 32},
			GC: GCConfig{
				TickInterval: time.Hour,
				BatchSize:    5000,
				// Ninety days of transfer history and thirty of worker logs.
				//
				// Chosen against what each is FOR. A settled transfer's rows
				// answer "what did that run do", which is asked in the days
				// after it and effectively never a quarter later; the content
				// it moved is at the destination and what we know about the
				// source is in the catalogue, so nothing recoverable is lost.
				// Worker logs answer "why did that job fail", which is asked
				// the same week.
				//
				// The audit trail and the placement cache are left unbounded on
				// purpose: an audit trail with a short retention is not an audit
				// trail, and the placements are what make a re-transfer nearly
				// free.
				Transfers:  90 * 24 * time.Hour,
				WorkerLogs: 30 * 24 * time.Hour,
			},
			ManifestCache: ManifestCacheConfig{
				// 512 MiB and a week.
				//
				// Sized against what it is FOR rather than against available
				// disk. A manifest body is a few kilobytes, so this holds on
				// the order of a hundred thousand of them - far more than any
				// plausible working set of packages being replicated in a
				// week, and a small fraction of the volume a database this
				// system runs on would be given. The TTL is the bound that
				// usually bites; the budget is the one that stops a bulk
				// inspection of an entire catalogue from being unbounded.
				//
				// Both are safe to lower aggressively: the cost of a miss is
				// re-fetching a few kilobytes from the source registry at
				// transfer time, and nothing else.
				BudgetBytes:   512 << 20,
				TTL:           7 * 24 * time.Hour,
				SweepInterval: 15 * time.Minute,
			},
		},
		Worker: WorkerConfig{
			CoordinatorEndpoint: "http://localhost:8080",
			Address:             ":8081",
			MaxConcurrentJobs:   16,
			CopyBufferSize:      1 << 20, // 1 MiB
			HeartbeatInterval:   20 * time.Second,
			StallTimeout:        15 * time.Minute,
		},
		Observability: ObservabilityConfig{
			Log:     LogConfig{Level: "info", Format: "json"},
			Metrics: MetricsConfig{Enabled: true, Path: "/metrics"},
			Tracing: TracingConfig{Enabled: false, SampleRatio: 0.05},
		},
		Concurrency: ConcurrencyConfig{
			// Matches what the previous seven knobs multiplied out to, so this
			// simplification changes the shape of the configuration and not the
			// load it produces.
			PerRegistry:       32,
			RequestsPerSecond: 0,
		},
		Retention: RetentionConfig{
			CompletedJobs:       7 * 24 * time.Hour,
			QueueHistory:        7 * 24 * time.Hour,
			DiscoveryHistory:    90 * 24 * time.Hour,
			NotificationHistory: 30 * 24 * time.Hour,
			AuditHistory:        365 * 24 * time.Hour,
		},
	}
}

// Load resolves configuration in precedence order: defaults, then the file if
// present, then SWGW_ environment variables.
//
// A missing file is not an error - the defaults plus environment must be
// enough to start, which is what makes the zero-setup development path work.
func Load(path string) (SystemConfig, error) {
	k := koanf.New(".")

	if err := k.Load(structs.Provider(Defaults(), "koanf"), nil); err != nil {
		return SystemConfig{}, fmt.Errorf("load defaults: %w", err)
	}

	if path != "" {
		if _, err := os.Stat(path); err == nil {
			if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
				return SystemConfig{}, fmt.Errorf("load config file %s: %w", path, err)
			}
		} else if !os.IsNotExist(err) {
			return SystemConfig{}, fmt.Errorf("stat config file %s: %w", path, err)
		}
	}

	// SWGW_DATABASE_DSN -> database.dsn, SWGW_DATABASE_MAXOPENCONNS ->
	// database.maxOpenConns.
	//
	// The second form is why this needs a lookup table rather than a string
	// transform. An environment variable cannot carry case, so the naive
	// mapping produces `database.maxopenconns` - which is a DIFFERENT koanf key
	// from `database.maxOpenConns` and therefore binds to nothing. The override
	// was silently ignored, which is the worst possible failure for a
	// configuration mechanism: the operator sets the variable, sees no error,
	// and gets the default.
	//
	// Resolving against the canonical keys makes every setting reachable and
	// makes an unknown variable detectable.
	canonical := canonicalKeys(k.Keys())
	var unknown []string

	err := k.Load(env.Provider(EnvPrefix, ".", func(s string) string {
		flat := strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(s, EnvPrefix)), "_", ".")
		if key, ok := canonical[flat]; ok {
			return key
		}
		unknown = append(unknown, s)
		// Returning "" tells koanf to skip the variable. Passing the unmatched
		// key through would reintroduce the silent-no-op this exists to fix.
		return ""
	}), nil)
	if err != nil {
		return SystemConfig{}, fmt.Errorf("load environment: %w", err)
	}
	if len(unknown) > 0 {
		// Fail rather than warn. A typo'd SWGW_ variable means the operator
		// believes they have changed something they have not, and finding that
		// out during an incident is far more expensive than at startup.
		sort.Strings(unknown)
		return SystemConfig{}, fmt.Errorf(
			"unknown environment variable(s): %s (no such configuration key; "+
				"names are SWGW_ plus the config path with dots as underscores, "+
				"e.g. SWGW_DATABASE_MAXOPENCONNS for database.maxOpenConns)",
			strings.Join(unknown, ", "))
	}

	var cfg SystemConfig
	if err := k.UnmarshalWithConf("", &cfg, koanf.UnmarshalConf{Tag: "koanf"}); err != nil {
		return SystemConfig{}, fmt.Errorf("unmarshal config: %w", err)
	}

	// Expand ${VAR} in the DSN so a manifest can reference a secret env var
	// without the literal ever appearing in a config file.
	cfg.Database.DSN = os.ExpandEnv(cfg.Database.DSN)

	if err := cfg.Validate(); err != nil {
		return SystemConfig{}, err
	}
	return cfg, nil
}

// canonicalKeys maps each lowercased config path to its real, cased form.
//
// Built from the defaults, which by construction contain every key the struct
// defines - so a key that exists in the schema is reachable from the
// environment, and one that does not is detected as a typo.
func canonicalKeys(keys []string) map[string]string {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[strings.ToLower(k)] = k
	}
	return out
}

// Validate rejects configurations that cannot work.
func (c SystemConfig) Validate() error {
	switch c.Database.Driver {
	case "postgres", "sqlite":
	default:
		return fmt.Errorf("database.driver: %q is not one of postgres, sqlite", c.Database.Driver)
	}
	if c.Database.DSN == "" {
		return fmt.Errorf("database.dsn: required")
	}
	if c.Server.Address == "" {
		return fmt.Errorf("server.address: required")
	}
	if r := c.Observability.Tracing.SampleRatio; r < 0 || r > 1 {
		return fmt.Errorf("observability.tracing.sampleRatio: %v is outside [0,1]", r)
	}
	if c.Coordinator.ManifestCache.BudgetBytes < 0 {
		return fmt.Errorf("coordinator.manifestCache.budgetBytes: must not be negative (0 disables the budget)")
	}
	if c.Coordinator.ManifestCache.TTL < 0 {
		return fmt.Errorf("coordinator.manifestCache.ttl: must not be negative (0 disables expiry)")
	}
	if c.Coordinator.Reaper.LeaseDuration <= c.Coordinator.Reaper.TickInterval {
		return fmt.Errorf(
			"coordinator.reaper.leaseDuration (%v) must exceed tickInterval (%v), "+
				"or leases expire faster than the reaper can observe them",
			c.Coordinator.Reaper.LeaseDuration, c.Coordinator.Reaper.TickInterval)
	}
	return nil
}

// ProductsDir is where per-product ConfigMaps are projected.
func (c SystemConfig) ProductsDir() string { return c.ConfigDir + "/products" }

// SecretsDir is where VSO-managed Secrets are projected.
func (c SystemConfig) SecretsDir() string { return c.ConfigDir + "/secrets" }

// IsProduction reports whether the store is production-grade. Used to warn at
// startup that SQLite is a development convenience only.
func (c SystemConfig) IsProduction() bool { return c.Database.Driver == "postgres" }
