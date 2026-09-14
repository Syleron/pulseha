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

	set, err := newTrustSet(map[string]*nodeCertificate{
		"node-a": {pem: aPEM},
		"node-b": {pem: bPEM},
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

	set, err := newTrustSet(map[string]*nodeCertificate{"node-a": {pem: aPEM}})
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

	set, _ := newTrustSet(map[string]*nodeCertificate{"node-a": {pem: aPEM}})

	if _, err := set.verifyPeer([][]byte{strangerDER, aDER}); err == nil {
		t.Error("a trusted certificate presented behind an untrusted leaf was accepted")
	}
	if _, err := set.verifyPeer([][]byte{aDER, strangerDER}); err != nil {
		t.Errorf("a trusted leaf with trailing chain was rejected: %v", err)
	}
}

func TestTheEmptyCases(t *testing.T) {
	aPEM, _ := selfSigned(t, "node-a")
	set, _ := newTrustSet(map[string]*nodeCertificate{"node-a": {pem: aPEM}})

	if _, err := set.verifyPeer(nil); err == nil {
		t.Error("a peer presenting nothing was accepted")
	}
	if _, err := (*trustSet)(nil).verifyPeer([][]byte{{1}}); err == nil {
		t.Error("a nil trust set accepted a peer")
	}

	// Nodes that have not published yet contribute nothing, which is the
	// permissive phase; a set with none at all is refused so the flip cannot
	// produce a cluster that trusts everybody or nobody by accident.
	if _, err := newTrustSet(map[string]*nodeCertificate{"node-a": {pem: ""}}); err == nil {
		t.Error("a config naming no certificates produced a usable trust set")
	}
}

func TestAMalformedCertificateIsAnError(t *testing.T) {
	if _, err := newTrustSet(map[string]*nodeCertificate{
		"node-a": {pem: "-----BEGIN CERTIFICATE-----\nnot der\n-----END CERTIFICATE-----"},
	}); err == nil || !strings.Contains(err.Error(), "node-a") {
		t.Errorf("err = %v, want it to name the node whose certificate cannot be read", err)
	}
}
