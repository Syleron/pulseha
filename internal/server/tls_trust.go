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
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
)

// trustSet is the set of certificates the cluster config names, keyed by the
// SHA-256 of the certificate's DER bytes.
//
// Keyed by the certificate itself rather than by node id on purpose. The
// question a handshake asks is "is this certificate one the cluster named", and
// answering it by identity rather than by position means a peer cannot present
// node A's certificate while claiming to be node B and have anything downstream
// depend on which claim was believed.
type trustSet struct {
	byHash map[string]string // hash -> node id, for the log line
}

// newTrustSet reads the certificates out of a config's node entries.
//
// A node with no certificate contributes nothing and is not an error: that is the
// permissive phase, where the set accumulates while nothing depends on it. The
// caller decides whether an incomplete set is acceptable, and for the flip to
// `required` it is not -- see tlsPreconditionsMet.
func newTrustSet(nodes map[string]*config.Node) (*trustSet, error) {
	set := &trustSet{byHash: map[string]string{}}
	for id, n := range nodes {
		if n == nil || strings.TrimSpace(n.TLSCert) == "" {
			continue
		}
		der, err := certificateDER(n.TLSCert)
		if err != nil {
			return nil, fmt.Errorf("node %s: %w", id, err)
		}
		set.byHash[hashDER(der)] = id
	}
	if len(set.byHash) == 0 {
		return nil, errors.New("no certificates in the cluster config")
	}
	return set, nil
}

// verifyPeer reports whether a presented certificate chain is one the cluster
// named.
//
// Exact match on the leaf, not a chain built to a root. The certificates here are
// self-issued per node and published individually, so there is no hierarchy to
// walk and nothing would be gained by inventing one: ADR-0005's rule is that a
// peer is trusted because the config names it, and this is that sentence in code.
// It is also stricter than a CA pool would be -- a CA that signed a second
// certificate would make that one trusted too, and here it would not be.
//
// Written for tls.Config.VerifyPeerCertificate, which hands over raw DER and runs
// after the handshake's own checks. Those are disabled (InsecureSkipVerify),
// because they would ask whether some authority vouched for the name, which is
// not the question.
func (t *trustSet) verifyPeer(rawCerts [][]byte) (string, error) {
	if t == nil || len(t.byHash) == 0 {
		return "", errors.New("no trust set")
	}
	if len(rawCerts) == 0 {
		return "", errors.New("peer presented no certificate")
	}
	// The leaf, which TLS puts first. Anything after it is a chain this design
	// does not use and must not be persuaded by.
	if id, ok := t.byHash[hashDER(rawCerts[0])]; ok {
		return id, nil
	}
	return "", fmt.Errorf("peer certificate %s is not in the cluster trust set",
		hashDER(rawCerts[0])[:12])
}

// members returns the node ids in the set, sorted, for logging.
func (t *trustSet) members() []string {
	ids := make([]string, 0, len(t.byHash))
	for _, id := range t.byHash {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// certificateDER decodes a PEM certificate to its DER bytes.
func certificateDER(pemCert string) ([]byte, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(pemCert)))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("not a PEM certificate")
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return nil, fmt.Errorf("unparseable certificate: %w", err)
	}
	return block.Bytes, nil
}

func hashDER(der []byte) string {
	sum := sha256.Sum256(der)
	return fmt.Sprintf("%x", sum)
}

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
	if _, err := newTrustSet(nodes); err != nil {
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
