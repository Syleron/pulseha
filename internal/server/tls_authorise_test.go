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
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/syleron/pulseha/internal/client"
	"github.com/syleron/pulseha/internal/clustertls"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/packages/security"
	"github.com/syleron/pulseha/rpc"
)

// serveWithAuthorisation stands the interceptor up in front of a real TLS
// listener, the way newClusterGRPCServer does, and returns its address.
//
// A real gRPC call rather than calling authorisePeer directly: what is under
// test is that the peer's certificate reaches the interceptor through the
// connection at all, and a test that constructed the context itself would pass
// against a listener that had never asked for one.
func serveWithAuthorisation(t *testing.T, s *Server) string {
	t.Helper()

	creds, err := clustertls.ServerCredentials(s.certDir, s.clusterSnapshot())
	if err != nil {
		t.Fatalf("ServerCredentials: %v", err)
	}
	if creds == nil {
		t.Fatal("no server credentials for a cluster that requires TLS")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(creds)),
		grpc.UnaryInterceptor(s.authorisePeer),
	)
	rpc.RegisterServerServer(srv, s)
	rpc.RegisterCLIServer(srv, s)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	return ln.Addr().String()
}

// dialAs opens a client connection using the identity in dir and the cluster
// view in cfg.
func dialAs(t *testing.T, dir string, cfg *config.Config, addr string) *client.Client {
	t.Helper()

	creds, err := clustertls.ClientCredentials(dir, func() *config.Config { return cfg })
	if err != nil {
		t.Fatalf("ClientCredentials: %v", err)
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	c, err := client.New()
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	if err := c.Connect(host, port, creds); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// authorisationTestServer is a one-node cluster that requires TLS, plus a second
// identity that the config does not name.
func authorisationTestServer(t *testing.T) (s *Server, strangerDir string, strangerCfg *config.Config) {
	t.Helper()

	s = newPropagationTestServer(t)
	localPEM := installNodeIdentity(t, "local-node")
	s.config.Pulse.TLSMode = config.TLSModeRequired
	s.config.Nodes[s.config.Pulse.LocalNode].TLSCert = localPEM
	if err := s.config.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	s.memberList.UpdateConfig(s.config)

	// Minted into its own directory rather than by moving security.CertDir, which
	// nothing in production moves and which other goroutines here are reading.
	strangerDir = t.TempDir()
	strangerPEM := mintIdentityInto(t, strangerDir, "stranger")

	// What the stranger believes: a cluster containing the target and itself. It
	// has to name the target or it would refuse to dial at all, and the question
	// here is what the target does about a caller it has not named.
	strangerCfg = &config.Config{Nodes: map[string]*config.Node{
		s.config.Pulse.LocalNode: {Hostname: "local-node", TLSCert: localPEM},
		"uuid-stranger":          {Hostname: "stranger", TLSCert: strangerPEM},
	}}
	strangerCfg.Pulse.TLSMode = config.TLSModeRequired

	return s, strangerDir, strangerCfg
}

// The rule the allowlist exists for, enforced on inbound traffic: a certificate
// the config does not name may ask to join and may do nothing else.
//
// The handshake cannot make this call, which is why the refusal lives here —
// a listener that rejected an unnamed certificate outright would make a cluster
// that requires TLS one that nobody could ever join (ADR-0005's bootstrap).
func TestAnUnnamedPeerMayOnlyJoin(t *testing.T) {
	s, strangerDir, strangerCfg := authorisationTestServer(t)
	addr := serveWithAuthorisation(t, s)
	c := dialAs(t, strangerDir, strangerCfg, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Join gets through the interceptor. It is refused further in, on the token,
	// which is the guard that belongs to it — what matters here is that it was
	// the handler that refused and not the interceptor.
	resp, err := c.CLI().Join(ctx, &rpc.JoinRequest{Hostname: "stranger", Token: "wrong"})
	if status.Code(err) == codes.PermissionDenied {
		t.Fatalf("a node asking to join a cluster that requires TLS was refused for not "+
			"being in it yet; there is then no way into such a cluster at all: %v", err)
	}
	if err == nil && resp != nil && resp.Success {
		t.Fatal("a join with a wrong token succeeded")
	}

	// Everything else does not.
	if _, err := c.Server().HealthCheck(ctx, &rpc.HealthCheckRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("HealthCheck from an unnamed certificate: %v, want PermissionDenied; the "+
			"bootstrap opens one door and must not open the building", err)
	}
	if _, err := c.Server().ConfigSync(ctx, &rpc.ConfigSyncRequest{Config: []byte("{}")}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("ConfigSync from an unnamed certificate: %v, want PermissionDenied", err)
	}
}

// A peer the config does name is not stopped by any of this.
func TestANamedPeerIsNotStoppedByTheInterceptor(t *testing.T) {
	s, _, _ := authorisationTestServer(t)
	addr := serveWithAuthorisation(t, s)

	// Dialling with this node's own identity, which the config names.
	c := dialAs(t, security.CertDir, s.config, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := c.Server().HealthCheck(ctx, &rpc.HealthCheckRequest{}); status.Code(err) == codes.PermissionDenied {
		t.Errorf("a peer the config names was refused: %v", err)
	}
}

// Removal is revocation on the inbound side too, and only if the trust set is
// read per call rather than captured when the listener was built.
func TestRemovingAPeerStopsItsRPCsAtTheListener(t *testing.T) {
	s, strangerDir, strangerCfg := authorisationTestServer(t)

	// Start with the stranger named, so it is an ordinary member.
	s.config.Nodes["uuid-stranger"] = strangerCfg.Nodes["uuid-stranger"]
	s.memberList.UpdateConfig(s.config)

	addr := serveWithAuthorisation(t, s)
	c := dialAs(t, strangerDir, strangerCfg, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := c.Server().HealthCheck(ctx, &rpc.HealthCheckRequest{}); status.Code(err) == codes.PermissionDenied {
		t.Fatalf("a named peer was refused before it was removed: %v", err)
	}

	s.Lock()
	delete(s.config.Nodes, "uuid-stranger")
	s.Unlock()
	s.memberList.UpdateConfig(s.config)

	if _, err := c.Server().HealthCheck(ctx, &rpc.HealthCheckRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a removed peer's RPC was still served (%v); removing a node from the "+
			"config is how this cluster revokes, and it only works if the trust set is "+
			"read per call", err)
	}
}
