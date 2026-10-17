package server

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/rpc"
)

// newWitnessTrio is a, b and the Witness c, each a real Server, with the
// Witness's view of b and a's view of the Witness pointed at served addresses.
// bAddr, when not empty, replaces where the Witness reaches b.
func newWitnessTrio(t *testing.T, bAddr string) (a, b, w *Server) {
	t.Helper()
	a, b, w = newVotingServer(t, "a"), newVotingServer(t, "b"), newVotingServer(t, "c")
	for _, s := range []*Server{a, b, w} {
		s.config.Nodes["c"].Witness = true
		s.memberList.UpdateConfig(s.config)
	}
	w.memberList.GetMemberByID("c").SetStatus(membership.StatusWitness)
	if bAddr == "" {
		bAddr = serveVoter(t, b)
	}
	setVoterAddress(t, w, "b", bAddr)
	setVoterAddress(t, a, "c", serveVoter(t, w))
	return a, b, w
}

func peerView(t *testing.T, resp *rpc.WitnessReportResponse, id string) *rpc.PeerView {
	t.Helper()
	for _, v := range resp.GetPeers() {
		if v.GetNodeId() == id {
			return v
		}
	}
	t.Fatalf("report has no view of %s: %+v", id, resp.GetPeers())
	return nil
}

// A member that is not the Witness answers with its own Claim and nothing else.
// That answer is what the Witness's round-trip to a peer receives.
func TestEveryMemberReportsItsOwnClaim(t *testing.T) {
	s := newVotingServer(t, "b")
	s.memberList.GetMemberByID("b").SetClaim(membership.Claim{Status: membership.StatusActive, ActiveIPs: []string{"10.0.0.1/32"}})
	s.Lock()
	s.clusterEpoch, s.leaderID = 7, "b"
	s.publishConvergenceLocked()
	s.Unlock()

	resp, err := s.WitnessReport(context.Background(), &rpc.WitnessReportRequest{NodeId: "a", ClusterToken: "vote-test-secret"})
	if err != nil || !resp.Success {
		t.Fatalf("WitnessReport = %+v, %v", resp, err)
	}
	self := resp.GetSelf()
	if self.GetNodeId() != "b" || self.GetStatus() != rpc.MemberStatusEnum_MEMBER_STATUS_ACTIVE ||
		len(self.GetActiveIps()) != 1 || self.GetClusterEpoch() != 7 || self.GetElectedNode() != "b" {
		t.Errorf("self = %+v; want b's Active claim, epoch 7, elected b", self)
	}
	if resp.GetFromWitness() || len(resp.GetPeers()) != 0 {
		t.Errorf("a member that is not the Witness reported peers: %+v", resp)
	}
}

func TestAReportIsRefusedToOutsiders(t *testing.T) {
	s := newVotingServer(t, "b")
	for _, req := range []*rpc.WitnessReportRequest{
		{NodeId: "a", ClusterToken: "wrong"},
		{NodeId: "a"},
		{NodeId: "stranger", ClusterToken: "vote-test-secret"},
	} {
		if resp, _ := s.WitnessReport(context.Background(), req); resp.GetSuccess() {
			t.Errorf("report given to %+v", req)
		}
	}
}

// The Witness asks the peer there and then, and reports what came back.
func TestTheWitnessReportsAPeerThatAnswers(t *testing.T) {
	a, b, _ := newWitnessTrio(t, "")
	b.memberList.GetMemberByID("b").SetClaim(membership.Claim{Status: membership.StatusActive, ActiveIPs: []string{"10.0.0.1/32"}})

	resp, err := a.askWitness(context.Background())
	if err != nil {
		t.Fatalf("askWitness: %v", err)
	}
	if !resp.GetFromWitness() || resp.GetSelf().GetStatus() != rpc.MemberStatusEnum_MEMBER_STATUS_WITNESS {
		t.Errorf("answer did not come from the Witness as the Witness: %+v", resp.GetSelf())
	}
	v := peerView(t, resp, "b")
	if !v.GetVisible() || v.GetClaim().GetStatus() != rpc.MemberStatusEnum_MEMBER_STATUS_ACTIVE {
		t.Errorf("view of b = %+v; want visible and Active", v)
	}
	for _, v := range resp.GetPeers() {
		if v.GetNodeId() == "a" {
			t.Error("the report asked the asker about itself; it carries its own claim")
		}
	}
}

