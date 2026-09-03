package server

import (
	"context"
	"testing"
	"time"

	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/rpc"
)

// GetClusterStatus reported LastResponse as time.Now() for any member that had
// ever responded, discarding the stored value entirely:
//
//	lastResp := ""
//	if !health.LastResponse.IsZero() {
//	    lastResp = time.Now().Format(time.RFC3339)
//	}
//
// So a node unreachable for days showed a last response of a moment ago. It is
// the one field an operator, the CLI's "Last Response" line, or lb_api's status
// page would use to tell a blip from a corpse, and it carried no information
// beyond "has ever responded".
//
// The health checker's unconditional stamp had the same effect one layer down,
// but fixing that alone would not have helped: this layer overwrote it anyway.
func TestGetClusterStatusReportsTheStoredLastResponse(t *testing.T) {
	s, ml := newConfigSyncTestServer(t, "node-local", "node-peer")

	silent := time.Now().Add(-49 * time.Hour).Truncate(time.Second)
	peer := ml.GetMemberByID("node-peer")
	if peer == nil {
		t.Fatal("node-peer missing from the member list")
	}
	peer.Lock()
	peer.Status = membership.StatusUnknown
	peer.LastHCResponse = silent
	peer.Unlock()

	resp, err := s.GetClusterStatus(context.Background(), &rpc.StatusRequest{})
	if err != nil {
		t.Fatalf("GetClusterStatus: %v", err)
	}

	var found bool
	for _, m := range resp.Members {
		if m.NodeId != "node-peer" {
			continue
		}
		found = true
		if m.LastResponse == "" {
			t.Fatalf("LastResponse is empty for a member that has responded before")
		}
		got, err := time.Parse(time.RFC3339, m.LastResponse)
		if err != nil {
			t.Fatalf("parsing LastResponse %q: %v", m.LastResponse, err)
		}
		if drift := got.Sub(silent); drift < -time.Second || drift > time.Second {
			t.Errorf("LastResponse = %s, want %s (stored). Reported %v away from "+
				"the stored value, so the field cannot distinguish a node silent "+
				"for two days from one that answered a moment ago",
				got.Format(time.RFC3339), silent.Format(time.RFC3339), drift.Round(time.Second))
		}
	}
	if !found {
		t.Fatal("node-peer not present in the status response")
	}
}

// TestGetClusterStatusLeavesLastResponseEmptyForANodeNeverHeardFrom keeps the
// other arm: a zero stored time must stay empty rather than being reported as
// the epoch, which the CLI renders as "Last Response: 0001-01-01...".
func TestGetClusterStatusLeavesLastResponseEmptyForANodeNeverHeardFrom(t *testing.T) {
	s, ml := newConfigSyncTestServer(t, "node-local", "node-peer")

	peer := ml.GetMemberByID("node-peer")
	peer.Lock()
	peer.Status = membership.StatusUnknown
	peer.LastHCResponse = time.Time{}
	peer.Unlock()

	resp, err := s.GetClusterStatus(context.Background(), &rpc.StatusRequest{})
	if err != nil {
		t.Fatalf("GetClusterStatus: %v", err)
	}
	for _, m := range resp.Members {
		if m.NodeId == "node-peer" && m.LastResponse != "" {
			t.Errorf("LastResponse = %q for a node never heard from, want empty",
				m.LastResponse)
		}
	}
}
