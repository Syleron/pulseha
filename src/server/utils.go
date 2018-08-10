/*
   PulseHA - HA Cluster Daemon
   Copyright (C) 2017-2018  Andrew Zak <andrew@pulseha.com>

   This program is free software: you can redistribute it and/or modify
   it under the terms of the GNU Affero General Public License as published
   by the Free Software Foundation, either version 3 of the License, or
   (at your option) any later version.

   This program is distributed in the hope that it will be useful,
   but WITHOUT ANY WARRANTY; without even the implied warranty of
   MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
   GNU Affero General Public License for more details.

   You should have received a copy of the GNU Affero General Public License
   along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/
package server

import (
	log "github.com/Sirupsen/logrus"
	"github.com/Syleron/PulseHA/proto"
	"runtime"
	"time"
)

/**
Networking - Bring up the groups on the current node
*/
func MakeMemberActive() error {
	log.Debug("Utils:MakeMemberActive() Local node now active")
	configCopy := db.GetConfig()
	for name, node := range configCopy.Nodes {
		if name == db.GetLocalNode() {
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
func MakeMemberPassive() error {
	log.Debug("Utils:MakeMemberPassive() Local node now passive")
	configCopy := db.GetConfig()
	for name, node := range configCopy.Nodes {
		if name == db.GetLocalNode() {
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
func BringUpIPs(iface string, ips []string) error {
	plugin := pulse.Plugins.getNetworkingPlugin()
	if plugin == nil {
		log.Debug("No networking plugin.. skipping network action")
		return nil
	}
	err := plugin.Plugin.(PluginNet).BringUpIPs(iface, ips)
	return err
}

/**
Bring down an []ips for a specific interface
*/
func BringDownIPs(iface string, ips []string) error {
	plugin := pulse.Plugins.getNetworkingPlugin()
	if plugin == nil {
		log.Debug("No networking plugin.. skipping network action")
		return nil
	}
	err := plugin.Plugin.(PluginNet).BringDownIPs(iface, ips)
	return err
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
	fun := runtime.FuncForPC(fpcs[0] - 1)
	if fun == nil {
		return "n/a"
	}
	// return its name
	return fun.Name()
}

/**

 */
func SetLogLevel(level string) {
	logLevel, err := log.ParseLevel(level)
	if err != nil {
		panic(err.Error())
	}
	log.SetLevel(logLevel)
}

/**
Determine who is the correct active node if more than one active is brought online
*/
func GetFailOverCountWinner(members []*proto.MemberlistMember) string {
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