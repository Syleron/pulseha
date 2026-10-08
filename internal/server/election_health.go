package server

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/syleron/pulseha/internal/client"
	"github.com/syleron/pulseha/internal/clustertls"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/packages/utils"
	"github.com/syleron/pulseha/rpc"
)

// checkElectionIncumbents uses this voter's direct observations, not the
// proposer's gossiped view. All checks share a bounded budget within the accept
// phase. A listening peer whose role cannot be established blocks the vote.
// Unreachability is still not fencing: a truly partitioned incumbent can keep
// serving, so the existing majority availability policy retains that limitation.
func (s *Server) checkElectionIncumbents(ctx context.Context, cfg *config.Config, candidate string) error {
	ctx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
	defer cancel()
	local := s.memberList.GetMemberByID(cfg.Pulse.LocalNode)
	if local == nil || local.GetStatus() == membership.StatusActive {
		return fmt.Errorf("local voter is unavailable or Active")
	}
	results := make(chan error, len(cfg.Nodes))
	count := 0
	for id, node := range cfg.Nodes {
		if id == candidate || node == nil {
			continue
		}
		if id == cfg.Pulse.LocalNode {
			continue
		}
		count++
		go func(id string, node *config.Node) { results <- s.checkElectionPeer(ctx, cfg, id, node) }(id, node)
	}
	var first error
	for i := 0; i < count; i++ {
		if err := <-results; first == nil && err != nil {
			first = err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return first
}

func (s *Server) checkElectionPeer(ctx context.Context, cfg *config.Config, id string, node *config.Node) error {
	m := s.memberList.GetMemberByID(id)
	if m == nil {
		return fmt.Errorf("member %s disappeared during election", id)
	}
	grace := time.Duration(cfg.Pulse.FailOverLimit) * time.Millisecond
	// A bounded TCP probe distinguishes an unreachable peer from a listening but
	// wedged daemon. The latter must never be treated as a released incumbent.
	dialer := net.Dialer{Timeout: 300 * time.Millisecond}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(utils.SanitizeIPv6(node.IP), node.Port))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if m.RecentlyReportedActive(time.Now(), grace) {
			return fmt.Errorf("member %s recently reported Active", id)
		}
		return nil
	}
	_ = conn.Close()
	c, err := client.New()
	if err != nil {
		return err
	}
	defer c.Close()
	creds, err := clustertls.ClientCredentials(s.certDir, func() *config.Config { return cfg })
	if err != nil {
		return err
	}
	if err := c.Connect(node.IP, node.Port, creds); err != nil {
		return err
	}
	reply, err := c.Server().HealthCheck(ctx, &rpc.HealthCheckRequest{NodeId: cfg.Pulse.LocalNode, ClusterToken: cfg.Pulse.ClusterToken})
	if err != nil {
		return fmt.Errorf("cannot confirm reachable member %s's role: %w", id, err)
	}
	if reply == nil || !reply.Success || reply.NodeId != id || !tokensEqual(reply.ClusterToken, cfg.Pulse.ClusterToken) {
		return fmt.Errorf("member %s supplied no identified cluster role", id)
	}
	role := membership.MemberStatus(reply.Status)
	switch role {
	case membership.StatusActive, membership.StatusPassive, membership.StatusMaintenance:
		m.ObserveRole(role, time.Now())
	default:
		return fmt.Errorf("member %s's role remains unknown", id)
	}
	if role == membership.StatusActive {
		return fmt.Errorf("member %s directly reports Active", id)
	}
	return nil
}
