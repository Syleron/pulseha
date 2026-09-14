// PulseHA - HA Cluster Daemon
// Copyright (C) 2017-2021  Andrew Zak <andrew@linux.com>
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syleron/pulseha/internal/client"
	"github.com/syleron/pulseha/internal/clustertls"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/packages/security"
	"github.com/syleron/pulseha/rpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// installNodeIdentity mints a keypair and points security.CertDir at it for the
// rest of the test, so this process looks like the node that owns it.
//
// It writes a package-level variable, which nothing in production ever does —
// only tests move the certificate directory. That makes it safe in production
// and a hazard here: a detached goroutine from an earlier case (a join spawns
// one) is still reading it, and the race detector is right to say so. Where a
// test only needs a certificate and not an identity, use mintIdentityInto, which
// touches nothing shared.
func installNodeIdentity(t *testing.T, cn string) string {
	t.Helper()

	dir := t.TempDir()
	prev := security.CertDir
	security.CertDir = dir
	t.Cleanup(func() { security.CertDir = prev })

	return mintIdentityInto(t, dir, cn)
}

// mintIdentityInto writes a keypair into dir, laid out the way security.CertDir
// is, and returns the PEM a config entry would carry. It changes nothing shared.
func mintIdentityInto(t *testing.T, dir, cn string) string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(filepath.Join(dir, "pulseha.crt"), certPEM, 0600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pulseha.key"), keyPEM, 0600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return strings.TrimSpace(string(certPEM))
}

// stopListenerOnCleanup shuts the cluster listener down when the test ends.
//
// The flip reconfigures this node, which binds a real socket and serves on a
// goroutine. Left running, it outlives the test that started it — and every
// harness in this package points the package-level config.CONFIG_LOCATION at its
// own t.TempDir(), so a goroutine still reading the config when the next test
// writes that global is a genuine race (see onAsyncReconfigure).
func stopListenerOnCleanup(t *testing.T, s *Server) {
	t.Helper()
	t.Cleanup(func() {
		s.awaitAsyncReconfigures()
		s.Lock()
		srv := s.grpcServer
		s.grpcServer, s.grpcServerTLS = nil, false
		s.Unlock()
		if srv != nil {
			srv.Stop()
		}
	})
}

// publishCertificates gives every node in the server's config a certificate,
// with the local node's matching the keypair on disk, and saves it — which is
// what the permissive phase produces and what the flip requires.
func publishCertificates(t *testing.T, s *Server, localPEM string) {
	t.Helper()

	for id, node := range s.config.Nodes {
		if id == s.config.Pulse.LocalNode {
			node.TLSCert = localPEM
			continue
		}
		node.TLSCert = mintIdentityInto(t, t.TempDir(), id)
	}
	if err := s.config.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	s.memberList.UpdateConfig(s.config)
}

// The flip must not be reachable by a typo. A value that is neither of the two
// names is refused before anything is written, so a mistyped `config set` leaves
// the cluster exactly as it was.
func TestAnUnknownTLSModeIsRefused(t *testing.T) {
	s := newPropagationTestServer(t)

	resp, err := s.UpdateConfig(context.Background(), &rpc.UpdateConfigRequest{
		Key:   "tls_mode",
		Value: "requird",
	})
	if err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if resp.Success {
		t.Fatal("a mistyped tls_mode was accepted")
	}
	if got := s.config.Pulse.TLSMode; got != "" {
		t.Errorf("tls_mode = %q after a refused set, want it untouched", got)
	}
}

// The precondition, reached through the path an operator actually uses. A peer
// that has not published cannot be trusted after the flip and would be severed
// by it, so the flip does not start.
func TestTheFlipIsRefusedWhileAPeerHasNotPublished(t *testing.T) {
	_, addr := startRecordingPeer(t)
	s := newPropagationTestServer(t, addr)

	localPEM := installNodeIdentity(t, "local-node")
	s.config.Nodes[s.config.Pulse.LocalNode].TLSCert = localPEM
	// The peer publishes nothing, which is a cluster still accumulating.

	resp, err := s.UpdateConfig(context.Background(), &rpc.UpdateConfigRequest{
		Key:   "tls_mode",
		Value: config.TLSModeRequired,
	})
	if err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if resp.Success {
		t.Fatal("the cluster was flipped with a peer that had published no certificate; " +
			"that peer would have been cut off by the change that did it")
	}
	if !strings.Contains(resp.Message, "no published certificate") {
		t.Errorf("message = %q, want it to say which precondition failed", resp.Message)
	}
	if got := s.config.Pulse.TLSMode; got != "" {
		t.Errorf("tls_mode = %q after a refused flip, want it untouched", got)
	}
}

