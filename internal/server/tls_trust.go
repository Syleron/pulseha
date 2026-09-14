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
func newTrustSet(nodes map[string]*nodeCertificate) (*trustSet, error) {
	set := &trustSet{byHash: map[string]string{}}
	for id, n := range nodes {
		if n == nil || strings.TrimSpace(n.pem) == "" {
			continue
		}
		der, err := certificateDER(n.pem)
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

// nodeCertificate is the sliver of a config node entry this file needs, so the
// trust logic can be tested without building a whole config.
type nodeCertificate struct{ pem string }

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
