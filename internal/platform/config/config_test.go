package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultsLoadWithNoFile(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("defaults must load with no file: %v", err)
	}
	if cfg.Server.Address != ":8080" || cfg.Database.Driver != "sqlite" {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
}

// A missing file is not an error: defaults plus environment must be enough to
// start, which is what makes the zero-setup development path work.
func TestMissingFileIsNotAnError(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err != nil {
		t.Errorf("a missing config file must not fail the load: %v", err)
	}
}

func TestFileOverridesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  address: "127.0.0.1:9999"
database:
  driver: postgres
  dsn: "postgres://localhost/x?sslmode=disable"
  maxOpenConns: 3
`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Server.Address != "127.0.0.1:9999" {
		t.Errorf("address = %q", cfg.Server.Address)
	}
	if cfg.Database.MaxOpenConns != 3 {
		t.Errorf("maxOpenConns = %d, want 3", cfg.Database.MaxOpenConns)
	}
	// Untouched keys keep their defaults.
	if cfg.Database.MaxIdleConns != 10 {
		t.Errorf("maxIdleConns = %d, want the default 10", cfg.Database.MaxIdleConns)
	}
}

// Every setting must be reachable from the environment, including camelCase
// keys.
//
// This is a regression test with teeth. The original mapper lowercased the
// variable name into a koanf path, so SWGW_DATABASE_MAXOPENCONNS produced
// `database.maxopenconns` — a different key from `database.maxOpenConns`, which
// bound to nothing. The override was accepted in silence and the default was
// used, which is the worst failure mode a configuration mechanism has.
func TestEnvironmentOverridesEveryKeyShape(t *testing.T) {
	tests := []struct {
		env   string
		value string
		check func(SystemConfig) (any, any) // got, want
	}{
		// Single lowercase word — worked before, must keep working.
		{"SWGW_SERVER_ADDRESS", "127.0.0.1:7000",
			func(c SystemConfig) (any, any) { return c.Server.Address, "127.0.0.1:7000" }},
		{"SWGW_OBSERVABILITY_LOG_LEVEL", "debug",
			func(c SystemConfig) (any, any) { return c.Observability.Log.Level, "debug" }},

		// camelCase leaf — silently ignored before the fix.
		{"SWGW_DATABASE_MAXOPENCONNS", "7",
			func(c SystemConfig) (any, any) { return c.Database.MaxOpenConns, 7 }},
		{"SWGW_SERVER_SHUTDOWNGRACEPERIOD", "45s",
			func(c SystemConfig) (any, any) { return c.Server.ShutdownGracePeriod, 45 * time.Second }},

		// camelCase SECTION — also silently ignored before the fix.
		{"SWGW_COORDINATOR_LEADERELECTION_ENABLED", "false",
			func(c SystemConfig) (any, any) { return c.Coordinator.LeaderElection.Enabled, false }},
		{"SWGW_COORDINATOR_LEADERELECTION_RETRYINTERVAL", "3s",
			func(c SystemConfig) (any, any) { return c.Coordinator.LeaderElection.RetryInterval, 3 * time.Second }},

		// Top-level key.
		{"SWGW_CONFIGDIR", "/tmp/swgw",
			func(c SystemConfig) (any, any) { return c.ConfigDir, "/tmp/swgw" }},

		// An acronym-cased key, which lowercases differently again.
		{"SWGW_COORDINATOR_LEADERELECTION_LOCKID", "42",
			func(c SystemConfig) (any, any) { return c.Coordinator.LeaderElection.LockID, int64(42) }},
	}

	for _, tc := range tests {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv(tc.env, tc.value)

			cfg, err := Load("")
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			got, want := tc.check(cfg)
			if got != want {
				t.Errorf("%s=%s gave %v, want %v", tc.env, tc.value, got, want)
			}
		})
	}
}

// A typo'd variable must fail the load rather than be ignored. An operator who
// believes they have changed something they have not will find out during an
// incident otherwise.
func TestUnknownEnvironmentVariableIsRejected(t *testing.T) {
	t.Setenv("SWGW_DATABASE_MAXOPENCONNECTIONS", "7") // real key is maxOpenConns

	_, err := Load("")
	if err == nil {
		t.Fatal("expected an unknown SWGW_ variable to be rejected")
	}
	if !strings.Contains(err.Error(), "SWGW_DATABASE_MAXOPENCONNECTIONS") {
		t.Errorf("the error must name the offending variable, got: %v", err)
	}
}

// Non-SWGW variables must be left alone: the process environment is full of
// them and none is ours.
func TestUnrelatedEnvironmentIsIgnored(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://elsewhere/db")
	t.Setenv("PATH_TO_NOWHERE", "x")

	if _, err := Load(""); err != nil {
		t.Errorf("unrelated environment must not affect the load: %v", err)
	}
}

func TestEnvironmentBeatsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  address: \":1111\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SWGW_SERVER_ADDRESS", ":2222")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Server.Address != ":2222" {
		t.Errorf("environment must beat the file, got %q", cfg.Server.Address)
	}
}

// The DSN expands ${VAR} so a manifest can reference a secret without the
// literal appearing in a config file.
func TestDSNExpandsEnvironment(t *testing.T) {
	t.Setenv("PGPASSWORD", "s3cret")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(
		"database:\n  driver: postgres\n  dsn: \"postgres://u:${PGPASSWORD}@h/db\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !strings.Contains(cfg.Database.DSN, "s3cret") {
		t.Errorf("expected ${PGPASSWORD} to expand, got %q", cfg.Database.DSN)
	}
}

func TestValidateRejectsUnworkableConfigurations(t *testing.T) {
	tests := map[string]func(*SystemConfig){
		"unknown driver":    func(c *SystemConfig) { c.Database.Driver = "mysql" },
		"empty dsn":         func(c *SystemConfig) { c.Database.DSN = "" },
		"empty address":     func(c *SystemConfig) { c.Server.Address = "" },
		"sample ratio > 1":  func(c *SystemConfig) { c.Observability.Tracing.SampleRatio = 1.5 },
		"sample ratio < 0":  func(c *SystemConfig) { c.Observability.Tracing.SampleRatio = -0.1 },
		"lease below tick":  func(c *SystemConfig) { c.Coordinator.Reaper.LeaseDuration = time.Second },
		"lease equals tick": func(c *SystemConfig) { c.Coordinator.Reaper.LeaseDuration = c.Coordinator.Reaper.TickInterval },
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := Defaults()
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Error("expected validation to reject this configuration")
			}
		})
	}
}

func TestProductAndSecretDirsDeriveFromConfigDir(t *testing.T) {
	c := SystemConfig{ConfigDir: "/etc/softwaregateway"}
	if c.ProductsDir() != "/etc/softwaregateway/products" {
		t.Errorf("ProductsDir = %q", c.ProductsDir())
	}
	if c.SecretsDir() != "/etc/softwaregateway/secrets" {
		t.Errorf("SecretsDir = %q", c.SecretsDir())
	}
}

// TestTLSKeyIsReachableFromTheEnvironment guards the canonical-key mapping for
// the newest setting. A camelCase key that the env provider cannot reach is
// silently ignored, which is how tls.allowNegativeSerialNumbers would look
// exactly like a setting that does not work.
func TestTLSKeyIsReachableFromTheEnvironment(t *testing.T) {
	t.Setenv("SWGW_TLS_ALLOWNEGATIVESERIALNUMBERS", "true")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.TLS.AllowNegativeSerialNumbers {
		t.Error("SWGW_TLS_ALLOWNEGATIVESERIALNUMBERS did not reach the config")
	}
}

func TestTLSDefaultsToStrict(t *testing.T) {
	if Defaults().TLS.AllowNegativeSerialNumbers {
		t.Error("relaxed X.509 parsing must be opt-in")
	}
}
