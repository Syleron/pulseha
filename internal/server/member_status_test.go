package server

import (
	"context"
	"io"
	"testing"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/rpc"
)

// Standby exists because Active answered two questions at once — "is this
// daemon healthy and eligible" and "is it serving floating IPs" — and gave the
// same answer to both. A node that has been promoted but assigned nothing was
// reported as Active, which told an operator nothing about whether traffic was
// reaching it.
//
// The load-bearing case here is selfReportsAssignments. An empty assignment list
// is only evidence of holding nothing where the list is knowledge; in
// active-passive it means "this node does not know", and calling that Standby
// would be worse than the vague Active it replaced.
func TestDeriveMemberStatus(t *testing.T) {
	tests := []struct {
		name                   string
		status                 membership.MemberStatus
		assignedIPs            int
		selfReportsAssignments bool
		want                   rpc.MemberStatusEnum
	}{
		{
			name:                   "active holding nothing in active-active is Standby",
			status:                 membership.StatusActive,
			assignedIPs:            0,
			selfReportsAssignments: true,
			want:                   rpc.MemberStatusEnum_MEMBER_STATUS_STANDBY,
		},
		{
			name:                   "active holding addresses stays Active",
			status:                 membership.StatusActive,
			assignedIPs:            61,
			selfReportsAssignments: true,
			want:                   rpc.MemberStatusEnum_MEMBER_STATUS_ACTIVE,
		},
		{
			// The #1/#21 shape: an election-promoted node in active-passive
			// holds every group address while its ActiveIPs is still empty, and
			// nobody self-reports in that mode. Reporting Standby here would
			// claim the node serving all the traffic is serving none.
			name:                   "active holding nothing in active-passive stays Active",
			status:                 membership.StatusActive,
			assignedIPs:            0,
			selfReportsAssignments: false,
			want:                   rpc.MemberStatusEnum_MEMBER_STATUS_ACTIVE,
		},
		{
			// Passive already means "holds nothing" in active-passive, so it is
			// left alone: renaming it would churn the wire value every existing
			// deployment reports without telling an operator anything new.
			name:                   "passive is unchanged even where the list is knowledge",
			status:                 membership.StatusPassive,
			assignedIPs:            0,
			selfReportsAssignments: true,
			want:                   rpc.MemberStatusEnum_MEMBER_STATUS_PASSIVE,
		},
		{
			// Maintenance is a deliberate operator decision to exclude the node
			// from promotion. Standby means "eligible", so it must not swallow
			// Maintenance just because the node holds nothing.
			name:                   "maintenance outranks an empty assignment list",
			status:                 membership.StatusMaintenance,
			assignedIPs:            0,
			selfReportsAssignments: true,
			want:                   rpc.MemberStatusEnum_MEMBER_STATUS_MAINTENANCE,
		},
		{
			name:                   "unknown is a health fact and is never derived away",
			status:                 membership.StatusUnknown,
			assignedIPs:            0,
			selfReportsAssignments: true,
			want:                   rpc.MemberStatusEnum_MEMBER_STATUS_UNKNOWN,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveMemberStatus(tt.status, tt.assignedIPs, tt.selfReportsAssignments)
			if got != tt.want {
				t.Errorf("deriveMemberStatus(%v, %d, %t) = %v, want %v",
					membership.StatusToString(tt.status), tt.assignedIPs, tt.selfReportsAssignments, got, tt.want)
			}
		})
	}
}

// Tenancy must stay derived. A stored Standby would reintroduce exactly the
// defect it is meant to describe, so this pins that the display value never
// leaks into the internal enum that placement and demotion read.
func TestStandbyIsNotAStoredMemberStatus(t *testing.T) {
	for _, s := range []membership.MemberStatus{
		membership.StatusUnknown,
		membership.StatusActive,
		membership.StatusPassive,
		membership.StatusMaintenance,
	} {
		if membership.StatusToString(s) == "Standby" {
			t.Fatalf("Standby must be derived at the status boundary, not stored as MemberStatus(%d)", s)
		}
	}
}

