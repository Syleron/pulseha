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

// credentialsAs builds what the node whose identity lives in dir would use,
// given the cluster config it can currently see.
func credentialsAs(t *testing.T, dir string, snapshot Snapshot,
	build func(string, Snapshot) (*tls.Config, error)) *tls.Config {
	t.Helper()

	cfg, err := build(dir, snapshot)
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if cfg == nil {
		t.Fatal("no credentials for a cluster that requires TLS")
	}
	return cfg
}

func serverAs(t *testing.T, dir string, snapshot Snapshot) *tls.Config {
	t.Helper()
	return credentialsAs(t, dir, snapshot, ServerCredentials)
}

func clientAs(t *testing.T, dir string, snapshot Snapshot) *tls.Config {
	t.Helper()
	return credentialsAs(t, dir, snapshot, ClientCredentials)
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

		for name, build := range map[string]func(string, Snapshot) (*tls.Config, error){
			"client": ClientCredentials, "server": ServerCredentials,
		} {
			got, err := build("", snapshotOf(cfg))
			if err != nil {
				t.Errorf("%s, tls_mode %q: %v", name, mode, err)
			}
			if got != nil {
				t.Errorf("%s, tls_mode %q produced credentials; the wire should stay plaintext",
					name, mode)
			}
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

	sErr, cErr := handshake(t, serverAs(t, aDir, cluster), clientAs(t, bDir, cluster))
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

	// Dialling out is where the refusal lives. A node never sends a request to a
	// server the config does not name.
	t.Run("stranger dialled to", func(t *testing.T) {
		sErr, cErr := handshake(t,
			serverAs(t, sDir, strangerSees), clientAs(t, aDir, cluster))
		if sErr == nil && cErr == nil {
			t.Fatal("a client accepted a listener the config does not name")
		}
	})

	// Dialling in, the handshake deliberately succeeds: the listener has to let a
	// node that is not in the trust set get far enough to ask to join it. What it
	// must not do is let that connection be anonymous -- the certificate has to be
	// there for the daemon's interceptor to judge, and TestAnUnnamedPeerMayOnlyJoin
	// in internal/server is where the judging is tested.
	t.Run("stranger dialling in reaches the listener, with a name on it", func(t *testing.T) {
		serverCfg := serverAs(t, aDir, cluster)
		sErr, cErr := handshake(t, serverCfg, clientAs(t, sDir, strangerSees))
		if sErr != nil || cErr != nil {
			t.Fatalf("a node that is not in the trust set could not reach the listener to "+
				"ask to join it: server=%v client=%v", sErr, cErr)
		}
		if serverCfg.ClientAuth != tls.RequireAnyClientCert {
			t.Errorf("ClientAuth = %v, want RequireAnyClientCert; without a certificate on "+
				"the connection the interceptor has nothing to authorise and an "+
				"anonymous caller would reach Join", serverCfg.ClientAuth)
		}
	})
}

// Removal is revocation, and that is only true if the set is re-read at the
// handshake rather than captured when the credentials were built. The same
// tls.Config object must start refusing a peer the instant the config stops
// naming it.
//
// Tested on the dialling side, which is where the trust set is consulted now:
// node-b refuses to talk to node-a from the moment b's own config stops naming
// it. The inbound half of the same property belongs to the daemon's
// authorisation interceptor and is tested there.
func TestRemovingANodeFromTheConfigRevokesIt(t *testing.T) {
	aDir, aPEM := mintIdentity(t, "node-a")
	bDir, bPEM := mintIdentity(t, "node-b")

	// What node-b believes about the cluster, which is what decides who it will
	// talk to. Built into its credentials once and never rebuilt below.
	live := requiredConfig(map[string]*config.Node{
		"uuid-a": {Hostname: "node-a", TLSCert: aPEM},
		"uuid-b": {Hostname: "node-b", TLSCert: bPEM},
	})
	clientCfg := clientAs(t, bDir, func() *config.Config { return live })
	serverCfg := serverAs(t, aDir, snapshotOf(requiredConfig(map[string]*config.Node{
		"uuid-a": {Hostname: "node-a", TLSCert: aPEM},
		"uuid-b": {Hostname: "node-b", TLSCert: bPEM},
	})))

	if sErr, cErr := handshake(t, serverCfg, clientCfg); sErr != nil || cErr != nil {
		t.Fatalf("node-a was refused while node-b's config still named it: server=%v client=%v",
			sErr, cErr)
	}

	delete(live.Nodes, "uuid-a")

	if sErr, cErr := handshake(t, serverCfg, clientCfg); sErr == nil && cErr == nil {
		t.Fatal("a removed node was still talked to; the trust set was captured when the " +
			"credentials were built rather than read at the handshake")
	}
}

// The other half of the same property: a node that joins after the credentials
// were built is talked to without them being rebuilt.
func TestANodeThatJoinsAfterwardsIsAccepted(t *testing.T) {
	aDir, aPEM := mintIdentity(t, "node-a")
	bDir, bPEM := mintIdentity(t, "node-b")

	live := requiredConfig(map[string]*config.Node{"uuid-b": {Hostname: "node-b", TLSCert: bPEM}})
	clientCfg := clientAs(t, bDir, func() *config.Config { return live })
	serverCfg := serverAs(t, aDir, snapshotOf(requiredConfig(map[string]*config.Node{
		"uuid-a": {Hostname: "node-a", TLSCert: aPEM},
		"uuid-b": {Hostname: "node-b", TLSCert: bPEM},
	})))

	if sErr, cErr := handshake(t, serverCfg, clientCfg); sErr == nil && cErr == nil {
		t.Fatal("node-a was talked to before node-b's config named it")
	}

	live.Nodes["uuid-a"] = &config.Node{Hostname: "node-a", TLSCert: aPEM}

	if sErr, cErr := handshake(t, serverCfg, clientCfg); sErr != nil || cErr != nil {
		t.Fatalf("a node that joined after the credentials were built was refused: "+
			"server=%v client=%v", sErr, cErr)
	}
}

// A cluster that asks for TLS and cannot produce it must say so, not quietly
// hand back the nil that means plaintext.
func TestRequiredWithNothingToOfferIsAnError(t *testing.T) {
	t.Run("no identity on disk", func(t *testing.T) {
		dir := t.TempDir()

		cfg := requiredConfig(map[string]*config.Node{"uuid-a": {Hostname: "node-a", TLSCert: "x"}})
		for name, build := range map[string]func(string, Snapshot) (*tls.Config, error){
			"client": ClientCredentials, "server": ServerCredentials,
		} {
			got, err := build(dir, snapshotOf(cfg))
			if err == nil {
				t.Errorf("%s: a node with no certificate on disk produced credentials", name)
			}
			if got != nil {
				t.Errorf("%s: an error came back with a usable tls.Config beside it", name)
			}
		}
	})

	t.Run("a config naming no certificates", func(t *testing.T) {
		dir, _ := mintIdentity(t, "node-a")

		cfg := requiredConfig(map[string]*config.Node{"uuid-a": {Hostname: "node-a"}})
		if _, err := ServerCredentials(dir, snapshotOf(cfg)); err == nil {
			t.Error("a cluster whose config names no certificates produced credentials; " +
				"the listener would have started and refused everybody")
		}
	})

	t.Run("no config at all", func(t *testing.T) {
		if _, err := ClientCredentials("", nil); err == nil {
			t.Error("a nil snapshot produced credentials")
		}
		if _, err := ServerCredentials("", func() *config.Config { return nil }); err == nil {
			t.Error("a snapshot returning nothing produced credentials")
		}
	})
}

// The bootstrap, from the joiner's side. A node that has never spoken to this
// cluster has no trust set, so it checks the far end against the one fingerprint
// an operator carried to it in the join token — and against nothing else.
func TestAJoinerPinsTheCertificateTheTokenNames(t *testing.T) {
	targetDir, targetPEM := mintIdentity(t, "target")
	joinerDir, _ := mintIdentity(t, "joiner")

	pin, err := Fingerprint(targetPEM)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}

	// The target is in a cluster the joiner is not in, which is the whole point:
	// its trust set does not name the joiner.
	serverCfg := serverAs(t, targetDir, snapshotOf(requiredConfig(map[string]*config.Node{
		"uuid-target": {Hostname: "target", TLSCert: targetPEM},
	})))

	t.Run("the certificate the token names", func(t *testing.T) {
		clientCfg, err := PinnedClientCredentials(joinerDir, pin)
		if err != nil {
			t.Fatalf("PinnedClientCredentials: %v", err)
		}
		if sErr, cErr := handshake(t, serverCfg, clientCfg); sErr != nil || cErr != nil {
			t.Fatalf("a joiner holding the right fingerprint could not reach the cluster: "+
				"server=%v client=%v", sErr, cErr)
		}
	})

	t.Run("a different certificate", func(t *testing.T) {
		_, otherPEM := mintIdentity(t, "impostor")
		otherPin, err := Fingerprint(otherPEM)
		if err != nil {
			t.Fatalf("Fingerprint: %v", err)
		}

		clientCfg, err := PinnedClientCredentials(joinerDir, otherPin)
		if err != nil {
			t.Fatalf("PinnedClientCredentials: %v", err)
		}
		if sErr, cErr := handshake(t, serverCfg, clientCfg); sErr == nil && cErr == nil {
			t.Fatal("a joiner talked to a node whose certificate its token does not name; " +
				"that is the trust-on-first-use the pin exists to remove")
		}
	})

	t.Run("a fingerprint that is not one", func(t *testing.T) {
		for _, bad := range []string{"", "abc", pin + "00", strings.ToUpper(pin) + "x"} {
			if _, err := PinnedClientCredentials(joinerDir, bad); err == nil {
				t.Errorf("%q was accepted as a certificate fingerprint", bad)
			}
		}
	})

	// Whitespace and case are how a fingerprint arrives after a human has moved
	// it — copied out of a terminal, pasted into another one.
	t.Run("as an operator would have carried it", func(t *testing.T) {
		clientCfg, err := PinnedClientCredentials(joinerDir, "  "+strings.ToUpper(pin)+"\n")
		if err != nil {
			t.Fatalf("PinnedClientCredentials: %v", err)
		}
		if sErr, cErr := handshake(t, serverCfg, clientCfg); sErr != nil || cErr != nil {
			t.Fatalf("a fingerprint that had been through a copy and paste was rejected: "+
				"server=%v client=%v", sErr, cErr)
		}
	})
}

// The fingerprint is over the DER, so reformatting the PEM the config carries it
// as cannot change a node's identity.
func TestTheFingerprintSurvivesReformattingThePEM(t *testing.T) {
	_, certPEM := mintIdentity(t, "node-a")

	want, err := Fingerprint(certPEM)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	for _, variant := range []string{certPEM + "\n", "\n\t" + certPEM + "  \n", certPEM + "\r\n"} {
		got, err := Fingerprint(variant)
		if err != nil {
			t.Fatalf("Fingerprint: %v", err)
		}
		if got != want {
			t.Errorf("whitespace around the PEM changed the fingerprint: %s vs %s", got, want)
		}
	}

	if _, err := Fingerprint("not a certificate"); err == nil {
		t.Error("something that is not a certificate produced a fingerprint")
	}
}
