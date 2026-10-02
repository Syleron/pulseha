package server

import (
	"context"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/internal/clustertls"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/internal/quorum"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/rpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const orphanProposal = `["10.0.0.1/32"]`

func newVotingServer(t *testing.T, local string) *Server {
	t.Helper()
	cfg := &config.Config{Pulse: config.Local{LocalNode: local, ClusterToken: "vote-test-secret", Mode: "active-active", FailOverLimit: 1000},
		Groups: map[string][]string{"group": {"10.0.0.1/32", "10.0.0.2/32"}}, Nodes: map[string]*config.Node{}}
	for _, id := range []string{"a", "b", "c"} {
		cfg.Nodes[id] = &config.Node{Hostname: id, IP: "127.0.0.1", Port: "1"}
	}
	logger := log.New(io.Discard)
	ml := membership.NewMemberList(cfg, logger)
	for id, node := range cfg.Nodes {
		if err := ml.AddMember(id, id, node.IP, node.Port); err != nil {
			t.Fatal(err)
		}
		ml.GetMemberByID(id).SetStatus(membership.StatusPassive)
	}
	s := NewServer(cfg, logger, ml, membership.NewHealthChecker(ml, logger))
	s.voteStatePath = filepath.Join(t.TempDir(), "votes.json")
	return s
}

func serveVoter(t *testing.T, impl rpc.ServerServer) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer()
	rpc.RegisterServerServer(g, impl)
	go g.Serve(ln)
	t.Cleanup(g.Stop)
	return ln.Addr().String()
}

func setVoterAddress(t *testing.T, s *Server, id, addr string) {
	t.Helper()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	s.config.Nodes[id].IP = host
	s.config.Nodes[id].Port = port
}

func startVote(t *testing.T, s *Server, timeout time.Duration) string {
	t.Helper()
	id, err := s.quorumManager.StartVotingSession(quorum.VoteTypeIPRedistribution, orphanProposal, "redistribute orphan", timeout)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestBroadcastVoteRequiresRealPeerDecision(t *testing.T) {
	for _, tc := range []struct {
		name   string
		live   bool
		reject bool
	}{
		{"no peers", false, false}, {"peer grants", true, false}, {"peer refuses held IP", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newVotingServer(t, "a")
			if tc.live {
				remote := newVotingServer(t, "b")
				if tc.reject {
					remote.memberList.GetMemberByID("c").SetActiveIPs([]string{"10.0.0.1/32"})
				}
				setVoterAddress(t, s, "b", serveVoter(t, remote))
			}
			id := startVote(t, s, 200*time.Millisecond)
			err := s.BroadcastVoteRequest(id, "ip_redistribution", orphanProposal, "redistribute orphan", 1)
			if !tc.live && err == nil {
				t.Fatal("no-peer broadcast succeeded")
			}
			session, _ := s.quorumManager.GetVotingSession(id)
			if tc.live && !tc.reject {
				if err != nil || session.Result == nil || !session.Result.Passed || session.Result.YesCount != 2 {
					t.Fatalf("real peer majority not recorded: %v %+v", err, session)
				}
			} else {
				if session.Result != nil && session.Result.Passed {
					t.Fatal("passed without peer approval")
				}
				if len(session.Votes) != 0 && !tc.reject {
					t.Fatalf("manufactured peer votes: %v", session.Votes)
				}
				if tc.reject && session.Votes["b"].Decision != quorum.VoteDecisionNo {
					t.Fatalf("peer rejection ignored: %v", session.Votes)
				}
			}
		})
	}
}

type invalidVoter struct {
	rpc.UnimplementedServerServer
	mode string
}

func (v *invalidVoter) RequestVote(ctx context.Context, r *rpc.RequestVoteRequest) (*rpc.RequestVoteResponse, error) {
	switch v.mode {
	case "hang":
		<-ctx.Done()
		return nil, ctx.Err()
	case "legacy":
		return nil, status.Error(codes.Unimplemented, "old peer")
	case "empty":
		return &rpc.RequestVoteResponse{}, nil
	}
	resp := &rpc.RequestVoteResponse{SessionId: r.SessionId, VoterId: "b", Epoch: r.Epoch, Granted: true, ProtocolVersion: 2, Phase: r.Phase, Ballot: r.Ballot}
	switch v.mode {
	case "identity":
		resp.VoterId = "c"
	case "session":
		resp.SessionId = "other"
	case "epoch":
		resp.Epoch++
	case "v1":
		resp.ProtocolVersion = 0
	}
	return resp, nil
}

