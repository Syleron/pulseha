package server

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"testing"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/rpc"
)

// Regression: an incoming ConfigSync that reports the local node as Unknown
// must not be treated as a demotion. On the whitecrane cluster a node holding
// 203 floating IPs was blocked long enough in the per-IP GARP for peers to mark
// it Unknown; it broadcast that back, the node released every IP including the
// live Management VIPs, and took ~13 minutes to bring them all back up.
func TestIsDemotion(t *testing.T) {
	cases := []struct {
		name string
		old  membership.MemberStatus
		new  membership.MemberStatus
		want bool
	}{
		{"Active to Passive is a demotion", membership.StatusActive, membership.StatusPassive, true},
		{"Active to Maintenance is a demotion", membership.StatusActive, membership.StatusMaintenance, true},
		{"Active to Unknown is not a demotion", membership.StatusActive, membership.StatusUnknown, false},
		{"Active to Active is not a demotion", membership.StatusActive, membership.StatusActive, false},
		{"Passive to Passive is not a demotion", membership.StatusPassive, membership.StatusPassive, false},
		{"Unknown to Passive is not a demotion", membership.StatusUnknown, membership.StatusPassive, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDemotion(tc.old, tc.new); got != tc.want {
				t.Errorf("isDemotion(%s, %s) = %v, want %v",
					membership.StatusToString(tc.old), membership.StatusToString(tc.new), got, tc.want)
			}
		})
	}
}

// newConfigSyncTestServer builds a Server that can take a real ConfigSync: a
// config on disk under t.TempDir(), a member list with every node Active as in
// active-active, and no health checker so the async Reconfigure is inert. The
// node IP is in TEST-NET-1 so the listener rebind fails immediately instead of
// leaving a socket open past the test.
func newConfigSyncTestServer(t *testing.T, localID string, peerIDs ...string) (*Server, *membership.MemberList) {
	t.Helper()

	t.Setenv("PULSEHA_TEST", "true")
	prevLocation := config.CONFIG_LOCATION
	config.CONFIG_LOCATION = filepath.Join(t.TempDir(), "config.json")
	t.Cleanup(func() { config.CONFIG_LOCATION = prevLocation })

	// Distinct endpoints per node: loadInitialMembers deduplicates members by
	// IP:Port, so sharing one address makes it delete a node at random.
	nodes := map[string]*config.Node{}
	for i, id := range append([]string{localID}, peerIDs...) {
		nodes[id] = &config.Node{
			Hostname: id,
			IP:       fmt.Sprintf("192.0.2.%d", i+1),
			Port:     "8443",
			IPGroups: map[string][]string{"eth0": {"group1"}},
		}
	}

	cfg := &config.Config{
		Pulse: config.Local{
			Mode:                "active-active",
			LocalNode:           localID,
			HealthCheckInterval: 1000,
			FailOverInterval:    5000,
			FailOverLimit:       10000,
		},
		Groups: map[string][]string{"group1": {"10.0.0.1/24", "10.0.0.2/24"}},
		Nodes:  nodes,
	}

	logger := log.New(io.Discard)
	ml := membership.NewMemberList(cfg, logger)
	for id := range nodes {
		if err := ml.AddMemberQuiet(id); err != nil {
			t.Fatalf("AddMemberQuiet(%s): %v", id, err)
		}
		ml.GetMemberByID(id).Status = membership.StatusActive
	}

	return &Server{config: cfg, logger: logger, memberList: ml}, ml
}

// Regression for docs/TEST-PLAN.md defect #28. SetMode propagates the new mode
// and the demotions it implies in one higher-epoch ConfigSync, and the local
// node's own status is only meant to be overridden by such a decisive sync — an
// equal-epoch heartbeat has no authority over what a node knows about itself.
//
// ConfigSync advanced s.clusterEpoch from the payload before computing whether
// the payload was decisive, so the comparison was always against the epoch it
// had just adopted and never true. Every peer's view of the local node's own
// status was therefore discarded, including a real demotion. On whitecrane that
// left node-4 holding mode=active-passive and self=Active at once, where an
// Active node expects the whole group: its expectations widened from its
// 51-address share to all 201 and its ENFORCE loop out-raced the incoming
// BringDownIP, so the entire group was up on two nodes for ~20s.
func TestDecisiveConfigSyncDemotesTheLocalNode(t *testing.T) {
	const localID, peerID = "node-local", "node-peer"
	s, ml := newConfigSyncTestServer(t, localID, peerID)
	s.clusterEpoch = 4

	// What SetMode's active-passive branch sends: the new mode, the statuses it
	// implies, and epoch+2.
	s.config.Pulse.Mode = "active-passive"
	payload, err := buildConfigAndStatePayload(s.config, map[string]membership.MemberStatus{
		localID: membership.StatusPassive,
		peerID:  membership.StatusActive,
	}, 6, peerID)
	if err != nil {
		t.Fatalf("buildConfigAndStatePayload: %v", err)
	}

	if _, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: payload}); err != nil {
		t.Fatalf("ConfigSync: %v", err)
	}

	if got := ml.GetMemberByID(localID).Status; got != membership.StatusPassive {
		t.Errorf("local node status after a decisive demotion = %s, want Passive",
			membership.StatusToString(got))
	}
}

// The other half of the rule the fix must not break: an equal-epoch ConfigSync
// is a peer's heartbeat, and a peer that still remembers this node as Passive
// must not demote it. In active-active the coordinator assigns IPs and the node
// goes Active in response; letting a stale peer undo that stripped the new IPs
// and had the coordinator assign them again (docs/TEST-PLAN.md defect #2).
func TestEqualEpochConfigSyncDoesNotDemoteTheLocalNode(t *testing.T) {
	const localID, peerID = "node-local", "node-peer"
	s, ml := newConfigSyncTestServer(t, localID, peerID)
	s.clusterEpoch = 6
	s.leaderID = peerID

	payload, err := buildConfigAndStatePayload(s.config, map[string]membership.MemberStatus{
		localID: membership.StatusPassive,
		peerID:  membership.StatusActive,
	}, 6, peerID)
	if err != nil {
		t.Fatalf("buildConfigAndStatePayload: %v", err)
	}

	if _, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: payload}); err != nil {
		t.Fatalf("ConfigSync: %v", err)
	}

	if got := ml.GetMemberByID(localID).Status; got != membership.StatusActive {
		t.Errorf("local node status after an equal-epoch peer view = %s, want Active",
			membership.StatusToString(got))
	}
}
