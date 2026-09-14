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
	"crypto/tls"
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/syleron/pulseha/internal/client"
	"github.com/syleron/pulseha/internal/clustertls"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/rpc"
)

// clusterSnapshot is how the TLS machinery reads the config this node is
// currently on.
//
// Through the member list rather than s.config, and the reason is lock ordering
// rather than convenience. This is called from gRPC handshake goroutines, and
// MemberList.Config is a leaf RWMutex held for a pointer read and nothing else,
// where s.Lock() is the daemon's big lock and is held across work that can take
// a while. Reconfigure hands the member list every new config pointer, so the
// two say the same thing.
//
// Falls back to the server's own pointer only when there is no member list,
// which is a Server built as a struct literal -- several tests in this package
// do that, and the join path found the last place that assumed they do not
// (#111 step 3a).
//
// **That fallback takes s.RLock(), so this must never be called from a path that
// already holds the server's write lock.** It is not reentrant, and the read
// would wait behind a write that is its own. The Token RPC did exactly that and
// nothing caught it, because nothing in the suite called Token -- see
// presentableTokenLocked. A caller that holds the lock has the config already and
// should pass it.
func (s *Server) clusterSnapshot() clustertls.Snapshot {
	return func() *config.Config {
		if s.memberList != nil {
			if cfg := s.memberList.Config(); cfg != nil {
				return cfg
			}
		}
		s.RLock()
		defer s.RUnlock()
		return s.config
	}
}

// peerCredentials is the TLS configuration this node offers when it dials a
// peer, or nil while the cluster is permissive and the wire stays plaintext.
func (s *Server) peerCredentials() (*tls.Config, error) {
	return clustertls.ClientCredentials(s.certDir, s.clusterSnapshot())
}

// dialPeer opens c against a peer, over TLS when the cluster requires it.
//
// Every inter-node dial in the daemon goes through here, so there is one place
// that decides and no call site that can be left on the wrong side of the flip.
// Credentials that will not build are returned as a connection failure rather
// than retried in clear: a cluster that says TLS is required must not be reached
// without it, and the caller that treats this as "peer unreachable" is treating
// it correctly.
func (s *Server) dialPeer(c *client.Client, ip, port string) error {
	creds, err := s.peerCredentials()
	if err != nil {
		s.logger.Error("Refusing to connect to peer without the TLS the cluster requires",
			"address", ip+":"+port, "error", err)
		return err
	}
	if err := c.Connect(ip, port, creds); err != nil {
		return err
	}
	// On the daemon's logger, which honours logging_level, because internal/client's
	// does not and a dial nobody can observe cannot be verified on an appliance
	// (#61). Debug because peers are dialled often; the listener's own line is
	// Info and is the one to look for first.
	s.logger.Debug("Connected to peer", "address", ip+":"+port, "tls", creds != nil)
	return nil
}

// newClusterGRPCServer builds the inter-node gRPC server, with the cluster's TLS
// credentials on it when the cluster requires them.
//
// Returns whether it put credentials on, so the caller can record what the
// listener it is about to start is actually serving -- the flip changes the
// listener without moving it, and Reconfigure decides whether to rebind by
// comparing both.
//
// An error here stops the listener from starting, and that is deliberate. A
// cluster whose config says `required` and whose credentials will not assemble
// has two options: serve nothing, or serve plaintext to peers that are about to
// stop accepting it. Serving nothing is the one an operator can diagnose, and it
// fails on this node rather than silently downgrading the cluster.
func (s *Server) newClusterGRPCServer() (srv *grpc.Server, tlsServed bool, err error) {
	creds, err := clustertls.ServerCredentials(s.certDir, s.clusterSnapshot())
	if err != nil {
		return nil, false, fmt.Errorf("cluster TLS credentials could not be built: %w", err)
	}

	var opts []grpc.ServerOption
	if creds != nil {
		// The interceptor goes on with the credentials and only with them. It is
		// what refuses a certificate the config does not name, which the handshake
		// no longer does -- see ServerCredentials for why that had to move.
		opts = append(opts,
			grpc.Creds(credentials.NewTLS(creds)),
			grpc.UnaryInterceptor(s.authorisePeer))
	}

	srv = grpc.NewServer(opts...)
	rpc.RegisterServerServer(srv, s)
	// The CLI service is registered on the cluster listener too, so remote
	// operations like Join reach it.
	rpc.RegisterCLIServer(srv, s)
	return srv, creds != nil, nil
}

