// PulseHA - HA Cluster Daemon
// Copyright (C) 2017-2021  Andrew Zak <andrew@linux.com>
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package pulseha

import (
	"errors"
	"github.com/syleron/pulseha/packages/network"
	"github.com/syleron/pulseha/packages/utils"
	"strconv"
)

/**
Generate a new group in memory.
*/
func groupNew(groupName string) (string, error) {
	DB.Config.Lock()
	defer DB.Config.Unlock()
	var name string
	if groupName != "" {
		name = groupName
	} else {
		name = genGroupName()
	}
	DB.Config.Groups[name] = []string{}
	return name, nil
}

/**
Remove group from memory
*/
func groupDelete(groupName string) error {
	DB.Config.Lock()
	defer DB.Config.Unlock()
	if groupExist(groupName) {
		if !nodeAssignedToInterface(groupName) {
			delete(DB.Config.Groups, groupName)
			return nil
		}
		return errors.New("group has network interface assignments. Please remove them and try again")
	} else {
		return errors.New("unable to delete group that doesn't exist")
	}
}

/**
Clear out all local Groups
*/
func groupClearLocal() {
	DB.Config.Lock()
	defer DB.Config.Unlock()
	DB.Config.Groups = map[string][]string{}
}

/**
Add floating IP to group
*/
func groupIpAdd(groupName string, ips []string) error {
	DB.Config.Lock()
	defer DB.Config.Unlock()
	_, activeMember := DB.MemberList.GetActiveMember()
	if activeMember == nil {
		return errors.New("unable to add IP(s) to group as there no active node in the cluster")
	}
	if !groupExist(groupName) {
		return errors.New("group does not exist")
	}
	for _, ip := range ips {
		if err := utils.ValidIPAddress(ip); err == nil {
			// Check to see if the IP exists in any of the groups
			if !groupIpExistAll(ip) {
				DB.Config.Groups[groupName] = append(DB.Config.Groups[groupName], ip)
			} else {
				return errors.New(ip + " already exists in group " + groupName + ".. skipping.")
			}
		} else {
			return err
		}
	}
	return nil
}

/**
Remove floating IP from group
*/
func groupIpRemove(groupName string, ips []string) error {
	DB.Config.Lock()
	defer DB.Config.Unlock()
	if !groupExist(groupName) {
		return errors.New("group does not exist")
	}
	for _, ip := range ips {
		if len(DB.Config.Groups[groupName]) > 0 {
			if exists, i := groupIPExist(groupName, ip); exists {
				DB.Config.Groups[groupName] = append(DB.Config.Groups[groupName][:i], DB.Config.Groups[groupName][i+1:]...)
			} else {
				DB.Logging.Warn(ip + " does not exist in group " + groupName + ".. skipping.")
			}
		}
	}
	return nil
}

/**
Assign a group to a node's interface
*/
func groupAssign(groupName, uid, iface string) error {
	DB.Config.Lock()
	defer DB.Config.Unlock()
	if !groupExist(groupName) {
		return errors.New("IP group does not exist")
	}
	exists, _ := network.InterfaceExist(iface)
	if exists {
		if exists, _ := nodeInterfaceGroupExists(uid, iface, groupName); !exists {
			// Add the group
			DB.Config.Nodes[uid].IPGroups[iface] = append(DB.Config.Nodes[uid].IPGroups[iface], groupName)
			// make the group active
			hostname, _ := DB.MemberList.GetActiveMember()
			localNode := DB.Config.GetLocalNode()
			if hostname == localNode.Hostname {
				makeGroupActive(iface, groupName)
			}
		} else {
			DB.Logging.Warn(groupName + " is already assigned to " + iface + ".. skipping.")
		}
		return nil
	}
	return errors.New("interface does not exist")
}

/**
Unassign a group from a node's interface
*/
func groupUnassign(groupName, uid, iface string) error {
	DB.Config.Lock()
	defer DB.Config.Unlock()
	exists, _ := network.InterfaceExist(iface)
	if exists {
		if exists, i := nodeInterfaceGroupExists(uid, iface, groupName); exists {
			// make the group passive before removing it
			makeGroupPassive(iface, groupName)
			// Remove it
			DB.Config.Nodes[uid].IPGroups[iface] = append(DB.Config.Nodes[uid].IPGroups[iface][:i], DB.Config.Nodes[uid].IPGroups[iface][i+1:]...)
		} else {
			DB.Logging.Warn(groupName + " does not exist in node " + uid + ".. skipping.")
		}
		return nil
	}
	return errors.New("interface does not exist")
}

/**
Generates an available IP floating group name.
*/
func genGroupName() string {
	totalGroups := len(DB.Config.Groups)
	for i := 1; i <= totalGroups; i++ {
		newName := "group" + strconv.Itoa(i)
		if _, ok := DB.Config.Groups[newName]; !ok {
			return newName
		}
	}
	return "group" + strconv.Itoa(totalGroups+1)
}

/**
Checks to see if a floating IP group already exists
*/
func groupExist(name string) bool {
	if _, ok := DB.Config.Groups[name]; ok {
		return true
	}
	return false
}

/**
Checks to see if a floating IP already exists inside of a floating ip group
*/
func groupIPExist(name string, ip string) (bool, int) {
	for index, cip := range DB.Config.Groups[name] {
		if ip == cip {
			return true, index
		}
	}
	return false, -1
}

/**
Checks to see if a floating IP already exists in any of the floating IP groups
*/
func groupIpExistAll(ip string) bool {
	for _, cip := range DB.Config.Groups {
		for _, dip := range cip {
			if ip == dip {
				return true
			}
		}
	}
	return false
}

/**
function to get the nodes and interfaces that relate to the specified node
*/
func getGroupNodes(group string) ([]string, []string) {
	var hosts []string
	var interfaces []string
	var found = false
	for _, node := range DB.Config.Nodes {
		for iface, groupNameSlice := range node.IPGroups {
			for _, groupName := range groupNameSlice {
				if group == groupName {
					hosts = append(hosts, node.Hostname)
					interfaces = append(interfaces, iface)
					found = true
				}
			}
		}
	}
	if found {
		return hosts, interfaces
	}
	return nil, nil
}

/**
Make a group of IPs active
*/
func makeGroupActive(iface string, groupName string) {
	DB.Logging.Debug("Groups:makeGroupActive() Adding floating IPs from " + iface + " defined in group " + groupName)
	if err := BringUpIPs(iface, DB.Config.Groups[groupName]); err != nil {
		DB.Logging.Error(err.Error())
	}
}

/**
Make a group of IPs passive
*/
func makeGroupPassive(iface string, groupName string) {
	DB.Logging.Debug("Groups:makeGroupPassive() Removing floating IPs from " + iface + " defined in group " + groupName)
	if err := BringDownIPs(iface, DB.Config.Groups[groupName]); err != nil {
		DB.Logging.Debug(err.Error())
	}
}