// A node whose peers have all published but whose own key is not on disk would
// come up serving nothing. It is asked of itself before the change is written,
// because it is the only node that can answer.
func TestTheFlipIsRefusedWhenThisNodeCannotServeIt(t *testing.T) {
	_, addr := startRecordingPeer(t)
	s := newPropagationTestServer(t, addr)

	localPEM := installNodeIdentity(t, "local-node")
	publishCertificates(t, s, localPEM)

	// Every certificate is published and every member is up; only this node's
	// private key is missing.
	if err := os.Remove(filepath.Join(security.CertDir, "pulseha.key")); err != nil {
		t.Fatalf("remove key: %v", err)
	}

	resp, err := s.UpdateConfig(context.Background(), &rpc.UpdateConfigRequest{
		Key:   "tls_mode",
		Value: config.TLSModeRequired,
	})
	if err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if resp.Success {
		t.Fatal("a node with no private key flipped the cluster to required")
	}
	if !strings.Contains(resp.Message, "cannot serve TLS") {
		t.Errorf("message = %q, want it to say this node is the problem", resp.Message)
	}
	if got := s.config.Pulse.TLSMode; got != "" {
		t.Errorf("tls_mode = %q after a refused flip, want it untouched", got)
	}
}

// The flip a healthy cluster is allowed to make: recorded, broadcast to the
// peers over the plaintext channel that still works, and applied here.
func TestAHealthyClusterFlipsAndTheListenerFollows(t *testing.T) {
	peer, addr := startRecordingPeer(t)
	s := newPropagationTestServer(t, addr)
	s.startConfigBroadcaster()
	t.Cleanup(s.stopConfigBroadcaster)
	stopListenerOnCleanup(t, s)

	localPEM := installNodeIdentity(t, "local-node")
	publishCertificates(t, s, localPEM)

	resp, err := s.UpdateConfig(context.Background(), &rpc.UpdateConfigRequest{
		Key:   "tls_mode",
		Value: config.TLSModeRequired,
	})
	if err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if !resp.Success {
		t.Fatalf("a healthy, fully published cluster refused the flip: %s", resp.Message)
	}

	if got := s.currentConfig().Pulse.TLSMode; got != config.TLSModeRequired {
		t.Errorf("tls_mode = %q, want %q", got, config.TLSModeRequired)
	}
	if !s.grpcServerTLS {
		t.Error("the flip was recorded but this node's listener is still serving plaintext; " +
			"the node has told its peers to encrypt and has not done so itself")
	}

	// The peer here serves plaintext, which is what every peer is at the instant
	// the flip is made. Delivering the change on the cluster's new terms would
	// offer TLS to a listener that does not speak it yet, and the one node that
	// most needs the message -- the one about to be cut off by it -- is the one
	// that would never get it.
	if ok, seen := awaitPulseValue(t, peer, "tls_mode", config.TLSModeRequired, 10*time.Second); !ok {
		t.Fatalf("the peer was never told about the flip (last value seen: %v). It has to "+
			"go out over the plaintext channel the flip removes, before this node "+
			"moves onto the new terms", seen)
	}
}

// A peer that cannot be reached at the moment of delivery is isolated by the
// change, so the operator is told which one and the command does not pretend it
// reached the cluster.
func TestAPeerThatMissesTheFlipIsNamed(t *testing.T) {
	_, addr := startRecordingPeer(t)
	s := newPropagationTestServer(t, addr)
	stopListenerOnCleanup(t, s)

	localPEM := installNodeIdentity(t, "local-node")
	publishCertificates(t, s, localPEM)

	// The peer is in the config and in the member list, and its listener is gone.
	for id, node := range s.config.Nodes {
		if id != s.config.Pulse.LocalNode {
			node.Port = "1" // nothing listens here
		}
	}
	if err := s.config.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	s.memberList.UpdateConfig(s.config)

	resp, err := s.UpdateConfig(context.Background(), &rpc.UpdateConfigRequest{
		Key:   "tls_mode",
		Value: config.TLSModeRequired,
	})
	if err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if !strings.Contains(resp.Message, "peer-") {
		t.Errorf("message = %q, want it to name the node that did not take the change; "+
			"that node is now unreachable and an operator has to go to it", resp.Message)
	}
}

