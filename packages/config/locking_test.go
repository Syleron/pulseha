package config

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

// Config's contents are shared mutable state that the daemon reads from several
// goroutines at once — the health-check tick reaches GetLocalNodeUUID on every
// pass, the join handler writes c.Nodes, the CLI writes c.Pulse. END-2325
// synchronised ClusterCheck and GetLocalNodeUUID (#87) and left two callers
// behind; these cover them.
//
// The concurrency cases matter more than the single-threaded ones: what was
// wrong with UpdateValue and GetLocalNode was not that they computed the wrong
// answer alone, but that they read and wrote without the lock while something
// else held it.

func newLockingTestConfig(t *testing.T) *Config {
	t.Helper()
	t.Setenv("PULSEHA_TEST", "true")
	return &Config{
		Pulse: Local{
			Mode:                "active-passive",
			LocalNode:           "node-a",
			HealthCheckInterval: 1000,
			FailOverInterval:    5000,
			FailOverLimit:       10000,
		},
		Groups: map[string][]string{},
		Nodes: map[string]*Node{
			"node-a": {Hostname: "host-a", IP: "192.0.2.1", Port: "8443",
				IPGroups: map[string][]string{"eth0": {"group1"}}},
		},
	}
}

// TestUpdateValueIsOneTransaction is why UpdateValue holds the lock across all
// of mutate, validate, save and roll back rather than taking it per step.
//
// Concurrent updates plus concurrent readers, so -race has both sides to pair
// up. The assertion is that whatever the final value is, it is one of the values
// actually offered — never a partially applied one, and never the pre-update
// value left behind by a rollback that restored the wrong thing.
func TestUpdateValueIsOneTransaction(t *testing.T) {
	c := newLockingTestConfig(t)

	offered := []string{"1000", "2000", "3000", "4000"}
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_ = c.UpdateValue("hcs_interval", offered[(w+i)%len(offered)])
			}
		}(w)
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				_ = hcInterval(c)
				_, _ = c.GetLocalNodeUUID()
				_ = c.ClusterCheck()
			}
		}()
	}
	wg.Wait()

	final := hcInterval(c)
	for _, want := range offered {
		if itoa(final) == want {
			return
		}
	}
	t.Errorf("final hcs_interval = %d, which is none of the values offered %v", final, offered)
}

// TestUpdateValueRollsBackUnderConcurrency pins the rollback path specifically.
// A rejected value must leave the config exactly as it was, and doing that
// correctly is why the mutation and its undo have to be inside one acquisition:
// otherwise the undo restores a snapshot another writer has since replaced.
func TestUpdateValueRollsBackUnderConcurrency(t *testing.T) {
	c := newLockingTestConfig(t)

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				// An invalid mode is rejected by validation, so every one of
				// these must roll back.
				_ = c.UpdateValue("mode", "not-a-mode")
			}
		}()
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				if got := mode(c); got != "active-passive" {
					t.Errorf("observed mode %q — a rejected value was left in place", got)
					return
				}
			}
		}()
	}
	wg.Wait()

	if got := mode(c); got != "active-passive" {
		t.Errorf("final mode = %q, want active-passive", got)
	}
}

