package server

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/rpc"
)

// A Witness can never become Active, and forcing it does not change that.
func TestPromoteRefusesTheWitness(t *testing.T) {
	for _, force := range []bool{false, true} {
		s := newVotingServer(t, "a")
		s.config.Nodes["c"].Witness = true
		s.memberList.UpdateConfig(s.config)

		resp, err := s.Promote(context.Background(), &rpc.PromoteRequest{NodeId: "c", ForceDemote: force})
		if err != nil || resp.Success || !strings.Contains(resp.Message, "Witness") {
			t.Fatalf("Promote(witness, force=%v) = %+v, %v; want a refusal naming the Witness", force, resp, err)
		}
		if got := s.memberList.GetMemberByID("c").GetStatus(); got != membership.StatusWitness {
			t.Fatalf("Witness status after refused promotion = %s", membership.StatusToString(got))
		}
	}
}

// Status carries the role, so a consumer can tell a Witness that is down from a
// node that has failed.
func TestStatusReportsTheWitnessRole(t *testing.T) {
	s := newVotingServer(t, "a")
	s.config.Nodes["c"].Witness = true
	s.memberList.UpdateConfig(s.config)
	s.memberList.GetMemberByID("c").SetStatus(membership.StatusUnknown)

	resp, err := s.GetClusterStatus(context.Background(), &rpc.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range resp.Members {
		if want := m.NodeId == "c"; m.Witness != want {
			t.Errorf("member %s witness = %v, want %v", m.NodeId, m.Witness, want)
		}
	}
}

func TestWitnessJoinRefusalRules(t *testing.T) {
	pair := func() map[string]*config.Node {
		return map[string]*config.Node{"a": {}, "b": {}}
	}
	withWitness := func() map[string]*config.Node {
		n := pair()
		n["w"] = &config.Node{Witness: true}
		return n
	}
	trio := func() map[string]*config.Node {
		n := pair()
		n["c"] = &config.Node{}
		return n
	}
	for _, tc := range []struct {
		name    string
		nodes   map[string]*config.Node
		id      string
		witness bool
		refused string
	}{
		{"a Witness joins a pair", pair(), "w", true, ""},
		{"a Witness joins a single node", map[string]*config.Node{"a": {}}, "w", true, ""},
		{"a second Witness", withWitness(), "w2", true, "already has a Witness"},
		{"a Witness rejoins under its own ID", withWitness(), "w", true, ""},
		{"a Witness for three nodes", trio(), "w", true, "two-node cluster"},
		{"a third node beside a Witness", withWitness(), "c", false, "remove the Witness"},
		{"a second node beside a Witness", map[string]*config.Node{"a": {}, "w": {Witness: true}}, "b", false, ""},
		{"an ordinary third node", pair(), "c", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := witnessJoinRefusal(tc.nodes, tc.id, tc.witness)
			if tc.refused == "" && got != "" {
				t.Errorf("refused: %q", got)
			}
			if tc.refused != "" && !strings.Contains(got, tc.refused) {
				t.Errorf("refusal = %q, want it to say %q", got, tc.refused)
			}
		})
	}
}

// witnessVersionPeer answers health checks reporting a fixed Witness protocol
// version, standing in for a member on a given build.
type witnessVersionPeer struct {
	rpc.UnimplementedServerServer
	version int32
}

func (p *witnessVersionPeer) HealthCheck(context.Context, *rpc.HealthCheckRequest) (*rpc.HealthCheckResponse, error) {
	return &rpc.HealthCheckResponse{Success: true, WitnessProtocolVersion: p.version}, nil
}

func joinAsWitness(t *testing.T, s *Server) *rpc.JoinResponse {
	t.Helper()
	resp, err := s.HandleNodeJoin(context.Background(), &rpc.JoinRequest{
		Hostname: "witness-box",
		NodeId:   "w",
		BindIp:   "10.20.70.99",
		BindPort: "9083",
		Token:    "irrelevant",
		Witness:  true,
	})
	if err != nil {
		t.Fatalf("HandleNodeJoin: %v", err)
	}
	return resp
}