// The failure the parent ticket names: a wedged Active still completes TCP
// handshakes -- the kernel does that, not the daemon -- and answers nothing.
// It must come back as not visible, and inside the asker's own deadline, so the
// asker reads "peer not visible" rather than "Witness silent".
func TestAPeerThatAcceptsButNeverAnswersIsNotVisible(t *testing.T) {
	wedged, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { wedged.Close() })
	go func() {
		for {
			conn, err := wedged.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { conn.Close() })
		}
	}()

	a, _, _ := newWitnessTrio(t, wedged.Addr().String())
	start := time.Now()
	resp, err := a.askWitness(context.Background())
	if err != nil {
		t.Fatalf("askWitness: %v -- a wedged peer turned into a silent Witness", err)
	}
	if elapsed := time.Since(start); elapsed >= witnessAskTimeout {
		t.Errorf("report took %s, not inside the asker's %s", elapsed, witnessAskTimeout)
	}
	if v := peerView(t, resp, "b"); v.GetVisible() || v.GetClaim() != nil {
		t.Errorf("wedged peer reported %+v; a TCP connect is not an answer", v)
	}
}

func TestAPeerThatIsGoneOrAnswersAsSomeoneElseIsNotVisible(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gone := closed.Addr().String()
	closed.Close()
	impostor := serveVoter(t, newVotingServer(t, "a")) // answers as a, at b's address

	for name, addr := range map[string]string{"gone": gone, "answers as another member": impostor} {
		t.Run(name, func(t *testing.T) {
			a, _, _ := newWitnessTrio(t, addr)
			resp, err := a.askWitness(context.Background())
			if err != nil {
				t.Fatalf("askWitness: %v", err)
			}
			if v := peerView(t, resp, "b"); v.GetVisible() {
				t.Errorf("view of b = %+v; want not visible", v)
			}
		})
	}
}

// No Witness is not a failure to report: it is tier 3, today's behaviour, and
// the caller has to be able to tell it apart from a Witness that did not answer.
func TestAskingWithNoWitnessConfigured(t *testing.T) {
	s := newVotingServer(t, "a")
	if _, err := s.askWitness(context.Background()); err != errNoWitness {
		t.Errorf("askWitness with no Witness = %v, want errNoWitness", err)
	}
}

// A report answers while the node's own election path holds the server lock.
// Waiting behind it would turn a live node into a "not visible" one.
func TestAReportDoesNotWaitOnTheServerLock(t *testing.T) {
	s := newVotingServer(t, "b")
	s.Lock()
	defer s.Unlock()
	done := make(chan *rpc.WitnessReportResponse, 1)
	go func() {
		resp, _ := s.WitnessReport(context.Background(), &rpc.WitnessReportRequest{NodeId: "a", ClusterToken: "vote-test-secret"})
		done <- resp
	}()
	select {
	case resp := <-done:
		if !resp.GetSuccess() {
			t.Errorf("report = %+v", resp)
		}
	case <-time.After(time.Second):
		t.Fatal("WitnessReport waited on the server lock")
	}
}

func TestThePeerBudgetSitsInsideTheAskersDeadline(t *testing.T) {
	now := time.Now()
	ctx, cancel := context.WithDeadline(context.Background(), now.Add(800*time.Millisecond))
	defer cancel()
	if got := witnessPeerBudget(ctx, now); got != 400*time.Millisecond {
		t.Errorf("budget with 800ms left = %s, want 400ms", got)
	}
	if got := witnessPeerBudget(context.Background(), now); got != witnessPeerTimeoutDefault {
		t.Errorf("budget with no deadline = %s, want %s", got, witnessPeerTimeoutDefault)
	}
}
