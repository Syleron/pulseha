/*
   PulseHA - HA Cluster Daemon
   Copyright (C) 2017-2019  Andrew Zak <andrew@linux.com>

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
package utils

import (
	"errors"
	log "github.com/Sirupsen/logrus"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

/**
Load a specific file and return byte code
 **/
func LoadFile(file string) ([]byte, error) {
	c, err := ioutil.ReadFile(file)

	if err != nil {
		return nil, errors.New("unable to load file: " + file)
	}

	return []byte(c), nil
}

/**
Execute system command.
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
Function that validates an IPv4 and IPv6 address.
*/
func ValidIPAddress(ipAddress string) error {
	ip, _, err := net.ParseCIDR(ipAddress)
	if err != nil {
		return errors.New("invalid CDIR address specified")
	}
	if !IsIPv6(ip.String()) {
		if !IsIPv4(ip.String()) {
			return errors.New("invalid IP address")
		}
	}
	return nil
}

/**
Function to schedule the execution every x time as time.Duration.
*/
func Scheduler(method func() bool, delay time.Duration) {
	for _ = range time.Tick(delay) {
		end := method()
		if end {
			break
		}
	}
}

/**
Create folder if it doesn't already exist!
Returns true or false depending on whether the folder was created or not.
*/
func CreateFolder(path string) bool {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		os.Mkdir(path, os.ModePerm)
		return true
	}
	return false
}

// DeleteFolder - Remove a folder and its conents
func DeleteFolder(path string) bool {
	err := os.RemoveAll(path)
	if err != nil {
		return false
	}
	return true
}

/**
Check if a folder exists.
*/
func CheckFolderExist(path string) bool {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false
	}
	return true
}

/**
Check if a file exists
*/
func CheckFileExists(path string) bool {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false
	}
	return true
}

/**
Get local hostname
TODO: Note: This may break with FQDs
*/
func GetHostname() (string, error) {
	output, err := Execute("hostname")
	if err != nil {
		return "", err
	}
	// Remove new line characters
	return strings.TrimSuffix(output, "\n"), nil
}

/**
Function to return an IP and Port from a single ip:port string
TODO:Note: Works only with IPv4
*/
func SplitIpPort(ipPort string) (string, string, error) {
	IPslice := strings.Split(ipPort, ":")
	if len(IPslice) < 2 {
		return "", "", errors.New("Invalid IP:Port string. Unable to split.")
	}
	return IPslice[0], IPslice[1], nil
}

/**
Checks to see if the address contains a colon.
TODO: Note: This will not work with ip:port combinations
*/
func IsIPv6(address string) bool {
	leftBrace := strings.Replace(address, "[", "", -1)
	cleanIP := strings.Replace(leftBrace, "]", "", -1)
	ip := net.ParseIP(cleanIP)
	return ip != nil && strings.Contains(cleanIP, ":")
}

/**
Checks to see if the address is an IPv4 address
*/
func IsIPv4(address string) bool {
	ip := net.ParseIP(address)
	return ip != nil && ip.To4() != nil
}

/**
Makes sure an IPv6 address is properly structured
*/
func SanitizeIPv6(address string) string {
	leftBrace := strings.Replace(address, "[", "", -1)
	cleanIP := strings.Replace(leftBrace, "]", "", -1)
	//cleanIP = "[" + cleanIP + "]"
	return cleanIP
}

/**
Format IPv6 address with brackets
*/
func FormatIPv6(address string) string {
	var found int
	var cleanIP string
	if IsIPv6(address) {
		if strings.Contains("[", address) {
			found++
		} else if strings.Contains("]", address) {
			found++
		}
		if found > 0 {
			cleanIP = SanitizeIPv6(address)
		}
		cleanIP = "[" + address + "]"
		return cleanIP
	}
	return address
}

/**
Checks to see if a port is valid
*/
func IsPort(port string) bool {
	if i, err := strconv.Atoi(port); err == nil && i > 0 && i < 65536 {
		return true
	}
	return false
}

/**
Validates whether an address is a CIDR address or not
TODO: Note: Should work with both IPv4 and IPv6
*/
func IsCIDR(cidr string) bool {
	_, _, err := net.ParseCIDR(cidr)
	return err == nil
}

/**

 */
func GetCIDR(cidr string) (net.IP, *net.IPNet) {
	ip, mask, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, nil
	}
	return ip, mask
}

/**
hasPort is given a string of the form "host", "host:port", "ipv6::address",
or "[ipv6::address]:port", and returns true if the string includes a port.
*/
func HasPort(s string) bool {
	if strings.LastIndex(s, "[") == 0 {
		return strings.LastIndex(s, ":") > strings.LastIndex(s, "]")
	}
	return strings.Count(s, ":") == 1
}

/**
Write text to a file
*/
func WriteTextFile(contents string, file string) error {
	err := ioutil.WriteFile(file, []byte(contents), 0644)
	if err != nil {
		log.Fatal("Failed to write tls config")
		return err
	}
	return nil
}
