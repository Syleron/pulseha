package main

import (
	"github.com/coreos/go-log/log"
	"errors"
)

/**
 * Generate a new group in memory.
 *
 * @return string - group name
 * @return error
 */
func GroupNew(c *Config) (string, error) {
	if clusterCheck(c) {
		groupName := genGroupName(c)
		c.Groups[groupName] = []string{}
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
func GroupDelete(groupName string, c *Config) (error) {
	if groupExist(groupName, c) {
		if !nodeAssignedToInterface(groupName, c) {
			delete(c.Groups, groupName)
			return nil
		}
		return errors.New("group has network interface assignments. Please remove them and try again")
	} else {
		return errors.New("unable to delete group that doesn't exist")
	}
}

/**
 * Add floating IP to group
 *
 * @return error
 */
func GroupIpAdd(groupName string, ips []string, c *Config) (error) {
	if groupExist(groupName, c) {
		for _, ip := range ips {
			if ValidIPAddress(ip) {
				if len(c.Groups[groupName]) > 0 {
					if exists, _ := groupIPExist(groupName, ip, c); !exists {
						c.Groups[groupName] = append(c.Groups[groupName], ip)
					} else {
						log.Warning(ip + " already exists in group " + groupName + ".. skipping.")
					}
				} else {
					c.Groups[groupName] = append(c.Groups[groupName], ip)
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
func GroupIpRemove(groupName string, ips []string, c *Config) (error) {
	if groupExist(groupName, c) {
		for _, ip := range ips {
			if len(c.Groups[groupName]) > 0 {
				if exists, i := groupIPExist(groupName, ip, c); exists {
					c.Groups[groupName] = append(c.Groups[groupName][:i], c.Groups[groupName][i+1:]...)
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
func GroupAssign(groupName, node, iface string, c *Config) (error) {
	if groupExist(groupName, c) {
		if interfaceExist(iface) {
			if exists, _ := nodeInterfaceGroupExists(node, iface, groupName, c); !exists {
				c.Nodes[node].IPGroups[iface] = append(c.Nodes[node].IPGroups[iface], groupName)
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
func GroupUnassign(groupName, node, iface string, c *Config) (error) {
	if groupExist(groupName, c) {
		if !interfaceExist(iface) {
			if exists, i := nodeInterfaceGroupExists(node, iface, groupName, c); exists {
				c.Nodes[node].IPGroups[iface] = append(c.Nodes[node].IPGroups[iface][:i], c.Nodes[node].IPGroups[iface][i+1:]...)
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
