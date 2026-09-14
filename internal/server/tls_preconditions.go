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
	"errors"
	"fmt"
	"sort"
	"strings"

	"crypto/tls"

	"github.com/syleron/pulseha/internal/client"

	"github.com/syleron/pulseha/internal/clustertls"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/rpc"
)

// tlsPreconditionsMet reports why this cluster must not be flipped to
// `required` yet, or nil when it may be.
//
// The whole safety argument of ADR-0005's migration is in this function. The
// flip is a config change, so it travels the plaintext channel it is about to
// remove: every node has to receive it, and a node that does not receive it
// keeps serving plaintext against peers that have stopped accepting it, goes
// unreachable, and gets failed over. There is no repair path over the network
// once that has happened -- the operator has to walk to the appliance. So the
// check is for unanimity before the flip starts, and it refuses on anything less
// (#103 is the record of what config divergence costs when a node is left
// behind).
//
// Two things are demanded of every node in the config, and they are separate
// failures worth naming separately:
//
//   - a published certificate, or the node cannot be trusted by anyone after the
//     flip, however well it receives the change; and
//   - a member status that is not Unknown, so the broadcast has somewhere to
//     land. Maintenance counts as present: it is a node excluded from failover
//     promotion, not one that has stopped taking config.
//
// Takes the two maps rather than a *Server so the rule can be exercised against
// the shapes that matter -- a fresh node, a node in maintenance, a node whose
// publish has not propagated -- without standing up a cluster to produce them.
func tlsPreconditionsMet(nodes map[string]*config.Node, statuses map[string]membership.MemberStatus) error {
	if len(nodes) == 0 {
		return errors.New("no nodes in the cluster config")
	}

	var noCert, unreachable []string
	for id, n := range nodes {
		if n == nil || strings.TrimSpace(n.TLSCert) == "" {
			noCert = append(noCert, nodeLabel(id, n))
			// Both failures are reported for a node that has both, because an
			// operator chasing one would otherwise fix it and be stopped again by
			// the other.
		}
		status, known := statuses[id]
		if !known || status == membership.StatusUnknown {
			unreachable = append(unreachable, nodeLabel(id, n))
		}
	}
	sort.Strings(noCert)
	sort.Strings(unreachable)

	// Named in this order because it is the order they get fixed in: an
	// unreachable node cannot publish, so its missing certificate is a symptom
	// rather than the thing to chase.
	var reasons []string
	if len(unreachable) > 0 {
		reasons = append(reasons, fmt.Sprintf("not reachable: %s", strings.Join(unreachable, ", ")))
	}
	if len(noCert) > 0 {
		reasons = append(reasons, fmt.Sprintf("no published certificate: %s", strings.Join(noCert, ", ")))
	}
	if len(reasons) > 0 {
		return fmt.Errorf("every node must be reachable and have published a certificate before "+
			"TLS can be required (%s)", strings.Join(reasons, "; "))
	}

	// The trust set is built as the last step rather than trusted to follow from
	// the loop above, because it is the thing the handshake will actually consult
	// and it can still refuse what the loop accepted -- a certificate that is
	// present but unparseable reaches here as a node with a non-empty TLSCert.
	if _, err := clustertls.NewTrustSet(nodes); err != nil {
		return fmt.Errorf("the cluster's certificates do not form a usable trust set: %w", err)
	}
	return nil
}

// nodeLabel names a node the way an operator knows it, falling back to the UUID
// when the entry carries no hostname -- which is the case for a node that has
// only just been added.
func nodeLabel(id string, n *config.Node) string {
	if n != nil && strings.TrimSpace(n.Hostname) != "" {
		return n.Hostname
	}
	return id
}