// Setting the mode it is already in is a no-op that succeeds, not a rebind. A
// second `config set` must not tear the listener down for nothing — that is
// defect #31's cost, paid on a command that changed nothing.
func TestSettingTheSameTLSModeChangesNothing(t *testing.T) {
	s := newPropagationTestServer(t)
	s.config.Pulse.TLSMode = config.TLSModePermissive

	resp, err := s.UpdateConfig(context.Background(), &rpc.UpdateConfigRequest{
		Key:   "tls_mode",
		Value: config.TLSModePermissive,
	})
	if err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if !resp.Success {
		t.Fatalf("setting the current mode failed: %s", resp.Message)
	}
	if !strings.Contains(resp.Message, "already") {
		t.Errorf("message = %q, want it to say nothing changed", resp.Message)
	}
}

// Reconfigure leaves a listener alone when nothing about it changed, which is
// defect #31's fix and is why the flip has to be part of what "changed" means.
// A listener that stayed up across the flip would serve plaintext to peers that
// had stopped speaking it.
func TestTheListenerRecordDistinguishesAModeChangeFromNoChange(t *testing.T) {
	s := &Server{}
	srv := grpc.NewServer()
	t.Cleanup(srv.Stop)

	s.setClusterListener(srv, "127.0.0.1:8080", false)

	if !s.clusterListenerServing("127.0.0.1:8080", false) {
		t.Error("a listener was rebound for a config change that did not touch it (defect #31)")
	}
	if s.clusterListenerServing("127.0.0.1:8080", true) {
		t.Error("a plaintext listener was left serving across the flip to required")
	}
	if s.clusterListenerServing("127.0.0.1:9090", false) {
		t.Error("a listener was left serving after its bind address moved")
	}

	s.setClusterListener(srv, "127.0.0.1:8080", true)
	if s.clusterListenerServing("127.0.0.1:8080", false) {
		t.Error("a TLS listener was left serving across the flip back to permissive")
	}
}

// The token an operator carries names this node's certificate once the cluster
// requires TLS, and does not pretend to before then.
func TestTheJoinTokenCarriesTheFingerprintOnlyWhenItMeansSomething(t *testing.T) {
	s := newPropagationTestServer(t)
	localPEM := installNodeIdentity(t, "local-node")
	s.config.Pulse.ClusterToken = "a-secret"
	s.config.Nodes[s.config.Pulse.LocalNode].TLSCert = localPEM
	s.memberList.UpdateConfig(s.config)

	// Permissive: there is no handshake to pin, so the token must not look as
	// though it pins one.
	if got := s.presentableTokenLocked(s.config, "a-secret"); got != "a-secret" {
		t.Errorf("permissive token = %q, want the bare secret; a pin that cannot be "+
			"checked is a claim the token cannot back", got)
	}

	s.config.Pulse.TLSMode = config.TLSModeRequired
	s.memberList.UpdateConfig(s.config)

	secret, fingerprint, pinned := clustertls.ParseJoinToken(s.presentableTokenLocked(s.config, "a-secret"))
	if !pinned {
		t.Fatal("a cluster that requires TLS issued a token with no fingerprint; a joiner " +
			"holding it has nothing to check the cluster against")
	}
	if secret != "a-secret" {
		t.Errorf("secret = %q, want it carried through untouched", secret)
	}
	want, err := clustertls.Fingerprint(localPEM)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if fingerprint != want {
		t.Errorf("fingerprint = %q, want this node's own %q", fingerprint, want)
	}
}

