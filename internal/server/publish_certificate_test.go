package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/packages/security"
	"github.com/syleron/pulseha/rpc"
)

// withCertFile points security.CertDir at a temp dir holding the given PEM, and
// returns the dir.
func withCertFile(t *testing.T, pem string) string {
	t.Helper()

	dir := t.TempDir()
	prev := security.CertDir
	security.CertDir = dir
	t.Cleanup(func() { security.CertDir = prev })

	if pem != "" {
		if err := os.WriteFile(filepath.Join(dir, "pulseha.crt"), []byte(pem), 0644); err != nil {
			t.Fatalf("write cert: %v", err)
		}
	}
	return dir
}

func localNode(t *testing.T, s *Server) *config.Node {
	t.Helper()

	s.RLock()
	defer s.RUnlock()
	return s.config.Nodes[removeIPLocal]
}

// The permissive phase of #111: a node publishes its certificate into its own
// config entry, and the ordinary ConfigSync path carries it to everyone else.
func TestTheLocalCertificateIsPublished(t *testing.T) {
	withCertFile(t, "-----BEGIN CERTIFICATE-----\nnode-a\n-----END CERTIFICATE-----\n")
	s := newRemoveIPTestServer(t)

	s.PublishLocalCertificate()

	if got := localNode(t, s).TLSCert; got == "" {
		t.Fatal("nothing published; the trust set can never accumulate")
	} else if got != "-----BEGIN CERTIFICATE-----\nnode-a\n-----END CERTIFICATE-----" {
		t.Errorf("published %q, want the file's contents trimmed", got)
	}
}

// Publishing is a config save and a cluster-wide broadcast, so an unconditional
// write would put one of each on every restart of every node. After the first,
// this must do nothing at all.
func TestRepublishingAnUnchangedCertificateIsANoOp(t *testing.T) {
	withCertFile(t, "-----BEGIN CERTIFICATE-----\nnode-a\n-----END CERTIFICATE-----\n")
	s := newRemoveIPTestServer(t)

	s.PublishLocalCertificate()
	before := s.loadConfigStamp()

	for range 3 {
		s.PublishLocalCertificate()
	}
	// The stamp is bumped by markConfigDirty, which is the broadcast. Unchanged
	// means the republish neither saved nor broadcast.
	if after := s.loadConfigStamp(); after.version != before.version {
		t.Errorf("config version %d -> %d; republishing wrote and broadcast",
			before.version, after.version)
	}
}

// A regenerated certificate has to reach the cluster, or the node presents one
// identity while the trust set names another.
func TestAChangedCertificateIsRepublished(t *testing.T) {
	dir := withCertFile(t, "-----BEGIN CERTIFICATE-----\nold\n-----END CERTIFICATE-----\n")
	s := newRemoveIPTestServer(t)
	s.PublishLocalCertificate()

	if err := os.WriteFile(filepath.Join(dir, "pulseha.crt"),
		[]byte("-----BEGIN CERTIFICATE-----\nnew\n-----END CERTIFICATE-----\n"), 0644); err != nil {
		t.Fatalf("rewrite cert: %v", err)
	}
	s.PublishLocalCertificate()

	if got := localNode(t, s).TLSCert; got != "-----BEGIN CERTIFICATE-----\nnew\n-----END CERTIFICATE-----" {
		t.Errorf("published %q, want the new certificate", got)
	}
}

// A node with no certificate has simply not published, which the permissive
// phase tolerates by design. It must not be an error and must not write.
func TestNoCertificateFileIsNotAnError(t *testing.T) {
	withCertFile(t, "")
	s := newRemoveIPTestServer(t)

	s.PublishLocalCertificate()

	if got := localNode(t, s).TLSCert; got != "" {
		t.Errorf("published %q with no certificate on disk", got)
	}
}

func TestCertificateFingerprintIsShortAndStable(t *testing.T) {
	const pem = "-----BEGIN CERTIFICATE-----\nnode-a\n-----END CERTIFICATE-----"

	a := certificateFingerprint(pem)
	if len(a) != 12 {
		t.Errorf("fingerprint %q is %d chars, want 12", a, len(a))
	}
	if a != certificateFingerprint(pem) {
		t.Error("fingerprint is not stable")
	}
	if a == certificateFingerprint(pem+"x") {
		t.Error("two certificates share a fingerprint")
	}
}

// The node-owned distinction (#111, ADR-0005), and the reason the field needed
// one: a sync adopts every other node's certificate and never its own.
//
// Every other field of a node entry is cluster state a peer may correct. A
// certificate is not — nobody but a node can say what its identity is, and a
// peer's copy is only as fresh as the last broadcast it saw. Adopting it lets a
// sync carrying a pre-regeneration copy hand this node back an identity it no
// longer has, after which it presents one certificate while the trust set names
// another.
func TestASyncNeverOverwritesThisNodesOwnCertificate(t *testing.T) {
	withCertFile(t, "-----BEGIN CERTIFICATE-----\nmine\n-----END CERTIFICATE-----\n")
	s := newRemoveIPTestServer(t, startReleasingPeer(t, &releasingPeer{}))
	s.PublishLocalCertificate()

	mine := localNode(t, s).TLSCert
	if mine == "" {
		t.Fatal("fixture published nothing")
	}

	// A peer syncs a config in which this node's certificate is a stale copy and
	// the peer's own is new.
	// Not a copy of *s.config: it embeds a mutex, and go vet rightly objects to
	// copying one. The fields the payload needs are read under the lock instead.
	payload := marshalConfigWithCerts(t, s, map[string]string{
		removeIPLocal: "-----BEGIN CERTIFICATE-----\nstale-copy-of-mine\n-----END CERTIFICATE-----",
		"peer-0":      "-----BEGIN CERTIFICATE-----\ntheirs\n-----END CERTIFICATE-----",
	})

	if _, err := s.ConfigSync(context.Background(), &rpc.ConfigSyncRequest{Config: payload}); err != nil {
		t.Fatalf("ConfigSync: %v", err)
	}

	if got := localNode(t, s).TLSCert; got != mine {
		t.Errorf("own certificate became %q, want it kept as %q", got, mine)
	}
	s.RLock()
	theirs := s.config.Nodes["peer-0"].TLSCert
	s.RUnlock()
	if theirs != "-----BEGIN CERTIFICATE-----\ntheirs\n-----END CERTIFICATE-----" {
		t.Errorf("peer certificate = %q, want it adopted from the sync; if this is empty "+
			"the rule is refusing everything rather than just this node's own", theirs)
	}
}

// marshalConfigWithCerts serialises a config with the given certificates stamped
// onto the named nodes, which is what a peer's ConfigSync payload looks like.
func marshalConfigWithCerts(t *testing.T, s *Server, certs map[string]string) []byte {
	t.Helper()

	s.RLock()
	defer s.RUnlock()

	clone := &config.Config{
		Pulse:   s.config.Pulse,
		Groups:  s.config.Groups,
		Plugins: s.config.Plugins,
		Nodes:   map[string]*config.Node{},
	}
	for id, n := range s.config.Nodes {
		copied := *n
		if cert, ok := certs[id]; ok {
			copied.TLSCert = cert
		}
		clone.Nodes[id] = &copied
	}
	b, err := json.Marshal(clone)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
