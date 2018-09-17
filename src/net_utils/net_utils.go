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
package net_utils

import (
	"bytes"
	"errors"
	log "github.com/Sirupsen/logrus"
	"github.com/Syleron/PulseHA/src/utils"
	"net"
	"os"
	"os/exec"
	"strings"
)

type ICMPv6MessageHeader struct {
	Type     byte
	Code     byte
	Checksum uint16
}

type ICMPv6NeighborSolicitation struct {
	Header            ICMPv6MessageHeader
	Reserved          uint32
	TargetAddress     [16]byte
	OptionType        byte
	OptionLength      byte
	SourceLinkAddress [6]byte
}

/**
Send Gratuitous ARP to automagically tell the router who has the new floating IP
NOTE: This function assumes the OS is LINUX and has "arping" installed.
*/
func SendGARP(iface, ip string) bool {
	if !InterfaceExist(iface) {
		log.Error("Unable to GARP as the network interface does not exist! Closing..")
		os.Exit(1)
	}
	log.Debug("Sending gratuitous arp for " + ip + " on interface " + iface)
	output, err := utils.Execute("arping", "-U", "-c", "5", "-I", iface, ip)
	if err != nil {
		return false
	}
	if output == "" {
		return true
	} else {
		return false
	}
}

/**
Checks to see what status a network interface is currently.
Possible responses are either up or down.
*/
func netInterfaceStatus(iface string) bool {
	_, err := utils.Execute("cat", "/sys/class/net/"+iface+"/operstate")
	if err != nil {
		//return err.Error();
		return false
	}
	return true
}

/**
This function is to bring up a network interface
*/
func BringIPup(iface, ip string) (bool, error) {
	if !InterfaceExist(iface) {
		return false, errors.New("unable to bring IP up as the network interface does not exist")
	}
	// Check to see if the ip already exists on another interface
	exists, eIface, err := CheckIfIPExists(ip)
	if err != nil {
		return false, err
	}
	if exists {
		BringIPdown(eIface, ip)
	}
	output, err := utils.Execute("ip", "ad", "ad", ip, "dev", iface)
	// guessing
	if err != nil {
		return true, errors.New("Unable to bring up ip " + ip + " on interface " + iface + ". Perhaps it already exists?")
	}
	if output == "" {
		return true, nil
	} else {
		return false, errors.New(output)
	}
}

/**
This function is to bring down a network interface
*/
func BringIPdown(iface, ip string) (bool, error) {
	if !InterfaceExist(iface) {
		return false, errors.New("unable to bring IP down as the network interface does not exist")
	}
	output, err := utils.Execute("ip", "ad", "del", ip, "dev", iface)
	// guessing
	if err != nil {
		return true, errors.New("Unable to bring down ip " + ip + " on interface " + iface + ". Perhaps it doesn't exist?")
	}
	if output == "" {
		return true, nil
	} else {
		return false, err
	}
}

/**
Perform a curl request to a web host.
This only returns a boolean based off the http status code received by the request.
*/
func Curl(httpRequestURL string) bool {
	output, err := utils.Execute("curl", "-s", "-o", "/dev/null", "-w", "\"%{http_code}\"", httpRequestURL)
	if err != nil {
		//log.Error("Http Curl request failed.")
		return false
	}
	if output == "\"200\"" {
		return true
	} else {
		return false
	}
}

/**

 */
func ICMPv4(Ipv4Addr string) bool {
	// Validate the IP address to ensure it's an IPv4 addr.
	if err := utils.ValidIPAddress(Ipv4Addr); err != nil {
		//log.Error("Invalid IPv4 address for ICMP check..")
		return false
	}
	cmds := "ping -c 1 -W 1 " + Ipv4Addr + " &> /dev/null ; echo $?"
	cmd := exec.Command("bash", "-c", cmds)
	cmd.Stdin = strings.NewReader("some input")
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		//log.Error("ICMP request failed.")
		return false
	}
	if strings.Contains(out.String(), "0") {
		return true
	} else {
		return false
	}
}

/**
Function to perform an arp scan on the network. This will allow us to see which IP's are available.
*/
func ArpScan(addrWSubnet string) string {
	output, err := utils.Execute("arp-scan", "arp-scan", addrWSubnet)
	if err != nil {
		return err.Error()
	}
	return output
}

/**
Send the eq. of IPv4 arping with IPv6
*/
func IPv6NDP(ipv6Iface string) string {
	output, err := utils.Execute("ndptool", "-t", "na", "-U", "-i", ipv6Iface)
	if err != nil {
		return err.Error()
	}
	return output
}

/**
Return network interface names
*/
func GetInterfaceNames() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		log.Errorf("Error retrieving network interfaces: ", err)
		return nil
	}
	var interfaceNames []string
	for _, iface := range ifaces {
		interfaceNames = append(interfaceNames, iface.Name)
	}
	return interfaceNames
}

/**
Check if an interface exists on the local node
*/
func InterfaceExist(name string) bool {
	ifaces, err := net.Interfaces()
	if err != nil {
		log.Errorf("Error retrieving network interfaces: ", err)
		return false
	}
	for _, iface := range ifaces {
		if iface.Name == name {
			return true
		}
	}
	return false
}

/**
Checks to see if an IP exists on an interface already
 */
func CheckIfIPExists(ipAddr string) (bool, string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return true, "", err
	}
	for _, i := range ifaces {
		addrs, err := i.Addrs()
		if err != nil {
			return true, "", err
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ipAddr == ip.String() {
				return true, i.Name, nil
			}
		}
	}
	return false, "", nil
}