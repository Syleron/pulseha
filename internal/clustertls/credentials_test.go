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

package clustertls

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/packages/security"
)

// mintIdentity writes a fresh keypair into its own directory, laid out the way
// security.CertDir is, and returns the directory and the certificate PEM a
// config entry would carry.
//
// A directory per node rather than one shared one, so a test can give each end
// of a handshake its own identity -- which is the only way to test that the two
// ends verify each other rather than themselves.
func mintIdentity(t *testing.T, cn string) (dir, certPEM string) {
	t.Helper()

	dir = t.TempDir()

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
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}

	certBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(filepath.Join(dir, "pulseha.crt"), certBytes, 0600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pulseha.key"), keyPEM, 0600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return dir, strings.TrimSpace(string(certBytes))
}

// credentialsAs builds what the node whose identity lives in dir would serve and
// offer, given the cluster config it can currently see.
func credentialsAs(t *testing.T, dir string, snapshot Snapshot) *tls.Config {
	t.Helper()

	prev := security.CertDir
	security.CertDir = dir
	defer func() { security.CertDir = prev }()

	cfg, err := Credentials(snapshot)
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	if cfg == nil {
		t.Fatal("Credentials returned no credentials for a cluster that requires TLS")
	}
	return cfg
}

// requiredConfig is a flipped cluster whose config names the given nodes.
func requiredConfig(nodes map[string]*config.Node) *config.Config {
	cfg := &config.Config{Nodes: nodes}
	cfg.Pulse.TLSMode = config.TLSModeRequired
	return cfg
}

func snapshotOf(cfg *config.Config) Snapshot {
	return func() *config.Config { return cfg }
}

// handshake runs a real TLS handshake between two endpoints over a socket pair,
// returning the server's error and the client's.
//
// A real handshake rather than calling VerifyPeerCertificate directly, because
// what is under test is as much the rest of the tls.Config as the callback:
// RequireAnyClientCert is what makes the server ask for a certificate at all,
// and a test that reached past it would pass with the server accepting anonymous
// clients.
func handshake(t *testing.T, serverCfg, clientCfg *tls.Config) (serverErr, clientErr error) {
	t.Helper()

	// A loopback socket rather than net.Pipe. The pipe is unbuffered and
	// synchronous, so a tls.Conn closing against a peer that has stopped reading
	// waits out crypto/tls's own five-second close_notify deadline -- which turned
	// every case here into a five-second test of nothing.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		raw, err := listener.Accept()
		if err != nil {
			serverErr = err
			return
		}
		defer raw.Close()
		_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
		s := tls.Server(raw, serverCfg)
		serverErr = s.Handshake()
	}()

	raw, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
	c := tls.Client(raw, clientCfg)
	clientErr = c.Handshake()
	raw.Close()
	wg.Wait()
	return serverErr, clientErr
}

// A permissive cluster gets no credentials and no complaint. This is every
// cluster that has not been flipped, so getting it wrong takes an estate off the
// air on upgrade.
func TestAPermissiveClusterGetsNoCredentials(t *testing.T) {
	for _, mode := range []string{"", config.TLSModePermissive, "somethingElse"} {
		cfg := &config.Config{Nodes: map[string]*config.Node{}}
		cfg.Pulse.TLSMode = mode

		got, err := Credentials(snapshotOf(cfg))
		if err != nil {
			t.Errorf("tls_mode %q: %v", mode, err)
		}
		if got != nil {
			t.Errorf("tls_mode %q produced credentials; the wire should stay plaintext", mode)
		}
	}
}

func TestTwoNodesTheConfigNamesCanSpeak(t *testing.T) {
	aDir, aPEM := mintIdentity(t, "node-a")
	bDir, bPEM := mintIdentity(t, "node-b")

	cluster := snapshotOf(requiredConfig(map[string]*config.Node{
		"uuid-a": {Hostname: "node-a", TLSCert: aPEM},
		"uuid-b": {Hostname: "node-b", TLSCert: bPEM},
	}))

	sErr, cErr := handshake(t, credentialsAs(t, aDir, cluster), credentialsAs(t, bDir, cluster))
	if sErr != nil || cErr != nil {
		t.Fatalf("two nodes the config names could not speak: server=%v client=%v", sErr, cErr)
	}
}

// The line the design rests on, at the handshake this time rather than in the
// set: a node holding a well-formed certificate the cluster does not name gets
// nowhere, dialling either way.
//
// The stranger is given a config that names both itself and node-a, which is what
// an attacker who had read a config off a disk would have. It is not what node-a
// has, and node-a's is the one that decides.
func TestAStrangerIsRefusedInBothDirections(t *testing.T) {
	aDir, aPEM := mintIdentity(t, "node-a")
	sDir, sPEM := mintIdentity(t, "stranger")

	cluster := snapshotOf(requiredConfig(map[string]*config.Node{
		"uuid-a": {Hostname: "node-a", TLSCert: aPEM},
	}))
	strangerSees := snapshotOf(requiredConfig(map[string]*config.Node{
		"uuid-a": {Hostname: "node-a", TLSCert: aPEM},
		"uuid-s": {Hostname: "stranger", TLSCert: sPEM},
	}))

	t.Run("stranger dialling in", func(t *testing.T) {
		sErr, cErr := handshake(t,
			credentialsAs(t, aDir, cluster), credentialsAs(t, sDir, strangerSees))
		if sErr == nil && cErr == nil {
			t.Fatal("an unnamed client was accepted by the listener")
		}
	})

	t.Run("stranger dialled to", func(t *testing.T) {
		sErr, cErr := handshake(t,
			credentialsAs(t, sDir, strangerSees), credentialsAs(t, aDir, cluster))
		if sErr == nil && cErr == nil {
			t.Fatal("a client accepted a listener the config does not name")
		}
	})
}

