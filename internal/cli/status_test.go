package cli

import (
	"testing"

	"github.com/syleron/pulseha/internal/client"
	rpc "github.com/syleron/pulseha/rpc"
)

// calculateClusterHealth counts reachable nodes, not serving ones. Adding
// Standby to the status vocabulary without adding it here would have reported a
// perfectly healthy cluster as "degraded" — or "down", had every node been
// holding nothing — purely because a label changed.
func TestCalculateClusterHealthCountsStandbyAsReachable(t *testing.T) {
	tests := []struct {
		name     string
		statuses []string
		want     string
	}{
		{
			name:     "a standby node does not degrade an otherwise healthy cluster",
			statuses: []string{"Active", "Active", "Standby"},
			want:     "online",
		},
		{
			// The whole cluster up but holding nothing — an empty group, say —
			// is online, not down.
			statuses: []string{"Standby", "Standby"},
			name:     "all nodes standby is online, not down",
			want:     "online",
		},
		{
			name:     "an unreachable node still degrades the cluster",
			statuses: []string{"Active", "Standby", "Unknown"},
			want:     "degraded",
		},
		{
			name:     "no reachable node is down",
			statuses: []string{"Unknown", "Unknown"},
			want:     "down",
		},
		{
			name:     "existing active-passive vocabulary is unaffected",
			statuses: []string{"Active", "Passive"},
			want:     "online",
		},
		{
			name:     "no members at all is down",
			statuses: nil,
			want:     "down",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			members := make([]client.Member, 0, len(tt.statuses))
			for _, s := range tt.statuses {
				members = append(members, client.Member{Status: s})
			}
			if got := calculateClusterHealth(members); got != tt.want {
				t.Errorf("calculateClusterHealth(%v) = %q, want %q", tt.statuses, got, tt.want)
			}
		})
	}
}

// The daemon can report STANDBY, so the CLI must render it. An unhandled enum
// value falls through to "Unknown", which would make a healthy node look dead.
func TestTranslateStatusResponseRendersStandby(t *testing.T) {
	resp := &rpc.StatusResponse{
		Mode: "active-active",
		Members: []*rpc.Member{
			{Hostname: "node-1", Status: rpc.MemberStatusEnum_MEMBER_STATUS_ACTIVE},
			{Hostname: "node-2", Status: rpc.MemberStatusEnum_MEMBER_STATUS_STANDBY},
			{Hostname: "node-3", Status: rpc.MemberStatusEnum_MEMBER_STATUS_MAINTENANCE},
		},
	}

	status, err := translateStatusResponse(resp)
	if err != nil {
		t.Fatalf("translateStatusResponse returned error: %v", err)
	}

	want := map[string]string{"node-1": "Active", "node-2": "Standby", "node-3": "Maintenance"}
	for _, m := range status.Members {
		if got := want[m.Hostname]; got != m.Status {
			t.Errorf("%s: status = %q, want %q", m.Hostname, m.Status, got)
		}
	}
}
