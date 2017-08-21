package security

import (
	"os"
	log "github.com/Sirupsen/logrus"
	"github.com/Syleron/Pulse/src/utils"
)

/**
 * Generate new Cert/Key pair. No Cert Authority.
 */
func Generate() {
    _, err := utils.Execute("openssl", "req", "-x509", "-newkey", "rsa:2048", "-keyout", "./certs/server.key", "-out", "./certs/server.crt", "-days", "365", "-subj", "/CN="+utils.GetHostname(), "-sha256", "-nodes")
    
	if err != nil {
		log.Fatal("Failed to create private server key.")
		os.Exit(1)
	}
	
	log.Info("TLS Keys generated!")
}
