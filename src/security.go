package main

import (
	"log"
	"os"
)

/**
 * Generate new Cert/Key pair. No Cert Authority.
 */
func GenOpenSSL() {
	_, err := Execute("openssl", "req", "-x509", "-newkey", "rsa:2048", "-keyout", "./certs/server.key", "-out", "./certs/server.crt", "-days", "365", "-subj", "/CN="+GetHostname(), "-sha256", "-nodes")

	if err != nil {
		log.Fatal("Failed to create private server key.")
		os.Exit(1)
	}
}