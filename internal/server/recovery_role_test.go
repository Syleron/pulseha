package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/rpc"
)

func TestUnknownGossipPreservesLocalRoleAndHealthReport(t *testing.T) {
	for _, role := range []membership.MemberStatus{membership.StatusActive, membership.StatusPassive, membership.StatusMaintenance} {
		t.Run(membership.StatusToString(role), func(t *testing.T) {
			s, ml := newConfigSyncTestServer(t, "owner", "coordinator", "observer")
			s.config.Pulse.Mode = "active-passive"
			s.config.Pulse.AutoFailback = true
			local := ml.GetMemberByID("owner")
			local.SetStatus(role)
			ml.GetMemberByID("coordinator").SetStatus(membership.StatusPassive)
			ml.GetMemberByID("observer").SetStatus(membership.StatusPassive)
			// The coordinator cannot reach this node, but the observer can. Repeated
			// higher-epoch gossip must not erase the role the observer will query.
			for epoch := int64(1); epoch <= 3; epoch++ {
				payload, err := json.Marshal(map[string]any{"member_states": map[string]int{"owner": int(membership.StatusUnknown)}, "epoch": epoch, "leader_id": "coordinator", "sender_id": "coordinator"})
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: payload}); err != nil {
					t.Fatal(err)
				}
				if local.GetStatus() != role {
					t.Fatalf("Unknown changed local %v to %v", role, local.GetStatus())
				}
				resp, err := s.HealthCheck(context.Background(), &rpc.HealthCheckRequest{NodeId: "observer"})
				if err != nil || !resp.Success || resp.NodeId != "owner" || resp.Status != rpc.MemberStatusEnum(role) {
					t.Fatalf("incorrect self report: %v %v", resp, err)
				}
			}
		})
	}
}
