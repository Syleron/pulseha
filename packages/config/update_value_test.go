package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newUpdateValueTestConfig gives a valid config whose Save() writes under
// t.TempDir(), so a test can tell a rejected write apart from a failed one.
//
// PULSEHA_TEST is deliberately not set: it short-circuits both Validate and Load,
// which are the two halves of what these tests are about.
func newUpdateValueTestConfig(t *testing.T) *Config {
	t.Helper()

	prevLocation := CONFIG_LOCATION
	CONFIG_LOCATION = filepath.Join(t.TempDir(), "config.json")
	t.Cleanup(func() { CONFIG_LOCATION = prevLocation })

	return &Config{
		Pulse: Local{
			Mode:                "active-passive",
			LocalNode:           "node-a",
			LoggingLevel:        "info",
			HealthCheckInterval: 1000,
			FailOverInterval:    5000,
			FailOverLimit:       10000,
		},
		Groups: map[string][]string{},
		Nodes:  map[string]*Node{"node-a": {Hostname: "node-a"}},
	}
}

// A value the validator rejects used to be left in the live struct: the setter
// writes into it before Validate runs, and only Save was skipped. The operator saw
// an error, the daemon carried the rejected value, and every later Save() — for
// any operation at all — failed the same validation. With the broadcaster now
// pushing a successful `config set` to the cluster, a rejected value left in place
// is also one an unrelated mutation could carry to every peer.
func TestUpdateValueRollsBackARejectedValue(t *testing.T) {
	c := newUpdateValueTestConfig(t)

	// Below the 1000ms floor Validate enforces.
	if err := c.UpdateValue("hcs_interval", "500"); err == nil {
		t.Fatal("UpdateValue accepted hcs_interval=500, which is below the validated minimum")
	}

	if got := c.Pulse.HealthCheckInterval; got != 1000 {
		t.Errorf("hcs_interval = %d after the rejected write, want it rolled back to 1000", got)
	}
	if err := c.Save(); err != nil {
		t.Errorf("Save() after a rejected UpdateValue: %v; a rejected value left in the "+
			"live config fails every subsequent save, including saves belonging to "+
			"unrelated operations", err)
	}
}

// The rollback must not swallow the reason. "invalid configuration value" alone
// gave the operator nothing to correct.
func TestUpdateValueErrorNamesTheConstraint(t *testing.T) {
	c := newUpdateValueTestConfig(t)

	err := c.UpdateValue("hcs_interval", "500")
	if err == nil {
		t.Fatal("UpdateValue accepted hcs_interval=500")
	}
	if !strings.Contains(err.Error(), "1000") {
		t.Errorf("error = %q, want it to name the constraint that rejected the value", err)
	}
}

// Regression for docs/TEST-PLAN.md defect #55, found while testing the above.
//
// Load() holds the config mutex for its whole body and calls migrateConfig, which
// called the exported Save() — which takes the same non-reentrant mutex. Any config
// on disk with all four syslog fields empty is one the migration fires for, so
// loading it hung the caller forever: daemon startup, and every Reload() behind a
// ConfigSync or Reconfigure.
func TestLoadMigratesAnOldConfigWithoutDeadlocking(t *testing.T) {
	c := newUpdateValueTestConfig(t)

	// An old config: no syslog settings at all, which is what migrateConfig keys on.
	c.Pulse.LogToSyslog = false
	c.Pulse.SyslogNetwork = ""
	c.Pulse.SyslogAddress = ""
	c.Pulse.SyslogFacility = ""
	c.Pulse.SyslogTag = ""
	if err := c.Save(); err != nil {
		t.Fatalf("seed Save(): %v", err)
	}

	loaded := &Config{}
	done := make(chan error, 1)
	go func() { done <- loaded.Load() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Load(): %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Load() did not return: migrating a config from inside Load()'s locked " +
			"region has to persist through the locked save, not the exported Save() " +
			"that takes the same mutex again (defect #55)")
	}

	if got := loaded.Pulse.SyslogTag; got != "pulseha" {
		t.Errorf("syslog_tag = %q after the migration, want pulseha", got)
	}
}

// An accepted value is still written and persisted.
func TestUpdateValueAppliesAnAcceptedValue(t *testing.T) {
	c := newUpdateValueTestConfig(t)

	if err := c.UpdateValue("hcs_interval", "2000"); err != nil {
		t.Fatalf("UpdateValue(hcs_interval, 2000): %v", err)
	}
	if got := c.Pulse.HealthCheckInterval; got != 2000 {
		t.Errorf("hcs_interval = %d, want 2000", got)
	}

	reloaded := &Config{}
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if got := reloaded.Pulse.HealthCheckInterval; got != 2000 {
		t.Errorf("persisted hcs_interval = %d, want 2000", got)
	}
}
