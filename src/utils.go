package main

import (
	"errors"
	"github.com/Syleron/PulseHA/src/netUtils"
	log "github.com/Sirupsen/logrus"
	"runtime"
	"github.com/Syleron/PulseHA/proto"
	"time"
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

/**

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
TODO: FOTime for any new node that hasnt gone active yet will be set as default. This may cause issues.
 */
func getFailOverCountWinner(members []*proto.MemberlistMember) string {
	var winnerMember *proto.MemberlistMember
	for index, member := range members {
		log.Debug("considering " + member.GetHostname() + " as correct active. " + member.GetLastReceived())
		if index == 0 {
			log.Info(member.GetHostname() + " is the current winner")
			winnerMember = member
			continue
		}
		foTime, _ := time.Parse(time.RFC1123, member.FoTime)
		winFoTime, _ := time.Parse(time.RFC1123, winnerMember.FoTime)
		// Check to see if the failover count is the same
		if member.FoCount == winnerMember.FoCount {
			if time.Since(foTime) > time.Since(winFoTime) {
				log.Debug(member.GetHostname() + " is the current winner with a time difference of " + time.Since(winFoTime).String())
				winnerMember = member
			}
		} else if member.FoCount > winnerMember.FoCount {
			log.Debug(member.GetHostname() + " is the current winner")
			winnerMember = member
		}
	}
	return winnerMember.Hostname
}