package server

import (
	"encoding/json"
	"io"
	"slices"
	"testing"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
)

// newSeedTestServer builds the minimum Server needed by
// seedActiveActiveAssignments: groups, per-node interface mappings, and a member
// list whose statuses mirror an active-passive cluster about to switch mode.
func newSeedTestServer(t *testing.T, group []string, statuses map[string]membership.MemberStatus) (*Server, *membership.MemberList) {
	t.Helper()

	nodes := map[string]*config.Node{}
	for id := range statuses {
		nodes[id] = &config.Node{IPGroups: map[string][]string{"eth0": {"group1"}}}
	}

	cfg := &config.Config{
		Pulse:  config.Local{Mode: "active-active", LocalNode: "node-a"},
		Groups: map[string][]string{"group1": group},
		Nodes:  nodes,
	}

	logger := log.New(io.Discard)
	ml := membership.NewMemberList(cfg, logger)
	for id, status := range statuses {
		if err := ml.AddMemberQuiet(id); err != nil {
			t.Fatalf("AddMemberQuiet(%s): %v", id, err)
		}
		m := ml.GetMemberByID(id)
		m.Status = status
	}

	return &Server{config: cfg, logger: logger, memberList: ml}, ml
}

// Regression for docs/TEST-PLAN.md defects #2/#26. Switching to active-active
// used to clear every member's ActiveIPs and redistribute the whole group as if
// nothing held it. The former sole Active still physically held all of it, so
// three peers were told to bring the same addresses up: on whitecrane the switch
// produced ~150 duplicated addresses immediately, and the coordinator then saw
// all 201 as orphaned and placed them again.
func TestSeedActiveActiveAssignmentsRecordsTheCurrentOwner(t *testing.T) {
	group := []string{"10.0.0.1/24", "10.0.0.2/24", "10.0.0.3/24", "10.0.0.4/24"}
	s, ml := newSeedTestServer(t, group, map[string]membership.MemberStatus{
		"node-a": membership.StatusActive,
		"node-b": membership.StatusPassive,
		"node-c": membership.StatusPassive,
	})

	if !s.seedActiveActiveAssignments() {
		t.Fatal("expected the Active node to be found")
	}

	if got := ml.GetMemberByID("node-a").ActiveIPs; !slices.Equal(got, group) {
		t.Errorf("owner ActiveIPs = %v, want the whole group %v", got, group)
	}
	for _, id := range []string{"node-b", "node-c"} {
		if got := ml.GetMemberByID(id).ActiveIPs; len(got) != 0 {
			t.Errorf("%s ActiveIPs = %v, want none", id, got)
		}
	}
}

// The election case the old comment called out: Active but with no recorded
// assignments. Reading ActiveIPs would report an empty cluster; the group
// mapping is what says the node holds the addresses.
func TestSeedActiveActiveAssignmentsIgnoresEmptyActiveIPs(t *testing.T) {
	group := []string{"10.0.0.1/24", "10.0.0.2/24"}
	s, ml := newSeedTestServer(t, group, map[string]membership.MemberStatus{
		"node-a": membership.StatusActive,
		"node-b": membership.StatusPassive,
	})
	ml.GetMemberByID("node-a").ActiveIPs = nil

	if !s.seedActiveActiveAssignments() {
		t.Fatal("expected the Active node to be found")
	}
	if got := ml.GetMemberByID("node-a").ActiveIPs; !slices.Equal(got, group) {
		t.Errorf("owner ActiveIPs = %v, want the whole group %v", got, group)
	}
}

// Regression for docs/TEST-PLAN.md defect #27. The return to active-passive used
// to send the mode change and the member states it implies as two separate
// broadcasts, so a peer could be in active-passive while still believing itself
// Active — and go on acting as active-active coordinator, consolidating the group
// onto a target of its own choosing while the node handling the request
// consolidated onto another. ConfigSync only reads the two together when they
// arrive in the same payload: the config by its "pulseha" root, the states by
// member_states/epoch/leader_id on that same object.
func TestConfigAndStatePayloadCarriesModeAndStatesTogether(t *testing.T) {
	cfg := &config.Config{
		Pulse:  config.Local{Mode: "active-passive", LocalNode: "node-a"},
		Groups: map[string][]string{"group1": {"10.0.0.1/24"}},
		Nodes:  map[string]*config.Node{"node-a": {}, "node-b": {}},
	}
	states := map[string]membership.MemberStatus{
		"node-a": membership.StatusActive,
		"node-b": membership.StatusPassive,
	}

	payload, err := buildConfigAndStatePayload(cfg, states, 42, "node-a")
	if err != nil {
		t.Fatalf("buildConfigAndStatePayload: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatalf("payload is not a JSON object: %v", err)
	}

	// Without this key ConfigSync treats the message as envelope-only and never
	// applies the mode change.
	if _, ok := raw["pulseha"]; !ok {
		t.Error("payload has no \"pulseha\" root, so ConfigSync would ignore the mode change")
	}

	// And without these the peer keeps whatever status it had, which after a
	// switch out of active-active is Active.
	for _, key := range []string{"member_states", "epoch", "leader_id"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("payload is missing %q, so the peer would not learn its new status", key)
		}
	}

	var decoded struct {
		Pulse        config.Local   `json:"pulseha"`
		MemberStates map[string]int `json:"member_states"`
		Epoch        int64          `json:"epoch"`
		LeaderID     string         `json:"leader_id"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal combined payload: %v", err)
	}
	if decoded.Pulse.Mode != "active-passive" {
		t.Errorf("mode = %q, want active-passive", decoded.Pulse.Mode)
	}
	if got := decoded.MemberStates["node-b"]; got != int(membership.StatusPassive) {
		t.Errorf("node-b state = %d, want Passive (%d)", got, membership.StatusPassive)
	}
	if decoded.Epoch != 42 || decoded.LeaderID != "node-a" {
		t.Errorf("epoch/leader = %d/%q, want 42/node-a", decoded.Epoch, decoded.LeaderID)
	}
}

// With nothing Active, nothing holds the group and the caller has to place it.
func TestSeedActiveActiveAssignmentsReportsNoOwner(t *testing.T) {
	s, _ := newSeedTestServer(t, []string{"10.0.0.1/24"}, map[string]membership.MemberStatus{
		"node-a": membership.StatusPassive,
		"node-b": membership.StatusUnknown,
	})

	if s.seedActiveActiveAssignments() {
		t.Error("expected no owner to be reported when no node is Active")
	}
}
