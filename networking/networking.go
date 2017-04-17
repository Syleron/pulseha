package networking

import (
	log "github.com/Sirupsen/logrus"
	"github.com/syleron/pulse/utils"
	"fmt"
	"net"
)

// Required System Calls to correctly Function
// arp-scan
// arping
// ping

/**
 * Send Gratuitous ARP to automagically tell the router who has the new floating IP
 * NOTE: This function assumes the OS is LINUX and has "arping" installed.
 */
func SendGARP() error {

	return nil
}

/**
 *
 */
func AssignFloatingIP() {

}

/**
 * Checks to see what status a network interface is currently.
 * Possible responses are either up or down.
 */
func _netInterfaceStatus(iface string) {
	args := []string{
		"/sys/class/net/"+iface+"operstate",
	}

	output, err := utils.Execute("cat", args)

	if err != nil {
		return err.Error();
	}

	log.Debug(output)

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
func BringIPup(iface string) bool{
	if !_ifaceExist(iface) {
		// TODO: Error log
		return false
	}

	args := []string{
		iface,
	}

	output, err := utils.Execute("ifup", args)

	if err != nil {
		return err.Error();
	}

	log.Debug(output)

	return true
}

/**
 * This function is to bring down a network interface
 */
func BringIPdown(iface string) bool{
	if !_ifaceExist(iface) {
		// TODO: Error log
		return false
	}

	args := []string{
		iface,
	}

	output, err := utils.Execute("ifdown", args)

	if err != nil {
		return err.Error();
	}

	log.Debug(output)

	return true
}



/**
 * TODO: This function should probably return a boolean on weather the request was successful or not
 */
func Curl(httpRequestURL string) {
	// Create list of commands to execute
	args := []string{
		"curl", // Is this needed?
		"-s",
		"-o",
		"/dev/null",
		"-w",
		"\"%{http_code}\"",
		httpRequestURL,
	}

	output, err := utils.Execute("curl", args)

	if err != nil {
		return err.Error();
	}

	return output
}

/**
 *
 */
func ICMPIPv4(Ipv4Addr string) string {
	// Validate the IP address to ensure it's an IPv4 addr.
	if utils.ValidIPv4(Ipv4Addr) {
		return	""
	}

	// Create list of commands to execute
	args := []string{
		"ping",
		Ipv4Addr,
	}

	output, err := utils.Execute("ping", args)

	if err != nil {
		return err.Error();
	}

	return output
}

/**
 * Function to perform an arp scan on the network. This will allow us to see which IP's are available.
 */
func arpScan(addrWSubnet string) string{
	// Create list of commands to execute
	args := []string{
		"arp-scan",
		addrWSubnet,
	}

	output, err := utils.Execute("arp-scan", args)

	if err != nil {
		return err.Error();
	}

	return output
}
