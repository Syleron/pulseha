package networking

// Required System Calls to correctly Function
// arp-scan
// arping
// ping
// ping6

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

	output, err := Execute("curl", args)

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
	if validIPv4(Ipv4Addr) {
		return	""
	}

	// Create list of commands to execute
	args := []string{
		"ping",
		Ipv4Addr,
	}

	output, err := Execute("ping", args)

	if err != nil {
		return err.Error();
	}

	return output
}

/**
 *
 */
func ICMPIPv6(Ipv6Addr string) string {
	// Validate the IP address to ensure it's an IPv4 addr.
	if validIPv6(Ipv6Addr) {
		return	""
	}

	// Create list of commands to execute
	args := []string{
		"ping6",
		Ipv6Addr,
	}

	output, err := Execute("ping6", args)

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

	output, err := Execute("arp-scan", args)

	if err != nil {
		return err.Error();
	}

	return output
}
