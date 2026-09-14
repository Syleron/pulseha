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

package testutils

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GiveNodeAnIdentity writes a keypair into dir and points the node at it,
// returning the certificate PEM the cluster config would carry.
//
// A directory per node, because every node in a test cluster is a Server in this
// one process: the daemon's process-wide certificate directory would hand them
// all the same certificate, and a trust set that cannot tell two nodes apart is
// not testing anything (#111).
//
// Mints rather than calling security.EnsureCertificates for the same reason —
// that writes to the process-wide directory — and because the harness sets
// PULSEHA_TEST=true, under which Start deliberately generates nothing.
func (n *TestNode) GiveNodeAnIdentity(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("cert dir: %w", err)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", fmt.Errorf("key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: n.Hostname},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth,
		},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", fmt.Errorf("cert: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	if err := os.WriteFile(filepath.Join(dir, "pulseha.crt"), certPEM, 0600); err != nil {
		return "", fmt.Errorf("write cert: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pulseha.key"), keyPEM, 0600); err != nil {
		return "", fmt.Errorf("write key: %w", err)
	}

	n.CertDir = dir
	return strings.TrimSpace(string(certPEM)), nil
}
