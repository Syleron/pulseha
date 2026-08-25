package server

import (
	"io"
	"slices"
	"testing"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
)

// newAnnouncePlanServer builds a two-node cluster where the *remote* node is the
// one to be announced on, which is the case that matters: consolidation picks its
// target by node ID, so roughly half the time the survivor is not the node running
// the pass.
func newAnnouncePlanServer(mode string, remoteActiveIPs []string) *Server {
	const localID, remoteID = "node-a", "node-b"

	group := []string{"10.0.0.1/24", "10.0.0.2/24", "10.0.0.3/24"}
	cfg := &config.Config{
		Pulse:  config.Local{Mode: mode, LocalNode: localID},
		Groups: map[string][]string{"group1": group},
		Nodes: map[string]*config.Node{
			localID:  {IPGroups: map[string][]string{"eth0": {"group1"}}},
			remoteID: {IPGroups: map[string][]string{"eth0": {"group1"}}},
		},
	}

	logger := log.New(io.Discard)
	ml := membership.NewMemberList(cfg, logger)
	for _, id := range []string{localID, remoteID} {
		if err := ml.AddMemberQuiet(id); err != nil {
			panic(err)
		}
	}
	// The remote node is Active and holds the whole group in reality. What the
	// local daemon *records* about its holdings is the variable under test.
	m := ml.GetMemberByID(remoteID)
	m.Status = membership.StatusActive
	m.ActiveIPs = remoteActiveIPs

	return &Server{config: cfg, logger: logger, memberList: ml}
}

// The announcement after consolidation has to name actual addresses, and the only
// list a node keeps of a *peer's* holdings is empty in active-passive: peers
// self-report what they host over ConfigSync in active-active and nowhere else
// (ADR-0001). A plan built from the member record would therefore be empty for
// exactly the target consolidation most often picks — a remote one — and would fix
// nothing while every test that only counted announce *calls* still passed.
func TestAnnouncePlanComesFromConfigNotTheMemberRecord(t *testing.T) {
	group := []string{"10.0.0.1/24", "10.0.0.2/24", "10.0.0.3/24"}

	t.Run("active-passive plans the whole group for a remote node reporting nothing", func(t *testing.T) {
		s := newAnnouncePlanServer("active-passive", nil)

		plan, err := s.announcePlan("node-b")
		if err != nil {
			t.Fatalf("announcePlan: %v", err)
		}
		if !slices.Equal(plan["eth0"], group) {
			t.Errorf("expected the whole configured group, got %v", plan["eth0"])
		}
	})

	t.Run("active-active plans only what the node is assigned", func(t *testing.T) {
		assigned := []string{"10.0.0.2/24"}
		s := newAnnouncePlanServer("active-active", assigned)

		plan, err := s.announcePlan("node-b")
		if err != nil {
			t.Fatalf("announcePlan: %v", err)
		}
		// Announcing the whole group here would claim addresses the node does not
		// hold, telling the segment to send another Active's traffic to it.
		if !slices.Equal(plan["eth0"], assigned) {
			t.Errorf("expected only the assigned addresses, got %v", plan["eth0"])
		}
	})

	t.Run("skips interfaces with nothing to announce", func(t *testing.T) {
		s := newAnnouncePlanServer("active-passive", nil)
		s.config.Nodes["node-b"].IPGroups["eth1"] = nil

		plan, err := s.announcePlan("node-b")
		if err != nil {
			t.Fatalf("announcePlan: %v", err)
		}
		if _, ok := plan["eth1"]; ok {
			t.Errorf("expected an empty interface to be skipped, got %v", plan)
		}
		if len(plan) != 1 {
			t.Errorf("expected only eth0 in the plan, got %v", plan)
		}
	})

	t.Run("an unknown node is an error, not an empty plan", func(t *testing.T) {
		s := newAnnouncePlanServer("active-passive", nil)

		// An empty plan announces nothing and reports success, which would report a
		// completed consolidation that never touched the segment.
		if _, err := s.announcePlan("node-missing"); err == nil {
			t.Error("expected an error for a node absent from config")
		}
	})
}
