package membership

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// The member list lock is a plain sync.RWMutex, so it is not reentrant.
// RemoveMember holds it and then redistributed the leaving node's addresses
// through the exported RedistributeIPs, which takes the same lock again — every
// removal of a node that still held floating IPs hung the daemon, with the write
// lock held, so nothing else touching the member list could make progress either.
//
// Both branches of RemoveMember are covered: the by-ID lookup and the
// by-hostname fallback each had their own copy of the call.
func TestRemoveMemberRedistributingDoesNotSelfDeadlock(t *testing.T) {
	for _, tc := range []struct {
		name       string
		identifier string
	}{
		{name: "by node ID", identifier: "node-a"},
		{name: "by hostname", identifier: "host-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newAATestConfig(map[string][]string{
				"group1": {"10.0.0.1/24", "10.0.0.2/24"},
			})
			leaving := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24"})
			survivor := newAATestMember("node-b", "host-b", StatusActive, nil)
			ml := newAATestMemberList(cfg, leaving, survivor)

			done := make(chan error, 1)
			go func() { done <- ml.RemoveMember(tc.identifier) }()

			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("RemoveMember(%q) returned an error: %v", tc.identifier, err)
				}
			case <-time.After(5 * time.Second):
				// The goroutine is wedged holding the write lock; do not touch
				// ml again from here.
				t.Fatalf("RemoveMember(%q) deadlocked redistributing the leaving node's IPs", tc.identifier)
			}
		})
	}
}

// A removed member's addresses have to be read under the member lock too — the
// call site used to pass member.ActiveIPs straight to redistribution while the
// health check loop was appending to it.
func TestRemoveMemberRedistributesTheAddressesItHeld(t *testing.T) {
	cfg := newAATestConfig(map[string][]string{
		"group1": {"10.0.0.1/24", "10.0.0.2/24"},
	})
	leaving := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.1/24", "10.0.0.2/24"})
	survivor := newAATestMember("node-b", "host-b", StatusActive, nil)
	ml := newAATestMemberList(cfg, leaving, survivor)

	if err := ml.RemoveMember("node-a"); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	if _, still := ml.Members["node-a"]; still {
		t.Fatal("expected node-a to be gone from the member list")
	}
	if got := survivor.GetActiveIPs(); len(got) == 0 {
		t.Errorf("expected the leaving node's addresses to land on the survivor, got %v", got)
	}
}

// The redistribution path read len(node.ActiveIPs), node.Capacity and
// member.Status without the member lock, while AddActiveIPs and the health check
// loop write all three under it. Run the two against each other so -race has
// something to catch; without the fix this reports a data race on every run.
func TestRedistributeIPsSnapshotsMemberStateUnderLock(t *testing.T) {
	cfg := newAATestConfig(map[string][]string{
		"group1": {"10.0.0.1/24", "10.0.0.2/24", "10.0.0.3/24", "10.0.0.4/24"},
	})
	a := newAATestMember("node-a", "host-a", StatusActive, nil)
	b := newAATestMember("node-b", "host-b", StatusPassive, nil)
	ml := newAATestMemberList(cfg, a, b)

	stop := make(chan struct{})
	var writers sync.WaitGroup

	// Mutate exactly the fields the read path touches, exactly the way the rest
	// of the package does: under the member's own lock, never the list lock.
	for i, member := range []*Member{a, b} {
		writers.Add(1)
		go func(seed int, m *Member) {
			defer writers.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				m.Lock()
				// Capped: the reader's cost is linear in this list, so letting it
				// grow unbounded made the test quadratic without exercising
				// anything new — the contended field accesses are what matter.
				if len(m.ActiveIPs) >= 32 {
					m.ActiveIPs = m.ActiveIPs[:0]
				}
				m.ActiveIPs = append(m.ActiveIPs, fmt.Sprintf("10.9.%d.%d/24", seed, n%256))
				if m.Status == StatusActive {
					m.Status = StatusPassive
				} else {
					m.Status = StatusActive
				}
				m.Capacity = n % 5
				m.Unlock()
			}
		}(i, member)
	}

	for n := 0; n < 300; n++ {
		// Errors are irrelevant here — these members have no node config, so
		// the bring-up fails past the point being exercised.
		_ = ml.RedistributeIPs([]string{"10.0.0.1/24", "10.0.0.2/24"})
	}

	close(stop)
	writers.Wait()
}
