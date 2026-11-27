package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/rpc"
)

// probeCall is one call into the duplicate address probe seam.
type probeCall struct{ iface, ip string }

// stubAddedIPProbe replaces the wire probe with a fixed answer and records what
// it was asked. The interface newAddIPTestServer assigns the group to does not
// exist, so without this the real probe would report nothing to ask.
func stubAddedIPProbe(s *Server, holder string, held bool, err error) *[]probeCall {
	calls := &[]probeCall{}
	s.probeAddedIP = func(iface, ip string) (string, bool, error) {
		*calls = append(*calls, probeCall{iface, ip})
		return holder, held, err
	}
	return calls
}

// The fault this exists for. A floating IP was configured with the address of a
// machine that was in service; the daemon brought it up and announced it, and
// the machine was unreachable from then on. Nothing between the request and the
// announcement asked whether the address was free.
//
// A refused add must leave nothing behind: not in memory, not on disk, and no
// attempt to place the address.
func TestAddIPToGroupRefusesAnAddressAnotherHostAnswersFor(t *testing.T) {
	s := newAddIPTestServer(t)
	calls := stubAddedIPProbe(s, "00:50:56:8c:e5:3a", true, nil)
	placed := false
	s.placeAddedIP = func(string, string) (bool, string) {
		placed = true
		return true, ""
	}

	resp, err := s.AddIPToGroup(context.Background(), &rpc.AddIPToGroupRequest{
		GroupName:    "group1",
		Ip:           "172.16.73.43/24",
		VerifyUnused: true,
	})
	if err != nil {
		t.Fatalf("AddIPToGroup: %v", err)
	}

	if resp.Success {
		t.Fatalf("Success = true (%q); the address belongs to another host", resp.Message)
	}
	// The operator has to be able to act on the message: which address, where,
	// and who has it.
	for _, want := range []string{"172.16.73.43", "pulseha-no-such-interface", "00:50:56:8c:e5:3a", "group1"} {
		if !strings.Contains(resp.Message, want) {
			t.Errorf("message %q does not mention %q", resp.Message, want)
		}
	}
	if len(*calls) != 1 {
		t.Fatalf("probe called %d times, want 1", len(*calls))
	}
	// Asked on the interface this node carries the group on, for the bare
	// address: the prefix length is not part of the question.
	if got := (*calls)[0]; got.iface != "pulseha-no-such-interface" || got.ip != "172.16.73.43" {
		t.Errorf("probed %+v, want the group's interface and the bare address", got)
	}

	if placed {
		t.Error("the address was placed after the probe found it in use")
	}
	if slices.Contains(s.config.Groups["group1"], "172.16.73.43/24") {
		t.Errorf("group1 = %v; a refused address was committed", s.config.Groups["group1"])
	}
	raw, err := os.ReadFile(config.CONFIG_LOCATION)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var onDisk config.Config
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if slices.Contains(onDisk.Groups["group1"], "172.16.73.43/24") {
		t.Errorf("persisted group1 = %v; a refused address reached the disk", onDisk.Groups["group1"])
	}
}

// A reply with no MAC in it is still a reply. The message has to stand without
// one.
func TestAddIPToGroupRefusesWithoutAHolderAddress(t *testing.T) {
	s := newAddIPTestServer(t)
	stubAddedIPProbe(s, "", true, nil)

	resp, err := s.AddIPToGroup(context.Background(), &rpc.AddIPToGroupRequest{
		GroupName:    "group1",
		Ip:           "172.16.73.43/24",
		VerifyUnused: true,
	})
	if err != nil {
		t.Fatalf("AddIPToGroup: %v", err)
	}
	if resp.Success || !strings.Contains(resp.Message, "another host") {
		t.Fatalf("Success=%v message=%q, want a refusal naming another host", resp.Success, resp.Message)
	}
}

// The check is the caller's to ask for. On a network that answers ARP for every
// address (a cloud virtual network, a router running proxy ARP) the probe finds
// a "holder" for anything, so a request that does not ask must not be probed
// and must behave exactly as it did before the check existed.
func TestAddIPToGroupDoesNotProbeUnlessAsked(t *testing.T) {
	s := newAddIPTestServer(t)
	calls := stubAddedIPProbe(s, "12:34:56:78:9a:bc", true, nil)

	resp, err := s.AddIPToGroup(context.Background(), &rpc.AddIPToGroupRequest{
		GroupName: "group1",
		Ip:        "10.0.0.8/24",
	})
	if err != nil {
		t.Fatalf("AddIPToGroup: %v", err)
	}
	if !resp.Success {
		t.Fatalf("Success = false (%q); the request did not ask for the check", resp.Message)
	}
	if len(*calls) != 0 {
		t.Errorf("probe called %d times for a request that did not ask", len(*calls))
	}
	if !slices.Contains(s.config.Groups["group1"], "10.0.0.8/24") {
		t.Errorf("group1 = %v, want it to contain 10.0.0.8/24", s.config.Groups["group1"])
	}
}

func TestAddIPToGroupAddsAnAddressNobodyAnswersFor(t *testing.T) {
	s := newAddIPTestServer(t)
	calls := stubAddedIPProbe(s, "", false, nil)

	resp, err := s.AddIPToGroup(context.Background(), &rpc.AddIPToGroupRequest{
		GroupName:    "group1",
		Ip:           "10.0.0.9/24",
		VerifyUnused: true,
	})
	if err != nil {
		t.Fatalf("AddIPToGroup: %v", err)
	}
	if !resp.Success {
		t.Fatalf("Success = false (%q); nothing answered for the address", resp.Message)
	}
	if len(*calls) != 1 {
		t.Errorf("probe called %d times, want 1", len(*calls))
	}
	if !slices.Contains(s.config.Groups["group1"], "10.0.0.9/24") {
		t.Errorf("group1 = %v, want it to contain 10.0.0.9/24", s.config.Groups["group1"])
	}
}

