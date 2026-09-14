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
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

// ClientCredentials builds what this node offers when it dials a peer, or
// (nil, nil) while the cluster is permissive and the wire stays plaintext.
//
// The dialling end verifies: the peer's certificate must be one the config
// names, byte for byte, or the connection does not happen. That is the whole of
// ADR-0005's rule, and it is checked here rather than deferred to anything
// later because a client that has already sent a request to the wrong server has
// already lost.
//
// A nil return is the permissive phase and is not an error; the caller passes it
// straight to a dial, which reads nil as plaintext. An error means the cluster
// asked for TLS and it could not be assembled, which callers must treat as fatal
// to the connection rather than falling back -- a fallback here would answer
// "TLS is required" with plaintext.
func ClientCredentials(certDir string, snapshot Snapshot) (*tls.Config, error) {
	cfg, keypair, err := identityFor(certDir, snapshot)
	if err != nil || cfg == nil {
		return nil, err
	}

	return &tls.Config{
		Certificates: []tls.Certificate{*keypair},
		MinVersion:   tls.VersionTLS13,

		// This is the InsecureSkipVerify that ADR-0005 says goes and does not come
		// back, and it is here on purpose -- the ADR is about what it meant on its
		// own, which was encryption with nobody checked. What it switches off is
		// the question "did an authority vouch for this name", and this cluster
		// has no authority and does not dial by name. What replaces it is stricter
		// than what it disables: the peer's certificate must be one the config
		// names. Setting this without the callback below is the defect; the two
		// are a pair and must not be separated.
		InsecureSkipVerify: true,

		VerifyPeerCertificate: verifyAgainst(snapshot),
	}, nil
}

// ServerCredentials builds what this node serves on its cluster listener, or
// (nil, nil) while the cluster is permissive.
//
// **It demands a certificate and verifies nothing at the handshake**, and that
// asymmetry with the dialling end is the bootstrap, not an oversight. A node
// joining a cluster that already requires TLS is by definition not in that
// cluster's trust set -- it is asking to be put in it -- so a handshake that
// refused an unnamed certificate would make joining impossible, which is exactly
// what happened when both ends shared one config.
//
// The refusal does not disappear, it moves one layer up: the peer's certificate
// is authorised per-RPC against the trust set by the daemon's interceptor, which
// lets an unnamed certificate reach `Join` and nothing else. An unnamed peer can
// therefore complete a handshake and then do exactly one thing, still gated by
// the cluster token a human carried. Encryption comes from the handshake;
// authorisation is a separate question asked at a layer that knows what is being
// asked for.
//
// RequireAnyClientCert rather than RequireAndVerifyClientCert: the latter wants a
// CA pool to chain to, which is the design this cluster deliberately does not
// have, and it would reject every peer. Any is what puts a certificate in the
// connection for the interceptor to identify.
func ServerCredentials(certDir string, snapshot Snapshot) (*tls.Config, error) {
	cfg, keypair, err := identityFor(certDir, snapshot)
	if err != nil || cfg == nil {
		return nil, err
	}

	return &tls.Config{
		Certificates: []tls.Certificate{*keypair},
		MinVersion:   tls.VersionTLS13,
		ClientAuth:   tls.RequireAnyClientCert,
	}, nil
}

// PinnedClientCredentials builds what a joining node offers when it dials the
// cluster it wants to join, verifying the far end against a single fingerprint
// instead of a trust set.
//
// This is the bootstrap from the other side. The joiner has no trust set -- it
// has never spoken to this cluster -- so it has nothing to check the far end
// against except the one fingerprint an operator carried to it inside the join
// token. Pinning it closes the window completely: there is no first use to
// trust, because the first use is already authenticated by something a human
// moved (ADR-0005).
//
// The fingerprint is the SHA-256 of the certificate's DER bytes, lowercase hex,
// which is what Fingerprint produces and what the token carries.
func PinnedClientCredentials(certDir, fingerprint string) (*tls.Config, error) {
	pin := strings.ToLower(strings.TrimSpace(fingerprint))
	if len(pin) != sha256HexLen {
		return nil, fmt.Errorf("a certificate fingerprint is %d hex characters, not %d",
			sha256HexLen, len(pin))
	}

	keypair, err := LocalKeypair(certDir)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		// The joiner's own certificate travels with the handshake as well as in
		// the request (#111 step 3a), because the far end needs something to
		// identify this connection by before it has anywhere to record it.
		Certificates:       []tls.Certificate{*keypair},
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("the node being joined presented no certificate")
			}
			// The leaf only, as everywhere else here: a chain is something this
			// design does not use and must not be persuaded by.
			if got := hashDER(rawCerts[0]); got != pin {
				return fmt.Errorf("the node being joined presented certificate %s, and the "+
					"join token names %s -- either the token is for a different cluster "+
					"or this is not the node it names", got[:12], pin[:12])
			}
			return nil
		},
	}, nil
}

