package server

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/rpc"
)

// witnessProtocolVersion is reported on every HealthCheckResponse. Per-feature,
// like ipVerificationVersion: members do not exchange a daemon version, so what
// a peer understands is asked of the feature, not inferred from a release.
//
// 1: the Witness status, the role in config, and a Witness never counted.
const witnessProtocolVersion = 1

// witnessJoinRefusal returns why the cluster in nodes cannot take a join by
// nodeID, or "" when it can. Pure, so the rules can be tabled.
//
// The rules follow from a Witness being for a two-node cluster and nothing else
// (ADR-0006):
//   - one Witness per cluster;
//   - no Witness for a cluster of three or more, which has a majority to appeal
//     to and is END-2325's territory;
//   - and from the other side, no third serving node into a cluster that has a
//     Witness, which would turn it into exactly that.
//
// A node rejoining under its own ID is judged against the cluster without its
// old entry, so a rejoin is not refused for conflicting with itself.
func witnessJoinRefusal(nodes map[string]*config.Node, nodeID string, witness bool) string {
	existingWitness := ""
	counted := 0
	for id, node := range nodes {
		if id == nodeID || node == nil {
			continue
		}
		if node.IsWitness() {
			existingWitness = id
		} else {
			counted++
		}
	}
	switch {
	case witness && existingWitness != "":
		return fmt.Sprintf("the cluster already has a Witness (%s); a cluster has at most one", existingWitness)
	case witness && counted >= 3:
		return fmt.Sprintf("a Witness is for a two-node cluster, and this one has %d nodes", counted)
	case !witness && existingWitness != "" && counted >= 2:
		return fmt.Sprintf("this cluster has two nodes and a Witness (%s); remove the Witness before adding a third node",
			existingWitness)
	}
	return ""
}

// confirmPeersUnderstandWitness asks every configured member other than this
// node, and other than skip, which version of the Witness protocol it
// understands, and refuses unless all of them answer at least 1.
//
// The cost of admitting a Witness to a cluster with an older binary in it is the
// one this feature is built to avoid: the older node counts every config entry,
// so to it a pair with a Witness is three, its two-node path switches off, and a
// partition it is on the wrong side of leaves it refusing to promote. A member
// that cannot be asked is refused for the same reason -- a join that cannot
// confirm this is not safe to make.
func (s *Server) confirmPeersUnderstandWitness(ctx context.Context, skip string) error {
	cfg := s.currentConfig()
	localID, _ := cfg.GetLocalNodeUUID()
	token := cfg.Pulse.ClusterToken

	cfg.Lock()
	peers := make(map[string]*config.Node, len(cfg.Nodes))
	for id, node := range cfg.Nodes {
		if id != localID && id != skip && node != nil {
			peers[id] = node
		}
	}
	cfg.Unlock()

	ids := make([]string, 0, len(peers))
	for id := range peers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		peer, err := s.getPeerClient(id, peers[id])
		if err != nil {
			return fmt.Errorf("cannot confirm %s understands a Witness: %w", id, err)
		}
		hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		resp, err := peer.Server().HealthCheck(hctx, &rpc.HealthCheckRequest{NodeId: localID, ClusterToken: token})
		cancel()
		if err != nil || resp == nil || !resp.Success {
			msg := "no answer"
			if err != nil {
				msg = err.Error()
			} else if resp != nil {
				msg = resp.Message
			}
			return fmt.Errorf("cannot confirm %s understands a Witness: %s", id, msg)
		}
		if resp.WitnessProtocolVersion < witnessProtocolVersion {
			return fmt.Errorf("%s runs a PulseHA build that predates the Witness; upgrade every member before adding one", id)
		}
	}
	return nil
}