// The probe is a safety net. When it cannot give an answer (arping missing, the
// link down, the process stuck) the add goes ahead as it always did, and says
// that the check was not made.
func TestAddIPToGroupAddsWhenTheProbeCannotAnswer(t *testing.T) {
	s := newAddIPTestServer(t)
	stubAddedIPProbe(s, "", false, errors.New("arping: Interface is down"))

	resp, err := s.AddIPToGroup(context.Background(), &rpc.AddIPToGroupRequest{
		GroupName:    "group1",
		Ip:           "10.0.0.9/24",
		VerifyUnused: true,
	})
	if err != nil {
		t.Fatalf("AddIPToGroup: %v", err)
	}
	if !resp.Success {
		t.Fatalf("Success = false (%q); a probe that failed must not refuse the address", resp.Message)
	}
	if !slices.Contains(s.config.Groups["group1"], "10.0.0.9/24") {
		t.Errorf("group1 = %v, want it to contain 10.0.0.9/24", s.config.Groups["group1"])
	}
	warned := slices.ContainsFunc(resp.Warnings, func(w string) bool {
		return strings.Contains(w, "Could not check") && strings.Contains(w, "Interface is down")
	})
	if !warned {
		t.Errorf("warnings = %v; the caller is not told the check was skipped, or why", resp.Warnings)
	}
}

// The appliance re-adds every floating IP on each daemon start. Those are
// already in the configuration, the handler answers them without touching an
// interface, and a second each for the probe would turn a restart with a few
// hundred addresses into minutes. It would also be wrong: an address the
// cluster already holds is answered for, by the cluster.
func TestAddIPToGroupDoesNotProbeAnAddressAlreadyConfigured(t *testing.T) {
	s := newAddIPTestServer(t)
	calls := stubAddedIPProbe(s, "00:50:56:8c:e5:3a", true, nil)

	resp, err := s.AddIPToGroup(context.Background(), &rpc.AddIPToGroupRequest{
		GroupName:    "group1",
		Ip:           "10.0.0.1/24", // seeded by newAddIPTestServer
		VerifyUnused: true,
	})
	if err != nil {
		t.Fatalf("AddIPToGroup: %v", err)
	}
	if !resp.Success {
		t.Errorf("Success = false (%q); a re-add of a configured address is idempotent", resp.Message)
	}
	if len(*calls) != 0 {
		t.Errorf("probe called %d times for an address already in the configuration", len(*calls))
	}
}

// Requests the handler rejects on its own account keep its message. A probe
// answer in front of "group does not exist" would send the operator after the
// wrong fault.
func TestAddIPToGroupDoesNotProbeARequestItWillReject(t *testing.T) {
	tests := []struct {
		name, group, ip string
	}{
		{"unknown group", "no-such-group", "10.0.0.9/24"},
		{"unparseable address", "group1", "not-an-address"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newAddIPTestServer(t)
			calls := stubAddedIPProbe(s, "00:50:56:8c:e5:3a", true, nil)

			resp, err := s.AddIPToGroup(context.Background(), &rpc.AddIPToGroupRequest{
				GroupName:    tt.group,
				Ip:           tt.ip,
				VerifyUnused: true,
			})
			if err != nil {
				t.Fatalf("AddIPToGroup: %v", err)
			}
			if resp.Success {
				t.Fatalf("Success = true for %s", tt.name)
			}
			if len(*calls) != 0 {
				t.Errorf("probe called %d times, want 0", len(*calls))
			}
			if strings.Contains(resp.Message, "already in use") {
				t.Errorf("message %q is the probe's, not the handler's", resp.Message)
			}
		})
	}
}

// A group this node carries on no interface has no segment to ask from here.
func TestAddIPToGroupDoesNotProbeAGroupThisNodeDoesNotCarry(t *testing.T) {
	s := newAddIPTestServer(t)
	calls := stubAddedIPProbe(s, "00:50:56:8c:e5:3a", true, nil)
	s.config.Groups["elsewhere"] = []string{}

	resp, err := s.AddIPToGroup(context.Background(), &rpc.AddIPToGroupRequest{
		GroupName:    "elsewhere",
		Ip:           "10.0.9.9/24",
		VerifyUnused: true,
	})
	if err != nil {
		t.Fatalf("AddIPToGroup: %v", err)
	}
	if !resp.Success {
		t.Errorf("Success = false (%q)", resp.Message)
	}
	if len(*calls) != 0 {
		t.Errorf("probe called %d times for a group with no local interface", len(*calls))
	}
}

// The probe waits on the wire for about a second. AddIPToGroup keeps its
// announcement off the server lock because an arping held under it stalls every
// other RPC, and the probe has to honour the same rule. Taking the lock from
// inside the probe deadlocks if the handler is holding it.
func TestAddIPToGroupProbesWithoutHoldingTheServerLock(t *testing.T) {
	s := newAddIPTestServer(t)
	acquired := make(chan struct{})
	s.probeAddedIP = func(string, string) (string, bool, error) {
		go func() {
			s.Lock()
			s.Unlock() //nolint:staticcheck // taking it is the assertion
			close(acquired)
		}()
		select {
		case <-acquired:
		case <-time.After(5 * time.Second):
			t.Error("the server lock was held across the duplicate address probe")
		}
		return "", false, nil
	}

	if _, err := s.AddIPToGroup(context.Background(), &rpc.AddIPToGroupRequest{
		GroupName:    "group1",
		Ip:           "10.0.0.9/24",
		VerifyUnused: true,
	}); err != nil {
		t.Fatalf("AddIPToGroup: %v", err)
	}
}