// presentableTokenLocked is the join token as an operator carries it, which is
// the stored secret plus this node's certificate fingerprint once the cluster
// requires TLS.
//
// Takes the config rather than reading it, and the xLocked name says why: its
// only caller is the Token RPC, which holds s.Lock() across the whole of itself.
// Reaching for clusterSnapshot here was a deadlock — the member-list read that
// usually satisfies it falls back to s.RLock(), and a read lock taken while this
// goroutine holds the write lock can never be granted. Found by probing the path
// rather than by running it: nothing calls Token in the suite, so nothing would
// have.
//
// The stored token is left a bare secret on purpose. It is the shared value both
// ends compare, it is compared byte for byte, and a cluster mid-upgrade has peers
// that would not know to strip a suffix -- so the fingerprint is added on the way
// out and never written down. That also means it is always this node's own
// fingerprint and always current: it is read from disk at the moment the operator
// asks, rather than recovered from a config entry that a peer may have a stale
// copy of.
//
// Permissive returns the bare secret, because there is no handshake to pin and a
// token that appeared to pin one would claim a guarantee it cannot make -- which
// is the mistake ADR-0005 corrected its own ordering to avoid.
func (s *Server) presentableTokenLocked(cfg *config.Config, secret string) string {
	if cfg == nil || !cfg.Pulse.TLSRequired() {
		return secret
	}

	fingerprint, err := clustertls.Fingerprint(s.localCertificatePEM())
	if err != nil {
		// The cluster requires TLS and this node cannot say what its own
		// certificate is, so it cannot hand out a token that would work. Better an
		// unusable token than one that silently drops the pin and produces a
		// plaintext join against a listener that will refuse it anyway.
		s.logger.Error("Cannot add this node's fingerprint to the join token",
			"error", err, "dir", clustertls.CertDirOr(s.certDir))
		return secret
	}
	return clustertls.FormatJoinToken(secret, fingerprint)
}

