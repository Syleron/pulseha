package main

import (
	"github.com/Syleron/PulseHA/src/netUtils"
	"github.com/coreos/go-log/log"
)

/**
	Bring up the groups on the current node
 */
func makeMemberActive()error{
	log.Debug("Making this node active")
	configCopy := gconf.GetConfig()
	for name, node := range configCopy.Nodes{
		if name == gconf.getLocalNode() {
			for iface, groups := range node.IPGroups {
				for _, groupName := range groups {
					makeGroupActive(iface, groupName)
				}
			}
		}
	}
	return nil
}

/**
 * Make a group of IPs active
 */
func makeGroupActive(iface string, groupName string) {
	log.Debugf("Make group active. Interface: %s, group: %s",iface ,groupName)
	// gconf.Reload()
	configCopy := gconf.GetConfig()
	bringUpIPs(iface, configCopy.Groups[groupName])
	//garp?
}


	//confirm takeover was successful if not shout
	// tell other nodes we are active


/**
 * Bring up a group of ip addresses
 */
func bringUpIPs(iface string, ips []string) {
	for _, ip := range ips {
		log.Debugf("Bringing up up %s on interface %s" ,ip, iface)
		success := netUtils.BringIPup(iface, ip)
		if !success {
			log.Errorf("Failed to bring up %s on interface %s", ip, iface)
		}
	}
}
