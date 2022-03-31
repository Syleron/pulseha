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

package network

import (
	"bytes"
	"errors"
	log "github.com/sirupsen/logrus"
	"github.com/syleron/pulseha/packages/utils"
	"github.com/vishvananda/netlink"
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
	exists, _ := InterfaceExist(iface)
	if !exists {
		log.Error("Unable to GARP as the network interface does not exist! Closing..")
		os.Exit(1)
	}
	cidrIP, _, err := net.ParseCIDR(ip)
	if err != nil{
		log.Error("failed to GARP. Cannot parse CIDR")
		return false
	}
	log.Debug("Sending gratuitous arp for " + cidrIP.String() + " on interface " + iface)
	_, err = utils.Execute("arping", "-U", "-c", "5", "-I", iface, cidrIP.String())
	if err != nil {
		log.Error("failed to GARP. " + err.Error())
		return false
	}
	return true
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
func BringIPup(iface, ip string) error {
	log.Debug("Attempting to bring up IP address via network package")
	exists, link := InterfaceExist(iface)
	if !exists {
		return errors.New("unable to bring IP up as the network interface does not exist")
	}
	// Check to see if the ip already exists on another interface
	ipOb, _ := utils.GetCIDR(ip)
	exists, eIface, err := CheckIfIPExists(ipOb.String())
	if err != nil {
		log.Debug("Network Package - BringIPup() Failed to check to see if IP exists. ", err)
		return err
	}
	if exists {
		if err := BringIPdown(eIface, ip); err != nil {
			log.Debug("Attempted to bring down ip " + ipOb.String() + " however it appears it wasn't up")
		}
	}
	addr, err := netlink.ParseAddr(ip)
	if err != nil {
		log.Debug("Network Package - BringIPup() Failed to parse addr ", err)
		return errors.New("unable to bring IP up because ip address couldn't be parsed")
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		log.Debug("Network Package - BringIPup() ", err)
		return errors.New("unable to bring IP up as netlink failed to do so")
	}
	return nil
}

/**
This function is to bring down a network interface
*/
func BringIPdown(iface, ip string) error {
	log.Debug("Attempting to bring down IP address via network package")
	exists, link := InterfaceExist(iface)
	if !exists {
		log.Debug("unable to bring IP down as the network interface does not exist")
		return errors.New("unable to bring IP down as the network interface does not exist")
	}
	addr, err := netlink.ParseAddr(ip)
	if err != nil {
		log.Debug("unable to bring IP down because ip address couldn't be parsed")
		return errors.New("unable to bring IP down because ip address couldn't be parsed")
	}
	if err := netlink.AddrDel(link, addr); err != nil {
		log.Debug("Unable to bring down ip " + ip + " on interface " + iface + ". Perhaps it doesn't exist?")
		return errors.New("Unable to bring down ip " + ip + " on interface " + iface + ". Perhaps it doesn't exist?")
	}
	return nil
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
func ICMPv4(Ipv4Addr string) error {
	// Validate the IP address to ensure it's an IPv4 addr.
	// if err := utils.ValidIPAddress(Ipv4Addr); err != nil {
	// 	return errors.New("invalid UPv4 address for ICMP check")
	// }
	cmds := "ping -c 1 -W 1 " + Ipv4Addr + " &> /dev/null ; echo $?"
	cmd := exec.Command("bash", "-c", cmds)
	// cmd.Stdin = strings.NewReader("some input")
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		log.Error("ICMP request failed.", Ipv4Addr)
		return err
	}
	if !strings.Contains(out.String(), "0") {
		log.Error("ICMP request failed. ", Ipv4Addr, out.String())
		return errors.New("failed to reach host")
	}
	return nil
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
	log.Debug("Network Package - GetInerfacesNames()")
	links, err := netlink.LinkList()
	if err != nil {
		log.Debug("Network Package - GetInterfaceNames() Error retrieving network links via netlink. ", err)
		return nil
	}
	var interfaceNames []string
	for _, iface := range links {
		intface, _ := netlink.LinkByName(iface.Attrs().Name)
		if intface.Attrs().Slave == nil {
			interfaceNames = append(interfaceNames, iface.Attrs().Name)
		}
	}
	return interfaceNames
}

/**
Check if an interface exists on the local node
*/
func InterfaceExist(name string) (bool, netlink.Link) {
	log.Debug("Network Package - InterfaceExists()")
	link, err := netlink.LinkByName(name)
	if err != nil {
		log.Debug(err)
		return false, nil
	}
	return true, link
}

/**
Checks to see if an IP exists on an interface already
*/
func CheckIfIPExists(ipAddr string) (bool, string, error) {
	links, err := netlink.LinkList()
	if err != nil {
		log.Debug("Network Package - CheckIfIPEixists() Failed to get network links via netlink. ", err)
		return true, "", err
	}

	// Note: Only does ipv4.
	// TODO: ipv6
	for _, link := range links {
		// Get IP addresses for link
		addrs, err := netlink.AddrList(link, 4)
		if err != nil {
			log.Debug("Network Package - CheckIfIPExists() Failed to get addresses for link via netlink. ", err)
			return true, "", err
		}
		for _, addr := range addrs {
			if ipAddr == addr.IP.String() {
				return true, addr.Label, nil
			}
		}
	}

	return false, "", nil
}