// TestGetLocalNodeIsReadUnderOneLock covers the second residual. It used to take
// and release the lock twice (ClusterCheck, then GetLocalNodeUUID) and then read
// c.Nodes with no lock at all, so a join writing c.Nodes could land between the
// id and the lookup that used it.
func TestGetLocalNodeIsReadUnderOneLock(t *testing.T) {
	c := newLockingTestConfig(t)

	var wg sync.WaitGroup
	// Writers doing what a join does: adding and removing entries in c.Nodes.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			c.Lock()
			c.Nodes["node-b"] = &Node{Hostname: "host-b", IP: "192.0.2.2", Port: "8443"}
			c.Unlock()
			c.Lock()
			delete(c.Nodes, "node-b")
			c.Unlock()
		}
	}()
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				node, err := c.GetLocalNode()
				if err != nil {
					t.Errorf("GetLocalNode: %v", err)
					return
				}
				// The local node is never the one being churned, so it must
				// always come back fully described.
				if node.Hostname != "host-a" || node.IP != "192.0.2.1" {
					t.Errorf("got %+v, want host-a/192.0.2.1", node)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestGetLocalNodeReturnsACopy is the other half of reading under the lock:
// handing back the *Node would give the caller a pointer into the config it is
// not holding the lock for.
func TestGetLocalNodeReturnsACopy(t *testing.T) {
	c := newLockingTestConfig(t)

	node, err := c.GetLocalNode()
	if err != nil {
		t.Fatalf("GetLocalNode: %v", err)
	}
	node.Hostname = "mutated"
	node.IPGroups["eth0"][0] = "mutated"

	again, err := c.GetLocalNode()
	if err != nil {
		t.Fatalf("GetLocalNode: %v", err)
	}
	if again.Hostname != "host-a" {
		t.Errorf("mutating the returned node changed the config: %s", again.Hostname)
	}
	if again.IPGroups["eth0"][0] != "group1" {
		t.Errorf("mutating the returned IPGroups changed the config: %v", again.IPGroups)
	}
}

// TestValidateCanBeCalledFromOutsideTheLock pins the split.
//
// Validate reads c.Pulse and calls clusterCheckLocked, so it has always assumed
// the lock was held while being named as though it took one. It is now a locking
// wrapper over validateLocked. Load and saveLocked call the locked form from
// inside the lock; anything outside calls this one.
//
// Asserted rather than assumed because getting it wrong is a deadlock, not a
// wrong answer: an earlier draft of this change left saveLocked calling the
// locking Validate, and Save wedged immediately.
func TestValidateCanBeCalledFromOutsideTheLock(t *testing.T) {
	c := newLockingTestConfig(t)

	done := make(chan error, 1)
	go func() { done <- c.Validate() }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Validate on a valid config: %v", err)
		}
	case <-timeoutAfter():
		t.Fatal("Validate did not return — it took a lock it already held")
	}
}

// TestSaveDoesNotWedgeOnValidation is the regression that draft produced, kept
// because it is the exact shape the naming split exists to prevent: Save takes
// the lock, saveLocked validates, and validation must not retake it.
func TestSaveDoesNotWedgeOnValidation(t *testing.T) {
	c := newLockingTestConfig(t)

	done := make(chan error, 1)
	go func() { done <- c.Save() }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Save: %v", err)
		}
	case <-timeoutAfter():
		t.Fatal("Save did not return — saveLocked reached a locking Validate")
	}
}

// TestUpdateValueDoesNotWedge covers the third caller of the same trap.
func TestUpdateValueDoesNotWedge(t *testing.T) {
	c := newLockingTestConfig(t)

	done := make(chan error, 1)
	go func() { done <- c.UpdateValue("hcs_interval", "1500") }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("UpdateValue: %v", err)
		}
	case <-timeoutAfter():
		t.Fatal("UpdateValue did not return — it reached a method that retook its lock")
	}
	if got := hcInterval(c); got != 1500 {
		t.Errorf("hcs_interval = %d, want 1500", got)
	}
}

func timeoutAfter() <-chan time.Time { return time.After(5 * time.Second) }

func itoa(n int) string { return strconv.Itoa(n) }

// hcInterval and mode read c.Pulse under the config lock.
//
// Read directly rather than through an accessor because Config has none for
// these: c.Pulse.Mode and c.Pulse.HealthCheckInterval are read bare in about
// two hundred places across the daemon, which is the wider question of who owns
// the config's synchronisation and is deliberately a separate ticket. A test
// asserting the lock works should not itself read without it.
func hcInterval(c *Config) int {
	c.Lock()
	defer c.Unlock()
	return c.Pulse.HealthCheckInterval
}

func mode(c *Config) string {
	c.Lock()
	defer c.Unlock()
	return c.Pulse.Mode
}
