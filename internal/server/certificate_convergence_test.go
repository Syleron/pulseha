package server

import (
	"encoding/json"
	"testing"

	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
)

// copyConfig is a deep copy through the config's own JSON form, which is exactly
// what a peer would hold of it.
func copyConfig(t *testing.T, s *Server) *config.Config {
	t.Helper()
	s.RLock()
	data, err := json.Marshal(s.config)
	s.RUnlock()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var cfg config.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return &cfg
}

func localCert(s *Server, id string) string {
	s.RLock()
	defer s.RUnlock()
	if n := s.config.Nodes[id]; n != nil {
		return n.TLSCert
	}
	return ""
}

// certRaceServer is node-b just after a start alongside node-a and node-c: it has
// published its own certificate, which is config version 1 under its own origin.
func certRaceServer(t *testing.T) *Server {
	t.Helper()
	s, _ := newConfigSyncTestServer(t, "node-b", "node-a", "node-c")
	s.Lock()
	s.config.Nodes["node-b"].TLSCert = "CERT-B"
	s.Unlock()
	setStamp(s, 1, "node-b")
	return s
}

// node-c's own publish, from node-c: version 1 too, and the highest origin, so it
// wins the tie on every node. It carries node-c's certificate and not node-b's.
func nodeCsPublish(t *testing.T, s *Server, nodeBCert string) *config.Config {
	t.Helper()
	cfg := copyConfig(t, s)
	cfg.Nodes["node-b"].TLSCert = nodeBCert
	cfg.Nodes["node-c"].TLSCert = "CERT-C"
	return cfg
}

var allActive = map[string]membership.MemberStatus{
	"node-a": membership.StatusActive, "node-b": membership.StatusActive, "node-c": membership.StatusActive,
}

// Regression for GitHub #268. Nodes that start together each publish their
// certificate as config version 1; the equal-version tie-break hands every node
// the highest origin's config; and each loser keeps its own certificate over the
// winner's copy (it is node-owned), so locally it holds both — while the
// winner's config, which is what everyone converges on, holds only the winner's.
// The loser's certificate was a local change nobody published: PublishLocalCertificate
// saw it already recorded and only re-sent the version everyone held. Three nodes
// sat at `held=1 heldOrigin=03` indefinitely, the tls_mode=required flip was
// refused, and the Witness never became trusted.
//
// Keeping a value of this node's own over the copy a winning config carried is a
// change to that config, and must be published as one.
func TestKeepingThisNodesCertificateOverAWinningConfigPublishesIt(t *testing.T) {
	s := certRaceServer(t)

	applyConfigPayload(t, s, nodeCsPublish(t, s, ""), allActive, "node-c", configStamp{version: 1, origin: "node-c"})

	if got := localCert(s, "node-c"); got != "CERT-C" {
		t.Fatalf("node-c's certificate = %q: the winning config was not applied", got)
	}
	if got := localCert(s, "node-b"); got != "CERT-B" {
		t.Fatalf("node-b's own certificate = %q, want it kept", got)
	}
	if got, want := heldStamp(s), (configStamp{version: 2, origin: "node-b"}); got != want {
		t.Errorf("stamp after the sync = %+v, want %+v: node-b kept a certificate the adopted config "+
			"did not carry, and that change was never given a version anyone would take", got, want)
	}
}

// The guard against a ping-pong: an incoming config that already carries this
// node's certificate is adopted as it stands. This is the winner hearing back
// from a loser that has re-published — it must not re-publish in turn.
func TestAWinningConfigThatCarriesThisNodesCertificateIsAdoptedAsItStands(t *testing.T) {
	s := certRaceServer(t)

	applyConfigPayload(t, s, nodeCsPublish(t, s, "CERT-B"), allActive, "node-c", configStamp{version: 1, origin: "node-c"})

	if got, want := heldStamp(s), (configStamp{version: 1, origin: "node-c"}); got != want {
		t.Errorf("stamp = %+v, want the incoming %+v: nothing of this node's differed, so nothing is owed", got, want)
	}
}

// A node that has not published a certificate yet keeps that empty value, as it
// always has — but publishing the emptiness would wipe whatever copy of this
// node's certificate the cluster holds, which is no one's intent. Its own
// PublishLocalCertificate, when it has one, is the publish.
func TestANodeWithNoCertificateYetDoesNotPublishTheEmptyValue(t *testing.T) {
	s := certRaceServer(t)
	s.Lock()
	s.config.Nodes["node-b"].TLSCert = ""
	s.Unlock()

	applyConfigPayload(t, s, nodeCsPublish(t, s, "OLD-CERT-B"), allActive, "node-c", configStamp{version: 1, origin: "node-c"})

	if got, want := heldStamp(s), (configStamp{version: 1, origin: "node-c"}); got != want {
		t.Errorf("stamp = %+v, want the incoming %+v: an empty certificate is not a value to publish", got, want)
	}
}