// Removal is revocation, and that is only true if the set is re-read at the
// handshake rather than captured when the listener was built. The same tls.Config
// object must start refusing a peer the instant the config stops naming it.
func TestRemovingANodeFromTheConfigRevokesIt(t *testing.T) {
	aDir, aPEM := mintIdentity(t, "node-a")
	bDir, bPEM := mintIdentity(t, "node-b")

	live := requiredConfig(map[string]*config.Node{
		"uuid-a": {Hostname: "node-a", TLSCert: aPEM},
		"uuid-b": {Hostname: "node-b", TLSCert: bPEM},
	})
	// node-a's listener credentials, built once and never rebuilt below.
	serverCfg := credentialsAs(t, aDir, func() *config.Config { return live })
	clientCfg := credentialsAs(t, bDir, snapshotOf(requiredConfig(map[string]*config.Node{
		"uuid-a": {Hostname: "node-a", TLSCert: aPEM},
		"uuid-b": {Hostname: "node-b", TLSCert: bPEM},
	})))

	if sErr, cErr := handshake(t, serverCfg, clientCfg); sErr != nil || cErr != nil {
		t.Fatalf("node-b was refused while the config still named it: server=%v client=%v", sErr, cErr)
	}

	delete(live.Nodes, "uuid-b")

	if sErr, cErr := handshake(t, serverCfg, clientCfg); sErr == nil && cErr == nil {
		t.Fatal("a removed node was still accepted; the trust set was captured " +
			"when the listener was built rather than read at the handshake")
	}
}

// The other half of the same property: a node that joins after the listener was
// built is accepted without the listener being touched.
func TestANodeThatJoinsAfterwardsIsAccepted(t *testing.T) {
	aDir, aPEM := mintIdentity(t, "node-a")
	bDir, bPEM := mintIdentity(t, "node-b")

	live := requiredConfig(map[string]*config.Node{"uuid-a": {Hostname: "node-a", TLSCert: aPEM}})
	serverCfg := credentialsAs(t, aDir, func() *config.Config { return live })
	clientCfg := credentialsAs(t, bDir, snapshotOf(requiredConfig(map[string]*config.Node{
		"uuid-a": {Hostname: "node-a", TLSCert: aPEM},
		"uuid-b": {Hostname: "node-b", TLSCert: bPEM},
	})))

	if sErr, cErr := handshake(t, serverCfg, clientCfg); sErr == nil && cErr == nil {
		t.Fatal("node-b was accepted before the config named it")
	}

	live.Nodes["uuid-b"] = &config.Node{Hostname: "node-b", TLSCert: bPEM}

	if sErr, cErr := handshake(t, serverCfg, clientCfg); sErr != nil || cErr != nil {
		t.Fatalf("a node that joined after the listener was built was refused: server=%v client=%v",
			sErr, cErr)
	}
}

// A cluster that asks for TLS and cannot produce it must say so, not quietly
// hand back the nil that means plaintext.
func TestRequiredWithNothingToOfferIsAnError(t *testing.T) {
	restore := func(t *testing.T, dir string) {
		t.Helper()
		prev := security.CertDir
		security.CertDir = dir
		t.Cleanup(func() { security.CertDir = prev })
	}

	t.Run("no identity on disk", func(t *testing.T) {
		restore(t, t.TempDir())

		cfg := requiredConfig(map[string]*config.Node{"uuid-a": {Hostname: "node-a", TLSCert: "x"}})
		got, err := Credentials(snapshotOf(cfg))
		if err == nil {
			t.Error("a node with no certificate on disk produced credentials")
		}
		if got != nil {
			t.Error("an error came back with a usable tls.Config beside it")
		}
	})

	t.Run("a config naming no certificates", func(t *testing.T) {
		dir, _ := mintIdentity(t, "node-a")
		restore(t, dir)

		cfg := requiredConfig(map[string]*config.Node{"uuid-a": {Hostname: "node-a"}})
		if _, err := Credentials(snapshotOf(cfg)); err == nil {
			t.Error("a cluster whose config names no certificates produced credentials; " +
				"the listener would have started and refused everybody")
		}
	})

	t.Run("no config at all", func(t *testing.T) {
		if _, err := Credentials(nil); err == nil {
			t.Error("a nil snapshot produced credentials")
		}
		if _, err := Credentials(func() *config.Config { return nil }); err == nil {
			t.Error("a snapshot returning nothing produced credentials")
		}
	})
}
