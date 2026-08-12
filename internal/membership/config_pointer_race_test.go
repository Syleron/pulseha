package membership

import (
	"io"
	"sync"
	"testing"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/packages/config"
)

// raceTestConfig builds a config distinguishable from its siblings, so a torn read
// would be visible as well as racy.
func raceTestConfig(nodeID, mode string, groupIPs []string) *config.Config {
	return &config.Config{
		Pulse: config.Local{
			Mode:          mode,
			LocalNode:     nodeID,
			FailOverLimit: 10000,
			ClusterToken:  "token",
		},
		Groups: map[string][]string{"group1": groupIPs},
		Nodes: map[string]*config.Node{
			nodeID: {IPGroups: map[string][]string{"eth0": {"group1"}}},
		},
	}
}

// UpdateConfig swaps m.config under MemberList.Lock(), but the read side went
// through the bare pointer.
//
// Defect #32 made each snapshot's *contents* stable, which is the important half
// and is what stopped the observable corruption. The pointer read itself stayed a
// race: deriveExpectedIPs, enforceExpectations, releaseUnassignedIPs,
// failoverGrace, makePassiveTimeout and redistributeOrphanedIPs — and, it turns
// out, another twenty-odd sites across health_check.go and ip_monitor.go — all
// dereferenced m.config with no lock held.
//
// -race missed it for a structural reason rather than a lucky one: nothing in the
// suite drove UpdateConfig concurrently with a read pass. This test is that missing
// driver, so it is the instrument as much as the assertion. It must be run under
// -race to mean anything; without the detector it only proves the calls do not
// panic.
func TestConfigPointerIsNotReadWhileUpdateConfigSwapsIt(t *testing.T) {
	const nodeID = "node-a"

	logger := log.New(io.Discard)

	// The member holds every address either config can name, so redistributeOrphanedIPs
	// finds nothing orphaned and returns after its config read. The point here is to
	// exercise the read, not to drive a redistribution down the netlink path.
	member := newAATestMember(nodeID, "host-a", StatusActive,
		[]string{"10.0.0.1/24", "10.0.0.2/24", "10.0.0.3/24"})
	ml := newAATestMemberList(raceTestConfig(nodeID, "active-active",
		[]string{"10.0.0.1/24", "10.0.0.2/24"}), member)

	monitor := NewIPMonitor(ml, logger)
	checker := NewHealthChecker(ml, logger)

	const rounds = 300
	var wg sync.WaitGroup

	// The writer: a ConfigSync landing, over and over.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			mode := "active-active"
			if i%2 == 1 {
				mode = "active-passive"
			}
			ml.UpdateConfig(raceTestConfig(nodeID, mode,
				[]string{"10.0.0.1/24", "10.0.0.2/24", "10.0.0.3/24"}))
		}
	}()

	// The readers: the passes that run off the config pointer on every tick. Each is
	// a distinct dereference site the reviewer named.
	readers := []func(){
		func() { monitor.deriveExpectedIPs(nodeID, member) },
		func() { checker.failoverGrace() },
		func() { checker.makePassiveTimeout() },
		func() { checker.redistributeOrphanedIPs(ml.MembersSnapshot()) },
	}
	for _, read := range readers {
		wg.Add(1)
		go func(read func()) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				read()
			}
		}(read)
	}

	wg.Wait()
}

// The accessor itself has to hand back one stable pointer per call, and must not
// deadlock when a caller already inside the member list's own locking calls it.
func TestMemberListConfigReturnsTheCurrentPointer(t *testing.T) {
	const nodeID = "node-a"

	first := raceTestConfig(nodeID, "active-active", []string{"10.0.0.1/24"})
	member := newAATestMember(nodeID, "host-a", StatusActive, nil)
	ml := newAATestMemberList(first, member)

	if got := ml.Config(); got != first {
		t.Fatalf("Config() = %p, want the pointer the list was built with (%p)", got, first)
	}

	second := raceTestConfig(nodeID, "active-passive", []string{"10.0.0.9/24"})
	ml.UpdateConfig(second)

	if got := ml.Config(); got != second {
		t.Errorf("Config() = %p after UpdateConfig, want the new pointer (%p)", got, second)
	}
	if got := ml.Config().Pulse.Mode; got != "active-passive" {
		t.Errorf("Config().Pulse.Mode = %q, want active-passive", got)
	}
}

// A nil config must come back as nil rather than panicking inside the accessor,
// because several callers already branch on that (health_check.go's tick guard
// among them) and moving the read behind a lock must not change what they see.
func TestMemberListConfigToleratesNil(t *testing.T) {
	ml := &MemberList{Members: map[string]*Member{}, logger: log.New(io.Discard)}
	if got := ml.Config(); got != nil {
		t.Errorf("Config() = %p on a list with no config, want nil", got)
	}
}
