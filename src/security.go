/*
   PulseHA - HA Cluster Daemon
   Copyright (C) 2017  Andrew Zak <andrew@pulseha.com>

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
package main

import (
	"github.com/Syleron/PulseHA/src/utils"
	log "github.com/Sirupsen/logrus"
	"os"
)

/**
 * Generate new Cert/Key pair. No Cert Authority.
 */
func GenOpenSSL() {
	// Make sure we have our TLS conf generated
	dir := "/etc/pulseha/certs/"
	_, err := utils.Execute("openssl", "req", "-x509", "-nodes", "-days", "365", "-newkey", "rsa:2048", "-keyout", dir+utils.GetHostname()+".key", "-out", dir+utils.GetHostname()+".crt", "-config", dir+"tls.conf")

	if err != nil {
		log.Info(err)
		log.Fatal("Failed to create private server key.")
		os.Exit(1)
	}
}

/**
Generate the ssl conf file to generate the certs
 */
func GenTLSConf(IP string) {
	dir := "/etc/pulseha/certs/"
	contents := `[req]
distinguished_name = req_distinguished_name
x509_extensions = v3_req
prompt = no
[req_distinguished_name]
CN = fred
[v3_req]
keyUsage = keyEncipherment, dataEncipherment
extendedKeyUsage = serverAuth
subjectAltName = @alt_names
[alt_names]
IP.1 = ` + IP
	utils.WriteTextFile(contents, dir+"tls.conf")
}
