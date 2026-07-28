package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// writeTestConfig points CONFIG_LOCATION at a temp file holding a valid
// two-node cluster config and returns the loaded *Config.
func writeTestConfig(t *testing.T) *Config {
	t.Helper()

	c := &Config{
		Pulse: Local{
			HealthCheckInterval: 1000,
			FailOverInterval:    5000,
			FailOverLimit:       10000,
			LocalNode:           "node-a",
			LoggingLevel:        "error",
			Mode:                "active-active",
			SyslogFacility:      "LOG_INFO",
			SyslogTag:           "pulseha",
		},
		Groups: map[string][]string{
			"group1": {"10.0.0.1/24", "10.0.0.2/24"},
		},
		Nodes: map[string]*Node{
			"node-a": {Hostname: "node-a", IP: "127.0.0.1", Port: "8443",
				IPGroups: map[string][]string{"eth0": {"group1"}}},
			"node-b": {Hostname: "node-b", IP: "127.0.0.2", Port: "8443",
				IPGroups: map[string][]string{"eth0": {"group1"}}},
		},
		Plugins: map[string]interface{}{},
	}

	b, err := json.MarshalIndent(c, "", "    ")
	if err != nil {
		t.Fatalf("marshal test config: %v", err)
	}

	prev := CONFIG_LOCATION
	CONFIG_LOCATION = filepath.Join(t.TempDir(), "config.json")
	t.Cleanup(func() { CONFIG_LOCATION = prev })

	if err := os.WriteFile(CONFIG_LOCATION, b, 0600); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	loaded, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return loaded
}

// TestReloadDoesNotRaceLiveReaders is defect #32.
//
// Reload() used to json.Unmarshal a freshly read file straight over the live
// *Config that every other goroutine is reading — ClusterCheck(),
// GetLocalNodeUUID(), Nodes[...], Groups[...]. None of those readers take the
// Config's own mutex, so the unmarshal wrote the Nodes and Groups maps out from
// under them: a data race under -race and a "concurrent map read and map write"
// fatal error in production.
//
// Reload must therefore leave the receiver alone. Callers take the freshly
// built *Config it returns and swap their pointer, so a reader that already
// holds the old pointer keeps reading a consistent, immutable snapshot.
func TestReloadDoesNotRaceLiveReaders(t *testing.T) {
	live := writeTestConfig(t)

	const readers = 8
	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// The unsynchronized reads that exist all over
				// internal/server.
				_ = live.ClusterCheck()
				_, _ = live.GetLocalNodeUUID()
				_ = live.NodeCount()
				for _, n := range live.Nodes {
					_ = n.Hostname
					for iface := range n.IPGroups {
						_ = iface
					}
				}
				for g := range live.Groups {
					_ = live.Groups[g]
				}
			}
		}()
	}

	for i := 0; i < 200; i++ {
		if _, err := live.Reload(); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("Reload: %v", err)
		}
	}

	close(stop)
	wg.Wait()
}

// Reload returns a config equivalent to what is on disk, without disturbing the
// receiver — the receiver stays valid for goroutines still holding it.
func TestReloadReturnsFreshConfigAndLeavesReceiverIntact(t *testing.T) {
	live := writeTestConfig(t)

	// Mutate the on-disk file behind the live config's back.
	b, err := os.ReadFile(CONFIG_LOCATION)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var onDisk Config
	if err := json.Unmarshal(b, &onDisk); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	onDisk.Groups["group1"] = []string{"192.168.5.5/24"}
	onDisk.Pulse.Mode = "active-passive"
	b, err = json.MarshalIndent(&onDisk, "", "    ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(CONFIG_LOCATION, b, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	fresh, err := live.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if fresh == live {
		t.Fatal("Reload returned the receiver; it must return a new *Config")
	}
	if got, want := fresh.Pulse.Mode, "active-passive"; got != want {
		t.Errorf("fresh mode = %q, want %q", got, want)
	}
	if got, want := fresh.Groups["group1"][0], "192.168.5.5/24"; got != want {
		t.Errorf("fresh group1[0] = %q, want %q", got, want)
	}

	// The receiver is untouched — this is the whole point.
	if got, want := live.Pulse.Mode, "active-active"; got != want {
		t.Errorf("receiver mode = %q, want %q (Reload mutated the receiver)", got, want)
	}
	if got, want := len(live.Groups["group1"]), 2; got != want {
		t.Errorf("receiver group1 len = %d, want %d (Reload mutated the receiver)", got, want)
	}
}
