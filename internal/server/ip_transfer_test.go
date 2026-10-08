package server

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/rpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestIPVerificationUsesFinalState(t *testing.T) {
	ips := []string{"10.0.0.1/32", "10.0.0.2/32", "10.0.0.3/32"}
	lookup := func(ip string) (bool, string, error) {
		switch ip {
		case "10.0.0.1":
			return true, "eth0", nil
		case "10.0.0.2":
			return true, "eth1", nil
		default:
			return false, "", nil
		}
	}
	up, failed := verifyIPBatch("eth0", ips, true, lookup)
	if !reflect.DeepEqual(up, ips[:1]) || len(failed) != 2 {
		t.Fatalf("wrong acquisition: %v %v", up, failed)
	}
	down, failed := verifyIPBatch("eth0", ips, false, lookup)
	if !reflect.DeepEqual(down, ips[2:]) || len(failed) != 2 {
		t.Fatalf("release elsewhere was accepted: %v %v", down, failed)
	}
	for _, lookup := range []func(string) (bool, string, error){nil, func(string) (bool, string, error) { return false, "", errors.New("inventory failed") }} {
		verified, failed := verifyIPBatch("eth0", ips, false, lookup)
		if len(verified) != 0 || len(failed) != 3 {
			t.Fatal("unreadable inventory confirmed release")
		}
	}
}

func TestIPReceiptRejectsLegacyAndIncompleteResults(t *testing.T) {
	ips := []string{"10.0.0.1/32", "10.0.0.2/32"}
	for _, tc := range []struct {
		name             string
		version          uint32
		verified, failed []string
	}{
		{"legacy", 0, ips, nil}, {"omission", 1, ips[:1], nil}, {"duplicate", 1, []string{ips[0], ips[0]}, nil},
		{"overlap", 1, ips, ips[:1]}, {"foreign", 1, []string{ips[0], "10.0.0.9/32"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := validatedIPResult(ips, true, tc.version, tc.verified, tc.failed, "")
			if err == nil || len(v) > 0 {
				t.Fatalf("trusted malformed result: %v %v", v, err)
			}
		})
	}
	v, err := validatedIPResult(ips, false, 1, ips[:1], ips[1:], "partial")
	if err == nil || !reflect.DeepEqual(v, ips[:1]) {
		t.Fatal("lost honest partial result")
	}
}

type transferPeer struct {
	rpc.UnimplementedServerServer
	mu     sync.Mutex
	events *[]string
	name   string
	mode   string
}

