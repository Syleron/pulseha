package security

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// certRenewalWindow is how close to expiry a node certificate may get before it
// is replaced. A year's validity against a daemon that may run for months means
// the renewal has to happen well before the last day, and on a restart the node
// was going to make anyway.
const certRenewalWindow = 30 * 24 * time.Hour

// EnsureCertificates gives this node a usable certificate, generating one only
// when what is on disk cannot be used.
//
// The point of the check is stability, not speed (docs/TEST-PLAN.md #111 and
// ADR-0005). GenerateCertificates is unguarded and rewrites all four files on
// every daemon start — observed on a lab appliance with the CA written at
// 07:55:31 against a process start of 07:55:30 — so a node's identity changed
// every time it restarted. That is survivable while nothing depends on the
// identity, and it is the first thing that has to stop being true for trust to
// be an allowlist: a certificate that changes on restart cannot be in one.
//
// Returns whether it generated, and why. The reason is for the caller's logger
// rather than this package's: nothing calls SetLevel on the logger here, so a
// line written from this package cannot reach the journal at any logging_level
// (#61).
func EnsureCertificates(hostname string) (generated bool, reason string, err error) {
	if err := os.MkdirAll(CertDir, 0755); err != nil {
		return false, "", fmt.Errorf("failed to create cert directory: %v", err)
	}

	if reason := certsUnusable(CertDir, hostname, time.Now()); reason != "" {
		if err := GenerateCertificates(hostname); err != nil {
			return false, reason, err
		}
		return true, reason, nil
	}
	return false, "", nil
}

// certsUnusable reports why the certificates on disk cannot be used, or "" when
// they can.
//
// A reason rather than a bool, because "the daemon replaced this node's identity"
// is something an operator should be able to find the cause of afterwards, and
// because the cases are not equally benign: a missing file is a first start, an
// expired certificate is a renewal, and a certificate that does not match its CA
// is a half-written state worth seeing.
//
// Every failure is a reason to regenerate rather than an error to return. What is
// on disk is this node's own identity and nothing else's, so replacing it when it
// cannot be read costs nothing that was working.
func certsUnusable(dir, hostname string, now time.Time) string {
	caCert, reason := loadCert(filepath.Join(dir, "ca.crt"), "CA certificate")
	if reason != "" {
		return reason
	}
	if _, reason := loadKey(filepath.Join(dir, "ca.key"), "CA key"); reason != "" {
		return reason
	}
	nodeCert, reason := loadCert(filepath.Join(dir, "pulseha.crt"), "node certificate")
	if reason != "" {
		return reason
	}
	nodeKey, reason := loadKey(filepath.Join(dir, "pulseha.key"), "node key")
	if reason != "" {
		return reason
	}

	// The pair has to belong together. Checked because the two are written
	// separately, so a crash between the renames leaves a node certificate signed
	// by a CA that is no longer on disk -- usable-looking, and unverifiable by
	// anyone else.
	if err := nodeCert.CheckSignatureFrom(caCert); err != nil {
		return "node certificate is not signed by the CA on disk: " + err.Error()
	}
	if pub, ok := nodeCert.PublicKey.(*rsa.PublicKey); !ok || pub.N.Cmp(nodeKey.N) != 0 {
		return "node certificate does not match the node key on disk"
	}

	if now.After(nodeCert.NotAfter) {
		return "node certificate expired on " + nodeCert.NotAfter.Format(time.RFC3339)
	}
	if now.Add(certRenewalWindow).After(nodeCert.NotAfter) {
		return "node certificate expires on " + nodeCert.NotAfter.Format(time.RFC3339) +
			", inside the renewal window"
	}
	if now.Before(caCert.NotBefore) || now.After(caCert.NotAfter) {
		return "CA certificate is outside its validity period"
	}

	// The hostname is the certificate's subject, so a renamed host needs a new
	// one. Cheap to check, and the alternative is a node presenting a name it no
	// longer has.
	if nodeCert.Subject.CommonName != hostname {
		return fmt.Sprintf("node certificate names %q but this host is %q",
			nodeCert.Subject.CommonName, hostname)
	}

	return ""
}

func loadCert(path, what string) (*x509.Certificate, string) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, what + " is missing"
		}
		return nil, what + " could not be read: " + err.Error()
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, what + " is not a PEM certificate"
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, what + " could not be parsed: " + err.Error()
	}
	return cert, ""
}

func loadKey(path, what string) (*rsa.PrivateKey, string) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, what + " is missing"
		}
		return nil, what + " could not be read: " + err.Error()
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, what + " is not PEM"
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, what + " could not be parsed: " + err.Error()
	}
	return key, ""
}
