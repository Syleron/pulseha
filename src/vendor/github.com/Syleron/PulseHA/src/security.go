package main

import (
	"os"
	"path/filepath"
	"github.com/coreos/go-log/log"
)

/**
 * Generate new Cert/Key pair. No Cert Authority.
 */
func GenOpenSSL() {
	// Get project directory location
	dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		log.Emergency(err)
	}
	_, err = Execute("openssl", "req", "-x509", "-newkey", "rsa:2048", "-keyout", dir + "/certs/server.key", "-out", dir + "/certs/server.crt", "-days", "365", "-subj", "/CN="+GetHostname(), "-sha256", "-nodes")

	if err != nil {
		log.Emergency("Failed to create private server key.")
		os.Exit(1)
	}
}