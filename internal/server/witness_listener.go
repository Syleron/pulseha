package server

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/syleron/pulseha/internal/client"
	"github.com/syleron/pulseha/internal/clustertls"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/rpc"
)

// The Witness listener: a second listener, on the node's witness_address, that
// serves one RPC -- WitnessReport -- over TLS whatever tls_mode says, to a
// certificate the cluster names and only as the node that certificate belongs
// to (ADR-0006).
//
// Separate from the cluster listener rather than a second address on it,
// because the two answer different questions over different networks. The
// cluster listener carries everything on the heartbeat path under the cluster's
// own terms; this one carries the one question that must survive that path
// breaking, on terms no migration phase relaxes.

// reconcileWitnessListener brings the listener in line with this node's
// witness_address: started, moved or stopped. Idempotent, and cheap when nothing
// changed, so the witness loop calls it every tick -- which is also how a
// listener that could not start, because certificates were not exchanged yet,
// starts once they are.
func (s *Server) reconcileWitnessListener() {
	cfg := s.clusterSnapshot()()
	want := ""
	if cfg != nil {
		cfg.Lock()
		if node := cfg.Nodes[cfg.Pulse.LocalNode]; node != nil {
			want = strings.TrimSpace(node.WitnessAddress)
		}
		cfg.Unlock()
	}

	s.witnessListenMu.Lock()
	defer s.witnessListenMu.Unlock()
	if want == s.witnessListenAddr && (want == "" || s.witnessListenSrv != nil) {
		return
	}
	if s.witnessListenSrv != nil {
		s.witnessListenSrv.Stop()
		s.witnessListenSrv = nil
		s.logger.Info("WITNESS: stopped the Witness listener", "address", s.witnessListenAddr)
	}
	s.witnessListenAddr = want
	if want == "" {
		return
	}

	creds, err := clustertls.WitnessServerCredentials(s.certDir, s.clusterSnapshot())
	if err != nil {
		s.noteWitnessListenerProblem(want, fmt.Errorf("no credentials to serve it with: %w", err))
		return
	}
	ln, err := net.Listen("tcp", want)
	if err != nil {
		s.noteWitnessListenerProblem(want, err)
		return
	}
	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(creds)),
		grpc.UnaryInterceptor(s.authoriseWitnessPath),
	)
	rpc.RegisterServerServer(srv, s)
	s.witnessListenSrv = srv
	s.witnessListenProblem = ""
	go func() { _ = srv.Serve(ln) }()
	s.logger.Info("WITNESS: serving the Witness path", "address", want)
}

// noteWitnessListenerProblem logs why the listener is not up, once per distinct
// reason rather than on every tick it is retried.
func (s *Server) noteWitnessListenerProblem(addr string, err error) {
	if msg := err.Error(); msg != s.witnessListenProblem {
		s.witnessListenProblem = msg
		s.logger.Warn("WITNESS: the Witness listener is not up; arbitration falls back to a silent Witness",
			"address", addr, "error", err)
	}
}

func (s *Server) stopWitnessListener() {
	s.witnessListenMu.Lock()
	defer s.witnessListenMu.Unlock()
	if s.witnessListenSrv != nil {
		s.witnessListenSrv.Stop()
		s.witnessListenSrv = nil
	}
	s.witnessListenAddr = ""
}

// authoriseWitnessPath is the Witness listener's only gate, and it is narrower
// than the cluster listener's: one method, a certificate the cluster names, and
// the request's node_id must be the node that certificate belongs to -- a
// member asking under another member's name is refused.
func (s *Server) authoriseWitnessPath(ctx context.Context, req interface{},
	info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {

	if info.FullMethod != rpc.Server_WitnessReport_FullMethodName {
		return nil, status.Error(codes.PermissionDenied, "the Witness path serves WitnessReport only")
	}
	cfg := s.clusterSnapshot()()
	if cfg == nil {
		return nil, status.Error(codes.Internal, "no cluster config to authorise against")
	}
	set, err := clustertls.NewTrustSet(cfg.Nodes)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "cluster trust set unusable: %v", err)
	}
	id, err := set.VerifyPeer(peerCertificates(ctx))
	if err != nil {
		return nil, status.Errorf(codes.PermissionDenied, "%v", err)
	}
	if asker := req.(*rpc.WitnessReportRequest).GetNodeId(); asker != id {
		return nil, status.Errorf(codes.PermissionDenied, "certificate belongs to %s, request names %s", id, asker)
	}
	return handler(ctx, req)
}