func (p *transferPeer) BringDownIP(_ context.Context, r *rpc.DownIpRequest) (*rpc.DownIpResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	*p.events = append(*p.events, p.name+":down")
	switch p.mode {
	case "unavailable":
		return nil, status.Error(codes.Unavailable, "lost source")
	case "legacy":
		return &rpc.DownIpResponse{Success: true}, nil
	case "refuse":
		return &rpc.DownIpResponse{Success: false, VerificationVersion: 1, FailedIps: r.Ips}, nil
	case "partial":
		return &rpc.DownIpResponse{Success: false, VerificationVersion: 1, VerifiedIps: r.Ips[:1], FailedIps: r.Ips[1:]}, nil
	}
	return &rpc.DownIpResponse{Success: true, VerificationVersion: 1, VerifiedIps: r.Ips}, nil
}
func (p *transferPeer) BringUpIP(_ context.Context, r *rpc.UpIpRequest) (*rpc.UpIpResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	*p.events = append(*p.events, p.name+":up")
	switch p.mode {
	case "refuse":
		return &rpc.UpIpResponse{Success: false, VerificationVersion: 1, FailedIps: r.Ips}, nil
	case "partial":
		return &rpc.UpIpResponse{Success: false, VerificationVersion: 1, VerifiedIps: r.Ips[:1], FailedIps: r.Ips[1:]}, nil
	case "legacy":
		return &rpc.UpIpResponse{Success: true}, nil
	}
	return &rpc.UpIpResponse{Success: true, VerificationVersion: 1, VerifiedIps: r.Ips}, nil
}
func newTransferTest(t *testing.T, sourceMode, destMode string) (*Server, *transferPeer, *transferPeer, *[]string) {
	t.Helper()
	s := newVotingServer(t, "a")
	events := []string{}
	source := &transferPeer{name: "source", mode: sourceMode, events: &events}
	dest := &transferPeer{name: "dest", mode: destMode, events: &events}
	setVoterAddress(t, s, "b", serveVoter(t, source))
	setVoterAddress(t, s, "c", serveVoter(t, dest))
	for _, id := range []string{"b", "c"} {
		s.config.Nodes[id].IPGroups = map[string][]string{"eth0": {"group"}}
	}
	s.memberList.GetMemberByID("b").SetClaim(membership.Claim{Status: membership.StatusActive, ActiveIPs: []string{"10.0.0.1/32", "10.0.0.2/32"}})
	return s, source, dest, &events
}
func TestTransferRequiresVerifiedReleaseBeforeAcquisition(t *testing.T) {
	for _, mode := range []string{"unavailable", "legacy", "refuse", "partial"} {
		t.Run(mode, func(t *testing.T) {
			s, _, _, events := newTransferTest(t, mode, "")
			if err := s.OrchestrateIPFailover("b", "c", s.config.Groups["group"]); err == nil {
				t.Fatal("unverified release accepted")
			}
			if !reflect.DeepEqual(*events, []string{"source:down"}) {
				t.Fatalf("acquired before confirmed release: %v", *events)
			}
			if len(s.memberList.GetMemberByID("c").Claim().ActiveIPs) > 0 {
				t.Fatal("credited unacquired destination")
			}
			remaining := s.memberList.GetMemberByID("b").Claim().ActiveIPs
			if mode == "partial" {
				if !reflect.DeepEqual(remaining, []string{"10.0.0.2/32"}) {
					t.Fatalf("lost partial source state: %v", remaining)
				}
			} else if len(remaining) != 2 {
				t.Fatal("forgot unconfirmed source")
			}
		})
	}
}
func TestPartialAcquisitionRetainsEvidenceAndRetryCompletes(t *testing.T) {
	s, _, dest, events := newTransferTest(t, "", "partial")
	ips := s.config.Groups["group"]
	if err := s.OrchestrateIPFailover("b", "c", ips); err == nil {
		t.Fatal("partial acquisition reported success")
	}
	if !reflect.DeepEqual(*events, []string{"source:down", "dest:up"}) {
		t.Fatalf("unexpected rollback: %v", *events)
	}
	if got := s.memberList.GetMemberByID("c").Claim().ActiveIPs; !reflect.DeepEqual(got, ips[:1]) {
		t.Fatalf("optimistic claim: %v", got)
	}
	dest.mu.Lock()
	dest.mode = ""
	dest.mu.Unlock()
	if err := s.OrchestrateIPFailover("b", "c", ips); err != nil {
		t.Fatal(err)
	}
	if got := s.memberList.GetMemberByID("c").Claim().ActiveIPs; !reflect.DeepEqual(got, ips) {
		t.Fatalf("retry duplicated/lost addresses: %v", got)
	}
}
func TestTransferPreflightHasNoSideEffects(t *testing.T) {
	for _, bad := range []string{"source", "destination", "ambiguous", "invalid"} {
		t.Run(bad, func(t *testing.T) {
			s, _, _, events := newTransferTest(t, "", "")
			ips := s.config.Groups["group"]
			switch bad {
			case "source":
				s.config.Nodes["b"].IPGroups = nil
			case "destination":
				s.config.Nodes["c"].IPGroups = nil
			case "ambiguous":
				s.config.Nodes["c"].IPGroups["eth1"] = []string{"group"}
			case "invalid":
				ips = []string{"bad-ip"}
			}
			if err := s.OrchestrateIPFailover("b", "c", ips); err == nil {
				t.Fatal("bad plan accepted")
			}
			if len(*events) > 0 {
				t.Fatalf("changed addresses before full preflight: %v", *events)
			}
		})
	}
}

func TestLocalReleaseCannotConfirmAnUnreadableInventory(t *testing.T) {
	s := newGroupDeleteTestServer(t)
	s.ipVerificationSnapshot = func() (ipStateLookup, error) { return nil, errors.New("inventory unavailable") }
	resp, err := s.RemoveIPFromGroup(context.Background(), &rpc.RemoveIPFromGroupRequest{GroupName: "group1", Ip: s.config.Groups["group1"][0]})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Success {
		t.Fatal("forgot configured address without observing release")
	}
	if len(s.config.Groups["group1"]) == 0 {
		t.Fatal("deleted unresolved config")
	}
}

func (p *transferPeer) MakePassive(context.Context, *rpc.MakePassiveRequest) (*rpc.MakePassiveResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	*p.events = append(*p.events, p.name+":demote")
	return &rpc.MakePassiveResponse{Success: false, Message: "address still held"}, nil
}

func TestSelfPromotionStopsOnRefusedDemotion(t *testing.T) {
	s, _, _, events := newTransferTest(t, "", "")
	s.config.Pulse.Mode = "active-passive"
	s.performPromotionAsync("a", nil, false)
	if s.memberList.GetMemberByID("a").GetStatus() != membership.StatusPassive {
		t.Fatal("self-promotion bypassed source refusal")
	}
	if !reflect.DeepEqual(*events, []string{"source:demote"}) {
		t.Fatalf("continued after refusal: %v", *events)
	}
}

func TestDestinationRefusalOrLegacyReplyCannotCompleteTransfer(t *testing.T) {
	for _, mode := range []string{"refuse", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			s, _, _, events := newTransferTest(t, "", mode)
			if err := s.OrchestrateIPFailover("b", "c", s.config.Groups["group"]); err == nil {
				t.Fatal("unverified destination reported completion")
			}
			if !reflect.DeepEqual(*events, []string{"source:down", "dest:up"}) {
				t.Fatalf("unexpected side effects: %v", *events)
			}
			if len(s.memberList.GetMemberByID("b").Claim().ActiveIPs) != 0 {
				t.Fatal("restored source after an ambiguous destination result")
			}
			if len(s.memberList.GetMemberByID("c").Claim().ActiveIPs) != 0 {
				t.Fatal("credited unverified acquisition")
			}
		})
	}
}