// applyTLSMode writes tls_mode after checking that the cluster can survive it.
//
// The flip travels the plaintext channel it removes, so the ordering here is the
// safety property and not an implementation detail:
//
//  1. refuse an unknown value, so a typo cannot reach the file;
//  2. refuse `required` unless every node is reachable and published, which is
//     tlsPreconditionsMet and is the one check that matters;
//  3. write and stamp it, so what the peers are handed is the real config;
//  4. push it to every peer over a connection opened explicitly in clear;
//  5. reconfigure this node, which rebinds its listener on the new terms.
//
// Step 5 is last on purpose. This node rebinding first would take its own
// listener off plaintext while every peer was still speaking it, and it would do
// so before the change that tells them otherwise had left the building.
//
// There is a window between the push and the last peer applying it, during
// which a node that has flipped cannot reach one that has not. It is bounded by
// broadcast latency, which is sub-second on the healthy cluster the precondition
// insists on, against a failover that needs fo_limit (10s by default) of
// continuous failure -- so the cluster rides through it. That margin is the
// reason the precondition demands health rather than merely counting
// certificates.
func (s *Server) applyTLSMode(value string) *rpc.UpdateConfigResponse {
	refuse := func(format string, args ...interface{}) *rpc.UpdateConfigResponse {
		return &rpc.UpdateConfigResponse{Success: false, Message: fmt.Sprintf(format, args...)}
	}

	switch value {
	case config.TLSModePermissive, config.TLSModeRequired:
	default:
		return refuse("tls_mode must be %q or %q, not %q",
			config.TLSModePermissive, config.TLSModeRequired, value)
	}

	cfg := s.currentConfig()
	if cfg == nil {
		return refuse("no cluster configuration to change")
	}
	if cfg.Pulse.TLSMode == value {
		return &rpc.UpdateConfigResponse{
			Success: true,
			Message: fmt.Sprintf("cluster is already in tls_mode %s", value),
		}
	}

	if value == config.TLSModeRequired {
		if err := tlsPreconditionsMet(cfg.Nodes, s.memberStatuses()); err != nil {
			return refuse("%v", err)
		}
		// Asked of this node before anything is written, because this node is the
		// one that will have to serve the credentials and the only one that can
		// look at its own disk. A peer's certificate being in the config says
		// nothing about whether this node still holds its own key.
		//
		// Against a stand-in config already flipped, so Credentials answers the
		// question the cluster is about to be in rather than the one it is in.
		// Built field by field because config.Config carries a mutex and must not
		// be copied whole; the node map is shared deliberately, being read and not
		// written.
		probe := &config.Config{Nodes: cfg.Nodes}
		probe.Pulse.TLSMode = config.TLSModeRequired
		if _, err := clustertls.ServerCredentials(func() *config.Config { return probe }); err != nil {
			return refuse("this node cannot serve TLS: %v", err)
		}
	}

	// The terms the cluster is on *now*, captured before the change is written.
	//
	// This is the whole delivery rule in one line, and getting it as "always in
	// clear" was wrong in the direction nobody tests: going back to `permissive`,
	// the peers are still serving TLS, so a plaintext push is refused by every one
	// of them, and this node then reconfigures to plaintext and can never reach
	// them again. Each half of the pair would be talking a protocol the other had
	// stopped accepting, in both directions, permanently.
	//
	// Building it from the pre-change config makes both directions the same
	// sentence: deliver a change on the terms in force before it. Going to
	// `required` that is nil, which is plaintext, which is what ADR-0005 means by
	// delivering the flip over the channel it removes.
	deliverWith, err := clustertls.ClientCredentials(s.clusterSnapshot())
	if err != nil {
		// Refused rather than changed unilaterally, in both directions. This node
		// cannot reach its peers on the terms they are on, so a change made here
		// would be a change only here -- and a cluster split across tls_mode is
		// exactly what this function exists to prevent.
		//
		// Coming back to permissive that can feel like the wrong call, since
		// plaintext is where the operator is trying to get to. It is not: the peers
		// are still on `required` and cannot be told, so this node would arrive
		// there alone. The way out of that state is the console, per
		// docs/RUNBOOK-111-verify.md -- on every node, not one.
		return refuse("cannot reach the peers on the terms the cluster is currently on, so "+
			"tls_mode has been left at %q (%v); if the cluster is stuck, set tls_mode on "+
			"each node's config.json from its console and restart them one at a time",
			cfg.Pulse.TLSMode, err)
	}

	s.Lock()
	if err := s.config.UpdateValue("tls_mode", value); err != nil {
		s.Unlock()
		s.logger.Error("Failed to update tls_mode", "error", err)
		return refuse("%v", err)
	}
	s.Unlock()
	s.logger.Info("Cluster TLS mode changed", "tls_mode", value)
	// Stamps a new config version and wakes the broadcaster, which is what keeps
	// retrying at a peer the push below could not reach. Its own dials are on the
	// new terms, so they fail against a peer that has not flipped yet and succeed
	// once it has -- which is the right way round: the broadcaster repairs a peer
	// that took the change and lost it, and the push is what delivers it first.
	s.markConfigDirty()

	// Hand it to the peers on the old terms, before this node's own listener and
	// dials move onto the new ones.
	//
	// Explicitly on the captured credentials rather than through dialPeer, and
	// this is the sentence the whole ordering turns on: the config has just been
	// written, so dialPeer would offer the *new* terms to peers that are all still
	// on the old ones, and every one of them would refuse -- the change would
	// never reach the nodes it is about to cut off.
	undelivered := s.deliverConfigWith(deliverWith)

	// Now this node, last. Its listener rebinds on the new terms and its dials
	// start offering them, which is what the peers above have just been told to
	// do for themselves.
	if err := s.Reconfigure(); err != nil {
		s.logger.Error("Failed to apply the new TLS mode locally", "error", err)
		return refuse("tls_mode %s was recorded and sent to the peers but this node could not "+
			"apply it: %v", value, err)
	}

	if len(undelivered) > 0 {
		// Reported rather than rolled back, and the asymmetry is deliberate. This
		// node and the peers that took it are consistent; a peer that did not is
		// isolated and needs an operator, which is ADR-0005's accepted consequence
		// and the reason the precondition demands unanimity beforehand. A revert
		// would have to reach the peers that already flipped, over a wire they
		// have stopped accepting in clear, so it would replace one divergence with
		// a worse one.
		sort.Strings(undelivered)
		s.logger.Error("Some nodes were not told about the TLS mode change",
			"tls_mode", value, "nodes", strings.Join(undelivered, ", "))
		return &rpc.UpdateConfigResponse{
			Success: true,
			Message: fmt.Sprintf("%s, except %s — %s did not accept the change and will be "+
				"unreachable until it does; check it before relying on this cluster",
				configScopeClusterMessage, strings.Join(undelivered, ", "),
				pluralNodes(len(undelivered))),
		}
	}

	return &rpc.UpdateConfigResponse{Success: true, Message: configScopeClusterMessage}
}

