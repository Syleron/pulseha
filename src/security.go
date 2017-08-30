package main

import (
	"github.com/Syleron/Pulse/src/utils"
	"log"
	"os"
)

/**
 * Generate new Cert/Key pair. No Cert Authority.
 */
func GenOpenSSL() {
	_, err := utils.Execute("openssl", "req", "-x509", "-newkey", "rsa:2048", "-keyout", "./certs/server.key", "-out", "./certs/server.crt", "-days", "365", "-subj", "/CN="+utils.GetHostname(), "-sha256", "-nodes")

	if err != nil {
		log.Fatal("Failed to create private server key.")
		os.Exit(1)
	}
}