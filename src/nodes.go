/*
   PulseHA - HA Cluster Daemon
   Copyright (C) 2017  Andrew Zak <andrew@pulseha.com>

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
package main

import (
	"errors"
	log "github.com/Sirupsen/logrus"
)

/**
 * Add a node type Node to our config.
 */
func NodeAdd(hostname string, node *Node) error {
	log.Debug(hostname + " added to local cluster config")
	if !NodeExists(hostname) {
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
func NodeDelete(hostname string) error {
	log.Debug(hostname + " remove from the local node")
	if NodeExists(hostname) {
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
func NodesClearLocal() {
	log.Debug("All nodes cleared from local config")
	gconf.Lock()
	gconf.Nodes = map[string]Node{}
	gconf.Unlock()
}

/**
* Determines whether a Node already exists in a config based
  off the nodes hostname.
*/
func NodeExists(hostname string) bool {
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
func NodeGetByName(hostname string) (Node, error) {
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
func NodeAssignedToInterface(group string) bool {
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
func NodeInterfaceGroupExists(node, iface, group string) (bool, int) {
	config := gconf.GetConfig()
	for index, existingGroup := range config.Nodes[node].IPGroups[iface] {
		if existingGroup == group {
			return true, index
		}
	}
	return false, -1
}