func TestAWitnessJoinIsRecordedAsAWitness(t *testing.T) {
	s := newRemoveIPTestServer(t, serveVoter(t, &witnessVersionPeer{version: witnessProtocolVersion}))
	if resp := joinAsWitness(t, s); !resp.Success {
		t.Fatalf("Witness join refused: %s", resp.Message)
	}
	s.config.Lock()
	node := *s.config.Nodes["w"]
	s.config.Unlock()
	if !node.Witness || node.Maintenance {
		t.Errorf("config entry = witness %v, maintenance %v; want a Witness that is not parked", node.Witness, node.Maintenance)
	}
	m := s.memberList.GetMemberByID("w")
	if !m.IsWitness() || m.GetStatus() != membership.StatusUnknown {
		t.Errorf("member: role %v, status %s; want the role, Unknown until it answers",
			m.IsWitness(), membership.StatusToString(m.GetStatus()))
	}
}

// An older binary counts every config entry, so to it a pair with a Witness is
// three. A Witness join must not reach a cluster with one in it, and must not
// reach one it cannot ask either.
func TestAWitnessJoinIsRefusedUnlessEveryPeerUnderstandsIt(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unreachable := closed.Addr().String()
	closed.Close()

	for _, tc := range []struct {
		name, peer, want string
	}{
		{"an older peer", serveVoter(t, &witnessVersionPeer{version: 0}), "predates the Witness"},
		{"a peer that cannot be asked", unreachable, "cannot confirm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRemoveIPTestServer(t, tc.peer)
			before := s.memberList.GetMemberCount()
			resp := joinAsWitness(t, s)
			if resp.Success || !strings.Contains(resp.Message, tc.want) {
				t.Fatalf("join = %+v, want refused saying %q", resp, tc.want)
			}
			if s.memberList.GetMemberCount() != before {
				t.Error("refused Witness join left a member behind")
			}
			s.config.Lock()
			_, recorded := s.config.Nodes["w"]
			s.config.Unlock()
			if recorded {
				t.Error("refused Witness join left a config entry behind")
			}
		})
	}
}

func TestHealthCheckReportsTheWitnessProtocolVersion(t *testing.T) {
	s := newVotingServer(t, "a")
	resp, err := s.HealthCheck(context.Background(), &rpc.HealthCheckRequest{NodeId: "b"})
	if err != nil || !resp.Success {
		t.Fatalf("HealthCheck = %+v, %v", resp, err)
	}
	if resp.WitnessProtocolVersion != witnessProtocolVersion {
		t.Errorf("witness_protocol_version = %d, want %d", resp.WitnessProtocolVersion, witnessProtocolVersion)
	}
}

// Neither a floating IP group nor Maintenance applies to a Witness.
func TestTheWitnessTakesNoGroupAndNoMaintenance(t *testing.T) {
	s := newVotingServer(t, "a")
	s.config.Nodes["c"].Witness = true
	s.memberList.UpdateConfig(s.config)

	assign, err := s.AssignGroupToNode(context.Background(),
		&rpc.AssignGroupRequest{GroupName: "group", NodeId: "c", Interface: "eth0"})
	if err != nil || assign.Success || !strings.Contains(assign.Message, "Witness") {
		t.Errorf("AssignGroupToNode(witness) = %+v, %v; want a refusal naming the Witness", assign, err)
	}
	if len(s.config.Nodes["c"].IPGroups["eth0"]) != 0 {
		t.Error("the Witness was given a group")
	}

	maint, err := s.SetMaintenance(context.Background(), &rpc.SetMaintenanceRequest{NodeId: "c", Enable: true})
	if err != nil || maint.Success || !strings.Contains(maint.Message, "Witness") {
		t.Errorf("SetMaintenance(witness) = %+v, %v; want a refusal naming the Witness", maint, err)
	}
}
