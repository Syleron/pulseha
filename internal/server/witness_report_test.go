package server

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/rpc"
)

// newWitnessTrio is a, b and the Witness c, each a real Server with its own
// identity, every certificate published to every config, and every node serving
// a real Witness listener -- the trusted Witness path, end to end. bAddr, when not
// empty, replaces where the Witness reaches b.
func newWitnessTrio(t *testing.T, bAddr string) (a, b, w *Server) {
	t.Helper()
	a, b, w = newVotingServer(t, "a"), newVotingServer(t, "b"), newVotingServer(t, "c")
	all := map[string]*Server{"a": a, "b": b, "c": w}
	certs := map[string]string{}
	addrs := map[string]string{}
	for id, s := range all {
		dir := t.TempDir()
		s.SetCertDir(dir)
		certs[id] = mintIdentityInto(t, dir, id)
		addrs[id] = freeAddr(t)
	}
	for _, s := range all {
		for id, node := range s.config.Nodes {
			node.TLSCert = certs[id]
			node.WitnessAddress = addrs[id]
		}
		s.config.Nodes["c"].Witness = true
		s.memberList.UpdateConfig(s.config)
	}
	w.memberList.GetMemberByID("c").SetStatus(membership.StatusWitness)
	if bAddr != "" {
		w.config.Nodes["b"].WitnessAddress = bAddr
	}
	for _, s := range all {
		s := s
		s.reconcileWitnessListener()
		t.Cleanup(s.stopWitnessListener)
	}
	return a, b, w
}

// freeAddr is a loopback address nothing is listening on, for a listener to take.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
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
	for _, name := range []string{"gone", "answers as another member"} {
		t.Run(name, func(t *testing.T) {
			addr := gone
			if name != "gone" {
				// The Witness is pointed at a's own listener while it expects b.
				addr = ""
			}
			a, _, w := newWitnessTrio(t, addr)
			if name != "gone" {
				w.config.Nodes["b"].WitnessAddress = a.config.Nodes["a"].WitnessAddress
			}
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

func TestTheWitnessAnswerBeforePromoting(t *testing.T) {
	t.Run("a peer serving the Witness", func(t *testing.T) {
		a, b, _ := newWitnessTrio(t, "")
		b.memberList.GetMemberByID("b").SetStatus(membership.StatusActive)
		if got := a.witnessAnswerFor(context.Background(), "b"); got != witnessPeerServing {
			t.Errorf("answer = %s, want peer-serving", got)
		}
	})
	t.Run("a peer the Witness cannot see clears the fence", func(t *testing.T) {
		closed, _ := net.Listen("tcp", "127.0.0.1:0")
		gone := closed.Addr().String()
		closed.Close()
		a, _, _ := newWitnessTrio(t, gone)
		a.witnessFence.fence(time.Now(), nil)
		if got := a.witnessAnswerFor(context.Background(), "b"); got != witnessPeerNotVisible {
			t.Errorf("answer = %s, want peer-not-visible", got)
		}
		if a.witnessFence.isFenced() {
			t.Error("still Fenced although the peer it was ruled out for is gone")
		}
	})
	t.Run("no Witness is silence", func(t *testing.T) {
		if got := newVotingServer(t, "a").witnessAnswerFor(context.Background(), "b"); got != witnessSilent {
			t.Errorf("answer = %s, want silent", got)
		}
	})
	t.Run("a Fenced node waits out a brief silence, and only a brief one", func(t *testing.T) {
		a := newVotingServer(t, "a")
		a.config.Nodes["c"].Witness = true // configured, but nothing listens there
		a.memberList.UpdateConfig(a.config)
		a.witnessFence.fence(time.Now(), nil)
		if got := a.witnessAnswerFor(context.Background(), "b"); got != witnessSilentWhileFenced {
			t.Errorf("answer inside the hold-down = %s, want silent-while-fenced", got)
		}
		a.witnessFence.answered(time.Now().Add(-2 * a.witnessHoldDown()))
		if got := a.witnessAnswerFor(context.Background(), "b"); got != witnessSilent {
			t.Errorf("answer after the hold-down = %s, want silent", got)
		}
	})
}

// End to end through the promotion itself: the incumbent is unreachable from
// here -- its port refuses -- but serving the Witness. The cluster-link-only
// partition this exists for. The promotion must abort and leave this node Passive.
func TestAPromotionOverAPeerServingTheWitnessAborts(t *testing.T) {
	a, b, _ := newWitnessTrio(t, "")
	a.config.Pulse.Mode = "active-passive"
	// No addresses: the abort is what is under test, and a promotion that went
	// ahead must not touch this machine's interfaces.
	a.config.Groups = map[string][]string{}
	b.memberList.GetMemberByID("b").SetStatus(membership.StatusActive)
	a.memberList.GetMemberByID("b").SetStatus(membership.StatusUnknown) // a cannot see b

	a.performPromotionAsync("a", nil, false)
	if got := a.memberList.GetMemberByID("a").GetStatus(); got != membership.StatusPassive {
		t.Fatalf("a = %s after promoting over a peer serving the Witness; want Passive", membership.StatusToString(got))
	}
}

// Seeing the peer again ends the partition, and with it the hold-down.
func TestSeeingThePeerAgainClearsTheFence(t *testing.T) {
	a := newVotingServer(t, "a")
	a.config.Nodes["c"].Witness = true
	a.memberList.UpdateConfig(a.config)
	a.witnessFence.fence(time.Now(), nil)

	a.memberList.GetMemberByID("b").SetStatus(membership.StatusUnknown)
	a.witnessLoopTick()
	if !a.witnessFence.isFenced() {
		t.Fatal("cleared while the peer is still unseen")
	}
	// The Witness being down is not the peer being unseen.
	a.memberList.GetMemberByID("b").SetStatus(membership.StatusPassive)
	a.memberList.GetMemberByID("c").SetStatus(membership.StatusUnknown)
	a.witnessLoopTick()
	if a.witnessFence.isFenced() {
		t.Fatal("still Fenced with the peer visible again")
	}
}