func TestInvalidPeerResponsesCannotSupplyVotes(t *testing.T) {
	for _, mode := range []string{"hang", "legacy", "empty", "identity", "session", "epoch", "v1"} {
		t.Run(mode, func(t *testing.T) {
			s := newVotingServer(t, "a")
			setVoterAddress(t, s, "b", serveVoter(t, &invalidVoter{mode: mode}))
			id := startVote(t, s, 100*time.Millisecond)
			started := time.Now()
			if err := s.BroadcastVoteRequest(id, "ip_redistribution", orphanProposal, "redistribute orphan", 1); err == nil {
				t.Fatal("invalid response accepted")
			}
			if time.Since(started) > time.Second {
				t.Fatal("request deadline not enforced")
			}
			session, _ := s.quorumManager.GetVotingSession(id)
			if len(session.Votes) != 0 {
				t.Fatalf("invalid peer counted: %v", session.Votes)
			}
		})
	}
}

func voteRequest(s *Server) *rpc.RequestVoteRequest {
	return &rpc.RequestVoteRequest{SessionId: "proposal", InitiatorId: "a", VoteType: "ip_redistribution", Subject: orphanProposal, MemberIds: []string{"a", "b", "c"}, Epoch: 1, ExpiresAtUnixMilli: time.Now().Add(time.Second).UnixMilli(), ClusterToken: s.config.Pulse.ClusterToken, Phase: "accept", Ballot: 1}
}

func preparedTestVote(s *Server, r *rpc.RequestVoteRequest) (*rpc.RequestVoteResponse, error) {
	p := proto.Clone(r).(*rpc.RequestVoteRequest)
	p.Phase = "prepare"
	resp, err := s.RequestVote(context.Background(), p)
	if err != nil || !resp.Granted {
		return resp, err
	}
	return s.RequestVote(context.Background(), r)
}

func TestRequestVoteValidatesProposalAndConflicts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*rpc.RequestVoteRequest)
	}{
		{"membership", func(r *rpc.RequestVoteRequest) { r.MemberIds = []string{"a", "b", "unknown"} }},
		{"duplicates", func(r *rpc.RequestVoteRequest) { r.MemberIds = []string{"a", "b", "b"} }},
		{"expired", func(r *rpc.RequestVoteRequest) { r.ExpiresAtUnixMilli = 1 }},
		{"epoch", func(r *rpc.RequestVoteRequest) { r.Epoch = 10 }},
		{"unknown address", func(r *rpc.RequestVoteRequest) { r.Subject = `["192.0.2.1/32"]` }},
		{"unstructured proposal", func(r *rpc.RequestVoteRequest) { r.Subject = "redistribute-1-ips" }},
		{"unsupported type", func(r *rpc.RequestVoteRequest) { r.VoteType = "config_change" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newVotingServer(t, "b")
			r := voteRequest(s)
			tc.change(r)
			resp, err := preparedTestVote(s, r)
			if err == nil && resp.Granted {
				t.Fatal("invalid proposal granted")
			}
		})
	}
	s := newVotingServer(t, "b")
	r := voteRequest(s)
	resp, err := preparedTestVote(s, r)
	if err != nil || !resp.Granted {
		t.Fatalf("valid proposal: %v %v", resp, err)
	}
	r.SessionId = "retry"
	resp, err = preparedTestVote(s, r)
	if err != nil || !resp.Granted {
		t.Fatal("identical retry refused")
	}
	r.Subject = `["10.0.0.2/32"]`
	resp, err = preparedTestVote(s, r)
	if err != nil || resp.Granted {
		t.Fatal("conflicting same-epoch grant")
	}
}

func TestVotingAuthentication(t *testing.T) {
	s := newVotingServer(t, "b")
	for _, id := range []string{"unknown", "a"} {
		r := voteRequest(s)
		r.InitiatorId = id
		r.ClusterToken = "wrong"
		if _, err := preparedTestVote(s, r); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("unauthenticated request: %v", err)
		}
	}
	id := startVote(t, s, time.Second)
	if _, err := s.CastVote(context.Background(), &rpc.CastVoteRequest{SessionId: id, VoterId: "a", Decision: rpc.VoteDecision_YES}); status.Code(err) != codes.PermissionDenied {
		t.Fatal("unsolicited ballot accepted")
	}
	session, _ := s.quorumManager.GetVotingSession(id)
	if len(session.Votes) != 0 {
		t.Fatal("ballot recorded")
	}
}

