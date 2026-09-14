package server

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
)

// selfSigned mints a certificate the way a node's own generation does, returning
// its PEM and DER.
func selfSigned(t *testing.T, cn string) (string, []byte) {
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
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), der
}

func TestTheTrustSetAcceptsWhatTheConfigNames(t *testing.T) {
	aPEM, aDER := selfSigned(t, "node-a")
	bPEM, bDER := selfSigned(t, "node-b")

	set, err := newTrustSet(map[string]*config.Node{
		"node-a": {Hostname: "node-a", TLSCert: aPEM},
		"node-b": {Hostname: "node-b", TLSCert: bPEM},
	})
	if err != nil {
		t.Fatalf("newTrustSet: %v", err)
	}

	for id, der := range map[string][]byte{"node-a": aDER, "node-b": bDER} {
		got, err := set.verifyPeer([][]byte{der})
		if err != nil {
			t.Errorf("%s rejected: %v", id, err)
		}
		if got != id {
			t.Errorf("identified %q, want %q", got, id)
		}
	}
}

// The line the whole design rests on: a certificate the config does not name is
// refused, however well-formed it is.
func TestAnUnnamedCertificateIsRefused(t *testing.T) {
	aPEM, _ := selfSigned(t, "node-a")
	_, strangerDER := selfSigned(t, "node-a") // same name, different key

	set, err := newTrustSet(map[string]*config.Node{"node-a": {Hostname: "node-a", TLSCert: aPEM}})
	if err != nil {
		t.Fatalf("newTrustSet: %v", err)
	}

	if _, err := set.verifyPeer([][]byte{strangerDER}); err == nil {
		t.Fatal("a certificate with the right CommonName but the wrong key was accepted; " +
			"the set names certificates, not names")
	}
}

// Only the leaf is consulted. A peer that appends a chain must not be able to
// talk its way in with something further down it.
func TestOnlyTheLeafIsConsidered(t *testing.T) {
	aPEM, aDER := selfSigned(t, "node-a")
	_, strangerDER := selfSigned(t, "stranger")

	set, _ := newTrustSet(map[string]*config.Node{"node-a": {Hostname: "node-a", TLSCert: aPEM}})

	if _, err := set.verifyPeer([][]byte{strangerDER, aDER}); err == nil {
		t.Error("a trusted certificate presented behind an untrusted leaf was accepted")
	}
	if _, err := set.verifyPeer([][]byte{aDER, strangerDER}); err != nil {
		t.Errorf("a trusted leaf with trailing chain was rejected: %v", err)
	}
}

func TestTheEmptyCases(t *testing.T) {
	aPEM, _ := selfSigned(t, "node-a")
	set, _ := newTrustSet(map[string]*config.Node{"node-a": {Hostname: "node-a", TLSCert: aPEM}})

	if _, err := set.verifyPeer(nil); err == nil {
		t.Error("a peer presenting nothing was accepted")
	}
	if _, err := (*trustSet)(nil).verifyPeer([][]byte{{1}}); err == nil {
		t.Error("a nil trust set accepted a peer")
	}

	// Nodes that have not published yet contribute nothing, which is the
	// permissive phase; a set with none at all is refused so the flip cannot
	// produce a cluster that trusts everybody or nobody by accident.
	if _, err := newTrustSet(map[string]*config.Node{"node-a": {Hostname: "node-a"}}); err == nil {
		t.Error("a config naming no certificates produced a usable trust set")
	}
}

func TestAMalformedCertificateIsAnError(t *testing.T) {
	if _, err := newTrustSet(map[string]*config.Node{
		"node-a": {TLSCert: "-----BEGIN CERTIFICATE-----\nnot der\n-----END CERTIFICATE-----"},
	}); err == nil || !strings.Contains(err.Error(), "node-a") {
		t.Errorf("err = %v, want it to name the node whose certificate cannot be read", err)
	}
}

