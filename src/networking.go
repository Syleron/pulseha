package main

import (
	"os"
	"net"
	"os/exec"
	"strings"
	"bytes"
	"github.com/coreos/go-log/log"
)

/**
 * Send Gratuitous ARP to automagically tell the router who has the new floating IP
 * NOTE: This function assumes the OS is LINUX and has "arping" installed.
 */
func SendGARP(iface, ip string) bool {
	if !_ifaceExist(iface) {
		//log.Warn("Network interface does not exist!");
		os.Exit(1)
	}

	output, err := Execute("arping", "-U", "-c", "5", "-I", iface, ip)

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
 * Checks to see what status a network interface is currently.
 * Possible responses are either up or down.
 */
func _netInterfaceStatus(iface string) bool {
	_, err := Execute("cat", "/sys/class/net/"+iface+"/operstate")

	if err != nil {
		//return err.Error();
		return false
	}

	return true

}

/**
 * Local function to see if an interface with name iface exists
 */
func _ifaceExist(iface string) bool {
	ifaces, _ := net.Interfaces()

	// TODO: handle err

	for _, i := range ifaces {
		if i.Name == iface {
			return true
		}
	}

	return false
}

/**
 * This function is to bring up a network interface
 */
func BringIPup(iface, ip string) bool {
	if !_ifaceExist(iface) {
		//log.Warn("Network interface does not exist!");
		os.Exit(1)
	}

	output, err := Execute("ifconfig", iface+":0", ip, "up")

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
 * This function is to bring down a network interface
 */
func BringIPdown(iface, ip string) bool {
	if !_ifaceExist(iface) {
		//log.Warn("Network interfaces does not exist!");
		return false
	}

	output, err := Execute("ifconfig", iface+":0", ip, "down")

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
 * Perform a curl request to a web host.
 * This only returns a boolean based off the http status code received by the request.
 */
func Curl(httpRequestURL string) bool {
	output, err := Execute("curl", "-s", "-o", "/dev/null", "-w", "\"%{http_code}\"", httpRequestURL)

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
 *
 */
func ICMPIPv4(Ipv4Addr string) bool {
	// Validate the IP address to ensure it's an IPv4 addr.
	if !ValidIPAddress(Ipv4Addr) {
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
 * Function to perform an arp scan on the network. This will allow us to see which IP's are available.
 */
func ArpScan(addrWSubnet string) string {
	output, err := Execute("arp-scan", "arp-scan", addrWSubnet)

	if err != nil {
		return err.Error()
	}

	return output
}

/**
 * Return network interface names
 */
func getInterfaceNames() ([]string) {
	ifaces, err := net.Interfaces()
	if err != nil {
		log.Errorf("Error retrieving network interfaces: ", err)
	}
	var interfaceNames []string
	for _, iface := range ifaces {
		interfaceNames = append(interfaceNames, iface.Name)
	}
	return interfaceNames
}

/**
 * Check if an interface exists on the local node
 */
func interfaceExist(name string) (bool) {
	ifaces, err := net.Interfaces()
	if err != nil {
		log.Errorf("Error retrieving network interfaces: ", err)
	}
	for _, iface := range ifaces {
		if iface.Name == name {
			return true
		}
	}
	return false
}