func TestVotingTLSBindsBothPeerIdentities(t *testing.T) {
	a, b := newVotingServer(t, "a"), newVotingServer(t, "b")
	a.certDir = t.TempDir()
	b.certDir = t.TempDir()
	certA := mintIdentityInto(t, a.certDir, "a")
	certB := mintIdentityInto(t, b.certDir, "b")
	for _, s := range []*Server{a, b} {
		s.config.Pulse.TLSMode = config.TLSModeRequired
		s.config.Nodes["a"].TLSCert = certA
		s.config.Nodes["b"].TLSCert = certB
		// A third trust entry must be distinct, though it does not participate.
		s.config.Nodes["c"].TLSCert = mintIdentityInto(t, t.TempDir(), "c")
	}
	addr := serveWithAuthorisation(t, b)
	setVoterAddress(t, a, "b", addr)
	c := dialAs(t, a.certDir, a.config, addr)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r := voteRequest(a)
	r.InitiatorId = "c"
	if _, err := c.Server().RequestVote(ctx, r); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("certificate impersonation accepted: %v", err)
	}
	id := startVote(t, a, time.Second)
	if err := a.BroadcastVoteRequest(id, "ip_redistribution", orphanProposal, "redistribute orphan", 1); err != nil {
		t.Fatal(err)
	}
	session, _ := a.quorumManager.GetVotingSession(id)
	if session.Result == nil || !session.Result.Passed {
		t.Fatal("authenticated majority failed")
	}
	// A trusted b certificate at c's endpoint must not be counted as voter c.
	a2 := newVotingServer(t, "a")
	a2.certDir = a.certDir
	a2.config.Pulse.TLSMode = config.TLSModeRequired
	for id, node := range a.config.Nodes {
		copy := *node
		a2.config.Nodes[id] = &copy
	}
	a2.config.Nodes["b"].Port = "1"
	creds, err := clustertls.ServerCredentials(b.certDir, b.clusterSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	forged := grpc.NewServer(grpc.Creds(credentials.NewTLS(creds)))
	rpc.RegisterServerServer(forged, &invalidVoter{mode: "identity"})
	go forged.Serve(ln)
	t.Cleanup(forged.Stop)
	setVoterAddress(t, a2, "c", ln.Addr().String())
	id = startVote(t, a2, time.Second)
	_ = a2.BroadcastVoteRequest(id, "ip_redistribution", orphanProposal, "redistribute orphan", 1)
	session, _ = a2.quorumManager.GetVotingSession(id)
	if len(session.Votes) != 0 {
		t.Fatalf("one peer impersonated another: %v", session.Votes)
	}
}

func TestConcurrentConflictingProposalsReceiveOnlyOneGrant(t *testing.T) {
	s := newVotingServer(t, "b")
	ready := make(chan struct{})
	results := make(chan bool, 2)
	for _, subject := range []string{orphanProposal, `["10.0.0.2/32"]`} {
		go func(subject string) {
			r := voteRequest(s)
			r.Subject = subject
			r.SessionId = subject
			<-ready
			resp, err := preparedTestVote(s, r)
			results <- err == nil && resp.Granted
		}(subject)
	}
	close(ready)
	grants := 0
	for i := 0; i < 2; i++ {
		if <-results {
			grants++
		}
	}
	if grants != 1 {
		t.Fatalf("conflicting grants = %d, want 1", grants)
	}
}

func TestNodeStatusVoteRejectsKnownActive(t *testing.T) {
	s := newVotingServer(t, "b")
	r := voteRequest(s)
	r.VoteType = "node_status"
	r.Subject = "a"
	s.memberList.GetMemberByID("c").SetStatus(membership.StatusActive)
	resp, err := preparedTestVote(s, r)
	if err != nil || resp.Granted {
		t.Fatalf("granted over known Active: %v %v", resp, err)
	}
	s.memberList.GetMemberByID("c").SetStatus(membership.StatusUnknown)
	resp, err = preparedTestVote(s, r)
	if err != nil || !resp.Granted {
		t.Fatalf("eligible candidate denied: %v %v", resp, err)
	}
}
