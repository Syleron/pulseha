package main

import (
	"github.com/Syleron/PulseHA/src/netUtils"
	"github.com/coreos/go-log/log"
	"errors"
)

/**
	Bring up the groups on the current node
 */
func makeMemberActive() error {
	log.Debug("Utils:makeMemberActive() Local node now active")
	configCopy := gconf.GetConfig()
	for name, node := range configCopy.Nodes {
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

func makeMemberPassive() error {
	log.Debug("Utils:MakeMemberPassive() Local node now passive")
	configCopy := gconf.GetConfig()
	for name, node := range configCopy.Nodes {
		if name == gconf.getLocalNode() {
			for iface, groups := range node.IPGroups {
				for _, groupName := range groups {
					makeGroupPassive(iface, groupName)
				}
			}
		}
	}
	return nil
}

/**
 * Bring up a group of ip addresses
 */
func bringUpIPs(iface string, ips []string) error {
	for _, ip := range ips {
		log.Debugf("Bringing up IP %s on interface %s", ip, iface)
		success := netUtils.BringIPup(iface, ip)
		if !success {
			log.Errorf("Failed to bring up %s on interface %s", ip, iface)
			return errors.New("failed to bring up ip " + ip + " on interface " + iface)
		}
	}
	return nil
}

/**

 */
func bringDownIPs(iface string, ips []string) {
	for _, ip := range ips {
		log.Debugf("Taking down up %s on interface %s", ip, iface)
		success := netUtils.BringIPdown(iface, ip)
		if !success {
			log.Errorf("Failed to take down %s on interface %s", ip, iface)
		}
	}
}
