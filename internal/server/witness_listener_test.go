package server

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/syleron/pulseha/internal/client"
	"github.com/syleron/pulseha/internal/clustertls"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/rpc"
)

// dialWitnessPath connects to addr on the Witness path's terms, as the node
// whose identity is in dir, expecting the far end to be expectID.
func dialWitnessPath(t *testing.T, dir string, cfg *config.Config, addr, expectID string) *client.Client {
	t.Helper()
	creds, err := clustertls.WitnessClientCredentials(dir, func() *config.Config { return cfg }, expectID)
	if err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(addr)
	c, err := client.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Connect(host, port, creds); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

// The Witness path serves one RPC, to a certificate the cluster names, asking as
// the node that certificate belongs to.
func TestTheWitnessListenerServesOneQuestionToTheNodeAsking(t *testing.T) {
	a, _, w := newWitnessTrio(t, "")
	addr := w.config.Nodes["c"].WitnessAddress
	asA := dialWitnessPath(t, a.certDir, a.config, addr, "c")
	ctx := context.Background()

	if resp, err := asA.Server().WitnessReport(ctx, &rpc.WitnessReportRequest{NodeId: "a", ClusterToken: "vote-test-secret"}); err != nil || !resp.GetSuccess() {
		t.Fatalf("a asking as a: %+v, %v", resp, err)
	}
	if _, err := asA.Server().WitnessReport(ctx, &rpc.WitnessReportRequest{NodeId: "b", ClusterToken: "vote-test-secret"}); err == nil {
		t.Error("a's certificate was allowed to ask as b")
	}
	if _, err := asA.Server().HealthCheck(ctx, &rpc.HealthCheckRequest{NodeId: "a"}); err == nil {
		t.Error("the Witness path served HealthCheck")
	}

	// A certificate the cluster does not name gets nothing.
	strangerDir := t.TempDir()
	stranger := mintIdentityInto(t, strangerDir, "stranger")
	strangerCfg := &config.Config{Nodes: map[string]*config.Node{"c": {TLSCert: w.config.Nodes["c"].TLSCert}, "x": {TLSCert: stranger}}}
	asStranger := dialWitnessPath(t, strangerDir, strangerCfg, addr, "c")
	if _, err := asStranger.Server().WitnessReport(ctx, &rpc.WitnessReportRequest{NodeId: "a", ClusterToken: "vote-test-secret"}); err == nil {
		t.Error("a certificate the cluster does not name was answered")
	}
}

// Configured but not trusted arbitrates nothing: one leg that is not verified
// TLS and no Report is acted on, and the status says so.
func TestConfiguredButNotTrustedArbitratesNothing(t *testing.T) {
	a, b, _ := newWitnessTrio(t, "")
	b.memberList.GetMemberByID("b").SetStatus(membership.StatusActive)
	if _, err := a.askWitness(context.Background()); err != nil {
		t.Fatalf("trusted path: %v", err)
	}
	if !witnessTrustedInStatus(t, a) {
		t.Error("status calls a fully trusted Witness path untrusted")
	}

	a.config.Nodes["b"].WitnessAddress = "" // b reached on its bind address, in a permissive cluster
	if _, err := a.askWitness(context.Background()); err != errWitnessNotTrusted {
		t.Errorf("askWitness with a plaintext leg = %v, want errWitnessNotTrusted", err)
	}
	if got := a.witnessAnswerFor(context.Background(), "b"); got != witnessSilent {
		t.Errorf("answer with a plaintext leg = %s, want silent", got)
	}
	if witnessTrustedInStatus(t, a) {
		t.Error("status calls the Witness trusted with a plaintext leg")
	}
}

func witnessTrustedInStatus(t *testing.T, s *Server) bool {
	t.Helper()
	resp, err := s.GetClusterStatus(context.Background(), &rpc.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range resp.Members {
		if m.Witness {
			return m.WitnessTrusted
		}
	}
	t.Fatal("no Witness in status")
	return false
}

func TestWitnessPathTrusted(t *testing.T) {
	full := func() map[string]*config.Node {
		return map[string]*config.Node{
			"a": {TLSCert: "x", WitnessAddress: "10.0.0.1:9084"},
			"b": {TLSCert: "y", WitnessAddress: "10.0.0.2:9084"},
			"w": {TLSCert: "z", WitnessAddress: "10.0.0.3:9084", Witness: true},
		}
	}
	if !witnessPathTrusted(full(), false) {
		t.Error("every leg on a witness_address with certificates published is trusted")
	}
	n := full()
	n["b"].TLSCert = ""
	if witnessPathTrusted(n, true) {
		t.Error("a member with no published certificate cannot be verified, whatever the mode")
	}
	n = full()
	n["b"].WitnessAddress = ""
	if witnessPathTrusted(n, false) {
		t.Error("a leg on a permissive bind address is plaintext")
	}
	if !witnessPathTrusted(n, true) {
		t.Error("a leg on a bind address that requires TLS is verified")
	}
	n = full()
	n["w"].Witness = false
	if witnessPathTrusted(n, false) {
		t.Error("no Witness at all reported as a trusted Witness")
	}
}

func TestSettingTheWitnessAddress(t *testing.T) {
	s := newRemoveIPTestServer(t)
	for _, bad := range []string{"10.0.0.1", "10.0.0.1:0", "10.0.0.1:http", ":9084"} {
		if resp, _ := s.UpdateConfig(context.Background(), &rpc.UpdateConfigRequest{Key: "witness_address", Value: bad}); resp.Success {
			t.Errorf("witness_address %q accepted", bad)
		}
	}
	addr := freeAddr(t)
	resp, err := s.UpdateConfig(context.Background(), &rpc.UpdateConfigRequest{Key: "witness_address", Value: addr})
	if err != nil || !resp.Success || !strings.Contains(resp.Message, "published") {
		t.Fatalf("UpdateConfig(witness_address) = %+v, %v", resp, err)
	}
	t.Cleanup(s.stopWitnessListener)
	if got := s.config.Nodes[removeIPLocal].WitnessAddress; got != addr {
		t.Errorf("recorded %q, want %q", got, addr)
	}
}

// witness_address is node-owned, like the certificate: a sync adopts every
// other node's and never this node's own, which a peer can only ever have a
// stale copy of.
func TestASyncNeverOverwritesThisNodesOwnWitnessAddress(t *testing.T) {
	withCertFile(t, "-----BEGIN CERTIFICATE-----\nmine\n-----END CERTIFICATE-----\n")
	s := newRemoveIPTestServer(t, startReleasingPeer(t, &releasingPeer{}))
	t.Cleanup(s.awaitAsyncReconfigures)
	s.Lock()
	s.config.Nodes[removeIPLocal].WitnessAddress = "10.9.9.1:9084"
	s.Unlock()

	payload := marshalConfigWith(t, s, func(id string, n *config.Node) {
		switch id {
		case removeIPLocal:
			n.WitnessAddress = "10.9.9.99:9084" // a stale copy of ours
		case "peer-0":
			n.WitnessAddress = "10.9.9.2:9084"
		}
	})
	if _, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: payload}); err != nil {
		t.Fatal(err)
	}
	s.RLock()
	mine, theirs := s.config.Nodes[removeIPLocal].WitnessAddress, s.config.Nodes["peer-0"].WitnessAddress
	s.RUnlock()
	if mine != "10.9.9.1:9084" {
		t.Errorf("own witness_address became %q, want it kept", mine)
	}
	if theirs != "10.9.9.2:9084" {
		t.Errorf("peer witness_address = %q, want it adopted from the sync", theirs)
	}
}

// marshalConfigWith serialises s's config with each node entry passed through
// edit, which is what a peer's ConfigSync payload looks like.
func marshalConfigWith(t *testing.T, s *Server, edit func(id string, n *config.Node)) []byte {
	t.Helper()
	s.RLock()
	defer s.RUnlock()
	clone := &config.Config{Pulse: s.config.Pulse, Groups: s.config.Groups, Plugins: s.config.Plugins, Nodes: map[string]*config.Node{}}
	for id, n := range s.config.Nodes {
		copied := *n
		edit(id, &copied)
		clone.Nodes[id] = &copied
	}
	b, err := json.Marshal(clone)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