// witnessPathClient is how this node reaches id on the Witness path: on its
// witness_address, over TLS verified to be id itself, when it declares one; on
// its bind_address under the cluster's own terms when it does not, which is the
// heartbeat path and the weaker topology (ADR-0006).
//
// A declared address is never fallen back from. If it cannot be reached, or its
// certificate does not check out, the answer is that the Witness path is
// unavailable -- a silent Witness -- not a quiet retreat to a path an attacker on
// the service network does not need to be able to forge.
func (s *Server) witnessPathClient(id string, node config.Node) (*client.Client, error) {
	addr := strings.TrimSpace(node.WitnessAddress)
	if addr == "" {
		return s.getPeerClient(id, &node)
	}
	key := id + "|" + addr

	s.witnessClientsMu.Lock()
	defer s.witnessClientsMu.Unlock()
	if c := s.witnessClients[key]; c != nil && c.Connection != nil {
		return c, nil
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("witness_address %q for %s: %w", addr, id, err)
	}
	creds, err := clustertls.WitnessClientCredentials(s.certDir, s.clusterSnapshot(), id)
	if err != nil {
		return nil, fmt.Errorf("no credentials for the Witness path: %w", err)
	}
	c, err := client.New()
	if err != nil {
		return nil, err
	}
	if err := c.Connect(host, port, creds); err != nil {
		c.Close()
		return nil, err
	}
	if s.witnessClients == nil {
		s.witnessClients = make(map[string]*client.Client)
	}
	s.witnessClients[key] = c
	return c, nil
}

// validWitnessAddress checks a witness_address before it is written: empty to
// clear it, or host:port with a usable port.
func validWitnessAddress(addr string) error {
	if addr == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("witness_address must be host:port: %w", err)
	}
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("witness_address %q has no host", addr)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("witness_address %q has no usable port", addr)
	}
	return nil
}

// applyWitnessAddress sets this node's witness_address, publishes it, and moves
// the listener. Node-owned, so it is only ever set here, on the node it
// describes; the broadcast is what tells the rest of the cluster where to dial.
func (s *Server) applyWitnessAddress(value string) *rpc.UpdateConfigResponse {
	value = strings.TrimSpace(value)
	if err := validWitnessAddress(value); err != nil {
		return &rpc.UpdateConfigResponse{Success: false, Message: err.Error()}
	}

	s.Lock()
	localID, err := s.config.GetLocalNodeUUID()
	if err != nil {
		s.Unlock()
		return &rpc.UpdateConfigResponse{Success: false, Message: "not in a cluster: " + err.Error()}
	}
	node := s.config.Nodes[localID]
	if node == nil {
		s.Unlock()
		return &rpc.UpdateConfigResponse{Success: false, Message: "this node has no entry in the cluster config"}
	}
	node.WitnessAddress = value
	if err := s.config.Save(); err != nil {
		s.Unlock()
		return &rpc.UpdateConfigResponse{Success: false, Message: "failed to save config: " + err.Error()}
	}
	s.markConfigDirty()
	s.Unlock()

	s.reconcileWitnessListener()
	if value == "" {
		return &rpc.UpdateConfigResponse{Success: true,
			Message: "cleared on this node and published; the Witness path to it falls back to bind_address"}
	}
	return &rpc.UpdateConfigResponse{Success: true,
		Message: "applied to this node and published to the cluster; set it on every node, the Witness included"}
}

// witnessPathTrusted reports whether every leg of the Witness path is verified
// TLS: every member has published its certificate, and each is reached either on
// a witness_address, which is always TLS, or on a cluster link that requires it.
// Pure, over the config's node entries.
func witnessPathTrusted(nodes map[string]*config.Node, tlsRequired bool) bool {
	hasWitness := false
	for _, node := range nodes {
		if node == nil {
			continue
		}
		hasWitness = hasWitness || node.IsWitness()
		if strings.TrimSpace(node.TLSCert) == "" {
			return false
		}
		if strings.TrimSpace(node.WitnessAddress) == "" && !tlsRequired {
			return false
		}
	}
	return hasWitness
}
