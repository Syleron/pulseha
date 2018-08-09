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
package agent

import (
	"errors"
	log "github.com/Sirupsen/logrus"
	"github.com/Syleron/PulseHA/src/utils"
	"github.com/Syleron/PulseHA/src/netUtils"
)

/**
Create new local config node definition
 */
func nodecreateLocal() (error) {
	log.Debug("create localhost node config definition")
	newNode := &Node{
		IPGroups: make(map[string][]string, 0),
	}
	hostname, err := GetHostname()
	if err != nil {
		return errors.New("cannot create cluster because unable to get hostname")
	}
	// Add the new node
	nodeAdd(hostname, newNode)
	// Create interface definitions each with their own group
	// TODO: Probably move this to another function?
	for _, ifaceName := range GetInterfaceNames() {
		if ifaceName != "lo" {
			newNode.IPGroups[ifaceName] = make([]string, 0)
			groupName := GenGroupName()
			gconf.Groups[groupName] = []string{}
			GroupAssign(groupName, hostname, ifaceName)
		}
	}
	// Save to our config
	gconf.save()
	// return our results
	return nil
}

/**
 * Add a node type Node to our config.
 */
func nodeAdd(hostname string, node *Node) error {
	log.Debug(hostname + " added to local cluster config")
	if !nodeExists(hostname) {
		gconf.Lock()
		gconf.Nodes[hostname] = *node
		gconf.Unlock()
		return nil
	}
	return errors.New("unable to add node as it already exists")
}

/**
 * Remove a node from our config by hostname.
 */
func nodeDelete(hostname string) error {
	log.Debug(hostname + " remove from the local node")
	if nodeExists(hostname) {
		gconf.Lock()
		delete(gconf.Nodes, hostname)
		gconf.Unlock()
		return nil
	}
	return errors.New("unable to delete node as it doesn't exist")
}

/**
 * Clear out local Nodes section of config.
 */
func nodesClearLocal() {
	log.Debug("All nodes cleared from local config")
	gconf.Lock()
	gconf.Nodes = map[string]Node{}
	gconf.Unlock()
}

/**
* Determines whether a Node already exists in a config based
  off the nodes hostname.
*/
func nodeExists(hostname string) bool {
	config := gconf.GetConfig()
	for key := range config.Nodes {
		if key == hostname {
			return true
		}
	}
	return false
}

/**
Get node by its hostname
*/
func nodeGetByName(hostname string) (Node, error) {
	config := gconf.GetConfig()
	for key, node := range config.Nodes {
		if key == hostname {
			return node, nil
		}
	}
	return Node{}, errors.New("unable to find node in config")
}

/**
 * Checks to see if a node has any interface assignments.
 * Note: Eww three for loops.
 */
func nodeAssignedToInterface(group string) bool {
	config := gconf.GetConfig()         // :-)
	for _, node := range config.Nodes { // :-|
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
	config := gconf.GetConfig()
	for index, existingGroup := range config.Nodes[node].IPGroups[iface] {
		if existingGroup == group {
			return true, index
		}
	}
	return false, -1
}
