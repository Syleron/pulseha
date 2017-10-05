package main

import (
	"github.com/Syleron/PulseHA/src/netUtils"
	"github.com/coreos/go-log/log"
)

/**
	Bring up the groups on the current node
 */
func makeActive()error{

	return nil
}
/**
 * Make a group of IPs active
 */
func makeActiveGroup(groupName string) {
	//get group config
	// check if we are active?
	// if not active go active
	// get group details and any plugin config
	//

	//groupConfig :=
	//if plugin loaded call plugin bringIPUP for each item in config

	/*
	Check
	 */
	configCopy := gconf.GetConfig()
	//configCopy.getLocalNode()
	localNode := configCopy.LocalNode()
	for iface, groupSlice := range localNode.IPGroups {
		for _, group := range groupSlice {
			if groupName == group {
				bringUpIPs(iface, configCopy.Groups[group])
				//garp?
			}
		}
	}


	//confirm takeover was successful if not shout
	// tell other nodes we are active
}

/**
 * Bring up a group of ip addresses
 */
func bringUpIPs(iface string, ips []string) {
	for _, ip := range ips {
		success := netUtils.BringIPup(iface, ip)
		if !success {
			log.Errorf("Failed to bring up %s on interface %s", ip, iface)
		}
	}
}
