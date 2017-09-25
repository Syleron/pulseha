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

func NodeAdd() {

}

func NodeDelete() {

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
