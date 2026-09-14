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

package clustertls

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/packages/security"
)

// Snapshot hands back the config as its owner currently understands it.
//
// Taken as a function rather than a *config.Config because the trust set has to
// be read at the handshake and not at the moment the listener was built. A
// cluster gains and loses nodes while its listener sits there serving -- the
// listener only rebinds when its address moves -- so a set captured at build
// time would refuse a node that joined afterwards, and would keep accepting one
// that had been removed. Removal-is-revocation is the property ADR-0005 chose
// this design for, and it is only true if the set is re-read.
//
// The implementation must be safe to call from a gRPC handshake goroutine and
// must not take a lock that anything holds across network I/O. MemberList.Config
// is exactly this and is what the daemon passes.
type Snapshot func() *config.Config

// Credentials builds the TLS configuration for inter-node connections, or
// (nil, nil) when the cluster has not been flipped to `required` and the wire
// stays plaintext.
//
// One tls.Config for both directions, deliberately. Every field that matters to
// only one end is ignored by the other -- a client ignores ClientAuth, a server
// ignores InsecureSkipVerify -- and VerifyPeerCertificate is consulted by both.
// The symmetry is not a saving, it is the design: two nodes verify each other
// against the same set by the same rule, and there is no direction in which a
// weaker check could be configured by accident.
//
// A nil return is the permissive phase and is not an error; the caller passes it
// straight to a dial or a listener, both of which read nil as plaintext. An error
// means the cluster asked for TLS and it could not be assembled, which callers
// must treat as fatal to the connection rather than falling back -- a fallback
// here would answer "TLS is required" with plaintext.
func Credentials(snapshot Snapshot) (*tls.Config, error) {
	if snapshot == nil {
		return nil, errors.New("no config to read the TLS mode from")
	}
	cfg := snapshot()
	if cfg == nil {
		return nil, errors.New("no config to read the TLS mode from")
	}
	if !cfg.Pulse.TLSRequired() {
		return nil, nil
	}

	// This node's own identity, read once: step 1 of #111 made it stable across
	// restarts, so there is nothing to re-read for.
	keypair, err := tls.LoadX509KeyPair(
		filepath.Join(security.CertDir, "pulseha.crt"),
		filepath.Join(security.CertDir, "pulseha.key"),
	)
	if err != nil {
		return nil, fmt.Errorf("this node's certificate and key could not be loaded from %s: %w",
			security.CertDir, err)
	}

	// Refuse now rather than at the first handshake if the config names nothing
	// usable. The set is still rebuilt per handshake below; this is the early
	// check, so a listener that could never accept anybody fails to start instead
	// of starting and refusing every peer.
	if _, err := NewTrustSet(cfg.Nodes); err != nil {
		return nil, fmt.Errorf("the cluster trust set is unusable: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{keypair},

		// TLS 1.3 only. Both ends of every connection this config serves are this
		// same binary, so there is no old peer to negotiate down for -- the
		// compatibility argument that keeps 1.2 alive elsewhere does not apply,
		// and 1.3 is what Go 1.26's hybrid post-quantum key exchange rides on.
		MinVersion: tls.VersionTLS13,

		// Server side: demand a certificate but do not ask Go to chain it to a
		// pool. RequireAndVerifyClientCert would need a CA to verify against,
		// which is the design this cluster deliberately does not have; it would
		// reject every peer. The verification is VerifyPeerCertificate's, below.
		ClientAuth: tls.RequireAnyClientCert,

		// Client side: the same statement, pointed the other way.
		//
		// This is the InsecureSkipVerify that ADR-0005 says goes and does not come
		// back, and it is here on purpose -- the ADR is about what it meant on its
		// own, which was encryption with nobody checked. What it switches off is
		// the question "did an authority vouch for this name", and this cluster
		// has no authority and does not connect by name. What replaces it is
		// stricter than what it disables: the peer's certificate must be one the
		// config names, byte for byte. Setting this without the callback below is
		// the defect; the two are a pair and must not be separated.
		InsecureSkipVerify: true,

		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			current := snapshot()
			if current == nil {
				return errors.New("no cluster config to verify the peer against")
			}
			set, err := NewTrustSet(current.Nodes)
			if err != nil {
				return fmt.Errorf("cluster trust set unusable: %w", err)
			}
			if _, err := set.VerifyPeer(rawCerts); err != nil {
				return err
			}
			return nil
		},
	}, nil
}
