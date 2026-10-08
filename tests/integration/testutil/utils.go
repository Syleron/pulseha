package testutil

import (
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// IsRoot checks if the current user is root
func IsRoot() bool {
	cmd := exec.Command("id", "-u")
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	// Trim the newline
	id := strings.TrimSpace(string(output))
	return id == "0"
}

// HasIPOnInterface checks if the specified IP address is on the given interface
func HasIPOnInterface(iface, ip string) (bool, error) {
	netIface, err := net.InterfaceByName(iface)
	if err != nil {
		return false, err
	}

	addrs, err := netIface.Addrs()
	if err != nil {
		return false, err
	}

	for _, addr := range addrs {
		// Get the IP address as a string
		addrStr := addr.String()
		// Remove the subnet mask if present
		if strings.Contains(addrStr, "/") {
			addrStr = strings.Split(addrStr, "/")[0]
		}

		if addrStr == ip {
			return true, nil
		}
	}

	return false, nil
}

// Contains checks if a slice contains a string
func Contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// HasNetAdmin checks the capability required for Linux address placement.
// Root inside a container does not necessarily have this capability.
func HasNetAdmin() bool {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "CapEff:" {
			caps, err := strconv.ParseUint(fields[1], 16, 64)
			return err == nil && caps&(1<<12) != 0
		}
	}
	return false
}