// The bootstrap, end to end: a node with no cluster and no trust set reaches one
// that requires TLS and asks to join it, holding nothing but the token.
//
// Driven against a real TLS listener with the real interceptor in front of it,
// because every piece of this passed on its own while joining such a cluster was
// impossible. The handshake has to accept an unnamed certificate, the interceptor
// has to let it through to Join and nowhere else, and the joiner has to be
// verifying the far end against the token's fingerprint — all three at once, or
// there is no way into the cluster.
func TestANodeJoinsAClusterThatRequiresTLS(t *testing.T) {
	target, _, _ := authorisationTestServer(t)
	target.config.Pulse.ClusterToken = "the-shared-secret"
	target.memberList.UpdateConfig(target.config)

	addr := serveWithAuthorisation(t, target)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}

	// Captured while security.CertDir still points at the target, because this is
	// the one thing that is read off its disk.
	token := target.presentableTokenLocked(target.config, target.config.Pulse.ClusterToken)
	secret, pin, pinned := clustertls.ParseJoinToken(token)
	if !pinned {
		t.Fatalf("token %q carries no fingerprint for the joiner to pin", token)
	}

	// The joining node, with its own identity and no cluster at all.
	installNodeIdentity(t, "joiner")

	dialPinned := func(t *testing.T, fingerprint string) *client.Client {
		t.Helper()
		creds, err := clustertls.PinnedClientCredentials(fingerprint)
		if err != nil {
			t.Fatalf("PinnedClientCredentials: %v", err)
		}
		c, err := client.New()
		if err != nil {
			t.Fatalf("client.New: %v", err)
		}
		if err := c.Connect(host, port, creds); err != nil {
			t.Fatalf("Connect: %v", err)
		}
		t.Cleanup(c.Close)
		return c
	}

	t.Run("with the token the cluster issued", func(t *testing.T) {
		c := dialPinned(t, pin)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		resp, err := c.CLI().Join(ctx, &rpc.JoinRequest{
			Hostname: "joiner",
			NodeId:   "uuid-joiner",
			BindIp:   "127.0.0.1",
			BindPort: "9083",
			Token:    secret,
			TlsCert:  localCertificatePEM(),
		})
		if err != nil {
			t.Fatalf("a node holding the cluster's own token could not join it: %v", err)
		}
		if !resp.Success {
			t.Fatalf("join refused: %s", resp.Message)
		}

		// And it is in the trust set the moment it is in the config, which is what
		// step 3a bought and what makes the next connection an ordinary one.
		target.RLock()
		recorded := target.config.Nodes["uuid-joiner"]
		target.RUnlock()
		if recorded == nil || recorded.TLSCert == "" {
			t.Error("the joiner is in the cluster config with no certificate against it, " +
				"so nothing it does next will be authorised")
		}
	})

	t.Run("with a token naming a different certificate", func(t *testing.T) {
		// Minted without moving security.CertDir: the successful join above left a
		// broadcast goroutine of its own running, and it reads that global.
		otherPEM := mintIdentityInto(t, t.TempDir(), "impostor")
		wrongPin, err := clustertls.Fingerprint(otherPEM)
		if err != nil {
			t.Fatalf("Fingerprint: %v", err)
		}

		creds, err := clustertls.PinnedClientCredentials(wrongPin)
		if err != nil {
			t.Fatalf("PinnedClientCredentials: %v", err)
		}
		c, err := client.New()
		if err != nil {
			t.Fatalf("client.New: %v", err)
		}
		defer c.Close()
		if err := c.Connect(host, port, creds); err != nil {
			return // refused at the dial, which is the earliest it can be
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := c.CLI().Join(ctx, &rpc.JoinRequest{
			Hostname: "impostor", NodeId: "uuid-impostor", Token: secret,
		}); err == nil {
			t.Fatal("a node joined a cluster whose certificate its token does not name; " +
				"that is the trust-on-first-use the pin exists to remove")
		}
	})
}

