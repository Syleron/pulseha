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
	"crypto/tls"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/syleron/pulseha/internal/clustertls"
	"github.com/syleron/pulseha/rpc"
)

// joinMethod is the one RPC an unnamed certificate may call.
//
// It is how a node that is not in the trust set gets into it, so gating it on
// trust-set membership would make a cluster that requires TLS one nobody can
// ever join. What guards it instead is the cluster token, which an operator
// carries out of band and which HandleNodeJoin checks (ADR-0005's bootstrap).
const joinMethod = rpc.CLI_Join_FullMethodName

// authorisePeer refuses an RPC from a certificate the cluster config does not
// name, and is where "a peer is trusted because the config names it" is enforced
// for inbound traffic.
//
// It lives here rather than in the handshake because the handshake does not know
// what is being asked for, and the bootstrap turns on exactly that distinction:
// an unnamed peer must be able to ask to join and must not be able to do anything
// else. ServerCredentials therefore accepts any certificate, and this decides
// what that certificate is allowed to do.
//
// Plaintext connections pass straight through. That is the permissive phase --
// there is no certificate to judge and the cluster has not asked for one -- and
// it is also the local CLI socket, which is a 0600 unix socket and is authorised
// by its file permissions. Only the cluster listener gets this interceptor, but
// the check is written so that adding it somewhere plaintext would not silently
// lock that path out.
func (s *Server) authorisePeer(ctx context.Context, req interface{},
	info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {

	rawCerts := peerCertificates(ctx)
	if rawCerts == nil {
		return handler(ctx, req)
	}

	cfg := s.clusterSnapshot()()
	if cfg == nil {
		s.logger.Error("Refusing a peer because there is no cluster config to judge it against",
			"method", info.FullMethod)
		return nil, status.Error(codes.Internal, "no cluster config to authorise against")
	}
	set, err := clustertls.NewTrustSet(cfg.Nodes)
	if err != nil {
		s.logger.Error("Refusing a peer because the cluster trust set is unusable",
			"method", info.FullMethod, "error", err)
		return nil, status.Errorf(codes.Internal, "cluster trust set unusable: %v", err)
	}

	if id, err := set.VerifyPeer(rawCerts); err == nil {
		s.logger.Debug("Authorised a peer against the cluster trust set",
			"node", id, "method", info.FullMethod)
		return handler(ctx, req)
	} else if info.FullMethod == joinMethod {
		s.logger.Info("Accepting a join from a certificate the cluster does not name yet",
			"reason", err)
		return handler(ctx, req)
	} else {
		s.logger.Warn("Refused an RPC from a certificate the cluster does not name",
			"method", info.FullMethod, "error", err)
		return nil, status.Errorf(codes.PermissionDenied, "%v", err)
	}
}

// peerCertificates returns the certificates the caller presented, or nil when the
// connection is not TLS.
//
// Nil and empty are deliberately different. Nil is "there was no handshake to
// present anything in", which is a plaintext connection and is allowed through;
// an empty slice on a TLS connection would be a peer that presented nothing,
// which RequireAnyClientCert should already have refused and which the trust set
// refuses again.
func peerCertificates(ctx context.Context) [][]byte {
	p, ok := peer.FromContext(ctx)
	if !ok || p == nil {
		return nil
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return nil
	}
	return rawCertificatesOf(info.State)
}

func rawCertificatesOf(state tls.ConnectionState) [][]byte {
	raw := make([][]byte, 0, len(state.PeerCertificates))
	for _, cert := range state.PeerCertificates {
		raw = append(raw, cert.Raw)
	}
	return raw
}
