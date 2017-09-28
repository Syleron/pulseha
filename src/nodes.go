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
	"github.com/coreos/go-log/log"
)

/**
 * Add a node type Node to our config.
 */
func NodeAdd(hostname string, node *Node, c *Config) (error) {
	log.Debug(hostname + " added to local cluster config")
	if !NodeExists(hostname, c) {
		c.Nodes[hostname] = *node
		return nil
	}
	return errors.New("unable to add node as it already exists")
}

/**
 * Remove a node from our config by hostname.
 */
func NodeDelete(hostname string, c *Config) (error) {
	log.Debug(hostname + " remove from the local node")
	if NodeExists(hostname, c)	{
		delete(c.Nodes, hostname)
		return nil
	}
	return errors.New("unable to delete node as it doesn't exist")
}

/**
 * Clear out local Nodes section of config.
 */
func NodesClearLocal(c *Config) {
	log.Debug("All nodes cleared from local config")
	c.Nodes = map[string]Node{}
}

/**
 * Determines whether a Node already exists in a config based
   off the nodes hostname.
 */
func NodeExists(hostName string, c *Config) (bool) {
	for key, _ := range c.Nodes {
		if key == hostName {
			return true
		}
	}
	return false
}

/**
 * Checks to see if a node has any interface assignments.
 * Note: Eww three for loops.
 */
func NodeAssignedToInterface(group string, c *Config) (bool) {
	for _, node := range c.Nodes {
		for _, groups := range node.IPGroups {
			for _, ifaceGroup := range groups {
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
func NodeInterfaceGroupExists(node, iface, group string, c *Config) (bool, int) {
	for index, existingGroup := range c.Nodes[node].IPGroups[iface] {
		if existingGroup == group {
			return true, index
		}
	}
	return false, -1
}
