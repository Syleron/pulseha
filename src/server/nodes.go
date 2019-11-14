/*
   PulseHA - HA Cluster Daemon
   Copyright (C) 2017-2019  Andrew Zak <andrew@linux.com>

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
	"errors"
	"github.com/google/uuid"
	"github.com/labstack/gommon/log"
	"github.com/syleron/pulseha/src/config"
	"github.com/syleron/pulseha/src/netUtils"
	"github.com/syleron/pulseha/src/utils"
)

/**
Create new local config node definition
*/
func nodeCreateLocal(ip string, port string, assignGroups bool) (*config.Node, error) {
	log.Debug("create localhost node config definition")
	hostname, err := utils.GetHostname()
	if err != nil {
		return &config.Node{}, errors.New("cannot create cluster because unable to get hostname")
	}
	newNode := &config.Node{
		IP:       ip,
		Port:     port,
		IPGroups: make(map[string][]string, 0),
		Hostname: hostname,
	}
	// Add the new node
	if err := nodeAdd(hostname, newNode); err != nil {
		return &config.Node{}, errors.New("unable to add local node to config")
	}
	// Create interface definitions each with their own group
	for _, ifaceName := range netUtils.GetInterfaceNames() {
		if ifaceName != "lo" {
			newNode.IPGroups[ifaceName] = make([]string, 0)
			if assignGroups {
				groupName := genGroupName()
				DB.Config.Groups[groupName] = []string{}
				if err := groupAssign(groupName, hostname, ifaceName); err != nil {
					log.Warnf("Unable to assign group to interface: %s", err.Error())
				}
			}
		}
	}
	// return our results
	return newNode, nil
}

// nodeUpdateLocalInterfaces()
func nodeUpdateLocalInterfaces() error {
	localHostname, err := utils.GetHostname()
	if err != nil {
		return err
	}
	localNode, err := nodeGetByHostname(localHostname)
	if err != nil {
		return err
	}
	// Get our local interfaces
	localifaces := DB.Config.Nodes[localHostname].IPGroups
	// Get our current interfaces
	ifaces := netUtils.GetInterfaceNames()
	// Add missing interfaces
	for _, b := range ifaces {
		exist := false
		for n := range localifaces {
			if n == b {
				exist = true
				break
			}
		}
		if !exist && b != "lo" {
			localNode.IPGroups[b] = make([]string, 0)
			groupName := genGroupName()
			DB.Config.Groups[groupName] = []string{}
			if err := groupAssign(groupName, localHostname, b); err != nil {
				log.Warnf("Unable to assign group to interface: %s", err.Error())
			}
		}
	}
	// Delete missing interfaces
	for b := range localifaces {
		exist := false
		for _, n := range ifaces {
			if n == b {
				exist = true
				break
			}
		}
		if !exist {
			delete(DB.Config.Nodes[localHostname].IPGroups, b)
		}
	}
	// Save to our config
	if err := DB.Config.Save(); err != nil {
		return errors.New("write local node failed")
	}
	return nil
}

/**
 * Add a node type Node to our config.
 */
func nodeAdd(hostname string, node *config.Node) error {
	DB.Logging.Debug(hostname + " added to local cluster config")
	if !nodeExistsByHostname(hostname) {
		// Generate UUID
		uuid := uuid.New()
		DB.Config.Lock()
		DB.Config.Nodes[uuid.String()] = *node
		DB.Config.Unlock()
		return nil
	}
	return errors.New("unable to add node as it already exists")
}

/**
 * Remove a node from our config by hostname.
 */
func nodeDelete(uuid string) error {
	DB.Logging.Debug("Nodes:nodeDelete()" + uuid + " node removed.")
	if nodeExistsByUUID(uuid) {
		DB.Config.Lock()
		delete(DB.Config.Nodes, uuid)
		DB.Config.Unlock()
		return nil
	}
	return errors.New("unable to delete node as it doesn't exist")
}

/**
 * Clear out local Nodes section of config.
 */
func nodesClearLocal() {
	DB.Logging.Debug("All nodes cleared from local config")
	DB.Config.Lock()
	DB.Config.Nodes = map[string]config.Node{}
	DB.Config.Unlock()
}

/**
* Determines whether a Node already exists in a config based
  off the nodes hostname.
*/
func nodeExistsByHostname(hostname string) bool {
	for _, node := range DB.Config.Nodes {
		if node.Hostname == hostname {
			return true
		}
	}
	return false
}

/**
* Determines whether a Node already exists in a config based
  off the nodes UUID.
*/
func nodeExistsByUUID(uuid string) bool {
	for key := range DB.Config.Nodes {
		if key == uuid {
			return true
		}
	}
	return false
}

/**
Get node by its hostname
*/
func nodeGetByUUID(uuid string) (config.Node, error) {
	for key, node := range DB.Config.Nodes {
		if key == uuid {
			return node, nil
		}
	}
	return config.Node{}, errors.New("unable to find node in config")
}

func nodeGetByHostname(hostname string) (config.Node, error) {
	for _, node := range DB.Config.Nodes {
		if node.Hostname == hostname {
			return node, nil
		}
	}
	return config.Node{}, errors.New("unable to find node in config")
}

/**
 * Checks to see if a node has any interface assignments.
 * Note: Eww three for loops.
 */
func nodeAssignedToInterface(group string) bool {
	for _, node := range DB.Config.Nodes { // :-|
		for _, groups := range node.IPGroups { // :-s
			for _, ifaceGroup := range groups { // :-(
				if ifaceGroup == group {
					return true
				}
			}
		}
	}
	return false
}

/**
 * Checks to see if a floating IP group has already been assigned to a node's interface.
 * Returns bool - exists/not & int - slice index
 */
func nodeInterfaceGroupExists(node, iface, group string) (bool, int) {
	for index, existingGroup := range DB.Config.Nodes[node].IPGroups[iface] {
		if existingGroup == group {
			return true, index
		}
	}
	return false, -1
}
