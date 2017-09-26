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
	"github.com/coreos/go-log/log"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
	"errors"
)

/**
 * Load a specific file and return byte code
 **/
func LoadFile(file string) []byte {
	c, err := ioutil.ReadFile(file)

	// We had an error attempting to decode the json into our struct! oops!
	if err != nil {
		//log.Error("Unable to load file. Does it exist?")
		os.Exit(1)
	}

	return []byte(c)
}

/**
 * Execute system command.
 */
func Execute(cmd string, args ...string) (string, error) {
	command := exec.Command(cmd, args...)

	//printCommand(command)
	output, err := command.CombinedOutput()

	if err != nil {
		//printError(err)
		return "", err
	}

	return string(output), err
}

/**
 * Function that validates an IPv4 and IPv6 address.
 *
 * @return bool
 */
func ValidIPAddress(ipAddress string) bool {
	testInput := net.ParseIP(ipAddress)
	if testInput.To4() == nil {
		return false
	}
	return true
}

/**
 * Function to schedule the execution every x time as time.Duration.
 */
func Scheduler(method func(), delay time.Duration) {
	for _ = range time.Tick(delay) {
		method()
	}
}

/**
 * Create folder if it doesn't already exist!
 * Returns true or false depending on whether the folder was created or not.
 */
func CreateFolder(path string) bool {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		os.Mkdir(path, os.ModePerm)
		return true
	}
	return false
}

/**
 * Check if a folder exists.
 */
func CheckFolderExist(path string) bool {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false
	}
	return true
}

/**
 * Get local hostname
 * Note: This may break with FQDs
 */
func GetHostname() string {
	output, err := Execute("hostname")
	if err != nil {
		log.Error("Failed to obtain hostname.")
		os.Exit(1)
	}
	// Remove new line characters
	return strings.TrimSuffix(output, "\n")
}

/**
 * Private - Check to see if we are in a configured cluster or not.
 */
func clusterCheck(c *Config) (bool) {
	if len(c.Nodes) > 0 {
		return true
	}
	return false
}

/**
 * Return the total number of configured nodes we have in our config.
 */
func clusterTotal(c *Config) (int) {
 return len(c.Nodes)
}


/**
 * Function to return an IP and Port from a single ip:port string
 */
func splitIpPort(ipPort string) (string, string, error) {
	IPslice := strings.Split(ipPort, ":")

	if len(IPslice) < 2 {
		return "", "", errors.New("Invalid IP:Port string. Unable to split.")
	}

	return IPslice[0], IPslice[1], nil
}