// healthy is the shape of a cluster that may be flipped: every node published,
// every node heard from.
func healthy(t *testing.T) (map[string]*config.Node, map[string]membership.MemberStatus) {
	t.Helper()
	aPEM, _ := selfSigned(t, "node-a")
	bPEM, _ := selfSigned(t, "node-b")
	return map[string]*config.Node{
		"uuid-a": {Hostname: "node-a", TLSCert: aPEM},
		"uuid-b": {Hostname: "node-b", TLSCert: bPEM},
	}, map[string]membership.MemberStatus{
		"uuid-a": membership.StatusActive,
		"uuid-b": membership.StatusPassive,
	}
}

func TestAUnanimousClusterMayBeFlipped(t *testing.T) {
	nodes, statuses := healthy(t)
	if err := tlsPreconditionsMet(nodes, statuses); err != nil {
		t.Fatalf("a cluster where every node is up and published was refused: %v", err)
	}
}

// The flip travels over the plaintext channel it removes, so a node that will
// not receive it must stop it. Each of these is a node that would be severed.
func TestTheFlipIsRefusedWhileAnyNodeWouldBeLeftBehind(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(map[string]*config.Node, map[string]membership.MemberStatus)
		wantNamed  string
		wantReason string
	}{
		{
			name: "a node that has not published",
			mutate: func(n map[string]*config.Node, _ map[string]membership.MemberStatus) {
				n["uuid-b"].TLSCert = ""
			},
			wantNamed:  "node-b",
			wantReason: "no published certificate",
		},
		{
			name: "a node nothing has heard from",
			mutate: func(_ map[string]*config.Node, s map[string]membership.MemberStatus) {
				s["uuid-b"] = membership.StatusUnknown
			},
			wantNamed:  "node-b",
			wantReason: "not reachable",
		},
		{
			name: "a node with no member entry at all",
			mutate: func(_ map[string]*config.Node, s map[string]membership.MemberStatus) {
				delete(s, "uuid-b")
			},
			wantNamed:  "node-b",
			wantReason: "not reachable",
		},
		{
			// Present in the config with a value that is not a certificate. The
			// per-node loop sees a non-empty field and passes it; only building
			// the set catches it, which is why the set is built.
			name: "a node whose certificate cannot be read",
			mutate: func(n map[string]*config.Node, _ map[string]membership.MemberStatus) {
				n["uuid-b"].TLSCert = "-----BEGIN CERTIFICATE-----\nnot der\n-----END CERTIFICATE-----"
			},
			wantNamed:  "uuid-b",
			wantReason: "usable trust set",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nodes, statuses := healthy(t)
			tc.mutate(nodes, statuses)

			err := tlsPreconditionsMet(nodes, statuses)
			if err == nil {
				t.Fatal("the flip was allowed with a node that would be left behind")
			}
			if !strings.Contains(err.Error(), tc.wantReason) {
				t.Errorf("err = %v, want it to give the reason %q", err, tc.wantReason)
			}
			if !strings.Contains(err.Error(), tc.wantNamed) {
				t.Errorf("err = %v, want it to name %s so an operator knows where to go",
					err, tc.wantNamed)
			}
		})
	}
}

// A node in maintenance is excluded from failover promotion, not from config.
// Refusing the flip for one would mean an estate could never enable TLS without
// first taking every node back out of maintenance.
func TestMaintenanceDoesNotBlockTheFlip(t *testing.T) {
	nodes, statuses := healthy(t)
	statuses["uuid-b"] = membership.StatusMaintenance

	if err := tlsPreconditionsMet(nodes, statuses); err != nil {
		t.Fatalf("a node in maintenance blocked the flip: %v", err)
	}
}

// Both failures on one node are reported together: an operator who fixed the
// reachability and was then stopped again by the certificate would have to make
// two trips.
func TestANodeWithBothFailuresIsReportedForBoth(t *testing.T) {
	nodes, statuses := healthy(t)
	nodes["uuid-b"].TLSCert = ""
	statuses["uuid-b"] = membership.StatusUnknown

	err := tlsPreconditionsMet(nodes, statuses)
	if err == nil {
		t.Fatal("the flip was allowed")
	}
	for _, want := range []string{"not reachable", "no published certificate"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to report %q as well", err, want)
		}
	}
}

func TestAnEmptyClusterCannotBeFlipped(t *testing.T) {
	if err := tlsPreconditionsMet(nil, nil); err == nil {
		t.Error("a config with no nodes produced a flippable cluster")
	}
}
