package config

import (
	"os"
	"strings"
	"testing"
)

// The config clock used to live only in the daemon's memory, so every restart
// began at zero and the first mutation after it was refused by every peer as
// stale (docs/TEST-PLAN.md #117). The stamp now travels to disk with the content
// it describes, and these are the properties the daemon relies on.

func TestSaveStampRoundTripsThroughLoad(t *testing.T) {
	c := newUpdateValueTestConfig(t)
	if err := c.Save(); err != nil {
		t.Fatalf("seed Save(): %v", err)
	}

	want := Stamp{Version: 42, Origin: "node-a"}
	if err := c.SaveStamp(want); err != nil {
		t.Fatalf("SaveStamp: %v", err)
	}
	if got := c.PersistedStamp(); got != want {
		t.Errorf("PersistedStamp() after SaveStamp = %+v, want %+v", got, want)
	}

	raw, err := os.ReadFile(CONFIG_LOCATION)
	if err != nil {
		t.Fatalf("reading the saved config: %v", err)
	}
	if !strings.Contains(string(raw), `"config_stamp"`) {
		t.Fatalf("the saved config carries no config_stamp key; a restart would start the "+
			"clock at zero again. File:\n%s", raw)
	}

	// A fresh Config loading that file resumes the clock.
	loaded := &Config{}
	if err := loaded.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := loaded.Stamp; got != want {
		t.Errorf("Load() gave stamp %+v, want %+v", got, want)
	}
}

// A config that has never been stamped must write exactly what it wrote before
// the field existed: a cluster upgrades one node at a time, and its configs are
// compared byte for byte by nothing, but hashed by everything.
func TestAnUnstampedConfigWritesNoStampKey(t *testing.T) {
	c := newUpdateValueTestConfig(t)
	if err := c.Save(); err != nil {
		t.Fatalf("Save(): %v", err)
	}
	raw, err := os.ReadFile(CONFIG_LOCATION)
	if err != nil {
		t.Fatalf("reading the saved config: %v", err)
	}
	if strings.Contains(string(raw), "config_stamp") {
		t.Errorf("an unstamped config wrote a config_stamp key:\n%s", raw)
	}
}

// Reload clones before it loads, so the clone has to carry the stamp or a
// reload of a file that happens to lack the key would silently reset the clock.
func TestReloadKeepsTheStamp(t *testing.T) {
	c := newUpdateValueTestConfig(t)
	want := Stamp{Version: 7, Origin: "node-a"}
	if err := c.SaveStamp(want); err != nil {
		t.Fatalf("SaveStamp: %v", err)
	}

	fresh, err := c.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := fresh.PersistedStamp(); got != want {
		t.Errorf("Reload() gave stamp %+v, want %+v", got, want)
	}
	if fresh == c {
		t.Error("Reload returned the receiver rather than a fresh config")
	}
}

func TestStampIsZero(t *testing.T) {
	cases := []struct {
		name string
		in   Stamp
		want bool
	}{
		{"empty", Stamp{}, true},
		{"version without origin", Stamp{Version: 3}, true},
		{"origin without version", Stamp{Origin: "node-a"}, true},
		{"negative version", Stamp{Version: -1, Origin: "node-a"}, true},
		{"stamped", Stamp{Version: 1, Origin: "node-a"}, false},
	}
	for _, tc := range cases {
		if got := tc.in.IsZero(); got != tc.want {
			t.Errorf("%s: IsZero(%+v) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}
}

// SaveStamp persists through the same validation as Save: a stamp cannot be
// written around a config the validator refuses, or the file would carry a
// version for content the daemon would not load.
func TestSaveStampStillValidates(t *testing.T) {
	c := newUpdateValueTestConfig(t)
	c.Pulse.HealthCheckInterval = 1 // below the validated floor

	if err := c.SaveStamp(Stamp{Version: 1, Origin: "node-a"}); err == nil {
		t.Fatal("SaveStamp wrote a config the validator refuses")
	}
	if _, err := os.Stat(CONFIG_LOCATION); err == nil {
		t.Error("a config that failed validation was written to disk")
	}
}