// payloadNamesTLSMode reports whether a ConfigSync payload's `pulseha` section
// carries a tls_mode key at all, as distinct from carrying an empty one.
//
// Asked of the raw JSON because the struct cannot answer it: an absent key and
// an empty string both unmarshal to "", and the difference between them is the
// difference between "a peer that does not know about tls_mode re-broadcast this"
// and "a peer told me the cluster is permissive". See the caller.
func payloadNamesTLSMode(raw map[string]json.RawMessage) bool {
	pulse, ok := raw["pulseha"]
	if !ok {
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(pulse, &fields); err != nil {
		return false
	}
	_, named := fields["tls_mode"]
	return named
}

// dropPeerConnectionsOnTermsChange closes every cached peer connection when the
// terms it was dialled on are no longer the cluster's, so the next use re-dials.
//
// A no-op unless the terms actually changed, which is what makes it safe to call
// on every reconfigure: almost every one of those is an ordinary config change,
// and tearing down working connections for one of those is the cost defect #31
// was about.
//
// Both caches, because there are two and they are reached by different code. The
// server's pool carries the config and cluster-state broadcasts; each Member's
// client carries the floating-IP RPCs. Missing either leaves half the cluster's
// traffic on a transport the far end has stopped accepting.
func (s *Server) dropPeerConnectionsOnTermsChange(tlsRequired bool) {
	s.clientMutex.Lock()
	if s.peerClientsTLS == tlsRequired {
		s.clientMutex.Unlock()
		return
	}
	stale := s.peerClients
	s.peerClients = make(map[string]*client.Client)
	s.peerClientsTLS = tlsRequired
	s.clientMutex.Unlock()

	for peerID, c := range stale {
		if c != nil {
			c.Close()
		}
		s.logger.Debug("Dropped a peer connection dialled on the previous terms", "peerID", peerID)
	}
	if s.memberList != nil {
		s.memberList.DropClients()
	}
	s.logger.Info("Dropped cached peer connections after a TLS mode change",
		"pooled", len(stale), "tls", tlsRequired)
}

// EnforceIdentityGuard keeps this node out of failover promotion while the
// cluster knows it by a certificate it no longer holds.
//
// The state: EnsureCertificates regenerated — an expiry, the 30-day renewal
// window, a hostname change, clock skew — and on a `required` cluster the new
// certificate is in nobody's trust set. The failure is asymmetric in a way that
// is not obvious from either side:
//
//   - peers can still reach this node. Their certificates are in *its* trust
//     set, so its interceptor authorises them and their config pushes work;
//   - this node cannot reach them. It presents a certificate none of their trust
//     sets name, so their interceptors refuse everything it sends except Join —
//     including the ConfigSync that would publish the new certificate.
//
// So it cannot announce its way out, and **its health checks of its peers fail
// while theirs of it succeed**. That asymmetry is the danger: this node concludes
// its peers are gone and elects itself, against a cluster that is fine and has
// not missed it. In active-passive that is two nodes holding the same addresses,
// which is #2/#26 reached from a direction neither of them came from.
//
// Maintenance is the lever because it already means exactly this — up, reachable,
// and excluded from promotion (selectBestCandidate skips it and says so). The
// node stays diagnosable rather than being taken off the air, which is why this
// is not "refuse to start".
//
// Three things it deliberately does differently from the operator's maintenance
// path, and each would break it:
//
//   - **It cannot be refused.** setMaintenanceLocal declines when no other node
//     would remain available, which is right for an operator taking a node down
//     and exactly wrong here: refusing would leave this node able to promote
//     itself, which is the one thing being prevented.
//   - **It does not demote over RPC.** That path calls MakePassive on peers
//     first. This node cannot reach peers — that is the condition — so the call
//     would fail and abort the guard.
//   - **It is not persisted.** The condition is re-derived at every start and
//     after every join, so the guard lives exactly as long as the thing it
//     guards against. Writing `maintenance: true` into the config would risk a
//     node stuck in maintenance after a re-join had already fixed it, which is a
//     new failure mode rather than a fix, and the write could not propagate
//     anyway.
//
// What it does not claim: if this node is *already* Active and holding addresses
// when its identity goes stale, it is in a worse state than this guard addresses
// and the addresses are not taken off it here. The realistic moment for this is
// startup, before any promotion has happened.
func (s *Server) EnforceIdentityGuard() {
	onDisk, expected, stale := s.identityIsStale()
	if !stale {
		s.releaseIdentityGuard()
		return
	}

	s.logger.Error("This node's certificate is not the one the cluster knows it by, and the "+
		"cluster requires TLS. Peers will refuse everything this node sends except a join, "+
		"including the config push that would publish the new certificate — so this cannot "+
		"repair itself. Re-join this node with a token from a healthy member.",
		"onDisk", certificateFingerprint(onDisk),
		"clusterExpects", certificateFingerprint(expected))

	localID, err := s.currentConfig().GetLocalNodeUUID()
	if err != nil {
		return
	}
	member := s.memberList.GetMemberByID(localID)
	if member == nil {
		s.logger.Error("Cannot hold this node out of promotion: it is not in its own member list")
		return
	}
	if member.GetStatus() == membership.StatusMaintenance {
		s.identityGuardHeld = true
		return
	}

	member.SetStatus(membership.StatusMaintenance)
	s.identityGuardHeld = true
	s.logger.Error("Holding this node out of failover promotion until its certificate is back " +
		"in the cluster's trust set. It would otherwise fail its own health checks of peers " +
		"that are fine, elect itself, and take addresses a healthy node is already serving.")
}

// releaseIdentityGuard returns the node to passive, but only if this guard is
// what put it in maintenance.
//
// The flag is the whole of it: an operator who deliberately put this node into
// maintenance must not have it undone by a certificate check that happens to be
// satisfied.
func (s *Server) releaseIdentityGuard() {
	if !s.identityGuardHeld {
		return
	}
	s.identityGuardHeld = false

	localID, err := s.currentConfig().GetLocalNodeUUID()
	if err != nil {
		return
	}
	if member := s.memberList.GetMemberByID(localID); member != nil &&
		member.GetStatus() == membership.StatusMaintenance {
		member.SetStatus(membership.StatusPassive)
		s.logger.Info("This node's certificate is in the cluster's trust set again; " +
			"it is eligible for promotion once more")
	}
}

// identityIsStale reports whether the cluster knows this node by a certificate it
// no longer holds, returning both so a caller can name them.
//
// False on a permissive cluster, where a regeneration publishes its way out on
// the next broadcast and there is nothing to guard against; false for a node that
// has never published, which has no identity the cluster knows to differ from.
func (s *Server) identityIsStale() (onDisk, expected string, stale bool) {
	cfg := s.clusterSnapshot()()
	if cfg == nil || !cfg.Pulse.TLSRequired() {
		return "", "", false
	}
	localID, err := cfg.GetLocalNodeUUID()
	if err != nil {
		return "", "", false
	}
	node, ok := cfg.Nodes[localID]
	if !ok || node == nil {
		return "", "", false
	}
	expected = strings.TrimSpace(node.TLSCert)
	if expected == "" {
		return "", "", false
	}
	onDisk = s.localCertificatePEM()
	if onDisk == "" {
		return "", "", false
	}
	return onDisk, expected, onDisk != expected
}

// SetCertDir points this node's TLS identity at a directory other than the
// process-wide one, and tells the member list to use it too.
//
// For a process that runs more than one node, which in practice means the
// integration harness: its nodes are Servers in one process, and a shared
// certificate directory means a shared identity, which makes TLS between them
// untestable — every node would present the same certificate and the trust set
// could not tell them apart. The daemon never calls this and runs on
// security.CertDir.
//
// Call before Start.
func (s *Server) SetCertDir(dir string) {
	s.certDir = dir
	if s.memberList != nil {
		s.memberList.SetCertDir(dir)
	}
}
