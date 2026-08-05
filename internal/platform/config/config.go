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

type GCConfig struct {
	TickInterval time.Duration `koanf:"tickInterval"`
	BatchSize    int           `koanf:"batchSize"`
}

type WorkerConfig struct {
	CoordinatorEndpoint string        `koanf:"coordinatorEndpoint"`
	WorkerID            string        `koanf:"workerID"`
	Address             string        `koanf:"address"`
	MaxConcurrentJobs   int           `koanf:"maxConcurrentJobs"`
	CopyBufferSize      int64         `koanf:"copyBufferSize"`
	HeartbeatInterval   time.Duration `koanf:"heartbeatInterval"`
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
// no setup at all — see docs/design/14 section 5.1. It is explicitly not
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
			GC:    GCConfig{TickInterval: time.Hour, BatchSize: 5000},
		},
		Worker: WorkerConfig{
			CoordinatorEndpoint: "http://localhost:8080",
			Address:             ":8081",
			MaxConcurrentJobs:   16,
			CopyBufferSize:      1 << 20, // 1 MiB
			HeartbeatInterval:   20 * time.Second,
		},
		Observability: ObservabilityConfig{
			Log:     LogConfig{Level: "info", Format: "json"},
			Metrics: MetricsConfig{Enabled: true, Path: "/metrics"},
			Tracing: TracingConfig{Enabled: false, SampleRatio: 0.05},
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
// A missing file is not an error — the defaults plus environment must be
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

	// SWGW_DATABASE_DSN -> database.dsn
	err := k.Load(env.Provider(EnvPrefix, ".", func(s string) string {
		return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(s, EnvPrefix)), "_", ".")
	}), nil)
	if err != nil {
		return SystemConfig{}, fmt.Errorf("load environment: %w", err)
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
