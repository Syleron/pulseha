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
	"fmt"

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
	return c.Connect(ip, port, creds)
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

// presentableToken is the join token as an operator carries it, which is the
// stored secret plus this node's certificate fingerprint once the cluster
// requires TLS.
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
func (s *Server) presentableToken(secret string) string {
	cfg := s.clusterSnapshot()()
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