// memberStatuses is what every node in the member list currently claims about
// itself, keyed the way the config's node map is.
func (s *Server) memberStatuses() map[string]membership.MemberStatus {
	statuses := map[string]membership.MemberStatus{}
	if s.memberList == nil {
		return statuses
	}
	for id, m := range s.memberList.MembersSnapshot() {
		if m == nil {
			continue
		}
		statuses[id] = m.GetStatus()
	}
	return statuses
}

// deliverConfigWith pushes this node's current config to every peer over a
// connection opened for the purpose on the given terms, and names the ones that
// did not take it. Nil credentials are plaintext.
//
// Only the TLS flip has any business calling this. Every other broadcast goes
// through the broadcaster, which dials on whatever terms the cluster is on; this
// exists for the one change that alters those terms, where the config on this
// node no longer describes the wire the peers are still listening on.
//
// The connections are opened and closed here rather than taken from the pool.
// The pool is keyed by peer and is about to be refilled with TLS connections by
// Reconfigure, and leaving a plaintext entry in it would outlive the moment it
// was correct for.
func (s *Server) deliverConfigWith(creds *tls.Config) []string {
	// Read before the server lock is taken, not from inside it. Asking the member
	// list for the statuses takes its lock and then each member's, and holding the
	// server's write lock across a call into another subsystem is the shape that
	// deadlocked startup once already (internal/membership's
	// TestIsRunningDoesNotBlockOnAPassInFlight). broadcastConfigAndStates is
	// handed its states from outside for the same reason.
	statuses := s.memberStatuses()

	s.Lock()
	localID, _ := s.config.GetLocalNodeUUID()
	payload, buildErr := buildFullConfigPayload(
		s.config, statuses, s.clusterEpoch, s.leaderID, localID, s.loadConfigStamp())
	peers := make(map[string]*config.Node, len(s.config.Nodes))
	for id, node := range s.config.Nodes {
		if id != localID && node != nil {
			peers[id] = node
		}
	}
	s.Unlock()

	if buildErr != nil {
		s.logger.Error("Failed to build the config payload for the TLS mode change", "error", buildErr)
		undelivered := make([]string, 0, len(peers))
		for id, node := range peers {
			undelivered = append(undelivered, nodeLabel(id, node))
		}
		return undelivered
	}

	var undelivered []string
	for id, node := range peers {
		c, err := client.New()
		if err != nil {
			undelivered = append(undelivered, nodeLabel(id, node))
			continue
		}
		if err := c.Connect(node.IP, node.Port, creds); err != nil {
			s.logger.Error("Could not reach a peer to tell it about the TLS mode change",
				"node", nodeLabel(id, node), "error", err)
			undelivered = append(undelivered, nodeLabel(id, node))
			c.Close()
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), configSyncTimeoutFor(len(payload)))
		resp, err := c.Server().ConfigSync(ctx, &rpc.ConfigSyncRequest{Config: payload})
		cancel()
		c.Close()
		if err != nil || resp == nil || !resp.Success {
			s.logger.Error("A peer did not accept the TLS mode change",
				"node", nodeLabel(id, node), "error", err)
			undelivered = append(undelivered, nodeLabel(id, node))
		}
	}
	return undelivered
}

func pluralNodes(n int) string {
	if n == 1 {
		return "that node"
	}
	return "those nodes"
}