// The Token RPC, driven as the CLI drives it.
//
// It had no test at all, which is how it came to hold s.Lock() across a helper
// that reached for a read lock — an unconditional deadlock on the one command an
// operator runs before every join, and one nothing in the suite would have hit.
// Driving the RPC rather than the helper is the point: the lock is the RPC's, so
// a test that called past it would pass against a daemon that wedged.
func TestTheTokenRPCHandsOutAUsableToken(t *testing.T) {
	s := newPropagationTestServer(t)
	localPEM := installNodeIdentity(t, "local-node")
	s.config.Pulse.ClusterToken = "the-shared-secret"
	s.config.Nodes[s.config.Pulse.LocalNode].TLSCert = localPEM
	s.memberList.UpdateConfig(s.config)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Run("permissive", func(t *testing.T) {
		resp, err := s.Token(ctx, &rpc.TokenRequest{})
		if err != nil {
			t.Fatalf("Token: %v", err)
		}
		if !resp.Success {
			t.Fatalf("Token refused: %s", resp.Message)
		}
		if resp.Token != "the-shared-secret" {
			t.Errorf("token = %q, want the bare secret", resp.Token)
		}
	})

	t.Run("required", func(t *testing.T) {
		s.Lock()
		s.config.Pulse.TLSMode = config.TLSModeRequired
		s.Unlock()
		s.memberList.UpdateConfig(s.config)

		resp, err := s.Token(ctx, &rpc.TokenRequest{})
		if err != nil {
			t.Fatalf("Token: %v", err)
		}
		if !resp.Success {
			t.Fatalf("Token refused: %s", resp.Message)
		}
		secret, fingerprint, pinned := clustertls.ParseJoinToken(resp.Token)
		if !pinned {
			t.Fatal("a cluster requiring TLS handed out a token with nothing to pin")
		}
		if secret != "the-shared-secret" {
			t.Errorf("secret = %q, want it carried through untouched", secret)
		}
		want, err := clustertls.Fingerprint(localPEM)
		if err != nil {
			t.Fatalf("Fingerprint: %v", err)
		}
		if fingerprint != want {
			t.Errorf("fingerprint = %q, want this node's own", fingerprint)
		}
	})

	// The shape that found the deadlock: a Server with no member list, which is
	// what several call sites and most of this package's tests build, and which
	// #111 step 3a already caught one panic on.
	t.Run("with no member list", func(t *testing.T) {
		bare := newPropagationTestServer(t)
		bare.config.Pulse.ClusterToken = "another-secret"
		bare.config.Pulse.TLSMode = config.TLSModeRequired
		bare.memberList = nil

		resp, err := bare.Token(ctx, &rpc.TokenRequest{})
		if err != nil {
			t.Fatalf("Token: %v", err)
		}
		if !resp.Success {
			t.Fatalf("Token refused: %s", resp.Message)
		}
	})
}

// startTLSRecordingPeer is a recordingPeer that serves the cluster's credentials,
// which is what every peer looks like once the cluster is on `required`.
func startTLSRecordingPeer(t *testing.T, snapshot func() *config.Config) (*recordingPeer, string) {
	t.Helper()

	creds, err := clustertls.ServerCredentials(snapshot)
	if err != nil {
		t.Fatalf("ServerCredentials: %v", err)
	}
	if creds == nil {
		t.Fatal("no server credentials for a cluster that requires TLS")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	peer := &recordingPeer{}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(creds)))
	rpc.RegisterServerServer(srv, peer)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	return peer, ln.Addr().String()
}

// Going back to permissive has to be delivered over TLS, because that is what
// the peers are still serving.
//
// The direction nobody thinks to test, and the one that severs a cluster
// permanently if it is wrong: a plaintext push is refused by every peer still on
// `required`, this node then reconfigures to plaintext, and the two halves are
// each talking a protocol the other has stopped accepting, in both directions,
// with no path left to repair it.
func TestTheFlipBackIsDeliveredOnTheTermsThePeersAreStillOn(t *testing.T) {
	s := newPropagationTestServer(t)
	localPEM := installNodeIdentity(t, "local-node")
	localID := s.config.Pulse.LocalNode
	s.config.Nodes[localID].TLSCert = localPEM
	s.config.Pulse.TLSMode = config.TLSModeRequired

	// A peer serving TLS, the way one looks after the cluster has been flipped.
	// It shares this node's view of the cluster, so it has to be in it.
	peerDir := t.TempDir()
	peerPEM := mintIdentityInto(t, peerDir, "peer-0")
	s.config.Nodes["peer-0"] = &config.Node{Hostname: "peer-0", TLSCert: peerPEM}
	s.memberList.UpdateConfig(s.config)

	peerView := func() *config.Config { return s.config }
	peer, addr := func() (*recordingPeer, string) {
		prev := security.CertDir
		security.CertDir = peerDir
		defer func() { security.CertDir = prev }()
		return startTLSRecordingPeer(t, peerView)
	}()

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	s.config.Nodes["peer-0"].IP, s.config.Nodes["peer-0"].Port = host, port
	if err := s.config.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.memberList.AddMemberQuiet("peer-0"); err != nil {
		t.Fatalf("AddMemberQuiet: %v", err)
	}
	s.memberList.GetMemberByID("peer-0").SetStatus(membership.StatusPassive)
	s.memberList.UpdateConfig(s.config)
	stopListenerOnCleanup(t, s)

	resp, err := s.UpdateConfig(context.Background(), &rpc.UpdateConfigRequest{
		Key:   "tls_mode",
		Value: config.TLSModePermissive,
	})
	if err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if !resp.Success {
		t.Fatalf("the flip back was refused: %s", resp.Message)
	}

	if ok, seen := awaitPulseValue(t, peer, "tls_mode", config.TLSModePermissive, 10*time.Second); !ok {
		t.Fatalf("a peer still serving TLS was never told the cluster had gone back to "+
			"plaintext (last value seen: %v). It would keep refusing this node's "+
			"plaintext dials while this node refuses its TLS ones, with no way left "+
			"to repair either side", seen)
	}
	if strings.Contains(resp.Message, "except") {
		t.Errorf("message = %q, want no peer reported as missed", resp.Message)
	}
}

