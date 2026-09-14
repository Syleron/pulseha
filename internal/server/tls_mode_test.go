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
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/packages/security"
	"github.com/syleron/pulseha/rpc"
	"google.golang.org/grpc"
)

// installNodeIdentity writes a keypair where this node's certificate lives and
// returns the PEM its config entry would carry.
func installNodeIdentity(t *testing.T, cn string) string {
	t.Helper()

	dir := t.TempDir()
	prev := security.CertDir
	security.CertDir = dir
	t.Cleanup(func() { security.CertDir = prev })

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
		node.TLSCert = installNodeIdentityPEMOnly(t, id)
	}
	if err := s.config.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	s.memberList.UpdateConfig(s.config)
}

// installNodeIdentityPEMOnly mints a certificate for a peer without touching the
// certificate directory — a peer's key never lives on this node.
func installNodeIdentityPEMOnly(t *testing.T, cn string) string {
	t.Helper()

	prev := security.CertDir
	pemOut := installNodeIdentity(t, cn)
	security.CertDir = prev
	return pemOut
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