// newStatusTestServer builds the minimum Server that GetClusterStatus needs: a
// two-node config in the given mode, and a member list holding a healthy elected
// node plus its passive peer.
//
// Pulse.LocalNode is deliberately populated and both nodes are present in Nodes,
// so config.ClusterCheck passes and GetLocalNodeUUID would return the local ID.
// The gate this pins used to consult exactly that, and a test where the local
// node cannot be identified would pass whether the gate were reverted or not.
func newStatusTestServer(t *testing.T, mode string, activeIPs []string) *Server {
	t.Helper()

	cfg := &config.Config{
		Pulse: config.Local{Mode: mode, LocalNode: "node-a"},
		Nodes: map[string]*config.Node{
			"node-a": {Hostname: "node-a", IP: "10.0.0.1", Port: "9083"},
			"node-b": {Hostname: "node-b", IP: "10.0.0.2", Port: "9083"},
		},
		Groups: map[string][]string{},
	}

	logger := log.New(io.Discard)
	ml := membership.NewMemberList(cfg, logger)
	for _, id := range []string{"node-a", "node-b"} {
		if err := ml.AddMemberQuiet(id); err != nil {
			t.Fatalf("AddMemberQuiet(%s): %v", id, err)
		}
	}

	local := ml.GetMemberByID("node-a")
	local.Status = membership.StatusActive
	local.ActiveIPs = activeIPs

	ml.GetMemberByID("node-b").Status = membership.StatusPassive

	return &Server{config: cfg, logger: logger, memberList: ml}
}

func reportedStatus(t *testing.T, s *Server, nodeID string) rpc.MemberStatusEnum {
	t.Helper()

	resp, err := s.GetClusterStatus(context.Background(), &rpc.StatusRequest{})
	if err != nil {
		t.Fatalf("GetClusterStatus: %v", err)
	}
	for _, m := range resp.Members {
		if m.NodeId == nodeID {
			return m.Status
		}
	}
	t.Fatalf("no member %s in the status response", nodeID)
	return rpc.MemberStatusEnum_MEMBER_STATUS_UNKNOWN
}

// END-2289. deriveMemberStatus was correct in isolation and wrong at its only
// call site, which had no test of any kind: the gate granted assignment truth to
// selfReportsAssignments OR "this row is the local node", so in active-passive
// the only row that could ever read Standby was the row you were asking about.
// A healthy elected node holding nothing reported itself Standby while its peer
// reported it Active — the same cluster, the same instant, two answers. On the
// appliance that surfaced as one node showing "Standby / No Data" and cluster
// health 1·1 against the other's 2·0.
//
// Both arms are asserted here because that is the whole point. Reverting the
// gate expression leaves the active-active arm passing and only the
// active-passive arm fails, and a fix whose two arms are not separately pinned
// is one edit from being a no-op (docs/TEST-PLAN.md #67).
func TestGetClusterStatusReportsStandbyOnlyInActiveActive(t *testing.T) {
	tests := []struct {
		name string
		mode string
		want rpc.MemberStatusEnum
	}{
		{
			// The reported defect. Nothing is configured to serve, so the
			// elected node holds nothing and knows it holds nothing — and its
			// peer, which cannot know, says Active. Active is the answer both
			// have to give.
			name: "active-passive elected node holding nothing is Active",
			mode: "active-passive",
			want: rpc.MemberStatusEnum_MEMBER_STATUS_ACTIVE,
		},
		{
			// Standby keeps working where it means something: every member
			// self-reports its hosted addresses in this mode, so every node
			// computes the same answer for every row.
			name: "active-active node holding nothing is Standby",
			mode: "active-active",
			want: rpc.MemberStatusEnum_MEMBER_STATUS_STANDBY,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStatusTestServer(t, tt.mode, nil)
			if got := reportedStatus(t, s, "node-a"); got != tt.want {
				t.Errorf("node-a reported as %v, want %v", got, tt.want)
			}
		})
	}
}

// The other half of the mode-relative meaning of Active: in active-active it
// claims the node is serving something, so a node that holds addresses must not
// be flattened into Standby just because Standby is available in that mode.
func TestGetClusterStatusReportsActiveWhenHoldingAddresses(t *testing.T) {
	for _, mode := range []string{"active-passive", "active-active"} {
		t.Run(mode, func(t *testing.T) {
			s := newStatusTestServer(t, mode, []string{"10.0.0.100/24"})
			got := reportedStatus(t, s, "node-a")
			if got != rpc.MemberStatusEnum_MEMBER_STATUS_ACTIVE {
				t.Errorf("node-a reported as %v, want ACTIVE", got)
			}
		})
	}
}
