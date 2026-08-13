package membership

import (
	"errors"
	"io"
	"reflect"
	"testing"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/packages/config"
)

// newDuplicateTestSetup builds a two-node cluster where localNodeID really is
// local — IsLocal() needs Nodes populated, since GetLocalNodeUUID fails the
// cluster check on an empty map — plus a stubbed view of the local interfaces.
func newDuplicateTestSetup(t *testing.T, localNodeID string, held map[string]bool,
	presenceErr error) (*HealthChecker, *Member, *Member) {
	t.Helper()

	cfg := &config.Config{
		Pulse:  config.Local{Mode: "active-active", LocalNode: localNodeID},
		Groups: map[string][]string{"group1": {"10.0.0.1/24", "10.0.0.2/24"}},
		Nodes: map[string]*config.Node{
			"node-a": {Hostname: "host-a", IPGroups: map[string][]string{"eth0": {"group1"}}},
			"node-b": {Hostname: "host-b", IPGroups: map[string][]string{"eth0": {"group1"}}},
		},
	}

	a := newAATestMember("node-a", "host-a", StatusActive, []string{"10.0.0.2/24"})
	b := newAATestMember("node-b", "host-b", StatusActive, []string{"10.0.0.2/24"})
	ml := newAATestMemberList(cfg, a, b)

	h := NewHealthChecker(ml, log.New(io.Discard))
	h.ipPresence = func(ip string) (bool, string, error) {
		if presenceErr != nil {
			return false, "", presenceErr
		}
		if held[ip] {
			return true, "eth0", nil
		}
		return false, "", nil
	}
	return h, a, b
}

// The survivor of a duplicate used to be whichever node sorted first by ID. When
// that is not the node actually holding the address, the coordinator brings down a
// live address and leaves the record on a node that may not have it up — the
// address is then served by nobody until the next orphan sweep re-places it.
//
// The local node's interfaces are the only ones readable, so they decide.
func TestDuplicateSurvivorPrefersTheNodeHoldingTheAddress(t *testing.T) {
	// node-b is local and really has 10.0.0.2 up; node-a sorts first by ID.
	h, a, b := newDuplicateTestSetup(t, "node-b", map[string]bool{"10.0.0.2": true}, nil)

	if !h.resolveDuplicateAssignments(h.members.Members) {
		t.Fatal("expected the duplicate to be reported as resolved")
	}

	if got := b.GetActiveIPs(); !reflect.DeepEqual(got, []string{"10.0.0.2/24"}) {
		t.Errorf("expected the holder (node-b) to keep the address, got %v", got)
	}
	if got := a.GetActiveIPs(); len(got) != 0 {
		t.Errorf("expected the non-holder (node-a) to lose the address, got %v", got)
	}
}

// The mirror case: the local node sorts first but demonstrably does not have the
// address up, so its record is the stale one and the peer should keep it.
func TestDuplicateSurvivorDropsAStaleLocalRecord(t *testing.T) {
	// node-a is local and does NOT hold 10.0.0.2, yet sorts first by ID.
	h, a, b := newDuplicateTestSetup(t, "node-a", map[string]bool{}, nil)

	if !h.resolveDuplicateAssignments(h.members.Members) {
		t.Fatal("expected the duplicate to be reported as resolved")
	}

	if got := b.GetActiveIPs(); !reflect.DeepEqual(got, []string{"10.0.0.2/24"}) {
		t.Errorf("expected the peer to keep the address, got %v", got)
	}
	if got := a.GetActiveIPs(); len(got) != 0 {
		t.Errorf("expected the stale local record to be dropped, got %v", got)
	}
}

// When the interfaces cannot be read, presence is no better evidence than record
// order, so the previous deterministic behaviour has to be preserved rather than
// guessed at.
func TestDuplicateSurvivorFallsBackToRecordOrder(t *testing.T) {
	h, a, b := newDuplicateTestSetup(t, "node-a", nil, errors.New("netlink unavailable"))

	if !h.resolveDuplicateAssignments(h.members.Members) {
		t.Fatal("expected the duplicate to be reported as resolved")
	}

	if got := a.GetActiveIPs(); !reflect.DeepEqual(got, []string{"10.0.0.2/24"}) {
		t.Errorf("expected the lowest node ID to keep the address, got %v", got)
	}
	if got := b.GetActiveIPs(); len(got) != 0 {
		t.Errorf("expected node-b to lose the address, got %v", got)
	}
}

// Neither contender being local is the other fallback: a coordinator resolving a
// duplicate between two peers has no kernel state to consult.
func TestDuplicateSurvivorWithNoLocalContenderUsesRecordOrder(t *testing.T) {
	h, a, b := newDuplicateTestSetup(t, "node-c", map[string]bool{"10.0.0.2": true}, nil)

	if !h.resolveDuplicateAssignments(h.members.Members) {
		t.Fatal("expected the duplicate to be reported as resolved")
	}

	if got := a.GetActiveIPs(); !reflect.DeepEqual(got, []string{"10.0.0.2/24"}) {
		t.Errorf("expected the lowest node ID to keep the address, got %v", got)
	}
	if got := b.GetActiveIPs(); len(got) != 0 {
		t.Errorf("expected node-b to lose the address, got %v", got)
	}
}
