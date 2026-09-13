package unit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/syleron/pulseha/packages/config"
)

func TestSyslogConfigDefaults(t *testing.T) {
	// Set test environment
	os.Setenv("PULSEHA_TEST", "true")
	defer os.Unsetenv("PULSEHA_TEST")

	cfg, err := config.New()
	assert.NoError(t, err, "New should not return error in test mode")

	assert.True(t, cfg.Pulse.LogToSyslog, "Syslog should be enabled by default")
	assert.Equal(t, "", cfg.Pulse.SyslogNetwork, "Default syslog network should be empty (local)")
	assert.Equal(t, "", cfg.Pulse.SyslogAddress, "Default syslog address should be empty (local)")
	assert.Equal(t, "LOG_INFO", cfg.Pulse.SyslogFacility, "Default syslog facility should be LOG_INFO")
	assert.Equal(t, "pulseha", cfg.Pulse.SyslogTag, "Default syslog tag should be 'pulseha'")
}

func TestSyslogConfigMigration(t *testing.T) {
	// Set test environment
	os.Setenv("PULSEHA_TEST", "true")
	defer os.Unsetenv("PULSEHA_TEST")

	// Create old config without syslog fields
	oldConfigJSON := `{
		"pulseha": {
			"hcs_interval": 1000,
			"fos_interval": 5000,
			"fo_limit": 10000,
			"local_node": "test-node",
			"logging_level": "info",
			"auto_failback": true,
			"log_to_file": true,
			"log_file_location": "/tmp/test.log",
			"mode": "active-passive"
		},
		"floating_ip_groups": {},
		"nodes": {},
		"plugins": {}
	}`

	var cfg config.Config
	err := json.Unmarshal([]byte(oldConfigJSON), &cfg)
	assert.NoError(t, err, "Should unmarshal old config format")

	// Syslog fields should be empty (zero values)
	assert.False(t, cfg.Pulse.LogToSyslog, "Old config should have false for LogToSyslog")
	assert.Equal(t, "", cfg.Pulse.SyslogTag, "Old config should have empty SyslogTag")
	assert.Equal(t, "", cfg.Pulse.SyslogFacility, "Old config should have empty SyslogFacility")
}

func TestSyslogConfigValidation(t *testing.T) {
	// Set test environment
	os.Setenv("PULSEHA_TEST", "true")
	defer os.Unsetenv("PULSEHA_TEST")

	testCases := []struct {
		name     string
		config   config.Local
		expectOK bool
	}{
		{
			name: "Valid syslog config with local syslog",
			config: config.Local{
				HealthCheckInterval: 1000,
				FailOverInterval:    5000,
				FailOverLimit:       10000,
				LogToSyslog:         true,
				SyslogNetwork:       "",
				SyslogAddress:       "",
				SyslogFacility:      "LOG_LOCAL0",
				SyslogTag:           "pulseha",
				Mode:                "active-passive",
			},
			expectOK: true,
		},
		{
			name: "Valid syslog config with remote syslog",
			config: config.Local{
				HealthCheckInterval: 1000,
				FailOverInterval:    5000,
				FailOverLimit:       10000,
				LogToSyslog:         true,
				SyslogNetwork:       "udp",
				SyslogAddress:       "192.168.1.100:514",
				SyslogFacility:      "LOG_LOCAL1",
				SyslogTag:           "pulseha-node1",
				Mode:                "active-passive",
			},
			expectOK: true,
		},
		{
			name: "Valid config with syslog disabled",
			config: config.Local{
				HealthCheckInterval: 1000,
				FailOverInterval:    5000,
				FailOverLimit:       10000,
				LogToSyslog:         false,
				SyslogNetwork:       "",
				SyslogAddress:       "",
				SyslogFacility:      "",
				SyslogTag:           "",
				Mode:                "active-passive",
			},
			expectOK: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				Pulse:   tc.config,
				Groups:  make(map[string][]string),
				Nodes:   make(map[string]*config.Node),
				Plugins: make(map[string]interface{}),
			}

			err := cfg.Validate()
			if tc.expectOK {
				assert.NoError(t, err, "Config validation should pass for: %s", tc.name)
			} else {
				assert.Error(t, err, "Config validation should fail for: %s", tc.name)
			}
		})
	}
}

