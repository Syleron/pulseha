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
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/packages/security"
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
	return clustertls.ClientCredentials(s.clusterSnapshot())
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
	creds, err := clustertls.ServerCredentials(s.clusterSnapshot())
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

	fingerprint, err := clustertls.Fingerprint(localCertificatePEM())
	if err != nil {
		// The cluster requires TLS and this node cannot say what its own
		// certificate is, so it cannot hand out a token that would work. Better an
		// unusable token than one that silently drops the pin and produces a
		// plaintext join against a listener that will refuse it anyway.
		s.logger.Error("Cannot add this node's fingerprint to the join token",
			"error", err, "dir", security.CertDir)
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

// ReportStaleIdentity names the one state a cluster on `required` cannot repair
// by itself: this node's certificate on disk is not the one the cluster's config
// says is its.
//
// It happens when EnsureCertificates regenerates — an expiry, the 30-day renewal
// window, a hostname change, a half-written pair, clock skew — and on a permissive
// cluster it is nothing at all, because the publish that follows propagates the
// new one. On a `required` cluster the publish cannot land, and the asymmetry is
// worth stating precisely because it is not obvious:
//
//   - peers can still reach this node. Their certificates are in *its* trust set,
//     so its interceptor authorises them and their config pushes work;
//   - this node cannot reach them. It presents a certificate none of their trust
//     sets name, so their interceptors refuse everything it sends except Join —
//     including the very ConfigSync that would publish its new certificate.
//
// So it cannot announce itself out of the hole, and its own health checks of its
// peers fail while theirs of it succeed. In active-passive that is the shape that
// ends with this node deciding its peer is gone and promoting itself, against a
// peer that is fine and thinks the same of nobody — which is the duplicate-address
// outcome of defects #2 and #26, reached from a new direction.
//
// The way out is a re-join: `Join` is the one RPC an unnamed certificate may call,
// and HandleNodeJoin records the joiner's certificate, so a join with a valid
// token puts this node back in the trust set. That is what the message says to do.
//
// Reporting only. Whether a node in this state should refuse to start, or take
// itself out of promotion, is a behavioural decision that has not been made.
func (s *Server) ReportStaleIdentity() {
	cfg := s.clusterSnapshot()()
	if cfg == nil || !cfg.Pulse.TLSRequired() {
		return
	}
	localID, err := cfg.GetLocalNodeUUID()
	if err != nil {
		return
	}
	node, ok := cfg.Nodes[localID]
	if !ok || node == nil || strings.TrimSpace(node.TLSCert) == "" {
		return
	}

	onDisk := localCertificatePEM()
	if onDisk == "" || onDisk == strings.TrimSpace(node.TLSCert) {
		return
	}

	s.logger.Error("This node's certificate is not the one the cluster knows it by, and the "+
		"cluster requires TLS. Peers will refuse everything this node sends except a join, "+
		"including the config push that would publish the new certificate — so this cannot "+
		"repair itself. Re-join this node with a token from a healthy member.",
		"onDisk", certificateFingerprint(onDisk),
		"clusterExpects", certificateFingerprint(node.TLSCert))
}
