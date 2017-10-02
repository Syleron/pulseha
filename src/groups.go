package main

import (
	"github.com/coreos/go-log/log"
	"errors"
	"strconv"
	"github.com/Syleron/PulseHA/src/netUtils"
	"github.com/Syleron/PulseHA/src/utils"
)

/**
 * Generate a new group in memory.
 *
 * @return string - group name
 * @return error
 */
func GroupNew() (string, error) {
	gconf.Lock()
	defer gconf.Unlock()
	if clusterCheck() {
		groupName := GenGroupName()
		gconf.Groups[groupName] = []string{}
		return groupName, nil
	} else {
		return "", errors.New("groups can only be created in a configured cluster")
	}
}

/**
 * Remove group from memory
 *
 * @return error
 */
func GroupDelete(groupName string) (error) {
	gconf.Lock()
	defer gconf.Unlock()
	if GroupExist(groupName) {
		if !NodeAssignedToInterface(groupName) {
			delete(gconf.Groups, groupName)
			return nil
		}
		return errors.New("group has network interface assignments. Please remove them and try again")
	} else {
		return errors.New("unable to delete group that doesn't exist")
	}
}

/**
 * Clear out all local Groups
 */
func GroupClearLocal() {
	gconf.Lock()
	defer gconf.Unlock()
	gconf.Groups = map[string][]string{}
}

/**
 * Add floating IP to group
 *
 * @return error
 */
func GroupIpAdd(groupName string, ips []string) (error) {
	gconf.Lock()
	defer gconf.Unlock()
	if GroupExist(groupName) {
		for _, ip := range ips {
			if utils.ValidIPAddress(ip) {
				if len(gconf.Groups[groupName]) > 0 {
					if exists, _ := GroupIPExist(groupName, ip); !exists {
						gconf.Groups[groupName] = append(gconf.Groups[groupName], ip)
					} else {
						log.Warning(ip + " already exists in group " + groupName + ".. skipping.")
					}
				} else {
					gconf.Groups[groupName] = append(gconf.Groups[groupName], ip)
				}
			} else {
				log.Warning(ip + " is not a valid IP address")
			}
		}
		return nil
	} else {
		return errors.New("group does not exist")
	}
}

/**
 * Remove floating IP from group
 *
 * @return error
 */
func GroupIpRemove(groupName string, ips []string) (error) {
	gconf.Lock()
	defer gconf.Unlock()
	if GroupExist(groupName) {
		for _, ip := range ips {
			if len(gconf.Groups[groupName]) > 0 {
				if exists, i := GroupIPExist(groupName, ip); exists {
					gconf.Groups[groupName] = append(gconf.Groups[groupName][:i], gconf.Groups[groupName][i+1:]...)
				} else {
					log.Warning(ip + " does not exist in group " + groupName + ".. skipping.")
				}
			}
		}
		return nil
	} else {
		return errors.New("group does not exist")
	}
}

/**
 * Assign a group to a node's interface
 *
 * @return error
 */
func GroupAssign(groupName, node, iface string) (error) {
	gconf.Lock()
	defer gconf.Unlock()
	if GroupExist(groupName) {
		if netUtils.InterfaceExist(iface) {
			if exists, _ := NodeInterfaceGroupExists(node, iface, groupName); !exists {
				gconf.Nodes[node].IPGroups[iface] = append(gconf.Nodes[node].IPGroups[iface], groupName)
			} else {
				log.Warning(groupName + " already exists in node " + node + ".. skipping.")
			}
			return nil
		}
		return errors.New("interface does not exist")
	}
	return errors.New("IP group does not exist")
}

/**
 * Unassign a group from a node's interface
 *
 * @return error
 */
func GroupUnassign(groupName, node, iface string) (error) {
	gconf.Lock()
	defer gconf.Unlock()
	if GroupExist(groupName) {
		if !netUtils.InterfaceExist(iface) {
			if exists, i := NodeInterfaceGroupExists(node, iface, groupName); exists {
				gconf.Nodes[node].IPGroups[iface] = append(gconf.Nodes[node].IPGroups[iface][:i], gconf.Nodes[node].IPGroups[iface][i+1:]...)
			} else {
				log.Warning(groupName + " does not exist in node " + node + ".. skipping.")
			}
			return nil
		}
		return errors.New("interface does not exist")
	} else {
		return errors.New("IP group does not exist")
	}
}

/**
 * Generates an available IP floating group name.
 */
func GenGroupName() (string) {
	config := gconf.GetConfig()
	totalGroups := len(config.Groups)
	for i := 1; i <= totalGroups; i++ {
		newName := "group" + strconv.Itoa(i)
		if _, ok := config.Groups[newName]; !ok {
			return newName
		}
	}
	return "group" + strconv.Itoa(totalGroups+1)
}

/**
 * Checks to see if a floating IP group already exists
 */
func GroupExist(name string) (bool) {
	config := gconf.GetConfig()
	if _, ok := config.Groups[name]; ok {
		return true
	}
	return false
}

/**
 * Checks to see if a floating IP already exists inside of a floating ip group
 * Returns bool - exists/not & int - slice index
 */
func GroupIPExist(name string, ip string) (bool, int) {
	config := gconf.GetConfig()
	for index, cip := range config.Groups[name] {
		if ip == cip {
			return true, index
		}
	}
	return false, -1
}

/**
 *function to get the nodes and interfaces that relate to the specified node
 */
func getGroupNodes(group string)([]string, []string) {
	var hosts []string
	var interfaces []string
	var found = false
	config := gconf.GetConfig()
	for name, node := range config.Nodes {
		for iface, groupNameSlice := range node.IPGroups {
			for _, groupName := range groupNameSlice{
				if group == groupName{
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