// Fingerprint is the SHA-256 of a PEM certificate's DER bytes, lowercase hex.
//
// The form the join token carries and the form PinnedClientCredentials pins.
// Taken over the DER rather than the PEM so that whitespace, line endings and a
// trailing newline cannot change a node's identity -- the config carries these
// as text, and text gets reformatted.
func Fingerprint(pemCert string) (string, error) {
	der, err := certificateDER(pemCert)
	if err != nil {
		return "", err
	}
	return hashDER(der), nil
}

// sha256HexLen is how long a fingerprint is written out.
const sha256HexLen = sha256.Size * 2

// identityFor answers the two questions both credential builders start with:
// does this cluster require TLS, and can this node produce the identity it would
// need. A nil config with a nil error is the permissive phase.
func identityFor(certDir string, snapshot Snapshot) (*config.Config, *tls.Certificate, error) {
	if snapshot == nil {
		return nil, nil, errors.New("no config to read the TLS mode from")
	}
	cfg := snapshot()
	if cfg == nil {
		return nil, nil, errors.New("no config to read the TLS mode from")
	}
	if !cfg.Pulse.TLSRequired() {
		return nil, nil, nil
	}

	keypair, err := LocalKeypair(certDir)
	if err != nil {
		return nil, nil, err
	}

	// Refuse now rather than at the first handshake if the config names nothing
	// usable. The set is still rebuilt per handshake; this is the early check, so
	// a listener that could never talk to anybody fails to start instead of
	// starting and failing every connection.
	if _, err := NewTrustSet(cfg.Nodes); err != nil {
		return nil, nil, fmt.Errorf("the cluster trust set is unusable: %w", err)
	}
	return cfg, keypair, nil
}

// LocalKeypair reads the node identity stored in dir. Read at build time rather
// than per handshake: step 1 of #111 made it stable across restarts, so there is
// nothing to re-read for.
//
// The directory is a parameter rather than security.CertDir read directly, and
// that is not only tidiness. It is the difference between a process being one
// node and a process being able to be several, which is what the integration
// harness is -- every node it runs is a Server in the same process, so a package
// global here means they all share one identity and TLS between them cannot be
// exercised at all. An empty dir falls back to security.CertDir, which is what
// the daemon passes and what every existing caller gets.
func LocalKeypair(dir string) (*tls.Certificate, error) {
	dir = CertDirOr(dir)
	keypair, err := tls.LoadX509KeyPair(
		filepath.Join(dir, "pulseha.crt"),
		filepath.Join(dir, "pulseha.key"),
	)
	if err != nil {
		return nil, fmt.Errorf("this node's certificate and key could not be loaded from %s: %w",
			dir, err)
	}
	return &keypair, nil
}

// CertDirOr resolves a node's certificate directory, defaulting to the process-wide
// one the daemon uses.
func CertDirOr(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return security.CertDir
	}
	return dir
}

// CertificatePEM returns the public certificate stored in dir, or "" if there is
// none.
//
// Deliberately silent about failure. Every caller is describing a node to somebody
// else, and a node without a certificate is one that has not published yet -- a
// state the permissive phase exists to tolerate, not an error to propagate.
func CertificatePEM(dir string) string {
	pemBytes, err := os.ReadFile(filepath.Join(CertDirOr(dir), "pulseha.crt"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(pemBytes))
}

// verifyAgainst is the dialling end's check: the certificate presented must be
// one the cluster config names.
//
// The trust set is rebuilt on every call rather than captured, because a
// connection can be made long after the credentials were built -- a peer joins,
// a peer is removed, and removal-is-revocation is only true if the set is
// re-read.
func verifyAgainst(snapshot Snapshot) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
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
	}
}
