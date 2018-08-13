package server

import (
	"errors"
	log "github.com/Sirupsen/logrus"
	"github.com/Syleron/PulseHA/src/net_utils"
	"github.com/Syleron/PulseHA/src/utils"
	"strconv"
	)

/**
Generate a new group in memory.
*/
func GroupNew(groupName string) (string, error) {
	DB.Config.Lock()
	defer DB.Config.Unlock()
	var name string
	if groupName != "" {
		name = groupName
	} else {
		name = GenGroupName()
	}
	DB.Config.Groups[name] = []string{}
	return name, nil
}

/**
Remove group from memory
*/
func GroupDelete(groupName string) error {
	DB.Config.Lock()
	defer DB.Config.Unlock()
	if GroupExist(groupName) {
		if !NodeAssignedToInterface(groupName) {
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
func GroupClearLocal() {
	DB.Config.Lock()
	defer DB.Config.Unlock()
	DB.Config.Groups = map[string][]string{}
}

/**
Add floating IP to group
*/
func GroupIpAdd(groupName string, ips []string) error {
	DB.Config.Lock()
	defer DB.Config.Unlock()
	_, activeMember := DB.Server.GetActiveMember()
	if activeMember == nil {
		return errors.New("unable to add IP(s) to group as there no active node in the cluster")
	}
	if !GroupExist(groupName) {
		return errors.New("group does not exist")
	}
	for _, ip := range ips {
		if err := utils.ValidIPAddress(ip); err == nil {
			// Check to see if the IP exists in any of the groups
			if !GroupIpExistAll(ip) {
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
func GroupIpRemove(groupName string, ips []string) error {
	DB.Config.Lock()
	defer DB.Config.Unlock()
	if !GroupExist(groupName) {
		return errors.New("group does not exist")
	}
	for _, ip := range ips {
		if len(DB.Config.Groups[groupName]) > 0 {
			if exists, i := GroupIPExist(groupName, ip); exists {
				DB.Config.Groups[groupName] = append(DB.Config.Groups[groupName][:i], DB.Config.Groups[groupName][i+1:]...)
			} else {
				log.Warning(ip + " does not exist in group " + groupName + ".. skipping.")
			}
		}
	}
	return nil
}

/**
Assign a group to a node's interface
*/
func GroupAssign(groupName, node, iface string) error {
	DB.Config.Lock()
	defer DB.Config.Unlock()
	if !GroupExist(groupName) {
		return errors.New("IP group does not exist")
	}
	if net_utils.InterfaceExist(iface) {
		if exists, _ := NodeInterfaceGroupExists(node, iface, groupName); !exists {
			// Add the group
			DB.Config.Nodes[node].IPGroups[iface] = append(DB.Config.Nodes[node].IPGroups[iface], groupName)
			// make the group active
			MakeGroupActive(iface, groupName)
		} else {
			log.Warning(groupName + " already exists in node " + node + ".. skipping.")
		}
		return nil
	}
	return errors.New("interface does not exist")
}

/**
Unassign a group from a node's interface
*/
func GroupUnassign(groupName, node, iface string) error {
	DB.Config.Lock()
	defer DB.Config.Unlock()
	if net_utils.InterfaceExist(iface) {
		if exists, i := NodeInterfaceGroupExists(node, iface, groupName); exists {
			// make the group passive before removing it
			MakeGroupPassive(iface, groupName)
			// Remove it
			DB.Config.Nodes[node].IPGroups[iface] = append(DB.Config.Nodes[node].IPGroups[iface][:i], DB.Config.Nodes[node].IPGroups[iface][i+1:]...)
		} else {
			log.Warning(groupName + " does not exist in node " + node + ".. skipping.")
		}
		return nil
	}
	return errors.New("interface does not exist")
}

/**
Generates an available IP floating group name.
*/
func GenGroupName() string {
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
func GroupExist(name string) bool {
	if _, ok := DB.Config.Groups[name]; ok {
		return true
	}
	return false
}

/**
Checks to see if a floating IP already exists inside of a floating ip group
*/
func GroupIPExist(name string, ip string) (bool, int) {
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
func GroupIpExistAll(ip string) bool {
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
func GetGroupNodes(group string) ([]string, []string) {
	var hosts []string
	var interfaces []string
	var found = false
	for name, node := range DB.Config.Nodes {
		for iface, groupNameSlice := range node.IPGroups {
			for _, groupName := range groupNameSlice {
				if group == groupName {
					hosts = append(hosts, name)
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
func MakeGroupActive(iface string, groupName string) {
	BringUpIPs(iface, DB.Config.Groups[groupName])
}

/**
Make a group of IPs passive
 */
func MakeGroupPassive(iface string, groupName string) {
	BringDownIPs(iface, DB.Config.Groups[groupName])
}