func TestSyslogConfigSerialization(t *testing.T) {
	// Set test environment
	os.Setenv("PULSEHA_TEST", "true")
	defer os.Unsetenv("PULSEHA_TEST")

	// Create config with syslog settings
	cfg := &config.Config{
		Pulse: config.Local{
			HealthCheckInterval: 1000,
			FailOverInterval:    5000,
			FailOverLimit:       10000,
			LoggingLevel:        "info",
			AutoFailback:        true,
			LogToFile:           true,
			LogFileLocation:     "/tmp/test.log",
			LogToSyslog:         true,
			SyslogNetwork:       "udp",
			SyslogAddress:       "syslog.example.com:514",
			SyslogFacility:      "LOG_LOCAL2",
			SyslogTag:           "pulseha-test",
			Mode:                "active-passive",
		},
		Groups:  make(map[string][]string),
		Nodes:   make(map[string]*config.Node),
		Plugins: make(map[string]interface{}),
	}

	// Serialize to JSON
	data, err := json.MarshalIndent(cfg, "", "    ")
	assert.NoError(t, err, "Should serialize config to JSON")

	// Deserialize back
	var cfg2 config.Config
	err = json.Unmarshal(data, &cfg2)
	assert.NoError(t, err, "Should deserialize config from JSON")

	// Verify syslog fields are preserved
	assert.Equal(t, cfg.Pulse.LogToSyslog, cfg2.Pulse.LogToSyslog, "LogToSyslog should be preserved")
	assert.Equal(t, cfg.Pulse.SyslogNetwork, cfg2.Pulse.SyslogNetwork, "SyslogNetwork should be preserved")
	assert.Equal(t, cfg.Pulse.SyslogAddress, cfg2.Pulse.SyslogAddress, "SyslogAddress should be preserved")
	assert.Equal(t, cfg.Pulse.SyslogFacility, cfg2.Pulse.SyslogFacility, "SyslogFacility should be preserved")
	assert.Equal(t, cfg.Pulse.SyslogTag, cfg2.Pulse.SyslogTag, "SyslogTag should be preserved")
}

// syslogLoadFixture writes a config file and loads it through the real path,
// returning what the daemon would hold in memory.
//
// PULSEHA_TEST is deliberately *off*. Under it config.Load returns before
// reading the disk, so a test that leaves it on measures the struct it built
// itself and nothing about loading — the trap #67 recorded, where inverting a
// guard killed zero tests because the harness was lying to the config package.
// Nodes is left empty so clusterCheckLocked is false and validate does not
// require a local node id.
func syslogLoadFixture(t *testing.T, pulse string) *config.Config {
	t.Helper()

	prev := config.CONFIG_LOCATION
	config.CONFIG_LOCATION = filepath.Join(t.TempDir(), "config.json")
	t.Cleanup(func() { config.CONFIG_LOCATION = prev })

	body := `{"pulseha":{` + pulse + `},"floating_ip_groups":{},"nodes":{},"plugins":{}}`
	if err := os.WriteFile(config.CONFIG_LOCATION, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.New()
	if err != nil {
		t.Fatalf("config.New: %v", err)
	}
	return cfg
}

// The backward-compatibility promise, tested where it is actually kept: a config
// written before syslog settings existed still logs to syslog.
//
// New() seeds LogToSyslog true and Load() unmarshals the file over it, so a key
// that is not in the file leaves the seed standing. That is the whole mechanism,
// and it is the only point in the program where "absent" is distinguishable from
// "false".
func TestOldConfigKeepsSyslogOn(t *testing.T) {
	os.Unsetenv("PULSEHA_TEST")

	cfg := syslogLoadFixture(t, `"logging_level":"info","mode":"active-passive"`)

	if !cfg.Pulse.SyslogEnabled() {
		t.Error("a config predating the syslog settings stopped logging to syslog; " +
			"that is the upgrade this defaulting exists to survive")
	}
	if cfg.Pulse.SyslogTag != "pulseha" || cfg.Pulse.SyslogFacility != "LOG_INFO" {
		t.Errorf("tag=%q facility=%q, want the defaults filled in",
			cfg.Pulse.SyslogTag, cfg.Pulse.SyslogFacility)
	}
}

// Regression for docs/TEST-PLAN.md defect #110: an explicit false was overridden
// and the node logged to syslog anyway.
//
// The shape matters. Every syslog *string* is left empty, which is exactly what
// migrateConfig read as "this is an old config" before deciding to turn syslog
// back on — so an operator who set log_to_syslog false and nothing else got the
// one config that was guaranteed to ignore them.
func TestExplicitlyDisabledSyslogStaysDisabled(t *testing.T) {
	os.Unsetenv("PULSEHA_TEST")

	cfg := syslogLoadFixture(t, `"logging_level":"info","mode":"active-passive","log_to_syslog":false`)

	if cfg.Pulse.SyslogEnabled() {
		t.Error("log_to_syslog:false was overridden; turning syslog off in the " +
			"config has to turn syslog off")
	}
}

// The same instruction, with a tag set, so the fix is not merely moving which
// spelling of "off" gets honoured.
func TestExplicitlyDisabledSyslogStaysDisabledWithATagSet(t *testing.T) {
	os.Unsetenv("PULSEHA_TEST")

	cfg := syslogLoadFixture(t,
		`"logging_level":"info","mode":"active-passive","log_to_syslog":false,"syslog_tag":"pulseha"`)

	if cfg.Pulse.SyslogEnabled() {
		t.Error("log_to_syslog:false was overridden even with a tag set")
	}
}
