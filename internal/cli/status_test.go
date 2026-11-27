package cli

import (
	"strings"
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

// A Witness is never counted, so it can neither make a cluster healthy nor make
// it degraded. The case that matters is the down one: a Witness that stops
// answering reads Unknown, and counting it would report a cluster that behaves
// exactly like one without a Witness as degraded.
func TestCalculateClusterHealthNeverCountsTheWitness(t *testing.T) {
	tests := []struct {
		name    string
		members []client.Member
		want    string
	}{
		{
			name: "a witness that is up does not change a healthy cluster",
			members: []client.Member{
				{Status: "Active"}, {Status: "Passive"}, {Status: "Witness", Witness: true},
			},
			want: "online",
		},
		{
			name: "a witness that is down does not degrade the cluster",
			members: []client.Member{
				{Status: "Active"}, {Status: "Passive"}, {Status: "Unknown", Witness: true},
			},
			want: "online",
		},
		{
			name: "a witness that is up does not rescue a cluster with no reachable node",
			members: []client.Member{
				{Status: "Unknown"}, {Status: "Unknown"}, {Status: "Witness", Witness: true},
			},
			want: "down",
		},
		{
			name: "a serving node that is down still degrades the cluster",
			members: []client.Member{
				{Status: "Active"}, {Status: "Unknown"}, {Status: "Witness", Witness: true},
			},
			want: "degraded",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := calculateClusterHealth(tt.members); got != tt.want {
				t.Errorf("calculateClusterHealth() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The daemon reports WITNESS, and the role separately; the CLI must carry both.
// An unhandled enum value falls through to "Unknown", which would show the
// cluster's Witness as a node that has failed.
func TestTranslateStatusResponseRendersTheWitness(t *testing.T) {
	resp := &rpc.StatusResponse{Members: []*rpc.Member{
		{Hostname: "up", Status: rpc.MemberStatusEnum_MEMBER_STATUS_WITNESS, Witness: true},
		{Hostname: "down", Status: rpc.MemberStatusEnum_MEMBER_STATUS_UNKNOWN, Witness: true},
	}}
	got, err := translateStatusResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if got.Members[0].Status != "Witness" || !got.Members[0].Witness {
		t.Errorf("witness that is up = %+v, want Status Witness with the role set", got.Members[0])
	}
	if got.Members[1].Status != "Unknown" || !got.Members[1].Witness {
		t.Errorf("witness that is down = %+v, want Status Unknown with the role still set", got.Members[1])
	}
}

func TestWitnessLine(t *testing.T) {
	witness := func(status string, trusted bool) client.Member {
		return client.Member{Status: status, Witness: true, WitnessTrusted: trusted, WitnessAddress: "10.0.0.3:9084"}
	}
	for _, tc := range []struct {
		name   string
		member client.Member
		all    []client.Member
		want   string
	}{
		{"an arbitrating Witness", witness("Witness", true), nil, "arbitrating"},
		{"a Witness not yet trusted", witness("Witness", false), nil, "configured but not trusted"},
		{"a Witness that is down", witness("Unknown", true), nil, "down"},
		{"a node with no witness_address beside a Witness", client.Member{Status: "Active"},
			[]client.Member{witness("Witness", true)}, "Witness address: none"},
		{"a node with a witness_address", client.Member{Status: "Active", WitnessAddress: "10.0.0.1:9084"},
			[]client.Member{witness("Witness", true)}, ""},
		{"a cluster with no Witness says nothing", client.Member{Status: "Active"},
			[]client.Member{{Status: "Passive"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := witnessLine(tc.member, tc.all)
			if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
				t.Errorf("witnessLine = %q, want it to say %q", got, tc.want)
			}
		})
	}
}
