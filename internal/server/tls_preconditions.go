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
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/syleron/pulseha/internal/clustertls"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
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
