package membership

import (
	"testing"
	"time"

	"github.com/syleron/pulseha/packages/config"
)

// EnterMaintenance deadlocked against an Active node holding floating IPs, which is the case
// `pulsectl node maintenance` exists for.
//
// It held the member lock with a defer and called BringDownIPs inside, which takes the same
// lock to read IsLocal and Status. Member embeds a plain sync.RWMutex, so that is a self
// deadlock — and the goroutine then held the member lock forever, wedging anything else that
// touched that member. Reached from the SetMaintenance RPC, so this is a live path, not the
// dead one the rest of this class of bug was found in (docs/TEST-PLAN.md #85).
//
// Every case is run under a watchdog rather than left to the package timeout, so a regression
// reports as this test failing rather than as the whole package timing out with a stack dump.
func TestEnterMaintenanceDoesNotDeadlock(t *testing.T) {
	// A remote member: BringDownIPs takes the non-local branch and issues no netlink calls,
	// while still taking the lock that used to be held — which is the part under test. Built
	// through the member list so m.config is wired, since groupIPsByInterface dereferences it.
	newRemote := func(status MemberStatus, ips []string) *Member {
		cfg := &config.Config{
			Pulse:  config.Local{Mode: "active-passive", LocalNode: "node-local"},
			Groups: map[string][]string{"group1": {"10.0.0.1/24", "10.0.0.2/24"}},
			Nodes: map[string]*config.Node{
				"node-a": {Hostname: "host-a", IPGroups: map[string][]string{"eth0": {"group1"}}},
			},
		}
		m := newAATestMember("node-a", "host-a", status, ips)
		newAATestMemberList(cfg, m)
		return m
	}

	within := func(t *testing.T, d time.Duration, name string, fn func()) {
		t.Helper()
		done := make(chan struct{})
		go func() { defer close(done); fn() }()
		select {
		case <-done:
		case <-time.After(d):
			t.Fatalf("%s did not return within %v — the member lock is held across BringDownIPs", name, d)
		}
	}

	t.Run("an Active member holding addresses", func(t *testing.T) {
		m := newRemote(StatusActive, []string{"10.0.0.1/24", "10.0.0.2/24"})

		within(t, 5*time.Second, "EnterMaintenance", func() { _ = m.EnterMaintenance() })

		m.mu.Lock()
		status, ips := m.Status, m.ActiveIPs
		m.mu.Unlock()

		if status != StatusMaintenance {
			t.Errorf("status = %s, want Maintenance", StatusToString(status))
		}
		// The addresses were released, so the record must not keep claiming them — peers
		// count these as hosted when deciding what to redistribute.
		if len(ips) != 0 {
			t.Errorf("expected ActiveIPs cleared, got %v", ips)
		}
	})

	t.Run("an Active member holding nothing", func(t *testing.T) {
		// No bring-down to make, so this arm never reached the deadlock. Pinned anyway,
		// because the fix moves the lock boundaries for both.
		m := newRemote(StatusActive, nil)

		within(t, 5*time.Second, "EnterMaintenance", func() { _ = m.EnterMaintenance() })

		m.mu.Lock()
		status := m.Status
		m.mu.Unlock()
		if status != StatusMaintenance {
			t.Errorf("status = %s, want Maintenance", StatusToString(status))
		}
	})

	t.Run("a Passive member", func(t *testing.T) {
		m := newRemote(StatusPassive, nil)

		within(t, 5*time.Second, "EnterMaintenance", func() { _ = m.EnterMaintenance() })

		m.mu.Lock()
		status := m.Status
		m.mu.Unlock()
		if status != StatusMaintenance {
			t.Errorf("status = %s, want Maintenance", StatusToString(status))
		}
	})

	t.Run("a member already in maintenance is a no-op", func(t *testing.T) {
		// The early return also had to stop using the deferred unlock, so it is worth
		// asserting the lock is genuinely released on that path too.
		m := newRemote(StatusMaintenance, nil)

		within(t, 5*time.Second, "EnterMaintenance", func() { _ = m.EnterMaintenance() })

		// Acquiring the lock afterwards is the assertion: a leaked lock hangs here.
		within(t, 5*time.Second, "re-locking after the no-op", func() {
			m.mu.Lock()
			m.mu.Unlock()
		})
	})

	t.Run("exit returns the member to passive", func(t *testing.T) {
		m := newRemote(StatusMaintenance, nil)

		within(t, 5*time.Second, "ExitMaintenance", func() { _ = m.ExitMaintenance() })

		m.mu.Lock()
		status := m.Status
		m.mu.Unlock()
		if status != StatusPassive {
			t.Errorf("status = %s, want Passive", StatusToString(status))
		}
	})
}
