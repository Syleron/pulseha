/*
   PulseHA - HA Cluster Daemon
   Copyright (C) 2017-2018  Andrew Zak <andrew@pulseha.com>

   This program is free software: you can redistribute it and/or modify
   it under the terms of the GNU Affero General Public License as published
   by the Free Software Foundation, either version 3 of the License, or
   (at your option) any later version.

   This program is distributed in the hope that it will be useful,
   but WITHOUT ANY WARRANTY; without even the implied warranty of
   MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
   GNU Affero General Public License for more details.

   You should have received a copy of the GNU Affero General Public License
   along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/
package security

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/Syleron/PulseHA/src/utils"
	"math/big"
	"net"
	"os"
	"time"
)

const CertDir = "/etc/pulseha/certs/"

/**
Generate TLS keys if they don't already exist
*/
func GenTLSKeys(ip string) error {
	utils.CreateFolder("/etc/pulseha/certs")
	log.Warning("TLS keys are missing! Generating..")
	if !utils.CheckFileExists(CertDir+"ca.crt") ||
		!utils.CheckFileExists(CertDir+"ca.key") {
		return errors.New("Unable to generate TLS keys as ca.crt/ca.key are missing")
	}
	// Load the CA
	caCert, err := utils.LoadFile(CertDir + "ca.crt")
	caKey, err := utils.LoadFile(CertDir + "ca.key")
	if err != nil {
		log.Error(err.Error())
		return errors.New(err.Error())
	}
	// Decode the cert and key
	cpb, _ := pem.Decode(caCert)
	kpb, _ := pem.Decode(caKey)
	//// Parse the cert
	cert, e := x509.ParseCertificate(cpb.Bytes)
	if e != nil {
		fmt.Println("parsex509:", e.Error())
		os.Exit(1)
	}
	// Parse the key
	key, e := x509.ParsePKCS1PrivateKey(kpb.Bytes)
	if e != nil {
		fmt.Println("parsekey:", e.Error())
		os.Exit(1)
	}
	// Generate Server certs
	GenerateServerCert(ip, cert, key)
	// Generate Client certs
	GenerateClientCert(cert, key)
	return nil
}

/**

 */
func GenerateCACert(ip string) {
	utils.CreateFolder("/etc/pulseha/certs")
	// Generate new key pair
	rootKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatalf("generating random key: %v", err)
	}
	// Generate Cert Template
	rootCertTmpl, err := certTemplate()
	if err != nil {
		log.Fatalf("creating cert template: %v", err)
	}
	// Populate cert template
	rootCertTmpl.IsCA = true
	rootCertTmpl.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature
	rootCertTmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	rootCertTmpl.IPAddresses = []net.IP{net.ParseIP(ip)}
	// Generate cert from template and sign
	_, rootCertPEM, err := createCert(rootCertTmpl, rootCertTmpl, &rootKey.PublicKey, rootKey)
	if err != nil {
		log.Fatalf("error creating cert: %v", err)
	}
	// write keys
	writeCertFile("ca", rootCertPEM)
	writeKeyFile("ca", rootKey)
}

/**

 */
func GenerateServerCert(ip string, caCert *x509.Certificate, caKey *rsa.PrivateKey) {
	utils.CreateFolder("/etc/pulseha/certs")
	// Generate new key pair
	servKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatalf("generating random key: %v", err)
	}
	// Generate Cert template
	servCertTmpl, err := certTemplate()
	if err != nil {
		log.Fatalf("creating cert template: %v", err)
	}
	// Populate cert template
	servCertTmpl.KeyUsage = x509.KeyUsageDigitalSignature
	servCertTmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	servCertTmpl.IPAddresses = []net.IP{net.ParseIP(ip)}
	// Generate cert from template and sign
	_, servCertPEM, err := createCert(servCertTmpl, caCert, &servKey.PublicKey, caKey)
	if err != nil {
		log.Fatalf("error creating cert: %v", err)
	}
	// write keys
	hostname, err := utils.GetHostname()
	if err != nil {
		log.Error("unable to generate cert because unable to get hostname")
		return
	}
	writeCertFile(hostname+".server", servCertPEM)
	writeKeyFile(hostname+".server", servKey)
}

/**

 */
func GenerateClientCert(caCert *x509.Certificate, caKey *rsa.PrivateKey) {
	utils.CreateFolder("/etc/pulseha/certs")
	// Generate new key pair
	clientKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatalf("generating random key: %v", err)
	}
	// Generate Cert Template
	clientCertTmpl, err := certTemplate()
	if err != nil {
		log.Fatalf("creating cert template: %v", err)
	}
	// Populate cert template
	clientCertTmpl.KeyUsage = x509.KeyUsageDigitalSignature
	clientCertTmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	// the root cert signs the cert by again providing its private key
	_, clientCertPEM, err := createCert(clientCertTmpl, caCert, &clientKey.PublicKey, caKey)
	if err != nil {
		log.Fatalf("error creating cert: %v", err)
	}
	// write keys
	hostname, err := utils.GetHostname()
	if err != nil {
		log.Error("unable to generate cert because unable to get hostname")
		return
	}
	writeCertFile(hostname+".client", clientCertPEM)
	writeKeyFile(hostname+".client", clientKey)
}

/**

 */
func certTemplate() (*x509.Certificate, error) {
	// generate a random serial number
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return nil, errors.New("failed to generate serial number: " + err.Error())
	}
	hostname, err := utils.GetHostname()
	if err != nil {
		return nil, errors.New("unable to generate cert template because unable to get hostname")
	}
	tmpl := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"PulseHA"},
			CommonName:   hostname,
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Duration(730) * time.Hour * 24),
		BasicConstraintsValid: true,
	}
	return &tmpl, nil
}

/**

 */
func createCert(template, parent *x509.Certificate, pub interface{}, parentPriv interface{}) (cert *x509.Certificate, certPEM []byte, err error) {
	certDER, err := x509.CreateCertificate(rand.Reader, template, parent, pub, parentPriv)
	if err != nil {
		return
	}
	// parse the resulting certificate so we can use it again
	cert, err = x509.ParseCertificate(certDER)
	if err != nil {
		return
	}
	// PEM encode the certificate
	b := pem.Block{Type: "CERTIFICATE", Bytes: certDER}
	certPEM = pem.EncodeToMemory(&b)
	return
}

/**
TODO: Use Utils functions
*/
func writeCertFile(fileName string, cert []byte) {
	// Write the cert to file
	certOut, err := os.Create(CertDir + fileName + ".crt")
	if err != nil {
		hostname, _ := utils.GetHostname()
		fmt.Println("Failed to open "+hostname+" for writing:", err)
		os.Exit(1)
	}
	certOut.Write(cert)
	certOut.Close()
}

/**
TODO: Use Utils functions
*/
func writeKeyFile(filename string, key *rsa.PrivateKey) {
	// Write the key to file
	keyOut, err := os.OpenFile(CertDir+filename+".key", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		hostname, _ := utils.GetHostname()
		fmt.Println("Failed to open key "+hostname+" for writing:", err)
		os.Exit(1)
	}
	pem.Encode(keyOut, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	keyOut.Close()
}
