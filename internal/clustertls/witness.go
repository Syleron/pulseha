package clustertls

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
)

// The Witness path is TLS whatever tls_mode says (ADR-0006).
//
// A Ruling can make a healthy node release its addresses, and the Witness is
// reached on the service network, which far more than the cluster can reach. A
// plaintext Ruling would be a one-packet outage for anyone who could answer it.
// ADR-0005's permissive phase exists to migrate links that already existed one
// node at a time; this one did not exist, so there is nothing to migrate and no
// reason to inherit the plaintext. These builders therefore never return (nil,
// nil): a node that cannot produce an identity, or a cluster whose trust set
// names nobody, gets an error, and the Witness path is simply unavailable -- which
// arbitration reads as a silent Witness, today's behaviour.

// WitnessServerCredentials is what a node serves on its Witness listener. It
// demands a certificate; the listener's interceptor authorises it.
func WitnessServerCredentials(certDir string, snapshot Snapshot) (*tls.Config, error) {
	keypair, err := requiredIdentity(certDir, snapshot)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{*keypair},
		MinVersion:   tls.VersionTLS13,
		ClientAuth:   tls.RequireAnyClientCert,
	}, nil
}

// WitnessClientCredentials is what a node offers when it dials expectedID on
// the Witness path. Stricter than ClientCredentials: the far end must be not
// just some member of the trust set but expectedID itself, because a Report is
// evidence about one particular node and an answer from any other is not it.
func WitnessClientCredentials(certDir string, snapshot Snapshot, expectedID string) (*tls.Config, error) {
	keypair, err := requiredIdentity(certDir, snapshot)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{*keypair},
		MinVersion:   tls.VersionTLS13,
		// Paired with the callback below, as in ClientCredentials: no authority
		// vouches for names here, and the config's trust set replaces it.
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
			id, err := set.VerifyPeer(rawCerts)
			if err != nil {
				return err
			}
			if id != expectedID {
				return fmt.Errorf("the Witness path to %s was answered by %s", expectedID, id)
			}
			return nil
		},
	}, nil
}

// requiredIdentity is identityFor without the mode check.
func requiredIdentity(certDir string, snapshot Snapshot) (*tls.Certificate, error) {
	if snapshot == nil {
		return nil, errors.New("no config to build the Witness path's identity from")
	}
	cfg := snapshot()
	if cfg == nil {
		return nil, errors.New("no config to build the Witness path's identity from")
	}
	keypair, err := LocalKeypair(certDir)
	if err != nil {
		return nil, err
	}
	if _, err := NewTrustSet(cfg.Nodes); err != nil {
		return nil, fmt.Errorf("the cluster trust set is unusable: %w", err)
	}
	return keypair, nil
}
