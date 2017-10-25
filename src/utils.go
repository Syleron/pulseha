package main

import (
	"github.com/Syleron/PulseHA/src/netUtils"
	log "github.com/Sirupsen/logrus"
	"runtime"
	"github.com/Syleron/PulseHA/proto"
	"time"
)

/**
Networking - Bring up the groups on the current node
*/
func makeMemberActive() error {
	log.Debug("Utils:MakeMemberActive() Local node now passive")
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

/**
Networking - Bring down the ip groups on the current node
 */
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
Bring up an []ips for a specific interface
 */
func bringUpIPs(iface string, ips []string) error {
	for _, ip := range ips {
		//log.Infof("Bringing up IP %s on interface %s", ip, iface)
		success, err := netUtils.BringIPup(iface, ip)
		if !success && err != nil {
			log.Error(err.Error())
		} else if success && err != nil {
			log.Warn(err.Error())
		}
		// Send GARP
		go netUtils.SendGARP(iface, ip)
	}
	return nil
}

/**
Bring down an []ips for a specific interface
 */
func bringDownIPs(iface string, ips []string) error {
	for _, ip := range ips {
		//log.Infof("Taking down %s on interface %s", ip, iface)
		success, err := netUtils.BringIPdown(iface, ip)
		if err != nil {
			log.Debug(err.Error())
		}
		if !success {
			log.Errorf("Failed to take down %s on interface %s", ip, iface)
		}
	}
	return nil
}

/**

 */
func MyCaller() string {
	// we get the callers as uintptrs - but we just need 1
	fpcs := make([]uintptr, 1)
	// skip 3 levels to get to the caller of whoever called Caller()
	n := runtime.Callers(3, fpcs)
	if n == 0 {
		return "n/a" // proper error her would be better
	}
	// get the info of the actual function that's in the pointer
	fun := runtime.FuncForPC(fpcs[0]-1)
	if fun == nil {
		return "n/a"
	}
	// return its name
	return fun.Name()
}
/**

 */
func setLogLevel(level string) {
	logLevel, err := log.ParseLevel(level)
	if err != nil {
		panic(err.Error())
	}
	log.SetLevel(logLevel)
}

/**
Determine who is the correct active node if more than one active is brought online
 */
func getFailOverCountWinner(members []*proto.MemberlistMember) string {
	for _, member := range members {
		if member.Status != proto.MemberStatus_UNAVAILABLE {
			tym, _ := time.Parse(time.RFC1123, member.LastReceived)
			if tym == (time.Time{}) {
				return member.Hostname
			}
		}
	}
	return ""
}