// The receiving half of the flip, which is the half that runs on every node
// except the one the operator typed on.
//
// A node learns the cluster requires TLS from an ordinary ConfigSync, and has to
// do two things with it: adopt the value rather than preserve its own, and rebind
// its listener. Neither was covered — the propagation tests all check what a peer
// was *sent*, and `tls_mode` sits in the same struct as the node-local logging
// keys that ConfigSync deliberately does not adopt. A `tls_mode` that ended up on
// that preserve list would look exactly like a working flip on the node that
// issued it, and leave every other node plaintext.
func TestANodeLearnsTheFlipFromAnOrdinaryConfigSync(t *testing.T) {
	s := newPropagationTestServer(t)
	localPEM := installNodeIdentity(t, "local-node")
	localID := s.config.Pulse.LocalNode
	s.config.Nodes[localID].TLSCert = localPEM
	s.config.Nodes[localID].IP, s.config.Nodes[localID].Port = "127.0.0.1", "0"
	// Node-local settings this node must keep through the same sync, so the test
	// can tell "adopted everything" from "adopted the right thing".
	s.config.Pulse.LoggingLevel = "debug"
	if err := s.config.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	s.memberList.UpdateConfig(s.config)
	stopListenerOnCleanup(t, s)

	// A listener already up and serving plaintext, which is what a node looks like
	// when the flip reaches it. Without this the test proves much less than it
	// appears to: Reconfigure only consults the TLS term when a listener is
	// already serving the same address, so a node with nothing bound rebinds
	// anyway and would pass whatever that term said.
	localNode, err := s.config.GetLocalNode()
	if err != nil {
		t.Fatalf("GetLocalNode: %v", err)
	}
	if err := s.startClusterListener(localNode); err != nil {
		t.Fatalf("startClusterListener: %v", err)
	}
	if s.grpcServerTLS {
		t.Fatal("the listener was already serving TLS before the sync")
	}
	s.RLock()
	boundAddr := fmt.Sprintf("%s:%s", s.config.Nodes[localID].IP, s.config.Nodes[localID].Port)
	s.RUnlock()
	if !s.clusterListenerServing(boundAddr, false) {
		t.Fatalf("no plaintext listener is recorded as serving %s, so the rebind this "+
			"test is about cannot be observed", boundAddr)
	}

	// What a peer that has just been flipped sends: the cluster config with
	// tls_mode set, and its own idea of the node-local keys.
	payload := func() []byte {
		s.Lock()
		defer s.Unlock()
		clone := &config.Config{
			Pulse:   s.config.Pulse,
			Groups:  s.config.Groups,
			Plugins: s.config.Plugins,
			Nodes:   map[string]*config.Node{},
		}
		clone.Pulse.TLSMode = config.TLSModeRequired
		clone.Pulse.LoggingLevel = "error" // the sender's, which must not be adopted
		for id, n := range s.config.Nodes {
			copied := *n
			clone.Nodes[id] = &copied
		}
		b, err := json.Marshal(clone)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}()

	resp, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: payload})
	if err != nil {
		t.Fatalf("ConfigSync: %v", err)
	}
	if !resp.Success {
		t.Fatalf("ConfigSync refused: %s", resp.Message)
	}
	s.awaitAsyncReconfigures()

	if got := s.currentConfig().Pulse.TLSMode; got != config.TLSModeRequired {
		t.Fatalf("tls_mode = %q after the sync, want %q; a node that does not adopt it "+
			"stays plaintext while the rest of the cluster encrypts, and is cut off",
			got, config.TLSModeRequired)
	}
	if got := s.currentConfig().Pulse.LoggingLevel; got != "debug" {
		t.Errorf("logging_level = %q, want this node's own %q kept; adopting the whole "+
			"pulseha section is not the same as adopting the cluster-scoped part of it",
			got, "debug")
	}
	if !s.grpcServerTLS {
		t.Error("the node adopted tls_mode=required and its listener is still serving " +
			"plaintext; the rebind is what the flip actually is on a receiving node")
	}
}
